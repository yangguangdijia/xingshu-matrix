package cicd

// 从源码创建时自动识别语言：在执行集群里用与流水线相同的 git-clone 初始化容器
// （同一套凭证注入：临时 Secret 中的克隆地址 / SSH 私钥）浅克隆仓库，列出浅层文件
// 并读取少量标志文件，按规则识别语言与构建工具，返回建议的构建模板（阶段、镜像、
// 命令、缓存路径）。没有 Dockerfile 时，构建阶段带 dockerfile_template，运行时由
// 初始化容器写入内置的多阶段 Dockerfile（仓库里已有同名文件则不覆盖）。

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const (
	detectJobLimit  = 110
	detectMaxFiles  = 3000
	detectMaxBytes  = 65536
	detectMarker    = "XSDETECT "
	dockerfileInitC = "dockerfile-template"
)

// detectContentFiles are read (first 64 KiB) to refine the detection.
var detectContentFiles = []string{"go.mod", "package.json", "pom.xml", "build.gradle", "build.gradle.kts",
	"requirements.txt", "pyproject.toml", ".nvmrc", ".node-version", ".python-version", "runtime.txt"}

// DetectResult is the response of POST /cicd/repos/detect.
type DetectResult struct {
	Language      string          `json:"language"`   // go | node | java | python | dockerfile | unknown
	BuildTool     string          `json:"build_tool"` // go | npm | yarn | pnpm | maven | gradle | pip | poetry | dockerfile | ""
	Version       string          `json:"version"`    // language / JDK version used for images
	HasDockerfile bool            `json:"has_dockerfile"`
	Dockerfile    string          `json:"dockerfile"`  // repo-relative path to use
	ContextDir    string          `json:"context_dir"` // as requested
	BuildMode     string          `json:"build_mode"`  // dockerfile | template | none
	Evidence      []string        `json:"evidence"`    // marker files found
	Others        []string        `json:"others"`      // other languages also present
	Notes         []string        `json:"notes"`
	Template      *DetectTemplate `json:"template"`
	Branch        string          `json:"branch"`
	Commit        string          `json:"commit,omitempty"`
}

// DetectTemplate is the suggested default pipeline (without deploy stages).
type DetectTemplate struct {
	Stages []Stage      `json:"stages"`
	Cache  *CacheConfig `json:"cache"`
}

// repoSnapshot is what the detect Job reports about the repository.
type repoSnapshot struct {
	Files    map[string]bool   // repo-relative paths (shallow listing)
	Contents map[string]string // path → first bytes
	Commit   string
}

// sub returns a view of the snapshot rooted at dir.
func (s *repoSnapshot) sub(dir string) *repoSnapshot {
	dir = strings.Trim(path.Clean("/"+dir), "/")
	if dir == "" || dir == "." {
		return s
	}
	out := &repoSnapshot{Files: map[string]bool{}, Contents: map[string]string{}, Commit: s.Commit}
	for f := range s.Files {
		if rest, ok := strings.CutPrefix(f, dir+"/"); ok {
			out.Files[rest] = true
		}
	}
	for f, c := range s.Contents {
		if rest, ok := strings.CutPrefix(f, dir+"/"); ok {
			out.Contents[rest] = c
		}
	}
	return out
}

func (s *repoSnapshot) has(names ...string) bool {
	for _, n := range names {
		if s.Files[n] {
			return true
		}
	}
	return false
}

func (s *repoSnapshot) hasPrefix(prefix string) bool {
	for f := range s.Files {
		if strings.HasPrefix(f, prefix) {
			return true
		}
	}
	return false
}

