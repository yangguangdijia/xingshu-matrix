package bastion

import (
	"context"
)

func (m *Module) auditCommands(ctx context.Context, ref sessionRef, events []CommandEvent) ([]int64, error) {
	var forwarded []int64
	check, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	for _, ev := range events {
		var rule any
		if ev.RuleID > 0 {
			rule = ev.RuleID
		}
		state := "blocked"
		switch ev.Action {
		case "allow", "confirmed":
			state = "pending"
		case "cancelled":
			state = "cancelled"
		}
		var id int64
		err := m.d.DB.QueryRow(check, `INSERT INTO bastion_commands
			(session_id,command,action,rule_id,delivery_state)
			SELECT id,$4,$5,$6,$7 FROM bastion_sessions
			WHERE id=$1 AND owner_id=$2 AND fence_token=$3 AND status='active'
			  AND lease_expires_at>clock_timestamp() AND kill_requested_at IS NULL RETURNING id`,
			ref.ID, ref.Owner, ref.Fence, ev.Command, ev.Action, rule, state).Scan(&id)
		if err != nil {
			return nil, err
		}
		if state == "pending" {
			forwarded = append(forwarded, id)
		}
	}
	return forwarded, nil
}

func (m *Module) commandDelivery(ctx context.Context, ref sessionRef, ids []int64, writeErr error) error {
	if len(ids) == 0 {
		return nil
	}
	state, detail := "forwarded", ""
	if writeErr != nil {
		state, detail = "write_failed", "SSH input write failed; delivery uncertain"
	}
	check, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	tag, err := m.d.DB.Exec(check, `UPDATE bastion_commands c SET delivery_state=$5,delivery_error=$6,
		forwarded_at=CASE WHEN $5='forwarded' THEN clock_timestamp() ELSE NULL END
		FROM bastion_sessions s WHERE c.id=ANY($4) AND c.session_id=s.id
		AND s.id=$1 AND s.owner_id=$2 AND s.fence_token=$3 AND s.status='active'
		AND s.lease_expires_at>clock_timestamp()`,
		ref.ID, ref.Owner, ref.Fence, ids, state, detail)
	if err == nil && tag.RowsAffected() != int64(len(ids)) {
		return errFenced
	}
	return err
}
