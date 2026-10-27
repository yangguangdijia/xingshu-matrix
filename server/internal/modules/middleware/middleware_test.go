package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	matrixcrypto "github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
)

func TestConsoleApprovalPayloadsEncryptAndRedactSecrets(t *testing.T) {
	cipher, err := matrixcrypto.New([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	m := &Module{d: &app.Deps{Crypto: cipher}}

	const sqlText = "UPDATE accounts SET password='sql-secret' WHERE id=7"
	sql := sqlPayload{InstanceID: 11, Instance: "prod-db", Database: "app"}
	if err := m.encryptSQLPayload(&sql, sqlText); err != nil {
		t.Fatal(err)
	}
	rawSQL, err := json.Marshal(sql)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawSQL), sqlText) || strings.Contains(string(rawSQL), "sql-secret") {
		t.Fatal("SQL approval storage contains plaintext")
	}
	publicSQL := sanitizeSQLApprovalPayload(rawSQL)
	if strings.Contains(string(publicSQL), sql.EncryptedSQL) || strings.Contains(string(publicSQL), "sql-secret") ||
		!strings.Contains(string(publicSQL), `"sql_redacted":true`) {
		t.Fatalf("unsafe SQL approval view: %s", publicSQL)
	}
	openedSQL, err := m.decryptSQLPayload(&sql)
	if err != nil || openedSQL != sqlText {
		t.Fatalf("SQL payload did not round-trip: %v", err)
	}
	sql.InstanceID++
	if _, err := m.decryptSQLPayload(&sql); err == nil {
		t.Fatal("SQL ciphertext should be bound to its instance")
	}

	const redisValue = "redis-secret"
	redis := redisPayload{InstanceID: 15, Instance: "prod-cache", DB: 2, Command: "SET"}
	if err := m.encryptRedisPayload(&redis, []string{"SET", "app:token", redisValue}); err != nil {
		t.Fatal(err)
	}
	rawRedis, err := json.Marshal(redis)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawRedis), redisValue) {
		t.Fatal("Redis approval storage contains plaintext")
	}
	publicRedis := sanitizeRedisApprovalPayload(rawRedis)
	if strings.Contains(string(publicRedis), redis.EncryptedArgs) || strings.Contains(string(publicRedis), redisValue) ||
		!strings.Contains(string(publicRedis), `"args_redacted":true`) {
		t.Fatalf("unsafe Redis approval view: %s", publicRedis)
	}
	openedArgs, err := m.decryptRedisPayload(&redis)
	if err != nil || len(openedArgs) != 3 || openedArgs[2] != redisValue {
		t.Fatalf("Redis payload did not round-trip: %v %q", err, openedArgs)
	}
	redis.DB++
	if _, err := m.decryptRedisPayload(&redis); err == nil {
		t.Fatal("Redis ciphertext should be bound to its target")
	}

	changeRaw := json.RawMessage(`{"op":"rabbitmq.create_queue","instance_id":1,"arguments":{"x-secret":"queue-secret"},"configs":{"password":"config-secret"}}`)
	publicChange := sanitizeChangeApprovalPayload(changeRaw)
	if strings.Contains(string(publicChange), "queue-secret") || strings.Contains(string(publicChange), "config-secret") {
		t.Fatalf("unsafe middleware change approval view: %s", publicChange)
	}
	if auditSQLStatement(SQLRisk{Keyword: "UPDATE"}) != "UPDATE [statement redacted]" ||
		auditRedisStatement([]string{"SET", "key", "redis-secret"}) != "SET [arguments redacted]" {
		t.Fatal("console audit statement should omit input values")
	}
}