var (
	goVersionRe      = regexp.MustCompile(`(?m)^go\s+([0-9]+\.[0-9]+)`)
	goToolchainRe    = regexp.MustCompile(`(?m)^toolchain\s+go([0-9]+\.[0-9]+)`)
	majorRe          = regexp.MustCompile(`([0-9]+)`)
	pyVersionRe      = regexp.MustCompile(`([0-9]+\.[0-9]+)`)
	pomJavaRe        = regexp.MustCompile(`<(?:maven\.compiler\.release|maven\.compiler\.source|maven\.compiler\.target|java\.version|release)>\s*(?:1\.)?([0-9]+)\s*<`)
	gradleJavaRe     = regexp.MustCompile(`(?:JavaVersion\.VERSION_(?:1_)?([0-9]+)|JavaLanguageVersion\.of\(\s*([0-9]+)\s*\)|(?:source|target)Compatibility\s*=\s*['"]?(?:1\.)?([0-9]+))`)
	pyRequiresRe     = regexp.MustCompile(`requires-python\s*=\s*["'][^0-9]*([0-9]+\.[0-9]+)`)
	detectRefRe      = regexp.MustCompile(`^[A-Za-z0-9._/@+-]{1,200}$`)
	detectRelPathRe  = regexp.MustCompile(`^[A-Za-z0-9._/@+-]{1,200}$`)
	dockerTemplateRe = regexp.MustCompile(`^(go|node|maven|gradle|python):([0-9]{1,2}(?:\.[0-9]{1,2})?)(?::(npm|npm-install|yarn|pnpm|requirements|pyproject|wrapper))?$`)
)

