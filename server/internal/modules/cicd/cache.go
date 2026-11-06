package cicd

// 编译缓存：
//   - 依赖缓存：流水线级配置，按缓存键把一个 PVC（或沙箱用的 hostPath）的子目录
//     挂载到 test / script 阶段容器的缓存路径（Go 模块、npm、Maven 仓库……），
//     同一流水线的多次运行复用同一卷。缓存键里只含分支等静态变量时直接用
//     subPath 挂载；含 {{hash:文件}} 时由 cache-prepare 初始化容器在克隆后计算
//     键值，步骤容器启动时用符号链接接入。
//   - 镜像层缓存：build 阶段给 Kaniko 加 --cache=true --cache-repo=<镜像仓库>/cache。
//   - cache-prepare 容器输出一行 XS_CACHE 标记（命中/未命中、键、占用），
//     Kaniko 输出的 "Using caching version of cmd" 等行被统计为层缓存命中；
//     两者都写入 cicd_runs.cache，供运行详情与缓存面板展示。拿不到就留空。

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

const (
	cacheMountRoot     = "/xs-cache"
	cacheKeyFile       = workDir + "/.xs-cache-key"
	cacheVolumeName    = "xs-cache"
	cachePrepareName   = "cache-prepare"
	cacheDefaultSize   = "5Gi"
	cacheDefaultKey    = "{{branch}}"
	cacheHostPathRoot  = "/var/lib/matrix-cicd/cache"
	cacheMaxPaths      = 10
	cacheMarker        = "XS_CACHE "
	cacheStoragePVC    = "pvc"
	cacheStorageHost   = "hostpath"
	cacheClearJobLimit = 300
)

// CacheConfig is the pipeline-level build cache configuration.
type CacheConfig struct {
	Enabled      bool     `json:"enabled"`                 // dependency cache on/off
	Language     string   `json:"language,omitempty"`      // go|node|npm|yarn|pnpm|maven|gradle|python|custom (default paths)
	Key          string   `json:"key,omitempty"`           // template: {{branch}} {{app}} {{pipeline}} {{hash:go.sum}}
	Paths        []string `json:"paths,omitempty"`         // absolute, ~/..., or relative to the source dir
	Storage      string   `json:"storage,omitempty"`       // pvc (default) | hostpath (sandbox only)
	StorageClass string   `json:"storage_class,omitempty"` // empty = cluster default StorageClass
	Size         string   `json:"size,omitempty"`          // PVC request, default 5Gi
	AccessMode   string   `json:"access_mode,omitempty"`   // ReadWriteOnce (default) | ReadWriteMany

	LayerCache     bool   `json:"layer_cache"`                // Kaniko --cache=true
	LayerCacheRepo string `json:"layer_cache_repo,omitempty"` // default <image repo>/cache
	LayerCacheTTL  int    `json:"layer_cache_ttl_hours,omitempty"`
}

// defaultCachePaths are the per-language dependency cache directories.
var defaultCachePaths = map[string][]string{
	"go":     {"/go/pkg/mod", "/root/.cache/go-build"},
	"node":   {"~/.npm"},
	"npm":    {"~/.npm"},
	"yarn":   {"/usr/local/share/.cache/yarn"},
	"pnpm":   {"~/.local/share/pnpm/store"},
	"maven":  {"~/.m2/repository"},
	"gradle": {"~/.gradle/caches", "~/.gradle/wrapper"},
	"python": {"~/.cache/pip"},
}

// DefaultCachePaths returns the default cache paths of a language / build tool.
func DefaultCachePaths(lang string) []string {
	return append([]string(nil), defaultCachePaths[strings.ToLower(strings.TrimSpace(lang))]...)
}

