// Package bastion implements the 堡垒机 module: assets, system accounts,
// authorization rules, command filters, a WebSocket SSH terminal proxy with
// asciicast v2 recording and per-command audit, plus a guacd health probe.
package bastion

import (
	"context"
	"embed"
	"io/fs"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

//go:embed migrations/*.sql
var migFS embed.FS

const (
	PermRead    = "bastion:read"
	PermManage  = "bastion:manage"
	PermConnect = "bastion:connect"
	PermAudit   = "bastion:audit"
	PermKill    = "bastion:kill"
)

type Module struct {
	d         *app.Deps
	recordDir string
	guacdAddr string

	mu     sync.Mutex
	active map[int64]*liveSession
}

type liveSession struct {
	cancel context.CancelFunc
	killed bool
}

func New() *Module {
	dir := os.Getenv("MATRIX_RECORD_DIR")
	if dir == "" {
		dir = "./data/recordings"
	}
	ga := os.Getenv("MATRIX_GUACD_ADDR")
	if ga == "" {
		ga = "127.0.0.1:4822"
	}
	return &Module{recordDir: dir, guacdAddr: ga, active: map[int64]*liveSession{}}
}

func (m *Module) Name() string { return "bastion" }

func (m *Module) Migrations() fs.FS { sub, _ := fs.Sub(migFS, "migrations"); return sub }

func (m *Module) Permissions() []rbac.Permission {
	return []rbac.Permission{
		rbac.P("bastion", "bastion", "read", "查看堡垒机资产与账号"),
		rbac.P("bastion", "bastion", "manage", "管理资产、系统账号、授权规则、命令过滤"),
		rbac.P("bastion", "bastion", "connect", "通过堡垒机连接已授权资产"),
		rbac.P("bastion", "bastion", "audit", "查看会话、命令审计与录像"),
		rbac.P("bastion", "bastion", "kill", "强制断开在线会话"),
	}
}

func (m *Module) Register(r *gin.RouterGroup, d *app.Deps) {
	m.d = d
	g := r.Group("/bastion")

	g.GET("/assets", rbac.RequirePerm(PermRead), m.listAssets)
	g.POST("/assets", rbac.RequirePerm(PermManage), m.createAsset)
	g.GET("/assets/:id", rbac.RequirePerm(PermRead), m.getAsset)
	g.PUT("/assets/:id", rbac.RequirePerm(PermManage), m.updateAsset)
	g.DELETE("/assets/:id", rbac.RequirePerm(PermManage), m.deleteAsset)

	g.GET("/accounts", rbac.RequirePerm(PermRead), m.listAccounts)
	g.POST("/accounts", rbac.RequirePerm(PermManage), m.createAccount)
	g.PUT("/accounts/:id", rbac.RequirePerm(PermManage), m.updateAccount)
	g.DELETE("/accounts/:id", rbac.RequirePerm(PermManage), m.deleteAccount)

	g.GET("/rules", rbac.RequirePerm(PermRead), m.listRules)
	g.POST("/rules", rbac.RequirePerm(PermManage), m.createRule)
	g.PUT("/rules/:id", rbac.RequirePerm(PermManage), m.updateRule)
	g.DELETE("/rules/:id", rbac.RequirePerm(PermManage), m.deleteRule)

	g.GET("/filters", rbac.RequirePerm(PermRead), m.listFilters)
	g.POST("/filters", rbac.RequirePerm(PermManage), m.createFilter)
	g.PUT("/filters/:id", rbac.RequirePerm(PermManage), m.updateFilter)
	g.DELETE("/filters/:id", rbac.RequirePerm(PermManage), m.deleteFilter)

	g.GET("/my/assets", rbac.RequirePerm(PermConnect), m.myAssets)
	// Auth: the authenticated group accepts "Authorization: Bearer" or ?access_token=.
	g.GET("/sessions/ssh/ws", rbac.RequirePerm(PermConnect), m.sshWS)

	g.GET("/sessions", rbac.RequirePerm(PermAudit), m.listSessions)
	g.GET("/sessions/:id", rbac.RequirePerm(PermAudit), m.getSession)
	g.GET("/sessions/:id/commands", rbac.RequirePerm(PermAudit), m.listCommands)
	g.GET("/sessions/:id/recording", rbac.RequirePerm(PermAudit), m.downloadRecording)
	g.POST("/sessions/:id/kill", rbac.RequirePerm(PermKill), m.killSession)

	g.GET("/guacd/health", rbac.RequirePerm(PermRead), m.guacdHealth)
}

// Start marks sessions left "active" by a previous process as closed.
func (m *Module) Start(ctx context.Context, d *app.Deps) error {
	if err := os.MkdirAll(m.recordDir, 0o750); err != nil {
		d.Logger.Warn("bastion: cannot create record dir", "dir", m.recordDir, "err", err)
	}
	_, err := d.DB.Exec(ctx, `UPDATE bastion_sessions SET status='closed', error='server restarted', ended_at=now() WHERE status='active'`)
	return err
}

func (m *Module) guacdHealth(c *gin.Context) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", m.guacdAddr, 3*time.Second)
	res := gin.H{"addr": m.guacdAddr, "reachable": err == nil, "latency_ms": time.Since(start).Milliseconds()}
	if err != nil {
		res["error"] = err.Error()
	} else {
		conn.Close()
	}
	ok(c, res)
}

func (m *Module) register(id int64, cancel context.CancelFunc) {
	m.mu.Lock()
	m.active[id] = &liveSession{cancel: cancel}
	m.mu.Unlock()
}

func (m *Module) unregister(id int64) (killed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.active[id]; s != nil {
		killed = s.killed
	}
	delete(m.active, id)
	return
}

func (m *Module) kill(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.active[id]
	if s == nil {
		return false
	}
	s.killed = true
	s.cancel()
	return true
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func validProtocol(p string) bool {
	switch strings.ToLower(p) {
	case "ssh", "rdp", "vnc", "mysql", "postgres":
		return true
	}
	return false
}
