package cluster

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubeFake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func testHelmSecret(t *testing.T, revision int, compressed bool) *corev1.Secret {
	t.Helper()
	raw := []byte(fmt.Sprintf(`{
		"name":"web","namespace":"apps","version":%d,
		"info":{"status":"deployed","last_deployed":"2026-10-29T01:00:00Z","description":"private-marker"},
		"chart":{"metadata":{"name":"nginx","version":"1.2.3","appVersion":"1.25"},"templates":[{"data":"private-marker"}]},
		"config":{"password":"private-marker"},"manifest":"private-marker","hooks":[{"manifest":"private-marker"}]
	}`, revision))
	if compressed {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		raw = buf.Bytes()
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("sh.helm.release.v1.web.v%d", revision), Namespace: "apps"},
		Type:       helmSecretType,
		Data:       map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(raw)), "other": []byte("private-marker")},
	}
}

func TestResourceHelmDecoderProjectsOnlyMetadata(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprint(compressed), func(t *testing.T) {
			row := helmRevision(*testHelmSecret(t, 2, compressed))
			if row.Name != "web" || row.Namespace != "apps" || row.Revision != 2 || row.Chart != "nginx" ||
				row.ChartVersion != "1.2.3" || row.AppVersion != "1.25" || row.Status != "deployed" || row.ParseError != "" {
				t.Fatalf("incorrect release projection: %+v", row)
			}
			raw, err := json.Marshal(aggregateHelmReleases([]HelmReleaseRevision{row}, false))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "private-marker") || strings.Contains(string(raw), "manifest") || strings.Contains(string(raw), "password") {
				t.Fatal("Helm private data escaped the projection")
			}
		})
	}
}

func TestResourceHelmCorruptionAndIdentityMismatchRemainVisible(t *testing.T) {
	valid := testHelmSecret(t, 1, true)
	bad := testHelmSecret(t, 2, true)
	bad.Data["release"] = []byte("private-marker")
	mismatch := testHelmSecret(t, 1, false)
	mismatch.Name = "sh.helm.release.v1.web.v3"
	rows := aggregateHelmReleases([]HelmReleaseRevision{helmRevision(*valid), helmRevision(*bad), helmRevision(*mismatch)}, true)
	if len(rows) != 1 || rows[0].Revision != 3 || rows[0].HistoryCount != 3 || rows[0].HistoryComplete ||
		len(rows[0].CorruptRevisions) != 2 || rows[0].ParseError == "" {
		t.Fatalf("corrupt revisions disappeared or history became complete: %+v", rows)
	}
	raw, _ := json.Marshal(rows)
	if strings.Contains(string(raw), "private-marker") {
		t.Fatal("decoder error disclosed payload")
	}
}

func TestResourceHelmRejectsMissingAndOversizedPayloads(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("not-base64"), bytes.Repeat([]byte("A"), (4<<20)+1)} {
		if _, err := decodeHelmRelease(data); err == nil {
			t.Fatal("invalid release accepted")
		}
	}
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	if _, err := z.Write(bytes.Repeat([]byte(" "), (16<<20)+1)); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeHelmRelease([]byte(base64.StdEncoding.EncodeToString(buf.Bytes()))); err == nil {
		t.Fatal("decompression size cap was ignored")
	}
}

func TestResourceHelmReadsTypedSecretsIncludingUnlabelledHistory(t *testing.T) {
	s1, s2 := testHelmSecret(t, 1, false), testHelmSecret(t, 2, true)
	cs := kubeFake.NewSimpleClientset(s1, s2, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "not-helm", Namespace: "apps"}, Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"release": s1.Data["release"]},
	})
	rows, truncated, err := readHelmRevisionsFor(context.Background(), &kubeClients{cs: cs}, "apps", "web")
	if err != nil || truncated || len(rows) != 2 || rows[0].Revision != 2 {
		t.Fatalf("history read: %+v %v %v", rows, truncated, err)
	}
	for _, action := range cs.Actions() {
		if action.GetVerb() != "list" {
			t.Fatalf("unexpected action: %s", action.GetVerb())
		}
		list := action.(k8stesting.ListAction)
		if list.GetListRestrictions().Fields.String() != "type="+helmSecretType || !list.GetListRestrictions().Labels.Empty() {
			t.Fatal("Helm read was not type-scoped or depended on optional labels")
		}
	}
}

