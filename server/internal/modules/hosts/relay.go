package hosts

// Multi-replica support for the host agent hub.
//
// Every replica keeps the WebSockets of the agents that connected to it. When
// an agent connects, the replica records itself as the agent's owner in the
// owner registry (PostgreSQL table hosts_agent_sessions) and renews that lease
// on a ticker; the claim is released when the socket closes and expires when
// renewals stop. A replica asked to run a task for an agent it does not hold
// looks up the owner and forwards the task over an HMAC-signed internal HTTP
// request (POST /api/v1/agent/relay). The owner runs the task on its local
// socket, persists the result as usual, and streams output and the final
// result back as NDJSON, so SSE streaming and synchronous waits keep working
// on the replica that received the API call. This mirrors the cluster tunnel
// relay (internal/tunnel/relay.go): same MATRIX_POD_IP self address and the
// same "PG ownership + signed peer request" model.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/hosts/proto"
	dbstore "github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

const (
	relayPath       = "/agent/relay" // relative to the /api/v1 group
	headerRelayAuth = "X-Matrix-Host-Relay-Auth"
	relaySkew       = 60 * time.Second
	minRelayKeyLen  = 32
	relayMaxBody    = 4 << 20
	relayGrace      = 15 * time.Second // requester-side slack beyond the owner's own deadline
	relayKeepalive  = 15 * time.Second
	registryTimeout = 5 * time.Second
	defaultLease    = 15 * time.Second
	defaultStale    = 45 * time.Second
)

var (
	errSuperseded = errors.New("agent session was superseded")
	// errOwnerUnreachable: the agent is connected to another replica that this
	// replica cannot reach. Shown to operators in task errors.
	errOwnerUnreachable = errors.New("主机 Agent 连接在其他服务副本上，但该副本当前不可达，请稍后重试")
)

// errRelayBroken is recorded when the owner replica's stream ends without a result.
const errRelayBroken = "与主机 Agent 所在服务副本的连接中断，任务结果以该副本记录为准"

// agentOwner is the replica holding an agent's socket.
type agentOwner struct {
	SessionID string
	Addr      string // relay base URL of the owner, "" when it has no MATRIX_POD_IP
	Fresh     bool   // lease renewed within the stale window (evaluated by the registry clock)
}

func (o agentOwner) usable() bool { return o.Fresh && o.SessionID != "" }

// ownerRegistry records which replica owns each host agent connection.
type ownerRegistry interface {
	claim(ctx context.Context, hostID int64, o agentOwner) error              // upsert; newest claim wins
	renew(ctx context.Context, hostID int64, sessionID string) error          // errSuperseded when not current
	release(ctx context.Context, hostID int64, sessionID string) (bool, error) // true when this session was current
	lookup(ctx context.Context, hostID int64) (agentOwner, error)             // errOffline when none
}

// relayConfig enables cross-replica routing on a Hub.
type relayConfig struct {
	reg      ownerRegistry
	selfAddr string
	key      []byte
	client   *http.Client
	lease    time.Duration
	replay   *replayCache
}

func (c *relayConfig) canSend() bool { return len(c.key) >= minRelayKeyLen }

func (c *relayConfig) canServe() bool { return c.selfAddr != "" && len(c.key) >= minRelayKeyLen }

func (c *relayConfig) lookupOwner(ctx context.Context, hostID int64) (agentOwner, error) {
	ctx, cancel := context.WithTimeout(ctx, registryTimeout)
	defer cancel()
	return c.reg.lookup(ctx, hostID)
}

// configureRelay turns on owner registration and cross-replica forwarding.
// selfAddr is this replica's base URL ("http://<MATRIX_POD_IP>:<port>"); an
// empty selfAddr still records ownership, but peers cannot forward to us.
func (h *Hub) configureRelay(reg ownerRegistry, selfAddr string, key []byte) {
	if reg == nil {
		return
	}
	h.relay = &relayConfig{
		reg:      reg,
		selfAddr: strings.TrimRight(strings.TrimSpace(selfAddr), "/"),
		key:      key,
		lease:    defaultLease,
		replay:   newReplayCache(),
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 nil, // pod-to-pod traffic must not go through an egress proxy
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ResponseHeaderTimeout: 20 * time.Second,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
		}},
	}
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---- ownership lifecycle -----------------------------------------------------