// DetectLanguage applies the marker-file rules to a repository snapshot.
// dockerfile is the repo-relative Dockerfile path the application uses (may
// be empty) and contextDir the build context directory.
func DetectLanguage(snap *repoSnapshot, contextDir, dockerfile string) *DetectResult {
	if contextDir == "" {
		contextDir = "."
	}
	ctx := snap.sub(contextDir)
	res := &DetectResult{Language: "unknown", ContextDir: contextDir, Evidence: []string{}, Others: []string{}, Notes: []string{}, Commit: snap.Commit}

	// Dockerfile: the configured path first, then the usual names in the context dir.
	candidates := []string{}
	if dockerfile != "" {
		candidates = append(candidates, path.Clean(dockerfile))
	}
	for _, n := range []string{"Dockerfile", "dockerfile", "Containerfile"} {
		candidates = append(candidates, path.Clean(path.Join(contextDir, n)))
	}
	for _, c := range candidates {
		if snap.Files[c] {
			res.HasDockerfile, res.Dockerfile = true, c
			res.Evidence = append(res.Evidence, c)
			break
		}
	}
	if !res.HasDockerfile {
		res.Dockerfile = path.Clean(path.Join(contextDir, "Dockerfile"))
	}

	type lang struct {
		name, tool string
		markers    []string
	}
	rules := []lang{
		{"go", "go", []string{"go.mod"}},
		{"node", "npm", []string{"package.json"}},
		{"java", "maven", []string{"pom.xml"}},
		{"java", "gradle", []string{"build.gradle", "build.gradle.kts"}},
		{"python", "pip", []string{"requirements.txt", "pyproject.toml"}},
	}
	for _, r := range rules {
		found := ""
		for _, mk := range r.markers {
			if ctx.Files[mk] {
				found = mk
				break
			}
		}
		if found == "" {
			continue
		}
		if res.Language == "unknown" {
			res.Language, res.BuildTool = r.name, r.tool
			res.Evidence = append(res.Evidence, found)
			continue
		}
		label := r.name
		if r.name == "java" {
			label = r.tool
		}
		if label != res.Language && label != res.BuildTool {
			res.Others = append(res.Others, label+"("+found+")")
		}
	}

	var test *Stage
	var cache *CacheConfig
	tplSpec := ""
	switch res.Language {
	case "go":
		res.Version = goVersion(ctx.Contents["go.mod"])
		cmds := []string{}
		if ctx.has("go.sum") {
			cmds = append(cmds, "go mod download")
		}
		cmds = append(cmds, "go vet ./...", "go test ./...")
		test = &Stage{Name: "test", Type: "test", Image: "golang:" + res.Version, Commands: cmds}
		cache = cacheFor("go")
		tplSpec = "go:" + res.Version
		if !ctx.has("main.go") {
			res.Notes = append(res.Notes, "仓库根目录没有 main.go，内置构建模板假设上下文目录是 main 包；否则请在仓库中提供 Dockerfile")
		}
	case "node":
		res.BuildTool = nodeTool(ctx)
		if ctx.has("package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml") {
			res.Evidence = append(res.Evidence, firstExisting(ctx, "pnpm-lock.yaml", "yarn.lock", "package-lock.json", "npm-shrinkwrap.json"))
		}
		res.Version = nodeVersion(ctx)
		pkg := parsePackageJSON(ctx.Contents["package.json"])
		test = &Stage{Name: "test", Type: "test", Image: "node:" + res.Version, Commands: nodeCommands(res.BuildTool, ctx, pkg)}
		tool := res.BuildTool
		if tool == "npm-install" {
			tool = "npm"
			res.Notes = append(res.Notes, "没有锁文件（package-lock.json / yarn.lock / pnpm-lock.yaml），依赖版本不可复现，建议提交锁文件")
		}
		cache = cacheFor(tool)
		tplSpec = "node:" + res.Version + ":" + res.BuildTool
		if res.BuildTool == "npm-install" {
			res.BuildTool = "npm"
		}
		if _, ok := pkg.Scripts["start"]; !ok && !res.HasDockerfile {
			res.Notes = append(res.Notes, "package.json 没有 start 脚本，内置构建模板以 npm start 启动，请补充或提供 Dockerfile")
		}
	case "java":
		if res.BuildTool == "maven" {
			res.Version = javaVersion(pomJavaRe, ctx.Contents["pom.xml"])
			mvn := "mvn"
			if ctx.has("mvnw") {
				mvn = "sh ./mvnw"
			}
			test = &Stage{Name: "test", Type: "test", Image: "maven:3.9-eclipse-temurin-" + res.Version, Commands: []string{mvn + " -B test"}}
			cache = cacheFor("maven")
			tplSpec = "maven:" + res.Version
		} else {
			res.Version = javaVersion(gradleJavaRe, ctx.Contents["build.gradle"]+"\n"+ctx.Contents["build.gradle.kts"])
			cmd := "gradle test --no-daemon"
			if ctx.has("gradlew") {
				cmd = "sh ./gradlew test --no-daemon"
			}
			test = &Stage{Name: "test", Type: "test", Image: "gradle:8.10-jdk" + res.Version, Commands: []string{cmd}}
			cache = cacheFor("gradle")
			tplSpec = "gradle:" + res.Version
		}
		if !res.HasDockerfile {
			res.Notes = append(res.Notes, "内置构建模板取第一个可执行 jar 运行；多模块工程请在仓库中提供 Dockerfile")
		}
	case "python":
		res.Version = pythonVersion(ctx)
		cmds := []string{"python -m pip install --upgrade pip"}
		mode := "requirements"
		pyproject := ctx.Contents["pyproject.toml"]
		switch {
		case ctx.has("requirements.txt"):
			cmds = append(cmds, "pip install -r requirements.txt")
		case strings.Contains(pyproject, "[tool.poetry]"):
			res.BuildTool, mode = "poetry", "pyproject"
			cmds = append(cmds, "pip install poetry", "poetry config virtualenvs.create false", "poetry install --no-interaction")
		default:
			mode = "pyproject"
			cmds = append(cmds, "pip install .")
		}
		if ctx.hasPrefix("tests/") || ctx.hasPrefix("test/") || strings.Contains(ctx.Contents["requirements.txt"]+pyproject, "pytest") || hasPyTestFile(ctx) {
			cmds = append(cmds, "pip install pytest", "python -m pytest -q")
		} else {
			cmds = append(cmds, "python -m compileall -q .")
		}
		test = &Stage{Name: "test", Type: "test", Image: "python:" + res.Version + "-slim", Commands: cmds}
		cache = cacheFor("python")
		tplSpec = "python:" + res.Version + ":" + mode
		if !res.HasDockerfile {
			res.Notes = append(res.Notes, "内置构建模板依次尝试 manage.py / main.py / app.py 启动，请确认入口或提供 Dockerfile")
		}
	}
	if res.Language == "unknown" && res.HasDockerfile {
		res.Language, res.BuildTool = "dockerfile", "dockerfile"
	}

	tpl := &DetectTemplate{Stages: []Stage{}}
	if test != nil {
		tpl.Stages = append(tpl.Stages, *test)
	}
	build := Stage{Name: "build", Type: "build"}
	switch {
	case res.HasDockerfile:
		res.BuildMode = "dockerfile"
		if res.BuildTool == "" {
			res.BuildTool = "dockerfile"
		}
		tpl.Stages = append(tpl.Stages, build)
	case tplSpec != "":
		res.BuildMode = "template"
		build.DockerfileTemplate = tplSpec
		tpl.Stages = append(tpl.Stages, build)
		res.Notes = append(res.Notes, "仓库中没有 Dockerfile，构建阶段将使用内置模板生成（仓库里出现同名文件后自动以仓库文件为准）")
	default:
		res.BuildMode = "none"
		res.Notes = append(res.Notes, "未识别出语言，也没有 Dockerfile；请在仓库中提供 Dockerfile 后再识别")
	}
	if cache == nil {
		cache = &CacheConfig{Enabled: false, Key: cacheDefaultKey}
	}
	cache.LayerCache = res.BuildMode != "none"
	_ = cache.normalize()
	if res.BuildMode != "none" || test != nil {
		tpl.Cache = cache
	}
	if len(tpl.Stages) > 0 {
		res.Template = tpl
	}
	sort.Strings(res.Others)
	return res
}

