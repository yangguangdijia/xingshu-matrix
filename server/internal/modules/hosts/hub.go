package hosts

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/hosts/proto"
)

const (
	helloTimeout = 15 * time.Second
	readTimeout  = 90 * time.Second
	pingInterval = 30 * time.Second
	writeTimeout = 10 * time.Second
	resultGrace  = 30 * time.Second // extra wait beyond the command timeout before failing a task
)

var errOffline = errors.New("主机代理不在线，请确认 Agent 正在运行且能连接平台")

// agentStore is the persistence the hub needs (implemented by *store; faked in tests).
type agentStore interface {
	validToken(ctx context.Context, plain string) (*EnrollToken, error)
	enroll(ctx context.Context, plain string, hello proto.Hello, remoteIP string) (*Host, string, error)
	authAgent(ctx context.Context, hostID int64, secret string) error
	markOnline(ctx context.Context, id int64, hello proto.Hello) error
	heartbeat(ctx context.Context, id int64, f *proto.Facts) error
	markOffline(ctx context.Context, id int64) error
	finishTask(ctx context.Context, id int64, status string, exit *int, output, errMsg string) error
}

// Event is streamed to SSE subscribers of a running task.
type Event struct {
	Stream string `json:"stream"`
	Data   string `json:"data"`
}

type agentConn struct {
	hostID    int64
	sessionID string
	ws        *websocket.Conn
	send      chan []byte
	done      chan struct{}
	once      sync.Once
}

func (c *agentConn) close() { c.once.Do(func() { close(c.done); _ = c.ws.Close() }) }

func (c *agentConn) enqueue(b []byte) error {
	select {
	case c.send <- b:
		return nil
	case <-c.done:
		return errOffline
	case <-time.After(writeTimeout):
		return errors.New("agent send queue full")
	}
}

type taskRun struct {
	id, hostID int64
	mu         sync.Mutex
	out        []byte
	truncated  bool
	subs       map[chan Event]struct{}
	done       chan struct{}
	timer      *time.Timer
	transient  bool
	remote     bool               // relayed to the owner replica; the owner persists the result
	cancel     context.CancelFunc // aborts the relay request of a remote run
	status     string
	exit       *int
	errMsg     string
}

func (t *taskRun) append(stream, data string) {
	data = redactTaskText(data)
	t.mu.Lock()
	defer t.mu.Unlock()
	if room := proto.MaxOutput - len(t.out); room > 0 {
		if len(data) > room {
			t.out = append(t.out, data[:room]...)
			t.truncated = true
		} else {
			t.out = append(t.out, data...)
		}
	} else {
		t.truncated = true
	}
	for ch := range t.subs {
		select {
		case ch <- Event{Stream: stream, Data: data}:
		default: // slow subscriber: drop; full output is still persisted
		}
	}
}

// Hub holds this replica's live agent connections and in-flight tasks. With a
// relay configured (see relay.go) it records which replica owns each agent in
// the owner registry and forwards tasks for agents connected elsewhere.
type Hub struct {
	st       agentStore
	log      *slog.Logger
	upgrader websocket.Upgrader
	mu       sync.Mutex
	conns    map[int64]*agentConn
	tasks    map[int64]*taskRun
	relay    *relayConfig // nil: single replica, local connections only
}

var diagnosticTaskID atomic.Int64

func newHub(st agentStore, log *slog.Logger) *Hub {
	return &Hub{
		st: st, log: log,
		// Agents are not browsers: no Origin check needed; auth is token/secret based.
		upgrader: websocket.Upgrader{ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10, CheckOrigin: func(*http.Request) bool { return true }},
		conns:    map[int64]*agentConn{},
		tasks:    map[int64]*taskRun{},
	}
}

// Connected reports whether the host's agent is connected to this replica.
func (h *Hub) Connected(id int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.conns[id]
	return ok
}

// Online reports whether the host's agent is connected to this replica or,
// with a relay configured, holds a fresh claim on another replica.
func (h *Hub) Online(ctx context.Context, id int64) bool {
	if h.Connected(id) {
		return true
	}
	if h.relay == nil {
		return false
	}
	o, err := h.relay.lookupOwner(ctx, id)
	if err != nil || !o.usable() {
		return false
	}
	// A fresh claim naming this replica without a local socket is stale.
	return h.relay.selfAddr == "" || o.Addr != h.relay.selfAddr
}

