// Package modules lists the feature modules compiled into matrix-server.
//
// To add a module: create internal/<yourmodule> implementing app.Module (see
// docs/dev-guide.md) and add ONE line to All() below. Keep the line on its own
// so parallel branches merge cleanly. Order matters only for migrations that
// reference another module's tables (list the dependency first).
package modules

import (
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/automation"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/bastion"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/cicd"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/cluster"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/devcenter"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/hosts"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/middleware"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/observability"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/provision"
)

// All returns the modules registered at startup, in order.
func All() []app.Module {
	return []app.Module{
		// ---- infrastructure ------------------------------------------------
		cluster.New(),   // internal/cluster: K8s clusters (kubeconfig / matrix-cluster-agent tunnel), nodes, workloads, Web Shell
		provision.New(), // SSH installation of fresh Kubernetes clusters, gated by environment approval
		hosts.New(),     // internal/hosts: matrix-agent enrolment (gRPC), host inventory, command/script execution
		bastion.New(),   // internal/bastion: SSH proxy + recording (MinIO), RDP/VNC via guacd, DB protocol proxy

		// ---- observability -------------------------------------------------
		observability.New(), // internal/observability: Prometheus query, Alertmanager webhook (use Deps.Public), logs (ES/Loki), traces

		// ---- delivery ------------------------------------------------------
		cicd.New(),       // internal/cicd: pipelines (K8s Job / Tekton), releases gated by approval.Gate, Helm
		automation.New(), // internal/automation: versioned, permissioned workflow definitions and runs
		devcenter.New(),  // internal/devcenter: requirements, iterations, bugs and test cases

		// ---- middleware ----------------------------------------------------
		middleware.New(), // internal/middleware: Kafka / RabbitMQ / RocketMQ admin, DB/Redis instances
	}
}