func cacheFor(tool string) *CacheConfig {
	return &CacheConfig{Enabled: true, Language: tool, Key: cacheDefaultKey, Paths: DefaultCachePaths(tool)}
}

func firstExisting(s *repoSnapshot, names ...string) string {
	for _, n := range names {
		if s.Files[n] {
			return n
		}
	}
	return ""
}

func hasPyTestFile(s *repoSnapshot) bool {
	for f := range s.Files {
		b := path.Base(f)
		if strings.HasPrefix(b, "test_") && strings.HasSuffix(b, ".py") {
			return true
		}
	}
	return false
}

func goVersion(gomod string) string {
	if m := goToolchainRe.FindStringSubmatch(gomod); m != nil {
		return m[1]
	}
	if m := goVersionRe.FindStringSubmatch(gomod); m != nil {
		major, minor := splitVersion(m[1])
		if major == 1 && minor < 20 { // very old go directive: use a supported toolchain
			return "1.23"
		}
		return m[1]
	}
	return "1.23"
}

func splitVersion(v string) (int, int) {
	a, b, _ := strings.Cut(v, ".")
	x, _ := strconv.Atoi(a)
	y, _ := strconv.Atoi(b)
	return x, y
}

func nodeTool(s *repoSnapshot) string {
	switch {
	case s.has("pnpm-lock.yaml"):
		return "pnpm"
	case s.has("yarn.lock"):
		return "yarn"
	case s.has("package-lock.json", "npm-shrinkwrap.json"):
		return "npm"
	}
	pkg := parsePackageJSON(s.Contents["package.json"])
	switch {
	case strings.HasPrefix(pkg.PackageManager, "pnpm@"):
		return "pnpm"
	case strings.HasPrefix(pkg.PackageManager, "yarn@"):
		return "yarn"
	}
	return "npm-install"
}

type packageJSON struct {
	Scripts        map[string]string `json:"scripts"`
	Engines        map[string]string `json:"engines"`
	PackageManager string            `json:"packageManager"`
}

func parsePackageJSON(raw string) packageJSON {
	var p packageJSON
	_ = json.Unmarshal([]byte(raw), &p)
	if p.Scripts == nil {
		p.Scripts = map[string]string{}
	}
	return p
}

func nodeVersion(s *repoSnapshot) string {
	for _, f := range []string{".nvmrc", ".node-version"} {
		if m := majorRe.FindStringSubmatch(strings.TrimSpace(s.Contents[f])); m != nil {
			return clampNode(m[1])
		}
	}
	pkg := parsePackageJSON(s.Contents["package.json"])
	if m := majorRe.FindStringSubmatch(pkg.Engines["node"]); m != nil {
		return clampNode(m[1])
	}
	return "20"
}

func clampNode(major string) string {
	n, _ := strconv.Atoi(major)
	switch {
	case n < 18:
		return "18"
	case n > 24:
		return "22"
	}
	return strconv.Itoa(n)
}

