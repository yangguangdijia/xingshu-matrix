package cicd

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func snapOf(files map[string]string) *repoSnapshot {
	s := &repoSnapshot{Files: map[string]bool{}, Contents: map[string]string{}}
	for f, c := range files {
		s.Files[f] = true
		if c != "" {
			s.Contents[f] = c
		}
	}
	return s
}

func stageByName(t *testing.T, tpl *DetectTemplate, name string) Stage {
	t.Helper()
	for _, s := range tpl.Stages {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("stage %s missing in %+v", name, tpl.Stages)
	return Stage{}
}

func TestDetectLanguageRules(t *testing.T) {
	cases := []struct {
		name               string
		files              map[string]string
		lang, tool, mode   string
		testImage          string
		cachePath          string
		dockerfileTemplate string
	}{
		{name: "go", files: map[string]string{"go.mod": "module x\n\ngo 1.22.3\n", "go.sum": "", "main.go": ""},
			lang: "go", tool: "go", mode: "template", testImage: "golang:1.22", cachePath: "/go/pkg/mod", dockerfileTemplate: "go:1.22"},
		{name: "go toolchain", files: map[string]string{"go.mod": "module x\ngo 1.21\ntoolchain go1.23.2\n", "Dockerfile": ""},
			lang: "go", tool: "go", mode: "dockerfile", testImage: "golang:1.23", cachePath: "/root/.cache/go-build"},
		{name: "node npm", files: map[string]string{"package.json": `{"scripts":{"test":"jest","build":"vite build"}}`, "package-lock.json": "", ".nvmrc": "v20.11.0"},
			lang: "node", tool: "npm", mode: "template", testImage: "node:20", cachePath: "~/.npm", dockerfileTemplate: "node:20:npm"},
		{name: "node yarn", files: map[string]string{"package.json": `{"engines":{"node":">=18"}}`, "yarn.lock": ""},
			lang: "node", tool: "yarn", mode: "template", testImage: "node:18", cachePath: "/usr/local/share/.cache/yarn", dockerfileTemplate: "node:18:yarn"},
		{name: "node pnpm", files: map[string]string{"package.json": `{}`, "pnpm-lock.yaml": "", "Dockerfile": ""},
			lang: "node", tool: "pnpm", mode: "dockerfile", testImage: "node:20", cachePath: "~/.local/share/pnpm/store"},
		{name: "node packageManager without lockfile", files: map[string]string{"package.json": `{"packageManager":"pnpm@9.1.0"}`},
			lang: "node", tool: "pnpm", mode: "template", testImage: "node:20", dockerfileTemplate: "node:20:pnpm"},
		{name: "node no lockfile", files: map[string]string{"package.json": `{}`},
			lang: "node", tool: "npm", mode: "template", testImage: "node:20", dockerfileTemplate: "node:20:npm-install"},
		{name: "maven", files: map[string]string{"pom.xml": "<properties><java.version>21</java.version></properties>"},
			lang: "java", tool: "maven", mode: "template", testImage: "maven:3.9-eclipse-temurin-21", cachePath: "~/.m2/repository", dockerfileTemplate: "maven:21"},
		{name: "gradle kts", files: map[string]string{"build.gradle.kts": "java { toolchain { languageVersion = JavaLanguageVersion.of(17) } }", "gradlew": ""},
			lang: "java", tool: "gradle", mode: "template", testImage: "gradle:8.10-jdk17", cachePath: "~/.gradle/caches", dockerfileTemplate: "gradle:17"},
		{name: "python requirements", files: map[string]string{"requirements.txt": "flask\npytest\n", ".python-version": "3.11.4"},
			lang: "python", tool: "pip", mode: "template", testImage: "python:3.11-slim", cachePath: "~/.cache/pip", dockerfileTemplate: "python:3.11:requirements"},
		{name: "python poetry", files: map[string]string{"pyproject.toml": "[tool.poetry]\nname='x'\n"},
			lang: "python", tool: "poetry", mode: "template", testImage: "python:3.12-slim", dockerfileTemplate: "python:3.12:pyproject"},
		{name: "dockerfile only", files: map[string]string{"Dockerfile": "", "index.html": ""},
			lang: "dockerfile", tool: "dockerfile", mode: "dockerfile"},
		{name: "nothing", files: map[string]string{"README.md": ""}, lang: "unknown", tool: "", mode: "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := DetectLanguage(snapOf(tc.files), ".", "")
			if res.Language != tc.lang || res.BuildTool != tc.tool || res.BuildMode != tc.mode {
				t.Fatalf("got lang=%s tool=%s mode=%s, want %s %s %s", res.Language, res.BuildTool, res.BuildMode, tc.lang, tc.tool, tc.mode)
			}
			if tc.mode == "none" {
				if res.Template != nil {
					t.Fatalf("no template expected, got %+v", res.Template)
				}
				return
			}
			build := stageByName(t, res.Template, "build")
			if build.DockerfileTemplate != tc.dockerfileTemplate {
				t.Fatalf("dockerfile_template=%q want %q", build.DockerfileTemplate, tc.dockerfileTemplate)
			}
			if tc.dockerfileTemplate != "" {
				if _, err := renderDockerfileTemplate(tc.dockerfileTemplate); err != nil {
					t.Fatalf("template %s does not render: %v", tc.dockerfileTemplate, err)
				}
			}
			if tc.testImage != "" {
				if test := stageByName(t, res.Template, "test"); test.Image != tc.testImage || len(test.Commands) == 0 {
					t.Fatalf("test stage %+v, want image %s", test, tc.testImage)
				}
			}
			if res.Template.Cache == nil || !res.Template.Cache.LayerCache {
				t.Fatalf("layer cache should be suggested: %+v", res.Template.Cache)
			}
			if tc.cachePath != "" {
				found := false
				for _, p := range res.Template.Cache.Paths {
					found = found || p == tc.cachePath
				}
				if !found || !res.Template.Cache.Enabled {
					t.Fatalf("cache paths %v missing %s", res.Template.Cache.Paths, tc.cachePath)
				}
			}
		})
	}
}