func (h *Hub) claimOwner(c *agentConn) {
	if h.relay == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), registryTimeout)
	defer cancel()
	if err := h.relay.reg.claim(ctx, c.hostID, agentOwner{SessionID: c.sessionID, Addr: h.relay.selfAddr}); err != nil {
		h.log.Warn("host agent owner claim failed", "host_id", c.hostID, "err", err)
	}
}

// leaseLoop renews the owner claim while the socket is open. A superseded
// claim (the agent reconnected elsewhere, or the host was deleted) closes the
// local socket so only one replica ever drives an agent.
func (h *Hub) leaseLoop(c *agentConn) {
	if h.relay == nil {
		return
	}
	t := time.NewTicker(h.relay.lease)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), registryTimeout)
			err := h.relay.reg.renew(ctx, c.hostID, c.sessionID)
			cancel()
			switch {
			case err == nil:
			case errors.Is(err, errSuperseded):
				h.log.Info("host agent session superseded", "host_id", c.hostID)
				c.close()
				return
			default:
				h.log.Warn("host agent lease renewal failed", "host_id", c.hostID, "err", err)
			}
		}
	}
}

// releaseOwner drops the claim; true when this connection still held it (or
// no registry is configured).
func (h *Hub) releaseOwner(c *agentConn) bool {
	if h.relay == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), registryTimeout)
	defer cancel()
	ok, err := h.relay.reg.release(ctx, c.hostID, c.sessionID)
	if err != nil {
		h.log.Warn("host agent owner release failed", "host_id", c.hostID, "err", err)
		return false
	}
	return ok
}

// ---- relay wire format ----------------------------------------------------------

type relayRequest struct {
	Op        string          `json:"op"` // "dispatch"
	HostID    int64           `json:"host_id"`
	TaskID    int64           `json:"task_id"`
	Transient bool            `json:"transient,omitempty"`
	FrameType string          `json:"frame_type"`
	Payload   json.RawMessage `json:"payload"`
	TimeoutMS int64           `json:"timeout_ms"`
}

type relayEvent struct {
	Type     string  `json:"type"` // output | ping | done
	Stream   string  `json:"stream,omitempty"`
	Data     string  `json:"data,omitempty"`
	Status   string  `json:"status,omitempty"`
	ExitCode *int    `json:"exit_code,omitempty"`
	Error    string  `json:"error,omitempty"`
	Output   *string `json:"output,omitempty"` // full output, transient runs only
}

type relayFailure struct {
	Message string `json:"message"`
	Offline bool   `json:"offline,omitempty"`
}

// signRelay builds "<unix>:<base64url(mac)>", mac = HMAC-SHA256(key, "hosts-relay|<unix>|<hex(sha256(body))>").
func signRelay(key, body []byte, now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	return ts + ":" + base64.RawURLEncoding.EncodeToString(relayMAC(key, body, ts))
}

func relayMAC(key, body []byte, ts string) []byte {
	sum := sha256.Sum256(body)
	m := hmac.New(sha256.New, key)
	m.Write([]byte("hosts-relay|" + ts + "|" + hex.EncodeToString(sum[:])))
	return m.Sum(nil)
}

