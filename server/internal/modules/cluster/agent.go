package cluster

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"k8s.io/client-go/rest"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/tunnel"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/version"
)

// agentHub is the part of tunnel.Hub the cluster module uses (faked in tests).
type agentHub interface {
	RESTConfig(ctx context.Context, clusterID int64) (*rest.Config, string, error)
	Online(ctx context.Context, clusterID int64) (bool, error)
	Disconnect(clusterID int64)
}

var errAgentTokenRejected = errors.New("agent credentials rejected")

func agentSecretAAD(clusterID int64) []byte {
	return []byte("cluster:" + strconv.FormatInt(clusterID, 10) + ":agent-session")
}

// pgRegistry stores tunnel session ownership so every server replica can find it.
type pgRegistry struct {
	db     store.DB
	cipher *crypto.Cipher
}

func (r pgRegistry) Claim(ctx context.Context, clusterID int64, o tunnel.Owner) error {
	enc, err := r.cipher.EncryptBytes([]byte(o.Secret), agentSecretAAD(clusterID))
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx, `INSERT INTO cluster_agent_sessions(cluster_id,session_id,owner_addr,secret_enc,claimed_at,seen_at)
		VALUES($1,$2,$3,$4,now(),now())
		ON CONFLICT (cluster_id) DO UPDATE SET session_id=EXCLUDED.session_id, owner_addr=EXCLUDED.owner_addr,
		secret_enc=EXCLUDED.secret_enc, claimed_at=now(), seen_at=now()`, clusterID, o.SessionID, o.Addr, enc)
	return err
}

func (r pgRegistry) Heartbeat(ctx context.Context, clusterID int64, sessionID string) error {
	tag, err := r.db.Exec(ctx, `UPDATE cluster_agent_sessions SET seen_at=now() WHERE cluster_id=$1 AND session_id=$2`, clusterID, sessionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return tunnel.ErrSuperseded
	}
	_, _ = r.db.Exec(ctx, `UPDATE cluster_clusters SET agent_seen_at=now() WHERE id=$1`, clusterID)
	return nil
}

func (r pgRegistry) Release(ctx context.Context, clusterID int64, sessionID string) error {
	_, err := r.db.Exec(ctx, `DELETE FROM cluster_agent_sessions WHERE cluster_id=$1 AND session_id=$2`, clusterID, sessionID)
	return err
}

func (r pgRegistry) Lookup(ctx context.Context, clusterID int64) (tunnel.Owner, error) {
	var o tunnel.Owner
	var enc string
	err := r.db.QueryRow(ctx, `SELECT session_id,owner_addr,secret_enc,seen_at FROM cluster_agent_sessions WHERE cluster_id=$1`, clusterID).
		Scan(&o.SessionID, &o.Addr, &enc, &o.SeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return tunnel.Owner{}, tunnel.ErrOffline
	}
	if err != nil {
		return tunnel.Owner{}, err
	}
	secret, err := r.cipher.DecryptBytes(enc, agentSecretAAD(clusterID))
	if err != nil {
		return tunnel.Owner{}, err
	}
	o.Secret = string(secret)
	return o, nil
}

// relayKey returns the HMAC key shared by all replicas, creating it once.
func relayKey(ctx context.Context, db store.DB, cipher *crypto.Cipher) ([]byte, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	aad := []byte("cluster-agent:relay-key")
	enc, err := cipher.EncryptBytes(buf, aad)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(ctx, `INSERT INTO cluster_agent_keys(name,secret_enc) VALUES('relay',$1) ON CONFLICT (name) DO NOTHING`, enc); err != nil {
		return nil, err
	}
	var stored string
	if err := db.QueryRow(ctx, `SELECT secret_enc FROM cluster_agent_keys WHERE name='relay'`).Scan(&stored); err != nil {
		return nil, err
	}
	return cipher.DecryptBytes(stored, aad)
}

