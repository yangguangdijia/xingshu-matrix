package cicd

// Image security scanning: a pipeline stage of type "scan" scans the image a
// preceding build stage pushed, either with Trivy run as a Kubernetes Job on
// the runner cluster (same execution path and registry credentials as the
// Kaniko build Jobs) or by reading the scan report Harbor already holds. The
// parsed result (counts per severity + the top N findings) is stored in
// cicd_image_scans and a gate policy decides whether the run may continue.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// TrivyImage is the scanner image run by scan stages (source=trivy).
var TrivyImage = "aquasec/trivy:0.57.1"

const (
	scanSourceTrivy  = "trivy"
	scanSourceHarbor = "harbor"

	defaultScanTopN = 50
	maxScanTopN     = 200
	maxIgnoreCVEs   = 500
	// maxScanReport bounds the decoded Trivy JSON report kept in memory.
	maxScanReport = 64 << 20

	trivyReportBegin = "@@MATRIX-TRIVY-REPORT-BEGIN@@"
	trivyReportEnd   = "@@MATRIX-TRIVY-REPORT-END@@"
)

// severityRank orders severities; higher is worse.
var severityRank = map[string]int{"UNKNOWN": 0, "LOW": 1, "MEDIUM": 2, "HIGH": 3, "CRITICAL": 4}

