// Package platform exposes platform-level settings that need side effects
// outside the database — currently the访问域名 (Ingress host) of the platform.
//
// 设计：域名本身存在 settings 表（platform.domain），保存时顺带把 Ingress
// 收敛到目标状态。集群里没有 Ingress 控制器、或进程不在集群内运行时，保存
// 依然成功，只是返回 skipped —— 前端据此提示改用 IP:NodePort 访问。
package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/settings"
)

const (
	// DomainKey is the settings key holding the platform's访问域名.
	DomainKey = "platform.domain"

	ingressName  = "xingshu-matrix"
	serviceName  = "xingshu-matrix"
	servicePort  = 80
	defaultNS    = "xingshu-system"
	defaultTLS   = "xingshu-matrix-tls"
	probeTimeout = 6 * time.Second
)

// hostRe matches a DNS host name (RFC 1123), no scheme / path / port.
var hostRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// State is the read model returned by GET/PUT /platform/domain.
type State struct {
	Domain           string `json:"domain"`
	IngressAvailable bool   `json:"ingress_available"`
	IngressClass     string `json:"ingress_class,omitempty"`
	Namespace        string `json:"namespace,omitempty"`
	TLSSecret        string `json:"tls_secret,omitempty"`
	Status           string `json:"status"` // ok | skipped | created | updated | removed
	Message          string `json:"message,omitempty"`
	NodePort         string `json:"nodeport,omitempty"`
}

// Register mounts the platform routes on the authenticated group.
func Register(r *gin.RouterGroup, s *settings.Service) {
	registerEnv(r)
	registerNotices(r, s)

	r.GET("/platform/domain", rbac.RequirePerm("setting:read"), func(c *gin.Context) {
		var domain string
		_ = s.Get(c.Request.Context(), DomainKey, &domain)
		st := probe(c.Request.Context())
		st.Domain = domain
		if st.IngressAvailable && domain == "" {
			st.Status, st.Message = "ok", "尚未设置域名；当前请使用 IP:NodePort 访问"
		}
		if st.IngressAvailable && domain != "" {
			st.Status, st.Message = "ok", "域名已生效，Ingress 由平台自动维护"
		}
		httpx.OK(c, st)
	})

	r.PUT("/platform/domain", rbac.RequirePerm("setting:manage"), func(c *gin.Context) {
		var in struct {
			Domain string `json:"domain" binding:"max=253"`
		}
		if !httpx.Bind(c, &in) {
			return
		}
		domain := strings.ToLower(strings.TrimSpace(in.Domain))
		domain = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(domain, "https://"), "http://"), "/")
		if i := strings.IndexByte(domain, '/'); i >= 0 {
			domain = domain[:i]
		}
		if i := strings.IndexByte(domain, ':'); i >= 0 {
			domain = domain[:i]
		}
		if ip := net.ParseIP(domain); ip != nil {
			httpx.Fail(c, httpx.BadRequest("请填写域名而不是 IP；IP 访问请直接使用 NodePort"))
			return
		}
		if domain != "" && !hostRe.MatchString(domain) {
			httpx.Fail(c, httpx.BadRequest("域名格式不合法，示例：xingshu-matrix.com"))
			return
		}

		raw, _ := json.Marshal(domain)
		audit.SetResource(c, "platform", "domain")
		audit.SetDetail(c, gin.H{"domain": domain})
		if _, err := s.Put(c.Request.Context(), DomainKey, raw, false, "平台访问域名（保存后自动维护 Ingress）", rbac.PrincipalFrom(c).UserID); err != nil {
			httpx.Fail(c, err)
			return
		}

		st, err := reconcile(c.Request.Context(), domain)
		st.Domain = domain
		if err != nil {
			// 设置已保存，Ingress 失败不阻断：把原因回给前端。
			st.Status = "skipped"
			st.Message = err.Error()
		}
		httpx.OK(c, st)
	})
}

// probe reports whether this process can manage Ingress objects.
func probe(ctx context.Context) State {
	st := State{Namespace: namespace(), TLSSecret: tlsSecret(), NodePort: os.Getenv("MATRIX_NODEPORT")}
	client, err := client()
	if err != nil {
		st.Status, st.Message = "skipped", "未运行在 Kubernetes 集群内，无法自动维护 Ingress；请使用 IP:NodePort 访问"
		return st
	}
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	classes, err := client.NetworkingV1().IngressClasses().List(cctx, metav1.ListOptions{})
	if err != nil || len(classes.Items) == 0 {
		st.Status, st.Message = "skipped", "集群内没有可用的 IngressClass，域名无法生效；请使用 IP:NodePort 访问"
		return st
	}
	st.IngressAvailable = true
	st.IngressClass = classes.Items[0].Name
	return st
}

// reconcile brings the platform Ingress in line with domain.
// domain == "" 表示"不启用域名"，此时删除平台自己的 Ingress。
func reconcile(ctx context.Context, domain string) (State, error) {
	st := probe(ctx)
	if !st.IngressAvailable {
		return st, errors.New(st.Message)
	}
	client, err := client()
	if err != nil {
		return st, err
	}
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	ings := client.NetworkingV1().Ingresses(st.Namespace)
	if domain == "" {
		err := ings.Delete(cctx, ingressName, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return st, err
		}
		st.Status, st.Message = "removed", "已清除域名，Ingress 已删除；请使用 IP:NodePort 访问"
		return st, nil
	}

	want := desired(domain, st.Namespace, st.IngressClass, st.TLSSecret)
	cur, err := ings.Get(cctx, ingressName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := ings.Create(cctx, want, metav1.CreateOptions{}); err != nil {
			return st, err
		}
		st.Status, st.Message = "created", "已创建 Ingress（自动申请/复用自签证书）"
		return st, nil
	}
	if err != nil {
		return st, err
	}
	// 保留人工添加的 annotation，只收敛 spec。
	cur.Spec = want.Spec
	if cur.Annotations == nil {
		cur.Annotations = map[string]string{}
	}
	for k, v := range want.Annotations {
		cur.Annotations[k] = v
	}
	if _, err := ings.Update(cctx, cur, metav1.UpdateOptions{}); err != nil {
		return st, err
	}
	st.Status, st.Message = "updated", "已更新 Ingress 域名"
	return st, nil
}

func desired(domain, ns, class, tls string) *networkingv1.Ingress {
	p := networkingv1.PathTypePrefix
	ann := map[string]string{
		"nginx.ingress.kubernetes.io/proxy-body-size":    "50m",
		"nginx.ingress.kubernetes.io/proxy-read-timeout": "3600",
		"nginx.ingress.kubernetes.io/ssl-redirect":       "true",
		"nginx.ingress.kubernetes.io/force-ssl-redirect": "true",
	}
	if class == "traefik" {
		ann = map[string]string{"traefik.ingress.kubernetes.io/router.entrypoints": "websecure"}
	}
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: ingressName, Namespace: ns, Annotations: ann},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &class,
			TLS:              []networkingv1.IngressTLS{{Hosts: []string{domain}, SecretName: tls}},
			Rules: []networkingv1.IngressRule{{
				Host: domain,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
						Path:     "/",
						PathType: &p,
						Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
							Name: serviceName,
							Port: networkingv1.ServiceBackendPort{Number: servicePort},
						}},
					}}},
				},
			}},
		},
	}
}

func client() (*kubernetes.Clientset, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	cfg.Timeout = probeTimeout
	return kubernetes.NewForConfig(cfg)
}

func namespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return defaultNS
}

func tlsSecret() string {
	if v := os.Getenv("MATRIX_TLS_SECRET"); v != "" {
		return v
	}
	return defaultTLS
}
