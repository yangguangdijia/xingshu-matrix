package cluster

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"regexp"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const (
	agentTokenPrefix       = "mca_"
	agentDefaultNamespace  = "matrix-system"
	agentDefaultCICDNS     = "matrix-cicd"
	agentServiceAccount    = "matrix-cluster-agent"
	cicdDeployerAccount    = "matrix-cicd-deployer"
	agentTokenSecret       = "matrix-cluster-agent-token"
	agentDefaultImageRepo  = "docker.io/xingshu/matrix-cluster-agent"
	agentManagedByLabel    = "app.kubernetes.io/managed-by"
	agentManagedByValue    = "xingshu-matrix"
	agentComponentLabelKey = "app.kubernetes.io/name"
)

var agentImagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:@-]{0,254}$`)

// newAgentToken returns a random join token and the hash stored for it.
func newAgentToken() (token, hash string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	token = agentTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return token, hashAgentToken(token), nil
}

// hashAgentToken is a plain SHA-256: tokens carry 256 random bits, so no slow KDF is needed.
func hashAgentToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func validAgentTokenFormat(token string) bool {
	return strings.HasPrefix(token, agentTokenPrefix) && len(token) == len(agentTokenPrefix)+43
}

func agentTokenMatches(token, storedHash string) bool {
	if storedHash == "" || !validAgentTokenFormat(token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashAgentToken(token)), []byte(storedHash)) == 1
}

type agentManifestInput struct {
	ServerURL     string `json:"server_url"`
	Image         string `json:"image"`
	Namespace     string `json:"namespace"`
	EnableCICD    bool   `json:"enable_cicd"`
	CICDNamespace string `json:"cicd_namespace"`
	AllowInsecure bool   `json:"allow_insecure_http"`
}

// normalize fills defaults and rejects values that would make the manifest unsafe or invalid.
func (in *agentManifestInput) normalize(defaultImage string) error {
	in.ServerURL = strings.TrimRight(strings.TrimSpace(in.ServerURL), "/")
	u, err := url.Parse(in.ServerURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return errors.New("平台地址必须是不含路径、凭据、查询或片段的 URL，例如 https://matrix.example.com")
	}
	switch u.Scheme {
	case "https":
		in.AllowInsecure = false
	case "http":
		if !in.AllowInsecure {
			return errors.New("平台地址应使用 HTTPS；仅在内网测试时可显式允许 HTTP")
		}
	default:
		return errors.New("平台地址必须以 https:// 开头")
	}
	in.ServerURL = u.Scheme + "://" + u.Host
	if in.Image = strings.TrimSpace(in.Image); in.Image == "" {
		in.Image = defaultImage
	}
	if !agentImagePattern.MatchString(in.Image) {
		return errors.New("agent 镜像地址无效")
	}
	if in.Namespace = strings.TrimSpace(in.Namespace); in.Namespace == "" {
		in.Namespace = agentDefaultNamespace
	}
	if in.CICDNamespace = strings.TrimSpace(in.CICDNamespace); in.CICDNamespace == "" {
		in.CICDNamespace = agentDefaultCICDNS
	}
	for _, ns := range []string{in.Namespace, in.CICDNamespace} {
		if len(validation.IsDNS1123Label(ns)) != 0 {
			return errors.New("命名空间必须是合法的 DNS-1123 名称")
		}
	}
	return nil
}

func agentDefaultImage(version string) string {
	tag := strings.TrimSpace(version)
	if tag == "" || tag == "dev" {
		tag = "latest"
	}
	return agentDefaultImageRepo + ":" + tag
}

// renderAgentManifest returns the YAML a user applies to join a cluster in agent mode.
func renderAgentManifest(clusterName, token string, in agentManifestInput) (string, error) {
	if !validAgentTokenFormat(token) {
		return "", errors.New("invalid agent token")
	}
	labels := map[string]string{agentManagedByLabel: agentManagedByValue, agentComponentLabelKey: agentServiceAccount}
	meta := func(name, ns string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}
	}
	nonRoot, noEscalation, readOnly := true, false, true
	uid := int64(10001)
	replicas := int32(1)
	env := []corev1.EnvVar{
		{Name: "MATRIX_SERVER", Value: in.ServerURL},
		{Name: "MATRIX_CLUSTER_TOKEN_FILE", Value: "/var/run/matrix/token"},
	}
	if in.AllowInsecure {
		env = append(env, corev1.EnvVar{Name: "MATRIX_ALLOW_INSECURE_HTTP", Value: "true"})
	}
	probe := func(path string) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(8081)}},
			PeriodSeconds: 10, FailureThreshold: 3}
	}
	objects := []any{
		&corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
			ObjectMeta: metav1.ObjectMeta{Name: in.Namespace, Labels: labels}},
		&corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
			ObjectMeta: meta(agentServiceAccount, in.Namespace)},
		&rbacv1.ClusterRoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
			ObjectMeta: metav1.ObjectMeta{Name: "matrix-cluster-agent", Labels: labels},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: agentServiceAccount, Namespace: in.Namespace}}},
		&corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: meta(agentTokenSecret, in.Namespace), Type: corev1.SecretTypeOpaque,
			StringData: map[string]string{"token": token}},
		&appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
			ObjectMeta: meta(agentServiceAccount, in.Namespace),
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{agentComponentLabelKey: agentServiceAccount}},
				Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{
						ServiceAccountName: agentServiceAccount,
						SecurityContext:    &corev1.PodSecurityContext{RunAsNonRoot: &nonRoot, RunAsUser: &uid, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
						Containers: []corev1.Container{{
							Name: "agent", Image: in.Image, ImagePullPolicy: corev1.PullIfNotPresent, Env: env,
							Ports:          []corev1.ContainerPort{{Name: "health", ContainerPort: 8081}},
							LivenessProbe:  probe("/healthz"),
							ReadinessProbe: probe("/readyz"),
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
								Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
							},
							SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &noEscalation, ReadOnlyRootFilesystem: &readOnly,
								Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
							VolumeMounts: []corev1.VolumeMount{{Name: "token", MountPath: "/var/run/matrix", ReadOnly: true}},
						}},
						Volumes: []corev1.Volume{{Name: "token", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: agentTokenSecret}}}},
					},
				},
			}},
	}
	if in.EnableCICD {
		objects = append(objects,
			&corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
				ObjectMeta: metav1.ObjectMeta{Name: in.CICDNamespace, Labels: labels}},
			&corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
				ObjectMeta: meta(cicdDeployerAccount, in.CICDNamespace)},
			&rbacv1.ClusterRoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
				ObjectMeta: metav1.ObjectMeta{Name: cicdDeployerAccount, Labels: labels},
				RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
				Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: cicdDeployerAccount, Namespace: in.CICDNamespace}}},
		)
	}
	var b strings.Builder
	b.WriteString("# Matrix 集群接入清单：" + strings.ReplaceAll(clusterName, "\n", " ") + "\n")
	b.WriteString("# 含一次性接入令牌，请勿提交到代码仓库。重新生成清单会使旧令牌失效。\n")
	b.WriteString("# agent 使用 cluster-admin，以便平台管理节点、工作负载、CRD 与 Helm；平台侧仍按 RBAC、环境与审批控制写操作。\n")
	for _, obj := range objects {
		out, err := yaml.Marshal(obj)
		if err != nil {
			return "", err
		}
		b.WriteString("---\n")
		b.WriteString(stripEmptyStatus(string(out)))
	}
	return b.String(), nil
}

func stripEmptyStatus(doc string) string {
	lines := strings.Split(doc, "\n")
	out := lines[:0]
	for _, line := range lines {
		if line == "status: {}" || strings.TrimSpace(line) == "creationTimestamp: null" {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
