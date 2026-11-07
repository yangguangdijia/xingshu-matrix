package apidoc

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/version"
)

// Document is the subset of OpenAPI 3.0 the generator emits.
type Document struct {
	OpenAPI    string                          `json:"openapi"`
	Info       Info                            `json:"info"`
	Servers    []Server                        `json:"servers"`
	Tags       []Tag                           `json:"tags"`
	Paths      map[string]map[string]Operation `json:"paths"`
	Components Components                      `json:"components"`
	Security   []map[string][]string           `json:"security"`
}

type Info struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

type Server struct {
	URL string `json:"url"`
}

type Tag struct {
	Name string `json:"name"`
}

type Components struct {
	SecuritySchemes map[string]SecurityScheme `json:"securitySchemes"`
}

type SecurityScheme struct {
	Type         string `json:"type"`
	Scheme       string `json:"scheme"`
	BearerFormat string `json:"bearerFormat,omitempty"`
}

type Parameter struct {
	Name     string            `json:"name"`
	In       string            `json:"in"`
	Required bool              `json:"required"`
	Schema   map[string]string `json:"schema"`
}

type Response struct {
	Description string `json:"description"`
}

// Operation is one method on one path. x-xs-access carries the inferred
// access requirements; x-required-permissions duplicates the flat list for
// tools that only read simple extensions.
type Operation struct {
	OperationID string                `json:"operationId"`
	Summary     string                `json:"summary"`
	Tags        []string              `json:"tags"`
	Parameters  []Parameter           `json:"parameters,omitempty"`
	Responses   map[string]Response   `json:"responses"`
	Security    []map[string][]string `json:"security"`
	Access      Access                `json:"x-xs-access"`
	Permissions []string              `json:"x-required-permissions"`
	Handler     string                `json:"x-handler,omitempty"`
}

// Generate builds the OpenAPI document for routes. Paths stay absolute
// (/api/v1/...) with server "/"; endpoints outside the API root (healthz,
// metrics) are listed under tag "system".
func Generate(routes []Route, v string) Document {
	doc := Document{
		OpenAPI: "3.0.3",
		Info: Info{
			Title:       "星枢 Matrix API",
			Version:     v,
			Description: "由运行中的路由表自动生成。所需权限由路由上的 RBAC 中间件推断；处理函数内部的二次校验不在此列。",
		},
		Servers: []Server{{URL: "/"}},
		Paths:   map[string]map[string]Operation{},
		Components: Components{SecuritySchemes: map[string]SecurityScheme{
			"bearerAuth": {Type: "http", Scheme: "bearer", BearerFormat: "JWT"},
		}},
		Security: []map[string][]string{{"bearerAuth": {}}},
	}
	seenTag := map[string]bool{}
	for _, r := range routes {
		if !seenTag[r.Tag] {
			seenTag[r.Tag] = true
			doc.Tags = append(doc.Tags, Tag{Name: r.Tag})
		}
		op := Operation{
			OperationID: r.OperationID,
			Summary:     r.Method + " " + r.Path,
			Tags:        []string{r.Tag},
			Responses: map[string]Response{
				"200": {Description: "成功，响应体为统一信封 {code, message, data, request_id}"},
			},
			Access:      r.Access,
			Permissions: append([]string{}, r.Access.Permissions...),
			Handler:     r.Handler,
		}
		for _, p := range r.Params {
			op.Parameters = append(op.Parameters, Parameter{Name: p, In: "path", Required: true, Schema: map[string]string{"type": "string"}})
		}
		if r.Access.Public {
			op.Security = []map[string][]string{} // explicitly no auth
		} else {
			op.Security = []map[string][]string{{"bearerAuth": {}}}
			op.Responses["401"] = Response{Description: "未登录或会话失效"}
			if len(r.Access.Permissions) > 0 || len(r.Access.AnyOf) > 0 || len(r.Access.DataScopes) > 0 || r.Access.EnvScoped {
				op.Responses["403"] = Response{Description: "权限不足或不在数据范围内"}
			}
		}
		m := doc.Paths[r.Path]
		if m == nil {
			m = map[string]Operation{}
			doc.Paths[r.Path] = m
		}
		m[strings.ToLower(r.Method)] = op
	}
	return doc
}

// Register mounts GET /openapi.json on authed (i.e. /api/v1/openapi.json,
// login required). Call it after every module has registered its routes; the
// document is generated on first request and cached for the process lifetime.
// ?envelope=1 wraps the document in the standard {code,data} envelope for the
// web console; the default is the raw OpenAPI JSON for tooling.
func Register(authed *gin.RouterGroup, e *gin.Engine) {
	markers := AuthMarkers(authed, e)
	var (
		once sync.Once
		raw  []byte
		doc  Document
		err  error
	)
	build := func() {
		doc = Generate(Collect(e, markers), version.Get().Version)
		raw, err = json.Marshal(doc)
	}
	authed.GET("/openapi.json", func(c *gin.Context) {
		once.Do(build)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		if c.Query("envelope") == "1" {
			httpx.OK(c, doc)
			return
		}
		c.Header("Cache-Control", "private, max-age=300")
		c.Data(http.StatusOK, "application/json; charset=utf-8", raw)
	})
}
