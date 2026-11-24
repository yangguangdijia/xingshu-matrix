package aiops

import (
	"context"
	"embed"
	"io/fs"
	"sort"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

//go:embed migrations/*.sql
var migFS embed.FS

// Permissions.
const (
	PermRead    = "aiops:read"    // view cases, plans, runbooks
	PermHandle  = "aiops:handle"  // start investigations, review plans, run manual checklists
	PermExecute = "aiops:execute" // let the platform execute accepted plans (plus each action's own permission)
	PermManage  = "aiops:manage"  // runbooks, auto-remediation policy
)

// Module is the AI operations workbench.
type Module struct {
	d      *app.Deps
	cancel context.CancelFunc
}

func New() *Module                  { return &Module{} }
func (m *Module) Name() string      { return "aiops" }
func (m *Module) Migrations() fs.FS { sub, _ := fs.Sub(migFS, "migrations"); return sub }
func (m *Module) Permissions() []rbac.Permission {
	return []rbac.Permission{
		rbac.P("aiops", "aiops", "read", "查看 AI 排查单、方案与预案"),
		rbac.P("aiops", "aiops", "handle", "发起 AI 排查、审阅方案、执行人工检查清单"),
		rbac.P("aiops", "aiops", "execute", "由平台自动执行已采纳的方案"),
		rbac.P("aiops", "aiops", "manage", "管理预案与自动处置策略"),
	}
}

// Register mounts routes. Each registerX lives in its own file.
func (m *Module) Register(r *gin.RouterGroup, d *app.Deps) {
	m.d = d
	g := r.Group("/aiops")
	m.registerCases(g)      // cases.go (aiops-core)
	m.registerRunbooks(g)   // runbooks.go (aiops-core)
	m.registerActionsAPI(g) // cases.go: GET /aiops/actions
	m.registerAITools()     // investigate.go: chat tools aiops_* for the assistant
}

// Start runs background workers (investigation queue, executor, auto policy).
func (m *Module) Start(ctx context.Context, d *app.Deps) error {
	ctx, m.cancel = context.WithCancel(ctx)
	go m.runWorkers(ctx) // cases.go
	return nil
}

func (m *Module) Stop(context.Context) error {
	if m.cancel != nil {
		m.cancel()
	}
	return nil
}

func sortSpecs(s []ActionSpec) { sort.Slice(s, func(i, j int) bool { return s[i].Name < s[j].Name }) }