func TestDetectDockerfilePriorityAndContextDir(t *testing.T) {
	snap := snapOf(map[string]string{"svc/go.mod": "module x\ngo 1.23\n", "svc/Dockerfile": "", "web/package.json": "{}"})
	res := DetectLanguage(snap, "svc", "")
	if res.Language != "go" || !res.HasDockerfile || res.Dockerfile != "svc/Dockerfile" || res.BuildMode != "dockerfile" {
		t.Fatalf("context dir detection: %+v", res)
	}
	res = DetectLanguage(snap, "web", "")
	if res.Language != "node" || res.HasDockerfile || res.Dockerfile != "web/Dockerfile" {
		t.Fatalf("web context: %+v", res)
	}
	// Configured Dockerfile path wins.
	snap = snapOf(map[string]string{"go.mod": "go 1.23", "deploy/app.Dockerfile": "", "Dockerfile": ""})
	if res := DetectLanguage(snap, ".", "deploy/app.Dockerfile"); res.Dockerfile != "deploy/app.Dockerfile" {
		t.Fatalf("configured dockerfile: %+v", res)
	}
	// Go takes precedence over package.json tooling; the other language is reported.
	snap = snapOf(map[string]string{"go.mod": "go 1.23", "package.json": "{}"})
	if res := DetectLanguage(snap, ".", ""); res.Language != "go" || len(res.Others) != 1 {
		t.Fatalf("priority: %+v", res)
	}
}

func TestNodeCommandsUseScripts(t *testing.T) {
	snap := snapOf(map[string]string{"package.json": `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1","build":"tsc"}}`, "yarn.lock": ""})
	res := DetectLanguage(snap, ".", "")
	cmds := strings.Join(stageByName(t, res.Template, "test").Commands, "|")
	if cmds != "corepack enable|yarn install --frozen-lockfile|yarn run build" {
		t.Fatalf("commands = %s", cmds)
	}
}