var (
	cachePathRe   = regexp.MustCompile(`^[~A-Za-z0-9._/@+-]{1,200}$`)
	cacheKeyVarRe = regexp.MustCompile(`\{\{\s*([a-z]+)(?::\s*([^}\s]+))?\s*\}\}`)
	cacheKeyLitRe = regexp.MustCompile(`^[A-Za-z0-9._-]*$`)
	cacheFileRe   = regexp.MustCompile(`^[A-Za-z0-9._@+-][A-Za-z0-9._/@+-]{0,199}$`)
	cacheClassRe  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
	cacheRepoRe   = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[a-z0-9._/-]+)+$`)
	keySanitizeRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// forbidden cache mount points (exact) and prefixes.
var cacheForbiddenExact = map[string]bool{"/": true, "/etc": true, "/bin": true, "/sbin": true, "/usr": true, "/lib": true,
	"/root": true, "/home": true, "/tmp": true, "/var": true, workDir: true, srcDir: true}
var cacheForbiddenPrefix = []string{"/proc", "/sys", "/dev", "/kaniko", cacheMountRoot, "/root/.ssh", "/root/.ssh-key", "/etc/"}

// resolveCachePath turns ~/x and relative paths into absolute mount paths.
func resolveCachePath(p string) string {
	p = strings.TrimSpace(p)
	switch {
	case p == "~":
		return "/root"
	case strings.HasPrefix(p, "~/"):
		return path.Clean("/root/" + strings.TrimPrefix(p, "~/"))
	case strings.HasPrefix(p, "/"):
		return path.Clean(p)
	default:
		return path.Clean(path.Join(srcDir, p))
	}
}

func validCachePath(p string) error {
	if !cachePathRe.MatchString(p) || strings.Contains(p, "..") || (strings.Contains(p, "~") && !strings.HasPrefix(p, "~")) {
		return httpx.Validation("cache path " + p + " is invalid")
	}
	abs := resolveCachePath(p)
	if cacheForbiddenExact[abs] {
		return httpx.Validation("cache path " + p + " cannot be used")
	}
	for _, pre := range cacheForbiddenPrefix {
		if abs == strings.TrimSuffix(pre, "/") || strings.HasPrefix(abs, strings.TrimSuffix(pre, "/")+"/") {
			return httpx.Validation("cache path " + p + " cannot be used")
		}
	}
	return nil
}

// normalize fills defaults and validates. It never mutates a nil config.
func (c *CacheConfig) normalize() error {
	if c == nil {
		return nil
	}
	c.Language = strings.ToLower(strings.TrimSpace(c.Language))
	if c.Language != "" && c.Language != "custom" {
		if _, ok := defaultCachePaths[c.Language]; !ok {
			return httpx.Validation("unknown cache language " + c.Language)
		}
	}
	c.Key = strings.TrimSpace(c.Key)
	if c.Key == "" {
		c.Key = cacheDefaultKey
	}
	if _, err := parseCacheKey(c.Key); err != nil {
		return err
	}
	paths := []string{}
	seen := map[string]bool{}
	for _, p := range c.Paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if err := validCachePath(p); err != nil {
			return err
		}
		abs := resolveCachePath(p)
		if seen[abs] {
			continue
		}
		seen[abs] = true
		paths = append(paths, p)
	}
	if len(paths) == 0 && c.Language != "" && c.Language != "custom" {
		paths = DefaultCachePaths(c.Language)
	}
	if len(paths) > cacheMaxPaths {
		return httpx.Validation(fmt.Sprintf("at most %d cache paths", cacheMaxPaths))
	}
	if c.Enabled && len(paths) == 0 {
		return httpx.Validation("dependency cache needs at least one cache path or a language")
	}
	c.Paths = paths
	c.Storage = strings.ToLower(strings.TrimSpace(c.Storage))
	switch c.Storage {
	case "":
		c.Storage = cacheStoragePVC
	case cacheStoragePVC, cacheStorageHost:
	default:
		return httpx.Validation("cache storage must be pvc or hostpath")
	}
	c.StorageClass = strings.TrimSpace(c.StorageClass)
	if c.StorageClass != "" && !cacheClassRe.MatchString(c.StorageClass) {
		return httpx.Validation("invalid storage class name")
	}
	c.Size = strings.TrimSpace(c.Size)
	if c.Size == "" {
		c.Size = cacheDefaultSize
	}
	q, err := resource.ParseQuantity(c.Size)
	if err != nil || q.Sign() <= 0 {
		return httpx.Validation("cache size must be a quantity such as 5Gi")
	}
	if q.Cmp(resource.MustParse("1Ti")) > 0 {
		return httpx.Validation("cache size must not exceed 1Ti")
	}
	switch c.AccessMode {
	case "":
		c.AccessMode = string(corev1.ReadWriteOnce)
	case string(corev1.ReadWriteOnce), string(corev1.ReadWriteMany):
	default:
		return httpx.Validation("access_mode must be ReadWriteOnce or ReadWriteMany")
	}
	if c.Storage == cacheStorageHost {
		c.StorageClass, c.AccessMode = "", ""
	}
	c.LayerCacheRepo = strings.TrimSpace(c.LayerCacheRepo)
	if c.LayerCacheRepo != "" && (!cacheRepoRe.MatchString(c.LayerCacheRepo) || inlineSecretText(c.LayerCacheRepo)) {
		return httpx.Validation("layer cache repository must look like registry.example.com/team/app/cache (no tag, no scheme)")
	}
	if c.LayerCacheTTL < 0 || c.LayerCacheTTL > 24*365 {
		return httpx.Validation("layer cache TTL must be between 0 and 8760 hours")
	}
	return nil
}

func (c *CacheConfig) depsOn() bool  { return c != nil && c.Enabled && len(c.Paths) > 0 }
func (c *CacheConfig) layerOn() bool { return c != nil && c.LayerCache }

// ---------------------------------------------------------------- cache key

type cacheKeyPart struct {
	Lit  string // literal text (already sanitized) or resolved static variable
	File string // file to hash (dynamic part)
}

// parseCacheKey splits a key template into literal / variable / file-hash parts.
func parseCacheKey(tpl string) ([]cacheKeyPart, error) {
	if len(tpl) > 200 {
		return nil, httpx.Validation("cache key template is too long")
	}
	var parts []cacheKeyPart
	rest := tpl
	for rest != "" {
		loc := cacheKeyVarRe.FindStringSubmatchIndex(rest)
		if loc == nil {
			if !cacheKeyLitRe.MatchString(rest) {
				return nil, httpx.Validation("cache key may only contain letters, digits, '.', '_', '-' and {{…}} variables")
			}
			parts = append(parts, cacheKeyPart{Lit: rest})
			break
		}
		if lit := rest[:loc[0]]; lit != "" {
			if !cacheKeyLitRe.MatchString(lit) {
				return nil, httpx.Validation("cache key may only contain letters, digits, '.', '_', '-' and {{…}} variables")
			}
			parts = append(parts, cacheKeyPart{Lit: lit})
		}
		name := rest[loc[2]:loc[3]]
		arg := ""
		if loc[4] >= 0 {
			arg = rest[loc[4]:loc[5]]
		}
		switch name {
		case "branch", "app", "pipeline":
			if arg != "" {
				return nil, httpx.Validation("cache key variable {{" + name + "}} takes no argument")
			}
			parts = append(parts, cacheKeyPart{Lit: "{{" + name + "}}"})
		case "hash", "checksum":
			if arg == "" || !cacheFileRe.MatchString(arg) || strings.Contains(arg, "..") {
				return nil, httpx.Validation("{{hash:FILE}} needs a relative file path such as go.sum")
			}
			parts = append(parts, cacheKeyPart{File: arg})
		default:
			return nil, httpx.Validation("unknown cache key variable {{" + name + "}}")
		}
		rest = rest[loc[1]:]
	}
	if len(parts) == 0 {
		return nil, httpx.Validation("cache key template is empty")
	}
	return parts, nil
}

func sanitizeKey(s string) string {
	s = strings.Trim(keySanitizeRe.ReplaceAllString(s, "-"), "-.")
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// cacheKeyPlan is a key template with static variables resolved.
type cacheKeyPlan struct {
	Static  string         // full key when no file hashes are involved
	Parts   []cacheKeyPart // resolved literals + file parts (dynamic)
	Dynamic bool
}

func resolveCacheKey(tpl string, vars map[string]string) (cacheKeyPlan, error) {
	parts, err := parseCacheKey(tpl)
	if err != nil {
		return cacheKeyPlan{}, err
	}
	out := cacheKeyPlan{}
	var b strings.Builder
	for _, p := range parts {
		if p.File != "" {
			out.Dynamic = true
			out.Parts = append(out.Parts, p)
			continue
		}
		lit := p.Lit
		if strings.HasPrefix(lit, "{{") {
			lit = sanitizeKey(vars[strings.Trim(lit, "{}")])
			if lit == "" {
				lit = "none"
			}
		}
		out.Parts = append(out.Parts, cacheKeyPart{Lit: lit})
		b.WriteString(lit)
	}
	if !out.Dynamic {
		out.Static = finalKey(b.String())
	}
	return out, nil
}

// finalKey keeps a key a safe single directory name.
func finalKey(k string) string {
	k = sanitizeKey(strings.ReplaceAll(k, "/", "-"))
	if len(k) > 100 {
		sum := sha1.Sum([]byte(k))
		k = k[:80] + "-" + hex.EncodeToString(sum[:])[:12]
	}
	if k == "" {
		k = "default"
	}
	return k
}

// shellKeyExpr renders the shell assignment that computes a dynamic key. All
// parts were validated by parseCacheKey (no quotes, spaces or $), so they are
// safe inside double quotes.
func (p cacheKeyPlan) shellKeyExpr() string {
	var b strings.Builder
	b.WriteString(`KEY="`)
	for _, part := range p.Parts {
		if part.File != "" {
			b.WriteString(`$(h ` + part.File + `)`)
		} else {
			b.WriteString(part.Lit)
		}
	}
	b.WriteString(`"`)
	return b.String()
}

