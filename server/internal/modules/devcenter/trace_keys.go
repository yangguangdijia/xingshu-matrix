package devcenter

import (
	"regexp"
	"strconv"
	"strings"
)

// ItemKey 是从文本（提交说明、分支名、合并请求标题）里识别出的研发对象编号。
// Type 为 "requirement"（需求 REQ-<id>）或 "bug"（缺陷 BUG-<id>），ID 为对应表主键。
type ItemKey struct {
	Type string `json:"type"`
	ID   int64  `json:"id"`
}

// Kind 返回对象对应的研发中心类型（requirements / bugs），用于拼表名与路由。
func (k ItemKey) Kind() string {
	if k.Type == "bug" {
		return "bugs"
	}
	return "requirements"
}

// String 返回规范写法，如 REQ-12、BUG-3。
func (k ItemKey) String() string {
	p := "REQ-"
	if k.Type == "bug" {
		p = "BUG-"
	}
	return p + strconv.FormatInt(k.ID, 10)
}

// itemTypeOfKind 把路由里的类型（requirements / bugs）转成关联表 item_type。
func itemTypeOfKind(k string) string {
	switch k {
	case "requirements":
		return "requirement"
	case "bugs":
		return "bug"
	}
	return ""
}

var itemKeyRe = regexp.MustCompile(`(?i)(req|bug)-([0-9]{1,18})`)

func isASCIIAlnum(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// ParseItemKeys 从任意文本中识别 REQ-<id> / BUG-<id>（大小写不敏感），也接受 #REQ-12、[BUG-3]、
// feature/REQ-12-login、fix_bug-3 等写法。编号前不能紧挨字母或数字（避免 XREQ-1），
// 编号后不能紧挨 ASCII 字母、数字或下划线（避免 REQ-12abc）。结果按首次出现顺序去重；id 必须为正数。
func ParseItemKeys(text string) []ItemKey {
	if text == "" {
		return nil
	}
	var out []ItemKey
	seen := map[ItemKey]bool{}
	for _, m := range itemKeyRe.FindAllStringSubmatchIndex(text, -1) {
		start, end := m[0], m[1]
		if start > 0 && isASCIIAlnum(text[start-1]) {
			continue
		}
		if end < len(text) && (isASCIIAlnum(text[end]) || text[end] == '_') {
			continue
		}
		id, err := strconv.ParseInt(text[m[4]:m[5]], 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		typ := "requirement"
		if strings.EqualFold(text[m[2]:m[3]], "bug") {
			typ = "bug"
		}
		k := ItemKey{Type: typ, ID: id}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}