func verifyRelay(key, body []byte, value string, now time.Time) bool {
	if len(key) < minRelayKeyLen {
		return false
	}
	ts, sig, ok := strings.Cut(strings.TrimSpace(value), ":")
	if !ok || ts == "" || sig == "" {
		return false
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if d := now.Sub(time.Unix(unix, 0)); d > relaySkew || d < -relaySkew {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	return hmac.Equal(got, relayMAC(key, body, ts))
}

// replayCache rejects a signed request seen before within the skew window, so
// a captured dispatch cannot be replayed to run a command twice.
type replayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newReplayCache() *replayCache { return &replayCache{seen: map[string]time.Time{}} }

func (r *replayCache) add(sig string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, t := range r.seen {
		if now.Sub(t) > 2*relaySkew {
			delete(r.seen, k)
		}
	}
	if _, dup := r.seen[sig]; dup {
		return false
	}
	r.seen[sig] = now
	return true
}

// ---- requester side ---------------------------------------------------------------

// startRemote forwards a task to the replica that owns the host's agent and
// mirrors its output and result into a local taskRun.
func (h *Hub) startRemote(spec runSpec) (*taskRun, error) {
	rc := h.relay
	owner, err := rc.lookupOwner(context.Background(), spec.hostID)
	if errors.Is(err, errOffline) || (err == nil && !owner.usable()) {
		return nil, errOffline
	}
	if err != nil {
		return nil, fmt.Errorf("查询主机 Agent 所在服务副本失败：%w", err)
	}
	if owner.Addr == "" || owner.Addr == rc.selfAddr || !rc.canSend() {
		// The claim points at a replica without a relay address (MATRIX_POD_IP
		// unset), or at ourselves although the socket is gone (stale claim).
		if owner.Addr == rc.selfAddr && rc.selfAddr != "" {
			return nil, errOffline
		}
		return nil, errOwnerUnreachable
	}
	payload, err := json.Marshal(spec.payload)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(relayRequest{Op: "dispatch", HostID: spec.hostID, TaskID: spec.taskID, Transient: spec.transient,
		FrameType: spec.frameType, Payload: payload, TimeoutMS: spec.timeout.Milliseconds()})
	if err != nil {
		return nil, err
	}
	deadline := spec.timeout + resultGrace + relayGrace
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, owner.Addr+"/api/v1"+relayPath, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerRelayAuth, signRelay(rc.key, body, time.Now()))
	resp, err := rc.client.Do(req)
	if err != nil {
		cancel()
		h.log.Warn("host agent relay failed", "host_id", spec.hostID, "owner", owner.Addr, "err", err)
		return nil, errOwnerUnreachable
	}
	if resp.StatusCode != http.StatusOK {
		var f relayFailure
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&f)
		_ = resp.Body.Close()
		cancel()
		if f.Offline {
			return nil, errOffline
		}
		h.log.Warn("host agent relay rejected", "host_id", spec.hostID, "owner", owner.Addr, "status", resp.StatusCode)
		return nil, fmt.Errorf("主机 Agent 所在服务副本拒绝了转发请求（HTTP %d）", resp.StatusCode)
	}
	t := newTaskRun(spec)
	t.remote, t.cancel = true, cancel
	h.mu.Lock()
	if _, dup := h.tasks[spec.taskID]; dup {
		h.mu.Unlock()
		_ = resp.Body.Close()
		cancel()
		return nil, errors.New("task is already running")
	}
	h.tasks[spec.taskID] = t
	h.mu.Unlock()
	msg := spec.timeoutMsg
	t.mu.Lock()
	t.timer = time.AfterFunc(deadline, func() { h.finish(spec.taskID, StatusFailed, nil, msg) })
	t.mu.Unlock()
	go h.consumeRelay(t, resp.Body)
	return t, nil
}

func (h *Hub) consumeRelay(t *taskRun, body io.ReadCloser) {
	defer body.Close()
	dec := json.NewDecoder(body)
	for {
		var ev relayEvent
		if err := dec.Decode(&ev); err != nil {
			// Owner went away mid-task. finish() is a no-op when the run was
			// already ended locally (timeout / cancel), and the persisted row
			// is only updated if the owner did not record a result first.
			h.finish(t.id, StatusFailed, nil, errRelayBroken)
			return
		}
		switch ev.Type {
		case "output":
			t.append(ev.Stream, ev.Data)
		case "done":
			if ev.Output != nil {
				t.mu.Lock()
				t.out, t.truncated = []byte(*ev.Output), false
				t.mu.Unlock()
			}
			status := ev.Status
			if status == "" {
				status = StatusFailed
			}
			h.finishRun(t.id, status, ev.ExitCode, ev.Error, false)
			return
		}
	}
}