// ---------------------------------------------------------------- pod wiring

// cacheSlug is the sub-directory name of one cache path inside a key directory.
func cacheSlug(p string) string {
	abs := resolveCachePath(p)
	s := sanitizeKey(strings.ReplaceAll(strings.TrimPrefix(abs, "/"), "/", "-"))
	if len(s) > 40 {
		s = s[:40]
	}
	sum := sha1.Sum([]byte(abs))
	return s + "-" + hex.EncodeToString(sum[:])[:6]
}

func cachePVCName(pipelineID int64) string { return fmt.Sprintf("cicd-cache-pipeline-%d", pipelineID) }
func cacheHostPath(pipelineID int64) string {
	return fmt.Sprintf("%s/pipeline-%d", cacheHostPathRoot, pipelineID)
}

// cacheVolume is the pod volume backing the dependency cache.
func cacheVolume(cfg *CacheConfig, pipelineID int64) corev1.Volume {
	if cfg.Storage == cacheStorageHost {
		t := corev1.HostPathDirectoryOrCreate
		return corev1.Volume{Name: cacheVolumeName, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: cacheHostPath(pipelineID), Type: &t}}}
	}
	return corev1.Volume{Name: cacheVolumeName, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: cachePVCName(pipelineID)}}}
}

// cachePVC renders the PersistentVolumeClaim of a pipeline's dependency cache.
func cachePVC(cfg *CacheConfig, pipelineID int64, ns string) (*corev1.PersistentVolumeClaim, error) {
	size, err := resource.ParseQuantity(cfg.Size)
	if err != nil {
		return nil, err
	}
	mode := corev1.PersistentVolumeAccessMode(cfg.AccessMode)
	if mode == "" {
		mode = corev1.ReadWriteOnce
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: cachePVCName(pipelineID), Namespace: ns, Labels: map[string]string{
			"app.kubernetes.io/managed-by": "matrix-cicd", "matrix.cicd/pipeline": fmt.Sprint(pipelineID), "matrix.cicd/purpose": "build-cache"}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{mode},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
	if cfg.StorageClass != "" {
		sc := cfg.StorageClass
		pvc.Spec.StorageClassName = &sc
	}
	return pvc, nil
}

// cachePrepareScript runs after git-clone: computes the key, reports hit/miss
// and usage, and creates the per-path directories.
func cachePrepareScript(plan cacheKeyPlan, paths []string) string {
	slugs := make([]string, 0, len(paths))
	for _, p := range paths {
		slugs = append(slugs, cacheSlug(p))
	}
	var b strings.Builder
	b.WriteString("set -e\ncd " + srcDir + " 2>/dev/null || true\n")
	b.WriteString("h() { if [ -f \"$1\" ]; then sha256sum \"$1\" | cut -c1-16; else echo nofile; fi; }\n")
	if plan.Dynamic {
		b.WriteString(plan.shellKeyExpr() + "\n")
		b.WriteString("KEY=$(echo \"$KEY\" | cut -c1-100)\n")
		b.WriteString("[ -n \"$KEY\" ] || KEY=default\n")
	} else {
		b.WriteString("KEY=" + plan.Static + "\n")
	}
	b.WriteString("R=miss\n")
	b.WriteString("for d in " + strings.Join(slugs, " ") + "; do\n")
	b.WriteString("  if [ -d \"" + cacheMountRoot + "/$KEY/$d\" ] && [ -n \"$(ls -A \"" + cacheMountRoot + "/$KEY/$d\" 2>/dev/null)\" ]; then R=hit; fi\n")
	b.WriteString("  mkdir -p \"" + cacheMountRoot + "/$KEY/$d\"\ndone\n")
	b.WriteString("echo \"$KEY\" > " + cacheKeyFile + "\n")
	b.WriteString("U=$(timeout 20 du -sk " + cacheMountRoot + " 2>/dev/null | cut -f1 || true)\n")
	b.WriteString("echo \"" + cacheMarker + "result=$R key=$KEY usage_kb=${U}\"\n")
	return b.String()
}

// cacheLinkPreamble is prepended to shell stages when the key is dynamic.
func cacheLinkPreamble(paths []string) string {
	var b strings.Builder
	b.WriteString("XS_CACHE_KEY=$(cat " + cacheKeyFile + ")\n")
	for _, p := range paths {
		abs := resolveCachePath(p)
		target := cacheMountRoot + "/$XS_CACHE_KEY/" + cacheSlug(p)
		b.WriteString("mkdir -p \"" + path.Dir(abs) + "\" && rm -rf \"" + abs + "\" && ln -s \"" + target + "\" \"" + abs + "\"\n")
	}
	return b.String()
}

// applyDepsCache wires the dependency cache into a test/script stage pod.
func applyDepsCache(spec *corev1.PodSpec, cfg *CacheConfig, pipelineID int64, plan cacheKeyPlan) {
	if !cfg.depsOn() || len(spec.Containers) == 0 {
		return
	}
	spec.Volumes = append(spec.Volumes, cacheVolume(cfg, pipelineID))
	workspaceMount := corev1.VolumeMount{Name: "workspace", MountPath: workDir}
	prep := corev1.Container{
		Name: cachePrepareName, Image: GitImage, Command: []string{"sh", "-c", cachePrepareScript(plan, cfg.Paths)},
		VolumeMounts: []corev1.VolumeMount{workspaceMount, {Name: cacheVolumeName, MountPath: cacheMountRoot}},
	}
	spec.InitContainers = append(spec.InitContainers, prep)
	step := &spec.Containers[0]
	if plan.Dynamic {
		step.VolumeMounts = append(step.VolumeMounts, corev1.VolumeMount{Name: cacheVolumeName, MountPath: cacheMountRoot})
		if len(step.Command) == 3 && step.Command[0] == "sh" && step.Command[1] == "-c" {
			script := strings.TrimPrefix(step.Command[2], "set -e\n")
			step.Command[2] = "set -e\n" + cacheLinkPreamble(cfg.Paths) + script
		}
	} else {
		for _, p := range cfg.Paths {
			step.VolumeMounts = append(step.VolumeMounts, corev1.VolumeMount{
				Name: cacheVolumeName, MountPath: resolveCachePath(p), SubPath: plan.Static + "/" + cacheSlug(p)})
		}
	}
	step.Env = append(step.Env, corev1.EnvVar{Name: "CI_CACHE", Value: "true"})
}

// imageRepoOf strips the tag / digest of an image reference.
func imageRepoOf(img string) string {
	if i := strings.Index(img, "@"); i >= 0 {
		img = img[:i]
	}
	slash := strings.LastIndex(img, "/")
	if colon := strings.LastIndex(img, ":"); colon > slash {
		img = img[:colon]
	}
	return img
}

// layerCacheRepo is the Kaniko --cache-repo of a build stage.
func layerCacheRepo(cfg *CacheConfig, destination string) string {
	if cfg.LayerCacheRepo != "" {
		return cfg.LayerCacheRepo
	}
	return imageRepoOf(destination) + "/cache"
}

// kanikoCacheArgs returns the Kaniko flags of the layer cache; it respects
// --cache flags already present in the stage's extra args.
func kanikoCacheArgs(cfg *CacheConfig, destination string, extra []string) []string {
	if !cfg.layerOn() {
		return nil
	}
	for _, a := range extra {
		if a == "--cache" || strings.HasPrefix(a, "--cache=") || strings.HasPrefix(a, "--cache-repo") {
			return nil
		}
	}
	args := []string{"--cache=true", "--cache-repo=" + layerCacheRepo(cfg, destination)}
	if cfg.LayerCacheTTL > 0 {
		args = append(args, fmt.Sprintf("--cache-ttl=%dh", cfg.LayerCacheTTL))
	}
	return args
}

// buildDestination is the image a build stage pushes.
func buildDestination(a *Application, r *Run, s Stage) string {
	if s.Image != "" {
		return s.Image
	}
	return a.ImageRepo + ":" + r.ImageTag
}

// applyLayerCache appends the Kaniko cache flags to a build stage pod.
func applyLayerCache(spec *corev1.PodSpec, cfg *CacheConfig, destination string, extra []string) {
	if len(spec.Containers) == 0 {
		return
	}
	spec.Containers[0].Args = append(spec.Containers[0].Args, kanikoCacheArgs(cfg, destination, extra)...)
}

// ---------------------------------------------------------------- run record

// RunCache is what a run used and observed.
type RunCache struct {
	Deps  *RunDepsCache  `json:"deps,omitempty"`
	Layer *RunLayerCache `json:"layer,omitempty"`
}

// RunDepsCache is the dependency cache of a run.
type RunDepsCache struct {
	Storage    string          `json:"storage"`
	Key        string          `json:"key"`    // resolved (static) or last observed key
	Result     string          `json:"result"` // hit | miss | partial | unknown
	UsageBytes *int64          `json:"usage_bytes"`
	Stages     []RunCacheStage `json:"stages"`
}

// RunCacheStage is one stage's dependency cache observation.
type RunCacheStage struct {
	Idx    int    `json:"idx"`
	Name   string `json:"name"`
	Key    string `json:"key"`
	Result string `json:"result"` // hit | miss | unknown
}

// RunLayerCache is the Kaniko layer cache of a run.
type RunLayerCache struct {
	Repo     string `json:"repo"`
	Hits     int    `json:"hits"`
	Misses   int    `json:"misses"`
	Observed bool   `json:"observed"` // Kaniko cache lines were seen
}

func (rc *RunCache) depsResult() string {
	if rc == nil || rc.Deps == nil {
		return ""
	}
	hit, miss := 0, 0
	for _, s := range rc.Deps.Stages {
		switch s.Result {
		case "hit":
			hit++
		case "miss":
			miss++
		}
	}
	switch {
	case hit > 0 && miss == 0:
		return "hit"
	case hit > 0:
		return "partial"
	case miss > 0:
		return "miss"
	}
	return "unknown"
}

// cacheTracker collects cache observations from the log stream of a run.
type cacheTracker struct {
	mu sync.Mutex
	rc RunCache
}

var cacheMarkerRe = regexp.MustCompile(`XS_CACHE result=(hit|miss) key=([A-Za-z0-9._-]*) usage_kb=([0-9]*)`)

// observe inspects one log line of stage idx and returns the (possibly
// rewritten) line to log.
func (t *cacheTracker) observe(idx int, name, stageType, line string) string {
	if t == nil {
		return line
	}
	if m := cacheMarkerRe.FindStringSubmatch(line); m != nil && t.rc.Deps != nil {
		t.mu.Lock()
		defer t.mu.Unlock()
		found := false
		for i := range t.rc.Deps.Stages {
			if t.rc.Deps.Stages[i].Idx == idx {
				t.rc.Deps.Stages[i].Result, t.rc.Deps.Stages[i].Key = m[1], m[2]
				found = true
			}
		}
		if !found {
			t.rc.Deps.Stages = append(t.rc.Deps.Stages, RunCacheStage{Idx: idx, Name: name, Key: m[2], Result: m[1]})
		}
		t.rc.Deps.Key = m[2]
		if kb, err := strconv.ParseInt(m[3], 10, 64); err == nil && m[3] != "" {
			b := kb * 1024
			t.rc.Deps.UsageBytes = &b
		}
		t.rc.Deps.Result = t.rc.depsResult()
		word := map[string]string{"hit": "命中", "miss": "未命中（本次运行后写入）"}[m[1]]
		return "依赖缓存" + word + "，缓存键 " + m[2]
	}
	if stageType == "build" && t.rc.Layer != nil {
		switch {
		case strings.Contains(line, "Using caching version of cmd"):
			t.mu.Lock()
			t.rc.Layer.Hits++
			t.rc.Layer.Observed = true
			t.mu.Unlock()
		case strings.Contains(line, "No cached layer found for cmd"):
			t.mu.Lock()
			t.rc.Layer.Misses++
			t.rc.Layer.Observed = true
			t.mu.Unlock()
		}
	}
	return line
}

func (t *cacheTracker) snapshot() *RunCache {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	raw, _ := json.Marshal(t.rc)
	var out RunCache
	_ = json.Unmarshal(raw, &out)
	return &out
}

// newCacheTracker builds the initial run record for the pipeline's stages.
func newCacheTracker(cfg *CacheConfig, pl *Pipeline, a *Application, r *Run, plan cacheKeyPlan) *cacheTracker {
	if cfg == nil || (!cfg.depsOn() && !cfg.layerOn()) {
		return nil
	}
	t := &cacheTracker{}
	if cfg.depsOn() {
		d := &RunDepsCache{Storage: cfg.Storage, Key: plan.Static, Result: "unknown", Stages: []RunCacheStage{}}
		for i, s := range pl.Stages {
			if s.Type == "test" || s.Type == "script" {
				d.Stages = append(d.Stages, RunCacheStage{Idx: i, Name: s.Name, Key: plan.Static, Result: "unknown"})
			}
		}
		if len(d.Stages) > 0 {
			t.rc.Deps = d
		}
	}
	if cfg.layerOn() {
		for _, s := range pl.Stages {
			if s.Type == "build" {
				t.rc.Layer = &RunLayerCache{Repo: redactCICDText(layerCacheRepo(cfg, buildDestination(a, r, s)))}
				break
			}
		}
	}
	if t.rc.Deps == nil && t.rc.Layer == nil {
		return nil
	}
	return t
}

func (m *Module) saveRunCache(runID int64, rc *RunCache) {
	if rc == nil || m.db == nil {
		return
	}
	raw, err := json.Marshal(rc)
	if err != nil {
		return
	}
	if _, err := m.db.Exec(context.Background(), `UPDATE cicd_runs SET cache=$2 WHERE id=$1`, runID, raw); err != nil {
		m.log.Error("save run cache", "run", runID, "err", err)
	}
}

// ensureCacheStorage creates the pipeline's cache PVC when it does not exist.
func ensureCacheStorage(ctx context.Context, cs kubeIface, ns string, cfg *CacheConfig, pipelineID int64) error {
	if !cfg.depsOn() || cfg.Storage != cacheStoragePVC {
		return nil
	}
	pvcs := cs.CoreV1().PersistentVolumeClaims(ns)
	if _, err := pvcs.Get(ctx, cachePVCName(pipelineID), metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read cache volume: %w", err)
	}
	pvc, err := cachePVC(cfg, pipelineID, ns)
	if err != nil {
		return err
	}
	if _, err := pvcs.Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create cache volume: %w", err)
	}
	return nil
}

