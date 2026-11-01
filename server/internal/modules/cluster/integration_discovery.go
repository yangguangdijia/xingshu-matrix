package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

const (
	integrationDiscoveryInterval = 30 * time.Second
	integrationRefreshInterval   = 30 * time.Minute
	integrationLeaseDuration     = 90 * time.Second
)

type integrationDiscoveryRecord struct {
	ClusterID     int64                      `json:"cluster_id"`
	Status        string                     `json:"status"`
	DiscoveredAt  *time.Time                 `json:"discovered_at"`
	NextRefreshAt time.Time                  `json:"next_refresh_at"`
	UpdatedAt     time.Time                  `json:"updated_at"`
	Attempts      int                        `json:"attempts"`
	LastError     string                     `json:"last_error,omitempty"`
	Truncated     bool                       `json:"truncated"`
	Result        integrationCandidateResult `json:"result"`
}

type integrationDiscoveryClaim struct {
	ClusterID        int64
	ClusterUpdatedAt time.Time
	Generation       int64
	Attempts         int
}

// A successful cluster mutation has already committed by the time this is
// called. The worker also reconciles registrations on startup and periodically
// so a lost post-commit notification cannot permanently skip discovery.
func (m *Module) queueIntegrationDiscovery(ctx context.Context, id int64) {
	queueCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := m.enqueueIntegrationDiscovery(queueCtx, id); err != nil && m.log != nil {
		m.log.Warn("queue integration discovery; periodic reconciliation will retry", "cluster_id", id, "error", err)
	}
}

func (m *Module) enqueueIntegrationDiscovery(ctx context.Context, id int64) error {
	_, err := m.d.DB.Exec(ctx, `INSERT INTO cluster_integration_discovery (cluster_id, cluster_updated_at)
		SELECT id, updated_at FROM cluster_clusters WHERE id=$1
		ON CONFLICT (cluster_id) DO UPDATE SET
		cluster_updated_at=EXCLUDED.cluster_updated_at, status='pending', snapshot='{}'::jsonb,
		discovered_at=NULL, next_refresh_at=now(), lease_until=NULL, attempts=0,
		last_error='', generation=cluster_integration_discovery.generation+1, updated_at=now()`, id)
	if err == nil && m.discoveryWake != nil {
		select {
		case m.discoveryWake <- struct{}{}:
		default:
		}
	}
	return err
}

func (m *Module) reconcileIntegrationDiscovery(ctx context.Context) error {
	_, err := m.d.DB.Exec(ctx, `INSERT INTO cluster_integration_discovery (cluster_id, cluster_updated_at)
		SELECT id, updated_at FROM cluster_clusters WHERE true
		ON CONFLICT (cluster_id) DO UPDATE SET
		cluster_updated_at=EXCLUDED.cluster_updated_at, status='pending', snapshot='{}'::jsonb,
		discovered_at=NULL, next_refresh_at=now(), lease_until=NULL, attempts=0,
		last_error='', generation=cluster_integration_discovery.generation+1, updated_at=now()
		WHERE cluster_integration_discovery.cluster_updated_at IS DISTINCT FROM EXCLUDED.cluster_updated_at`)
	return err
}

// Start is called after migrations and registration. The database lease and
// generation fence make the scan safe with multiple Matrix replicas.
func (m *Module) Start(ctx context.Context, d *app.Deps) error {
	if d == nil || d.DB == nil {
		return errors.New("integration discovery requires a database")
	}
	if err := m.reconcileIntegrationDiscovery(ctx); err != nil {
		return fmt.Errorf("reconcile integration discovery: %w", err)
	}
	m.discoveryWG.Add(1)
	go func() {
		defer m.discoveryWG.Done()
		m.runIntegrationDiscovery(ctx)
	}()
	return nil
}