// ServeWS is mounted on the public group: GET /agent/ws.
// Enrolment: ?token=<one-time enrol token>. Reconnect: Authorization: Bearer <secret> + X-Matrix-Host-ID.
func (h *Hub) ServeWS(c *gin.Context) {
	ctx := c.Request.Context()
	token := c.Query("token")
	var hostID int64
	if token != "" {
		if _, err := h.st.validToken(ctx, token); err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 401, "message": err.Error()})
			return
		}
	} else {
		hostID, _ = strconv.ParseInt(c.GetHeader(proto.HeaderHostID), 10, 64)
		secret := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		if hostID == 0 || secret == "" || h.st.authAgent(ctx, hostID, secret) != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "invalid host credentials"})
			return
		}
	}
	ws, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(4 << 20)
	_ = ws.SetReadDeadline(time.Now().Add(helloTimeout))
	var env proto.Envelope
	var hello proto.Hello
	if err := ws.ReadJSON(&env); err != nil || env.Type != proto.TypeHello || json.Unmarshal(env.Data, &hello) != nil {
		h.closeWith(ws, "expected hello frame")
		return
	}
	bg := context.Background()
	if token != "" {
		host, secret, err := h.st.enroll(bg, token, hello, c.ClientIP())
		if err != nil {
			h.closeWith(ws, err.Error())
			return
		}
		h.log.Info("agent enrolled", "host_id", host.ID, "name", host.Name)
		b, _ := proto.Encode(proto.TypeWelcome, proto.Welcome{HostID: host.ID, Secret: secret})
		_ = ws.SetWriteDeadline(time.Now().Add(writeTimeout))
		_ = ws.WriteMessage(websocket.TextMessage, b)
		_ = ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "enrolled, reconnect with secret"))
		_ = ws.Close()
		return
	}
	if err := h.st.markOnline(bg, hostID, hello); err != nil {
		h.closeWith(ws, "internal error")
		return
	}
	conn := &agentConn{hostID: hostID, sessionID: newSessionID(), ws: ws, send: make(chan []byte, 256), done: make(chan struct{})}
	h.mu.Lock()
	if old := h.conns[hostID]; old != nil {
		old.close() // newest connection wins
	}
	h.conns[hostID] = conn
	h.mu.Unlock()
	h.claimOwner(conn)
	h.log.Info("agent connected", "host_id", hostID, "version", hello.AgentVersion)
	go h.writer(conn)
	go h.leaseLoop(conn)
	b, _ := proto.Encode(proto.TypeWelcome, proto.Welcome{HostID: hostID})
	_ = conn.enqueue(b)
	h.reader(conn)

	conn.close()
	h.mu.Lock()
	stillCurrent := h.conns[hostID] == conn
	if stillCurrent {
		delete(h.conns, hostID)
	}
	var orphaned []int64
	for id, t := range h.tasks {
		if t.hostID == hostID && stillCurrent {
			orphaned = append(orphaned, id)
		}
	}
	h.mu.Unlock()
	released := h.releaseOwner(conn)
	if stillCurrent {
		if released {
			// Only mark offline when this replica still held the agent's claim:
			// if it already reconnected through another replica, it stays online.
			_ = h.st.markOffline(bg, hostID)
		}
		for _, id := range orphaned {
			h.finish(id, StatusFailed, nil, "agent disconnected")
		}
		h.log.Info("agent disconnected", "host_id", hostID)
	}
}

func (h *Hub) closeWith(ws *websocket.Conn, msg string) {
	b, _ := proto.Encode(proto.TypeError, proto.Error{Message: msg})
	_ = ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	_ = ws.WriteMessage(websocket.TextMessage, b)
	_ = ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, msg))
	_ = ws.Close()
}

func (h *Hub) writer(c *agentConn) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case b := <-c.send:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := c.ws.WriteMessage(websocket.TextMessage, b); err != nil {
				c.close()
				return
			}
		case <-t.C:
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
				c.close()
				return
			}
		}
	}
}

