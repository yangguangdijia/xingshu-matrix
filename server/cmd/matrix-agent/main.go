// Command matrix-agent runs on managed hosts and connects to matrix-server
// over WebSocket (JSON frames, see internal/modules/hosts/proto).
//
//	matrix-agent -server https://matrix.example.com -token mxe_xxx     # first run: enrol
//	matrix-agent -server https://matrix.example.com                    # later runs use the saved secret
package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/hosts/proto"
)

var version = "0.1.0"

type state struct {
	Server string `json:"server"`
	HostID int64  `json:"host_id"`
	Secret string `json:"secret"`
}

type agent struct {
	server    string
	token     string
	statePath string
	dialer    *websocket.Dialer
	st        state
	log       *slog.Logger
	sem       chan struct{}
}

func main() {
	server := flag.String("server", os.Getenv("MATRIX_SERVER"), "matrix-server base URL, e.g. https://matrix.example.com")
	token := flag.String("token", os.Getenv("MATRIX_ENROLL_TOKEN"), "one-time enrolment token (first run only)")
	statePath := flag.String("state", "/var/lib/matrix-agent/state.json", "where the host id and secret are stored")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification")
	maxConc := flag.Int("max-concurrency", 8, "max concurrent commands")
	showVer := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *server == "" {
		log.Error("-server is required")
		os.Exit(2)
	}
	a := &agent{
		server: strings.TrimRight(*server, "/"), token: *token, statePath: *statePath, log: log,
		dialer: &websocket.Dialer{HandshakeTimeout: 15 * time.Second, Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: *insecure}}, //nolint:gosec // opt-in flag
		sem: make(chan struct{}, max(1, *maxConc)),
	}
	if b, err := os.ReadFile(a.statePath); err == nil {
		_ = json.Unmarshal(b, &a.st)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a.run(ctx)
}