func TestParseDetectOutput(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("module x\ngo 1.22\n"))
	lines := []string{"--- [git-clone] ---", "checked out abc", "--- [step] ---",
		"XSDETECT H 0123456789abcdef", "XSDETECT F go.mod", "XSDETECT F cmd/main.go", "XSDETECT C go.mod " + enc, "XSDETECT END"}
	snap, done := parseDetectOutput(lines)
	if !done || !snap.Files["cmd/main.go"] || snap.Contents["go.mod"] != "module x\ngo 1.22\n" || snap.Commit != "0123456789abcdef" {
		t.Fatalf("parse: done=%v %+v", done, snap)
	}
	if _, done := parseDetectOutput(lines[:5]); done {
		t.Fatal("missing END must be reported")
	}
	script := detectScript("svc")
	if !strings.Contains(script, "svc/go.mod") || !strings.Contains(script, "XSDETECT END") || strings.Contains(script, "..") {
		t.Fatalf("detect script: %s", script)
	}
}

func TestDockerfileTemplateValidationAndInit(t *testing.T) {
	for _, bad := range []string{"", "rust:1.80", "go", "node:20:bun", "go:1.22;rm -rf /", "python:3.12:poetry"} {
		if _, err := renderDockerfileTemplate(bad); err == nil {
			t.Fatalf("template %q should be rejected", bad)
		}
	}
	df, err := renderDockerfileTemplate("go:1.22")
	if err != nil || !strings.Contains(df, "FROM golang:1.22-alpine AS build") {
		t.Fatalf("go dockerfile: %v %s", err, df)
	}
	spec := corev1.PodSpec{InitContainers: []corev1.Container{{Name: cloneContainer}}, Containers: []corev1.Container{{Name: stepContainer}}}
	if err := applyDockerfileTemplate(&spec, Stage{Type: "build", DockerfileTemplate: "node:20:pnpm"}, "/workspace/src/Dockerfile"); err != nil {
		t.Fatal(err)
	}
	if len(spec.InitContainers) != 2 || spec.InitContainers[1].Name != dockerfileInitC {
		t.Fatalf("init containers: %+v", spec.InitContainers)
	}
	env := map[string]string{}
	for _, e := range spec.InitContainers[1].Env {
		env[e.Name] = e.Value
	}
	if env["DF"] != "/workspace/src/Dockerfile" || !strings.Contains(env["DOCKERFILE"], "pnpm install --frozen-lockfile") ||
		!strings.Contains(spec.InitContainers[1].Command[2], `if [ -f "$DF" ]`) {
		t.Fatalf("dockerfile init env: %+v", env)
	}
	// Non-build stages and stages without template are untouched.
	before := len(spec.InitContainers)
	_ = applyDockerfileTemplate(&spec, Stage{Type: "test", DockerfileTemplate: "go:1.22"}, "x")
	_ = applyDockerfileTemplate(&spec, Stage{Type: "build"}, "x")
	if len(spec.InitContainers) != before {
		t.Fatal("unexpected init container")
	}
	a := &Application{ImageRepo: "r/app"}
	if err := validateStages([]Stage{{Name: "build", Type: "build", DockerfileTemplate: "cobol:1"}}, a); err == nil {
		t.Fatal("invalid dockerfile_template accepted by validateStages")
	}
}

// ---------------------------------------------------------------- cache

