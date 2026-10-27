package automation

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	ansibleruntime "github.com/yangguangdijia/xingshu-matrix/server/internal/ansible"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

func (m *Module) ansiblePreflight(c *gin.Context) {
	var in ansiblePreflightInput
	if !httpx.Bind(c, &in) {
		return
	}
	env, targets, err := parsePreflightInput(in)
	if err != nil {
		httpx.Fail(c, httpx.Validation(err.Error()))
		return
	}
	p := rbac.PrincipalFrom(c)
	if !p.CanEnv(env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope"))
		return
	}
	if m.d.Crypto == nil {
		httpx.Fail(c, httpx.Conflict("credential encryption service unavailable"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	items := make([]sshHostKey, len(targets))
	errs := make([]error, len(targets))
	sem := make(chan struct{}, 5)
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target sshTarget) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()
			items[i], errs[i] = captureHostKey(ctx, target)
		}(i, target)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			httpx.Fail(c, httpx.Validation("SSH preflight failed; check target reachability and credentials"))
			return
		}
	}
	for i := range targets {
		targets[i].PublicKey = items[i].PublicKey
		targets[i].Fingerprint = items[i].Fingerprint
	}
	challenge, err := makePreflightChallenge(ansibleRunPayload{Targets: targets})
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	payload, err := json.Marshal(challenge.Payload)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	encrypted, err := m.d.Crypto.EncryptBytes(payload, []byte("automation:preflight:"+challenge.TokenHash))
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	if _, err := m.d.DB.Exec(ctx, `DELETE FROM automation_ansible_preflights WHERE expires_at<=now()`); err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	if _, err := m.d.DB.Exec(ctx, `INSERT INTO automation_ansible_preflights(token_hash,env,created_by,payload_enc,expires_at)
		VALUES($1,$2,$3,$4,now()+interval '10 minutes')`, challenge.TokenHash, env, p.UserID, encrypted); err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	audit.SetResource(c, "automation", "ansible-preflight")
	audit.SetDetail(c, gin.H{"env": env, "targets": len(items)})
	httpx.OK(c, gin.H{
		"items": items, "confirmation_token": challenge.Token,
		"expires_in_seconds": 600,
	})
}

func (m *Module) ansibleRuntime(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 6*time.Second)
	defer cancel()
	version, err := ansibleruntime.RuntimeVersion(ctx)
	result := gin.H{
		"available":  err == nil,
		"version":    version,
		"controller": "platform",
	}
	if err != nil {
		result["error"] = "embedded Ansible runtime unavailable; run the Matrix platform image"
	}
	httpx.OK(c, result)
}