// authenticateAgent maps a join token to its cluster and pins the cluster fingerprint on first use.
func (m *Module) authenticateAgent(ctx context.Context, info tunnel.AgentInfo) (int64, error) {
	if !validAgentTokenFormat(info.Token) || strings.TrimSpace(info.Fingerprint) == "" || len(info.Fingerprint) > 64 {
		return 0, errAgentTokenRejected
	}
	var id int64
	var hash, pinned string
	err := m.d.DB.QueryRow(ctx, `SELECT id,agent_token_hash,agent_fingerprint FROM cluster_clusters
		WHERE connect_mode='agent' AND agent_token_hash=$1`, hashAgentToken(info.Token)).Scan(&id, &hash, &pinned)
	if err != nil || !agentTokenMatches(info.Token, hash) {
		return 0, errAgentTokenRejected
	}
	if pinned != "" && pinned != info.Fingerprint {
		m.log.Warn("cluster agent fingerprint mismatch", "cluster_id", id, "remote", info.RemoteAddr)
		return 0, errAgentTokenRejected
	}
	tag, err := m.d.DB.Exec(ctx, `UPDATE cluster_clusters SET agent_fingerprint=$2, agent_version=$3
		WHERE id=$1 AND agent_token_hash=$4 AND (agent_fingerprint='' OR agent_fingerprint=$2)`,
		id, info.Fingerprint, truncateRunes(info.Version, 64), hash)
	if err != nil || tag.RowsAffected() != 1 {
		return 0, errAgentTokenRejected
	}
	return id, nil
}

func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

func (m *Module) onAgentConnect(clusterID int64, info tunnel.AgentInfo, _ string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = m.d.DB.Exec(ctx, `UPDATE cluster_clusters SET agent_connected_at=now(), agent_seen_at=now(), status='connected', last_error='' WHERE id=$1`, clusterID)
	m.log.Info("cluster agent connected", "cluster_id", clusterID, "agent_version", info.Version)
	m.queueIntegrationDiscovery(ctx, clusterID)
}

func (m *Module) onAgentDisconnect(clusterID int64, _ string) {
	m.clients.drop(clusterID)
	m.log.Info("cluster agent disconnected", "cluster_id", clusterID)
}

// initAgentHub creates the tunnel hub. Without it, agent clusters fail closed as offline.
func (m *Module) initAgentHub(ctx context.Context, d *app.Deps) {
	if d.DB == nil || d.Crypto == nil {
		return
	}
	listen := ""
	if d.Cfg != nil {
		listen = d.Cfg.Server.Listen
	}
	selfAddr := tunnel.SelfAddrFromEnv(listen)
	key, err := relayKey(ctx, d.DB, d.Crypto)
	if err != nil {
		m.log.Error("cluster agent relay key unavailable; multi-replica relay disabled", "err", err)
		selfAddr, key = "", nil
	}
	hub, err := tunnel.NewHub(tunnel.Options{
		Authenticate: m.authenticateAgent,
		Registry:     pgRegistry{db: d.DB, cipher: d.Crypto},
		SelfAddr:     selfAddr,
		RelayKey:     key,
		OnConnect:    m.onAgentConnect,
		OnDisconnect: m.onAgentDisconnect,
		Logger:       m.log,
	})
	if err != nil {
		m.log.Error("cluster agent hub disabled", "err", err)
		return
	}
	m.hub, m.tunnelHub = hub, hub
	if d.Public != nil {
		d.Public.GET(tunnel.ConnectPath, gin.WrapF(hub.ServeAgent))
		d.Public.GET(tunnel.RelayPath, gin.WrapF(hub.ServeRelay))
	}
	if selfAddr == "" {
		m.log.Warn("MATRIX_POD_IP is not set; cluster agent tunnels only work on the replica the agent connected to")
	}
}