func (h *Hub) reader(c *agentConn) {
	c.ws.SetPongHandler(func(string) error { return c.ws.SetReadDeadline(time.Now().Add(readTimeout)) })
	for {
		_ = c.ws.SetReadDeadline(time.Now().Add(readTimeout))
		var env proto.Envelope
		if err := c.ws.ReadJSON(&env); err != nil {
			return
		}
		ctx := context.Background()
		switch env.Type {
		case proto.TypeHeartbeat:
			var hb proto.Heartbeat
			if json.Unmarshal(env.Data, &hb) == nil {
				if err := h.st.heartbeat(ctx, c.hostID, hb.Facts); err != nil {
					h.log.Warn("heartbeat store failed", "host_id", c.hostID, "err", err)
				}
			}
		case proto.TypeExecOutput:
			var o proto.ExecOutput
			if json.Unmarshal(env.Data, &o) == nil {
				if t := h.task(o.TaskID, c.hostID); t != nil {
					t.append(o.Stream, o.Data)
				}
			}
		case proto.TypeExecResult:
			var r proto.ExecResult
			if json.Unmarshal(env.Data, &r) == nil && h.task(r.TaskID, c.hostID) != nil {
				status := StatusSuccess
				switch {
				case r.TimedOut:
					status = StatusTimeout
				case r.ExitCode != 0 || r.Error != "":
					status = StatusFailed
				}
				code := r.ExitCode
				h.finish(r.TaskID, status, &code, r.Error)
			}
		case proto.TypeFileResult:
			var r proto.FileResult
			if json.Unmarshal(env.Data, &r) == nil {
				if t := h.task(r.TaskID, c.hostID); t != nil {
					if r.Error != "" {
						h.finish(r.TaskID, StatusFailed, nil, r.Error)
					} else {
						t.append("stdout", "wrote "+strconv.Itoa(r.Bytes)+" bytes")
						h.finish(r.TaskID, StatusSuccess, nil, "")
					}
				}
			}
		}
	}
}

func (h *Hub) task(id, hostID int64) *taskRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t := h.tasks[id]; t != nil && t.hostID == hostID {
		return t
	}
	return nil
}

// runSpec describes one task to start on a host's agent.
type runSpec struct {
	hostID, taskID int64
	frameType      string
	payload        any
	timeout        time.Duration
	transient      bool       // not persisted to the task history (diagnostics)
	timeoutMsg     string     // error recorded when no result arrives in time
	sub            chan Event // optional subscriber attached before the frame is sent
}

// Dispatch sends an exec / file_push frame to the host's agent and tracks the
// task. When the agent is connected to another replica the task is relayed to
// that replica; the result is persisted by whichever replica holds the agent.
func (h *Hub) Dispatch(hostID, taskID int64, frameType string, payload any, timeout time.Duration) error {
	_, err := h.start(runSpec{hostID: hostID, taskID: taskID, frameType: frameType, payload: payload, timeout: timeout, timeoutMsg: "no result from agent"})
	return err
}

func (h *Hub) start(spec runSpec) (*taskRun, error) {
	h.mu.Lock()
	conn := h.conns[spec.hostID]
	h.mu.Unlock()
	if conn != nil {
		return h.startLocal(conn, spec)
	}
	if h.relay == nil {
		return nil, errOffline
	}
	return h.startRemote(spec)
}

func newTaskRun(spec runSpec) *taskRun {
	t := &taskRun{id: spec.taskID, hostID: spec.hostID, subs: map[chan Event]struct{}{}, done: make(chan struct{}), transient: spec.transient}
	if spec.sub != nil {
		t.subs[spec.sub] = struct{}{}
	}
	return t
}

func (h *Hub) startLocal(conn *agentConn, spec runSpec) (*taskRun, error) {
	t := newTaskRun(spec)
	h.mu.Lock()
	if _, dup := h.tasks[spec.taskID]; dup {
		h.mu.Unlock()
		return nil, errors.New("task is already running")
	}
	h.tasks[spec.taskID] = t
	h.mu.Unlock()
	b, err := proto.Encode(spec.frameType, spec.payload)
	if err == nil {
		err = conn.enqueue(b)
	}
	if err != nil {
		h.mu.Lock()
		if h.tasks[spec.taskID] == t {
			delete(h.tasks, spec.taskID)
		}
		h.mu.Unlock()
		return nil, err
	}
	msg := spec.timeoutMsg
	t.mu.Lock()
	t.timer = time.AfterFunc(spec.timeout+resultGrace, func() { h.finish(spec.taskID, StatusFailed, nil, msg) })
	t.mu.Unlock()
	return t, nil
}

