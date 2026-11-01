package cluster

import "sort"

// ComponentField is a reviewed, typed value exposed by the install wizard.
// The backend never accepts arbitrary Helm values.
type ComponentField struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Type        string   `json:"type"`
	Required    bool     `json:"required,omitempty"`
	Default     string   `json:"default,omitempty"`
	Options     []string `json:"options,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
}

type ComponentDefinition struct {
	Key                 string           `json:"key"`
	Name                string           `json:"name"`
	Category            string           `json:"category"`
	Supported           bool             `json:"supported"`
	Installable         bool             `json:"installable"`
	RepoURL             string           `json:"repo_url,omitempty"`
	Chart               string           `json:"chart,omitempty"`
	Versions            []string         `json:"versions,omitempty"`
	Dependencies        []string         `json:"dependencies,omitempty"`
	Prerequisites       []string         `json:"prerequisites,omitempty"`
	Notes               string           `json:"notes,omitempty"`
	Fields              []string         `json:"fields,omitempty"`
	ConfigurationFields []ComponentField `json:"configuration_fields,omitempty"`
	ExposureTypes       []string         `json:"exposure_types,omitempty"`
	CRDStrategy         string           `json:"crd_strategy,omitempty"`
	HookStrategy        string           `json:"hook_strategy,omitempty"`
	StorageDefault      string           `json:"storage_default,omitempty"`
	KubeVersion         string           `json:"kube_version,omitempty"`
}

var componentCatalog = []ComponentDefinition{
	{Key: "ingress-nginx", Name: "ingress-nginx", Category: "controller", Notes: "仅发现既有安装；上游项目已退役，禁止新安装。"},
	{Key: "traefik", Name: "Traefik", Category: "controller"},
	{Key: "istio-base", Name: "Istio base", Category: "service-mesh"},
	{Key: "istiod", Name: "Istiod", Category: "service-mesh"},
	{Key: "istio-gateway", Name: "Istio gateway", Category: "service-mesh"},
	{Key: "cert-manager", Name: "cert-manager", Category: "controller"},
	{Key: "argocd", Name: "Argo CD", Category: "controller"},
	{Key: "jenkins", Name: "Jenkins", Category: "application"},
	{Key: "prometheus", Name: "Prometheus", Category: "observability"},
	{Key: "alertmanager", Name: "Alertmanager", Category: "observability"},
	{Key: "loki", Name: "Loki", Category: "observability"},
	{Key: "tempo", Name: "Tempo", Category: "observability"},
	{Key: "jaeger", Name: "Jaeger", Category: "observability"},
	{Key: "elasticsearch", Name: "Elasticsearch", Category: "storage", Notes: "仅发现既有安装；当前没有经过审核的安装 profile。"},
}

func componentDefinition(key string) (ComponentDefinition, bool) {
	for _, item := range sortedComponentCatalog() {
		if item.Key == key {
			return item, true
		}
	}
	return ComponentDefinition{}, false
}

func sortedComponentCatalog() []ComponentDefinition {
	out := append([]ComponentDefinition(nil), componentCatalog...)
	for i := range out {
		d := &out[i]
		if p, ok := componentProfileFor(d.Key); ok {
			d.RepoURL = "https://" + p.RepoHost + p.RepoPath
			d.Chart = p.Chart
			d.Versions = append([]string(nil), p.Versions...)
			d.Dependencies = append([]string(nil), p.Dependencies...)
			d.Supported = p.Installable
			d.Installable = p.Installable
			d.Fields = p.fieldKeys()
			d.ConfigurationFields = append([]ComponentField(nil), p.Fields...)
			d.ExposureTypes = p.exposureTypes()
			d.CRDStrategy = p.CRDStrategy
			d.HookStrategy = p.HookStrategy
			d.StorageDefault = p.StorageDefault
			d.KubeVersion = p.KubeVersion
			if p.Notes != "" {
				d.Notes = p.Notes
			}
		}
		if d.Key == "ingress-nginx" {
			d.Supported, d.Installable = false, false
			d.Fields = nil
			d.ConfigurationFields = nil
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func hasComponentField(fields []string, want string) bool {
	for _, field := range fields {
		if field == want {
			return true
		}
	}
	return false
}
