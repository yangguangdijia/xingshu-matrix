package rbac

import (
	"sync"
	"unsafe"

	"github.com/gin-gonic/gin"
)

// Guard describes what a permission / scope middleware built by one of the
// rbac factories enforces. It is recorded at construction time so tooling such
// as the OpenAPI generator can map a route's handler chain back to the
// permissions it requires without executing anything.
type Guard struct {
	Kind  string   `json:"kind"`  // all | any | env | scope
	Perms []string `json:"perms"` // required permission codes (all/any/env)
	Scope string   `json:"scope"` // cluster | namespace | workspace | workspace-manage (scope)
}

type guardEntry struct {
	h gin.HandlerFunc // keeps the closure alive so its address is never reused
	g Guard
}

var guards struct {
	sync.RWMutex
	m map[uintptr]guardEntry
}

// HandlerKey identifies one handler closure instance. Unlike
// reflect.Value.Pointer (the code pointer, shared by every closure built from
// the same function literal), it returns the closure object's address, which is
// distinct for each call of a factory such as RequirePerm.
func HandlerKey(h gin.HandlerFunc) uintptr {
	if h == nil {
		return 0
	}
	return *(*uintptr)(unsafe.Pointer(&h))
}

func registerGuard(h gin.HandlerFunc, g Guard) gin.HandlerFunc {
	g.Perms = append([]string(nil), g.Perms...)
	guards.Lock()
	if guards.m == nil {
		guards.m = make(map[uintptr]guardEntry)
	}
	guards.m[HandlerKey(h)] = guardEntry{h: h, g: g}
	guards.Unlock()
	return h
}

// LookupGuard reports what h enforces if it was built by an rbac factory.
func LookupGuard(h gin.HandlerFunc) (Guard, bool) {
	guards.RLock()
	e, ok := guards.m[HandlerKey(h)]
	guards.RUnlock()
	if !ok {
		return Guard{}, false
	}
	g := e.g
	g.Perms = append([]string(nil), g.Perms...)
	return g, true
}

func workspaceScope(manage bool) string {
	if manage {
		return "workspace-manage"
	}
	return "workspace"
}