// ---- owner side ---------------------------------------------------------------------

// ServeRelay handles a peer replica's forwarded task (public route, HMAC-authenticated).
func (h *Hub) ServeRelay(c *gin.Context) {
	rc := h.relay
	if rc == nil || !rc.canServe() {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, relayMaxBody+1))
	if err != nil || len(body) > relayMaxBody {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, relayFailure{Message: "relay request too large"})
		return
	}
	auth := c.GetHeader(headerRelayAuth)
	now := time.Now()
	if !verifyRelay(rc.key, body, auth, now) || !rc.replay.add(auth, now) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, relayFailure{Message: "unauthorized"})
		return
	}
	var req relayRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Op != "dispatch" {
		c.AbortWithStatusJSON(http.StatusBadRequest, relayFailure{Message: "invalid relay request"})
		return
	}
	h.serveRelayDispatch(c, req)
}

func (h *Hub) serveRelayDispatch(c *gin.Context, req relayRequest) {
	if req.HostID < 1 || (req.FrameType != proto.TypeExec && req.FrameType != proto.TypeFilePush) || req.TimeoutMS <= 0 ||
		(!req.Transient && req.TaskID < 1) || len(req.Payload) == 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest, relayFailure{Message: "invalid relay request"})
		return
	}
	timeout := time.Duration(req.TimeoutMS) * time.Millisecond
	if timeout > (maxTimeout+60)*time.Second {
		timeout = (maxTimeout + 60) * time.Second
	}
	h.mu.Lock()
	conn := h.conns[req.HostID]
	h.mu.Unlock()
	if conn == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, relayFailure{Message: errOffline.Error(), Offline: true})
		return
	}
	id := req.TaskID
	if req.Transient {
		id = -diagnosticTaskID.Add(1) // transient ids are per replica; never reuse the requester's
	}
	payload, err := rewriteTaskID(req.Payload, id)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, relayFailure{Message: "invalid relay payload"})
		return
	}
	sub := make(chan Event, 1024)
	t, err := h.startLocal(conn, runSpec{hostID: req.HostID, taskID: id, frameType: req.FrameType, payload: payload,
		timeout: timeout, transient: req.Transient, timeoutMsg: "no result from agent", sub: sub})
	if err != nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, relayFailure{Message: err.Error(), Offline: errors.Is(err, errOffline)})
		return
	}
	w := c.Writer
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	w.Flush()
	enc := json.NewEncoder(w)
	write := func(ev relayEvent) bool {
		if err := enc.Encode(ev); err != nil {
			return false
		}
		w.Flush()
		return true
	}
	detach := func() {
		t.mu.Lock()
		if t.subs != nil {
			delete(t.subs, sub)
		}
		t.mu.Unlock()
	}
	keep := time.NewTicker(relayKeepalive)
	defer keep.Stop()
	ctx := c.Request.Context()
	for {
		select {
		case ev, ok := <-sub:
			if !ok {
				t.mu.Lock()
				done := relayEvent{Type: "done", Status: t.status, ExitCode: t.exit, Error: t.errMsg}
				if t.transient {
					out := string(t.out)
					done.Output = &out
				}
				t.mu.Unlock()
				_ = write(done)
				return
			}
			if !write(relayEvent{Type: "output", Stream: ev.Stream, Data: ev.Data}) {
				detach()
				return
			}
		case <-keep.C:
			if !write(relayEvent{Type: "ping"}) {
				detach()
				return
			}
		case <-ctx.Done():
			detach() // requester left; the task keeps running here and is persisted
			return
		}
	}
}

func rewriteTaskID(raw json.RawMessage, id int64) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, errors.New("payload must be an object")
	}
	m["task_id"] = json.RawMessage(strconv.FormatInt(id, 10))
	b, err := json.Marshal(m)
	return b, err
}

// ---- PostgreSQL owner registry -------------------------------------------------------

type pgOwnerRegistry struct {
	db         dbstore.DB
	staleAfter time.Duration
}

