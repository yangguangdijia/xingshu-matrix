package bastion

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const AccessRequestType = "bastion.access_request"

type requestableAccount struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

type requestableAsset struct {
	ID       int64                `json:"id"`
	Name     string               `json:"name"`
	Host     string               `json:"host"`
	Env      string               `json:"env"`
	Accounts []requestableAccount `json:"accounts"`
}

func (m *Module) listRequestableAssets(c *gin.Context) {
	p := rbac.PrincipalFrom(c)
	rows, err := m.d.DB.Query(c, `SELECT a.id,a.name,a.host,a.env,acc.id,acc.name,acc.username
		FROM bastion_assets a JOIN bastion_accounts acc ON acc.asset_id=a.id
		WHERE a.protocol='ssh' ORDER BY a.name,acc.name`)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer rows.Close()

	out := make([]requestableAsset, 0)
	positions := make(map[int64]int)
	for rows.Next() {
		var assetID, accountID int64
		var assetName, host, env, accountName, username string
		if err := rows.Scan(&assetID, &assetName, &host, &env, &accountID, &accountName, &username); err != nil {
			httpx.Fail(c, err)
			return
		}
		if !p.CanEnv(env) {
			continue
		}
		i, exists := positions[assetID]
		if !exists {
			i = len(out)
			positions[assetID] = i
			out = append(out, requestableAsset{ID: assetID, Name: assetName, Host: host, Env: env, Accounts: []requestableAccount{}})
		}
		out[i].Accounts = append(out[i].Accounts, requestableAccount{ID: accountID, Name: accountName, Username: username})
	}
	if err := rows.Err(); err != nil {
		httpx.Fail(c, err)
		return
	}
	ok(c, out)
}

type accessRequestInput struct {
	AssetID         int64  `json:"asset_id" binding:"required,min=1"`
	AccountID       int64  `json:"account_id" binding:"required,min=1"`
	DurationMinutes int    `json:"duration_minutes" binding:"required,min=5,max=480"`
	Reason          string `json:"reason" binding:"required,min=4,max=2000"`
}

func (in *accessRequestInput) check() error {
	in.Reason = strings.TrimSpace(in.Reason)
	if in.AssetID < 1 || in.AccountID < 1 {
		return httpx.Validation("asset_id and account_id are required")
	}
	if in.DurationMinutes < 5 || in.DurationMinutes > 480 {
		return httpx.Validation("duration_minutes must be between 5 and 480")
	}
	if len([]rune(in.Reason)) < 4 || len([]rune(in.Reason)) > 2000 {
		return httpx.Validation("reason must contain between 4 and 2000 characters")
	}
	return nil
}

type accessRequestPayload struct {
	AssetID         int64 `json:"asset_id"`
	AccountID       int64 `json:"account_id"`
	DurationMinutes int   `json:"duration_minutes"`
}

func (m *Module) createAccessRequest(c *gin.Context) {
	var in accessRequestInput
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.check(); err != nil {
		httpx.Fail(c, err)
		return
	}
	p := rbac.PrincipalFrom(c)
	asset, err := queryOne[Asset](c, m, `SELECT * FROM bastion_assets WHERE id=$1`, in.AssetID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if asset.Protocol != "ssh" {
		httpx.Fail(c, httpx.Validation("temporary access requests currently support SSH assets only"))
		return
	}
	if !p.CanEnv(asset.Env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope: "+asset.Env))
		return
	}
	if _, err := queryOne[Account](c, m, `SELECT * FROM bastion_accounts WHERE id=$1 AND asset_id=$2`, in.AccountID, in.AssetID); err != nil {
		httpx.Fail(c, err)
		return
	}
	if m.d.Approval == nil {
		httpx.Fail(c, httpx.Conflict("审批服务不可用，临时访问申请已拒绝"))
		return
	}

	t, err := m.d.Approval.CreateOwned(c.Request.Context(), p, approval.CreateInput{
		Type:    AccessRequestType,
		Title:   fmt.Sprintf("SSH 临时访问：%s（%d 分钟）", asset.Name, in.DurationMinutes),
		Env:     asset.Env,
		Reason:  in.Reason,
		Payload: accessRequestPayload{AssetID: in.AssetID, AccountID: in.AccountID, DurationMinutes: in.DurationMinutes},
	})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "bastion_access_request", strconv.FormatInt(t.ID, 10))
	httpx.Created(c, t)
}

func (m *Module) approveAccessRequest(ctx context.Context, t *approval.Ticket) (any, error) {
	var payload accessRequestPayload
	if err := t.DecodePayload(&payload); err != nil {
		return nil, httpx.Validation("invalid access request payload")
	}
	if payload.AssetID < 1 || payload.AccountID < 1 || payload.DurationMinutes < 5 || payload.DurationMinutes > 480 {
		return nil, httpx.Validation("invalid access request payload")
	}
	if m.d.RBAC == nil {
		return nil, httpx.Unauthorized("RBAC service is unavailable")
	}

	var env, protocol string
	var accountAssetID int64
	err := m.d.DB.QueryRow(ctx, `SELECT a.env,a.protocol,acc.asset_id
		FROM bastion_assets a JOIN bastion_accounts acc ON acc.id=$2
		WHERE a.id=$1`, payload.AssetID, payload.AccountID).Scan(&env, &protocol, &accountAssetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpx.NotFound("requested asset or account no longer exists")
	}
	if err != nil {
		return nil, err
	}
	_, permissions, envScope, err := m.d.RBAC.UserAccess(ctx, t.ApplicantID)
	if err != nil {
		return nil, err
	}
	applicant := &rbac.Principal{UserID: t.ApplicantID, Permissions: permissions, EnvScope: envScope}
	if !applicant.Can(PermRequest) || !applicant.CanEnv(env) {
		return nil, httpx.Forbidden("applicant no longer has access request permission for this environment")
	}
	if accountAssetID != payload.AssetID || protocol != "ssh" || env != t.Env {
		return nil, httpx.Conflict("requested asset scope changed; submit a new access request")
	}

	var ruleID int64
	var expiresAt time.Time
	err = m.d.DB.QueryRow(ctx, `INSERT INTO bastion_rules
		(name,subject_type,subject,asset_ids,account_ids,expires_at,enabled)
		VALUES ($1,'user',$2,$3,$4,now()+make_interval(mins => $5),true)
		RETURNING id,expires_at`,
		fmt.Sprintf("临时访问申请 #%d", t.ID), strconv.FormatInt(t.ApplicantID, 10),
		[]int64{payload.AssetID}, []int64{payload.AccountID}, payload.DurationMinutes,
	).Scan(&ruleID, &expiresAt)
	if err != nil {
		return nil, err
	}
	return gin.H{"rule_id": ruleID, "expires_at": expiresAt}, nil
}
