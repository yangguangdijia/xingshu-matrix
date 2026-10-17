package hosts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/hosts/proto"
)

// onlineWindow: a host is online when its agent is connected and was seen recently.
const onlineWindow = 90 * time.Second

type Host struct {
	ID           int64             `json:"id"`
	Name         string            `json:"name"`
	IP           string            `json:"ip"`
	Env          string            `json:"env"`
	Labels       map[string]string `json:"labels"`
	Facts        proto.Facts       `json:"facts"`
	AgentVersion string            `json:"agent_version"`
	Enrolled     bool              `json:"enrolled"`
	Online       bool              `json:"online"`
	LastSeenAt   *time.Time        `json:"last_seen_at"`
	RegisteredAt *time.Time        `json:"registered_at"`
	CreatedBy    string            `json:"created_by"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

type EnrollToken struct {
	ID         int64             `json:"id"`
	Name       string            `json:"name"`
	Env        string            `json:"env"`
	Labels     map[string]string `json:"labels"`
	ExpiresAt  time.Time         `json:"expires_at"`
	UsedAt     *time.Time        `json:"used_at"`
	UsedHostID *int64            `json:"used_host_id"`
	CreatedBy  string            `json:"created_by"`
	CreatedAt  time.Time         `json:"created_at"`
	Token      string            `json:"token,omitempty"` // plaintext, only in the create response
}

type Task struct {
	ID          int64      `json:"id"`
	HostID      int64      `json:"host_id"`
	Kind        string     `json:"kind"`
	BatchID     string     `json:"batch_id"`
	Command     string     `json:"command"`
	TimeoutSec  int        `json:"timeout_sec"`
	Status      string     `json:"status"`
	ExitCode    *int       `json:"exit_code"`
	Output      string     `json:"output"`
	Error       string     `json:"error"`
	TriggeredBy string     `json:"triggered_by"`
	TicketID    *int64     `json:"ticket_id"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
}

const (
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusFailed  = "failed"
	StatusTimeout = "timeout"
)

func randomSecret(prefix string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

type store struct{ db *pgxpool.Pool }

const hostCols = `id,name,ip,env,labels,facts,agent_version,secret_hash<>'',online,last_seen_at,registered_at,created_by,created_at,updated_at`

func scanHost(row pgx.Row) (*Host, error) {
	var h Host
	var labels, facts []byte
	if err := row.Scan(&h.ID, &h.Name, &h.IP, &h.Env, &labels, &facts, &h.AgentVersion, &h.Enrolled, &h.Online,
		&h.LastSeenAt, &h.RegisteredAt, &h.CreatedBy, &h.CreatedAt, &h.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("host not found")
		}
		return nil, err
	}
	_ = json.Unmarshal(labels, &h.Labels)
	_ = json.Unmarshal(facts, &h.Facts)
	if h.Labels == nil {
		h.Labels = map[string]string{}
	}
	h.Online = h.Online && h.LastSeenAt != nil && time.Since(*h.LastSeenAt) < onlineWindow
	return &h, nil
}

type hostFilter struct {
	Env, Keyword string
	OnlineOnly   bool
	IDs          []int64
}

func (s *store) listHosts(ctx context.Context, f hostFilter, p httpx.Page) ([]Host, int64, error) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.Env != "" {
		add("env = $%d", f.Env)
	}
	if f.Keyword != "" {
		add("(name ILIKE $%[1]d OR ip ILIKE $%[1]d OR facts->>'hostname' ILIKE $%[1]d)", "%"+f.Keyword+"%")
	}
	if f.OnlineOnly {
		add("online AND last_seen_at > now() - $%d::interval", fmt.Sprintf("%d seconds", int(onlineWindow.Seconds())))
	}
	if len(f.IDs) > 0 {
		add("id = ANY($%d)", f.IDs)
	}
	w := ""
	if len(where) > 0 {
		w = " WHERE " + strings.Join(where, " AND ")
	}
	var total int64
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM hosts_hosts"+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, p.Limit(), p.Offset())
	rows, err := s.db.Query(ctx, fmt.Sprintf("SELECT %s FROM hosts_hosts%s ORDER BY id LIMIT $%d OFFSET $%d", hostCols, w, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Host{}
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, *h)
	}
	return items, total, rows.Err()
}

func (s *store) getHost(ctx context.Context, id int64) (*Host, error) {
	return scanHost(s.db.QueryRow(ctx, "SELECT "+hostCols+" FROM hosts_hosts WHERE id=$1", id))
}

type hostInput struct {
	Name   string            `json:"name" binding:"required,max=128"`
	IP     string            `json:"ip" binding:"max=64"`
	Env    string            `json:"env" binding:"max=64"`
	Labels map[string]string `json:"labels"`
}

