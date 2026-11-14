package bastion

// AI 辅助起草命令过滤规则（对齐 JumpServer 5.1）：管理员用一句中文描述想拦截/放行的命令，
// 并给出「应拦截」「应放行」样例；平台默认大模型生成规则草稿，服务端再做三层校验：
//   1. filterIn.check()（与创建过滤规则完全一致）；
//   2. 正则可编译且长度 ≤ aiDraftPatternMax；
//   3. 用终端拦截同一匹配逻辑（Evaluate + splitCommandSegments 逐段）预览每条样例。
// 结果只返回给前端，由用户确认后再走 POST /bastion/filters 落库；本接口不写数据库。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/ai"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const (
	aiDraftPatternMax = 1000 // 草稿正则长度上限（防止模型生成病态正则）
	aiDraftSamplesMax = 50   // 每类样例最多条数
	aiDraftSampleLen  = 1000 // 单条样例最大长度
	aiDraftScene      = "bastion.command_filter.draft"
	aiDraftNoModelMsg = "未配置大模型"
)

// aiDraftMatchTypes 是 filterIn 支持的匹配方式：过滤规则只存正则 pattern，故仅 regex。
var aiDraftMatchTypes = []string{"regex"}

type aiDraftIn struct {
	Description  string   `json:"description" binding:"required,max=2000"`
	SamplesBlock []string `json:"samples_block"`
	SamplesAllow []string `json:"samples_allow"`
}

// AIDraft 是模型返回的规则草稿。
type AIDraft struct {
	Name      string `json:"name"`
	MatchType string `json:"match_type"`
	Pattern   string `json:"pattern"`
	Action    string `json:"action"`
	Reason    string `json:"reason"`
}

// AIDraftPreview 是一条样例的预览结果。
type AIDraftPreview struct {
	Command  string `json:"command"`
	Expected string `json:"expected"` // block | allow
	Matched  bool   `json:"matched"`  // 规则是否命中该命令（含拆段后的任一段）
	Pass     bool   `json:"pass"`     // 按规则动作推得的结果是否符合预期
}

// AIDraftResult 是 POST /bastion/command-filters/ai-draft 的返回体。
type AIDraftResult struct {
	Draft   AIDraft          `json:"draft"`
	Valid   bool             `json:"valid"`
	Errors  []string         `json:"errors"`
	Preview []AIDraftPreview `json:"preview"`
}

// draftModel 是起草所需的最小模型能力：给定 system/user 提示返回完整文本。
type draftModel interface {
	Complete(ctx context.Context, p *rbac.Principal, system, user string) (string, error)
}

// errNoModel 表示平台未配置可用大模型。
var errNoModel = httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, aiDraftNoModelMsg)

// newDraftModel 返回平台默认模型；未配置时返回 nil。测试中替换为 fake。
var newDraftModel = func(m *Module) draftModel {
	if m == nil || m.d == nil || m.d.AI == nil || m.d.AI.Store == nil {
		return nil
	}
	return aiServiceModel{s: m.d.AI}
}

type aiServiceModel struct{ s *ai.Service }

func (a aiServiceModel) Complete(ctx context.Context, p *rbac.Principal, system, user string) (string, error) {
	col := &draftCollector{}
	err := a.s.Chat(ctx, p, ai.ChatRequest{
		Scene:    aiDraftScene,
		Messages: []ai.Message{ai.TextMessage("system", system), ai.TextMessage("user", user)},
		Tools:    json.RawMessage(`[]`),
	}, col)
	if err != nil {
		// ai.Store.ResolveModel 在没有任何启用模型时返回该校验错误，统一映射为 503。
		if e, ok := err.(*httpx.Error); ok && strings.Contains(e.Message, "尚未配置可用的 AI 模型") {
			return "", errNoModel
		}
		return "", err
	}
	return col.b.String(), nil
}

// draftCollector 汇总流式 chat.completion.chunk 的文本增量。
type draftCollector struct{ b strings.Builder }

