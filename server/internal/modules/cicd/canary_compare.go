package cicd

// 金丝雀发布详情页「主版本 vs 金丝雀」对比数据。
//
// GET /cicd/canaries/:id/compare（cicd:read）
//
// 返回：放量步骤进度、整个生命周期内的流量权重阶梯曲线（由已记录的「步骤」事件还原）、
// 两侧工作负载 / 路由 / 镜像，以及前端读取两侧监控曲线所需的定位信息（集群、命名空间、
// 工作负载名、路由名）。
//
// 监控数据本身不在这里查询：Prometheus 数据源的按集群解析（selectSource /
// resolveSourceScope）是 observability 模块的未导出逻辑，且需要调用者的身份做集群 /
// 命名空间授权；cicd 模块拿不到它（app.Deps 没有对应能力，跨模块直接引用会绕过授权）。
// 因此由前端用当前用户身份调用 observability 已有接口（数据源解析 + 范围查询，或
// 工作负载迷你趋势），金丝雀本身只提供它已记录的事实。未关联已注册集群的执行目标
// 不返回 metrics_target，前端只显示权重与步骤。

import (
	"encoding/json"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// canaryCompareMaxPoints 限制权重曲线点数（步骤最多 20 步，加上回滚 / 全量与首尾点）。
const canaryCompareMaxPoints = 64

// CanaryCompareStep 是一个放量步骤的进度。
type CanaryCompareStep struct {
	Index     int        `json:"index"` // 从 0 开始
	Weight    int        `json:"weight"`
	PauseSec  int        `json:"pause_sec"`
	Manual    bool       `json:"manual"`
	State     string     `json:"state"` // done | current | pending | stopped
	EnteredAt *time.Time `json:"entered_at,omitempty"`
}

// CanaryCompareSide 是对比的一侧（主版本 / 金丝雀）。
type CanaryCompareSide struct {
	Workload string `json:"workload"`
	Service  string `json:"service"`
	Ingress  string `json:"ingress"`
	Image    string `json:"image"`
	Weight   int    `json:"weight"`
}

// CanaryMetricsTarget 告诉前端去哪个集群、哪个命名空间读取两侧监控。
type CanaryMetricsTarget struct {
	ClusterID     int64  `json:"cluster_id"`
	Namespace     string `json:"namespace"`
	WorkloadKind  string `json:"workload_kind"`
	StableName    string `json:"stable_workload"`
	CanaryName    string `json:"canary_workload"`
	StableIngress string `json:"stable_ingress"`
	CanaryIngress string `json:"canary_ingress"`
}

// CanaryCompare 是对比接口的返回体。时间均为 unix 秒。
type CanaryCompare struct {
	CanaryID      int64                `json:"canary_id"`
	Status        string               `json:"status"`
	Final         bool                 `json:"final"`
	Weight        int                  `json:"weight"`
	CurrentStep   int                  `json:"current_step"`
	TotalSteps    int                  `json:"total_steps"`
	DoneSteps     int                  `json:"done_steps"`
	Start         int64                `json:"start"`
	End           int64                `json:"end"`
	WeightSeries  [][2]int64           `json:"weight_series"` // [unix 秒, 金丝雀权重 %]，阶梯曲线
	Steps         []CanaryCompareStep  `json:"steps"`
	Stable        CanaryCompareSide    `json:"stable"`
	Canary        CanaryCompareSide    `json:"canary"`
	MetricsTarget *CanaryMetricsTarget `json:"metrics_target,omitempty"`
	Note          string               `json:"note,omitempty"`
}

func canaryFinalStatus(s string) bool {
	switch s {
	case canarySucceeded, canaryRolledBack, canaryFailed, canaryRejected, canaryCancelled:
		return true
	}
	return false
}

// canaryStepEventData 是「步骤」事件的附加数据：
// 进入第 n 步写 {"step":n,"weight":w}；全量写 {"image":..}；回滚完成不带数据。
type canaryStepEventData struct {
	Step   *int    `json:"step"`
	Weight *int    `json:"weight"`
	Image  *string `json:"image"`
}

type canaryWeightPoint struct {
	at     time.Time
	weight int
	step   int // 进入的步骤（从 1 开始），0 表示非进入步骤事件
}

// canaryWeightPoints 从步骤事件还原权重变化点（按时间升序）。
func canaryWeightPoints(events []*CanaryEvent) []canaryWeightPoint {
	out := []canaryWeightPoint{}
	for _, e := range events {
		if e == nil || e.Kind != "step" {
			continue
		}
		var d canaryStepEventData
		if len(e.Data) > 0 && string(e.Data) != "null" {
			if err := json.Unmarshal(e.Data, &d); err != nil {
				continue
			}
		}
		p := canaryWeightPoint{at: e.TS}
		switch {
		case d.Weight != nil:
			p.weight = clampWeight(*d.Weight)
			if d.Step != nil {
				p.step = *d.Step
			}
		case d.Image != nil:
			p.weight = 100 // 全量：新镜像承接全部流量
		default:
			p.weight = 0 // 回滚完成：金丝雀流量归零
		}
		out = append(out, p)
	}
	// canaryEvents 已按 id 升序；时间戳相同或乱序时保持稳定的非递减顺序。
	for i := 1; i < len(out); i++ {
		if out[i].at.Before(out[i-1].at) {
			out[i].at = out[i-1].at
		}
	}
	return out
}

func clampWeight(w int) int {
	if w < 0 {
		return 0
	}
	if w > 100 {
		return 100
	}
	return w
}

// buildCanaryCompare 是纯函数（不访问数据库 / 集群），便于单测。
func buildCanaryCompare(c *Canary, events []*CanaryEvent, now time.Time) *CanaryCompare {
	final := canaryFinalStatus(c.Status)
	steps := c.Plan.Steps
	out := &CanaryCompare{
		CanaryID: c.ID, Status: c.Status, Final: final, Weight: clampWeight(c.Weight),
		CurrentStep: c.CurrentStep, TotalSteps: len(steps), Steps: make([]CanaryCompareStep, 0, len(steps)),
		Stable: CanaryCompareSide{Workload: c.Deployment, Service: c.Service, Ingress: c.Ingress, Image: c.StableImage,
			Weight: 100 - clampWeight(c.Weight)},
		Canary: CanaryCompareSide{Workload: c.CanaryDeployment, Service: c.CanaryService, Ingress: c.CanaryIngress, Image: c.Image,
			Weight: clampWeight(c.Weight)},
	}
	if c.Status == canarySucceeded {
		// 全量后主工作负载已切换到新镜像，金丝雀资源已清理。
		out.Stable.Weight, out.Canary.Weight = 0, 100
	}

	points := canaryWeightPoints(events)
	entered := map[int]time.Time{}
	for _, p := range points {
		if p.step > 0 {
			entered[p.step] = p.at // 同一步重复进入（如重试）取最后一次
		}
	}
	for i, s := range steps {
		st := CanaryCompareStep{Index: i, Weight: s.Weight, PauseSec: s.PauseSec, Manual: s.Manual, State: "pending"}
		switch {
		case c.Status == canarySucceeded || i < c.CurrentStep:
			st.State = "done"
		case i == c.CurrentStep && final:
			st.State = "stopped"
		case i == c.CurrentStep:
			st.State = "current"
		}
		if st.State == "done" {
			out.DoneSteps++
		}
		if t, ok := entered[i+1]; ok {
			tt := t
			st.EnteredAt = &tt
		}
		out.Steps = append(out.Steps, st)
	}

	start := c.CreatedAt
	if len(points) > 0 && (start.IsZero() || points[0].at.Before(start)) {
		start = points[0].at
	}
	end := now
	if final && c.FinishedAt != nil && !c.FinishedAt.IsZero() {
		end = *c.FinishedAt
	}
	if end.Before(start) {
		end = start
	}
	out.Start, out.End = start.Unix(), end.Unix()

	series := [][2]int64{{start.Unix(), 0}}
	for _, p := range points {
		at := p.at
		if at.Before(start) {
			at = start
		}
		if at.After(end) {
			at = end
		}
		series = append(series, [2]int64{at.Unix(), int64(p.weight)})
	}
	series = append(series, [2]int64{end.Unix(), int64(out.Canary.Weight)})
	if c.Status == canarySucceeded {
		series[len(series)-1][1] = 100
	}
	out.WeightSeries = compactWeightSeries(series)
	return out
}

// compactWeightSeries 去掉与前一点完全相同的点，并在超长时保留首尾、均匀抽样中间点。
func compactWeightSeries(in [][2]int64) [][2]int64 {
	out := make([][2]int64, 0, len(in))
	for i, p := range in {
		if i > 0 && i < len(in)-1 {
			prev := out[len(out)-1]
			if prev == p {
				continue
			}
		}
		out = append(out, p)
	}
	if len(out) <= canaryCompareMaxPoints {
		return out
	}
	sampled := make([][2]int64, 0, canaryCompareMaxPoints)
	last := len(out) - 1
	for i := 0; i < canaryCompareMaxPoints; i++ {
		sampled = append(sampled, out[i*last/(canaryCompareMaxPoints-1)])
	}
	return sampled
}

// canaryMetricsTargetOf 在执行目标关联了已注册集群、两侧工作负载都已知时返回定位信息。
func canaryMetricsTargetOf(c *Canary, clusterID int64) *CanaryMetricsTarget {
	if clusterID <= 0 || c.Namespace == "" || c.Deployment == "" || c.CanaryDeployment == "" {
		return nil
	}
	return &CanaryMetricsTarget{ClusterID: clusterID, Namespace: c.Namespace, WorkloadKind: "deployments",
		StableName: c.Deployment, CanaryName: c.CanaryDeployment, StableIngress: c.Ingress, CanaryIngress: c.CanaryIngress}
}

func (m *Module) canaryCompare(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	cn, err := m.loadCanary(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	events, err := m.canaryEvents(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	out := buildCanaryCompare(cn, events, time.Now())
	var clusterID int64
	if cn.TargetID > 0 {
		// 执行目标读取失败只影响监控曲线，不影响权重与步骤展示。
		if row, terr := m.loadTarget(c, cn.TargetID); terr == nil && row.linked() {
			clusterID = row.ClusterID
		}
	}
	out.MetricsTarget = canaryMetricsTargetOf(cn, clusterID)
	if out.MetricsTarget == nil {
		out.Note = "执行目标未关联已注册集群，暂无两侧监控数据"
	}
	httpx.OK(c, out)
}
