package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const certificatePurposeAnnotation = "matrix.xingshu.io/certificate-purpose"

type certificateImportInput struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Purpose     string `json:"purpose"`
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"private_key"`
}

type certificateListItem struct {
	Namespace   string   `json:"namespace"`
	Name        string   `json:"name"`
	Purpose     string   `json:"purpose,omitempty"`
	DNSNames    []string `json:"dns_names,omitempty"`
	IPAddresses []string `json:"ip_addresses,omitempty"`
	CommonName  string   `json:"common_name,omitempty"`
	Issuer      string   `json:"issuer,omitempty"`
	NotBefore   string   `json:"not_before,omitempty"`
	NotAfter    string   `json:"not_after,omitempty"`
	Source      string   `json:"source"`
	IngressRefs []string `json:"ingress_refs,omitempty"`
	Status      string   `json:"status"`
	ParseError  string   `json:"parse_error,omitempty"`
}

// Cache parsed metadata only. Kubernetes Secret.Data must never enter this map.
var certificateMetadataCache = struct {
	sync.Mutex
	entries map[string]certificateMetadataEntry
	flights map[string]*certificateMetadataFlight
}{entries: make(map[string]certificateMetadataEntry), flights: make(map[string]*certificateMetadataFlight)}

type certificateMetadataEntry struct {
	expires time.Time
	items   []certificateListItem
}

type certificateMetadataFlight struct {
	done   chan struct{}
	cancel context.CancelFunc
	items  []certificateListItem
	err    error
}

const certificateMetadataTTL = 2 * time.Minute
const certificateReadTimeout = 15 * time.Second

func certificateCacheKey(m *Module, cl *Cluster) string {
	return fmt.Sprintf("%p/%d/%s", m, cl.ID, cl.UpdatedAt.UTC().Format(time.RFC3339Nano))
}

func certificateMetadataView(items []certificateListItem, now time.Time) []certificateListItem {
	out := make([]certificateListItem, len(items))
	for i, item := range items {
		item.DNSNames = append([]string(nil), item.DNSNames...)
		item.IPAddresses = append([]string(nil), item.IPAddresses...)
		item.IngressRefs = append([]string(nil), item.IngressRefs...)
		// Validity changes with the clock even while parsed metadata is cached.
		if item.ParseError == "" && item.NotBefore != "" && item.NotAfter != "" {
			start, startErr := time.Parse(time.RFC3339, item.NotBefore)
			end, endErr := time.Parse(time.RFC3339, item.NotAfter)
			switch {
			case startErr != nil || endErr != nil:
				item.Status = "invalid"
			case !now.Before(end):
				item.Status = "已过期"
			case now.Before(start):
				item.Status = "尚未生效"
			default:
				item.Status = "有效"
			}
		}
		out[i] = item
	}
	return out
}

func certificateMetadataGet(key string, now time.Time) ([]certificateListItem, bool) {
	certificateMetadataCache.Lock()
	defer certificateMetadataCache.Unlock()
	entry, ok := certificateMetadataCache.entries[key]
	if !ok || !now.Before(entry.expires) {
		delete(certificateMetadataCache.entries, key)
		return nil, false
	}
	return certificateMetadataView(entry.items, now), true
}

func certificateMetadataPut(key string, items []certificateListItem, now time.Time) {
	certificateMetadataCache.Lock()
	defer certificateMetadataCache.Unlock()
	certificateMetadataPutLocked(key, items, now)
}

func certificateMetadataPutLocked(key string, items []certificateListItem, now time.Time) {
	for oldKey, entry := range certificateMetadataCache.entries {
		if !now.Before(entry.expires) {
			delete(certificateMetadataCache.entries, oldKey)
		}
	}
	certificateMetadataCache.entries[key] = certificateMetadataEntry{
		expires: now.Add(certificateMetadataTTL), items: certificateMetadataView(items, now),
	}
}

func certificateMetadataInvalidate(key string) {
	certificateMetadataCache.Lock()
	delete(certificateMetadataCache.entries, key)
	if call := certificateMetadataCache.flights[key]; call != nil {
		delete(certificateMetadataCache.flights, key)
		call.cancel()
	}
	certificateMetadataCache.Unlock()
}

func certificateMetadataInvalidateCluster(m *Module, id int64) {
	prefix := fmt.Sprintf("%p/%d/", m, id)
	certificateMetadataCache.Lock()
	defer certificateMetadataCache.Unlock()
	for key := range certificateMetadataCache.entries {
		if strings.HasPrefix(key, prefix) {
			delete(certificateMetadataCache.entries, key)
		}
	}
	for key, call := range certificateMetadataCache.flights {
		if strings.HasPrefix(key, prefix) {
			delete(certificateMetadataCache.flights, key)
			call.cancel()
		}
	}
}

// Each process shares one bounded scan per cluster revision. A disconnected
// waiter can leave without cancelling other callers or retaining Secret.Data.
func certificateMetadataLoad(ctx context.Context, key string, timeout time.Duration, load func(context.Context) ([]certificateListItem, error)) ([]certificateListItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	certificateMetadataCache.Lock()
	now := time.Now()
	if entry, ok := certificateMetadataCache.entries[key]; ok && now.Before(entry.expires) {
		certificateMetadataCache.Unlock()
		return certificateMetadataView(entry.items, now), nil
	}
	delete(certificateMetadataCache.entries, key)
	call := certificateMetadataCache.flights[key]
	if call == nil {
		scanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		call = &certificateMetadataFlight{done: make(chan struct{}), cancel: cancel}
		certificateMetadataCache.flights[key] = call
		go func() {
			defer cancel()
			items, err := load(scanCtx)
			if scanCtx.Err() != nil {
				err = scanCtx.Err()
			}
			certificateMetadataCache.Lock()
			if certificateMetadataCache.flights[key] != call {
				items, err = nil, context.Canceled
			} else {
				delete(certificateMetadataCache.flights, key)
				if err == nil {
					certificateMetadataPutLocked(key, items, time.Now())
				}
			}
			call.items, call.err = items, err
			close(call.done)
			certificateMetadataCache.Unlock()
		}()
	}
	certificateMetadataCache.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-call.done:
		if call.err != nil {
			return nil, call.err
		}
		return certificateMetadataView(call.items, time.Now()), nil
	}
}

func (m *Module) listCertificates(c *gin.Context) {
	cl, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	items, err := certificateMetadataLoad(c.Request.Context(), certificateCacheKey(m, cl), certificateReadTimeout,
		func(ctx context.Context) ([]certificateListItem, error) { return readCertificateMetadata(ctx, kc) })
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	respondCertificateList(c, items)
}

func readCertificateMetadata(ctx context.Context, kc *kubeClients) ([]certificateListItem, error) {
	var secrets *corev1.SecretList
	var ingresses *networkingv1.IngressList
	var secretErr, ingressErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		secrets, secretErr = kc.cs.CoreV1().Secrets("").List(ctx, metav1.ListOptions{FieldSelector: "type=" + string(corev1.SecretTypeTLS)})
	}()
	go func() {
		defer wg.Done()
		ingresses, ingressErr = kc.cs.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
	}()
	wg.Wait()
	if secretErr != nil {
		return nil, secretErr
	}
	if ingressErr != nil {
		return nil, ingressErr
	}

	refs := make(map[string][]string)
	for _, ingress := range ingresses.Items {
		for _, tlsConfig := range ingress.Spec.TLS {
			if tlsConfig.SecretName == "" {
				continue
			}
			key := ingress.Namespace + "/" + tlsConfig.SecretName
			refs[key] = append(refs[key], ingress.Namespace+"/"+ingress.Name)
		}
	}

	items := make([]certificateListItem, 0)
	for _, secret := range secrets.Items {
		if secret.Type != corev1.SecretTypeTLS {
			continue
		}
		item := certificateListItem{
			Namespace:   secret.Namespace,
			Name:        secret.Name,
			Purpose:     secret.Annotations[certificatePurposeAnnotation],
			Source:      "Kubernetes TLS Secret",
			Status:      "invalid",
			IngressRefs: refs[secret.Namespace+"/"+secret.Name],
		}
		sort.Strings(item.IngressRefs)
		if item.Purpose == "" && len(item.IngressRefs) > 0 {
			item.Purpose = "Ingress TLS"
		}
		cert, parseErr := parseLeafCertificate(secret.Data[corev1.TLSCertKey])
		switch {
		case len(secret.Data[corev1.TLSCertKey]) == 0:
			item.ParseError = "缺少 tls.crt"
		case parseErr != nil:
			item.ParseError = "tls.crt 不是有效的 X.509 PEM 证书"
		default:
			item.DNSNames = append([]string(nil), cert.DNSNames...)
			for _, address := range cert.IPAddresses {
				item.IPAddresses = append(item.IPAddresses, address.String())
			}
			item.CommonName = cert.Subject.CommonName
			item.Issuer = cert.Issuer.String()
			item.NotBefore = cert.NotBefore.UTC().Format(time.RFC3339)
			item.NotAfter = cert.NotAfter.UTC().Format(time.RFC3339)
			now := time.Now()
			switch {
			case now.Before(cert.NotBefore):
				item.Status = "尚未生效"
			case !now.Before(cert.NotAfter):
				item.Status = "已过期"
			default:
				item.Status = "有效"
			}
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace == items[j].Namespace {
			return items[i].Name < items[j].Name
		}
		return items[i].Namespace < items[j].Namespace
	})
	return items, nil
}

func respondCertificateList(c *gin.Context, items []certificateListItem) {
	httpx.OK(c, gin.H{
		"items": items,
		"cert_manager": gin.H{
			"state":         "not_implemented",
			"can_issue":     false,
			"issue_message": "本轮未接入 cert-manager Certificate 读取或签发；当前仅展示 Kubernetes TLS Secret。",
		},
	})
}

func (m *Module) importCertificate(c *gin.Context) {
	cl, ok := m.clusterFor(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20)
	var input certificateImportInput
	if !httpx.Bind(c, &input) {
		return
	}
	if err := validateConfigIdentity(input.Namespace, input.Name); err != nil {
		httpx.Fail(c, err)
		return
	}
	input.Purpose = strings.TrimSpace(input.Purpose)
	if len(input.Purpose) > 160 {
		httpx.Fail(c, httpx.BadRequest("purpose must be 160 characters or fewer"))
		return
	}
	if configSize(map[string]string{
		corev1.TLSCertKey:       input.Certificate,
		corev1.TLSPrivateKeyKey: input.PrivateKey,
	}) > maxConfigBytes {
		httpx.Fail(c, httpx.BadRequest("certificate and private key exceed 1 MiB"))
		return
	}
	if _, err := validateCertificateMaterial(input.Certificate, input.PrivateKey); err != nil {
		httpx.Fail(c, err)
		return
	}

	kc, err := m.kube(c.Request.Context(), cl)
	if err != nil {
		httpx.Fail(c, httpx.BadRequest("cluster client: "+err.Error()))
		return
	}
	params := secretParams{
		Name: input.Name, Namespace: input.Namespace, Type: string(corev1.SecretTypeTLS),
		Data: map[string]string{
			corev1.TLSCertKey:       input.Certificate,
			corev1.TLSPrivateKeyKey: input.PrivateKey,
		},
	}
	current, getErr := kc.cs.CoreV1().Secrets(input.Namespace).Get(c.Request.Context(), input.Name, metav1.GetOptions{})
	action := "secret_create"
	if getErr == nil {
		if current.Type != corev1.SecretTypeTLS {
			httpx.Fail(c, httpx.BadRequest("existing Secret is not kubernetes.io/tls"))
			return
		}
		if current.Immutable != nil && *current.Immutable &&
			(string(current.Data[corev1.TLSCertKey]) != input.Certificate ||
				string(current.Data[corev1.TLSPrivateKeyKey]) != input.PrivateKey) {
			httpx.Fail(c, httpx.BadRequest("immutable TLS Secret cannot be changed"))
			return
		}
		action = "secret_update"
		params.UID = current.UID
		params.ResourceVersion = current.ResourceVersion
		params.Annotations = make(map[string]string, len(current.Annotations)+1)
		for key, value := range current.Annotations {
			params.Annotations[key] = value
		}
	} else if !apierrors.IsNotFound(getErr) {
		httpx.Fail(c, kubeErr(getErr))
		return
	}
	if input.Purpose != "" {
		if params.Annotations == nil {
			params.Annotations = map[string]string{}
		}
		params.Annotations[certificatePurposeAnnotation] = input.Purpose
	}
	raw, err := json.Marshal(params)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	pl := changePayload{
		ClusterID: cl.ID, Cluster: cl.Name, Action: action,
		Namespace: input.Namespace, Kind: "secrets", Name: input.Name, Params: raw,
	}
	if err := validateChange(&pl); err != nil {
		httpx.Fail(c, err)
		return
	}
	if err := m.encryptSecretParams(&pl); err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetAction(c, "certificate_import")
	audit.SetResource(c, "certificate", input.Namespace+"/"+input.Name)
	audit.SetDetail(c, gin.H{
		"namespace": input.Namespace,
		"name":      input.Name,
		"operation": action,
	})
	_, ticket, err := m.runChange(c.Request.Context(), rbac.PrincipalFrom(c), cl, pl, c.Query("reason"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	certificateMetadataInvalidateCluster(m, cl.ID)
	if ticket != nil {
		approval.RespondPending(c, ticket)
		return
	}
	httpx.OK(c, gin.H{
		"namespace": input.Namespace,
		"name":      input.Name,
		"type":      corev1.SecretTypeTLS,
	})
}

func validateCertificateMaterial(certificatePEM, privateKeyPEM string) (*x509.Certificate, error) {
	if strings.TrimSpace(certificatePEM) == "" || strings.TrimSpace(privateKeyPEM) == "" {
		return nil, httpx.BadRequest("certificate and private_key PEM are required")
	}
	if _, err := tls.X509KeyPair([]byte(certificatePEM), []byte(privateKeyPEM)); err != nil {
		return nil, httpx.BadRequest("certificate or private_key PEM is invalid, or the key does not match")
	}
	cert, err := parseLeafCertificate([]byte(certificatePEM))
	if err != nil {
		return nil, httpx.BadRequest("certificate PEM is not a valid X.509 certificate")
	}
	if len(cert.DNSNames) == 0 && len(cert.IPAddresses) == 0 && cert.Subject.CommonName == "" {
		return nil, httpx.BadRequest("certificate must contain a DNS name, IP SAN, or Common Name")
	}
	for _, name := range cert.DNSNames {
		if !validCertificateDNSName(name) {
			return nil, httpx.BadRequest("certificate contains an invalid DNS SAN")
		}
	}
	if cn := cert.Subject.CommonName; cn != "" && net.ParseIP(cn) == nil && !validCertificateDNSName(cn) {
		return nil, httpx.BadRequest("certificate contains an invalid Common Name")
	}
	return cert, nil
}

func validCertificateDNSName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(name, "*.") {
		name = strings.TrimPrefix(name, "*.")
		if strings.Contains(name, "*") {
			return false
		}
	} else if strings.Contains(name, "*") {
		return false
	}
	return len(validation.IsDNS1123Subdomain(name)) == 0
}

func parseLeafCertificate(certificatePEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, httpx.BadRequest("invalid certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	return cert, nil
}