func TestAnalyzeSQL(t *testing.T) {
	cases := []struct {
		sql, typ, level string
		wantErr         bool
	}{
		{"SELECT * FROM users", TypeMySQL, RiskRead, false},
		{"  -- comment\n select 1;", TypePostgres, RiskRead, false},
		{"DELETE FROM users", TypeMySQL, RiskDangerous, false},
		{"delete from users where id = 1", TypeMySQL, RiskWrite, false},
		{"UPDATE t SET a=1", TypePostgres, RiskDangerous, false},
		{"UPDATE t SET a='where' ", TypePostgres, RiskDangerous, false}, // WHERE inside literal does not count
		{"UPDATE t SET a=1 WHERE id=2", TypePostgres, RiskWrite, false},
		{"DROP TABLE users", TypeMySQL, RiskDangerous, false},
		{"truncate users", TypePostgres, RiskDangerous, false},
		{"ALTER TABLE t ADD c int", TypeMySQL, RiskDangerous, false},
		{"/* x */ INSERT INTO t VALUES (1)", TypeMySQL, RiskWrite, false},
		{"WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d", TypePostgres, RiskDangerous, false},
		{"WITH a AS (SELECT 1) SELECT * FROM a", TypePostgres, RiskRead, false},
		{"SELECT 1; DROP TABLE t", TypeMySQL, "", true},
		{"SELECT ';' AS x", TypeMySQL, RiskRead, false},
		{"SELECT 1 # ; DROP TABLE t", TypeMySQL, RiskRead, false}, // MySQL comment hides the rest
		{"SELECT 1 # 2; DROP TABLE t", TypePostgres, "", true},    // '#' is an operator in PG
		{"   ;  ", TypeMySQL, "", true},
	}
	for _, c := range cases {
		r, err := AnalyzeSQL(c.sql, c.typ)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err=%v wantErr=%v", c.sql, err, c.wantErr)
			continue
		}
		if err == nil && r.Level != c.level {
			t.Errorf("%q: level=%s want %s", c.sql, r.Level, c.level)
		}
	}
}

func TestRedisCommand(t *testing.T) {
	args, err := ParseRedisCommand(`SET "my key" 'a b' `)
	if err != nil || len(args) != 3 || args[1] != "my key" || args[2] != "a b" {
		t.Fatalf("parse: %v %q", err, args)
	}
	for _, line := range []string{"flushall", "FLUSHDB", "keys *", "config set maxmemory 1", "CONFIG REWRITE"} {
		a, _ := ParseRedisCommand(line)
		if _, err := CheckRedisCommand(a); err == nil {
			t.Errorf("%q should be blocked", line)
		}
	}
	if r, err := CheckRedisCommand([]string{"config", "get", "maxmemory"}); err != nil || r != RiskRead {
		t.Errorf("CONFIG GET: %v %s", err, r)
	}
	if r, _ := CheckRedisCommand([]string{"set", "a", "1"}); r != RiskWrite {
		t.Errorf("SET risk %s", r)
	}
	if !isProdEnv("PROD") || !isProdEnv("production") || isProdEnv("staging") {
		t.Error("isProdEnv")
	}
}

func TestRabbitAndRocket(t *testing.T) {
	var gotPath, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/login/login.do":
			http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "s1", Path: "/"})
			_, _ = w.Write([]byte(`{"status":0,"data":{"loginUserName":"admin"}}`))
		case r.URL.Path == "/topic/list.query":
			if c, err := r.Cookie("JSESSIONID"); err != nil || c.Value != "s1" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"status":0,"data":{"topicList":["TopicTest","%RETRY%g1"]},"errMsg":null}`))
		default:
			gotPath, gotUser = r.URL.EscapedPath(), ""
			if u, _, ok := r.BasicAuth(); ok {
				gotUser = u
			}
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"rabbitmq_version":"3.13.1","cluster_name":"rabbit@x"}`))
			}
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	rmq := &Instance{Type: TypeRabbitMQ, Endpoints: []string{srv.URL}, Auth: Auth{Username: "guest", Password: "guest"}}
	m := &Module{}
	if _, err := m.execChange(ctx, rmq, change{Op: "rabbitmq.create_queue", VHost: "/", Queue: "orders", Durable: true}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/queues/%2F/orders" || gotUser != "guest" {
		t.Fatalf("path=%s user=%s", gotPath, gotUser)
	}
	ver, err := ping(ctx, rmq)
	if err != nil || ver != "RabbitMQ 3.13.1" {
		t.Fatalf("ping %v %s", err, ver)
	}

	rkt := &Instance{Type: TypeRocketMQ, Endpoints: []string{srv.URL}, Auth: Auth{Username: "admin", Password: "admin"}}
	data, err := rocketGetData(ctx, rkt, "/topic/list.query")
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ TopicList []string }
	if json.Unmarshal(data, &v) != nil || len(v.TopicList) != 2 {
		t.Fatalf("topics %s", data)
	}
}
