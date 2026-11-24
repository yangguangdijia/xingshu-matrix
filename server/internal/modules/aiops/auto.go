package aiops

import (
	"context"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// autoRemediate runs after a case is concluded. When a runbook matches the
// case and the policy allows automatic remediation for it, the runbook's plan
// is stored as an accepted plan and executed in AI mode.
//
// The execution acts as the principal that started the case. Cases without a
// principal (e.g. created by a webhook) are never auto-executed: there is no
// real reviewer to act as, and the contract forbids a superuser stand-in.
func (m *Module) autoRemediate(ctx context.Context, c *Case, conclusion Conclusion, p *rbac.Principal) {
	if p == nil || c == nil {
		return
	}
	pol, err := m.loadPolicy(ctx)
	if err != nil || !pol.AutoExecute.Enabled {
		return
	}
	for _, rb := range m.matchRunbooks(ctx, c, conclusion.Findings) {
		if !m.autoExecutable(rb, c, pol) {
			continue
		}
		plan, err := caseBuildPlan(c.ID, 99, rb.Plan)
		if err != nil {
			_ = m.appendTimeline(ctx, c.ID, "auto_skipped", "system", map[string]any{"runbook_id": rb.ID, "reason": err.Error()})
			continue
		}
		plan.Title = "预案：" + rb.Name
		plan.Status = execPlanAccepted
		plan.Risk = planRisk(plan.Steps)
		plan.Rollback = caseRollbackSummary(plan.Steps)
		if err := m.insertPlan(ctx, plan); err != nil {
			return
		}
		if err := m.updateCaseStatus(ctx, c.ID, CaseAccepted, ""); err != nil {
			return
		}
		c.Status = CaseAccepted
		if err := m.execStartAuto(ctx, c, plan, p); err != nil {
			_ = m.appendTimeline(ctx, c.ID, "auto_skipped", "system", map[string]any{"runbook_id": rb.ID, "reason": err.Error()})
			_ = m.updateCaseStatus(ctx, c.ID, CaseConcluded, "")
			return
		}
		_ = m.bumpRunbookUse(ctx, rb.ID, false)
		return
	}
}