func nodeCommands(tool string, s *repoSnapshot, pkg packageJSON) []string {
	run := "npm run"
	var cmds []string
	switch tool {
	case "pnpm":
		cmds = []string{"corepack enable", "pnpm install --frozen-lockfile"}
		run = "pnpm run"
	case "yarn":
		cmds = []string{"corepack enable", "yarn install --frozen-lockfile"}
		run = "yarn run"
	case "npm":
		cmds = []string{"npm ci"}
	default:
		cmds = []string{"npm install"}
	}
	if t, ok := pkg.Scripts["test"]; ok && !strings.Contains(t, "no test specified") {
		cmds = append(cmds, run+" test")
	}
	if _, ok := pkg.Scripts["build"]; ok {
		cmds = append(cmds, run+" build")
	}
	return cmds
}

func javaVersion(re *regexp.Regexp, text string) string {
	if m := re.FindStringSubmatch(text); m != nil {
		for _, g := range m[1:] {
			if g != "" {
				n, _ := strconv.Atoi(g)
				switch {
				case n <= 8:
					return "8"
				case n <= 11:
					return "11"
				case n <= 17:
					return "17"
				default:
					return "21"
				}
			}
		}
	}
	return "17"
}

func pythonVersion(s *repoSnapshot) string {
	for _, f := range []string{".python-version", "runtime.txt"} {
		if m := pyVersionRe.FindStringSubmatch(s.Contents[f]); m != nil {
			return clampPython(m[1])
		}
	}
	if m := pyRequiresRe.FindStringSubmatch(s.Contents["pyproject.toml"]); m != nil {
		return clampPython(m[1])
	}
	return "3.12"
}

func clampPython(v string) string {
	major, minor := splitVersion(v)
	if major != 3 || minor < 8 {
		return "3.12"
	}
	if minor > 13 {
		return "3.13"
	}
	return fmt.Sprintf("3.%d", minor)
}

// ---------------------------------------------------------------- built-in Dockerfiles

// renderDockerfileTemplate returns the built-in Dockerfile of a template spec
// such as "go:1.22", "node:20:pnpm", "maven:17", "gradle:21", "python:3.12:requirements".
func renderDockerfileTemplate(spec string) (string, error) {
	m := dockerTemplateRe.FindStringSubmatch(spec)
	if m == nil {
		return "", httpx.Validation("unknown dockerfile_template " + spec)
	}
	lang, ver, mode := m[1], m[2], m[3]
	switch lang {
	case "go":
		return "FROM golang:" + ver + "-alpine AS build\n" +
			"WORKDIR /src\n" +
			"COPY go.mod go.sum* ./\n" +
			"RUN go mod download\n" +
			"COPY . .\n" +
			"RUN CGO_ENABLED=0 go build -trimpath -ldflags \"-s -w\" -o /out/app .\n" +
			"FROM alpine:3.20\n" +
			"RUN apk add --no-cache ca-certificates tzdata\n" +
			"COPY --from=build /out/app /usr/local/bin/app\n" +
			"EXPOSE 8080\n" +
			"ENTRYPOINT [\"/usr/local/bin/app\"]\n", nil
	case "node":
		install := map[string]string{
			"": "npm ci", "npm": "npm ci", "npm-install": "npm install",
			"yarn": "corepack enable && yarn install --frozen-lockfile",
			"pnpm": "corepack enable && pnpm install --frozen-lockfile",
		}[mode]
		if install == "" {
			return "", httpx.Validation("unknown dockerfile_template " + spec)
		}
		return "FROM node:" + ver + "-alpine\n" +
			"WORKDIR /app\n" +
			"COPY package.json package-lock.json* npm-shrinkwrap.json* yarn.lock* pnpm-lock.yaml* ./\n" +
			"RUN " + install + "\n" +
			"COPY . .\n" +
			"RUN npm run build --if-present\n" +
			"ENV NODE_ENV=production\n" +
			"EXPOSE 3000\n" +
			"CMD [\"npm\", \"start\"]\n", nil
	case "maven", "gradle":
		jdk := ver
		var build string
		if lang == "maven" {
			build = "FROM maven:3.9-eclipse-temurin-" + jdk + " AS build\n" +
				"WORKDIR /src\n" +
				"COPY pom.xml .\n" +
				"RUN mvn -B -q dependency:go-offline || true\n" +
				"COPY . .\n" +
				"RUN mvn -B -DskipTests package && mkdir -p /out && cp \"$(ls target/*.jar | grep -v -e '-sources' -e '-javadoc' -e '/original-' | head -n1)\" /out/app.jar\n"
		} else {
			build = "FROM gradle:8.10-jdk" + jdk + " AS build\n" +
				"WORKDIR /src\n" +
				"COPY . .\n" +
				"RUN gradle build -x test --no-daemon && mkdir -p /out && cp \"$(ls build/libs/*.jar | grep -v -e '-plain' | head -n1)\" /out/app.jar\n"
		}
		return build +
			"FROM eclipse-temurin:" + jdk + "-jre\n" +
			"WORKDIR /app\n" +
			"COPY --from=build /out/app.jar /app/app.jar\n" +
			"EXPOSE 8080\n" +
			"ENTRYPOINT [\"java\", \"-jar\", \"/app/app.jar\"]\n", nil
	case "python":
		head := "FROM python:" + ver + "-slim\n" +
			"WORKDIR /app\n" +
			"ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1\n"
		var deps string
		switch mode {
		case "", "requirements":
			deps = "COPY requirements.txt .\nRUN pip install --no-cache-dir -r requirements.txt\nCOPY . .\n"
		case "pyproject":
			deps = "COPY . .\nRUN pip install --no-cache-dir .\n"
		default:
			return "", httpx.Validation("unknown dockerfile_template " + spec)
		}
		return head + deps +
			"EXPOSE 8000\n" +
			"CMD [\"sh\", \"-c\", \"if [ -f manage.py ]; then exec python manage.py runserver 0.0.0.0:8000; elif [ -f main.py ]; then exec python main.py; else exec python app.py; fi\"]\n", nil
	}
	return "", httpx.Validation("unknown dockerfile_template " + spec)
}

