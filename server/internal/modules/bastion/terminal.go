package bastion

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// FilterRule is a compiled command filter rule.
type FilterRule struct {
	ID     int64
	Name   string
	Re     *regexp.Regexp
	Action string // deny | confirm
}

// Evaluate returns the first matching rule (rules are pre-sorted by priority) or nil.
func Evaluate(rules []FilterRule, cmd string) *FilterRule {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return nil
	}
	for i := range rules {
		if rules[i].Re.MatchString(cmd) {
			return &rules[i]
		}
	}
	return nil
}

// CommandEvent is one submitted command line to audit.
type CommandEvent struct {
	Command string
	Action  string // allow | deny | confirmed | cancelled
	RuleID  int64
}

// InputProcessor line-buffers terminal keystrokes and applies filter rules
// before the Enter key is forwarded to the remote shell.
//
// Keystrokes are forwarded immediately (so the remote echo / readline keep
// working); on Enter the buffered line is checked. A denied line is erased on
// the remote with Ctrl-U instead of being submitted. When the line was edited
// with history/arrow keys or Tab completion the local buffer is unreliable, so
// the processor falls back to the last echoed screen line (ANSI stripped).
type InputProcessor struct {
	rules   []FilterRule
	buf     []byte
	dirty   bool
	screen  []byte // current output line as echoed by the remote
	pending *CommandEvent
	local   []byte // answer typed while waiting for confirmation
}

func NewInputProcessor(rules []FilterRule) *InputProcessor { return &InputProcessor{rules: rules} }

const (
	colorRed    = "\x1b[31m"
	colorYellow = "\x1b[33m"
	colorReset  = "\x1b[0m"
)

// Input processes client keystrokes. It returns bytes for the remote, bytes to
// write back to the client terminal, and commands to audit.
func (p *InputProcessor) Input(data []byte) (toRemote, toClient []byte, events []CommandEvent) {
	for i := 0; i < len(data); i++ {
		b := data[i]
		if p.pending != nil {
			switch b {
			case '\r', '\n':
				ans := strings.ToLower(strings.TrimSpace(string(p.local)))
				ev := *p.pending
				p.pending, p.local = nil, nil
				if ans == "y" || ans == "yes" {
					ev.Action = "confirmed"
					toClient = append(toClient, "\r\n"...)
					toRemote = append(toRemote, ev.Command...)
					toRemote = append(toRemote, '\r')
				} else {
					ev.Action = "cancelled"
					toClient = append(toClient, "\r\n"+colorYellow+"[bastion] 已取消"+colorReset+"\r\n"...)
					toRemote = append(toRemote, '\r')
				}
				events = append(events, ev)
			case 0x7f, 0x08:
				if n := len(p.local); n > 0 {
					_, sz := utf8.DecodeLastRune(p.local)
					p.local = p.local[:n-sz]
					toClient = append(toClient, "\b \b"...)
				}
			case 0x03:
				ev := *p.pending
				ev.Action = "cancelled"
				events = append(events, ev)
				p.pending, p.local = nil, nil
				toClient = append(toClient, "^C\r\n"...)
				toRemote = append(toRemote, '\r')
			default:
				if b >= 0x20 {
					p.local = append(p.local, b)
					toClient = append(toClient, b)
				}
			}
			continue
		}
		switch b {
		case '\r', '\n':
			line := string(p.buf)
			wasDirty := p.dirty
			if wasDirty {
				line = string(p.screen)
			}
			p.buf, p.dirty = p.buf[:0], false
			cmd := strings.TrimSpace(line)
			if cmd == "" {
				toRemote = append(toRemote, b)
				continue
			}
			r := Evaluate(p.rules, cmd)
			switch {
			case r == nil:
				events = append(events, CommandEvent{Command: cmd, Action: "allow"})
				toRemote = append(toRemote, b)
			case r.Action == "confirm":
				// Erase the line remotely; resubmit the exact buffered text if confirmed.
				toRemote = append(toRemote, 0x15)
				// For an edited (dirty) line the screen fallback includes the
				// shell prompt, so resubmission is only safe for clean lines.
				if !wasDirty {
					p.pending = &CommandEvent{Command: cmd, RuleID: r.ID}
				} else {
					events = append(events, CommandEvent{Command: cmd, Action: "deny", RuleID: r.ID})
					toRemote = append(toRemote, '\r')
					toClient = append(toClient, fmt.Sprintf("\r\n%s[bastion] 命令需确认（规则「%s」），但该行经历史/补全编辑无法安全重放，已拦截，请完整重新输入%s\r\n", colorRed, r.Name, colorReset)...)
					continue
				}
				toClient = append(toClient, fmt.Sprintf("\r\n%s[bastion] 命令命中规则「%s」，需要确认。执行? (yes/no): %s", colorYellow, r.Name, colorReset)...)
			default:
				events = append(events, CommandEvent{Command: cmd, Action: "deny", RuleID: r.ID})
				toRemote = append(toRemote, 0x15, '\r')
				toClient = append(toClient, fmt.Sprintf("\r\n%s[bastion] 命令被拒绝（规则「%s」）: %s%s\r\n", colorRed, r.Name, cmd, colorReset)...)
			}
		case 0x7f, 0x08:
			if n := len(p.buf); n > 0 {
				_, sz := utf8.DecodeLastRune(p.buf)
				p.buf = p.buf[:n-sz]
			}
			toRemote = append(toRemote, b)
		case 0x03, 0x15, 0x04:
			p.buf, p.dirty = p.buf[:0], false
			toRemote = append(toRemote, b)
		case 0x1b: // escape sequence (arrows, history, Home/End)
			j := i + 1
			if j < len(data) && (data[j] == '[' || data[j] == 'O') {
				j++
				for j < len(data) && !(data[j] >= 0x40 && data[j] <= 0x7e) {
					j++
				}
			}
			if j >= len(data) {
				j = len(data) - 1
			}
			toRemote = append(toRemote, data[i:j+1]...)
			p.dirty = true
			i = j
		case '\t':
			p.dirty = true
			toRemote = append(toRemote, b)
		default:
			if b >= 0x20 || b >= 0x80 {
				p.buf = append(p.buf, b)
			} else {
				p.dirty = true // other control keys (Ctrl-R, Ctrl-A, ...)
			}
			toRemote = append(toRemote, b)
		}
	}
	return
}

