package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func testCertificatePEM(t *testing.T, dnsNames []string, commonName string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: commonName},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return string(certPEM), string(keyPEM)
}

func TestValidateCertificateMaterial(t *testing.T) {
	certPEM, keyPEM := testCertificatePEM(t, []string{"api.example.com", "*.apps.example.com"}, "api.example.com")
	cert, err := validateCertificateMaterial(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "api.example.com" || len(cert.DNSNames) != 2 {
		t.Fatalf("unexpected parsed certificate identity: %#v", cert)
	}
}

func TestValidateCertificateMaterialRejectsBadPEMAndMismatchedKey(t *testing.T) {
	certPEM, _ := testCertificatePEM(t, []string{"api.example.com"}, "api.example.com")
	_, otherKeyPEM := testCertificatePEM(t, []string{"api.example.com"}, "api.example.com")
	for name, material := range map[string][2]string{
		"invalid PEM":    {"not a certificate", otherKeyPEM},
		"mismatched key": {certPEM, otherKeyPEM},
		"missing key":    {certPEM, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateCertificateMaterial(material[0], material[1]); err == nil {
				t.Fatal("expected invalid certificate material to be rejected")
			}
		})
	}
}

func TestValidateCertificateMaterialRejectsInvalidDNSNames(t *testing.T) {
	certPEM, keyPEM := testCertificatePEM(t, []string{"bad*.example.com"}, "bad*.example.com")
	if _, err := validateCertificateMaterial(certPEM, keyPEM); err == nil {
		t.Fatal("invalid DNS SAN was accepted")
	}
	for _, name := range []string{"api.example.com", "*.example.com"} {
		if !validCertificateDNSName(name) {
			t.Errorf("valid DNS name %q was rejected", name)
		}
	}
	for _, name := range []string{"", "*", "foo.*.example.com", "bad name.example.com"} {
		if validCertificateDNSName(name) {
			t.Errorf("invalid DNS name %q was accepted", name)
		}
	}
}

func TestValidateCertificateImportIdentity(t *testing.T) {
	if err := validateConfigIdentity("apps", "web-tls"); err != nil {
		t.Fatalf("valid Secret identity rejected: %v", err)
	}
	for _, identity := range [][2]string{{"bad namespace", "web-tls"}, {"apps", "bad/name"}} {
		if err := validateConfigIdentity(identity[0], identity[1]); err == nil {
			t.Errorf("invalid identity %q/%q was accepted", identity[0], identity[1])
		}
	}
}

func TestCertificateListItemDoesNotSerializePrivateKey(t *testing.T) {
	encoded, err := json.Marshal(certificateListItem{
		Namespace: "apps", Name: "web-tls", Source: "Kubernetes TLS Secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "tls.key") {
		t.Fatalf("certificate metadata response exposed a private-key field: %s", encoded)
	}
}

func TestCertificateMetadataCacheExpiresAndInvalidates(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { certificateMetadataInvalidate(key) })
	now := time.Now()
	items := []certificateListItem{{Namespace: "apps", Name: "web-tls", Status: "有效"}}
	certificateMetadataPut(key, items, now)
	got, ok := certificateMetadataGet(key, now.Add(time.Second))
	if !ok || len(got) != 1 || got[0].Name != "web-tls" {
		t.Fatal("metadata cache miss")
	}
	encoded, err := json.Marshal(got)
	if err != nil || strings.Contains(string(encoded), "tls.key") || strings.Contains(string(encoded), "private_key") {
		t.Fatalf("cache contains secret material: %s %v", encoded, err)
	}
	if _, ok := certificateMetadataGet(key, now.Add(certificateMetadataTTL)); ok {
		t.Fatal("expired metadata was returned")
	}
	certificateMetadataInvalidate(key)
	if _, ok := certificateMetadataGet(key, now); ok {
		t.Fatal("invalidated metadata was returned")
	}
}

