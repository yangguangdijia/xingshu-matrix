package settings

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// AppearanceKey 保存外观设置（Logo、主题色、登录页标题、版权信息）。
const AppearanceKey = "platform.appearance"

// Appearance 是外观设置，登录前即可通过 GET /branding 读取。
type Appearance struct {
	Logo         string `json:"logo"`          // data:image/png|jpeg|svg+xml;base64,...，≤256 KB；空为默认 Logo
	PrimaryColor string `json:"primary_color"` // #RRGGBB；空为默认蓝色
	LoginTitle   string `json:"login_title"`   // 登录页主标题；空为平台名称
	Copyright    string `json:"copyright"`     // 登录页底部版权信息
}

var (
	hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	logoURL  = regexp.MustCompile(`^data:image/(png|jpeg|svg\+xml);base64,([A-Za-z0-9+/=]+)$`)
)

// Validate 校验外观设置。SVG 只作为 <img> 引用，浏览器不会执行其中脚本，但仍拒绝含 script 的 SVG。
func (a *Appearance) Validate() error {
	a.PrimaryColor = strings.TrimSpace(a.PrimaryColor)
	a.LoginTitle = strings.TrimSpace(a.LoginTitle)
	a.Copyright = strings.TrimSpace(a.Copyright)
	if a.PrimaryColor != "" && !hexColor.MatchString(a.PrimaryColor) {
		return httpx.Validation("primary_color must be #RRGGBB")
	}
	if len([]rune(a.LoginTitle)) > 40 || len([]rune(a.Copyright)) > 120 {
		return httpx.Validation("login_title ≤ 40 chars, copyright ≤ 120 chars")
	}
	if a.Logo != "" {
		m := logoURL.FindStringSubmatch(a.Logo)
		if m == nil {
			return httpx.Validation("logo must be a base64 data URL of png, jpeg or svg")
		}
		raw, err := base64.StdEncoding.DecodeString(m[2])
		if err != nil {
			return httpx.Validation("logo is not valid base64")
		}
		if len(raw) > 256*1024 {
			return httpx.Validation("logo must be ≤ 256 KB")
		}
		if m[1] == "svg+xml" {
			low := strings.ToLower(string(raw))
			if strings.Contains(low, "<script") || strings.Contains(low, "javascript:") || regexp.MustCompile(`\son[a-z]+\s*=`).MatchString(low) {
				return httpx.Validation("svg logo must not contain scripts or event handlers")
			}
		}
	}
	return nil
}

func (s *Service) appearance(c *gin.Context) Appearance {
	var a Appearance
	_ = s.Get(c.Request.Context(), AppearanceKey, &a)
	return a
}

func (s *Service) registerAppearance(r *gin.RouterGroup) {
	r.GET("/settings/appearance", rbac.RequirePerm("setting:read"), func(c *gin.Context) { httpx.OK(c, s.appearance(c)) })
	r.PUT("/settings/appearance", rbac.RequirePerm("setting:manage"), func(c *gin.Context) {
		var a Appearance
		if !httpx.Bind(c, &a) {
			return
		}
		if err := a.Validate(); err != nil {
			httpx.Fail(c, err)
			return
		}
		raw, _ := json.Marshal(a)
		var by int64
		if p := rbac.PrincipalFrom(c); p != nil {
			by = p.UserID
		}
		if _, err := s.Put(c.Request.Context(), AppearanceKey, raw, false, "外观：Logo、主题色、登录页标题、版权", by); err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetResource(c, "setting", AppearanceKey)
		httpx.OK(c, a)
	})
}
