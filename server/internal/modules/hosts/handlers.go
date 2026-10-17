package hosts

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/hosts/proto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// ---- hosts CRUD ----------------------------------------------------------------

func (m *Module) listHosts(c *gin.Context) {
	p := httpx.ParsePage(c)
	online, _ := strconv.ParseBool(c.Query("online"))
	items, total, err := m.st.listHosts(c, hostFilter{Env: c.Query("env"), Keyword: c.Query("keyword"), OnlineOnly: online}, p)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, httpx.NewPageResult(items, total, p))
}

func (m *Module) hostParam(c *gin.Context) (*Host, bool) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return nil, false
	}
	h, err := m.st.getHost(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return nil, false
	}
	return h, true
}

func (m *Module) getHost(c *gin.Context) {
	if h, ok := m.hostParam(c); ok {
		httpx.OK(c, h)
	}
}

func (m *Module) createHost(c *gin.Context) {
	var in hostInput
	if !httpx.Bind(c, &in) {
		return
	}
	pr := rbac.PrincipalFrom(c)
	if !pr.CanEnv(in.Env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope: "+in.Env))
		return
	}
	h, err := m.st.createHost(c, in, who(pr))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "host", strconv.FormatInt(h.ID, 10))
	httpx.Created(c, h)
}

func (m *Module) updateHost(c *gin.Context) {
	h, ok := m.hostParam(c)
	if !ok {
		return
	}
	var in hostInput
	if !httpx.Bind(c, &in) {
		return
	}
	pr := rbac.PrincipalFrom(c)
	if !pr.CanEnv(h.Env) || !pr.CanEnv(in.Env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope"))
		return
	}
	out, err := m.st.updateHost(c, h.ID, in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "host", strconv.FormatInt(h.ID, 10))
	httpx.OK(c, out)
}

func (m *Module) deleteHost(c *gin.Context) {
	h, ok := m.hostParam(c)
	if !ok {
		return
	}
	if !rbac.PrincipalFrom(c).CanEnv(h.Env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope: "+h.Env))
		return
	}
	if err := m.st.deleteHost(c, h.ID); err != nil {
		httpx.Fail(c, err)
		return
	}
	m.hub.Disconnect(h.ID) // its secret is gone, the agent can no longer reconnect
	audit.SetResource(c, "host", strconv.FormatInt(h.ID, 10))
	httpx.OK(c, gin.H{"id": h.ID})
}

// ---- enrolment tokens ----------------------------------------------------------

func (m *Module) createToken(c *gin.Context) {
	var in tokenInput
	if !httpx.Bind(c, &in) {
		return
	}
	pr := rbac.PrincipalFrom(c)
	if !pr.CanEnv(in.Env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope: "+in.Env))
		return
	}
	t, err := m.st.createToken(c, in, who(pr))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "host_enroll_token", strconv.FormatInt(t.ID, 10))
	scheme := "http"
	if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	httpx.Created(c, gin.H{"token": t, "install_command": fmt.Sprintf("matrix-agent -server %s://%s -token %s", scheme, c.Request.Host, t.Token)})
}

func (m *Module) listTokens(c *gin.Context) {
	ts, err := m.st.listTokens(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, ts)
}

func (m *Module) deleteToken(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	if err := m.st.deleteToken(c, id); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"id": id})
}

// ---- exec ----------------------------------------------------------------------

type execInput struct {
	Command    string `json:"command" binding:"required,max=65536"`
	TimeoutSec int    `json:"timeout_sec" binding:"min=0,max=3600"`
}

func wantsStream(c *gin.Context) bool {
	s, _ := strconv.ParseBool(c.Query("stream"))
	return s || strings.Contains(c.GetHeader("Accept"), "text/event-stream")
}

// POST /hosts/:id/exec — waits for the result (JSON), or streams it with ?stream=true / Accept: text/event-stream.
func (m *Module) exec(c *gin.Context) {
	h, ok := m.hostParam(c)
	if !ok {
		return
	}
	var in execInput
	if !httpx.Bind(c, &in) {
		return
	}
	audit.SetResource(c, "host", strconv.FormatInt(h.ID, 10))
	audit.SetDetail(c, gin.H{"command": trunc(in.Command, 500), "env": h.Env})
	res, err := m.execForPrincipal(c, rbac.PrincipalFrom(c), execRequest{HostIDs: []int64{h.ID}, Command: in.Command, TimeoutSec: in.TimeoutSec}, "")
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if len(res.Pending) > 0 {
		approval.RespondPending(c, res.Pending[0])
		return
	}
	t := res.Tasks[0]
	if wantsStream(c) {
		m.sse(c, t)
		return
	}
	ctx := c.Request.Context()
	_ = m.hub.Wait(ctx, t.ID)
	if ft, err := m.st.getTask(ctx, t.ID); err == nil {
		t = ft
	}
	httpx.OK(c, t)
}

