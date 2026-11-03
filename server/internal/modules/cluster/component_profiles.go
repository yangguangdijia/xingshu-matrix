package cluster

import (
	"encoding/json"
	"strings"
)

type componentProfile struct {
	Key, RepoHost, RepoPath, Chart string
	Versions                       []string
	Installable                    bool
	Fields                         []ComponentField
	Dependencies                   []string
	ChartDependencies              []string
	CRDStrategy, HookStrategy      string
	KubeVersion, StorageDefault    string
	Notes                          string
}

func componentField(key, label, typ string, required bool, def string, options ...string) ComponentField {
	return ComponentField{Key: key, Label: label, Type: typ, Required: required, Default: def, Options: options}
}

func componentServiceField(key string) ComponentField {
	return componentField(key, "暴露方式", "enum", false, "ClusterIP", "ClusterIP", "NodePort", "LoadBalancer")
}

func componentStorageFields(prefix string) []ComponentField {
	return []ComponentField{
		componentField(prefix+".enabled", "启用持久化", "boolean", false, "false"),
		componentField(prefix+".storageClass", "StorageClass", "storageClass", false, ""),
	}
}

// Pinned official chart versions checked against upstream indexes on 2026-10-29.
// Engine support and target Kubernetes compatibility are separate checks.
var curatedComponentProfiles = map[string]componentProfile{
	"traefik": {
		Key: "traefik", RepoHost: "traefik.github.io", RepoPath: "/charts", Chart: "traefik", Versions: []string{"41.7.1"},
		Installable: true, KubeVersion: ">=1.25.0-0", CRDStrategy: "reviewed", HookStrategy: "disabled",
		Fields: []ComponentField{componentField("deployment.replicas", "副本数", "integer", false, "1"), componentServiceField("service.type")},
		Notes:  "默认 Kubernetes Ingress provider；不启用依赖自定义资源的默认 dashboard 路由。",
	},
	"istio-base": {
		Key: "istio-base", RepoHost: "istio-release.storage.googleapis.com", RepoPath: "/charts", Chart: "base", Versions: []string{"1.30.7"},
		Installable: true, KubeVersion: ">=1.32.0-0 <1.37.0-0", CRDStrategy: "reviewed", HookStrategy: "disabled",
		Notes: "Istio 1.30 维护系列；支持 Kubernetes 1.32–1.36，1.29 不在支持范围。先安装 base CRD，再安装 istiod 与 gateway。",
	},
	"istiod": {
		Key: "istiod", RepoHost: "istio-release.storage.googleapis.com", RepoPath: "/charts", Chart: "istiod", Versions: []string{"1.30.7"},
		Installable: true, Dependencies: []string{"istio-base"}, KubeVersion: ">=1.32.0-0 <1.37.0-0", HookStrategy: "disabled",
		Fields: []ComponentField{componentField("pilot.resources.requests.cpu", "CPU request", "quantity", false, "100m"), componentField("pilot.resources.requests.memory", "Memory request", "quantity", false, "128Mi"), componentField("pilot.replicaCount", "副本数", "integer", false, "1")},
	},
	"istio-gateway": {
		Key: "istio-gateway", RepoHost: "istio-release.storage.googleapis.com", RepoPath: "/charts", Chart: "gateway", Versions: []string{"1.30.7"},
		Installable: true, Dependencies: []string{"istio-base", "istiod"}, KubeVersion: ">=1.32.0-0 <1.37.0-0", HookStrategy: "disabled",
		Fields: []ComponentField{componentField("replicaCount", "副本数", "integer", false, "1"), componentServiceField("service.type")},
	},
	"cert-manager": {
		Key: "cert-manager", RepoHost: "charts.jetstack.io", Chart: "cert-manager", Versions: []string{"v1.21.3"},
		Installable: true, KubeVersion: ">=1.33.0-0 <1.37.0-0", CRDStrategy: "reviewed", HookStrategy: "disabled",
		Fields: []ComponentField{componentField("replicaCount", "副本数", "integer", false, "1"), componentField("webhook.replicaCount", "Webhook 副本数", "integer", false, "1")},
		Notes:  "维护系列与内置 CRD；Kubernetes 1.29 不在此 profile 支持范围。关闭 startupapicheck hook，Helm 等待 webhook 工作负载就绪。",
	},
	"argocd": {
		Key: "argocd", RepoHost: "argoproj.github.io", RepoPath: "/argo-helm", Chart: "argo-cd", Versions: []string{"10.11.6"},
		Installable: true, KubeVersion: ">=1.25.0-0", CRDStrategy: "reviewed", HookStrategy: "reviewed", ChartDependencies: []string{"redis-ha"},
		Fields: []ComponentField{componentField("server.replicas", "Server 副本数", "integer", false, "1"), componentServiceField("server.service.type"), componentField("controller.replicas", "Controller 副本数", "integer", false, "1")},
		Notes:  "单实例 Redis；仅允许 redis-secret-init 生命周期 hook。初始管理员凭据由集群 Secret 提供。",
	},
	"jenkins": {
		Key: "jenkins", RepoHost: "charts.jenkins.io", Chart: "jenkins", Versions: []string{"5.10.4"},
		Installable: true, KubeVersion: ">=1.25.0-0", HookStrategy: "disabled", StorageDefault: "ephemeral",
		Fields: append([]ComponentField{componentField("controller.numExecutors", "执行器数量", "integer", false, "2"), componentServiceField("controller.serviceType")}, componentStorageFields("persistence")...),
		Notes:  "单节点、默认临时存储；管理员密码由 chart 生成的 Kubernetes Secret 提供。",
	},
	"prometheus": {
		Key: "prometheus", RepoHost: "prometheus-community.github.io", RepoPath: "/helm-charts", Chart: "kube-prometheus-stack", Versions: []string{"92.15.0"},
		Installable: true, KubeVersion: ">=1.25.0-0", CRDStrategy: "reviewed", HookStrategy: "disabled",
		ChartDependencies: []string{"crds", "kube-state-metrics", "prometheus-node-exporter", "grafana", "prometheus-windows-exporter"},
		Fields:            []ComponentField{componentField("prometheus.prometheusSpec.replicas", "Prometheus 副本数", "integer", false, "1"), componentField("prometheus.prometheusSpec.resources.requests.cpu", "CPU request", "quantity", false, "200m"), componentField("prometheus.prometheusSpec.resources.requests.memory", "Memory request", "quantity", false, "512Mi"), componentServiceField("prometheus.service.type"), componentField("prometheus-node-exporter.service.port", "node-exporter 端口", "port", false, "9100"), componentField("prometheus-node-exporter.service.targetPort", "node-exporter 容器端口", "port", false, "9100")},
		Notes:             "单机 Prometheus Operator；关闭 Grafana、Alertmanager 和 admission webhook。先建立 CRD，再验证自定义资源。",
	},
	"alertmanager": {
		Key: "alertmanager", RepoHost: "prometheus-community.github.io", RepoPath: "/helm-charts", Chart: "alertmanager", Versions: []string{"2.1.2"},
		Installable: true, KubeVersion: ">=1.25.0-0", HookStrategy: "disabled", StorageDefault: "ephemeral",
		Fields: append([]ComponentField{componentField("replicaCount", "副本数", "integer", false, "1"), componentServiceField("service.type")}, componentStorageFields("persistence")...),
		Notes:  "独立 Alertmanager；不重复安装或接管既有 Prometheus Operator。默认临时存储。",
	},
	"loki": {
		Key: "loki", RepoHost: "grafana.github.io", RepoPath: "/helm-charts", Chart: "loki", Versions: []string{"7.5.0"},
		Installable: true, KubeVersion: ">=1.25.0-0", HookStrategy: "disabled", StorageDefault: "ephemeral", ChartDependencies: []string{"minio", "grafana-agent", "rollout-operator"},
		Fields: append([]ComponentField{componentField("singleBinary.replicas", "副本数", "integer", false, "1"), componentField("singleBinary.resources.requests.cpu", "CPU request", "quantity", false, "100m"), componentField("singleBinary.resources.requests.memory", "Memory request", "quantity", false, "256Mi")}, componentStorageFields("singleBinary.persistence")...),
		Notes:  "SingleBinary 单实例、文件系统存储；默认临时存储，仅限集群内部访问，需另行配置日志采集。",
	},
	"tempo": {
		Key: "tempo", RepoHost: "grafana.github.io", RepoPath: "/helm-charts", Chart: "tempo", Versions: []string{"1.24.4"},
		Installable: true, KubeVersion: ">=1.25.0-0", HookStrategy: "disabled", StorageDefault: "ephemeral",
		Fields: append([]ComponentField{componentField("replicas", "副本数", "integer", false, "1"), componentField("resources.requests.cpu", "CPU request", "quantity", false, "100m"), componentField("resources.requests.memory", "Memory request", "quantity", false, "256Mi"), componentServiceField("service.type")}, componentStorageFields("persistence")...),
	},
	"jaeger": {
		Key: "jaeger", RepoHost: "jaegertracing.github.io", RepoPath: "/helm-charts", Chart: "jaeger", Versions: []string{"4.16.1"},
		Installable: true, KubeVersion: ">=1.25.0-0", HookStrategy: "disabled", ChartDependencies: []string{"cassandra", "elasticsearch", "kafka"},
		Fields: []ComponentField{componentField("jaeger.replicas", "副本数", "integer", false, "1"), componentField("jaeger.resources.requests.cpu", "CPU request", "quantity", false, "100m"), componentField("jaeger.resources.requests.memory", "Memory request", "quantity", false, "256Mi")},
		Notes:  "Jaeger 2 单实例、内存存储，重启后不保留 traces。",
	},
}

