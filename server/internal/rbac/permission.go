// Package rbac implements role-based access control with permissions written as
// "resource:action", wildcard matching, a permission catalog modules register
// into, gin middleware, and per-environment change policies (prod approval).
package rbac

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Standard actions. Modules may define others (e.g. "exec", "approve").
const (
	ActionRead   = "read"   // list / get
	ActionWrite  = "write"  // create / update / delete of operational resources
	ActionExec   = "exec"   // run commands, deploy, open shells
	ActionManage = "manage" // platform administration (users, roles, settings, AI models)
)

// Permission describes one grantable permission.
type Permission struct {
	Code        string `json:"code"` // resource:action
	Resource    string `json:"resource"`
	Action      string `json:"action"`
	Module      string `json:"module"`
	Description string `json:"description"`
}

// P is a shorthand constructor for Permission.
func P(module, resource, action, desc string) Permission {
	return Permission{Code: resource + ":" + action, Resource: resource, Action: action, Module: module, Description: desc}
}

// ValidCode reports whether s is a syntactically valid grant: "*", "res:act",
// "res:*" or "*:act" (lowercase letters, digits, '.', '_', '-').
func ValidCode(s string) bool {
	if s == "*" {
		return true
	}
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if p == "*" {
			continue
		}
		if p == "" || len(p) > 64 {
			return false
		}
		for _, r := range p {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
				return false
			}
		}
	}
	return true
}

// Match reports whether any granted permission satisfies required.
func Match(granted []string, required string) bool {
	rr, ra, ok := strings.Cut(required, ":")
	if !ok {
		return false
	}
	for _, g := range granted {
		if g == "*" || g == required {
			return true
		}
		gr, ga, ok := strings.Cut(g, ":")
		if !ok {
			continue
		}
		if (gr == "*" || gr == rr) && (ga == "*" || ga == ra) {
			return true
		}
	}
	return false
}

// Catalog is the registry of known permissions (for the role editor UI).
type Catalog struct {
	mu    sync.RWMutex
	perms map[string]Permission
}

// NewCatalog returns an empty catalog.
func NewCatalog() *Catalog { return &Catalog{perms: map[string]Permission{}} }

// Register adds permissions; registering the same code twice is an error unless identical.
func (c *Catalog) Register(perms ...Permission) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range perms {
		if p.Resource == "" || p.Action == "" || strings.ContainsAny(p.Resource+p.Action, ":* ") {
			return fmt.Errorf("rbac: invalid permission %q", p.Code)
		}
		p.Code = p.Resource + ":" + p.Action
		if old, ok := c.perms[p.Code]; ok && old != p {
			return fmt.Errorf("rbac: permission %s registered twice with different metadata", p.Code)
		}
		c.perms[p.Code] = p
	}
	return nil
}

// MustRegister is Register that panics (for init-time registration).
func (c *Catalog) MustRegister(perms ...Permission) {
	if err := c.Register(perms...); err != nil {
		panic(err)
	}
}

// List returns all permissions sorted by code.
func (c *Catalog) List() []Permission {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Permission, 0, len(c.perms))
	for _, p := range c.perms {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}
