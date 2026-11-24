package aiops

import (
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// AnalyzerInput is the read-only snapshot the rule analyzers inspect. It is
// collected by investigate.go and consumed by analyzers.go (runAnalyzers).
// Contract file: do not edit in sub-tasks.
type AnalyzerInput struct {
	ClusterID   int64
	Namespace   string
	AlertAt     time.Time // zero when not from an alert
	Pods        []corev1.Pod
	Events      []corev1.Event
	Nodes       []corev1.Node
	Deployments []appsv1.Deployment
	ReplicaSets []appsv1.ReplicaSet
	PVCs        []corev1.PersistentVolumeClaim
	ConfigMaps  []string // names that exist in Namespace
	Secrets     []string // names that exist in Namespace
}

// CreateCaseInput is what the AI assistant tools and the HTTP API pass to
// createCase (implemented in cases.go).
type CreateCaseInput struct {
	Kind      string
	Source    string
	Title     string
	Request   string
	Env       string
	ClusterID *int64
	AlertID   *int64
	AssetID   *int64
	Target    []byte // JSON
}