// applyDockerfileTemplate adds an init container that writes the built-in
// Dockerfile when the repository does not contain one at dockerfilePath.
func applyDockerfileTemplate(spec *corev1.PodSpec, s Stage, dockerfilePath string) error {
	if s.Type != "build" || s.DockerfileTemplate == "" {
		return nil
	}
	content, err := renderDockerfileTemplate(s.DockerfileTemplate)
	if err != nil {
		return err
	}
	c := corev1.Container{
		Name: dockerfileInitC, Image: GitImage,
		Command:      []string{"sh", "-c", `if [ -f "$DF" ]; then echo "使用仓库中的 $DF"; else mkdir -p "$(dirname "$DF")" && printf '%s' "$DOCKERFILE" > "$DF" && echo "仓库中没有 Dockerfile，已使用内置构建模板"; fi`},
		Env:          []corev1.EnvVar{{Name: "DF", Value: dockerfilePath}, {Name: "DOCKERFILE", Value: content}},
		VolumeMounts: []corev1.VolumeMount{{Name: "workspace", MountPath: workDir}},
	}
	spec.InitContainers = append(spec.InitContainers, c)
	return nil
}

// ---------------------------------------------------------------- detect Job

// detectScript lists shallow files and dumps the marker files of contextDir.
func detectScript(contextDir string) string {
	var b strings.Builder
	b.WriteString("cd " + srcDir + "\n")
	b.WriteString("echo \"" + detectMarker + "H $(git rev-parse HEAD 2>/dev/null)\"\n")
	b.WriteString(fmt.Sprintf("git ls-files | awk -F/ 'NF<=4' | head -n %d | sed 's/^/%sF /'\n", detectMaxFiles, detectMarker))
	b.WriteString("for f in")
	for _, f := range detectContentFiles {
		p := path.Clean(path.Join(contextDir, f))
		b.WriteString(" " + p)
	}
	b.WriteString("; do\n")
	b.WriteString(fmt.Sprintf("  if [ -f \"$f\" ]; then echo \"%sC $f $(head -c %d \"$f\" | base64 | tr -d '\\n')\"; fi\n", detectMarker, detectMaxBytes))
	b.WriteString("done\n")
	b.WriteString("echo \"" + detectMarker + "END\"\n")
	return b.String()
}