func (c *draftCollector) Chunk(raw []byte) error {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &chunk) != nil {
		return nil
	}
	for _, ch := range chunk.Choices {
		c.b.WriteString(ch.Delta.Content)
	}
	return nil
}

func (c *draftCollector) Event(string, any) error { return nil }

const aiDraftSystem = `你是「星枢 Matrix」堡垒机的命令过滤规则起草助手。
根据管理员的中文描述和样例，生成一条命令过滤规则，只输出严格合法的 JSON 对象（不要 Markdown 代码块、不要解释）：
{"name": "规则名称（中文，≤60 字）", "match_type": "%s", "pattern": "Go RE2 正则", "action": "deny|confirm|approve|allow", "reason": "一句中文说明为何这样写"}
要求：
- match_type 只能取：%s。
- pattern 使用 Go regexp（RE2）语法，不支持反向引用和环视；不能匹配空字符串；长度不超过 %d。
- 命令会被拆成多段（按 ; && || | 与 $( ) 拆分，并去掉 sudo/env 等前缀与 /bin/ 路径），每段单独匹配，所以 pattern 只需描述单条命令，建议以 ^ 开头、用 \b 或 \s 界定命令名。
- action 含义：deny 拒绝；confirm 二次确认；approve 提交审批；allow 放行（白名单）。
- 「应拦截」样例必须被规则拦截，「应放行」样例必须不被拦截。`

func aiDraftPrompt(in aiDraftIn) (system, user string) {
	types := strings.Join(aiDraftMatchTypes, " | ")
	system = fmt.Sprintf(aiDraftSystem, aiDraftMatchTypes[0], types, aiDraftPatternMax)
	var b strings.Builder
	b.WriteString("描述：")
	b.WriteString(strings.TrimSpace(in.Description))
	writeList := func(title string, xs []string) {
		if len(xs) == 0 {
			return
		}
		b.WriteString("\n" + title + "：\n")
		for _, x := range xs {
			b.WriteString("- " + x + "\n")
		}
	}
	writeList("应拦截样例", in.SamplesBlock)
	writeList("应放行样例", in.SamplesAllow)
	return system, b.String()
}

// cleanSamples 去空白、去空行，并限制条数与长度。
func cleanSamples(xs []string) ([]string, error) {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x == "" {
			continue
		}
		if len(x) > aiDraftSampleLen {
			return nil, httpx.Validation(fmt.Sprintf("单条样例不能超过 %d 个字符", aiDraftSampleLen))
		}
		out = append(out, x)
	}
	if len(out) > aiDraftSamplesMax {
		return nil, httpx.Validation(fmt.Sprintf("每类样例最多 %d 条", aiDraftSamplesMax))
	}
	return out, nil
}