func jsonb(v any) []byte {
	b, _ := json.Marshal(v)
	if string(b) == "null" {
		return []byte("{}")
	}
	return b
}

func (s *store) createHost(ctx context.Context, in hostInput, by string) (*Host, error) {
	return scanHost(s.db.QueryRow(ctx, `INSERT INTO hosts_hosts (name,ip,env,labels,created_by) VALUES ($1,$2,$3,$4,$5) RETURNING `+hostCols,
		in.Name, in.IP, in.Env, jsonb(in.Labels), by))
}

func (s *store) updateHost(ctx context.Context, id int64, in hostInput) (*Host, error) {
	return scanHost(s.db.QueryRow(ctx, `UPDATE hosts_hosts SET name=$2, ip=$3, env=$4, labels=$5, updated_at=now() WHERE id=$1 RETURNING `+hostCols,
		id, in.Name, in.IP, in.Env, jsonb(in.Labels)))
}

func (s *store) deleteHost(ctx context.Context, id int64) error {
	ct, err := s.db.Exec(ctx, "DELETE FROM hosts_hosts WHERE id=$1", id)
	if err == nil && ct.RowsAffected() == 0 {
		return httpx.NotFound("host not found")
	}
	return err
}

// ---- enrolment -------------------------------------------------------------

type tokenInput struct {
	Name     string            `json:"name" binding:"max=128"`
	Env      string            `json:"env" binding:"max=64"`
	Labels   map[string]string `json:"labels"`
	TTLHours int               `json:"ttl_hours" binding:"min=0,max=720"`
}

const tokenCols = `id,name,env,labels,expires_at,used_at,used_host_id,created_by,created_at`

func scanToken(row pgx.Row) (*EnrollToken, error) {
	var t EnrollToken
	var labels []byte
	if err := row.Scan(&t.ID, &t.Name, &t.Env, &labels, &t.ExpiresAt, &t.UsedAt, &t.UsedHostID, &t.CreatedBy, &t.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("enrol token not found")
		}
		return nil, err
	}
	_ = json.Unmarshal(labels, &t.Labels)
	return &t, nil
}

func (s *store) createToken(ctx context.Context, in tokenInput, by string) (*EnrollToken, error) {
	if in.TTLHours == 0 {
		in.TTLHours = 24
	}
	plain := randomSecret("mxe_")
	t, err := scanToken(s.db.QueryRow(ctx, `INSERT INTO hosts_enroll_tokens (token_hash,name,env,labels,expires_at,created_by)
		VALUES ($1,$2,$3,$4,now()+make_interval(hours => $5),$6) RETURNING `+tokenCols,
		hashSecret(plain), in.Name, in.Env, jsonb(in.Labels), in.TTLHours, by))
	if err != nil {
		return nil, err
	}
	t.Token = plain
	return t, nil
}

func (s *store) listTokens(ctx context.Context) ([]EnrollToken, error) {
	rows, err := s.db.Query(ctx, "SELECT "+tokenCols+" FROM hosts_enroll_tokens ORDER BY id DESC LIMIT 200")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EnrollToken{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *store) deleteToken(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, "DELETE FROM hosts_enroll_tokens WHERE id=$1", id)
	return err
}

// validToken checks (without consuming) that a plaintext token is usable.
func (s *store) validToken(ctx context.Context, plain string) (*EnrollToken, error) {
	t, err := scanToken(s.db.QueryRow(ctx, "SELECT "+tokenCols+" FROM hosts_enroll_tokens WHERE token_hash=$1 AND used_at IS NULL AND expires_at>now()", hashSecret(plain)))
	if err != nil {
		return nil, errors.New("invalid, used or expired enrol token")
	}
	return t, nil
}

// enroll atomically consumes the token and creates the host with a fresh secret.
func (s *store) enroll(ctx context.Context, plain string, hello proto.Hello, remoteIP string) (*Host, string, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var tokID int64
	var name, env, createdBy string
	var labels []byte
	err = tx.QueryRow(ctx, `UPDATE hosts_enroll_tokens SET used_at=now() WHERE token_hash=$1 AND used_at IS NULL AND expires_at>now()
		RETURNING id,name,env,labels,created_by`, hashSecret(plain)).Scan(&tokID, &name, &env, &labels, &createdBy)
	if err != nil {
		return nil, "", errors.New("invalid, used or expired enrol token")
	}
	if name == "" {
		name = hello.Facts.Hostname
	}
	ip := primaryIP(hello.Facts.IPs, remoteIP)
	secret := randomSecret("mxh_")
	h, err := scanHost(tx.QueryRow(ctx, `INSERT INTO hosts_hosts (name,ip,env,labels,facts,agent_version,secret_hash,registered_at,last_seen_at,created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,now(),now(),$8) RETURNING `+hostCols,
		name, ip, env, labels, jsonb(hello.Facts), hello.AgentVersion, hashSecret(secret), createdBy))
	if err != nil {
		return nil, "", err
	}
	if _, err := tx.Exec(ctx, "UPDATE hosts_enroll_tokens SET used_host_id=$2 WHERE id=$1", tokID, h.ID); err != nil {
		return nil, "", err
	}
	return h, secret, tx.Commit(ctx)
}

