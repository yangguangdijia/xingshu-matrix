package observability

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	matrixcrypto "github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// TestNotifyCenterPostgres exercises the migration, routing, records,
// rate limiting and the incident cursor against a real PostgreSQL. It runs
// only when MATRIX_TEST_DATABASE_URL points at a disposable database.
func TestNotifyCenterPostgres(t *testing.T) {
	pool := newDatasourceScopeTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `CREATE TABLE cluster_clusters (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL UNIQUE, env TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	migrations, err := store.ParseMigrations(New().Migrations())
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := matrixcrypto.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	m := &Module{d: &app.Deps{DB: pool, Crypto: cipher}}
	for _, mg := range migrations {
		if mg.Version == 7 {
			// a channel created before the upgrade keeps receiving alerts
			enc, _ := m.encConfig(&ChannelConfig{Webhook: "https://127.0.0.1:1/legacy"})
			if _, err := pool.Exec(ctx, `INSERT INTO obs_channels (name,type,config_enc,severities,envs) VALUES ('legacy','wecom',$1,'{critical}','{prod}')`, enc); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pool.Exec(ctx, mg.SQL); err != nil {
			t.Fatalf("migration %d: %v", mg.Version, err)
		}
	}
	subs, err := m.loadSubs(ctx, true)
	if err != nil || len(subs) != 1 || subs[0].Name != "告警 → legacy" || subs[0].Severities[0] != "critical" {
		t.Fatalf("legacy subscription: %v %+v", err, subs)
	}
	if _, err := pool.Exec(ctx, `UPDATE obs_notify_subscriptions SET dedup_minutes=10`); err != nil {
		t.Fatal(err)
	}

	ev := sampleEvent("alert")
	m.dispatch(ctx, []*NotifyEvent{&ev}) // loopback target: blocked by the default policy → failed record
	var status, reason string
	if err := pool.QueryRow(ctx, `SELECT status, reason FROM obs_notify_records ORDER BY id DESC LIMIT 1`).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || !strings.Contains(reason, "内网") {
		t.Fatalf("record %s %s", status, reason)
	}
	// failures do not count for rate limiting; fake a success and send again
	if _, err := pool.Exec(ctx, `UPDATE obs_notify_records SET status='success'`); err != nil {
		t.Fatal(err)
	}
	notifyLimiter.forget(limiterKey(subs[0], &Channel{ID: subs[0].ChannelIDs[0]}, &ev))
	m.dispatch(ctx, []*NotifyEvent{&ev})
	if err := pool.QueryRow(ctx, `SELECT status, reason FROM obs_notify_records ORDER BY id DESC LIMIT 1`).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "suppressed" || !strings.Contains(reason, "限频") {
		t.Fatalf("dedup via records: %s %s", status, reason)
	}

	// incident cursor: first poll initialises, later events are collected once
	if evs, err := m.collectIncidents(ctx, ""); err != nil || len(evs) != 0 {
		t.Fatalf("init poll %v %v", evs, err)
	}
	var iid int64
	if err := pool.QueryRow(ctx, `INSERT INTO oncall_incidents (title,severity,env) VALUES ('db down','P1','prod') RETURNING id`).Scan(&iid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO oncall_incident_events (incident_id,kind,actor,message) VALUES ($1,'declared','a','x'),($1,'note','a','y')`, iid); err != nil {
		t.Fatal(err)
	}
	evs, err := m.collectIncidents(ctx, "")
	if err != nil || len(evs) != 1 || evs[0].Status != "declared" || evs[0].Severity != "critical" {
		t.Fatalf("incident events %v %+v", err, evs)
	}
	if evs, _ = m.collectIncidents(ctx, ""); len(evs) != 0 {
		t.Fatal("events must not be collected twice")
	}

	// template references survive / reset on delete
	var tid int64
	if err := pool.QueryRow(ctx, `INSERT INTO obs_notify_templates (name,event_kind,title_tpl,body_tpl) VALUES ('t','alert','{{.AlertName}}','x') RETURNING id`).Scan(&tid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE obs_channels SET templates=jsonb_build_object('alert',$1::bigint)`, tid); err != nil {
		t.Fatal(err)
	}
	refs, err := m.templateRefs(ctx, tid)
	if err != nil || len(refs) != 1 {
		t.Fatalf("template refs %v %v", refs, err)
	}
}