// cacheRunPlan prepares the cache of a run; nil when cache is not configured.
type cacheRunPlan struct {
	cfg     *CacheConfig
	key     cacheKeyPlan
	tracker *cacheTracker
}

func (m *Module) prepareRunCache(ctx context.Context, cs kubeIface, ns string, pl *Pipeline, a *Application, r *Run, lw *logWriter) (*cacheRunPlan, error) {
	cfg := pl.Cache
	if cfg == nil || (!cfg.Enabled && !cfg.LayerCache) {
		return nil, nil
	}
	c := *cfg
	if err := c.normalize(); err != nil {
		return nil, fmt.Errorf("cache configuration: %w", err)
	}
	key, err := resolveCacheKey(c.Key, map[string]string{"branch": r.Branch, "app": a.Name, "pipeline": fmt.Sprint(pl.ID)})
	if err != nil {
		return nil, fmt.Errorf("cache key: %w", err)
	}
	plan := &cacheRunPlan{cfg: &c, key: key}
	plan.tracker = newCacheTracker(&c, pl, a, r, key)
	if plan.tracker != nil && plan.tracker.rc.Deps != nil {
		if err := ensureCacheStorage(ctx, cs, ns, &c, pl.ID); err != nil {
			// The run continues without the dependency cache rather than failing.
			lw.add(-1, "依赖缓存不可用，本次运行不使用缓存："+redactCICDText(err.Error()))
			plan.tracker.rc.Deps = nil
			c.Enabled = false
		}
	}
	if plan.tracker != nil {
		m.saveRunCache(r.ID, plan.tracker.snapshot())
	}
	return plan, nil
}

