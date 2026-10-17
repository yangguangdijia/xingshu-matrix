package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/ai"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

type aiClusterRef struct {
	Cluster string `json:"cluster"` // id or name
}

// aiCluster resolves the cluster argument and enforces the principal's env scope.
func (m *Module) aiCluster(ctx context.Context, ref string) (*Cluster, *kubeClients, error) {
	if ref == "" {
		return nil, nil, errors.New("cluster is required (id or name)")
	}
	var (
		cl  *Cluster
		err error
	)
	if id, e := strconv.ParseInt(ref, 10, 64); e == nil {
		cl, err = m.loadCluster(ctx, id)
	} else {
		cl, err = m.loadClusterByName(ctx, ref)
	}
	if err != nil {
		return nil, nil, err
	}
	if p := rbac.FromContext(ctx); p != nil && !p.CanEnv(cl.Env) {
		return nil, nil, fmt.Errorf("no access to environment %s", cl.Env)
	}
	kc, err := m.kube(ctx, cl)
	return cl, kc, err
}

func (m *Module) registerAITools(d *app.Deps) {
	clusterProp := `"cluster":{"type":"string","description":"集群 ID 或名称"}`
	d.AITools.MustRegister(ai.Tool{
		Name: "k8s_list_pods", Module: "cluster", Permission: PermWorkloadRead,
		Description: "列出 Kubernetes 集群中的 Pod（状态、就绪数、重启次数、所在节点）",
		Parameters: json.RawMessage(`{"type":"object","properties":{` + clusterProp + `,
			"namespace":{"type":"string","description":"命名空间，留空表示全部"},
			"label_selector":{"type":"string","description":"标签选择器，如 app=web"},
			"limit":{"type":"integer","description":"最多返回条数，默认 100"}},"required":["cluster"]}`),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				aiClusterRef
				Namespace     string `json:"namespace"`
				LabelSelector string `json:"label_selector"`
				Limit         int    `json:"limit"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			_, kc, err := m.aiCluster(ctx, a.Cluster)
			if err != nil {
				return nil, err
			}
			rows, _, err := listWorkloadRows(ctx, kc.cs, "pods", a.Namespace, metav1.ListOptions{LabelSelector: a.LabelSelector})
			if err != nil {
				return nil, err
			}
			pods := rows.([]PodRow)
			if a.Limit <= 0 || a.Limit > 500 {
				a.Limit = 100
			}
			total := len(pods)
			if total > a.Limit {
				pods = pods[:a.Limit]
			}
			return map[string]any{"total": total, "pods": pods}, nil
		},
	})
	d.AITools.MustRegister(ai.Tool{
		Name: "k8s_get_events", Module: "cluster", Permission: PermWorkloadRead,
		Description: "查询 Kubernetes 事件（按最近时间倒序），可按对象类型/名称、Warning 过滤，用于排障",
		Parameters: json.RawMessage(`{"type":"object","properties":{` + clusterProp + `,
			"namespace":{"type":"string"},"kind":{"type":"string","description":"对象类型，如 Pod/Deployment"},
			"name":{"type":"string","description":"对象名称"},"type":{"type":"string","enum":["Normal","Warning"]},
			"limit":{"type":"integer","description":"默认 50"}},"required":["cluster"]}`),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				aiClusterRef
				Namespace, Kind, Name, Type string
				Limit                       int
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			_, kc, err := m.aiCluster(ctx, a.Cluster)
			if err != nil {
				return nil, err
			}
			if a.Limit <= 0 {
				a.Limit = 50
			}
			return listEventRows(ctx, kc.cs, eventQuery{Namespace: a.Namespace, Kind: a.Kind, Name: a.Name, Type: a.Type, Limit: a.Limit})
		},
	})
	d.AITools.MustRegister(ai.Tool{
		Name: "k8s_scale_deployment", Module: "cluster", Permission: PermWorkloadWrite, RequiresConfirmation: true,
		Description: "调整 Deployment 副本数（需用户确认；需审批的环境会生成审批单）",
		Parameters: json.RawMessage(`{"type":"object","properties":{` + clusterProp + `,
			"namespace":{"type":"string"},"name":{"type":"string","description":"Deployment 名称"},
			"replicas":{"type":"integer","minimum":0}},"required":["cluster","namespace","name","replicas"]}`),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				aiClusterRef
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
				Replicas  *int32 `json:"replicas"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			cl, _, err := m.aiCluster(ctx, a.Cluster)
			if err != nil {
				return nil, err
			}
			params, _ := json.Marshal(scaleParams{Replicas: a.Replicas})
			pl := changePayload{ClusterID: cl.ID, Cluster: cl.Name, Action: "scale", Namespace: a.Namespace,
				Kind: "deployments", Name: a.Name, Params: params}
			if err := validateChange(&pl); err != nil {
				return nil, err
			}
			p := rbac.FromContext(ctx)
			if p == nil {
				return nil, errors.New("no principal in context")
			}
			res, t, err := m.runChange(ctx, p, cl, pl, "AI 助手发起")
			if err != nil {
				return nil, err
			}
			if t != nil {
				return map[string]any{"pending_approval": true, "ticket_id": t.ID, "message": "该环境需要审批，已创建审批单"}, nil
			}
			return res, nil
		},
	})
}
