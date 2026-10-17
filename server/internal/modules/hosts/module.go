// Package hosts implements host inventory and the matrix-agent channel:
// enrolment tokens, a JSON-over-WebSocket agent hub (see ./proto), facts &
// heartbeats, remote command execution (single / batch, SSE streaming) and
// small file push. Production executions are gated by approval ("hosts.exec").
package hosts

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/ai"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/hosts/proto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

//go:embed migrations/*.sql
var migFS embed.FS

const (
	TypeExec     = "hosts.exec"
	TypeFilePush = "hosts.file_push"

	permRead  = "host:read"
	permWrite = "host:write"
	permExec  = "host:exec"

	defaultTimeout = 60
	maxTimeout     = 3600
	maxBatchHosts  = 200
)

type Module struct {
	d   *app.Deps
	st  *store
	hub *Hub
	log *slog.Logger
}

func New() *Module { return &Module{} }

func (m *Module) Name() string { return "hosts" }

func (m *Module) Migrations() fs.FS { sub, _ := fs.Sub(migFS, "migrations"); return sub }

func (m *Module) Permissions() []rbac.Permission {
	return []rbac.Permission{
		rbac.P("hosts", "host", rbac.ActionRead, "查看主机、事实信息与执行记录"),
		rbac.P("hosts", "host", rbac.ActionWrite, "管理主机与 Agent 注册令牌"),
		rbac.P("hosts", "host", rbac.ActionExec, "在主机上执行命令、推送文件"),
	}
}

func (m *Module) Register(r *gin.RouterGroup, d *app.Deps) {
	m.d, m.log = d, d.Logger
	if m.log == nil {
		m.log = slog.Default()
	}
	m.st = &store{db: d.DB}
	m.hub = newHub(m.st, m.log)
	m.mount(r, d.Public)

	d.Approval.MustRegister(approval.TypeSpec{
		Type: TypeExec, Module: "hosts", Description: "在主机上执行命令", CreatePermission: permExec,
		Timeout: maxTimeout*time.Second + time.Minute, Handler: approval.HandlerFunc(m.onExecApproved),
	})
	d.Approval.MustRegister(approval.TypeSpec{
		Type: TypeFilePush, Module: "hosts", Description: "向主机推送文件", CreatePermission: permExec,
		Handler: approval.HandlerFunc(m.onFileApproved),
	})
	m.registerAITools(d.AITools)
}

func (m *Module) mount(r, public *gin.RouterGroup) {
	g := r.Group("/hosts")
	g.GET("", rbac.RequirePerm(permRead), m.listHosts)
	g.POST("", rbac.RequirePerm(permWrite), m.createHost)
	g.POST("/batch-exec", rbac.RequirePerm(permExec), m.batchExec)
	g.GET("/enroll-tokens", rbac.RequirePerm(permWrite), m.listTokens)
	g.POST("/enroll-tokens", rbac.RequirePerm(permWrite), m.createToken)
	g.DELETE("/enroll-tokens/:id", rbac.RequirePerm(permWrite), m.deleteToken)
	g.GET("/tasks", rbac.RequirePerm(permRead), m.listBatchTasks)
	g.GET("/tasks/:id", rbac.RequirePerm(permRead), m.getTask)
	g.GET("/tasks/:id/stream", rbac.RequirePerm(permRead), m.streamTask)
	g.GET("/:id", rbac.RequirePerm(permRead), m.getHost)
	g.PUT("/:id", rbac.RequirePerm(permWrite), m.updateHost)
	g.DELETE("/:id", rbac.RequirePerm(permWrite), m.deleteHost)
	g.GET("/:id/tasks", rbac.RequirePerm(permRead), m.listHostTasks)
	g.POST("/:id/exec", rbac.RequirePerm(permExec), m.exec)
	g.POST("/:id/files", rbac.RequirePerm(permExec), m.pushFile)
	if public != nil {
		public.GET("/agent/ws", m.hub.ServeWS)
	}
}

func (m *Module) Start(ctx context.Context, d *app.Deps) error {
	// Single-instance hub: no agent is connected to us yet.
	if err := m.st.resetOnline(ctx); err != nil {
		return fmt.Errorf("hosts: reset online: %w", err)
	}
	go func() { <-ctx.Done(); m.hub.Close() }()
	return nil
}

// ---- execution service -------------------------------------------------------

type execRequest struct {
	HostIDs    []int64 `json:"host_ids"`
	Command    string  `json:"command"`
	TimeoutSec int     `json:"timeout_sec"`
}

type fileRequest struct {
	HostID        int64  `json:"host_id"`
	Path          string `json:"path"`
	ContentBase64 string `json:"content_base64"`
	Mode          uint32 `json:"mode"`
}

func normTimeout(t int) int {
	if t <= 0 {
		return defaultTimeout
	}
	if t > maxTimeout {
		return maxTimeout
	}
	return t
}