// apply wires the cache into one stage's pod spec.
func (p *cacheRunPlan) apply(spec *corev1.PodSpec, pipelineID int64, a *Application, r *Run, s Stage) {
	if p == nil {
		return
	}
	switch s.Type {
	case "test", "script":
		if p.tracker != nil && p.tracker.rc.Deps != nil {
			applyDepsCache(spec, p.cfg, pipelineID, p.key)
		}
	case "build":
		applyLayerCache(spec, p.cfg, buildDestination(a, r, s), s.ExtraArgs)
	}
}

// wrapLog lets the tracker see each log line before the (possibly withheld) logger.
func (p *cacheRunPlan) wrapLog(idx int, s Stage, inner func(string, ...any)) func(string, ...any) {
	if p == nil || p.tracker == nil {
		return inner
	}
	return func(f string, args ...any) {
		line := fmt.Sprintf(f, args...)
		out := p.tracker.observe(idx, s.Name, s.Type, line)
		inner("%s", out)
	}
}

// ---------------------------------------------------------------- HTTP

// CacheStatus is the response of GET /cicd/pipelines/:id/cache.
type CacheStatus struct {
	Config         CacheConfig    `json:"config"`
	Configured     bool           `json:"configured"`
	ResolvedPaths  []string       `json:"resolved_paths"`
	Storage        *CacheStorage  `json:"storage"`
	UsageBytes     *int64         `json:"usage_bytes"`
	UsageAt        *time.Time     `json:"usage_at"`
	LastRun        *CacheRunBrief `json:"last_run"`
	LastHit        *CacheRunBrief `json:"last_hit"`
	LastLayerHit   *CacheRunBrief `json:"last_layer_hit"`
	LayerCacheRepo string         `json:"layer_cache_repo"`
}