func wsURL(server string) string {
	u := server
	switch {
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u + proto.WSPath
}

// run loops forever with exponential backoff (1s → 60s, jittered).
func (a *agent) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		var err error
		if a.st.Secret == "" || a.st.Server != a.server {
			if a.token == "" {
				a.log.Error("not enrolled: pass -token")
				os.Exit(2)
			}
			err = a.enroll(ctx)
			if err == nil {
				backoff = time.Second
				continue // reconnect immediately with the secret
			}
		} else {
			err = a.session(ctx)
		}
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		wait := backoff/2 + time.Duration(rand.Int64N(int64(backoff)))
		a.log.Warn("disconnected, retrying", "err", err, "in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (a *agent) dial(ctx context.Context, url string, hdr http.Header) (*websocket.Conn, error) {
	ws, resp, err := a.dialer.DialContext(ctx, url, hdr)
	if err != nil && resp != nil {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%w: HTTP %d %s", err, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return ws, err
}

func (a *agent) hello(ws *websocket.Conn) error {
	b, _ := proto.Encode(proto.TypeHello, proto.Hello{AgentVersion: version, Facts: collectFacts()})
	_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return ws.WriteMessage(websocket.TextMessage, b)
}

func readWelcome(ws *websocket.Conn) (*proto.Welcome, error) {
	_ = ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	var env proto.Envelope
	if err := ws.ReadJSON(&env); err != nil {
		return nil, err
	}
	if env.Type == proto.TypeError {
		var e proto.Error
		_ = json.Unmarshal(env.Data, &e)
		return nil, errors.New("server: " + e.Message)
	}
	var w proto.Welcome
	if env.Type != proto.TypeWelcome || json.Unmarshal(env.Data, &w) != nil {
		return nil, fmt.Errorf("unexpected frame %q", env.Type)
	}
	return &w, nil
}

func (a *agent) enroll(ctx context.Context) error {
	ws, err := a.dial(ctx, wsURL(a.server)+"?token="+a.token, nil)
	if err != nil {
		return err
	}
	defer ws.Close()
	if err := a.hello(ws); err != nil {
		return err
	}
	w, err := readWelcome(ws)
	if err != nil {
		return err
	}
	if w.Secret == "" {
		return errors.New("server did not issue a secret")
	}
	a.st = state{Server: a.server, HostID: w.HostID, Secret: w.Secret}
	b, _ := json.MarshalIndent(a.st, "", "  ")
	if err := os.MkdirAll(filepath.Dir(a.statePath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(a.statePath, b, 0o600); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	a.log.Info("enrolled", "host_id", w.HostID, "state", a.statePath)
	return nil
}

// session is one authenticated connection: heartbeats + command handling.
func (a *agent) session(ctx context.Context) error {
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+a.st.Secret)
	hdr.Set(proto.HeaderHostID, strconv.FormatInt(a.st.HostID, 10))
	ws, err := a.dial(ctx, wsURL(a.server), hdr)
	if err != nil {
		return err
	}
	defer ws.Close()
	if err := a.hello(ws); err != nil {
		return err
	}
	if _, err := readWelcome(ws); err != nil {
		return err
	}
	a.log.Info("connected", "host_id", a.st.HostID)

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	send := make(chan []byte, 256)
	out := func(typ string, v any) {
		b, _ := proto.Encode(typ, v)
		select {
		case send <- b:
		case <-sctx.Done():
		}
	}
	var wg sync.WaitGroup
	errc := make(chan error, 2)
	go func() { // writer
		hb := time.NewTicker(30 * time.Second)
		defer hb.Stop()
		for {
			select {
			case <-sctx.Done():
				_ = ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			case b := <-send:
				_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := ws.WriteMessage(websocket.TextMessage, b); err != nil {
					errc <- err
					return
				}
			case <-hb.C:
				f := collectFacts()
				b, _ := proto.Encode(proto.TypeHeartbeat, proto.Heartbeat{Facts: &f})
				_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := ws.WriteMessage(websocket.TextMessage, b); err != nil {
					errc <- err
					return
				}
			}
		}
	}()
	go func() { // reader
		ws.SetPingHandler(func(s string) error {
			_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))
			return ws.WriteControl(websocket.PongMessage, []byte(s), time.Now().Add(5*time.Second))
		})
		for {
			_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))
			var env proto.Envelope
			if err := ws.ReadJSON(&env); err != nil {
				errc <- err
				return
			}
			switch env.Type {
			case proto.TypeExec:
				var e proto.Exec
				if json.Unmarshal(env.Data, &e) == nil {
					wg.Add(1)
					go func() { defer wg.Done(); a.runExec(sctx, e, out) }()
				}
			case proto.TypeFilePush:
				var f proto.FilePush
				if json.Unmarshal(env.Data, &f) == nil {
					n, err := writeFile(f)
					r := proto.FileResult{TaskID: f.TaskID, Bytes: n}
					if err != nil {
						r.Error = err.Error()
					}
					out(proto.TypeFileResult, r)
				}
			case proto.TypeError:
				var e proto.Error
				_ = json.Unmarshal(env.Data, &e)
				a.log.Warn("server error", "msg", e.Message)
			}
		}
	}()
	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	cancel()
	_ = ws.Close()
	wg.Wait()
	return err
}

type chunkWriter struct {
	mu     *sync.Mutex
	taskID int64
	stream string
	total  *int
	out    func(string, any)
}

func (w chunkWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	room := proto.MaxOutput - *w.total
	n := len(p)
	if room <= 0 {
		w.mu.Unlock()
		return n, nil // discard beyond cap, keep the process running
	}
	if n > room {
		p = p[:room]
	}
	*w.total += len(p)
	w.mu.Unlock()
	w.out(proto.TypeExecOutput, proto.ExecOutput{TaskID: w.taskID, Stream: w.stream, Data: strings.ToValidUTF8(string(p), "\uFFFD")})
	return n, nil
}

