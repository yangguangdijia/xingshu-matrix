package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/hosts/proto"
)

func collect() (func(string, any), func() ([]proto.ExecOutput, proto.ExecResult)) {
	var mu sync.Mutex
	var outs []proto.ExecOutput
	var res proto.ExecResult
	return func(typ string, v any) {
			mu.Lock()
			defer mu.Unlock()
			switch x := v.(type) {
			case proto.ExecOutput:
				outs = append(outs, x)
			case proto.ExecResult:
				res = x
			}
		}, func() ([]proto.ExecOutput, proto.ExecResult) {
			mu.Lock()
			defer mu.Unlock()
			return outs, res
		}
}

func TestRunExec(t *testing.T) {
	a := &agent{sem: make(chan struct{}, 2)}
	out, get := collect()
	command, waitCommand := "echo hello; echo err >&2; exit 3", "sleep 10"
	if runtime.GOOS == "windows" {
		command, waitCommand = "echo hello & echo err 1>&2 & exit /b 3", "ping -n 11 127.0.0.1 >NUL"
	}
	a.runExec(context.Background(), proto.Exec{TaskID: 1, Command: command, TimeoutSec: 5}, out)
	outs, res := get()
	var all strings.Builder
	for _, o := range outs {
		all.WriteString(o.Data)
	}
	if res.ExitCode != 3 || !strings.Contains(all.String(), "hello") || !strings.Contains(all.String(), "err") {
		t.Fatalf("res=%+v out=%q", res, all.String())
	}
	out, get = collect()
	a.runExec(context.Background(), proto.Exec{TaskID: 2, Command: waitCommand, TimeoutSec: 1}, out)
	if _, res := get(); !res.TimedOut {
		t.Fatalf("expected timeout: %+v", res)
	}
}

func TestWriteFileAndFacts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "b.txt")
	n, err := writeFile(proto.FilePush{Path: p, ContentBase64: base64.StdEncoding.EncodeToString([]byte("abc")), Mode: 0o600})
	if err != nil || n != 3 {
		t.Fatal(n, err)
	}
	st, _ := os.Stat(p)
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	if _, err := writeFile(proto.FilePush{Path: "rel/x"}); err == nil {
		t.Fatal("relative path accepted")
	}
	f := collectFacts()
	if f.Hostname == "" || f.CPUCores == 0 || f.MemTotal == 0 {
		t.Fatalf("facts %+v", f)
	}
	if wsURL("https://x.io") != "wss://x.io"+proto.WSPath {
		t.Fatal("wsURL")
	}
}