// CacheStorage is the live state of the cache volume (best effort).
type CacheStorage struct {
	Type         string `json:"type"` // pvc | hostpath
	Name         string `json:"name"`
	Exists       *bool  `json:"exists"`
	Phase        string `json:"phase"`
	Capacity     string `json:"capacity"`
	StorageClass string `json:"storage_class"`
	Error        string `json:"error,omitempty"`
}

// CacheRunBrief is a run that used the cache.
type CacheRunBrief struct {
	RunID  int64     `json:"run_id"`
	At     time.Time `json:"at"`
	Result string    `json:"result"`
	Key    string    `json:"key"`
	Hits   int       `json:"hits,omitempty"`
}

func (m *Module) getPipelineCache(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	pl, err := m.loadPipe(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	out := CacheStatus{ResolvedPaths: []string{}}
	if pl.Cache != nil {
		out.Config = *pl.Cache
		out.Configured = true
	}
	cfg := out.Config
	if err := cfg.normalize(); err == nil {
		out.Config = cfg
	}
	for _, p := range out.Config.Paths {
		out.ResolvedPaths = append(out.ResolvedPaths, resolveCachePath(p))
	}
	if a, err := m.loadApp(c, pl.AppID); err == nil && out.Config.LayerCache {
		out.LayerCacheRepo = redactCICDText(layerCacheRepo(&out.Config, a.ImageRepo))
	}
	if err := m.cacheRunHistory(c, id, &out); err != nil {
		httpx.Fail(c, err)
		return
	}
	if out.Configured && out.Config.Enabled {
		out.Storage = m.cacheStorageState(c.Request.Context(), pl, &out.Config)
	}
	httpx.OK(c, out)
}

func (m *Module) cacheRunHistory(ctx context.Context, pipelineID int64, out *CacheStatus) error {
	rows, err := m.db.Query(ctx, `SELECT id, COALESCE(started_at, created_at), cache FROM cicd_runs
		WHERE pipeline_id=$1 AND cache IS NOT NULL ORDER BY id DESC LIMIT 50`, pipelineID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var at time.Time
		var raw []byte
		if err := rows.Scan(&id, &at, &raw); err != nil {
			return err
		}
		var rc RunCache
		if json.Unmarshal(raw, &rc) != nil {
			continue
		}
		if rc.Deps != nil {
			brief := &CacheRunBrief{RunID: id, At: at, Result: rc.Deps.Result, Key: rc.Deps.Key}
			if out.LastRun == nil {
				out.LastRun = brief
			}
			if out.LastHit == nil && (rc.Deps.Result == "hit" || rc.Deps.Result == "partial") {
				out.LastHit = brief
			}
			if out.UsageBytes == nil && rc.Deps.UsageBytes != nil {
				v, t := *rc.Deps.UsageBytes, at
				out.UsageBytes, out.UsageAt = &v, &t
			}
		}
		if rc.Layer != nil && out.LastLayerHit == nil && rc.Layer.Hits > 0 {
			out.LastLayerHit = &CacheRunBrief{RunID: id, At: at, Result: "hit", Hits: rc.Layer.Hits}
		}
	}
	return rows.Err()
}

func (m *Module) cacheStorageState(ctx context.Context, pl *Pipeline, cfg *CacheConfig) *CacheStorage {
	st := &CacheStorage{Type: cfg.Storage}
	if cfg.Storage == cacheStorageHost {
		st.Name = cacheHostPath(pl.ID)
		return st // a host directory cannot be inspected from the server
	}
	st.Name = cachePVCName(pl.ID)
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	conn, err := m.targetClient(ctx, pl.RunnerTargetID)
	if err != nil {
		st.Error = "执行集群暂不可用"
		return st
	}
	pvc, err := conn.cs.CoreV1().PersistentVolumeClaims(conn.namespace).Get(ctx, st.Name, metav1.GetOptions{})
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		st.Error = "无法读取缓存卷状态"
		return st
	}
	st.Exists = &exists
	if exists {
		st.Phase = string(pvc.Status.Phase)
		if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
			st.Capacity = q.String()
		} else if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			st.Capacity = q.String()
		}
		if pvc.Spec.StorageClassName != nil {
			st.StorageClass = *pvc.Spec.StorageClassName
		}
	}
	return st
}

