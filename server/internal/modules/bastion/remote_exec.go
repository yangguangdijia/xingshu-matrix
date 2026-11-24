package bastion

// remote_exec.go implements app.RemoteExecProvider: bounded, non-interactive
// command execution on bastion-managed SSH assets, plus asset onboarding and
// resolution for callers such as the AIOps module.
//
// Every execution goes through the same gates as an interactive SSH session:
// bastion:connect, an active asset/account grant, the caller's env scope, the
// login ACL and the command filters. Each call becomes one bastion session
// (lease + recording + login log + command audit), so it shows up in the
// session list and can be killed by an administrator.
//
// Non-interactive filter semantics:
//   deny    -> not executed, Result.Blocked set, audited as "deny"
//   approve -> not executed, an approval-required error is returned, audited as "cancelled"
//   confirm -> the caller's plan review counts as the confirmation; executed, audited as "confirmed"
//   allow / no match -> executed, audited as "allow"
//
// Credentials are decrypted only inside the SSH dial; they never enter
// errors, logs, audit rows, recordings or the returned result (the account's
// own password is additionally scrubbed from captured output and commands).

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const (
	remoteExecMaxTimeout     = 30 * time.Minute
	remoteExecDefaultTimeout = 5 * time.Minute
	remoteExecMaxOutput      = 256 << 10 // 256 KiB
	remoteExecMaxCommand     = 16 << 10
	remoteExecDialTimeout    = 10 * time.Second
	remoteExecProbeTimeout   = 5 * time.Second
	remoteExecTruncNote      = "\n[output truncated at 256 KiB]"
)

var _ app.RemoteExecProvider = (*Module)(nil)

// ---------- pure helpers (unit tested) ----------

// clampExecTimeout applies the default (<=0) and the 30 minute ceiling.
func clampExecTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return remoteExecDefaultTimeout
	}
	if d > remoteExecMaxTimeout {
		return remoteExecMaxTimeout
	}
	return d
}

// truncateOutput cuts b to at most max bytes without splitting a UTF-8 rune.
func truncateOutput(b []byte, max int) ([]byte, bool) {
	if max < 0 {
		max = 0
	}
	if len(b) <= max {
		return b, false
	}
	cut := max
	for cut > 0 && cut > max-utf8.UTFMax && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return b[:cut], true
}

// cappedBuffer is a concurrency-safe writer for combined stdout/stderr that
// keeps the first limit bytes (plus a small slack so the final cut can land on
// a rune boundary) and silently discards the rest, so a chatty command can
// never exhaust memory or block on a full pipe.
type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func newCappedBuffer(limit int) *cappedBuffer { return &cappedBuffer{limit: limit} }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.limit + utf8.UTFMax - c.buf.Len()
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

// Bytes returns the captured output cut to limit, and whether anything was dropped.
func (c *cappedBuffer) Bytes() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out, cut := truncateOutput(append([]byte(nil), c.buf.Bytes()...), c.limit)
	return out, cut || c.truncated
}

// execDecision maps the filter rule hit for a non-interactive command.
type execDecision struct {
	Run           bool   // execute the command
	AuditAction   string // value for bastion_commands.action
	Blocked       string // non-empty when denied
	NeedsApproval bool   // approve rule: return an approval-required error
	RuleID        int64
}

func decideExec(rule *FilterRule) execDecision {
	if rule == nil {
		return execDecision{Run: true, AuditAction: "allow"}
	}
	name := rule.Name
	if name == "" {
		name = "#" + itoa(rule.ID)
	}
	switch rule.Action {
	case "deny":
		return execDecision{AuditAction: "deny", RuleID: rule.ID, Blocked: fmt.Sprintf("命令被过滤规则「%s」禁止", name)}
	case "approve":
		return execDecision{AuditAction: "cancelled", RuleID: rule.ID, NeedsApproval: true,
			Blocked: fmt.Sprintf("命令命中需审批规则「%s」，非交互执行不能发起命令审批，请在终端中执行或调整规则", name)}
	case "confirm":
		return execDecision{Run: true, AuditAction: "confirmed", RuleID: rule.ID}
	case "allow":
		return execDecision{Run: true, AuditAction: "allow", RuleID: rule.ID}
	}
	// Unknown action: fail closed.
	return execDecision{AuditAction: "deny", RuleID: rule.ID, Blocked: fmt.Sprintf("命令过滤规则「%s」动作无效，已拒绝", name)}
}

