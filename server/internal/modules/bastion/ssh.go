package bastion

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
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
			_, err := m.d.DB.Exec(context.Background(), `UPDATE bastion_assets SET host_key=$2 WHERE id=$1 AND host_key=''`, asset.ID, k)
			return err
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
	if okk, err := m.authorized(c, p, assetID, accountID); err != nil {
		httpx.Fail(c, err)
		return
	} else if !okk {
		httpx.Fail(c, httpx.Forbidden("no active authorization rule for this asset/account"))
		return
	}
	auth, err := m.authMethod(acc)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	filters, err := m.loadFilters(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}

	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return // upgrader already wrote the HTTP error
	}
	defer ws.Close()

	var sessID int64
	err = m.d.DB.QueryRow(context.Background(), `INSERT INTO bastion_sessions
		(user_id,username,asset_id,asset_name,account_id,account_user,protocol,client_ip)
		VALUES ($1,$2,$3,$4,$5,$6,'ssh',$7) RETURNING id`,
		p.UserID, p.Username, asset.ID, asset.Name, acc.ID, acc.Username, c.ClientIP()).Scan(&sessID)
	if err != nil {
		_ = ws.WriteJSON(wsMsg{Type: "error", Data: "create session: " + err.Error()})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.register(sessID, cancel)

	status, errMsg, recPath := "closed", "", ""
	defer func() {
		if m.unregister(sessID) {
			status = "killed"
		}
		_, _ = m.d.DB.Exec(context.Background(), `UPDATE bastion_sessions SET status=$2,error=$3,recording_path=$4,ended_at=now() WHERE id=$1`,
			sessID, status, errMsg, recPath)
	}()

	var wmu sync.Mutex
	send := func(msg wsMsg) error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return ws.WriteJSON(msg)
	}
	fail := func(e error) {
		status, errMsg = "failed", e.Error()
		_ = send(wsMsg{Type: "output", Data: "\r\n\x1b[31m[bastion] " + e.Error() + "\x1b[0m\r\n"})
		_ = send(wsMsg{Type: "closed", Data: e.Error()})
	}

	cols, rows := qInt(c, "cols", 120), qInt(c, "rows", 32)
	addr := net.JoinHostPort(asset.Host, strconv.Itoa(asset.Port))
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User: acc.Username, Auth: []ssh.AuthMethod{auth},
		HostKeyCallback: m.hostKeyCallback(asset), Timeout: 10 * time.Second,
	})
	if err != nil {
		fail(fmt.Errorf("ssh dial %s: %w", addr, err))
		return
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		fail(err)
		return
	}
	defer sess.Close()
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		fail(err)
		return
	}
	stdin, _ := sess.StdinPipe()
	stdout, _ := sess.StdoutPipe()
	stderr, _ := sess.StderrPipe()
	if err := sess.Shell(); err != nil {
		fail(err)
		return
	}

	name := fmt.Sprintf("%d-%s.cast", sessID, time.Now().Format("20060102T150405"))
	rec, err := NewRecorder(filepath.Join(m.recordDir, name), cols, rows, fmt.Sprintf("%s@%s by %s", acc.Username, asset.Name, p.Username))
	if err != nil {
		m.d.Logger.Warn("bastion: recording disabled", "session", sessID, "err", err)
	} else {
		recPath = name
		defer rec.Close()
	}

	var pmu sync.Mutex
	proc := NewInputProcessor(filters)
	done := make(chan struct{}, 3)

	pump := func(r io.Reader) {
		buf := make([]byte, 16*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				pmu.Lock()
				proc.Output(chunk)
				pmu.Unlock()
				rec.Output(chunk)
				if send(wsMsg{Type: "output", Data: string(chunk)}) != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}
	go pump(stdout)
	go pump(stderr)

	go func() { // client -> remote
		defer func() { done <- struct{}{} }()
		ws.SetReadLimit(1 << 20)
		for {
			var msg wsMsg
			if err := ws.ReadJSON(&msg); err != nil {
				return
			}
			switch msg.Type {
			case "input":
				pmu.Lock()
				toRemote, toClient, events := proc.Input([]byte(msg.Data))
				pmu.Unlock()
				if len(toClient) > 0 {
					rec.Output(toClient)
					_ = send(wsMsg{Type: "output", Data: string(toClient)})
				}
				for _, ev := range events {
					var rule any
					if ev.RuleID > 0 {
						rule = ev.RuleID
					}
					_, _ = m.d.DB.Exec(context.Background(), `INSERT INTO bastion_commands (session_id,command,action,rule_id) VALUES ($1,$2,$3,$4)`,
						sessID, ev.Command, ev.Action, rule)
				}
				if len(toRemote) > 0 {
					if _, err := stdin.Write(toRemote); err != nil {
						return
					}
				}
			case "resize":
				if msg.Cols > 0 && msg.Rows > 0 {
					_ = sess.WindowChange(msg.Rows, msg.Cols)
					rec.Resize(msg.Cols, msg.Rows)
				}
			}
		}
	}()

	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = send(wsMsg{Type: "output", Data: "\r\n\x1b[31m[bastion] 会话已被管理员强制断开\x1b[0m\r\n"})
			_ = send(wsMsg{Type: "closed", Data: "killed"})
			return
		case <-done:
			_ = send(wsMsg{Type: "closed", Data: "session ended"})
			return
		case <-ping.C:
			wmu.Lock()
			err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			wmu.Unlock()
			if err != nil && !errors.Is(err, websocket.ErrCloseSent) {
				return
			}
		}
	}
}
