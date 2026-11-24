package cluster

const componentPendingChartNote = "待核实官方 chart 版本后开放安装；当前仅展示。"

// extraComponentCatalog lists components shown in the component center but
// not yet installable: no curated profile, pinned version, or digest exists.
// Kept separate from componentCatalog so existing catalog/status endpoints and
// install validation are unaffected.
var extraComponentCatalog = []ComponentDefinition{
	{Key: "metrics-server", Name: "metrics-server", Category: "controller", Notes: componentPendingChartNote},
	{Key: "local-path-provisioner", Name: "local-path-provisioner", Category: "storage", Notes: componentPendingChartNote},
	{Key: "velero", Name: "Velero", Category: "storage", Notes: componentPendingChartNote},
	{Key: "kyverno", Name: "Kyverno", Category: "policy", Notes: componentPendingChartNote},
	{Key: "gatekeeper", Name: "OPA Gatekeeper", Category: "policy", Notes: componentPendingChartNote},
}