// agentClients returns clients that reach an agent cluster through the tunnel.
func (m *Module) agentClients(ctx context.Context, cl *Cluster) (*kubeClients, error) {
	if m.hub == nil {
		return nil, app.ErrClusterOffline
	}
	cfg, sessionID, err := m.hub.RESTConfig(ctx, cl.ID)
	if errors.Is(err, tunnel.ErrOffline) {
		return nil, app.ErrClusterOffline
	}
	if err != nil {
		return nil, err
	}
	return m.clients.getKeyed(cl.ID, "agent:"+sessionID, func() (*kubeClients, error) { return clientsFromRest(cfg) })
}

// markAgentOnline fills AgentOnline for agent clusters in a response.
func (m *Module) markAgentOnline(ctx context.Context, items ...*Cluster) {
	for _, cl := range items {
		if cl == nil || cl.ConnectMode != app.ClusterConnectAgent {
			continue
		}
		online := false
		if m.hub != nil {
			online, _ = m.hub.Online(ctx, cl.ID)
		}
		cl.AgentOnline = &online
	}
}

// clusterAccess implements app.ClusterAccess for other modules.
type clusterAccess struct{ m *Module }

func (a clusterAccess) Cluster(ctx context.Context, id int64) (app.ClusterRef, error) {
	cl, err := a.m.loadCluster(ctx, id)
	if err != nil {
		var httpErr *httpx.Error
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound {
			return app.ClusterRef{}, app.ErrClusterNotFound
		}
		return app.ClusterRef{}, err
	}
	return app.ClusterRef{ID: cl.ID, Name: cl.Name, Env: cl.Env, ConnectMode: cl.ConnectMode}, nil
}

func (a clusterAccess) RESTConfig(ctx context.Context, id int64) (*rest.Config, error) {
	cl, err := a.m.loadCluster(ctx, id)
	if err != nil {
		if _, refErr := a.Cluster(ctx, id); errors.Is(refErr, app.ErrClusterNotFound) {
			return nil, app.ErrClusterNotFound
		}
		return nil, err
	}
	kc, err := a.m.kube(ctx, cl)
	if err != nil {
		return nil, err
	}
	return rest.CopyConfig(kc.rest), nil
}

func (a clusterAccess) Kubeconfig(ctx context.Context, id int64) ([]byte, error) {
	cl, err := a.m.loadCluster(ctx, id)
	if err != nil {
		if _, refErr := a.Cluster(ctx, id); errors.Is(refErr, app.ErrClusterNotFound) {
			return nil, app.ErrClusterNotFound
		}
		return nil, err
	}
	if cl.ConnectMode == app.ClusterConnectAgent || cl.encKubeconfig == "" {
		return nil, app.ErrNoKubeconfig
	}
	raw, err := a.m.d.Crypto.Decrypt(cl.encKubeconfig)
	if err != nil {
		return nil, err
	}
	return []byte(raw), nil
}

type agentClusterInput struct {
	Name        string `json:"name" binding:"required,max=128"`
	Env         string `json:"env" binding:"required,max=64"`
	Description string `json:"description"`
}

