package cicd

// Git webhook delivery history and de-duplication. Providers retry or
// redeliver a hook with the same delivery id; a verified delivery whose id
// was already processed is answered 200 without starting runs. Deliveries are
// kept for webhookDeliveryRetention and listed on the pipeline trigger page.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

const webhookDeliveryRetention = 7 * 24 * time.Hour

// deliveryKey returns the provider and delivery id of a webhook request ("",
// "" when the provider sent no usable id). Gitea sends GitHub-compatible
// headers too, so its own header is checked first.
func deliveryKey(h http.Header) (provider, id string) {
	pick := func(v string) string {
		v = strings.TrimSpace(v)
		if v == "" || len(v) > 128 {
			return ""
		}
		for _, r := range v {
			if r < 0x21 || r > 0x7e {
				return ""
			}
		}
		return v
	}
	if h.Get("X-Gitlab-Event") != "" || h.Get("X-Gitlab-Token") != "" {
		// Idempotency-Key (GitLab 17.4+) stays the same across retries; older
		// versions only send X-Gitlab-Event-UUID.
		if v := pick(h.Get("Idempotency-Key")); v != "" {
			return "gitlab", v
		}
		if v := pick(h.Get("X-Gitlab-Event-UUID")); v != "" {
			return "gitlab", v
		}
		return "gitlab", ""
	}
	for _, c := range []struct{ header, provider string }{
		{"X-Gitee-Delivery", "gitee"}, {"X-Gitea-Delivery", "gitea"}, {"X-Gogs-Delivery", "gogs"}, {"X-GitHub-Delivery", "github"},
	} {
		if v := pick(h.Get(c.header)); v != "" {
			return c.provider, v
		}
	}
	return "", ""
}

// WebhookRunRef is a run started by a delivery.
type WebhookRunRef struct {
	PipelineID int64 `json:"pipeline_id"`
	RunID      int64 `json:"run_id"`
}

// WebhookDelivery is one recorded webhook delivery.
type WebhookDelivery struct {
	ID              int64           `json:"id"`
	AppID           int64           `json:"app_id"`
	Provider        string          `json:"provider"`
	DeliveryID      string          `json:"delivery_id"`
	Event           string          `json:"event"`
	Ref             string          `json:"ref"`
	Commit          string          `json:"commit"`
	Status          string          `json:"status"`
	Detail          string          `json:"detail"`
	Runs            []WebhookRunRef `json:"runs"`
	Duplicates      int             `json:"duplicates"`
	Attempts        int             `json:"attempts"`
	ReceivedAt      time.Time       `json:"received_at"`
	LastDuplicateAt *time.Time      `json:"last_duplicate_at"`
}

// claimDelivery records a verified delivery. duplicate=true means the same
// delivery id was already processed (or is being processed) and must not
// trigger again; the duplicate counter is bumped instead. A delivery whose
// earlier attempt ended in "error" is processed again.
func (m *Module) claimDelivery(ctx context.Context, appID int64, provider, deliveryID, event, ref, sha string) (int64, bool, error) {
	var nullableID any
	if deliveryID != "" {
		nullableID = deliveryID
	}
	var rowID int64
	duplicate := false
	err := pgx.BeginFunc(ctx, m.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO cicd_webhook_deliveries(app_id,provider,delivery_id,event,ref,commit_sha)
			VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT (app_id,provider,delivery_id) DO NOTHING RETURNING id`,
			appID, provider, nullableID, event, ref, sha).Scan(&rowID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT id,status FROM cicd_webhook_deliveries WHERE app_id=$1 AND provider=$2 AND delivery_id=$3 FOR UPDATE`,
			appID, provider, deliveryID).Scan(&rowID, &status); err != nil {
			return err
		}
		if status == "error" {
			_, err := tx.Exec(ctx, `UPDATE cicd_webhook_deliveries SET status='processing', attempts=attempts+1, detail='', event=$2, ref=$3, commit_sha=$4 WHERE id=$1`,
				rowID, event, ref, sha)
			return err
		}
		duplicate = true
		_, err = tx.Exec(ctx, `UPDATE cicd_webhook_deliveries SET duplicates=duplicates+1, last_duplicate_at=now() WHERE id=$1`, rowID)
		return err
	})
	return rowID, duplicate, err
}