func TestCertificateMetadataColdRequestsShareOneSecretAndIngressScan(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { certificateMetadataInvalidate(key) })
	cert, privateKey := testCertificatePEM(t, []string{"api.example.com"}, "api.example.com")
	cs := fake.NewSimpleClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "web-tls"},
			Type: corev1.SecretTypeTLS, Data: map[string][]byte{corev1.TLSCertKey: []byte(cert), corev1.TLSPrivateKeyKey: []byte(privateKey)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "opaque"},
			Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"password": []byte("never-expose")}},
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "web"},
			Spec: networkingv1.IngressSpec{TLS: []networkingv1.IngressTLS{{SecretName: "web-tls"}}}},
	)
	kc := &kubeClients{cs: cs}
	var scans atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	load := func(ctx context.Context) ([]certificateListItem, error) {
		if scans.Add(1) == 1 {
			close(started)
		}
		<-release
		return readCertificateMetadata(ctx, kc)
	}
	const callers = 32
	results, failures := make(chan []certificateListItem, callers), make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, err := certificateMetadataLoad(context.Background(), key, time.Second, load)
			results <- items
			failures <- err
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	for rows := range results {
		if len(rows) != 1 || rows[0].Name != "web-tls" || len(rows[0].IngressRefs) != 1 || rows[0].Purpose != "Ingress TLS" {
			t.Fatalf("incorrect certificate inventory: %#v", rows)
		}
		data, err := json.Marshal(rows)
		if err != nil || strings.Contains(string(data), "BEGIN ") || strings.Contains(string(data), "tls.key") || strings.Contains(string(data), "never-expose") {
			t.Fatal("certificate inventory exposed Secret material")
		}
	}
	if scans.Load() != 1 {
		t.Fatalf("cold callers started %d scans", scans.Load())
	}
	secretLists, ingressLists := 0, 0
	for _, action := range cs.Actions() {
		if action.GetVerb() != "list" {
			continue
		}
		switch action.GetResource().Resource {
		case "secrets":
			secretLists++
			if got := action.(k8stesting.ListAction).GetListRestrictions().Fields.String(); got != "type=kubernetes.io/tls" {
				t.Fatalf("unbounded Secret type scan: %s", got)
			}
		case "ingresses":
			ingressLists++
		}
	}
	if secretLists != 1 || ingressLists != 1 {
		t.Fatalf("duplicated Kubernetes scans: Secret=%d Ingress=%d", secretLists, ingressLists)
	}
}

func TestCertificateMetadataClusterRevisionAndInvalidation(t *testing.T) {
	m, otherModule := New(), New()
	cl := &Cluster{ID: 42, UpdatedAt: time.Now().UTC()}
	oldKey := certificateCacheKey(m, cl)
	cl.UpdatedAt = cl.UpdatedAt.Add(time.Nanosecond)
	newKey := certificateCacheKey(m, cl)
	otherKey := certificateCacheKey(otherModule, cl)
	t.Cleanup(func() {
		certificateMetadataInvalidateCluster(m, cl.ID)
		certificateMetadataInvalidateCluster(otherModule, cl.ID)
	})
	if oldKey == newKey || newKey == otherKey {
		t.Fatal("cluster revision or module identity is absent from cache key")
	}
	for _, key := range []string{oldKey, newKey, otherKey} {
		certificateMetadataPut(key, []certificateListItem{{Name: key}}, time.Now())
	}
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	certificateMetadataInvalidate(oldKey)
	go func() {
		_, err := certificateMetadataLoad(context.Background(), oldKey, time.Second, func(ctx context.Context) ([]certificateListItem, error) {
			close(started)
			<-release // Simulate a late response from a transport ignoring cancellation.
			return []certificateListItem{{Name: "stale"}}, nil
		})
		done <- err
	}()
	<-started
	certificateMetadataInvalidateCluster(m, cl.ID)
	fresh, err := certificateMetadataLoad(context.Background(), oldKey, time.Second, func(context.Context) ([]certificateListItem, error) {
		return []certificateListItem{{Name: "fresh"}}, nil
	})
	if err != nil || len(fresh) != 1 || fresh[0].Name != "fresh" {
		t.Fatalf("replacement scan failed: %v", err)
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("invalidated scan returned old success: %v", err)
	}
	if rows, ok := certificateMetadataGet(oldKey, time.Now()); !ok || rows[0].Name != "fresh" {
		t.Fatal("late scan overwrote fresh cache")
	}
	if _, ok := certificateMetadataGet(newKey, time.Now()); ok {
		t.Fatal("cluster invalidation left another revision cached")
	}
	if _, ok := certificateMetadataGet(otherKey, time.Now()); !ok {
		t.Fatal("cluster invalidation affected another module")
	}
}