type batchInput struct {
	HostIDs    []int64 `json:"host_ids" binding:"required,min=1,max=200"`
	Command    string  `json:"command" binding:"required,max=65536"`
	TimeoutSec int     `json:"timeout_sec" binding:"min=0,max=3600"`
}

// POST /hosts/batch-exec — asynchronous; poll GET /hosts/tasks?batch_id= or stream each task.
func (m *Module) batchExec(c *gin.Context) {
	var in batchInput
	if !httpx.Bind(c, &in) {
		return
	}
	batchID := "b" + strconv.FormatInt(time.Now().UnixNano(), 36)
	audit.SetResource(c, "host_batch", batchID)
	audit.SetDetail(c, gin.H{"command": trunc(in.Command, 500), "host_ids": in.HostIDs})
	res, err := m.execForPrincipal(c, rbac.PrincipalFrom(c), execRequest(in), batchID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	status := http.StatusOK
	if len(res.Tasks) == 0 && len(res.Pending) > 0 {
		status = http.StatusAccepted
	}
	c.JSON(status, httpx.Envelope{Code: 0, Message: "ok", Data: res, RequestID: httpx.RequestIDFrom(c)})
}

func (m *Module) getTask(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	t, err := m.st.getTask(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, t)
}

func (m *Module) streamTask(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	t, err := m.st.getTask(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	m.sse(c, t)
}

func (m *Module) listHostTasks(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	p := httpx.ParsePage(c)
	items, total, err := m.st.listTasks(c, id, "", p)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, httpx.NewPageResult(items, total, p))
}

func (m *Module) listBatchTasks(c *gin.Context) {
	b := c.Query("batch_id")
	if b == "" {
		httpx.Fail(c, httpx.Validation("batch_id is required"))
		return
	}
	p := httpx.ParsePage(c)
	items, total, err := m.st.listTasks(c, 0, b, p)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, httpx.NewPageResult(items, total, p))
}

// sse streams events: "task" (row at start), "output" {stream,data}, "done" (final row).
func (m *Module) sse(c *gin.Context, t *Task) {
	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(ev string, v any) bool {
		b, _ := json.Marshal(v)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, b); err != nil {
			return false
		}
		w.Flush()
		return true
	}
	send("task", t)
	snap, ch, cancel, running := m.hub.Subscribe(t.ID)
	defer cancel()
	if running {
		if snap != "" {
			send("output", Event{Stream: "stdout", Data: snap})
		}
		ctx := c.Request.Context()
		keep := time.NewTicker(15 * time.Second)
		defer keep.Stop()
	loop:
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					break loop
				}
				if !send("output", ev) {
					return
				}
			case <-keep.C:
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					return
				}
				w.Flush()
			case <-ctx.Done():
				return // client left; task keeps running and is persisted
			}
		}
	}
	if ft, err := m.st.getTask(c.Request.Context(), t.ID); err == nil {
		t = ft
	}
	send("done", t)
}

// ---- file push -----------------------------------------------------------------

type fileInput struct {
	Path          string `json:"path" binding:"required,max=1024"`
	ContentBase64 string `json:"content_base64" binding:"max=1500000"`
	Mode          string `json:"mode"` // octal string, e.g. "0644"
}

func (m *Module) pushFile(c *gin.Context) {
	h, ok := m.hostParam(c)
	if !ok {
		return
	}
	var in fileInput
	if !httpx.Bind(c, &in) {
		return
	}
	if !path.IsAbs(in.Path) || path.Clean(in.Path) != in.Path {
		httpx.Fail(c, httpx.Validation("path must be absolute and clean"))
		return
	}
	raw, err := base64.StdEncoding.DecodeString(in.ContentBase64)
	if err != nil || len(raw) > proto.MaxFile {
		httpx.Fail(c, httpx.Validation(fmt.Sprintf("content_base64 must be valid base64 of at most %d bytes", proto.MaxFile)))
		return
	}
	mode := uint64(0o644)
	if in.Mode != "" {
		if mode, err = strconv.ParseUint(in.Mode, 8, 32); err != nil || mode > 0o7777 {
			httpx.Fail(c, httpx.Validation("mode must be an octal string like 0644"))
			return
		}
	}
	pr := rbac.PrincipalFrom(c)
	if !pr.CanEnv(h.Env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope: "+h.Env))
		return
	}
	audit.SetResource(c, "host", strconv.FormatInt(h.ID, 10))
	audit.SetDetail(c, gin.H{"path": in.Path, "bytes": len(raw)})
	req := fileRequest{HostID: h.ID, Path: in.Path, ContentBase64: in.ContentBase64, Mode: uint32(mode)}
	tk, pending, err := m.d.Approval.Gate(c, pr, approval.CreateInput{Type: TypeFilePush, Env: h.Env, Payload: req,
		Title: fmt.Sprintf("向主机 %s 推送文件 %s (%d 字节)", h.Name, in.Path, len(raw))})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if pending {
		approval.RespondPending(c, tk)
		return
	}
	t, err := m.startFilePush(c, req, who(pr), nil)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, m.waitAll(c.Request.Context(), []*Task{t})[0])
}