// startExec creates one task per host and dispatches it. Offline hosts get a failed task.
func (m *Module) startExec(ctx context.Context, req execRequest, by string, ticketID *int64, batchID string) ([]*Task, error) {
	req.TimeoutSec = normTimeout(req.TimeoutSec)
	tasks := make([]*Task, 0, len(req.HostIDs))
	for _, hid := range req.HostIDs {
		t := &Task{HostID: hid, Kind: "exec", BatchID: batchID, Command: req.Command, TimeoutSec: req.TimeoutSec, TriggeredBy: by, TicketID: ticketID}
		if err := m.st.createTask(ctx, t); err != nil {
			return tasks, err
		}
		err := m.hub.Dispatch(hid, t.ID, proto.TypeExec, proto.Exec{TaskID: t.ID, Command: req.Command, TimeoutSec: req.TimeoutSec}, time.Duration(req.TimeoutSec)*time.Second)
		if err != nil {
			_ = m.st.finishTask(ctx, t.ID, StatusFailed, nil, "", err.Error())
			t.Status, t.Error = StatusFailed, err.Error()
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func (m *Module) startFilePush(ctx context.Context, req fileRequest, by string, ticketID *int64) (*Task, error) {
	t := &Task{HostID: req.HostID, Kind: "file_push", Command: req.Path, TimeoutSec: 60, TriggeredBy: by, TicketID: ticketID}
	if err := m.st.createTask(ctx, t); err != nil {
		return nil, err
	}
	err := m.hub.Dispatch(req.HostID, t.ID, proto.TypeFilePush, proto.FilePush{TaskID: t.ID, Path: req.Path, ContentBase64: req.ContentBase64, Mode: req.Mode}, time.Minute)
	if err != nil {
		_ = m.st.finishTask(ctx, t.ID, StatusFailed, nil, "", err.Error())
		t.Status, t.Error = StatusFailed, err.Error()
	}
	return t, nil
}

// waitAll waits for tasks and reloads their final rows.
func (m *Module) waitAll(ctx context.Context, tasks []*Task) []*Task {
	out := make([]*Task, 0, len(tasks))
	for _, t := range tasks {
		_ = m.hub.Wait(ctx, t.ID)
		if ft, err := m.st.getTask(context.Background(), t.ID); err == nil {
			t = ft
		}
		out = append(out, t)
	}
	return out
}

func (m *Module) onExecApproved(ctx context.Context, t *approval.Ticket) (any, error) {
	var req execRequest
	if err := t.DecodePayload(&req); err != nil {
		return nil, err
	}
	id := t.ID
	tasks, err := m.startExec(ctx, req, t.ApplicantName, &id, fmt.Sprintf("ticket-%d", t.ID))
	if err != nil {
		return nil, err
	}
	return summarize(m.waitAll(ctx, tasks), 4096), nil
}

func (m *Module) onFileApproved(ctx context.Context, t *approval.Ticket) (any, error) {
	var req fileRequest
	if err := t.DecodePayload(&req); err != nil {
		return nil, err
	}
	id := t.ID
	task, err := m.startFilePush(ctx, req, t.ApplicantName, &id)
	if err != nil {
		return nil, err
	}
	return summarize(m.waitAll(ctx, []*Task{task}), 1024), nil
}

type taskSummary struct {
	TaskID   int64  `json:"task_id"`
	HostID   int64  `json:"host_id"`
	Status   string `json:"status"`
	ExitCode *int   `json:"exit_code"`
	Output   string `json:"output"`
	Error    string `json:"error,omitempty"`
}

func summarize(ts []*Task, maxOut int) []taskSummary {
	out := make([]taskSummary, 0, len(ts))
	for _, t := range ts {
		o := t.Output
		if len(o) > maxOut {
			o = o[len(o)-maxOut:] // keep the tail, usually most relevant
		}
		out = append(out, taskSummary{t.ID, t.HostID, t.Status, t.ExitCode, o, t.Error})
	}
	return out
}

// gateGroups checks env scope and opens approval tickets per environment.
// It returns the hosts allowed to run now and the pending tickets.
func (m *Module) gateGroups(ctx context.Context, p *rbac.Principal, hosts []Host, req execRequest) (now []int64, pending []*approval.Ticket, err error) {
	byEnv := map[string][]int64{}
	var order []string
	for _, h := range hosts {
		if _, ok := byEnv[h.Env]; !ok {
			order = append(order, h.Env)
		}
		byEnv[h.Env] = append(byEnv[h.Env], h.ID)
	}
	for _, env := range order {
		ids := byEnv[env]
		r := execRequest{HostIDs: ids, Command: req.Command, TimeoutSec: normTimeout(req.TimeoutSec)}
		t, isPending, err := m.d.Approval.Gate(ctx, p, approval.CreateInput{
			Type: TypeExec, Env: env, Payload: r,
			Title: fmt.Sprintf("在 %d 台 %s 主机执行: %s", len(ids), env, trunc(req.Command, 80)),
		})
		if err != nil {
			return nil, nil, err
		}
		if isPending {
			pending = append(pending, t)
		} else {
			now = append(now, ids...)
		}
	}
	return now, pending, nil
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func who(p *rbac.Principal) string {
	if p == nil {
		return ""
	}
	return p.Username
}

// ---- AI tools ----------------------------------------------------------------

func (m *Module) registerAITools(reg *ai.ToolRegistry) {
	if reg == nil {
		return
	}
	reg.MustRegister(ai.Tool{
		Name: "hosts_list", Module: "hosts", Permission: permRead,
		Description: "列出受管主机（名称、IP、环境、标签、操作系统、在线状态、CPU/内存/磁盘使用率）。",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"env":{"type":"string","description":"按环境过滤，如 prod/test"},
			"keyword":{"type":"string","description":"按名称/IP/主机名模糊匹配"},
			"online_only":{"type":"boolean"}}}`),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Env, Keyword string
				OnlineOnly   bool `json:"online_only"`
			}
			_ = json.Unmarshal(args, &a)
			hs, total, err := m.st.listHosts(ctx, hostFilter{Env: a.Env, Keyword: a.Keyword, OnlineOnly: a.OnlineOnly}, httpx.Page{Page: 1, Size: 100})
			if err != nil {
				return nil, err
			}
			items := make([]gin.H, 0, len(hs))
			for _, h := range hs {
				items = append(items, gin.H{"id": h.ID, "name": h.Name, "ip": h.IP, "env": h.Env, "labels": h.Labels, "online": h.Online,
					"os": strings.TrimSpace(h.Facts.Platform + " " + h.Facts.PlatformVersion), "kernel": h.Facts.Kernel,
					"cpu_cores": h.Facts.CPUCores, "cpu_percent": h.Facts.CPUPercent, "mem_percent": h.Facts.MemPercent, "disks": h.Facts.Disks})
			}
			return gin.H{"total": total, "items": items}, nil
		},
	})
	reg.MustRegister(ai.Tool{
		Name: "hosts_exec", Module: "hosts", Permission: permExec, RequiresConfirmation: true,
		Description: "在一台或多台主机上通过 /bin/sh -c 执行 shell 命令并返回输出。生产环境需要审批。",
		Parameters: json.RawMessage(`{"type":"object","required":["host_ids","command"],"properties":{
			"host_ids":{"type":"array","items":{"type":"integer"},"description":"主机 ID 列表（来自 hosts_list）"},
			"command":{"type":"string"},
			"timeout_sec":{"type":"integer","description":"超时秒数，默认 60，最大 300"}}}`),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var req execRequest
			if err := json.Unmarshal(args, &req); err != nil || len(req.HostIDs) == 0 || strings.TrimSpace(req.Command) == "" {
				return nil, httpx.Validation("host_ids and command are required")
			}
			if req.TimeoutSec <= 0 || req.TimeoutSec > 300 {
				req.TimeoutSec = min(normTimeout(req.TimeoutSec), 300)
			}
			p := rbac.FromContext(ctx)
			if p == nil {
				return nil, httpx.Unauthorized("no principal")
			}
			res, err := m.execForPrincipal(ctx, p, req, "")
			if err != nil {
				return nil, err
			}
			tasks := m.waitAll(ctx, res.Tasks)
			return gin.H{"results": summarize(tasks, 8192), "pending_approvals": res.Pending}, nil
		},
	})
}

type execResult struct {
	BatchID string             `json:"batch_id,omitempty"`
	Tasks   []*Task            `json:"tasks"`
	Pending []*approval.Ticket `json:"pending_approvals"`
}

// execForPrincipal loads the hosts, applies env scope + approval gate and starts the runnable part.
func (m *Module) execForPrincipal(ctx context.Context, p *rbac.Principal, req execRequest, batchID string) (*execResult, error) {
	req.HostIDs = uniqIDs(req.HostIDs)
	if len(req.HostIDs) > maxBatchHosts {
		return nil, httpx.Validation(fmt.Sprintf("at most %d hosts per batch", maxBatchHosts))
	}
	hosts, _, err := m.st.listHosts(ctx, hostFilter{IDs: req.HostIDs}, httpx.Page{Page: 1, Size: maxBatchHosts})
	if err != nil {
		return nil, err
	}
	if len(hosts) != len(req.HostIDs) {
		return nil, httpx.NotFound("some hosts do not exist")
	}
	for _, h := range hosts {
		if !p.CanEnv(h.Env) {
			return nil, httpx.Forbidden("environment not in your scope: " + h.Env)
		}
	}
	now, pending, err := m.gateGroups(ctx, p, hosts, req)
	if err != nil {
		return nil, err
	}
	res := &execResult{BatchID: batchID, Tasks: []*Task{}, Pending: pending}
	if len(now) > 0 {
		req.HostIDs = now
		if res.Tasks, err = m.startExec(ctx, req, who(p), nil, batchID); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func uniqIDs(ids []int64) []int64 {
	seen := map[int64]bool{}
	out := ids[:0:0]
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