func TestCacheConfigNormalize(t *testing.T) {
	c := &CacheConfig{Enabled: true, Language: "go"}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Key != "{{branch}}" || c.Storage != "pvc" || c.Size != "5Gi" || c.AccessMode != "ReadWriteOnce" || len(c.Paths) != 2 {
		t.Fatalf("defaults: %+v", c)
	}
	bad := []CacheConfig{
		{Enabled: true},                                  // no paths
		{Enabled: true, Language: "cobol"},               // unknown language
		{Enabled: true, Paths: []string{"/"}},            // root
		{Enabled: true, Paths: []string{"/workspace"}},   // workspace
		{Enabled: true, Paths: []string{"/proc/x"}},      // forbidden prefix
		{Enabled: true, Paths: []string{"../etc"}},       // traversal
		{Enabled: true, Paths: []string{"/a b"}},         // space
		{Enabled: true, Paths: []string{"/kaniko/x"}},    // kaniko
		{Enabled: true, Paths: []string{"/xs-cache/a"}},  // own mount
		{Enabled: true, Paths: []string{"/root/.ssh"}},   // ssh key dir
		{Enabled: true, Language: "go", Key: "{{x}}"},    // unknown var
		{Enabled: true, Language: "go", Key: "a b"},      // bad literal
		{Enabled: true, Language: "go", Key: "{{hash}}"}, // hash without file
		{Enabled: true, Language: "go", Key: "{{hash:../go.sum}}"},
		{Enabled: true, Language: "go", Key: "{{branch:x}}"},
		{Enabled: true, Language: "go", Storage: "nfs"},
		{Enabled: true, Language: "go", Size: "lots"},
		{Enabled: true, Language: "go", Size: "2Ti"},
		{Enabled: true, Language: "go", AccessMode: "ReadOnlyMany"},
		{Enabled: true, Language: "go", StorageClass: "Bad Class"},
		{LayerCache: true, LayerCacheRepo: "https://r/x"},
		{LayerCache: true, LayerCacheRepo: "r:5000/app:tag"},
		{LayerCache: true, LayerCacheTTL: -1},
	}
	for i := range bad {
		if err := bad[i].normalize(); err == nil {
			t.Fatalf("case %d should fail: %+v", i, bad[i])
		}
	}
	ok := CacheConfig{Enabled: true, Paths: []string{"node_modules", "~/.npm", "~/.npm"}, Key: "web-{{branch}}-{{hash:package-lock.json}}",
		Storage: "hostpath", StorageClass: "ignored", LayerCache: true, LayerCacheRepo: "10.42.0.1:5000/demo/hello-web/cache"}
	if err := ok.normalize(); err != nil {
		t.Fatal(err)
	}
	if len(ok.Paths) != 2 || ok.StorageClass != "" || ok.AccessMode != "" {
		t.Fatalf("hostpath normalize: %+v", ok)
	}
	var nilCfg *CacheConfig
	if nilCfg.normalize() != nil || nilCfg.depsOn() || nilCfg.layerOn() {
		t.Fatal("nil config must be a no-op")
	}
}

func TestResolveCacheKey(t *testing.T) {
	p, err := resolveCacheKey("{{app}}-{{branch}}", map[string]string{"app": "web", "branch": "feature/Login #2"})
	if err != nil || p.Dynamic || p.Static != "web-feature-Login-2" {
		t.Fatalf("static key: %+v %v", p, err)
	}
	p, err = resolveCacheKey("deps-{{hash:go.sum}}-{{branch}}", map[string]string{"branch": "main"})
	if err != nil || !p.Dynamic || p.Static != "" {
		t.Fatalf("dynamic key: %+v %v", p, err)
	}
	if got := p.shellKeyExpr(); got != `KEY="deps-$(h go.sum)-main"` {
		t.Fatalf("shell expr = %s", got)
	}
	long := strings.Repeat("a", 150)
	if k := finalKey(long); len(k) > 100 {
		t.Fatalf("final key too long: %d", len(k))
	}
	if p, _ := resolveCacheKey("{{branch}}", map[string]string{}); p.Static != "none" {
		t.Fatalf("empty branch key = %q", p.Static)
	}
}

func TestResolveCachePathAndSlug(t *testing.T) {
	if resolveCachePath("~/.npm") != "/root/.npm" || resolveCachePath("node_modules") != "/workspace/src/node_modules" || resolveCachePath("/go/pkg/mod/") != "/go/pkg/mod" {
		t.Fatal("resolveCachePath")
	}
	a, b := cacheSlug("/go/pkg/mod"), cacheSlug("/root/.cache/go-build")
	if a == b || strings.Contains(a, "/") || !strings.HasPrefix(a, "go-pkg-mod-") {
		t.Fatalf("slugs %s %s", a, b)
	}
	if cacheSlug("~/.npm") != cacheSlug("/root/.npm") {
		t.Fatal("slug must depend on the resolved path")
	}
}

func testStagePod() corev1.PodSpec {
	a := &Application{ImageRepo: "r/app", Dockerfile: "Dockerfile", ContextDir: "."}
	return stagePod(a, &Run{ID: 7, Branch: "main", ImageTag: "run-7"}, Stage{Name: "test", Type: "test", Image: "golang:1.23", Commands: []string{"go test ./..."}}, "cicd-run-7")
}