// RunDiagnostic executes a fixed internal inspection without persisting raw output
// to the operator-visible host task history.
func (h *Hub) runDiagnostic(ctx context.Context, hostID int64, timeout time.Duration) (string, error) {
	if hostID < 1 {
		return "", errors.New("invalid host diagnostic request")
	}
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	id := -diagnosticTaskID.Add(1)
	t, err := h.start(runSpec{
		hostID: hostID, taskID: id, frameType: proto.TypeExec, transient: true, timeout: timeout,
		payload:    proto.Exec{TaskID: id, Command: hostDiagnosticCommand, TimeoutSec: int(timeout.Seconds())},
		timeoutMsg: "host diagnostic timed out",
	})
	if err != nil {
		return "", err
	}
	select {
	case <-t.done:
	case <-ctx.Done():
		h.finish(id, StatusFailed, nil, "host diagnostic cancelled")
		return "", ctx.Err()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != StatusSuccess {
		return "", errors.New("host diagnostic did not complete")
	}
	return string(t.out), nil
}

// finish persists the final state and releases waiters/subscribers. Idempotent.
func (h *Hub) finish(taskID int64, status string, exit *int, errMsg string) {
	h.finishRun(taskID, status, exit, errMsg, true)
}

// finishRun ends a tracked task. persist=false is used when the owner replica
// already persisted the result of a relayed task.
func (h *Hub) finishRun(taskID int64, status string, exit *int, errMsg string, persist bool) {
	h.mu.Lock()
	t := h.tasks[taskID]
	delete(h.tasks, taskID)
	h.mu.Unlock()
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.timer != nil {
		t.timer.Stop()
	}
	out := string(t.out)
	if t.truncated {
		out += "\n[output truncated]"
	}
	cancel := t.cancel
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if persist && !t.transient {
		// finishTask only updates a still-running row, so a relayed task the
		// owner already finished is never overwritten here.
		if err := h.st.finishTask(context.Background(), taskID, status, exit, out, errMsg); err != nil {
			h.log.Error("persist task result failed", "task_id", taskID, "err", err)
		}
	}
	t.mu.Lock()
	t.status, t.exit, t.errMsg = status, exit, errMsg
	for ch := range t.subs {
		close(ch)
	}
	t.subs = nil
	t.mu.Unlock()
	close(t.done)
}

// Subscribe returns output so far plus a channel of further output; the channel
// is closed when the task finishes. ok=false means the task is not running.
func (h *Hub) Subscribe(taskID int64) (snapshot string, ch chan Event, cancel func(), ok bool) {
	h.mu.Lock()
	t := h.tasks[taskID]
	h.mu.Unlock()
	if t == nil {
		return "", nil, func() {}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.subs == nil {
		return "", nil, func() {}, false
	}
	ch = make(chan Event, 512)
	t.subs[ch] = struct{}{}
	return string(t.out), ch, func() {
		t.mu.Lock()
		if t.subs != nil {
			if _, ok := t.subs[ch]; ok {
				delete(t.subs, ch)
				close(ch)
			}
		}
		t.mu.Unlock()
	}, true
}

// Wait blocks until the task finishes (or ctx ends). Returns immediately if it is not running.
func (h *Hub) Wait(ctx context.Context, taskID int64) error {
	h.mu.Lock()
	t := h.tasks[taskID]
	h.mu.Unlock()
	if t == nil {
		return nil
	}
	select {
	case <-t.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close drops every agent connection (shutdown).
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.conns {
		c.close()
	}
}

// Disconnect closes a host's agent connection (e.g. host deleted).
func (h *Hub) Disconnect(id int64) {
	h.mu.Lock()
	c := h.conns[id]
	h.mu.Unlock()
	if c != nil {
		c.close()
	}
}