func (r pgOwnerRegistry) stale() time.Duration {
	if r.staleAfter > 0 {
		return r.staleAfter
	}
	return defaultStale
}

func (r pgOwnerRegistry) claim(ctx context.Context, hostID int64, o agentOwner) error {
	_, err := r.db.Exec(ctx, `INSERT INTO hosts_agent_sessions(host_id,session_id,owner_addr,claimed_at,seen_at)
		VALUES($1,$2,$3,now(),now())
		ON CONFLICT (host_id) DO UPDATE SET session_id=EXCLUDED.session_id, owner_addr=EXCLUDED.owner_addr,
		claimed_at=now(), seen_at=now()`, hostID, o.SessionID, o.Addr)
	return err
}

func (r pgOwnerRegistry) renew(ctx context.Context, hostID int64, sessionID string) error {
	tag, err := r.db.Exec(ctx, `UPDATE hosts_agent_sessions SET seen_at=now() WHERE host_id=$1 AND session_id=$2`, hostID, sessionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errSuperseded
	}
	return nil
}

func (r pgOwnerRegistry) release(ctx context.Context, hostID int64, sessionID string) (bool, error) {
	tag, err := r.db.Exec(ctx, `DELETE FROM hosts_agent_sessions WHERE host_id=$1 AND session_id=$2`, hostID, sessionID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r pgOwnerRegistry) lookup(ctx context.Context, hostID int64) (agentOwner, error) {
	var o agentOwner
	err := r.db.QueryRow(ctx, `SELECT session_id, owner_addr, seen_at > now() - make_interval(secs => $2)
		FROM hosts_agent_sessions WHERE host_id=$1`, hostID, r.stale().Seconds()).Scan(&o.SessionID, &o.Addr, &o.Fresh)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentOwner{}, errOffline
	}
	return o, err
}

// hostsRelayKey returns the HMAC key shared by all replicas, creating it once.
func hostsRelayKey(ctx context.Context, db dbstore.DB, cipher *crypto.Cipher) ([]byte, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	aad := []byte("hosts-agent:relay-key")
	enc, err := cipher.EncryptBytes(buf, aad)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(ctx, `INSERT INTO hosts_relay_keys(name,secret_enc) VALUES('relay',$1) ON CONFLICT (name) DO NOTHING`, enc); err != nil {
		return nil, err
	}
	var stored string
	if err := db.QueryRow(ctx, `SELECT secret_enc FROM hosts_relay_keys WHERE name='relay'`).Scan(&stored); err != nil {
		return nil, err
	}
	return cipher.DecryptBytes(stored, aad)
}

// resetOnlineAtStart marks hosts offline and fails running tasks at startup,
// except for hosts whose agent holds a fresh claim on another replica.
func (s *store) resetOnlineAtStart(ctx context.Context, selfAddr string) error {
	const othersFresh = `SELECT host_id FROM hosts_agent_sessions WHERE seen_at > now() - make_interval(secs => $1) AND owner_addr <> $2 AND owner_addr <> ''`
	stale := defaultStale.Seconds()
	if _, err := s.db.Exec(ctx, `DELETE FROM hosts_agent_sessions WHERE owner_addr = $1 OR seen_at <= now() - make_interval(secs => $2)`, selfAddr, stale); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `UPDATE hosts_hosts SET online=false WHERE online AND id NOT IN (`+othersFresh+`)`, stale, selfAddr); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `UPDATE hosts_tasks SET status='failed', error='server restarted', finished_at=now()
		WHERE status='running' AND host_id NOT IN (`+othersFresh+`)`, stale, selfAddr)
	return err
}

// sweep expires abandoned ownership claims and tasks whose owner replica died
// before recording a result. Safe to run on every replica.
func (s *store) sweep(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM hosts_agent_sessions WHERE seen_at < now() - interval '10 minutes'`); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `UPDATE hosts_tasks SET status='failed', error='agent did not return a result (server replica lost)', finished_at=now()
		WHERE status='running' AND started_at < now() - make_interval(secs => timeout_sec + 180)`)
	return err
}