var ansiRe = regexp.MustCompile(`\x1b(\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[()][0-9A-Za-z]|[=>78DEHM])`)

// Output observes remote output to maintain the current screen line.
func (p *InputProcessor) Output(data []byte) {
	clean := ansiRe.ReplaceAll(data, nil)
	for _, b := range clean {
		switch b {
		case '\n':
			p.screen = p.screen[:0]
		case '\r':
			// readline redraws with \r + prompt + line; keep only the redraw.
			p.screen = p.screen[:0]
		case 0x08:
			if n := len(p.screen); n > 0 {
				_, sz := utf8.DecodeLastRune(p.screen)
				p.screen = p.screen[:n-sz]
			}
		default:
			if b >= 0x20 {
				p.screen = append(p.screen, b)
			}
		}
	}
	if len(p.screen) > 4096 {
		p.screen = p.screen[len(p.screen)-4096:]
	}
}

// Recorder writes an asciicast v2 file (https://docs.asciinema.org/manual/asciicast/v2/).
type Recorder struct {
	mu    sync.Mutex
	f     *os.File
	start time.Time
}

func NewRecorder(path string, width, height int, title string) (*Recorder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	hdr, _ := json.Marshal(map[string]any{
		"version": 2, "width": width, "height": height, "timestamp": now.Unix(), "title": title,
		"env": map[string]string{"TERM": "xterm-256color", "SHELL": "/bin/bash"},
	})
	if _, err := f.Write(append(hdr, '\n')); err != nil {
		f.Close()
		return nil, err
	}
	return &Recorder{f: f, start: now}, nil
}

func (r *Recorder) event(kind, data string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return
	}
	line, _ := json.Marshal([]any{float64(time.Since(r.start).Microseconds()) / 1e6, kind, data})
	_, _ = r.f.Write(append(line, '\n'))
}

func (r *Recorder) Output(b []byte)       { r.event("o", string(b)) }
func (r *Recorder) Resize(cols, rows int) { r.event("r", fmt.Sprintf("%dx%d", cols, rows)) }

func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
