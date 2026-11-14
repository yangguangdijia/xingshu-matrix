package bastion

// 命令规范化：把用户在堡垒机终端输入的一行命令拆成若干「段」，供命令过滤规则逐段匹配，
// 以防止 `ls; rm -rf /`、`echo $(rm -rf /)`、`sudo rm -rf /`、`/bin/rm -rf /`、续行等方式绕过规则。
//
// 规则（纯函数，无外部依赖）：
//   - 去掉反斜杠续行（\<换行>），引号外的连续空白合并为一个空格；
//   - 在引号外按 ; && || | & 换行（以及子 shell 的括号）拆分，重定向里的 >& <& &> >| 不算分隔符；
//   - $( … )、<( … )、>( … ) 与反引号中的子命令递归拆分，作为独立段（原段保留）；
//   - 段首的 sudo / env VAR=… / nohup / time / nice / timeout / exec / command 等前缀与 VAR=… 赋值
//     去掉后的命令也作为一段（原段同样保留）；
//   - 命令名去掉路径与引号（/bin/rm → rm、\rm → rm、"rm" → rm）后也作为一段；
//   - 结果去重保序，单段最长 cmdSegmentMaxLen 字节，最多 cmdSegmentMaxCount 段。

import (
	"strings"
	"unicode/utf8"
)

const (
	cmdSegmentMaxLen   = 4096
	cmdSegmentMaxCount = 64
	cmdSegmentMaxDepth = 16
)

// splitCommandSegments 返回 cmd 规范化后的全部段（去重保序）。空命令返回 nil。
func splitCommandSegments(cmd string) []string {
	c := &cmdSegCollector{seen: map[string]bool{}}
	cmdScan(cmd, 0, c)
	return c.out
}

type cmdSegCollector struct {
	out  []string
	seen map[string]bool
}

func (c *cmdSegCollector) full() bool { return len(c.out) >= cmdSegmentMaxCount }

func (c *cmdSegCollector) add(s string) {
	s = strings.TrimSpace(s)
	if s == "" || c.full() {
		return
	}
	s = cmdTruncate(s, cmdSegmentMaxLen)
	if c.seen[s] {
		return
	}
	c.seen[s] = true
	c.out = append(c.out, s)
}

// emitSegment 写入一个原始段及其去前缀 / 去路径变体。
func (c *cmdSegCollector) emitSegment(raw string) {
	seg := cmdCollapseSpaces(raw)
	if seg == "" {
		return
	}
	c.add(seg)
	toks := cmdTokenize(seg)
	if len(toks) == 0 {
		return
	}
	stripped := cmdStripPrefixes(toks)
	if len(stripped) > 0 {
		c.add(strings.Join(stripped, " "))
	}
	for _, ts := range [][]string{toks, stripped} {
		if len(ts) == 0 {
			continue
		}
		if name := cmdBaseName(cmdUnquote(ts[0])); name != "" && name != ts[0] {
			c.add(strings.Join(append([]string{name}, ts[1:]...), " "))
		}
	}
}

// cmdScan 在引号外按分隔符拆分 s；遇到命令替换时递归。
func cmdScan(s string, depth int, c *cmdSegCollector) {
	var cur strings.Builder
	var subs []string
	inSingle, inDouble := false, false

	flush := func() {
		c.emitSegment(cur.String())
		cur.Reset()
		pending := subs
		subs = nil
		for _, sub := range pending {
			if c.full() {
				return
			}
			if depth+1 < cmdSegmentMaxDepth {
				cmdScan(sub, depth+1, c)
			} else {
				c.emitSegment(sub)
			}
		}
	}

	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inSingle {
			cur.WriteByte(ch)
			if ch == '\'' {
				inSingle = false
			}
			continue
		}
		// 反斜杠：续行直接去掉，其余转义原样保留两个字节。
		if ch == '\\' {
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
				continue
			}
			if i+2 < len(s) && s[i+1] == '\r' && s[i+2] == '\n' {
				i += 2
				continue
			}
			cur.WriteByte(ch)
			if i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			}
			continue
		}
		// 命令替换 / 进程替换：$( … )、<( … )、>( … )
		if (ch == '$' || (!inDouble && (ch == '<' || ch == '>'))) && i+1 < len(s) && s[i+1] == '(' {
			inner, end := cmdExtractParen(s, i+2)
			cur.WriteString(s[i:end])
			subs = append(subs, inner)
			i = end - 1
			continue
		}
		if ch == '`' {
			inner, end := cmdExtractBacktick(s, i+1)
			cur.WriteString(s[i:end])
			subs = append(subs, inner)
			i = end - 1
			continue
		}
		if inDouble {
			cur.WriteByte(ch)
			if ch == '"' {
				inDouble = false
			}
			continue
		}
		switch ch {
		case '\'':
			inSingle = true
			cur.WriteByte(ch)
		case '"':
			inDouble = true
			cur.WriteByte(ch)
		case ';', '\n', '\r', '(', ')':
			flush()
		case '&':
			// 重定向 >& <& &> 不是分隔符。
			if p := cmdLastNonSpace(cur.String()); p == '>' || p == '<' || (i+1 < len(s) && s[i+1] == '>') {
				cur.WriteByte(ch)
			} else {
				flush()
			}
		case '|':
			// >| 是强制覆盖重定向。
			if len(cur.String()) > 0 && cur.String()[cur.Len()-1] == '>' {
				cur.WriteByte(ch)
			} else {
				flush()
			}
		default:
			cur.WriteByte(ch)
		}
		if c.full() {
			return
		}
	}
	flush()
}