func (m *Module) aiDraftFilter(c *gin.Context) {
	var in aiDraftIn
	if !httpx.Bind(c, &in) {
		return
	}
	if strings.TrimSpace(in.Description) == "" {
		httpx.Fail(c, httpx.Validation("请填写规则描述"))
		return
	}
	var err error
	if in.SamplesBlock, err = cleanSamples(in.SamplesBlock); err != nil {
		httpx.Fail(c, err)
		return
	}
	if in.SamplesAllow, err = cleanSamples(in.SamplesAllow); err != nil {
		httpx.Fail(c, err)
		return
	}
	model := newDraftModel(m)
	if model == nil {
		httpx.Fail(c, errNoModel)
		return
	}
	res, err := runAIDraft(c.Request.Context(), model, rbac.PrincipalFrom(c), in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}

// runAIDraft 调用模型并校验、预览草稿。模型输出不是合法 JSON 时返回 400。
func runAIDraft(ctx context.Context, model draftModel, p *rbac.Principal, in aiDraftIn) (*AIDraftResult, error) {
	system, user := aiDraftPrompt(in)
	raw, err := model.Complete(ctx, p, system, user)
	if err != nil {
		return nil, err
	}
	var d AIDraft
	if err := json.Unmarshal([]byte(trimDraftFence(raw)), &d); err != nil {
		return nil, httpx.BadRequest("模型没有返回合法 JSON，请调整描述后重试")
	}
	d.Name = strings.TrimSpace(d.Name)
	d.MatchType = strings.ToLower(strings.TrimSpace(d.MatchType))
	d.Action = strings.ToLower(strings.TrimSpace(d.Action))
	if d.MatchType == "" {
		d.MatchType = aiDraftMatchTypes[0]
	}
	return validateAIDraft(d, in.SamplesBlock, in.SamplesAllow), nil
}

func validateAIDraft(d AIDraft, block, allow []string) *AIDraftResult {
	res := &AIDraftResult{Draft: d, Errors: []string{}, Preview: []AIDraftPreview{}}
	addErr := func(s string) { res.Errors = append(res.Errors, s) }

	if d.Name == "" {
		addErr("规则名称为空")
	} else if len([]rune(d.Name)) > 120 {
		addErr("规则名称超过 120 个字符")
	}
	okType := false
	for _, t := range aiDraftMatchTypes {
		okType = okType || d.MatchType == t
	}
	if !okType {
		addErr("不支持的匹配方式：" + d.MatchType + "（仅支持 " + strings.Join(aiDraftMatchTypes, "、") + "）")
	}
	switch d.Action {
	case "deny", "confirm", "approve", "allow":
	default:
		addErr("不支持的动作：" + d.Action + "（仅支持 deny、confirm、approve、allow）")
	}
	if strings.TrimSpace(d.Pattern) == "" {
		addErr("正则为空")
	} else if len(d.Pattern) > aiDraftPatternMax {
		addErr(fmt.Sprintf("正则长度超过 %d", aiDraftPatternMax))
	} else if len(d.Pattern) > 512 {
		addErr("正则长度超过过滤规则保存上限 512")
	}

	var re *regexp.Regexp
	if d.Pattern != "" && len(d.Pattern) <= aiDraftPatternMax {
		var err error
		if re, err = regexp.Compile(d.Pattern); err != nil {
			addErr("正则无法编译：" + err.Error())
			re = nil
		} else {
			// 与创建过滤规则同一套校验（含不能匹配空命令）。
			fi := filterIn{Name: d.Name, Pattern: d.Pattern, Action: d.Action}
			if err := fi.check(); err != nil {
				msg := err.Error()
				if e, ok := err.(*httpx.Error); ok {
					msg = e.Message
				}
				addErr("规则校验未通过：" + msg)
				re = nil
			}
		}
	}

	if re != nil {
		rule := []FilterRule{{Name: d.Name, Re: re, Action: d.Action}}
		mismatch := 0
		add := func(cmd, expected string) {
			matched := aiDraftMatches(rule, cmd)
			blocked := matched && d.Action != "allow"
			allowed := !blocked
			if d.Action == "allow" {
				// 白名单规则：命中即放行，未命中则交给后续规则（视为未放行）。
				allowed = matched
			}
			pass := (expected == "block" && !allowed) || (expected == "allow" && allowed)
			if !pass {
				mismatch++
			}
			res.Preview = append(res.Preview, AIDraftPreview{Command: cmd, Expected: expected, Matched: matched, Pass: pass})
		}
		for _, s := range block {
			add(s, "block")
		}
		for _, s := range allow {
			add(s, "allow")
		}
		if mismatch > 0 {
			addErr(fmt.Sprintf("有 %d 条样例与预期不符", mismatch))
		}
	}
	res.Valid = len(res.Errors) == 0
	return res
}

// aiDraftMatches 与终端拦截使用同一匹配逻辑：整行交给 Evaluate，再对 splitCommandSegments
// 拆出的每一段调用 Evaluate，任一命中即视为命中。
func aiDraftMatches(rules []FilterRule, cmd string) bool {
	if Evaluate(rules, cmd) != nil {
		return true
	}
	for _, seg := range splitCommandSegments(cmd) {
		if Evaluate(rules, seg) != nil {
			return true
		}
	}
	return false
}

// trimDraftFence 去掉部分模型仍会加的 ```json 代码块包裹。
func trimDraftFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimPrefix(s, "json")
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