func (a *agent) runExec(ctx context.Context, e proto.Exec, out func(string, any)) {
	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
	case <-ctx.Done():
		return
	}
	timeout := time.Duration(max(1, e.TimeoutSec)) * time.Second
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	cmd := shellCommand(cctx, e.Command)
	configureProcessCommand(cmd)
	cmd.WaitDelay = 5 * time.Second
	var mu sync.Mutex
	total := 0
	cmd.Stdout = chunkWriter{&mu, e.TaskID, "stdout", &total, out}
	cmd.Stderr = chunkWriter{&mu, e.TaskID, "stderr", &total, out}
	err := cmd.Run()
	res := proto.ExecResult{TaskID: e.TaskID, DurationMs: time.Since(start).Milliseconds()}
	var ee *exec.ExitError
	switch {
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		res.TimedOut, res.ExitCode, res.Error = true, -1, fmt.Sprintf("timed out after %s", timeout)
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	case err != nil:
		res.ExitCode, res.Error = -1, err.Error()
	}
	if total >= proto.MaxOutput {
		out(proto.TypeExecOutput, proto.ExecOutput{TaskID: e.TaskID, Stream: "stderr", Data: "\n[matrix-agent: output truncated]\n"})
	}
	out(proto.TypeExecResult, res)
}

func writeFile(f proto.FilePush) (int, error) {
	if !filepath.IsAbs(f.Path) || filepath.Clean(f.Path) != f.Path {
		return 0, errors.New("path must be absolute and clean")
	}
	data, err := base64.StdEncoding.DecodeString(f.ContentBase64)
	if err != nil {
		return 0, err
	}
	if len(data) > proto.MaxFile {
		return 0, errors.New("file too large")
	}
	mode := os.FileMode(f.Mode)
	if mode == 0 {
		mode = 0o644
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), ".matrix-push-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	return len(data), os.Rename(tmp.Name(), f.Path)
}

var skipFS = map[string]bool{"tmpfs": true, "devtmpfs": true, "overlay": true, "squashfs": true, "proc": true, "sysfs": true, "cgroup": true, "cgroup2": true}

func collectFacts() proto.Facts {
	f := proto.Facts{Arch: runtime.GOARCH, OS: runtime.GOOS}
	if hi, err := host.Info(); err == nil {
		f.Hostname, f.OS, f.Platform, f.PlatformVersion = hi.Hostname, hi.OS, hi.Platform, hi.PlatformVersion
		f.Kernel, f.Uptime, f.BootTime = hi.KernelVersion, hi.Uptime, hi.BootTime
		if hi.KernelArch != "" {
			f.Arch = hi.KernelArch
		}
	}
	if ci, err := cpu.Info(); err == nil && len(ci) > 0 {
		f.CPUModel = ci[0].ModelName
	}
	if n, err := cpu.Counts(true); err == nil {
		f.CPUCores = n
	}
	if p, err := cpu.Percent(200*time.Millisecond, false); err == nil && len(p) > 0 {
		f.CPUPercent = round(p[0])
	}
	if l, err := load.Avg(); err == nil {
		f.Load1 = l.Load1
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		f.MemTotal, f.MemUsed, f.MemPercent = vm.Total, vm.Used, round(vm.UsedPercent)
	}
	if parts, err := disk.Partitions(false); err == nil {
		seen := map[string]bool{}
		for _, p := range parts {
			if skipFS[p.Fstype] || seen[p.Device] || len(f.Disks) >= 16 {
				continue
			}
			if u, err := disk.Usage(p.Mountpoint); err == nil && u.Total > 0 {
				seen[p.Device] = true
				f.Disks = append(f.Disks, proto.Disk{Mount: p.Mountpoint, FSType: p.Fstype, Total: u.Total, Used: u.Used, Percent: round(u.UsedPercent)})
			}
		}
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, ad := range addrs {
			if ipn, ok := ad.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
				f.IPs = append(f.IPs, ipn.IP.String())
			}
		}
	}
	return f
}

func round(v float64) float64 { return float64(int(v*10+0.5)) / 10 }