func TestResourceHelmPaginationAndForbiddenStayDistinct(t *testing.T) {
	cs := kubeFake.NewSimpleClientset()
	calls := 0
	cs.PrependReactor("list", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		opts := action.(k8stesting.ListActionImpl).GetListOptions()
		switch calls {
		case 1:
			if opts.Continue != "" || opts.Limit != 500 {
				t.Fatal("incorrect first page options")
			}
			return true, &corev1.SecretList{ListMeta: metav1.ListMeta{Continue: "next"}, Items: []corev1.Secret{*testHelmSecret(t, 1, true)}}, nil
		case 2:
			if opts.Continue != "next" {
				t.Fatal("continue token ignored")
			}
			return true, &corev1.SecretList{Items: []corev1.Secret{*testHelmSecret(t, 2, true)}}, nil
		default:
			t.Fatal("unexpected page")
		}
		return true, nil, nil
	})
	rows, truncated, err := readHelmRevisions(context.Background(), &kubeClients{cs: cs}, "apps")
	if err != nil || truncated || len(rows) != 2 {
		t.Fatalf("pagination failed: %d %v %v", len(rows), truncated, err)
	}
	cs.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("private-marker"))
	})
	_, _, err = readHelmRevisions(context.Background(), &kubeClients{cs: cs}, "")
	var platform *httpx.Error
	if !errors.As(err, &platform) || platform.Status != http.StatusForbidden || strings.Contains(err.Error(), "private-marker") {
		t.Fatalf("forbidden read was hidden or leaked upstream content: %v", err)
	}
}

func TestResourcePaginationCapsOversizedPagesAndRejectsLoops(t *testing.T) {
	rows, truncated, err := readResourcePages(context.Background(), metav1.ListOptions{}, func(context.Context, metav1.ListOptions) ([]int, string, error) {
		return make([]int, resourceListCap+1), "", nil
	})
	if err != nil || !truncated || len(rows) != resourceListCap {
		t.Fatal("oversized server page bypassed cap")
	}
	_, _, err = readResourcePages(context.Background(), metav1.ListOptions{}, func(context.Context, metav1.ListOptions) ([]int, string, error) {
		return []int{}, "same-token", nil
	})
	if err == nil {
		t.Fatal("repeating pagination token accepted")
	}
	_, _, err = readResourcePages(context.Background(), metav1.ListOptions{}, func(context.Context, metav1.ListOptions) ([]int, string, error) {
		return nil, "", errors.New("failed")
	})
	if err == nil {
		t.Fatal("failed read returned a successful empty list")
	}
}

func TestResourceHelmRejectsPaginationLoopsAndMarksTruncatedHistory(t *testing.T) {
	cs := kubeFake.NewSimpleClientset()
	cs.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.SecretList{ListMeta: metav1.ListMeta{Continue: "same"}}, nil
	})
	if _, _, err := readHelmRevisions(context.Background(), &kubeClients{cs: cs}, "apps"); err == nil {
		t.Fatal("nonadvancing Helm pagination became a complete inventory")
	}
	cs.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		items := make([]corev1.Secret, resourceListCap+1)
		// Even if an upstream ignores Limit, untyped Secrets count toward the
		// work cap and cannot turn a partial history into a complete result.
		items[0] = *testHelmSecret(t, 1, false)
		return true, &corev1.SecretList{Items: items}, nil
	})
	revisions, truncated, err := readHelmRevisions(context.Background(), &kubeClients{cs: cs}, "apps")
	if err != nil || !truncated || len(revisions) != 1 {
		t.Fatalf("unbounded Helm response was not capped: %+v %v %v", revisions, truncated, err)
	}
	rows := aggregateHelmReleases(revisions, truncated)
	if len(rows) != 1 || rows[0].HistoryComplete {
		t.Fatal("truncated Helm history marked complete")
	}
}

func TestResourceHelmUsesSecretIdentityOverStaleLabels(t *testing.T) {
	secret := testHelmSecret(t, 2, true)
	secret.Labels = map[string]string{"name": "stale", "version": "1", "status": "failed"}
	row := helmRevision(*secret)
	if row.Name != "web" || row.Revision != 2 || row.Status != "deployed" || row.ParseError != "" {
		t.Fatalf("optional stale labels overrode the release identity: %+v", row)
	}
}