var (
	cveIDRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{2,79}$`)
	harborHost = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
)

// ScanPolicy configures a scan stage and its gate.
type ScanPolicy struct {
	Source          string   `json:"source,omitempty"`           // trivy (default) | harbor
	BlockSeverities []string `json:"block_severities,omitempty"` // default CRITICAL,HIGH; empty list after normalize = report only
	ReportOnly      bool     `json:"report_only,omitempty"`      // never fail the run
	IgnoreCVEs      []string `json:"ignore_cves,omitempty"`      // findings with these ids never block
	IgnoreUnfixed   bool     `json:"ignore_unfixed,omitempty"`   // findings without a fixed version never block
	TopN            int      `json:"top_n,omitempty"`            // findings stored per scan (default 50, max 200)
	Insecure        bool     `json:"insecure,omitempty"`         // plain-HTTP / self-signed registry
	HarborTrigger   bool     `json:"harbor_trigger,omitempty"`   // harbor: start a scan when the artifact has no report yet
}

// normalize validates and fills defaults in place.
func (p *ScanPolicy) normalize() error {
	if p == nil {
		return nil
	}
	p.Source = strings.ToLower(strings.TrimSpace(p.Source))
	if p.Source == "" {
		p.Source = scanSourceTrivy
	}
	if p.Source != scanSourceTrivy && p.Source != scanSourceHarbor {
		return httpx.Validation("scan.source must be trivy or harbor")
	}
	if p.BlockSeverities == nil && !p.ReportOnly {
		p.BlockSeverities = []string{"CRITICAL", "HIGH"}
	}
	seen := map[string]bool{}
	sev := make([]string, 0, len(p.BlockSeverities))
	for _, s := range p.BlockSeverities {
		s = strings.ToUpper(strings.TrimSpace(s))
		if _, ok := severityRank[s]; !ok {
			return httpx.Validation("scan.block_severities accepts CRITICAL, HIGH, MEDIUM, LOW, UNKNOWN")
		}
		if !seen[s] {
			seen[s] = true
			sev = append(sev, s)
		}
	}
	sort.Slice(sev, func(i, j int) bool { return severityRank[sev[i]] > severityRank[sev[j]] })
	p.BlockSeverities = sev
	if len(p.IgnoreCVEs) > maxIgnoreCVEs {
		return httpx.Validation(fmt.Sprintf("scan.ignore_cves accepts at most %d entries", maxIgnoreCVEs))
	}
	ids := make([]string, 0, len(p.IgnoreCVEs))
	seenID := map[string]bool{}
	for _, id := range p.IgnoreCVEs {
		id = strings.ToUpper(strings.TrimSpace(id))
		if id == "" {
			continue
		}
		if !cveIDRe.MatchString(id) {
			return httpx.Validation("scan.ignore_cves contains an invalid vulnerability id")
		}
		if !seenID[id] {
			seenID[id] = true
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	p.IgnoreCVEs = ids
	if p.TopN <= 0 {
		p.TopN = defaultScanTopN
	}
	if p.TopN > maxScanTopN {
		p.TopN = maxScanTopN
	}
	return nil
}

func (p *ScanPolicy) blocks(sev string) bool {
	if p == nil || p.ReportOnly {
		return false
	}
	for _, s := range p.BlockSeverities {
		if s == sev {
			return true
		}
	}
	return false
}

func (p *ScanPolicy) ignored(id string) bool {
	if p == nil {
		return false
	}
	id = strings.ToUpper(id)
	i := sort.SearchStrings(p.IgnoreCVEs, id)
	return i < len(p.IgnoreCVEs) && p.IgnoreCVEs[i] == id
}

// ScanVuln is one finding.
type ScanVuln struct {
	ID        string `json:"id"`
	Package   string `json:"package"`
	Installed string `json:"installed"`
	Fixed     string `json:"fixed"`
	Severity  string `json:"severity"`
	Title     string `json:"title,omitempty"`
	Target    string `json:"target,omitempty"`
	URL       string `json:"url,omitempty"`
	Ignored   bool   `json:"ignored,omitempty"`  // listed in ignore_cves
	Blocking  bool   `json:"blocking,omitempty"` // counts against the gate
}

// ScanSummary counts findings (deduplicated by id + package + version).
type ScanSummary struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
	Unknown  int `json:"unknown"`
	Total    int `json:"total"`
	Ignored  int `json:"ignored"`
	Blocking int `json:"blocking"`
}

func (s *ScanSummary) add(sev string) {
	switch sev {
	case "CRITICAL":
		s.Critical++
	case "HIGH":
		s.High++
	case "MEDIUM":
		s.Medium++
	case "LOW":
		s.Low++
	default:
		s.Unknown++
	}
	s.Total++
}

// ImageScan is one stored scan result.
type ImageScan struct {
	ID              int64       `json:"id"`
	RunID           int64       `json:"run_id"`
	StageIdx        int         `json:"stage_idx"`
	Image           string      `json:"image"`
	Source          string      `json:"source"`
	Status          string      `json:"status"` // passed | blocked | error
	Summary         ScanSummary `json:"summary"`
	Policy          ScanPolicy  `json:"policy"`
	Vulnerabilities []ScanVuln  `json:"vulnerabilities"`
	Error           string      `json:"error"`
	ScannedAt       time.Time   `json:"scanned_at"`
}

func normSeverity(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	switch s {
	case "NEGLIGIBLE":
		return "LOW"
	case "NONE", "":
		return "UNKNOWN"
	}
	if _, ok := severityRank[s]; ok {
		return s
	}
	return "UNKNOWN"
}

// evaluateScan dedups findings, applies the policy and returns the summary,
// whether the gate passed, and the top N findings (blocking first, then by
// severity).
func evaluateScan(vulns []ScanVuln, pol ScanPolicy) (ScanSummary, bool, []ScanVuln) {
	var sum ScanSummary
	seen := map[string]bool{}
	kept := make([]ScanVuln, 0, len(vulns))
	for _, v := range vulns {
		v.Severity = normSeverity(v.Severity)
		key := strings.ToUpper(v.ID) + "\x00" + v.Package + "\x00" + v.Installed
		if v.ID == "" || seen[key] {
			continue
		}
		seen[key] = true
		sum.add(v.Severity)
		v.Ignored = pol.ignored(v.ID)
		v.Blocking = false
		switch {
		case v.Ignored:
			sum.Ignored++
		case pol.IgnoreUnfixed && strings.TrimSpace(v.Fixed) == "":
		case pol.blocks(v.Severity):
			v.Blocking = true
			sum.Blocking++
		}
		kept = append(kept, v)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if a.Blocking != b.Blocking {
			return a.Blocking
		}
		if severityRank[a.Severity] != severityRank[b.Severity] {
			return severityRank[a.Severity] > severityRank[b.Severity]
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Package < b.Package
	})
	top := pol.TopN
	if top <= 0 {
		top = defaultScanTopN
	}
	if len(kept) > top {
		kept = kept[:top]
	}
	for i := range kept {
		kept[i].Title = truncateRunes(kept[i].Title, 200)
		kept[i].URL = truncateRunes(kept[i].URL, 300)
	}
	return sum, sum.Blocking == 0, kept
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// gateMessage is the user-facing failure message of a blocked scan.
func gateMessage(sum ScanSummary, pol ScanPolicy) string {
	parts := []string{}
	for _, item := range []struct {
		sev string
		n   int
	}{{"CRITICAL", sum.Critical}, {"HIGH", sum.High}, {"MEDIUM", sum.Medium}, {"LOW", sum.Low}, {"UNKNOWN", sum.Unknown}} {
		if pol.blocks(item.sev) && item.n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", item.sev, item.n))
		}
	}
	return fmt.Sprintf("镜像扫描未通过：%d 个阻断级别漏洞（%s；阻断级别 %s，已忽略 %d 个）",
		sum.Blocking, strings.Join(parts, "，"), strings.Join(pol.BlockSeverities, "/"), sum.Ignored)
}

// ---------------------------------------------------------------- Trivy

type trivyReport struct {
	ArtifactName string `json:"ArtifactName"`
	Results      []struct {
		Target          string `json:"Target"`
		Vulnerabilities []struct {
			VulnerabilityID  string `json:"VulnerabilityID"`
			PkgName          string `json:"PkgName"`
			InstalledVersion string `json:"InstalledVersion"`
			FixedVersion     string `json:"FixedVersion"`
			Severity         string `json:"Severity"`
			Title            string `json:"Title"`
			PrimaryURL       string `json:"PrimaryURL"`
		} `json:"Vulnerabilities"`
	} `json:"Results"`
}

// parseTrivyJSON parses `trivy image --format json` output.
func parseTrivyJSON(raw []byte) ([]ScanVuln, error) {
	var rep trivyReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, fmt.Errorf("invalid Trivy report: %w", err)
	}
	out := []ScanVuln{}
	for _, r := range rep.Results {
		for _, v := range r.Vulnerabilities {
			out = append(out, ScanVuln{ID: v.VulnerabilityID, Package: v.PkgName, Installed: v.InstalledVersion,
				Fixed: v.FixedVersion, Severity: v.Severity, Title: v.Title, Target: r.Target, URL: v.PrimaryURL})
		}
	}
	return out, nil
}

// trivyScript runs Trivy and prints the JSON report gzip+base64 encoded
// between markers, so it can be separated from scanner log output.
const trivyScript = `set -u
rc=0
trivy image --quiet --scanners vuln --format json --output /tmp/report.json --exit-code 0 $TRIVY_FLAGS "$SCAN_IMAGE" 1>&2 || rc=$?
if [ "$rc" -ne 0 ] || [ ! -s /tmp/report.json ]; then echo "trivy failed with exit code $rc" 1>&2; exit 1; fi
echo "` + trivyReportBegin + `"
gzip -c /tmp/report.json | base64
echo "` + trivyReportEnd + `"`

// scanPod builds the Trivy Job pod. registryKey selects a dockerconfigjson in
// the run Secret; registrySecret names a dockerconfigjson Secret in the runner
// namespace (legacy). Stage env (e.g. TRIVY_DB_REPOSITORY for an offline
// mirror) is passed through like other stages.
func scanPod(s Stage, image, runSecret, registryKey, registrySecret string, timeout time.Duration) corev1.PodSpec {
	flags := []string{fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))}
	if s.Scan != nil && s.Scan.Insecure {
		flags = append(flags, "--insecure")
	}
	env := []corev1.EnvVar{{Name: "SCAN_IMAGE", Value: image}, {Name: "TRIVY_FLAGS", Value: strings.Join(flags, " ")},
		{Name: "TRIVY_CACHE_DIR", Value: "/tmp/trivy-cache"}}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		value := s.Env[k]
		if sensitiveStageKey.MatchString(k) {
			if secret, secretKey, ok := secretReferenceParts(value); ok {
				env = append(env, corev1.EnvVar{Name: k, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: secretKey}}})
				continue
			}
		}
		env = append(env, corev1.EnvVar{Name: k, Value: value})
	}
	step := corev1.Container{Name: stepContainer, Image: TrivyImage, Command: []string{"sh", "-c", trivyScript}, Env: env}
	spec := corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever}
	switch {
	case registryKey != "":
		spec.Volumes = []corev1.Volume{{Name: "docker-config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: runSecret, Items: []corev1.KeyToPath{{Key: registryKey, Path: "config.json"}}}}}}
	case registrySecret != "":
		spec.Volumes = []corev1.Volume{{Name: "docker-config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: registrySecret, Items: []corev1.KeyToPath{{Key: ".dockerconfigjson", Path: "config.json"}}}}}}
	}
	if len(spec.Volumes) > 0 {
		step.Env = append(step.Env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: "/docker-config"})
		step.VolumeMounts = []corev1.VolumeMount{{Name: "docker-config", MountPath: "/docker-config", ReadOnly: true}}
	}
	spec.Containers = []corev1.Container{step}
	return spec
}

// reportCapture separates the marked report from scanner output lines.
type reportCapture struct {
	in, done, overflow bool
	buf                strings.Builder
}

// feed consumes a log line; it returns true when the line belongs to the report.
func (r *reportCapture) feed(line string) bool {
	t := strings.TrimSpace(line)
	switch {
	case t == trivyReportBegin:
		r.in, r.done, r.overflow = true, false, false
		r.buf.Reset()
		return true
	case t == trivyReportEnd && r.in:
		r.in, r.done = false, true
		return true
	case r.in:
		if r.buf.Len()+len(t) > maxScanReport {
			r.overflow = true
			return true
		}
		r.buf.WriteString(t)
		return true
	}
	return false
}

func (r *reportCapture) report() ([]byte, error) {
	if !r.done {
		return nil, errors.New("Trivy report not found in scanner output")
	}
	if r.overflow {
		return nil, errors.New("Trivy report exceeds size limit")
	}
	return decodeTrivyPayload(r.buf.String())
}

func decodeTrivyPayload(b64 string) ([]byte, error) {
	gz, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode Trivy report: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, fmt.Errorf("decompress Trivy report: %w", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, maxScanReport+1))
	if err != nil {
		return nil, fmt.Errorf("decompress Trivy report: %w", err)
	}
	if len(raw) > maxScanReport {
		return nil, errors.New("Trivy report exceeds size limit")
	}
	return raw, nil
}

// ---------------------------------------------------------------- Harbor

// imageRef splits registry/repository[:tag|@digest].
type imageRef struct {
	Host, Repository, Reference string
}

func parseImageRef(img string) (imageRef, error) {
	img = strings.TrimSpace(img)
	if img == "" || strings.ContainsAny(img, " \t\n") {
		return imageRef{}, errors.New("invalid image reference")
	}
	var ref imageRef
	name := img
	if at := strings.Index(name, "@"); at >= 0 {
		ref.Reference, name = name[at+1:], name[:at]
	} else if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		ref.Reference, name = name[colon+1:], name[:colon]
	}
	if ref.Reference == "" {
		ref.Reference = "latest"
	}
	first, rest, hasSlash := strings.Cut(name, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		ref.Host, ref.Repository = first, rest
	} else {
		ref.Host, ref.Repository = "docker.io", name
	}
	if ref.Repository == "" {
		return imageRef{}, errors.New("invalid image reference")
	}
	return ref, nil
}

type harborClient struct {
	base, user, pass string
	hc               *http.Client
}

func newHarborClient(host, user, pass string, insecure bool) (*harborClient, error) {
	if !harborHost.MatchString(host) {
		return nil, errors.New("invalid Harbor host")
	}
	scheme := "https"
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		scheme = "http"
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit per-stage opt-in
	}
	return &harborClient{base: scheme + "://" + host, user: user, pass: pass,
		hc: &http.Client{Timeout: 30 * time.Second, Transport: tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (h *harborClient) artifactPath(ref imageRef) (string, error) {
	project, repo, ok := strings.Cut(ref.Repository, "/")
	if !ok || project == "" || repo == "" {
		return "", errors.New("Harbor images must be <host>/<project>/<repository>")
	}
	// Harbor expects '/' inside repository names double-encoded (a%252Fb).
	return "/api/v2.0/projects/" + url.PathEscape(project) + "/repositories/" + url.PathEscape(url.PathEscape(repo)) +
		"/artifacts/" + url.PathEscape(ref.Reference), nil
}

func (h *harborClient) do(ctx context.Context, method, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if h.user != "" || h.pass != "" {
		req.SetBasicAuth(h.user, h.pass)
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("Harbor %s: %w", method, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("Harbor %s %s: HTTP %d", method, strings.SplitN(path, "?", 2)[0], resp.StatusCode)
	}
	if out != nil && len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return resp.StatusCode, fmt.Errorf("Harbor response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

type harborReport struct {
	Severity        string `json:"severity"`
	Vulnerabilities []struct {
		ID          string   `json:"id"`
		Package     string   `json:"package"`
		Version     string   `json:"version"`
		FixVersion  string   `json:"fix_version"`
		Severity    string   `json:"severity"`
		Description string   `json:"description"`
		Links       []string `json:"links"`
	} `json:"vulnerabilities"`
}

// parseHarborVulnerabilities parses the artifact "additions/vulnerabilities"
// document (keyed by report mime type). ok=false means no report exists yet.
func parseHarborVulnerabilities(raw map[string]json.RawMessage) ([]ScanVuln, bool, error) {
	out := []ScanVuln{}
	found := false
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !strings.Contains(k, "vuln") {
			continue
		}
		var rep harborReport
		if err := json.Unmarshal(raw[k], &rep); err != nil {
			return nil, false, fmt.Errorf("invalid Harbor report: %w", err)
		}
		found = true
		for _, v := range rep.Vulnerabilities {
			link := ""
			if len(v.Links) > 0 {
				link = v.Links[0]
			}
			out = append(out, ScanVuln{ID: v.ID, Package: v.Package, Installed: v.Version, Fixed: v.FixVersion,
				Severity: v.Severity, Title: firstLine(v.Description), URL: link})
		}
	}
	return out, found, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// harborScanStatus returns the scan_status of the artifact's vulnerability overview ("" when none).
func (h *harborClient) scanStatus(ctx context.Context, ref imageRef) (string, error) {
	p, err := h.artifactPath(ref)
	if err != nil {
		return "", err
	}
	var art struct {
		ScanOverview map[string]struct {
			ScanStatus string `json:"scan_status"`
		} `json:"scan_overview"`
	}
	if _, err := h.do(ctx, http.MethodGet, p+"?with_scan_overview=true", &art); err != nil {
		return "", err
	}
	for k, v := range art.ScanOverview {
		if strings.Contains(k, "vuln") {
			return v.ScanStatus, nil
		}
	}
	return "", nil
}

// harborVulnerabilities reads an existing Harbor report, optionally starting
// a scan and waiting for it when none exists.
func (h *harborClient) vulnerabilities(ctx context.Context, ref imageRef, trigger bool, logf func(string, ...any)) ([]ScanVuln, error) {
	p, err := h.artifactPath(ref)
	if err != nil {
		return nil, err
	}
	fetch := func() ([]ScanVuln, bool, error) {
		raw := map[string]json.RawMessage{}
		if _, err := h.do(ctx, http.MethodGet, p+"/additions/vulnerabilities", &raw); err != nil {
			return nil, false, err
		}
		return parseHarborVulnerabilities(raw)
	}
	status, err := h.scanStatus(ctx, ref)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(status, "Success") {
		v, ok, err := fetch()
		if err != nil || ok {
			return v, err
		}
		return nil, errors.New("Harbor 扫描完成但没有返回漏洞报告")
	}
	if status == "" || strings.EqualFold(status, "Error") || strings.EqualFold(status, "Stopped") {
		if !trigger {
			return nil, errors.New("Harbor 中该镜像还没有扫描报告（可开启「无报告时触发扫描」）")
		}
		if code, err := h.do(ctx, http.MethodPost, p+"/scan", nil); err != nil && code != http.StatusConflict {
			return nil, err
		}
		logf("Harbor scan requested for %s/%s:%s", ref.Host, ref.Repository, ref.Reference)
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
		status, err = h.scanStatus(ctx, ref)
		if err != nil {
			return nil, err
		}
		switch strings.ToLower(status) {
		case "success":
			v, ok, err := fetch()
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, errors.New("Harbor 扫描完成但没有返回漏洞报告")
			}
			return v, nil
		case "error", "stopped":
			return nil, fmt.Errorf("Harbor 扫描失败（状态 %s）", status)
		}
	}
}

// ---------------------------------------------------------------- stage execution

// scanTarget resolves which image a scan stage scans and which registry
// credential it uses: the stage's own settings, else those of the nearest
// preceding build stage.
func scanTarget(stages []Stage, idx int, a *Application, run *Run) (image string, credentialID int64, registrySecret string) {
	s := stages[idx]
	image, credentialID, registrySecret = s.Image, s.RegistryCredentialID, s.RegistrySecret
	for i := idx - 1; i >= 0; i-- {
		b := stages[i]
		if b.Type != "build" {
			continue
		}
		if image == "" {
			image = b.Image
			if image == "" && a != nil && run != nil {
				image = a.ImageRepo + ":" + run.ImageTag
			}
		}
		if credentialID == 0 && registrySecret == "" {
			credentialID, registrySecret = b.RegistryCredentialID, b.RegistrySecret
		}
		break
	}
	if image == "" && a != nil && run != nil && a.ImageRepo != "" {
		image = a.ImageRepo + ":" + run.ImageTag
	}
	return image, credentialID, registrySecret
}

func validateScanStage(s *Stage, a *Application, stages []Stage, idx int) error {
	if s.Scan == nil {
		s.Scan = &ScanPolicy{}
	}
	if err := s.Scan.normalize(); err != nil {
		return err
	}
	img, cred, _ := scanTarget(stages, idx, a, &Run{ImageTag: "tag"})
	if strings.TrimSpace(img) == "" || strings.HasPrefix(img, ":") {
		return httpx.Validation(fmt.Sprintf("stage %s: scan needs an image or a preceding build stage", s.Name))
	}
	if s.Scan.Source == scanSourceHarbor && cred == 0 {
		return httpx.Validation(fmt.Sprintf("stage %s: harbor scan needs a registry credential (own or from the build stage)", s.Name))
	}
	return nil
}

// scanStage executes a scan stage and stores the result. It returns an error
// when the scanner failed or the gate blocked the image.
func (m *Module) scanStage(ctx context.Context, rt *runnerCtx, run *Run, a *Application, stages []Stage, idx int, job string, logf func(string, ...any)) error {
	s := stages[idx]
	pol := ScanPolicy{}
	if s.Scan != nil {
		pol = *s.Scan
	}
	if err := pol.normalize(); err != nil {
		return err
	}
	image, credID, regSecret := scanTarget(stages, idx, a, run)
	if image == "" {
		return errors.New("no image to scan")
	}
	logf("scanning %s with %s (block: %s, ignored ids: %d)", image, pol.Source, strings.Join(pol.BlockSeverities, ","), len(pol.IgnoreCVEs))
	timeout := time.Duration(s.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	var vulns []ScanVuln
	var err error
	switch pol.Source {
	case scanSourceHarbor:
		vulns, err = m.harborScan(ctx, image, credID, pol, timeout, logf)
	default:
		if rt == nil {
			return errors.New("runner cluster is not available for the Trivy scan")
		}
		registryKey := ""
		if credID != 0 {
			registryKey, regSecret = fmt.Sprintf("dockerconfig-%d", idx), ""
		}
		capture := &reportCapture{}
		capLog := func(f string, args ...any) {
			line := fmt.Sprintf(f, args...)
			if capture.feed(line) {
				return
			}
			logf("%s", line)
		}
		spec := scanPod(s, image, rt.secret, registryKey, regSecret, timeout)
		if err = runJob(ctx, rt.cs, rt.ns, job, run.ID, spec, int(timeout.Seconds())+120, capLog); err == nil {
			var raw []byte
			if raw, err = capture.report(); err == nil {
				vulns, err = parseTrivyJSON(raw)
			}
		}
	}
	if err != nil {
		if ctx.Err() == nil {
			m.saveScan(run.ID, idx, image, pol, ScanSummary{}, "error", nil, redactCICDText(err.Error()))
		}
		return err
	}
	sum, passed, top := evaluateScan(vulns, pol)
	status := "passed"
	if !passed {
		status = "blocked"
	}
	m.saveScan(run.ID, idx, image, pol, sum, status, top, "")
	logf("scan result: critical=%d high=%d medium=%d low=%d unknown=%d ignored=%d blocking=%d",
		sum.Critical, sum.High, sum.Medium, sum.Low, sum.Unknown, sum.Ignored, sum.Blocking)
	if !passed {
		return errors.New(gateMessage(sum, pol))
	}
	return nil
}

func (m *Module) harborScan(ctx context.Context, image string, credID int64, pol ScanPolicy, timeout time.Duration, logf func(string, ...any)) ([]ScanVuln, error) {
	if credID == 0 {
		return nil, errors.New("harbor scan needs a registry credential")
	}
	cr, err := m.loadCredential(ctx, credID)
	if err != nil {
		return nil, fmt.Errorf("load registry credential: %w", err)
	}
	if cr.Type != "registry" {
		return nil, errors.New("credential is not a registry credential")
	}
	ref, err := parseImageRef(image)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(ref.Host, cr.Host) {
		return nil, fmt.Errorf("image registry %s does not match the credential host", ref.Host)
	}
	secret, err := m.credentialSecret(cr)
	if err != nil {
		return nil, err
	}
	hc, err := newHarborClient(cr.Host, cr.Username, secret, pol.Insecure)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return hc.vulnerabilities(sctx, ref, pol.HarborTrigger, logf)
}

func (m *Module) saveScan(runID int64, idx int, image string, pol ScanPolicy, sum ScanSummary, status string, vulns []ScanVuln, errMsg string) {
	if vulns == nil {
		vulns = []ScanVuln{}
	}
	pj, _ := json.Marshal(pol)
	vj, _ := json.Marshal(vulns)
	_, err := m.db.Exec(context.Background(), `INSERT INTO cicd_image_scans(run_id,stage_idx,image,source,status,critical,high,medium,low,unknown,total,ignored,blocking,policy,vulnerabilities,error)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		runID, idx, image, pol.Source, status, sum.Critical, sum.High, sum.Medium, sum.Low, sum.Unknown, sum.Total, sum.Ignored, sum.Blocking, pj, vj, errMsg)
	if err != nil {
		m.log.Error("save image scan", "run", runID, "stage", idx, "err", err)
	}
}

func (m *Module) runScans(ctx context.Context, runID int64) ([]*ImageScan, error) {
	rows, err := m.db.Query(ctx, `SELECT id,run_id,stage_idx,image,source,status,critical,high,medium,low,unknown,total,ignored,blocking,policy,vulnerabilities,error,scanned_at
		FROM cicd_image_scans WHERE run_id=$1 ORDER BY stage_idx, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ImageScan{}
	for rows.Next() {
		var s ImageScan
		var pj, vj []byte
		if err := rows.Scan(&s.ID, &s.RunID, &s.StageIdx, &s.Image, &s.Source, &s.Status, &s.Summary.Critical, &s.Summary.High,
			&s.Summary.Medium, &s.Summary.Low, &s.Summary.Unknown, &s.Summary.Total, &s.Summary.Ignored, &s.Summary.Blocking,
			&pj, &vj, &s.Error, &s.ScannedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(pj, &s.Policy)
		_ = json.Unmarshal(vj, &s.Vulnerabilities)
		if s.Vulnerabilities == nil {
			s.Vulnerabilities = []ScanVuln{}
		}
		s.Image = redactCICDText(s.Image)
		out = append(out, &s)
	}
	return out, rows.Err()
}