// scrubSecret replaces every occurrence of secret (if long enough to be
// meaningful) with a fixed mask.
func scrubSecret(b []byte, secret string) []byte {
	if len(secret) < 4 {
		return b
	}
	return bytes.ReplaceAll(b, []byte(secret), []byte("******"))
}

// osFromBanner guesses the OS family from an SSH identification string such as
// "SSH-2.0-OpenSSH_8.9p1 Ubuntu-3ubuntu0.6". It never logs in.
func osFromBanner(banner string) string {
	l := strings.ToLower(banner)
	for _, k := range []struct{ key, os string }{
		{"ubuntu", "linux/ubuntu"}, {"debian", "linux/debian"}, {"raspbian", "linux/raspbian"},
		{"centos", "linux/centos"}, {"rhel", "linux/rhel"}, {"red hat", "linux/rhel"},
		{"fedora", "linux/fedora"}, {"suse", "linux/suse"}, {"alpine", "linux/alpine"},
		{"freebsd", "freebsd"}, {"openbsd", "openbsd"}, {"netbsd", "netbsd"},
		{"windows", "windows"}, {"dropbear", "linux"},
	} {
		if strings.Contains(l, k.key) {
			return k.os
		}
	}
	if strings.HasPrefix(l, "ssh-") {
		return "unknown"
	}
	return ""
}