func TestApplyDepsCacheStaticPVC(t *testing.T) {
	cfg := &CacheConfig{Enabled: true, Language: "go"}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	plan, _ := resolveCacheKey(cfg.Key, map[string]string{"branch": "main"})
	spec := testStagePod()
	applyDepsCache(&spec, cfg, 42, plan)

	var vol *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == cacheVolumeName {
			vol = &spec.Volumes[i]
		}
	}
	if vol == nil || vol.PersistentVolumeClaim == nil || vol.PersistentVolumeClaim.ClaimName != "cicd-cache-pipeline-42" {
		t.Fatalf("cache volume: %+v", spec.Volumes)
	}
	if len(spec.InitContainers) != 2 || spec.InitContainers[0].Name != cloneContainer || spec.InitContainers[1].Name != cachePrepareName {
		t.Fatalf("init order: %+v", spec.InitContainers)
	}
	prep := spec.InitContainers[1].Command[2]
	if !strings.Contains(prep, "KEY=main") || !strings.Contains(prep, "XS_CACHE result=$R") || !strings.Contains(prep, cacheSlug("/go/pkg/mod")) {
		t.Fatalf("prepare script:\n%s", prep)
	}
	mounts := map[string]string{}
	for _, m := range spec.Containers[0].VolumeMounts {
		if m.Name == cacheVolumeName {
			mounts[m.MountPath] = m.SubPath
		}
	}
	if mounts["/go/pkg/mod"] != "main/"+cacheSlug("/go/pkg/mod") || mounts["/root/.cache/go-build"] != "main/"+cacheSlug("/root/.cache/go-build") {
		t.Fatalf("step mounts: %+v", mounts)
	}
	if spec.Containers[0].Command[2] != "set -e\ngo test ./..." {
		t.Fatalf("static mode must not change the command: %q", spec.Containers[0].Command[2])
	}

	pvc, err := cachePVC(cfg, 42, "matrix-cicd")
	if err != nil || pvc.Name != "cicd-cache-pipeline-42" || pvc.Spec.StorageClassName != nil ||
		pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce || pvc.Spec.Resources.Requests.Storage().String() != "5Gi" {
		t.Fatalf("pvc: %+v %v", pvc, err)
	}
	cfg.StorageClass, cfg.Size, cfg.AccessMode = "local-path", "20Gi", "ReadWriteMany"
	pvc, _ = cachePVC(cfg, 42, "matrix-cicd")
	if *pvc.Spec.StorageClassName != "local-path" || pvc.Spec.AccessModes[0] != corev1.ReadWriteMany || pvc.Spec.Resources.Requests.Storage().String() != "20Gi" {
		t.Fatalf("pvc options: %+v", pvc.Spec)
	}
}

func TestApplyDepsCacheDynamicHostPath(t *testing.T) {
	cfg := &CacheConfig{Enabled: true, Paths: []string{"~/.npm"}, Key: "{{hash:package-lock.json}}", Storage: "hostpath"}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	plan, _ := resolveCacheKey(cfg.Key, map[string]string{"branch": "main"})
	spec := testStagePod()
	applyDepsCache(&spec, cfg, 9, plan)
	var vol *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == cacheVolumeName {
			vol = &spec.Volumes[i]
		}
	}
	if vol == nil || vol.HostPath == nil || vol.HostPath.Path != "/var/lib/matrix-cicd/cache/pipeline-9" || *vol.HostPath.Type != corev1.HostPathDirectoryOrCreate {
		t.Fatalf("hostPath volume: %+v", vol)
	}
	cmd := spec.Containers[0].Command[2]
	if !strings.HasPrefix(cmd, "set -e\nXS_CACHE_KEY=$(cat /workspace/.xs-cache-key)\n") || !strings.Contains(cmd, `ln -s "/xs-cache/$XS_CACHE_KEY/`+cacheSlug("~/.npm")+`" "/root/.npm"`) ||
		!strings.HasSuffix(cmd, "go test ./...") {
		t.Fatalf("dynamic command:\n%s", cmd)
	}
	rootMount := false
	for _, m := range spec.Containers[0].VolumeMounts {
		rootMount = rootMount || (m.Name == cacheVolumeName && m.MountPath == cacheMountRoot && m.SubPath == "")
	}
	if !rootMount {
		t.Fatal("dynamic mode mounts the cache root")
	}
	if !strings.Contains(spec.InitContainers[1].Command[2], `KEY="$(h package-lock.json)"`) {
		t.Fatalf("prepare script:\n%s", spec.InitContainers[1].Command[2])
	}
	// Disabled cache leaves the pod untouched.
	spec2 := testStagePod()
	applyDepsCache(&spec2, &CacheConfig{Enabled: false, Paths: []string{"/x"}}, 9, plan)
	if len(spec2.InitContainers) != 1 || len(spec2.Volumes) != len(testStagePod().Volumes) {
		t.Fatal("disabled cache changed the pod")
	}
}