func (m *Module) finishDelivery(rowID int64, status, detail string, runs []WebhookRunRef) {
	if rowID == 0 {
		return
	}
	if runs == nil {
		runs = []WebhookRunRef{}
	}
	raw, _ := json.Marshal(runs)
	if len(detail) > 500 {
		detail = detail[:500]
	}
	if _, err := m.db.Exec(context.Background(), `UPDATE cicd_webhook_deliveries SET status=$2, detail=$3, runs=$4 WHERE id=$1`,
		rowID, status, redactCICDText(detail), raw); err != nil {
		m.log.Error("update webhook delivery", "id", rowID, "err", err)
	}
}

func (m *Module) pruneDeliveries(ctx context.Context) {
	if _, err := m.db.Exec(ctx, `DELETE FROM cicd_webhook_deliveries WHERE received_at < $1`, time.Now().Add(-webhookDeliveryRetention)); err != nil && ctx.Err() == nil {
		m.log.Warn("prune webhook deliveries", "err", err)
	}
}

// listWebhookDeliveries serves GET /cicd/pipelines/:id/webhook-deliveries:
// deliveries of the pipeline's application (newest first), each with the
// run it started for this pipeline, if any.
func (m *Module) listWebhookDeliveries(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	pl, err := m.loadPipe(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	p := httpx.ParsePage(c)
	var total int64
	if err := m.db.QueryRow(c, `SELECT count(*) FROM cicd_webhook_deliveries WHERE app_id=$1`, pl.AppID).Scan(&total); err != nil {
		httpx.Fail(c, err)
		return
	}
	rows, err := m.db.Query(c, `SELECT id,app_id,provider,COALESCE(delivery_id,''),event,ref,commit_sha,status,detail,runs,duplicates,attempts,received_at,last_duplicate_at
		FROM cicd_webhook_deliveries WHERE app_id=$1 ORDER BY id DESC LIMIT $2 OFFSET $3`, pl.AppID, p.Limit(), p.Offset())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer rows.Close()
	items := []gin.H{}
	for rows.Next() {
		var d WebhookDelivery
		var runs []byte
		if err := rows.Scan(&d.ID, &d.AppID, &d.Provider, &d.DeliveryID, &d.Event, &d.Ref, &d.Commit, &d.Status, &d.Detail,
			&runs, &d.Duplicates, &d.Attempts, &d.ReceivedAt, &d.LastDuplicateAt); err != nil {
			httpx.Fail(c, err)
			return
		}
		_ = json.Unmarshal(runs, &d.Runs)
		if d.Runs == nil {
			d.Runs = []WebhookRunRef{}
		}
		var runID *int64
		for _, r := range d.Runs {
			if r.PipelineID == pl.ID {
				v := r.RunID
				runID = &v
			}
		}
		items = append(items, gin.H{"id": d.ID, "app_id": d.AppID, "provider": d.Provider, "delivery_id": d.DeliveryID,
			"event": d.Event, "ref": d.Ref, "commit": d.Commit, "status": d.Status, "detail": d.Detail, "runs": d.Runs,
			"run_id": runID, "duplicates": d.Duplicates, "attempts": d.Attempts, "received_at": d.ReceivedAt,
			"last_duplicate_at": d.LastDuplicateAt})
	}
	if err := rows.Err(); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"items": items, "total": total, "page": p.Page, "page_size": p.Size,
		"retention_days": int(webhookDeliveryRetention / (24 * time.Hour)), "on_push": pl.OnPush})
}