func primaryIP(ips []string, fallback string) string {
	for _, ip := range ips {
		if !strings.Contains(ip, ":") && !strings.HasPrefix(ip, "127.") {
			return ip
		}
	}
	return fallback
}

// authAgent verifies a reconnecting agent's per-host secret.
func (s *store) authAgent(ctx context.Context, hostID int64, secret string) error {
	var ok bool
	err := s.db.QueryRow(ctx, "SELECT secret_hash=$2 AND secret_hash<>'' FROM hosts_hosts WHERE id=$1", hostID, hashSecret(secret)).Scan(&ok)
	if err != nil || !ok {
		return errors.New("invalid host credentials")
	}
	return nil
}

func (s *store) markOnline(ctx context.Context, id int64, hello proto.Hello) error {
	_, err := s.db.Exec(ctx, `UPDATE hosts_hosts SET online=true, last_seen_at=now(), facts=$2, agent_version=$3,
		ip=CASE WHEN ip='' THEN $4 ELSE ip END, updated_at=now() WHERE id=$1`, id, jsonb(hello.Facts), hello.AgentVersion, primaryIP(hello.Facts.IPs, ""))
	return err
}

func (s *store) heartbeat(ctx context.Context, id int64, f *proto.Facts) error {
	if f == nil {
		_, err := s.db.Exec(ctx, "UPDATE hosts_hosts SET online=true, last_seen_at=now() WHERE id=$1", id)
		return err
	}
	_, err := s.db.Exec(ctx, "UPDATE hosts_hosts SET online=true, last_seen_at=now(), facts=$2 WHERE id=$1", id, jsonb(f))
	return err
}

func (s *store) markOffline(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, "UPDATE hosts_hosts SET online=false WHERE id=$1", id)
	return err
}

func (s *store) resetOnline(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, "UPDATE hosts_hosts SET online=false WHERE online"); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, "UPDATE hosts_tasks SET status='failed', error='server restarted', finished_at=now() WHERE status='running'")
	return err
}

// ---- tasks -----------------------------------------------------------------

const taskCols = `id,host_id,kind,batch_id,command,timeout_sec,status,exit_code,output,error,triggered_by,ticket_id,started_at,finished_at`

func scanTask(row pgx.Row) (*Task, error) {
	var t Task
	if err := row.Scan(&t.ID, &t.HostID, &t.Kind, &t.BatchID, &t.Command, &t.TimeoutSec, &t.Status, &t.ExitCode, &t.Output,
		&t.Error, &t.TriggeredBy, &t.TicketID, &t.StartedAt, &t.FinishedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("task not found")
		}
		return nil, err
	}
	return &t, nil
}

func (s *store) createTask(ctx context.Context, t *Task) error {
	return s.db.QueryRow(ctx, `INSERT INTO hosts_tasks (host_id,kind,batch_id,command,timeout_sec,triggered_by,ticket_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id,status,started_at`,
		t.HostID, t.Kind, t.BatchID, t.Command, t.TimeoutSec, t.TriggeredBy, t.TicketID).Scan(&t.ID, &t.Status, &t.StartedAt)
}

func (s *store) finishTask(ctx context.Context, id int64, status string, exit *int, output, errMsg string) error {
	_, err := s.db.Exec(ctx, `UPDATE hosts_tasks SET status=$2, exit_code=$3, output=$4, error=$5, finished_at=now() WHERE id=$1 AND status='running'`,
		id, status, exit, strings.ToValidUTF8(output, "\uFFFD"), errMsg)
	return err
}

func (s *store) getTask(ctx context.Context, id int64) (*Task, error) {
	return scanTask(s.db.QueryRow(ctx, "SELECT "+taskCols+" FROM hosts_tasks WHERE id=$1", id))
}

func (s *store) listTasks(ctx context.Context, hostID int64, batchID string, p httpx.Page) ([]Task, int64, error) {
	w, arg := "host_id=$1", any(hostID)
	if batchID != "" {
		w, arg = "batch_id=$1", batchID
	}
	var total int64
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM hosts_tasks WHERE "+w, arg).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(ctx, "SELECT "+taskCols+" FROM hosts_tasks WHERE "+w+" ORDER BY id DESC LIMIT $2 OFFSET $3", arg, p.Limit(), p.Offset())
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *t)
	}
	return out, total, rows.Err()
}