func componentProfileFor(key string) (componentProfile, bool) {
	p, ok := curatedComponentProfiles[key]
	return p, ok
}

// Defaults are rebuilt for every request, so user edits cannot mutate profiles.
func componentDefaultValues(key string) map[string]any {
	defaults := map[string]map[string]any{
		"traefik":       {"deployment": map[string]any{"replicas": 1}, "service": map[string]any{"type": "ClusterIP"}, "providers": map[string]any{"kubernetesCRD": map[string]any{"enabled": false}}, "ingressRoute": map[string]any{"dashboard": map[string]any{"enabled": false}}},
		"istio-base":    {"defaultRevision": "default"},
		"istiod":        {"pilot": map[string]any{"autoscaleEnabled": false, "replicaCount": 1}},
		"istio-gateway": {"autoscaling": map[string]any{"enabled": false}, "replicaCount": 1, "service": map[string]any{"type": "ClusterIP"}},
		"cert-manager":  {"crds": map[string]any{"enabled": true, "keep": true}, "startupapicheck": map[string]any{"enabled": false}},
		"argocd":        {"crds": map[string]any{"install": true, "keep": true}, "redis-ha": map[string]any{"enabled": false}},
		"jenkins":       {"controller": map[string]any{"testEnabled": false}, "persistence": map[string]any{"enabled": false}},
		"prometheus":    {"grafana": map[string]any{"enabled": false}, "alertmanager": map[string]any{"enabled": false}, "kubeControllerManager": map[string]any{"enabled": false}, "kubeScheduler": map[string]any{"enabled": false}, "kubeEtcd": map[string]any{"enabled": false}, "kubeProxy": map[string]any{"enabled": false}, "coreDns": map[string]any{"service": map[string]any{"enabled": false}}, "prometheusOperator": map[string]any{"tls": map[string]any{"enabled": false}, "admissionWebhooks": map[string]any{"enabled": false, "patch": map[string]any{"enabled": false}}}},
		"alertmanager":  {"persistence": map[string]any{"enabled": false}, "testFramework": map[string]any{"enabled": false}},
		"loki":          {"deploymentMode": "SingleBinary", "loki": map[string]any{"auth_enabled": false, "commonConfig": map[string]any{"replication_factor": 1}, "storage": map[string]any{"type": "filesystem"}, "schemaConfig": map[string]any{"configs": []any{map[string]any{"from": "2024-01-01", "store": "tsdb", "object_store": "filesystem", "schema": "v13", "index": map[string]any{"prefix": "loki_index_", "period": "24h"}}}}}, "singleBinary": map[string]any{"replicas": 1, "persistence": map[string]any{"enabled": false}}, "read": map[string]any{"replicas": 0}, "write": map[string]any{"replicas": 0}, "backend": map[string]any{"replicas": 0}, "test": map[string]any{"enabled": false}, "lokiCanary": map[string]any{"enabled": false}, "chunksCache": map[string]any{"enabled": false}, "resultsCache": map[string]any{"enabled": false}, "gateway": map[string]any{"enabled": false}},
		"tempo":         {"persistence": map[string]any{"enabled": false}},
		"jaeger": {
			"cassandra": map[string]any{"enabled": false}, "elasticsearch": map[string]any{"enabled": false}, "kafka": map[string]any{"enabled": false},
			"userconfig": map[string]any{
				"service":    map[string]any{"extensions": []any{"jaeger_storage", "jaeger_query", "healthcheckv2"}, "pipelines": map[string]any{"traces": map[string]any{"receivers": []any{"otlp"}, "processors": []any{"batch"}, "exporters": []any{"jaeger_storage_exporter"}}}},
				"extensions": map[string]any{"healthcheckv2": map[string]any{"use_v2": true, "http": map[string]any{"endpoint": "0.0.0.0:13133"}}, "jaeger_query": map[string]any{"storage": map[string]any{"traces": "memory"}}, "jaeger_storage": map[string]any{"backends": map[string]any{"memory": map[string]any{"memory": map[string]any{"max_traces": 10000}}}}},
				"receivers":  map[string]any{"otlp": map[string]any{"protocols": map[string]any{"grpc": map[string]any{"endpoint": "0.0.0.0:4317"}, "http": map[string]any{"endpoint": "0.0.0.0:4318"}}}},
				"processors": map[string]any{"batch": map[string]any{}}, "exporters": map[string]any{"jaeger_storage_exporter": map[string]any{"trace_storage": "memory"}},
			},
		},
	}
	raw, _ := json.Marshal(defaults[key])
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

func (p componentProfile) fieldKeys() []string {
	keys := make([]string, 0, len(p.Fields))
	for _, f := range p.Fields {
		keys = append(keys, f.Key)
	}
	return keys
}

func (p componentProfile) exposureTypes() []string {
	for _, f := range p.Fields {
		if f.Type == "enum" && (strings.HasSuffix(f.Key, ".type") || f.Key == "controller.serviceType") {
			return append([]string(nil), f.Options...)
		}
	}
	return nil
}