func (m *Module) pipelineCacheEnvs(ctx context.Context, pl *Pipeline) ([]string, error) {
	env, err := m.targetConfigEnv(ctx, pl.RunnerTargetID)
	if err != nil {
		return nil, httpx.Conflict("pipeline runner target is unavailable")
	}
	return []string{env}, nil
}

func (m *Module) updatePipelineCache(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	pl, err := m.loadPipe(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var in CacheConfig
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.normalize(); err != nil {
		httpx.Fail(c, err)
		return
	}
	envs, err := m.pipelineCacheEnvs(c, pl)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	change := cicdConfigMutation{Kind: "pipeline.cache", ID: id, Pipeline: &Pipeline{ID: id, AppID: pl.AppID, Cache: &in},
		Envs: envs, ExpectedUpdatedAt: &pl.UpdatedAt}
	if m.gateConfigMutation(c, change, nil) {
		return
	}
	if _, err := m.applyCICDConfigMutation(c, change); err != nil {
		httpx.Fail(c, httpx.Conflict("pipeline changed, reload and retry").Wrap(err))
		return
	}
	audit.SetResource(c, "cicd_pipeline", fmt.Sprint(id))
	audit.SetDetail(c, gin.H{"cache_enabled": in.Enabled, "layer_cache": in.LayerCache, "storage": in.Storage, "paths": len(in.Paths)})
	m.getPipelineCache(c)
}

type cacheClearIn struct {
	Confirm string `json:"confirm"`
}

func (m *Module) clearPipelineCache(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	pl, err := m.loadPipe(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var in cacheClearIn
	if !httpx.Bind(c, &in) {
		return
	}
	if strings.TrimSpace(in.Confirm) != pl.Name {
		httpx.Fail(c, httpx.Validation("confirmation does not match the pipeline name"))
		return
	}
	if pl.Cache == nil {
		httpx.Fail(c, httpx.Conflict("pipeline has no dependency cache"))
		return
	}
	if err := m.cacheIdle(c, id); err != nil {
		httpx.Fail(c, err)
		return
	}
	envs, err := m.pipelineCacheEnvs(c, pl)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	change := cicdConfigMutation{Kind: "pipeline.cache.clear", ID: id, Envs: envs, ExpectedUpdatedAt: &pl.UpdatedAt}
	if m.gateConfigMutation(c, change, nil) {
		return
	}
	if _, err := m.applyCICDConfigMutation(c, change); err != nil {
		var he *httpx.Error
		if errors.As(err, &he) {
			httpx.Fail(c, he)
			return
		}
		httpx.Fail(c, httpx.Conflict("cache could not be cleared").Wrap(err))
		return
	}
	audit.SetResource(c, "cicd_pipeline", fmt.Sprint(id))
	audit.SetDetail(c, gin.H{"action": "clear_cache", "storage": pl.Cache.Storage})
	httpx.OK(c, gin.H{"cleared": id})
}

func (m *Module) cacheIdle(ctx context.Context, pipelineID int64) error {
	var n int
	if err := m.db.QueryRow(ctx, `SELECT count(*) FROM cicd_runs WHERE pipeline_id=$1 AND status IN ('queued','running','cancelling')`, pipelineID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return httpx.Conflict("pipeline has active runs; wait for them to finish before clearing the cache")
	}
	return nil
}

// clearCache deletes the cache PVC, or empties the sandbox hostPath with a Job.
func (m *Module) clearCache(ctx context.Context, pipelineID int64) error {
	pl, err := m.loadPipe(ctx, pipelineID)
	if err != nil {
		return err
	}
	if pl.Cache == nil {
		return httpx.Conflict("pipeline has no dependency cache")
	}
	if err := m.cacheIdle(ctx, pipelineID); err != nil {
		return err
	}
	cfg := *pl.Cache
	_ = cfg.normalize()
	conn, err := m.targetClient(ctx, pl.RunnerTargetID)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, (cacheClearJobLimit+30)*time.Second)
	defer cancel()
	if cfg.Storage == cacheStorageHost {
		job := fmt.Sprintf("cicd-cache-clean-%d", pipelineID)
		spec := cacheClearPod(pipelineID)
		return runJob(cctx, conn.cs, conn.namespace, job, 0, spec, cacheClearJobLimit, func(string, ...any) {})
	}
	err = conn.cs.CoreV1().PersistentVolumeClaims(conn.namespace).Delete(cctx, cachePVCName(pipelineID), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete cache volume: %w", err)
	}
	return nil
}

// cacheClearPod empties the hostPath cache directory of a pipeline.
func cacheClearPod(pipelineID int64) corev1.PodSpec {
	cfg := &CacheConfig{Storage: cacheStorageHost}
	return corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		Volumes:       []corev1.Volume{cacheVolume(cfg, pipelineID)},
		Containers: []corev1.Container{{
			Name: "clean", Image: GitImage, Command: []string{"sh", "-c", "find " + cacheMountRoot + " -mindepth 1 -maxdepth 1 -exec rm -rf {} + && echo cleaned"},
			VolumeMounts: []corev1.VolumeMount{{Name: cacheVolumeName, MountPath: cacheMountRoot}},
		}},
	}
}