// cmdExtractParen 从 start（'(' 之后）开始找匹配的 ')'，返回内部文本与 ')' 之后的下标；未闭合时取到末尾。
func cmdExtractParen(s string, start int) (string, int) {
	level := 1
	inSingle, inDouble := false, false
	for i := start; i < len(s); i++ {
		ch := s[i]
		switch {
		case inSingle:
			if ch == '\'' {
				inSingle = false
			}
		case ch == '\\':
			i++
		case inDouble:
			if ch == '"' {
				inDouble = false
			} else if ch == '$' && i+1 < len(s) && s[i+1] == '(' {
				_, end := cmdExtractParen(s, i+2)
				i = end - 1
			}
		case ch == '\'':
			inSingle = true
		case ch == '"':
			inDouble = true
		case ch == '`':
			_, end := cmdExtractBacktick(s, i+1)
			i = end - 1
		case ch == '(':
			level++
		case ch == ')':
			level--
			if level == 0 {
				return s[start:i], i + 1
			}
		}
	}
	return s[start:], len(s)
}

// cmdExtractBacktick 从 start（'`' 之后）找下一个未转义的 '`'，返回反转义后的内部文本与结束下标。
func cmdExtractBacktick(s string, start int) (string, int) {
	var b strings.Builder
	for i := start; i < len(s); i++ {
		ch := s[i]
		if ch == '\\' && i+1 < len(s) && (s[i+1] == '`' || s[i+1] == '\\' || s[i+1] == '$') {
			b.WriteByte(s[i+1])
			i++
			continue
		}
		if ch == '`' {
			return b.String(), i + 1
		}
		b.WriteByte(ch)
	}
	return b.String(), len(s)
}

func cmdLastNonSpace(s string) byte {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != ' ' && s[i] != '\t' {
			return s[i]
		}
	}
	return 0
}

// cmdCollapseSpaces 把引号外的连续空白合并为一个空格并去掉首尾空白。
func cmdCollapseSpaces(s string) string {
	var b strings.Builder
	inSingle, inDouble, space := false, false, false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !inSingle && !inDouble && (ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '\v' || ch == '\f') {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteByte(ch)
		switch {
		case inSingle:
			if ch == '\'' {
				inSingle = false
			}
		case ch == '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case inDouble:
			if ch == '"' {
				inDouble = false
			}
		case ch == '\'':
			inSingle = true
		case ch == '"':
			inDouble = true
		}
	}
	return b.String()
}

