// Package web serves the single-page front-end (public/) with history-mode fallback.
package web

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// NoRoute returns the gin NoRoute handler: unknown /api/* paths get a JSON 404,
// existing files under dir are served, and every other GET/HEAD falls back to
// dir/index.html so client-side routes work. dir == "" disables static serving.
func NoRoute(dir string) gin.HandlerFunc {
	root, _ := filepath.Abs(dir)
	index := filepath.Join(root, "index.html")
	return func(c *gin.Context) {
		p := c.Request.URL.Path
		if strings.HasPrefix(p, "/api/") || p == "/api" || dir == "" {
			httpx.Fail(c, httpx.NotFound("route not found"))
			return
		}
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			httpx.Fail(c, httpx.NewError(http.StatusMethodNotAllowed, httpx.CodeNotFound, "method not allowed"))
			return
		}
		clean := path.Clean("/" + p)
		full := filepath.Join(root, filepath.FromSlash(clean))
		if !strings.HasPrefix(full, root) { // defence in depth; Clean already strips ..
			httpx.Fail(c, httpx.NotFound("not found"))
			return
		}
		if fi, err := os.Stat(full); err == nil && !fi.IsDir() && clean != "/index.html" {
			// 前端没有构建步骤、文件名不带内容哈希：一旦设长缓存，发版后浏览器会继续用
			// 旧的 index.html / *.js（实测踩过：升级镜像后页面仍跑旧脚本）。
			// 因此一律 no-cache，由 Last-Modified/ETag 协商成 304，成本可忽略。
			c.Header("Cache-Control", "no-cache")
			c.File(full)
			return
		}
		// A missing asset with an extension is a real 404, not a client route.
		if ext := path.Ext(clean); ext != "" && ext != ".html" {
			c.Status(http.StatusNotFound)
			return
		}
		if _, err := os.Stat(index); err != nil {
			httpx.Fail(c, httpx.NotFound("front-end not found (static_dir has no index.html)"))
			return
		}
		c.Header("Cache-Control", "no-cache")
		c.File(index)
	}
}
