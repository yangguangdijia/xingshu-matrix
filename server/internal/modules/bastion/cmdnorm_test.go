package bastion

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitCommandSegments(t *testing.T) {
	cases := []struct {
		name    string
		cmd     string
		want    []string // 非 nil 时要求完全相等（顺序也一致）
		contain []string // 必须包含的段
		absent  []string // 不能出现的段
	}{
		{name: "empty", cmd: "   ", want: nil},
		{name: "plain", cmd: "ls -l", want: []string{"ls -l"}},
		{name: "collapse spaces", cmd: "  rm   -rf\t / ", want: []string{"rm -rf /"}},
		{name: "semicolon", cmd: "ls; rm -rf /", want: []string{"ls", "rm -rf /"}},
		{name: "and no spaces", cmd: "ls&&rm -rf /", want: []string{"ls", "rm -rf /"}},
		{name: "or", cmd: "false || rm -rf /", want: []string{"false", "rm -rf /"}},
		{name: "pipe", cmd: "yes|rm -rf /", want: []string{"yes", "rm -rf /"}},
		{name: "background", cmd: "sleep 1 & rm -rf /", want: []string{"sleep 1", "rm -rf /"}},
		{name: "newline", cmd: "ls\nrm -rf /", want: []string{"ls", "rm -rf /"}},
		{name: "crlf", cmd: "ls\r\nrm -rf /", want: []string{"ls", "rm -rf /"}},
		{name: "line continuation", cmd: "r\\\nm -rf /", want: []string{"rm -rf /"}},
		{name: "line continuation between args", cmd: "rm \\\n  -rf \\\n /", want: []string{"rm -rf /"}},
		{name: "dollar paren", cmd: "echo $(rm -rf /)", want: []string{"echo $(rm -rf /)", "rm -rf /"}},
		{name: "dollar paren in double quotes", cmd: `echo "$(rm -rf /)"`, want: []string{`echo "$(rm -rf /)"`, "rm -rf /"}},
		{name: "nested substitution", cmd: "echo $(echo $(rm -rf /))", contain: []string{"echo $(rm -rf /)", "rm -rf /"}},
		{name: "backtick", cmd: "echo `rm -rf /`", want: []string{"echo `rm -rf /`", "rm -rf /"}},
		{name: "backtick with separator inside", cmd: "echo `ls; rm -rf /`", contain: []string{"ls", "rm -rf /"}},
		{name: "process substitution", cmd: "cat <(rm -rf /)", contain: []string{"rm -rf /"}},
		{name: "sudo", cmd: "sudo rm -rf /", want: []string{"sudo rm -rf /", "rm -rf /"}},
		{name: "sudo with user", cmd: "sudo -u root rm -rf /", want: []string{"sudo -u root rm -rf /", "rm -rf /"}},
		{name: "env assignments", cmd: "env FOO=1 BAR=2 rm -rf /", want: []string{"env FOO=1 BAR=2 rm -rf /", "rm -rf /"}},
		{name: "bare assignment", cmd: "FOO=1 rm -rf /", want: []string{"FOO=1 rm -rf /", "rm -rf /"}},
		{name: "nohup and time", cmd: "nohup time rm -rf / &", want: []string{"nohup time rm -rf /", "rm -rf /"}},
		{name: "absolute path", cmd: "/bin/rm -rf /", want: []string{"/bin/rm -rf /", "rm -rf /"}},
		{name: "sudo plus path", cmd: "sudo /usr/bin/rm -rf /", want: []string{"sudo /usr/bin/rm -rf /", "/usr/bin/rm -rf /", "rm -rf /"}},
		{name: "path to sudo", cmd: "/usr/bin/sudo rm -rf /", contain: []string{"rm -rf /"}},
		{name: "quoted command name", cmd: `"rm" -rf /`, want: []string{`"rm" -rf /`, "rm -rf /"}},
		{name: "backslash alias bypass", cmd: `\rm -rf /`, want: []string{`\rm -rf /`, "rm -rf /"}},
		{name: "subshell", cmd: "(rm -rf /)", want: []string{"rm -rf /"}},
		{name: "brace group", cmd: "{ rm -rf /; }", want: []string{"{ rm -rf /", "rm -rf /", "}"}},
		{name: "if then", cmd: "if true; then rm -rf /; fi", contain: []string{"rm -rf /"}},
		{name: "quoted semicolon not split", cmd: `echo "a;b"`, want: []string{`echo "a;b"`}},
		{name: "single quoted operators not split", cmd: `echo 'a && b | c $(d)'`, want: []string{`echo 'a && b | c $(d)'`}},
		{name: "spaces inside quotes kept", cmd: `echo "a   b"`, want: []string{`echo "a   b"`}},
		{name: "redirect not split", cmd: "ls >/dev/null 2>&1", want: []string{"ls >/dev/null 2>&1"}},
		{name: "amp redirect not split", cmd: "ls &>/dev/null", want: []string{"ls &>/dev/null"}},
		{name: "dedupe", cmd: "rm -rf /; rm -rf /", want: []string{"rm -rf /"}},
		{name: "escaped semicolon not split", cmd: `find . -exec rm {} \;`, want: []string{`find . -exec rm {} \;`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitCommandSegments(tc.cmd)
			if tc.want != nil || (tc.contain == nil && tc.absent == nil) {
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("splitCommandSegments(%q)\n got: %q\nwant: %q", tc.cmd, got, tc.want)
				}
			}
			for _, w := range tc.contain {
				if !cmdSegContains(got, w) {
					t.Errorf("splitCommandSegments(%q) = %q, missing %q", tc.cmd, got, w)
				}
			}
			for _, a := range tc.absent {
				if cmdSegContains(got, a) {
					t.Errorf("splitCommandSegments(%q) = %q, unexpected %q", tc.cmd, got, a)
				}
			}
		})
	}
}

func TestSplitCommandSegmentsLimits(t *testing.T) {
	long := "echo " + strings.Repeat("a", 10000)
	got := splitCommandSegments(long)
	if len(got) != 1 || len(got[0]) != cmdSegmentMaxLen {
		t.Fatalf("long segment not truncated: n=%d", len(got))
	}

	var parts []string
	for i := 0; i < 200; i++ {
		parts = append(parts, "cmd"+strings.Repeat("x", i%50)+string(rune('a'+i%26)))
	}
	got = splitCommandSegments(strings.Join(parts, ";"))
	if len(got) > cmdSegmentMaxCount {
		t.Fatalf("too many segments: %d", len(got))
	}

	deep := strings.Repeat("$(", 100) + "rm -rf /" + strings.Repeat(")", 100)
	if got := splitCommandSegments(deep); len(got) == 0 || len(got) > cmdSegmentMaxCount {
		t.Fatalf("deep nesting: %d segments", len(got))
	}
}

func cmdSegContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
