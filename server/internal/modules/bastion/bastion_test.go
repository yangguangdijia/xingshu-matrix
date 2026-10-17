package bastion

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
)

func rules() []FilterRule {
	return []FilterRule{
		{ID: 1, Name: "rm-root", Re: regexp.MustCompile(`rm\s+-rf\s+/`), Action: "deny"},
		{ID: 2, Name: "reboot", Re: regexp.MustCompile(`^(reboot|shutdown)\b`), Action: "confirm"},
	}
}

func TestDenyCommand(t *testing.T) {
	p := NewInputProcessor(rules())
	remote, client, ev := p.Input([]byte("ls -l\rrm -rf /tmp\r"))
	if len(ev) != 2 || ev[0].Action != "allow" || ev[1].Action != "deny" || ev[1].RuleID != 1 {
		t.Fatalf("events = %+v", ev)
	}
	if !strings.HasSuffix(string(remote), "rm -rf /tmp\x15\r") {
		t.Fatalf("remote = %q", remote)
	}
	if !strings.Contains(string(client), "命令被拒绝") {
		t.Fatalf("client = %q", client)
	}
}

func TestBackspaceAndConfirm(t *testing.T) {
	p := NewInputProcessor(rules())
	_, _, ev := p.Input([]byte("rebootx\x7f\r"))
	if len(ev) != 0 || p.pending == nil {
		t.Fatalf("expected pending confirm, ev=%+v", ev)
	}
	remote, _, ev := p.Input([]byte("yes\r"))
	if len(ev) != 1 || ev[0].Action != "confirmed" || string(remote) != "reboot\r" {
		t.Fatalf("ev=%+v remote=%q", ev, remote)
	}
	p.Input([]byte("shutdown now\r"))
	remote, _, ev = p.Input([]byte("n\r"))
	if ev[0].Action != "cancelled" || string(remote) != "\r" {
		t.Fatalf("ev=%+v remote=%q", ev, remote)
	}
}

func TestHistoryFallsBackToScreen(t *testing.T) {
	p := NewInputProcessor(rules())
	p.Input([]byte("\x1b[A")) // up arrow recalls history
	p.Output([]byte("\r\x1b[Kuser@h:~$ rm -rf /var"))
	_, _, ev := p.Input([]byte("\r"))
	if len(ev) != 1 || ev[0].Action != "deny" {
		t.Fatalf("ev=%+v", ev)
	}
}

func TestRecorderAsciicast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.cast")
	r, err := NewRecorder(path, 80, 24, "t")
	if err != nil {
		t.Fatal(err)
	}
	r.Output([]byte("hello\r\n"))
	r.Resize(100, 30)
	r.Close()
	f, _ := os.Open(path)
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan()
	var hdr map[string]any
	if err := json.Unmarshal(sc.Bytes(), &hdr); err != nil || hdr["version"].(float64) != 2 || hdr["width"].(float64) != 80 {
		t.Fatalf("hdr=%v err=%v", hdr, err)
	}
	sc.Scan()
	var ev []any
	_ = json.Unmarshal(sc.Bytes(), &ev)
	if ev[1] != "o" || ev[2] != "hello\r\n" {
		t.Fatalf("event=%v", ev)
	}
}

func TestRoutesAndGuacdHealth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	m := New()
	m.guacdAddr = ln.Addr().String()
	e := gin.New()
	m.Register(e.Group("/api/v1"), &app.Deps{}) // must not panic (static + :id routes)
	h := gin.New()
	h.GET("/h", m.guacdHealth)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/h", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"reachable":true`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