func TestKanikoCacheArgs(t *testing.T) {
	cfg := &CacheConfig{LayerCache: true}
	got := kanikoCacheArgs(cfg, "10.42.0.1:5000/demo/hello-web:abc123", nil)
	if strings.Join(got, " ") != "--cache=true --cache-repo=10.42.0.1:5000/demo/hello-web/cache" {
		t.Fatalf("args = %v", got)
	}
	cfg.LayerCacheRepo, cfg.LayerCacheTTL = "harbor.example.com/ci/cache", 72
	got = kanikoCacheArgs(cfg, "harbor.example.com/shop/web@sha256:abcd", nil)
	if strings.Join(got, " ") != "--cache=true --cache-repo=harbor.example.com/ci/cache --cache-ttl=72h" {
		t.Fatalf("args = %v", got)
	}
	if got := kanikoCacheArgs(cfg, "r/x:1", []string{"--cache=false"}); got != nil {
		t.Fatalf("explicit --cache flag must win: %v", got)
	}
	if got := kanikoCacheArgs(&CacheConfig{}, "r/x:1", nil); got != nil {
		t.Fatalf("disabled: %v", got)
	}
	if imageRepoOf("localhost:5000/a/b:tag") != "localhost:5000/a/b" || imageRepoOf("localhost:5000/a/b") != "localhost:5000/a/b" {
		t.Fatal("imageRepoOf")
	}

	a := &Application{ImageRepo: "10.42.0.1:5000/demo/hello-web", Dockerfile: "Dockerfile", ContextDir: "."}
	r := &Run{ID: 3, ImageTag: "run-3"}
	s := Stage{Name: "build", Type: "build", ExtraArgs: []string{"--insecure"}}
	spec := stagePod(a, r, s, "cicd-run-3")
	plan := &cacheRunPlan{cfg: &CacheConfig{LayerCache: true}}
	plan.apply(&spec, 1, a, r, s)
	args := strings.Join(spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--destination=10.42.0.1:5000/demo/hello-web:run-3 --insecure --cache=true --cache-repo=10.42.0.1:5000/demo/hello-web/cache") {
		t.Fatalf("kaniko args: %s", args)
	}
}