// parseDetectOutput reads the XSDETECT lines of the detect Job.
func parseDetectOutput(lines []string) (*repoSnapshot, bool) {
	snap := &repoSnapshot{Files: map[string]bool{}, Contents: map[string]string{}}
	done := false
	for _, l := range lines {
		i := strings.Index(l, detectMarker)
		if i < 0 {
			continue
		}
		rest := l[i+len(detectMarker):]
		switch {
		case rest == "END":
			done = true
		case strings.HasPrefix(rest, "H "):
			snap.Commit = strings.TrimSpace(rest[2:])
		case strings.HasPrefix(rest, "F "):
			if f := strings.TrimSpace(rest[2:]); f != "" && len(snap.Files) < detectMaxFiles {
				snap.Files[f] = true
			}
		case strings.HasPrefix(rest, "C "):
			name, enc, ok := strings.Cut(rest[2:], " ")
			if !ok {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
			if err != nil {
				continue
			}
			snap.Contents[name] = string(raw)
			snap.Files[name] = true
		}
	}
	return snap, done
}

type detectIn struct {
	RepoURL        string `json:"repo_url" binding:"required,max=2048"`
	Branch         string `json:"branch"`
	CredentialID   int64  `json:"credential_id"`
	AppID          int64  `json:"app_id"`
	RunnerTargetID int64  `json:"runner_target_id"`
	ContextDir     string `json:"context_dir"`
	Dockerfile     string `json:"dockerfile"`
}

func validDetectPath(p string) bool {
	return p == "" || p == "." || (detectRelPathRe.MatchString(p) && !strings.Contains(p, "..") && !strings.HasPrefix(p, "/"))
}

func shortRand() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *Module) detectRepo(c *gin.Context) {
	var in detectIn
	if !httpx.Bind(c, &in) {
		return
	}
	in.RepoURL = strings.TrimSpace(in.RepoURL)
	in.Branch = strings.TrimSpace(in.Branch)
	if in.Branch == "" {
		in.Branch = "main"
	}
	if in.ContextDir = strings.TrimSpace(in.ContextDir); in.ContextDir == "" {
		in.ContextDir = "."
	}
	in.Dockerfile = strings.TrimSpace(in.Dockerfile)
	if err := validateApplicationRepoURL(in.RepoURL); err != nil {
		httpx.Fail(c, err)
		return
	}
	if !detectRefRe.MatchString(in.Branch) || strings.Contains(in.Branch, "..") || strings.HasPrefix(in.Branch, "-") {
		httpx.Fail(c, httpx.Validation("invalid branch name"))
		return
	}
	if !validDetectPath(in.ContextDir) || !validDetectPath(in.Dockerfile) {
		httpx.Fail(c, httpx.Validation("context_dir and dockerfile must be relative paths inside the repository"))
		return
	}
	// Reuse the application's stored credential when the repository is unchanged.
	app := &Application{Name: "detect", RepoURL: in.RepoURL, Branch: in.Branch, CredentialID: in.CredentialID}
	if in.AppID != 0 {
		if a, err := m.loadApp(c, in.AppID); err == nil && a.RepoURL == in.RepoURL {
			if in.CredentialID == 0 || in.CredentialID == a.CredentialID {
				app = a
				app.Branch = in.Branch
			}
		}
	}
	if app.ID == 0 {
		if _, err := m.validateCredentialRef(c, app.CredentialID, true); err != nil {
			httpx.Fail(c, err)
			return
		}
	}
	targetID := in.RunnerTargetID
	if targetID == 0 {
		if err := m.db.QueryRow(c, `SELECT id FROM cicd_targets ORDER BY id LIMIT 1`).Scan(&targetID); err != nil {
			httpx.Fail(c, httpx.Conflict("no execution cluster is configured"))
			return
		}
	}
	env, err := m.targetConfigEnv(c, targetID)
	if err != nil {
		httpx.Fail(c, httpx.Validation("unknown runner target"))
		return
	}
	if p := rbac.PrincipalFrom(c); env != "" && (p == nil || !p.CanEnv(env)) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope: "+env))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), (detectJobLimit+20)*time.Second)
	defer cancel()
	usesCredential := app.CredentialID != 0 || app.gitTokenEnc != ""
	res, err := m.runDetect(ctx, targetID, app, in.ContextDir, in.Dockerfile, usesCredential)
	audit.SetResource(c, "cicd_repo_detect", fmt.Sprint(targetID))
	if err != nil {
		var he *httpx.Error
		if errors.As(err, &he) {
			httpx.Fail(c, he)
			return
		}
		httpx.Fail(c, httpx.Conflict("language detection failed").Wrap(err))
		return
	}
	audit.SetDetail(c, gin.H{"language": res.Language, "build_tool": res.BuildTool, "build_mode": res.BuildMode})
	httpx.OK(c, res)
}

