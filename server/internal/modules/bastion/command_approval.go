package bastion

// 命令审批：命令过滤规则的动作为 approve 时，命令在堡垒机侧暂存，
// 以会话用户的身份提交审批单（类型 bastion.command），审批人在「审批中心」通过后命令才发往主机。
// 审批被拒绝、申请人撤回（Ctrl+C）或超时（CommandApprovalTimeout）都会取消该命令；全过程写入命令审计。

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// CommandApprovalType 是命令审批的审批单类型。
const CommandApprovalType = "bastion.command"

// CommandApprovalTimeout 是等待审批的最长时间。
var CommandApprovalTimeout = 10 * time.Minute

type commandApprovalPayload struct {
	SessionID int64  `json:"session_id"`
	AssetID   int64  `json:"asset_id"`
	Asset     string `json:"asset"`
	Command   string `json:"command"`
	RuleID    int64  `json:"rule_id"`
	Rule      string `json:"rule"`
}

// approvalNotice 通过 WebSocket（type=approval）告诉 Web 终端当前命令审批的状态，
// 终端据此显示「等待审批」横幅；文字提示同时以终端输出形式发送，旧前端也能看到。
type approvalNotice struct {
	State    string `json:"state"` // submitting | pending | approved | rejected | cancelled | timeout | error
	TicketID int64  `json:"ticket_id,omitempty"`
	Command  string `json:"command,omitempty"`
	Rule     string `json:"rule,omitempty"`
	Note     string `json:"note,omitempty"`
	Deadline string `json:"deadline,omitempty"` // RFC3339，等待截止时间
}

func (m *Module) awaitCommandApproval(ctx context.Context, p *rbac.Principal, asset *Asset, sessionID int64, req *CommandEvent,
	mu *sync.Mutex, current *int64, resolve func(approved bool, note string), say func(string), notify func(approvalNotice)) {
	if notify == nil {
		notify = func(approvalNotice) {}
	}
	done := func(approved bool, state string, id int64, note string) {
		notify(approvalNotice{State: state, TicketID: id, Command: req.Command, Rule: req.Action, Note: note})
		resolve(approved, note)
	}
	notify(approvalNotice{State: "submitting", Command: req.Command, Rule: req.Action})
	if m.d.Approval == nil {
		done(false, "error", 0, "审批服务不可用，命令已取消")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	t, err := m.d.Approval.CreateOwned(cctx, p, approval.CreateInput{
		Type:    CommandApprovalType,
		Title:   fmt.Sprintf("堡垒机命令审批：%s", asset.Name),
		Env:     asset.Env,
		Reason:  req.Command,
		Payload: commandApprovalPayload{SessionID: sessionID, AssetID: asset.ID, Asset: asset.Name, Command: req.Command, RuleID: req.RuleID, Rule: req.Action},
	})
	cancel()
	if err != nil {
		done(false, "error", 0, "提交审批失败："+err.Error()+"，命令已取消")
		return
	}
	mu.Lock()
	*current = t.ID
	mu.Unlock()
	say(fmt.Sprintf("\r\n\x1b[33m[bastion] 已提交审批单 #%d，等待审批人处理（最长 %d 分钟，Ctrl+C 放弃）…\x1b[0m", t.ID, int(CommandApprovalTimeout.Minutes())))
	notify(approvalNotice{State: "pending", TicketID: t.ID, Command: req.Command, Rule: req.Action,
		Deadline: time.Now().Add(CommandApprovalTimeout).UTC().Format(time.RFC3339)})
	deadline := time.NewTimer(CommandApprovalTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	stillMine := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return *current == t.ID
	}
	finish := func() {
		mu.Lock()
		if *current == t.ID {
			*current = 0
		}
		mu.Unlock()
	}
	for {
		select {
		case <-ctx.Done():
			_, _ = m.d.Approval.Cancel(context.Background(), p, t.ID)
			finish()
			return
		case <-deadline.C:
			_, _ = m.d.Approval.Cancel(context.Background(), p, t.ID)
			finish()
			done(false, "timeout", t.ID, fmt.Sprintf("审批单 #%d 超时未处理，已撤回，命令已取消", t.ID))
			return
		case <-tick.C:
			if !stillMine() { // 用户按了 Ctrl+C，审批单已撤回
				return
			}
			gctx, gcancel := context.WithTimeout(ctx, 5*time.Second)
			cur, err := m.d.Approval.Get(gctx, p, t.ID)
			gcancel()
			if err != nil {
				continue
			}
			switch cur.Status {
			case approval.StatusApproved, approval.StatusExecuted:
				finish()
				done(true, "approved", t.ID, fmt.Sprintf("审批单 #%d 已由 %s 通过，正在执行", t.ID, cur.ApproverName))
				return
			case approval.StatusRejected, approval.StatusCancelled, approval.StatusFailed:
				finish()
				note := fmt.Sprintf("审批单 #%d 未通过（%s），命令已取消", t.ID, statusZH(cur.Status))
				if cur.Comment != "" {
					note += "：" + cur.Comment
				}
				done(false, "rejected", t.ID, note)
				return
			}
		}
	}
}

func statusZH(s string) string {
	switch s {
	case approval.StatusRejected:
		return "已拒绝"
	case approval.StatusCancelled:
		return "已撤回"
	case approval.StatusFailed:
		return "失败"
	}
	return s
}