// createAgentCluster registers a cluster that will join through matrix-cluster-agent.
func (m *Module) createAgentCluster(c *gin.Context) {
	var in agentClusterInput
	if !httpx.Bind(c, &in) {
		return
	}
	name, err := normalizeClusterName(in.Name)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	p := rbac.PrincipalFrom(c)
	in.Env = strings.TrimSpace(in.Env)
	if p == nil || in.Env == "" || !p.CanEnv(in.Env) {
		httpx.Fail(c, httpx.Forbidden("no access to environment "+in.Env))
		return
	}
	input := clusterInput{Name: name, Env: in.Env, Description: strings.TrimSpace(in.Description), ConnectMode: app.ClusterConnectAgent}
	res, ticket, err := m.runRegistryChange(c.Request.Context(), p, "create", nil, name, in.Env,
		registryMutation{Input: input, Actor: p.Username}, c.Query("reason"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "cluster", name)
	audit.SetAction(c, "cluster.create_agent")
	audit.SetDetail(c, gin.H{"name": name, "env": in.Env, "connect_mode": app.ClusterConnectAgent})
	if ticket != nil {
		approval.RespondPending(c, ticket)
		return
	}
	httpx.Created(c, res)
}

// agentManifest rotates the join token and returns the manifest to apply on the member cluster.
func (m *Module) agentManifest(c *gin.Context) {
	cl, ok := m.clusterFor(c)
	if !ok {
		return
	}
	if cl.ConnectMode != app.ClusterConnectAgent {
		httpx.Fail(c, httpx.BadRequest("该集群使用 kubeconfig 接入，无需 agent 清单"))
		return
	}
	var in agentManifestInput
	if !httpx.Bind(c, &in) {
		return
	}
	if strings.TrimSpace(in.ServerURL) == "" {
		in.ServerURL = requestBaseURL(c)
	}
	if err := in.normalize(agentDefaultImage(version.Version)); err != nil {
		httpx.Fail(c, httpx.Validation(err.Error()))
		return
	}
	token, hash, err := newAgentToken()
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	manifest, err := renderAgentManifest(cl.Name, token, in)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	ctx := c.Request.Context()
	err = store.InTx(ctx, m.d.DB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE cluster_clusters SET agent_token_hash=$2, updated_at=now() WHERE id=$1 AND connect_mode='agent'`, cl.ID, hash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return httpx.Conflict("集群已变更，请刷新后重试")
		}
		_, err = tx.Exec(ctx, `DELETE FROM cluster_agent_sessions WHERE cluster_id=$1`, cl.ID)
		return err
	})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if m.hub != nil {
		m.hub.Disconnect(cl.ID)
	}
	m.clients.drop(cl.ID)
	audit.SetAction(c, "cluster.agent_manifest")
	audit.SetDetail(c, gin.H{"name": cl.Name, "server_url": in.ServerURL, "image": in.Image, "namespace": in.Namespace,
		"enable_cicd": in.EnableCICD, "token_rotated": true})
	c.Header("Cache-Control", "no-store")
	httpx.OK(c, gin.H{
		"cluster_id": cl.ID, "manifest": manifest, "server_url": in.ServerURL, "image": in.Image,
		"namespace": in.Namespace, "enable_cicd": in.EnableCICD, "cicd_namespace": in.CICDNamespace,
		"apply_command": "kubectl apply -f matrix-cluster-agent-" + strconv.FormatInt(cl.ID, 10) + ".yaml",
	})
}

// resetAgentBinding clears the pinned cluster fingerprint so a rebuilt cluster can join again.
func (m *Module) resetAgentBinding(c *gin.Context) {
	cl, ok := m.clusterFor(c)
	if !ok {
		return
	}
	if cl.ConnectMode != app.ClusterConnectAgent {
		httpx.Fail(c, httpx.BadRequest("该集群不是 agent 接入"))
		return
	}
	ctx := c.Request.Context()
	err := store.InTx(ctx, m.d.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE cluster_clusters SET agent_fingerprint='', agent_token_hash='', updated_at=now() WHERE id=$1`, cl.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM cluster_agent_sessions WHERE cluster_id=$1`, cl.ID)
		return err
	})
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	if m.hub != nil {
		m.hub.Disconnect(cl.ID)
	}
	m.clients.drop(cl.ID)
	audit.SetAction(c, "cluster.agent_reset")
	audit.SetDetail(c, gin.H{"name": cl.Name})
	httpx.OK(c, gin.H{"cluster_id": cl.ID, "reset": true})
}

func requestBaseURL(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := c.GetHeader("X-Forwarded-Host")
	if host == "" {
		host = c.Request.Host
	}
	return fmt.Sprintf("%s://%s", scheme, strings.TrimSpace(strings.Split(host, ",")[0]))
}