// runDetect clones the repository on the runner target (same git-clone init
// container and credential handling as pipeline runs) and inspects it.
func (m *Module) runDetect(ctx context.Context, targetID int64, a *Application, contextDir, dockerfile string, withhold bool) (*DetectResult, error) {
	conn, err := m.targetClient(ctx, targetID)
	if err != nil {
		return nil, err
	}
	gitURL, sshKey, err := m.cloneSource(ctx, a)
	if err != nil {
		return nil, httpx.Validation("cannot use the repository credential: " + redactCICDText(err.Error()))
	}
	suffix := shortRand()
	rt := &runnerCtx{cs: conn.cs, ns: conn.namespace, secret: "cicd-detect-" + suffix}
	data := map[string]string{"git_url": gitURL}
	if sshKey != "" {
		data["ssh_key"] = strings.TrimRight(sshKey, "\n") + "\n"
		a.sshClone = true
	}
	if err := rt.createSecret(ctx, data, 0); err != nil {
		return nil, fmt.Errorf("create detect secret in %s: %w", conn.namespace, err)
	}
	defer func() {
		_ = rt.cs.CoreV1().Secrets(rt.ns).Delete(context.Background(), rt.secret, metav1.DeleteOptions{})
	}()
	stage := Stage{Name: "detect", Type: "script", Image: GitImage, Commands: []string{detectScript(contextDir)}}
	spec := stagePod(a, &Run{Branch: a.Branch}, stage, rt.secret)
	job := "cicd-detect-" + suffix
	var mu sync.Mutex
	var lines []string
	collect := func(f string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if len(lines) < detectMaxFiles+200 {
			lines = append(lines, fmt.Sprintf(f, args...))
		}
	}
	jerr := runJob(ctx, conn.cs, conn.namespace, job, 0, spec, detectJobLimit, collect)
	bg := metav1.DeletePropagationBackground
	_ = conn.cs.BatchV1().Jobs(conn.namespace).Delete(context.Background(), job, metav1.DeleteOptions{PropagationPolicy: &bg})
	mu.Lock()
	snap, done := parseDetectOutput(lines)
	tail := detectTail(lines, 3)
	mu.Unlock()
	if jerr != nil || !done {
		msg := "cannot clone the repository: check the URL, branch and credential"
		if !withhold && tail != "" {
			msg += " (" + redactCICDText(tail) + ")"
		}
		if jerr != nil && strings.Contains(jerr.Error(), "Err") && strings.Contains(jerr.Error(), "Pull") {
			msg = "the execution cluster cannot pull the Git image " + GitImage
		}
		return nil, httpx.Validation(msg)
	}
	res := DetectLanguage(snap, contextDir, dockerfile)
	res.Branch = a.Branch
	return res, nil
}

// detectTail returns the last non-marker output lines for diagnostics.
func detectTail(lines []string, n int) string {
	out := []string{}
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || strings.Contains(l, detectMarker) || strings.HasPrefix(l, "---") || strings.HasPrefix(l, "job ") {
			continue
		}
		if len(l) > 200 {
			l = l[:200]
		}
		out = append([]string{l}, out...)
	}
	return strings.Join(out, "; ")
}
