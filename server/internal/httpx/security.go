package httpx

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// SecurityOptions configures SecurityHeaders. The zero value is not useful;
// start from DefaultSecurityOptions.
type SecurityOptions struct {
	// CSP is the full Content-Security-Policy value. Empty disables the header.
	CSP string
	// FrameAncestors is the CSP frame-ancestors source list (e.g. "'self'").
	// It is appended to CSP and also drives X-Frame-Options: "'none'" maps to
	// DENY, "'self'" to SAMEORIGIN, anything else omits X-Frame-Options (the
	// legacy header cannot express an allow-list; CSP covers modern browsers).
	FrameAncestors string
	// ReferrerPolicy value; empty disables the header.
	ReferrerPolicy string
	// PermissionsPolicy value; empty disables the header.
	PermissionsPolicy string
	// HSTS enables Strict-Transport-Security on HTTPS requests (TLS on this
	// listener, or X-Forwarded-Proto=https from a trusted proxy).
	HSTS bool
	// HSTSMaxAge in seconds (default one year).
	HSTSMaxAge int
	// HSTSIncludeSubdomains adds includeSubDomains.
	HSTSIncludeSubdomains bool
}

// DefaultCSP is the policy the shipped SPA (public/) needs, derived from an
// audit of public/*.js, public/*.css and public/index.html:
//
//   - script-src 'unsafe-inline': index.html carries several large inline
//     <script> blocks (the SPA shell). No eval / new Function is used, so
//     'unsafe-eval' is NOT granted. Remove 'unsafe-inline' once the inline
//     scripts are moved to files (or nonce/hash them).
//   - style-src 'unsafe-inline': inline <style> blocks and style="" attributes
//     everywhere; plus Google Fonts stylesheet (fonts.googleapis.com).
//   - font-src fonts.gstatic.com (Geist / Geist Mono web fonts) and data:.
//   - img-src data: blob:: inline SVG data URIs (icons, watermark) and
//     URL.createObjectURL previews/downloads.
//   - connect-src 'self': XS API (fetch, SSE over fetch, WebSocket on the same
//     origin; CSP3 'self' matches ws:/wss: of the same host).
//   - frame-src 'self' blob:, worker-src 'self' blob:, object-src 'none'.
const DefaultCSP = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline'; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
	"font-src 'self' data: https://fonts.gstatic.com; " +
	"img-src 'self' data: blob:; " +
	"connect-src 'self'; " +
	"frame-src 'self' blob:; " +
	"worker-src 'self' blob:; " +
	"media-src 'self' blob:; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'"

// DefaultSecurityOptions returns the recommended defaults.
func DefaultSecurityOptions() SecurityOptions {
	return SecurityOptions{
		CSP:                   DefaultCSP,
		FrameAncestors:        "'self'",
		ReferrerPolicy:        "strict-origin-when-cross-origin",
		PermissionsPolicy:     "camera=(), microphone=(), geolocation=(), payment=(), usb=(), fullscreen=(self)",
		HSTS:                  true,
		HSTSMaxAge:            31536000,
		HSTSIncludeSubdomains: false,
	}
}

// BuildCSP returns the effective Content-Security-Policy value.
func (o SecurityOptions) BuildCSP() string {
	csp := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(o.CSP), ";"))
	if csp == "" {
		return ""
	}
	if fa := strings.TrimSpace(o.FrameAncestors); fa != "" && !strings.Contains(csp, "frame-ancestors") {
		csp += "; frame-ancestors " + fa
	}
	return csp
}

func (o SecurityOptions) frameOptions() string {
	switch strings.TrimSpace(o.FrameAncestors) {
	case "'none'":
		return "DENY"
	case "'self'":
		return "SAMEORIGIN"
	}
	return ""
}

func (o SecurityOptions) hstsValue() string {
	age := o.HSTSMaxAge
	if age <= 0 {
		age = 31536000
	}
	v := "max-age=" + strconv.Itoa(age)
	if o.HSTSIncludeSubdomains {
		v += "; includeSubDomains"
	}
	return v
}

// IsHTTPS reports whether the client connection is HTTPS: TLS on this
// listener, or X-Forwarded-Proto=https set by a trusted proxy (gin's trusted
// proxy list decides whether forwarded headers are believed).
func IsHTTPS(c *gin.Context) bool {
	if c.Request.TLS != nil {
		return true
	}
	if !trustedForwarder(c) {
		return false
	}
	p := c.GetHeader("X-Forwarded-Proto")
	if i := strings.IndexByte(p, ','); i >= 0 {
		p = p[:i]
	}
	return strings.EqualFold(strings.TrimSpace(p), "https")
}

// trustedForwarder reports whether the direct peer is a trusted proxy. gin
// exposes this indirectly: ClientIP() differs from RemoteIP() only when the
// forwarded header was honoured; when it is equal we additionally accept
// proxies on loopback (local reverse proxy / sidecar).
func trustedForwarder(c *gin.Context) bool {
	remote := c.RemoteIP()
	if remote == "127.0.0.1" || remote == "::1" {
		return true
	}
	return c.ClientIP() != remote
}

// SecurityHeaders sets the browser hardening headers on every response.
func SecurityHeaders(o SecurityOptions) gin.HandlerFunc {
	csp := o.BuildCSP()
	xfo := o.frameOptions()
	hsts := o.hstsValue()
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		if csp != "" {
			h.Set("Content-Security-Policy", csp)
		}
		if xfo != "" {
			h.Set("X-Frame-Options", xfo)
		}
		if o.ReferrerPolicy != "" {
			h.Set("Referrer-Policy", o.ReferrerPolicy)
		}
		if o.PermissionsPolicy != "" {
			h.Set("Permissions-Policy", o.PermissionsPolicy)
		}
		if o.HSTS && IsHTTPS(c) {
			h.Set("Strict-Transport-Security", hsts)
		}
		c.Next()
	}
}