func (m *Module) Stop(ctx context.Context) error {
	done := make(chan struct{})
	go func() { m.discoveryWG.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Module) runIntegrationDiscovery(ctx context.Context) {
	ticker := time.NewTicker(integrationDiscoveryInterval)
	defer ticker.Stop()
	for {
		if err := m.reconcileIntegrationDiscovery(ctx); err != nil && ctx.Err() == nil {
			m.log.Warn("reconcile integration discovery", "error", err)
		}
		for i := 0; i < 8 && ctx.Err() == nil; i++ {
			claim, err := m.claimIntegrationDiscovery(ctx)
			if errors.Is(err, pgx.ErrNoRows) {
				break
			}
			if err != nil {
				if ctx.Err() == nil {
					m.log.Warn("claim integration discovery", "error", err)
				}
				break
			}
			m.refreshIntegrationDiscovery(ctx, claim)
		}
		select {
		case <-ctx.Done():
			return
		case <-m.discoveryWake:
		case <-ticker.C:
		}
	}
}

func (m *Module) claimIntegrationDiscovery(ctx context.Context) (integrationDiscoveryClaim, error) {
	var claim integrationDiscoveryClaim
	err := m.d.DB.QueryRow(ctx, `UPDATE cluster_integration_discovery SET
		status='running', lease_until=now()+($1::bigint * interval '1 second'),
		generation=generation+1, attempts=attempts+1, updated_at=now()
		WHERE cluster_id=(SELECT cluster_id FROM cluster_integration_discovery
			WHERE next_refresh_at<=now() AND (status<>'running' OR lease_until<now())
			ORDER BY next_refresh_at, cluster_id FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING cluster_id, cluster_updated_at, generation, attempts`, int64(integrationLeaseDuration/time.Second)).
		Scan(&claim.ClusterID, &claim.ClusterUpdatedAt, &claim.Generation, &claim.Attempts)
	return claim, err
}

func integrationRetryDelay(attempts int) time.Duration {
	delay := time.Minute
	for i := 1; i < attempts && i < 6; i++ {
		delay *= 2
	}
	return delay
}

func integrationResultState(result integrationCandidateResult) (string, string) {
	if result.Truncated {
		return "partial", "discovery inventory truncated"
	}
	for _, source := range result.Sources {
		if !source.Available {
			return "partial", "discovery inventory incomplete"
		}
	}
	return "ready", ""
}

func (m *Module) refreshIntegrationDiscovery(ctx context.Context, claim integrationDiscoveryClaim) {
	status, message := "error", "cluster discovery unavailable"
	result := integrationCandidateResult{ClusterID: claim.ClusterID, Items: []integrationCandidate{},
		Components: map[string]networkReadStatus{}, Sources: map[string]networkReadStatus{}}
	cl, err := m.loadCluster(ctx, claim.ClusterID)
	if err == nil {
		var kc *kubeClients
		kc, err = m.kube(ctx, cl)
		if err == nil {
			result = readIntegrationCandidates(ctx, kc, claim.ClusterID, "", integrationTypes)
			status, message = integrationResultState(result)
		}
	}
	if ctx.Err() != nil {
		return
	}
	raw, err := json.Marshal(result)
	if err != nil {
		status, message, raw = "error", "discovery result could not be stored", []byte(`{}`)
	}
	delay := integrationRefreshInterval
	if status != "ready" {
		delay = integrationRetryDelay(claim.Attempts)
	}
	_, err = m.d.DB.Exec(ctx, `UPDATE cluster_integration_discovery SET
		status=$4::varchar, snapshot=$5, discovered_at=CASE WHEN $4::varchar='error' THEN NULL ELSE now() END,
		next_refresh_at=now()+($6::bigint * interval '1 second'), lease_until=NULL,
		attempts=CASE WHEN $4::varchar='ready' THEN 0 ELSE attempts END,
		last_error=$7, updated_at=now()
		WHERE cluster_id=$1 AND generation=$2 AND cluster_updated_at=$3
		AND EXISTS (SELECT 1 FROM cluster_clusters WHERE id=$1 AND updated_at=$3)`,
		claim.ClusterID, claim.Generation, claim.ClusterUpdatedAt, status, raw, int64(delay/time.Second), message)
	if err != nil && ctx.Err() == nil {
		m.log.Warn("save integration discovery", "cluster_id", claim.ClusterID, "error", err)
	}
}

func (m *Module) loadIntegrationDiscovery(ctx context.Context, id int64) (integrationDiscoveryRecord, error) {
	record := integrationDiscoveryRecord{ClusterID: id, Status: "pending", Result: integrationCandidateResult{
		ClusterID: id, Items: []integrationCandidate{}, Components: map[string]networkReadStatus{},
		Sources: map[string]networkReadStatus{},
	}, NextRefreshAt: time.Now().UTC()}
	var raw []byte
	var current bool
	err := m.d.DB.QueryRow(ctx, `SELECT d.status,d.snapshot,d.discovered_at,d.next_refresh_at,d.updated_at,
		d.attempts,d.last_error,d.cluster_updated_at=c.updated_at
		FROM cluster_integration_discovery d JOIN cluster_clusters c ON c.id=d.cluster_id
		WHERE d.cluster_id=$1`, id).
		Scan(&record.Status, &raw, &record.DiscoveredAt, &record.NextRefreshAt,
			&record.UpdatedAt, &record.Attempts, &record.LastError, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return record, nil
	}
	if err != nil {
		return record, err
	}
	if !current {
		record.Status, record.DiscoveredAt, record.LastError, record.Attempts = "pending", nil, "", 0
		record.NextRefreshAt = time.Now().UTC()
		return record, nil
	}
	if err := json.Unmarshal(raw, &record.Result); err != nil {
		return record, err
	}
	if record.Result.Items == nil {
		record.Result.Items = []integrationCandidate{}
	}
	if record.Result.Components == nil {
		record.Result.Components = map[string]networkReadStatus{}
	}
	if record.Result.Sources == nil {
		record.Result.Sources = map[string]networkReadStatus{}
	}
	record.Result.ClusterID = id
	record.Truncated = record.Result.Truncated
	return record, nil
}

func (m *Module) integrationDiscovery(c *gin.Context) {
	cl, ok := m.clusterFor(c)
	if !ok {
		return
	}
	record, err := m.loadIntegrationDiscovery(c.Request.Context(), cl.ID)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	httpx.OK(c, record)
}

func (m *Module) integrationDiscoveryStatus(c *gin.Context) {
	cl, ok := m.clusterFor(c)
	if !ok {
		return
	}
	record, err := m.loadIntegrationDiscovery(c.Request.Context(), cl.ID)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	httpx.OK(c, gin.H{"cluster_id": cl.ID, "status": record.Status,
		"discovered_at": record.DiscoveredAt, "next_refresh_at": record.NextRefreshAt,
		"updated_at": record.UpdatedAt, "attempts": record.Attempts,
		"truncated": record.Truncated, "last_error": record.LastError})
}

// Binding suggestions are deliberately not datasource bindings. Only the
// observability module can create or report a persisted datasource binding.
func (m *Module) integrationBindingSuggestions(c *gin.Context) {
	cl, ok := m.clusterFor(c)
	if !ok {
		return
	}
	record, err := m.loadIntegrationDiscovery(c.Request.Context(), cl.ID)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	items := []integrationCandidate{}
	for _, item := range record.Result.Items {
		if !item.Collector && len(item.Addresses) > 0 {
			items = append(items, item)
		}
	}
	httpx.OK(c, gin.H{"cluster_id": cl.ID, "discovery_status": record.Status,
		"binding_source": "observability", "items": items, "bound": nil})
}

func (m *Module) refreshIntegrationDiscoveryRequest(c *gin.Context) {
	cl, ok := m.clusterFor(c)
	if !ok {
		return
	}
	if err := m.enqueueIntegrationDiscovery(c.Request.Context(), cl.ID); err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	audit.SetAction(c, "cluster.integration_discovery.refresh")
	httpx.OK(c, gin.H{"cluster_id": cl.ID, "status": "pending"})
}

// The worker belongs to the cluster module, not a global in-process map.
var _ app.Starter = (*Module)(nil)
var _ app.Stopper = (*Module)(nil)