// cmdTokenize 按引号外的空格切词（输入已经过 cmdCollapseSpaces），词保留原始引号。
func cmdTokenize(s string) []string {
	var toks []string
	start := -1
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !inSingle && !inDouble && ch == ' ' {
			if start >= 0 {
				toks = append(toks, s[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
		switch {
		case inSingle:
			if ch == '\'' {
				inSingle = false
			}
		case ch == '\\':
			i++
		case inDouble:
			if ch == '"' {
				inDouble = false
			}
		case ch == '\'':
			inSingle = true
		case ch == '"':
			inDouble = true
		}
	}
	if start >= 0 {
		toks = append(toks, s[start:])
	}
	return toks
}

// cmdUnquote 去掉词中的引号与反斜杠转义（"rm" → rm、\rm → rm、r"m" → rm）。
func cmdUnquote(t string) string {
	if !strings.ContainsAny(t, `'"\`) {
		return t
	}
	var b strings.Builder
	inSingle, inDouble := false, false
	for i := 0; i < len(t); i++ {
		ch := t[i]
		switch {
		case inSingle:
			if ch == '\'' {
				inSingle = false
			} else {
				b.WriteByte(ch)
			}
		case ch == '\\':
			if i+1 < len(t) {
				i++
				b.WriteByte(t[i])
			}
		case inDouble:
			if ch == '"' {
				inDouble = false
			} else {
				b.WriteByte(ch)
			}
		case ch == '\'':
			inSingle = true
		case ch == '"':
			inDouble = true
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// cmdBaseName 去掉命令路径前缀：/bin/rm → rm；以 / 结尾或为空时原样返回。
func cmdBaseName(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 && i < len(name)-1 {
		return name[i+1:]
	}
	return name
}

func cmdIsAssignment(t string) bool {
	eq := strings.IndexByte(t, '=')
	if eq <= 0 {
		return false
	}
	name := strings.TrimSuffix(t[:eq], "+")
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		ch := name[i]
		if ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (i > 0 && ch >= '0' && ch <= '9') {
			continue
		}
		return false
	}
	return true
}

// cmdSkipOptions 跳过以 '-' 开头的选项；argFlags 中的选项（如 -u）若未附带值则再跳过一个参数。
func cmdSkipOptions(toks []string, argFlags map[string]bool) []string {
	for len(toks) > 0 && strings.HasPrefix(toks[0], "-") && toks[0] != "-" {
		t := toks[0]
		toks = toks[1:]
		if t == "--" {
			break
		}
		if argFlags[t] && len(toks) > 0 {
			toks = toks[1:]
		}
	}
	return toks
}

var (
	cmdSudoArgFlags    = map[string]bool{"-u": true, "-g": true, "-C": true, "-p": true, "-U": true, "-r": true, "-t": true, "-D": true, "-h": true, "--user": true, "--group": true, "--prompt": true, "--chdir": true}
	cmdEnvArgFlags     = map[string]bool{"-u": true, "-C": true, "-S": true, "--unset": true, "--chdir": true, "--split-string": true}
	cmdNiceArgFlags    = map[string]bool{"-n": true, "--adjustment": true}
	cmdTimeoutArgFlags = map[string]bool{"-s": true, "-k": true, "--signal": true, "--kill-after": true}
	cmdStdbufArgFlags  = map[string]bool{"-i": true, "-o": true, "-e": true}
	cmdIoniceArgFlags  = map[string]bool{"-c": true, "-n": true, "-p": true}
	cmdTimeArgFlags    = map[string]bool{"-f": true, "-o": true, "--format": true, "--output": true}
)

// cmdStripPrefixes 反复去掉段首的包装命令、shell 关键字与 VAR=… 赋值，以及结尾的 '}'。
// 若没有可去掉的前缀返回 nil。
func cmdStripPrefixes(toks []string) []string {
	orig := len(toks)
	for len(toks) > 0 {
		t := toks[0]
		if cmdIsAssignment(t) {
			toks = toks[1:]
			continue
		}
		switch cmdBaseName(cmdUnquote(t)) {
		case "!", "{", "if", "then", "else", "elif", "do", "while", "until", "nohup", "setsid", "builtin":
			toks = cmdSkipOptions(toks[1:], nil)
		case "exec", "command":
			toks = cmdSkipOptions(toks[1:], map[string]bool{"-a": true})
		case "sudo", "doas":
			toks = cmdSkipOptions(toks[1:], cmdSudoArgFlags)
		case "env":
			toks = cmdSkipOptions(toks[1:], cmdEnvArgFlags)
		case "time":
			toks = cmdSkipOptions(toks[1:], cmdTimeArgFlags)
		case "nice":
			toks = cmdSkipOptions(toks[1:], cmdNiceArgFlags)
		case "stdbuf":
			toks = cmdSkipOptions(toks[1:], cmdStdbufArgFlags)
		case "ionice":
			toks = cmdSkipOptions(toks[1:], cmdIoniceArgFlags)
		case "timeout":
			toks = cmdSkipOptions(toks[1:], cmdTimeoutArgFlags)
			if len(toks) > 0 {
				toks = toks[1:] // 时长
			}
		default:
			goto done
		}
	}
done:
	if n := len(toks); n > 0 && toks[n-1] == "}" {
		toks = toks[:n-1]
	}
	if len(toks) == 0 || len(toks) == orig {
		return nil
	}
	return toks
}

func cmdTruncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