func TestCertificateMetadataCancelledCallerDoesNotPoisonSharedScan(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { certificateMetadataInvalidate(key) })
	started, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := certificateMetadataLoad(ctx, key, time.Second, func(scanCtx context.Context) ([]certificateListItem, error) {
			close(started)
			select {
			case <-release:
				return []certificateListItem{{Name: "shared"}}, nil
			case <-scanCtx.Done():
				return nil, scanCtx.Err()
			}
		})
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter was not released: %v", err)
	}
	close(release)
	rows, err := certificateMetadataLoad(context.Background(), key, time.Second, func(context.Context) ([]certificateListItem, error) {
		t.Error("cancelled caller started a replacement scan")
		return nil, errors.New("duplicate scan")
	})
	if err != nil || len(rows) != 1 || rows[0].Name != "shared" {
		t.Fatalf("shared result lost after one caller cancelled: %v", err)
	}
}

func TestCertificateMetadataTimeoutAndFailuresAreNotCached(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { certificateMetadataInvalidate(key) })
	_, err := certificateMetadataLoad(context.Background(), key, 20*time.Millisecond, func(ctx context.Context) ([]certificateListItem, error) {
		<-ctx.Done()
		return []certificateListItem{{Name: "partial"}}, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("scan did not respect timeout: %v", err)
	}
	now := time.Now()
	certificateMetadataPut(key, []certificateListItem{{Name: "old"}}, now.Add(-certificateMetadataTTL))
	for i := 0; i < 2; i++ {
		want := errors.New("discovery failed")
		_, err := certificateMetadataLoad(context.Background(), key, time.Second, func(context.Context) ([]certificateListItem, error) {
			return []certificateListItem{{Name: "partial"}}, want
		})
		if !errors.Is(err, want) {
			t.Fatalf("failed scan returned stale/partial success: %v", err)
		}
		if _, ok := certificateMetadataGet(key, time.Now()); ok {
			t.Fatal("failed or expired data was cached")
		}
	}
}

func TestCertificateMetadataViewReevaluatesExpiryAndOwnsSlices(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { certificateMetadataInvalidate(key) })
	now := time.Now().UTC().Truncate(time.Second)
	expiry := now.Add(30 * time.Second)
	items := []certificateListItem{{Name: "web-tls", Status: "有效",
		NotBefore: now.Add(-time.Minute).Format(time.RFC3339), NotAfter: expiry.Format(time.RFC3339),
		DNSNames: []string{"api.example.com"}, IngressRefs: []string{"apps/web"}}}
	certificateMetadataPut(key, items, now)
	items[0].DNSNames[0] = "mutated"
	before, _ := certificateMetadataGet(key, now)
	if before[0].Status != "有效" || before[0].DNSNames[0] != "api.example.com" {
		t.Fatal("cache owns mutable input slices or changed valid status")
	}
	before[0].IngressRefs[0] = "mutated"
	atExpiry, ok := certificateMetadataGet(key, expiry)
	if !ok || atExpiry[0].Status != "已过期" || atExpiry[0].IngressRefs[0] != "apps/web" {
		t.Fatal("cached certificate stayed healthy at expiration or leaked mutable output")
	}
}

func TestCertificateScanFailsClosedWhenIngressReadIsForbidden(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "ingresses", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "ingresses"}, "", errors.New("denied"))
	})
	rows, err := readCertificateMetadata(context.Background(), &kubeClients{cs: cs})
	if err == nil || !apierrors.IsForbidden(err) || rows != nil {
		t.Fatalf("failed scan became a healthy empty list: %#v %v", rows, err)
	}
}

func TestClusterResourceRoutesRegisterWithoutConflicts(t *testing.T) {
	router := gin.New()
	New().Register(router.Group("/api/v1"), &app.Deps{})
	got := make(map[string]bool)
	for _, route := range router.Routes() {
		got[route.Method+" "+route.Path] = true
	}
	for _, route := range []string{
		"GET /api/v1/clusters/:id/helm/releases",
		"GET /api/v1/clusters/:id/crds",
		"GET /api/v1/clusters/:id/projects",
		"POST /api/v1/clusters/:id/quotas",
		"PATCH /api/v1/clusters/:id/limitranges/:ns/:name",
		"GET /api/v1/clusters/:id/components/catalog",
		"POST /api/v1/clusters/:id/components/:component/preflight",
		"POST /api/v1/clusters/:id/components/:component/install",
		"GET /api/v1/clusters/:id/components/operations/:operation",
	} {
		if !got[route] {
			t.Errorf("missing route %s", route)
		}
	}
}