// splitNodePath turns "a/b/c" (or "/a/b/c/") into its non-empty segments.
func splitNodePath(path string) []string {
	var out []string
	for _, s := range strings.Split(path, "/") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ---------- RemoteExec ----------

func (m *Module) RemoteExec(ctx context.Context, p *rbac.Principal, req app.RemoteExecRequest) (app.RemoteExecResult, error) {
	var res app.RemoteExecResult
	if m == nil || m.d == nil || m.d.DB == nil {
		return res, httpx.Conflict("bastion is not available")
	}
	if p == nil || !p.Can(PermConnect) {
		return res, httpx.Forbidden("permission " + PermConnect + " required")
	}
	cmd := strings.TrimSpace(req.Command)
	if cmd == "" {
		return res, httpx.Validation("command is required")
	}
	if len(cmd) > remoteExecMaxCommand {
		return res, httpx.Validation("command is too long (max 16 KiB)")
	}
	if strings.ContainsRune(cmd, 0) {
		return res, httpx.Validation("command must not contain NUL bytes")
	}
	timeout := clampExecTimeout(req.Timeout)

	asset, err := m.execAsset(ctx, p, req.AssetID)
	if err != nil {
		return res, err
	}
	var acc *Account
	if req.AccountID > 0 {
		acc, err = queryOne[Account](ctx, m, `SELECT * FROM bastion_accounts WHERE id=$1 AND asset_id=$2`, req.AccountID, asset.ID)
		if err != nil {
			return res, err
		}
	} else {
		if acc, err = m.defaultExecAccount(ctx, p, asset.ID); err != nil {
			return res, err
		}
		if acc == nil {
			return res, httpx.Forbidden("no authorized account on this asset for the caller")
		}
	}
	logIn := loginLog{UserID: p.UserID, Username: p.Username, AssetID: asset.ID, AssetName: asset.Name, Account: acc.Username, Protocol: "ssh"}
	if okk, err := m.authorized(ctx, p, asset.ID, acc.ID); err != nil {
		return res, err
	} else if !okk {
		logIn.Result, logIn.Reason = "denied", "没有生效的资产授权（远程执行）"
		m.writeLoginLog(logIn)
		return res, httpx.Forbidden("no active authorization rule for this asset/account")
	}
	// No HTTP client here: ACL rules restricted by source IP see no address.
	if reason, err := m.checkACL(ctx, p, asset.ID, acc.Username, ""); err != nil {
		return res, err
	} else if reason != "" {
		logIn.Result, logIn.Reason = "denied", reason
		m.writeLoginLog(logIn)
		return res, httpx.Forbidden(reason)
	}
	auth, err := m.authMethod(acc)
	if err != nil {
		logIn.Result, logIn.Reason = "failed", "托管凭据不可用"
		m.writeLoginLog(logIn)
		return res, httpx.Conflict("SSH credential is unavailable or invalid")
	}
	plainSecret := ""
	if acc.AuthType == "password" {
		plainSecret, _ = m.d.Crypto.Decrypt(acc.Secret) // only used to scrub output
	}
	filters, err := m.loadFilters(ctx, asset.ID)
	if err != nil {
		return res, err
	}
	masker, err := m.loadMasker(ctx)
	if err != nil {
		return res, err
	}
	clean := func(b []byte) []byte { return masker.Apply(scrubSecret(b, plainSecret)) }
	decision := decideExec(Evaluate(filters, cmd))

	if err := m.recordingReady(); err != nil {
		return res, httpx.Conflict("shared durable recording storage is unavailable; remote exec refused")
	}
	m.mu.Lock()
	unavailable := m.stopping || m.store == nil
	m.mu.Unlock()
	if unavailable {
		return res, httpx.Conflict("bastion session owner is unavailable")
	}

	// ---- session record (same shape as the interactive SSH session) ----
	ref := sessionRef{Owner: m.owner, Fence: newToken()}
	createCtx, createCancel := context.WithTimeout(context.Background(), databaseTimeout)
	err = m.d.DB.QueryRow(createCtx, `INSERT INTO bastion_sessions
		(user_id,username,asset_id,asset_name,account_id,account_user,protocol,client_ip,
		 owner_id,fence_token,heartbeat_at,lease_expires_at,recording_state,recording_storage_id)
		VALUES ($1,$2,$3,$4,$5,$6,'ssh',$7,$8,$9,clock_timestamp(),
		 clock_timestamp()+make_interval(secs => $10),'unavailable',$11) RETURNING id`,
		p.UserID, p.Username, asset.ID, asset.Name, acc.ID, acc.Username, "",
		ref.Owner, ref.Fence, leaseSeconds, m.storageID).Scan(&ref.ID)
	createCancel()
	if err != nil {
		return res, httpx.Conflict("session audit could not be created")
	}
	res.SessionID = itoa(ref.ID)

	sessCtx, cancel := context.WithCancel(context.Background())
	end := sessionEnd{Status: "closed", RecordingState: "unavailable", StorageID: m.storageID}
	if !m.register(ref, cancel) {
		cancel()
		end.Status, end.Error = "failed", "owner is shutting down"
		finalCtx, finalCancel := context.WithTimeout(context.Background(), databaseTimeout)
		_ = m.store.finish(finalCtx, ref, end)
		finalCancel()
		return res, httpx.Conflict("bastion session owner is unavailable")
	}
	watchDone := m.watchLease(sessCtx, ref)
	var rec *Recorder
	defer func() {
		if rec != nil {
			if err := rec.Close(); err != nil {
				end.RecordingState = "failed"
				end.Status, end.Error = "failed", "recording persistence failed"
			} else if end.RecordingState == "recording" {
				end.RecordingState = "ready"
			}
		}
		switch m.sessionReason(ref.ID) {
		case "killed":
			end.Status = "killed"
		case "shutdown":
			end.Status, end.Error = "closed", "owner shut down"
		case "lease_lost":
			end.Status, end.Error = "failed", "session ownership or lease lost"
		}
		finalCtx, finalCancel := context.WithTimeout(context.Background(), databaseTimeout)
		if err := m.store.finish(finalCtx, ref, end); err != nil && m.d.Logger != nil {
			m.d.Logger.Warn("bastion: remote exec session final audit not persisted", "session", ref.ID)
		}
		finalCancel()
		cancel()
		<-watchDone
		m.unregister(ref.ID)
	}()

	title := fmt.Sprintf("%s@%s by %s (remote exec)", acc.Username, asset.Name, p.Username)
	if r := strings.TrimSpace(req.Reason); r != "" {
		if len(r) > 200 {
			r = r[:200]
		}
		title += " — " + string(clean([]byte(r)))
	}
	var recPath string
	rec, recPath, err = m.createRecording(ref, 160, 40, title)
	if err != nil {
		rec = nil
		end.Status, end.Error, end.RecordingState = "failed", "durable recording could not be created", "failed"
		return res, httpx.Conflict("durable recording could not be created")
	}
	end.RecordingPath, end.RecordingState = recPath, "recording"
	recordCtx, recordCancel := context.WithTimeout(sessCtx, databaseTimeout)
	tag, err := m.d.DB.Exec(recordCtx, `UPDATE bastion_sessions SET recording_path=$4,
		recording_state='recording' WHERE id=$1 AND owner_id=$2 AND fence_token=$3
		AND status='active' AND lease_expires_at>clock_timestamp() AND kill_requested_at IS NULL`,
		ref.ID, ref.Owner, ref.Fence, recPath)
	recordCancel()
	if err != nil || tag.RowsAffected() != 1 {
		end.Status, end.Error = "failed", "recording audit could not be persisted"
		return res, httpx.Conflict("recording audit could not be persisted")
	}
	auditedCmd := string(clean([]byte(cmd)))
	_ = rec.Output([]byte("$ " + auditedCmd + "\r\n"))

	// ---- command filter ----
	ids, err := m.auditCommands(sessCtx, ref, []CommandEvent{{Command: auditedCmd, Action: decision.AuditAction, RuleID: decision.RuleID}})
	if err != nil {
		end.Status, end.Error = "failed", "command audit could not be persisted"
		return res, httpx.Conflict("command audit could not be persisted")
	}
	if !decision.Run {
		res.Blocked = decision.Blocked
		_ = rec.Output([]byte("[blocked] " + decision.Blocked + "\r\n"))
		end.Error = "command blocked by filter"
		if decision.NeedsApproval {
			return res, httpx.Conflict(decision.Blocked)
		}
		return res, nil
	}

	// ---- SSH exec (no PTY) ----
	runCtx, runCancel := context.WithTimeout(sessCtx, timeout)
	defer runCancel()
	stopCaller := context.AfterFunc(ctx, runCancel) // caller cancellation also stops the command
	defer stopCaller()

	addr := net.JoinHostPort(asset.Host, strconv.Itoa(asset.Port))
	dialCtx, dialCancel := context.WithTimeout(runCtx, remoteExecDialTimeout)
	raw, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", addr)
	dialCancel()
	if err != nil {
		logIn.Result, logIn.Reason, logIn.SessionID = "failed", "网络连接失败", &ref.ID
		m.writeLoginLog(logIn)
		_ = m.commandDelivery(sessCtx, ref, ids, err)
		end.Status, end.Error = "failed", "SSH network connection failed"
		return res, httpx.Conflict("SSH network connection failed")
	}
	defer raw.Close()
	stopConn := context.AfterFunc(runCtx, func() { _ = raw.Close() })
	defer stopConn()
	_ = raw.SetDeadline(time.Now().Add(remoteExecDialTimeout))
	conn, chans, reqs, err := ssh.NewClientConn(raw, addr, &ssh.ClientConfig{
		User: acc.Username, Auth: []ssh.AuthMethod{auth},
		HostKeyCallback: m.hostKeyCallback(asset), Timeout: remoteExecDialTimeout,
	})
	if err != nil {
		logIn.Result, logIn.Reason, logIn.SessionID = "failed", "SSH 握手、认证或主机指纹校验失败", &ref.ID
		m.writeLoginLog(logIn)
		_ = m.commandDelivery(sessCtx, ref, ids, err)
		end.Status, end.Error = "failed", "SSH handshake, authentication or host key verification failed"
		return res, httpx.Conflict("SSH handshake, authentication or host key verification failed")
	}
	logIn.Result, logIn.Reason, logIn.SessionID = "success", "", &ref.ID
	m.writeLoginLog(logIn)
	_ = raw.SetDeadline(time.Time{})
	client := ssh.NewClient(conn, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		_ = m.commandDelivery(sessCtx, ref, ids, err)
		end.Status, end.Error = "failed", "SSH session creation failed"
		return res, httpx.Conflict("SSH session creation failed")
	}
	defer sess.Close()
	out := newCappedBuffer(remoteExecMaxOutput)
	sess.Stdout, sess.Stderr = out, out

	start := time.Now()
	if err := sess.Start(cmd); err != nil {
		_ = m.commandDelivery(sessCtx, ref, ids, err)
		end.Status, end.Error = "failed", "remote command could not be started"
		return res, httpx.Conflict("remote command could not be started")
	}
	_ = m.commandDelivery(sessCtx, ref, ids, nil)
	waitErr := make(chan error, 1)
	go func() { waitErr <- sess.Wait() }()
	var runErr error
	select {
	case runErr = <-waitErr:
	case <-runCtx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		_ = client.Close()
		<-waitErr
		runErr = runCtx.Err()
	}
	res.Duration = time.Since(start)

	body, truncated := out.Bytes()
	body = clean(body)
	if truncated {
		// masking may change length; re-cut so the 256 KiB contract holds
		body, _ = truncateOutput(body, remoteExecMaxOutput-len(remoteExecTruncNote))
		body = append(body, remoteExecTruncNote...)
	} else if len(body) > remoteExecMaxOutput {
		body, _ = truncateOutput(body, remoteExecMaxOutput-len(remoteExecTruncNote))
		body = append(body, remoteExecTruncNote...)
		truncated = true
	}
	res.Output, res.Truncated = string(body), truncated
	_ = rec.Output(bytes.ReplaceAll(body, []byte("\n"), []byte("\r\n")))

	var exitErr *ssh.ExitError
	switch {
	case runErr == nil:
		res.ExitCode = 0
	case errors.As(runErr, &exitErr):
		res.ExitCode = exitErr.ExitStatus()
	default:
		res.ExitCode = -1
		switch reason := m.sessionReason(ref.ID); {
		case reason == "killed":
			return res, httpx.Conflict("remote exec session killed by administrator")
		case reason != "":
			end.Status, end.Error = "failed", "session interrupted: "+reason
			return res, httpx.Conflict("remote exec session interrupted: " + reason)
		case errors.Is(runErr, context.DeadlineExceeded):
			end.Status, end.Error = "failed", "command timed out after "+timeout.String()
			_ = rec.Output([]byte("\r\n[timeout after " + timeout.String() + "]\r\n"))
			return res, httpx.Conflict("remote command timed out after " + timeout.String())
		case errors.Is(runErr, context.Canceled):
			end.Status, end.Error = "failed", "cancelled by caller"
			return res, ctx.Err()
		}
		// ExitMissingError etc.: connection dropped without an exit status.
		end.Status, end.Error = "failed", "remote command ended without exit status"
		return res, httpx.Conflict("remote command ended without an exit status")
	}
	return res, nil
}

// execAsset loads an SSH asset the caller may operate on (env scope).
func (m *Module) execAsset(ctx context.Context, p *rbac.Principal, assetID int64) (*Asset, error) {
	if assetID <= 0 {
		return nil, httpx.Validation("asset id is required")
	}
	asset, err := queryOne[Asset](ctx, m, `SELECT * FROM bastion_assets WHERE id=$1`, assetID)
	if err != nil {
		return nil, err
	}
	if asset.Protocol != "ssh" {
		return nil, httpx.BadRequest("asset protocol is " + asset.Protocol + ", remote exec supports ssh only")
	}
	if !p.CanEnv(asset.Env) {
		return nil, httpx.Forbidden("environment " + asset.Env + " not in your scope")
	}
	return asset, nil
}

// defaultExecAccount picks the account used when AccountID is 0: the first
// account on the asset the caller holds a connect grant for, preferring root.
func (m *Module) defaultExecAccount(ctx context.Context, p *rbac.Principal, assetID int64) (*Account, error) {
	accs, err := queryAll[Account](ctx, m, `SELECT * FROM bastion_accounts WHERE asset_id=$1 ORDER BY (username='root') DESC, id`, assetID)
	if err != nil {
		return nil, err
	}
	for i := range accs {
		okk, err := m.authorized(ctx, p, assetID, accs[i].ID)
		if err != nil {
			return nil, err
		}
		if okk {
			return &accs[i], nil
		}
	}
	return nil, nil
}

// ---------- OnboardAsset ----------

func (m *Module) OnboardAsset(ctx context.Context, p *rbac.Principal, req app.AssetOnboardRequest) (app.AssetRef, error) {
	if m == nil || m.d == nil || m.d.DB == nil {
		return app.AssetRef{}, httpx.Conflict("bastion is not available")
	}
	if p == nil || !p.Can(PermManage) {
		return app.AssetRef{}, httpx.Forbidden("permission " + PermManage + " required")
	}
	host := strings.TrimSpace(req.Address)
	if host == "" {
		return app.AssetRef{}, httpx.Validation("address is required")
	}
	if h, port, err := net.SplitHostPort(host); err == nil && req.Port == 0 {
		host = h
		req.Port, _ = strconv.Atoi(port)
	}
	if req.Port == 0 {
		req.Port = 22
	}
	username := strings.TrimSpace(req.Username)
	if username == "" {
		return app.AssetRef{}, httpx.Validation("username is required")
	}
	if (req.Password == "") == (req.PrivateKey == "") {
		return app.AssetRef{}, httpx.Validation("exactly one of password or private key is required")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = host
	}
	in := assetIn{Name: name, Host: host, Port: req.Port, Protocol: "ssh", Env: strings.TrimSpace(req.Env),
		Description: "onboarded via remote exec API by " + p.Username}
	if in.Port < 1 || in.Port > 65535 {
		return app.AssetRef{}, httpx.Validation("port must be between 1 and 65535")
	}
	if err := in.check(); err != nil {
		return app.AssetRef{}, err
	}
	if !p.CanEnv(in.Env) {
		return app.AssetRef{}, httpx.Forbidden("environment " + in.Env + " not in your scope")
	}
	acct := accountIn{Name: username, Username: username, AuthType: "password", Secret: req.Password}
	if req.PrivateKey != "" {
		acct.AuthType, acct.Secret = "private_key", req.PrivateKey
		if _, err := parseSigner(req.PrivateKey, ""); err != nil {
			return app.AssetRef{}, httpx.Validation("invalid private key (encrypted keys are not supported here)")
		}
	}
	sec, pass, err := m.encSecrets(&acct)
	if err != nil {
		return app.AssetRef{}, httpx.Conflict("credential could not be encrypted")
	}

	tx, err := m.d.DB.Begin(ctx)
	if err != nil {
		return app.AssetRef{}, err
	}
	defer tx.Rollback(ctx)
	if segs := splitNodePath(req.Node); len(segs) > 0 {
		var parent *int64
		for _, s := range segs {
			var id int64
			if err := tx.QueryRow(ctx, `SELECT id FROM bastion_nodes WHERE name=$1 AND COALESCE(parent_id,0)=COALESCE($2::bigint,0)`, s, parent).Scan(&id); err != nil {
				return app.AssetRef{}, httpx.Validation("asset tree node not found: " + req.Node)
			}
			parent = &id
		}
		in.NodeID = parent
	}
	var ref app.AssetRef
	if err := tx.QueryRow(ctx, `INSERT INTO bastion_assets (name,host,port,protocol,env,description,host_key,options,node_id,platform_id)
		VALUES ($1,$2,$3,$4,$5,$6,'',$7,$8,NULL) RETURNING id`,
		in.Name, in.Host, in.Port, in.Protocol, in.Env, in.Description, in.Options, in.NodeID).Scan(&ref.AssetID); err != nil {
		return app.AssetRef{}, mapPgErr(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO bastion_accounts (asset_id,name,username,auth_type,secret_enc,passphrase_enc)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`, ref.AssetID, acct.Name, acct.Username, acct.AuthType, sec, pass).Scan(&ref.AccountID); err != nil {
		return app.AssetRef{}, mapPgErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return app.AssetRef{}, err
	}
	ref.Name, ref.Address, ref.Env = in.Name, net.JoinHostPort(in.Host, strconv.Itoa(in.Port)), in.Env
	ref.Reachable, ref.OS = probeSSH(ctx, in.Host, in.Port)
	return ref, nil
}

// ---------- ResolveAsset ----------

func (m *Module) ResolveAsset(ctx context.Context, p *rbac.Principal, assetID int64) (app.AssetRef, error) {
	if m == nil || m.d == nil || m.d.DB == nil {
		return app.AssetRef{}, httpx.Conflict("bastion is not available")
	}
	if p == nil || !p.Can(PermConnect) {
		return app.AssetRef{}, httpx.Forbidden("permission " + PermConnect + " required")
	}
	asset, err := m.execAsset(ctx, p, assetID)
	if err != nil {
		return app.AssetRef{}, err
	}
	ref := app.AssetRef{AssetID: asset.ID, Name: asset.Name, Env: asset.Env,
		Address: net.JoinHostPort(asset.Host, strconv.Itoa(asset.Port))}
	acc, err := m.defaultExecAccount(ctx, p, asset.ID)
	if err != nil {
		return app.AssetRef{}, err
	}
	if acc != nil {
		ref.AccountID = acc.ID // 0 = caller holds no connect grant on this asset
	}
	ref.Reachable, ref.OS = probeSSH(ctx, asset.Host, asset.Port)
	return ref, nil
}

// probeSSH is a read-only reachability probe: a TCP connect plus the SSH
// identification banner. It never authenticates.
func probeSSH(ctx context.Context, host string, port int) (bool, string) {
	pctx, cancel := context.WithTimeout(ctx, remoteExecProbeTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(pctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false, ""
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, _ := bufio.NewReader(conn).ReadString('\n')
	return true, osFromBanner(strings.TrimSpace(line))
}
