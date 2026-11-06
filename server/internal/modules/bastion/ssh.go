package bastion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// wsMsg is the xterm.js wire protocol.
type wsMsg struct {
	Type string `json:"type"` // input | resize | output | error | closed
	Data string `json:"data,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 8192,
	Subprotocols:    []string{"xterm"},
	// Auth is a bearer/access token (not a cookie), so cross-origin hijacking
	// cannot ride on ambient credentials.
	CheckOrigin: func(*http.Request) bool { return true },
}

func parseSigner(key, passphrase string) (ssh.Signer, error) {
	if passphrase != "" {
		return ssh.ParsePrivateKeyWithPassphrase([]byte(key), []byte(passphrase))
	}
	return ssh.ParsePrivateKey([]byte(key))
}

func (m *Module) authMethod(a *Account) (ssh.AuthMethod, error) {
	secret, err := m.d.Crypto.Decrypt(a.Secret)
	if err != nil {
		return nil, fmt.Errorf("decrypt credential: %w", err)
	}
	if a.AuthType == "password" {
		return ssh.Password(secret), nil
	}
	pass := ""
	if a.Passphrase != "" {
		if pass, err = m.d.Crypto.Decrypt(a.Passphrase); err != nil {
			return nil, fmt.Errorf("decrypt passphrase: %w", err)
		}
	}
	s, err := parseSigner(secret, pass)
	if err != nil {
		return nil, err
	}
	return ssh.PublicKeys(s), nil
}

// hostKeyCallback verifies against the stored key, trusting and saving it on first use.
func (m *Module) hostKeyCallback(asset *Asset) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if asset.HostKey == "" {
			k := string(bytes.TrimSpace(ssh.MarshalAuthorizedKey(key)))
			ctx, cancel := context.WithTimeout(context.Background(), databaseTimeout)
			defer cancel()
			// Concurrent first connections must all compare with the winning
			// stored key, rather than accepting an UPDATE that affected no rows.
			var stored string
			err := m.d.DB.QueryRow(ctx, `UPDATE bastion_assets SET host_key=
				CASE WHEN host_key='' THEN $2 ELSE host_key END WHERE id=$1 RETURNING host_key`, asset.ID, k).Scan(&stored)
			if err != nil {
				return errors.New("host key could not be verified")
			}
			want, _, _, _, err := ssh.ParseAuthorizedKey([]byte(stored))
			if err != nil || !bytes.Equal(want.Marshal(), key.Marshal()) {
				return errors.New("host key mismatch")
			}
			return nil
		}
		want, _, _, _, err := ssh.ParseAuthorizedKey([]byte(asset.HostKey))
		if err != nil {
			return fmt.Errorf("stored host key invalid: %w", err)
		}
		if !bytes.Equal(want.Marshal(), key.Marshal()) {
			return fmt.Errorf("host key mismatch (got %s)", ssh.FingerprintSHA256(key))
		}
		return nil
	}
}

func qInt(c *gin.Context, k string, def int) int {
	if v, err := strconv.Atoi(c.Query(k)); err == nil && v > 0 {
		return v
	}
	return def
}

func (m *Module) sshWS(c *gin.Context) {
	p := rbac.PrincipalFrom(c)
	assetID, _ := strconv.ParseInt(c.Query("asset"), 10, 64)
	accountID, _ := strconv.ParseInt(c.Query("account"), 10, 64)
	if assetID <= 0 || accountID <= 0 {
		httpx.Fail(c, httpx.BadRequest("asset and account query params required"))
		return
	}
	asset, err := queryOne[Asset](c, m, `SELECT * FROM bastion_assets WHERE id=$1`, assetID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if asset.Protocol != "ssh" {
		httpx.Fail(c, httpx.BadRequest("asset protocol is "+asset.Protocol+", only ssh supported by this endpoint"))
		return
	}
	if !p.CanEnv(asset.Env) {
		httpx.Fail(c, httpx.Forbidden("environment "+asset.Env+" not in your scope"))
		return
	}
	acc, err := queryOne[Account](c, m, `SELECT * FROM bastion_accounts WHERE id=$1 AND asset_id=$2`, accountID, assetID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	logIn := loginLog{UserID: p.UserID, Username: p.Username, AssetID: asset.ID, AssetName: asset.Name, Account: acc.Username, Protocol: "ssh", ClientIP: c.ClientIP()}
	if okk, err := m.authorized(c, p, assetID, accountID); err != nil {
		httpx.Fail(c, err)
		return
	} else if !okk {
		logIn.Result, logIn.Reason = "denied", "没有生效的资产授权"
		m.writeLoginLog(logIn)
		httpx.Fail(c, httpx.Forbidden("no active authorization rule for this asset/account"))
		return
	}
	if reason, err := m.checkACL(c, p, asset.ID, c.ClientIP()); err != nil {
		httpx.Fail(c, err)
		return
	} else if reason != "" {
		logIn.Result, logIn.Reason = "denied", reason
		m.writeLoginLog(logIn)
		httpx.Fail(c, httpx.Forbidden(reason))
		return
	}
	auth, err := m.authMethod(acc)
	if err != nil {
		logIn.Result, logIn.Reason = "failed", "托管凭据不可用"
		m.writeLoginLog(logIn)
		httpx.Fail(c, httpx.Conflict("SSH credential is unavailable or invalid"))
		return
	}
	filters, err := m.loadFilters(c, asset.ID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	masker, err := m.loadMasker(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if err := m.recordingReady(); err != nil {
		httpx.Fail(c, httpx.Conflict("shared durable recording storage is unavailable; SSH connection refused"))
		return
	}
	m.mu.Lock()
	unavailable := m.stopping || m.store == nil
	m.mu.Unlock()
	if unavailable {
		httpx.Fail(c, httpx.Conflict("bastion session owner is unavailable"))
		return
	}

	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return // upgrader already wrote the HTTP error
	}
	defer ws.Close()

	ref := sessionRef{Owner: m.owner, Fence: newToken()}
	createCtx, createCancel := context.WithTimeout(context.Background(), databaseTimeout)
	err = m.d.DB.QueryRow(createCtx, `INSERT INTO bastion_sessions
		(user_id,username,asset_id,asset_name,account_id,account_user,protocol,client_ip,
		 owner_id,fence_token,heartbeat_at,lease_expires_at,recording_state,recording_storage_id)
		VALUES ($1,$2,$3,$4,$5,$6,'ssh',$7,$8,$9,clock_timestamp(),
		 clock_timestamp()+make_interval(secs => $10),'unavailable',$11) RETURNING id`,
		p.UserID, p.Username, asset.ID, asset.Name, acc.ID, acc.Username, c.ClientIP(),
		ref.Owner, ref.Fence, leaseSeconds, m.storageID).Scan(&ref.ID)
	createCancel()
	if err != nil {
		_ = ws.WriteJSON(wsMsg{Type: "error", Data: "session audit could not be created"})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wmu sync.Mutex
	send := func(msg wsMsg) error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return ws.WriteJSON(msg)
	}
	end := sessionEnd{Status: "closed", RecordingState: "unavailable", StorageID: m.storageID}
	if !m.register(ref, cancel) {
		cancel()
		end.Status, end.Error = "failed", "owner is shutting down"
		finalCtx, finalCancel := context.WithTimeout(context.Background(), databaseTimeout)
		_ = m.store.finish(finalCtx, ref, end)
		finalCancel()
		return
	}
	watchDone := m.watchLease(ctx, ref)
	var rec *Recorder
	var raw net.Conn
	var sess *ssh.Session
	var streams sync.WaitGroup
	var input sync.WaitGroup
	var cancelTransport context.CancelFunc
	defer func() {
		// A kill acknowledgement is written only after all local transports,
		// readers, input delivery and the durable recording have finished.
		if raw != nil {
			_ = raw.Close()
		}
		_ = ws.Close()
		if sess != nil {
			_ = sess.Close()
		}
		if cancelTransport != nil {
			cancelTransport()
		}
		streams.Wait()
		input.Wait()
		if rec != nil {
			if err := rec.Close(); err != nil {
				end.RecordingState = "failed"
				end.Status, end.Error = "failed", "recording persistence failed"
			} else if end.RecordingState == "recording" {
				end.RecordingState = "ready"
			}
		}
		reason := m.sessionReason(ref.ID)
		switch reason {
		case "killed":
			end.Status = "killed"
		case "shutdown":
			end.Status, end.Error = "closed", "owner shut down"
		case "lease_lost":
			end.Status, end.Error = "failed", "session ownership or lease lost"
		}
		finalCtx, finalCancel := context.WithTimeout(context.Background(), databaseTimeout)
		if err := m.store.finish(finalCtx, ref, end); err != nil {
			m.d.Logger.Warn("bastion: session final audit not persisted", "session", ref.ID)
		}
		finalCancel()
		cancel()
		<-watchDone
		m.unregister(ref.ID)
	}()
	fail := func(message string) {
		end.Status, end.Error = "failed", message
		_ = send(wsMsg{Type: "error", Data: message})
	}
	_ = send(wsMsg{Type: "session", Data: itoa(ref.ID)})

	cols, rows := qInt(c, "cols", 120), qInt(c, "rows", 32)
	rec, end.RecordingPath, err = m.createRecording(ref, cols, rows, fmt.Sprintf("%s@%s by %s", acc.Username, asset.Name, p.Username))
	if err != nil {
		end.RecordingState = "failed"
		fail("durable recording could not be created")
		return
	}
	end.RecordingState = "recording"
	recordCtx, recordCancel := context.WithTimeout(ctx, databaseTimeout)
	tag, err := m.d.DB.Exec(recordCtx, `UPDATE bastion_sessions SET recording_path=$4,
		recording_state='recording' WHERE id=$1 AND owner_id=$2 AND fence_token=$3
		AND status='active' AND lease_expires_at>clock_timestamp() AND kill_requested_at IS NULL`,
		ref.ID, ref.Owner, ref.Fence, end.RecordingPath)
	recordCancel()
	if err != nil || tag.RowsAffected() != 1 {
		fail("recording audit could not be persisted")
		return
	}
	addr := net.JoinHostPort(asset.Host, strconv.Itoa(asset.Port))
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	raw, err = (&net.Dialer{}).DialContext(dialCtx, "tcp", addr)
	dialCancel()
	if err != nil {
		logIn.Result, logIn.Reason, logIn.SessionID = "failed", "网络连接失败", &ref.ID
		m.writeLoginLog(logIn)
		fail("SSH network connection failed")
		return
	}
	transportCtx, transportCancel := context.WithCancel(ctx)
	cancelTransport = transportCancel
	go func(conn net.Conn) {
		<-transportCtx.Done()
		_ = conn.Close()
		_ = ws.Close()
	}(raw)
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	conn, chans, reqs, err := ssh.NewClientConn(raw, addr, &ssh.ClientConfig{
		User: acc.Username, Auth: []ssh.AuthMethod{auth},
		HostKeyCallback: m.hostKeyCallback(asset), Timeout: 10 * time.Second,
	})
	if err != nil {
		logIn.Result, logIn.Reason, logIn.SessionID = "failed", "SSH 握手、认证或主机指纹校验失败", &ref.ID
		m.writeLoginLog(logIn)
		fail("SSH handshake, authentication or host key verification failed")
		return
	}
	logIn.Result, logIn.Reason, logIn.SessionID = "success", "", &ref.ID
	m.writeLoginLog(logIn)
	_ = raw.SetDeadline(time.Time{})
	client := ssh.NewClient(conn, chans, reqs)
	sess, err = client.NewSession()
	if err != nil {
		fail("SSH session creation failed")
		return
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		fail("SSH PTY request failed")
		return
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		fail("SSH stdin setup failed")
		return
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		fail("SSH stdout setup failed")
		return
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		fail("SSH stderr setup failed")
		return
	}
	if ctx.Err() != nil {
		return
	}
	if err := sess.Shell(); err != nil {
		fail("SSH shell request failed")
		return
	}

	var pmu sync.Mutex
	proc := NewInputProcessor(filters)
	type ioResult struct {
		kind string
		err  error
	}
	done := make(chan ioResult, 4)

	pump := func(r io.Reader) {
		defer streams.Done()
		buf := make([]byte, 16*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				pmu.Lock()
				proc.Output(buf[:n])
				pmu.Unlock()
				// 脱敏后再录像与下发：录像与浏览器看到的是同一份内容
				chunk := masker.Apply(append([]byte(nil), buf[:n]...))
				if err := rec.Output(chunk); err != nil {
					done <- ioResult{"recording", err}
					return
				}
				if send(wsMsg{Type: "output", Data: string(chunk)}) != nil {
					done <- ioResult{"client", io.EOF}
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) && ctx.Err() == nil {
					done <- ioResult{"output", err}
				}
				return
			}
		}
	}
	streams.Add(2)
	go pump(stdout)
	go pump(stderr)
	go func() {
		err := sess.Wait()
		streams.Wait()
		done <- ioResult{"remote", err}
	}()

	// deliver 把处理器结果写回客户端、审计并转发到远端；出错时返回结束原因。
	var deliverMu sync.Mutex
	deliver := func(toRemote, toClient []byte, events []CommandEvent) *ioResult {
		deliverMu.Lock()
		defer deliverMu.Unlock()
		if len(toClient) > 0 {
			if err := rec.Output(toClient); err != nil {
				return &ioResult{"recording", err}
			}
			_ = send(wsMsg{Type: "output", Data: string(toClient)})
		}
		ids, err := m.auditCommands(ctx, ref, events)
		if err != nil {
			return &ioResult{"audit", err}
		}
		if len(toRemote) > 0 {
			n, writeErr := stdin.Write(toRemote)
			if n != len(toRemote) && writeErr == nil {
				writeErr = io.ErrShortWrite
			}
			if err := m.commandDelivery(ctx, ref, ids, writeErr); err != nil {
				return &ioResult{"audit", err}
			}
			if writeErr != nil {
				return &ioResult{"input", writeErr}
			}
		}
		return nil
	}
	var approvalMu sync.Mutex
	var approvalTicket int64
	input.Add(1)
	go func() { // client -> remote
		defer input.Done()
		ws.SetReadLimit(1 << 20)
		for {
			var msg wsMsg
			if err := ws.ReadJSON(&msg); err != nil {
				done <- ioResult{"client", err}
				return
			}
			if ctx.Err() != nil {
				return
			}
			switch msg.Type {
			case "input":
				pmu.Lock()
				toRemote, toClient, events := proc.Input([]byte(msg.Data))
				req, cancelReq := proc.TakeApprovalRequest(), proc.TakeApprovalCancel()
				pmu.Unlock()
				if r := deliver(toRemote, toClient, events); r != nil {
					done <- *r
					return
				}
				if cancelReq {
					approvalMu.Lock()
					tid := approvalTicket
					approvalTicket = 0
					approvalMu.Unlock()
					if tid > 0 && m.d.Approval != nil {
						_, _ = m.d.Approval.Cancel(context.Background(), p, tid)
					}
					if b, err := json.Marshal(approvalNotice{State: "cancelled", TicketID: tid, Note: "已放弃，审批单已撤回"}); err == nil {
						_ = send(wsMsg{Type: "approval", Data: string(b)})
					}
				}
				if req != nil {
					go m.awaitCommandApproval(ctx, p, asset, ref.ID, req, &approvalMu, &approvalTicket, func(approved bool, note string) {
						pmu.Lock()
						tr, tc, ev := proc.ResolveApproval(approved, note)
						pmu.Unlock()
						if r := deliver(tr, tc, ev); r != nil {
							select {
							case done <- *r:
							default:
							}
						}
					}, func(text string) {
						_ = rec.Output([]byte(text))
						_ = send(wsMsg{Type: "output", Data: text})
					}, func(n approvalNotice) {
						if b, err := json.Marshal(n); err == nil {
							_ = send(wsMsg{Type: "approval", Data: string(b)})
						}
					})
				}
			case "resize":
				if msg.Cols > 0 && msg.Rows > 0 {
					if err := rec.Resize(msg.Cols, msg.Rows); err != nil {
						done <- ioResult{"recording", err}
						return
					}
					if err := sess.WindowChange(msg.Rows, msg.Cols); err != nil {
						done <- ioResult{"input", err}
						return
					}
				}
			}
		}
	}()

	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case result := <-done:
			if result.kind == "client" {
				if result.err != nil && !errors.Is(result.err, io.EOF) && !websocket.IsCloseError(result.err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					fail("client connection ended unexpectedly")
				}
			} else if result.err != nil {
				if result.kind == "recording" {
					end.RecordingState = "failed"
				}
				fail(result.kind + " failed; remote command outcome is not guaranteed")
			}
			_ = send(wsMsg{Type: "closed", Data: "session ended"})
			return
		case <-ping.C:
			wmu.Lock()
			err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			wmu.Unlock()
			if err != nil && !errors.Is(err, websocket.ErrCloseSent) {
				end.Status, end.Error = "failed", "client heartbeat failed"
				return
			}
		}
	}
}
