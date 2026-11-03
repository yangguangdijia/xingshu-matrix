package cicd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"sigs.k8s.io/yaml"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// statsWindow is how many recent finished runs the list statistics cover.
const statsWindow = 20

// PipelineRunBrief is the newest run of a pipeline, shown in the list.
type PipelineRunBrief struct {
	ID          int64      `json:"id"`
	Status      string     `json:"status"`
	Trigger     string     `json:"trigger"`
	TriggeredBy string     `json:"triggered_by"`
	Branch      string     `json:"branch"`
	Commit      string     `json:"commit"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
}

// PipelineStats summarises the last statsWindow finished runs of a pipeline.
// SuccessRate and AvgDurationSec are nil until at least one run finished.
type PipelineStats struct {
	Window         int               `json:"window"`
	Finished       int               `json:"finished"`
	Succeeded      int               `json:"succeeded"`
	Failed         int               `json:"failed"`
	SuccessRate    *float64          `json:"success_rate"`
	AvgDurationSec *float64          `json:"avg_duration_sec"`
	Recent         []string          `json:"recent"` // newest first, up to 10 statuses (all runs)
	LastRun        *PipelineRunBrief `json:"last_run"`
}

// pipelineStats loads statistics for the given pipelines in two queries.
func (m *Module) pipelineStats(ctx context.Context, ids []int64) (map[int64]*PipelineStats, error) {
	out := make(map[int64]*PipelineStats, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	for _, id := range ids {
		out[id] = &PipelineStats{Window: statsWindow, Recent: []string{}}
	}
	rows, err := m.db.Query(ctx, `SELECT pipeline_id, status, rn, dur FROM (
		SELECT pipeline_id, status,
			row_number() OVER (PARTITION BY pipeline_id ORDER BY id DESC) AS rn,
			EXTRACT(EPOCH FROM (finished_at - started_at))::float8 AS dur
		FROM cicd_runs WHERE pipeline_id = ANY($1)) t
		WHERE rn <= $2 ORDER BY pipeline_id, rn`, ids, statsWindow)
	if err != nil {
		return nil, err
	}
	durs := map[int64][]float64{}
	for rows.Next() {
		var pid, rn int64
		var status string
		var dur *float64
		if err := rows.Scan(&pid, &status, &rn, &dur); err != nil {
			rows.Close()
			return nil, err
		}
		s := out[pid]
		if s == nil {
			continue
		}
		if rn <= 10 {
			s.Recent = append(s.Recent, status)
		}
		if terminal(status) && status != "cancelled" {
			s.Finished++
			if status == "succeeded" {
				s.Succeeded++
				if dur != nil && *dur >= 0 {
					durs[pid] = append(durs[pid], *dur)
				}
			} else {
				s.Failed++
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for pid, s := range out {
		if s.Finished > 0 {
			rate := float64(s.Succeeded) / float64(s.Finished)
			s.SuccessRate = &rate
		}
		if d := durs[pid]; len(d) > 0 {
			var sum float64
			for _, v := range d {
				sum += v
			}
			avg := sum / float64(len(d))
			s.AvgDurationSec = &avg
		}
	}
	last, err := m.db.Query(ctx, `SELECT DISTINCT ON (pipeline_id) pipeline_id,id,status,trigger,triggered_by,branch,commit_sha,created_at,started_at,finished_at
		FROM cicd_runs WHERE pipeline_id = ANY($1) ORDER BY pipeline_id, id DESC`, ids)
	if err != nil {
		return nil, err
	}
	defer last.Close()
	for last.Next() {
		var pid int64
		var b PipelineRunBrief
		if err := last.Scan(&pid, &b.ID, &b.Status, &b.Trigger, &b.TriggeredBy, &b.Branch, &b.Commit, &b.CreatedAt, &b.StartedAt, &b.FinishedAt); err != nil {
			return nil, err
		}
		if s := out[pid]; s != nil {
			brief := b
			s.LastRun = &brief
		}
	}
	return out, last.Err()
}

// retryRun starts a new run of the same pipeline with the same branch and commit.
func (m *Module) retryRun(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	old, err := m.loadRun(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if !terminal(old.Status) {
		httpx.Fail(c, httpx.Conflict("run is still active; cancel it before retrying"))
		return
	}
	r, err := m.startRun(c, rbac.PrincipalFrom(c), old.PipelineID, runIn{Branch: old.Branch, Commit: old.Commit}, fmt.Sprintf("retry:%d", old.ID))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "cicd_run", fmt.Sprint(r.ID))
	httpx.Created(c, r)
}

// pipelineDoc is the YAML form of a pipeline definition shown in the editor.
type pipelineDoc struct {
	Name           string  `json:"name"`
	AppID          int64   `json:"app_id"`
	RunnerTargetID int64   `json:"runner_target_id"`
	OnPush         bool    `json:"on_push"`
	Stages         []Stage `json:"stages"`
}

type yamlIn struct {
	YAML string `json:"yaml"`
}

// renderPipelineYAML converts a pipeline JSON body into YAML for the editor's code view.
func (m *Module) renderPipelineYAML(c *gin.Context) {
	var in pipelineDoc
	if !httpx.Bind(c, &in) {
		return
	}
	out, err := yaml.Marshal(in)
	if err != nil {
		httpx.Fail(c, httpx.Validation("cannot render pipeline"))
		return
	}
	httpx.OK(c, gin.H{"yaml": string(out)})
}

// parsePipelineYAML converts YAML back to the pipeline JSON and validates the
// stages against the application, so the editor can switch views safely.
func (m *Module) parsePipelineYAML(c *gin.Context) {
	var in yamlIn
	if !httpx.Bind(c, &in) {
		return
	}
	if len(in.YAML) > 256*1024 {
		httpx.Fail(c, httpx.Validation("pipeline YAML is too large"))
		return
	}
	var doc pipelineDoc
	if err := yaml.UnmarshalStrict([]byte(in.YAML), &doc); err != nil {
		httpx.Fail(c, httpx.Validation("invalid pipeline YAML: "+strings.TrimPrefix(err.Error(), "error unmarshaling JSON: ")))
		return
	}
	if doc.AppID != 0 {
		a, err := m.loadApp(c, doc.AppID)
		if err != nil {
			httpx.Fail(c, httpx.Validation("unknown app_id"))
			return
		}
		if err := validateStages(doc.Stages, a); err != nil {
			httpx.Fail(c, err)
			return
		}
	}
	httpx.OK(c, doc)
}