func TestCacheTrackerObservations(t *testing.T) {
	pl := &Pipeline{ID: 1, Stages: []Stage{{Name: "test", Type: "test"}, {Name: "lint", Type: "script"}, {Name: "build", Type: "build"}}}
	a := &Application{Name: "web", ImageRepo: "r/web"}
	r := &Run{ID: 5, ImageTag: "run-5"}
	cfg := &CacheConfig{Enabled: true, Language: "go", LayerCache: true}
	_ = cfg.normalize()
	key, _ := resolveCacheKey(cfg.Key, map[string]string{"branch": "main"})
	tr := newCacheTracker(cfg, pl, a, r, key)
	if tr == nil || tr.rc.Deps == nil || len(tr.rc.Deps.Stages) != 2 || tr.rc.Layer == nil || tr.rc.Layer.Repo != "r/web/cache" {
		t.Fatalf("tracker: %+v", tr)
	}
	out := tr.observe(0, "test", "test", "XS_CACHE result=hit key=main usage_kb=2048")
	if !strings.Contains(out, "命中") {
		t.Fatalf("rewritten line = %q", out)
	}
	tr.observe(1, "lint", "script", "XS_CACHE result=miss key=main usage_kb=")
	tr.observe(2, "build", "build", "INFO[0003] Using caching version of cmd: RUN go build")
	tr.observe(2, "build", "build", "INFO[0004] No cached layer found for cmd RUN apk add")
	tr.observe(2, "build", "build", "plain line")
	snap := tr.snapshot()
	if snap.Deps.Result != "partial" || snap.Deps.UsageBytes == nil || *snap.Deps.UsageBytes != 2048*1024 || snap.Deps.Key != "main" {
		t.Fatalf("deps snapshot: %+v", snap.Deps)
	}
	if snap.Layer.Hits != 1 || snap.Layer.Misses != 1 || !snap.Layer.Observed {
		t.Fatalf("layer snapshot: %+v", snap.Layer)
	}
	// Disabled config → no tracker; no shell stage → no deps record.
	if newCacheTracker(&CacheConfig{}, pl, a, r, key) != nil {
		t.Fatal("disabled config must not track")
	}
	onlyBuild := &Pipeline{Stages: []Stage{{Name: "build", Type: "build"}}}
	if tr := newCacheTracker(&CacheConfig{Enabled: true, Paths: []string{"/x"}}, onlyBuild, a, r, key); tr != nil {
		t.Fatalf("deps cache without shell stages should not track: %+v", tr.rc)
	}
	// wrapLog forwards rewritten lines even when the inner logger withholds output.
	var got []string
	plan := &cacheRunPlan{tracker: newCacheTracker(cfg, pl, a, r, key)}
	logf := plan.wrapLog(0, pl.Stages[0], func(f string, args ...any) { got = append(got, f) })
	logf("%s", "XS_CACHE result=miss key=main usage_kb=1")
	if len(got) != 1 || plan.tracker.snapshot().Deps.Stages[0].Result != "miss" {
		t.Fatalf("wrapLog: %v", got)
	}
	var nilPlan *cacheRunPlan
	inner := func(string, ...any) {}
	if nilPlan.wrapLog(0, Stage{}, inner) == nil {
		t.Fatal("nil plan wrapLog")
	}
	nilPlan.apply(&corev1.PodSpec{}, 1, a, r, Stage{Type: "test"})
}

func TestCacheClearPodAndPrepareScript(t *testing.T) {
	spec := cacheClearPod(4)
	if spec.Volumes[0].HostPath == nil || spec.Volumes[0].HostPath.Path != "/var/lib/matrix-cicd/cache/pipeline-4" ||
		!strings.Contains(spec.Containers[0].Command[2], "-mindepth 1") {
		t.Fatalf("clear pod: %+v", spec)
	}
	script := cachePrepareScript(cacheKeyPlan{Static: "main"}, []string{"/go/pkg/mod"})
	for _, want := range []string{"KEY=main", "mkdir -p", "timeout 20 du -sk /xs-cache", "/workspace/.xs-cache-key"} {
		if !strings.Contains(script, want) {
			t.Fatalf("prepare script missing %q:\n%s", want, script)
		}
	}
}

func TestEnsureCacheStorageCreatesPVCOnce(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cfg := &CacheConfig{Enabled: true, Language: "maven", StorageClass: "local-path"}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ { // second call finds the existing claim
		if err := ensureCacheStorage(ctx, cs, "matrix-cicd", cfg, 11); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := cs.CoreV1().PersistentVolumeClaims("matrix-cicd").List(ctx, metav1.ListOptions{})
	if len(list.Items) != 1 || list.Items[0].Name != "cicd-cache-pipeline-11" || list.Items[0].Labels["matrix.cicd/pipeline"] != "11" {
		t.Fatalf("pvcs: %+v", list.Items)
	}
	host := &CacheConfig{Enabled: true, Language: "go", Storage: "hostpath"}
	_ = host.normalize()
	if err := ensureCacheStorage(ctx, cs, "matrix-cicd", host, 12); err != nil {
		t.Fatal(err)
	}
	list, _ = cs.CoreV1().PersistentVolumeClaims("matrix-cicd").List(ctx, metav1.ListOptions{})
	if len(list.Items) != 1 {
		t.Fatal("hostPath storage must not create a claim")
	}
}
