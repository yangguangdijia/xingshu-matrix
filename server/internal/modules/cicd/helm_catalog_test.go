package cicd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNormalizeHelmRepoURL(t *testing.T) {
	valid := []struct {
		input string
		want  string
	}{
		{"https://charts.example.com/", "https://charts.example.com"},
		{"HTTPS://charts.example.com/team/", "https://charts.example.com/team"},
		{"https://10.0.0.12:8443/charts", "https://10.0.0.12:8443/charts"},
		{"https://[fd00::12]:8443/charts/", "https://[fd00::12]:8443/charts"},
	}
	for _, tc := range valid {
		got, err := normalizeHelmRepoURL(tc.input)
		if err != nil || got != tc.want {
			t.Errorf("normalizeHelmRepoURL(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
	invalid := []string{
		"", "http://charts.example.com", "oci://registry.example.com/charts",
		"https://user:pass@charts.example.com", "https://charts.example.com/?token=x",
		"https://charts.example.com/#chart", "https://charts.example.com/a/../b",
		"https://localhost/charts", "https://api.localhost/charts",
		"https://127.0.0.1/charts", "https://[::1]/charts",
		"https://169.254.169.254/latest", "https://100.100.100.200/latest",
	}
	for _, input := range invalid {
		if got, err := normalizeHelmRepoURL(input); err == nil {
			t.Errorf("normalizeHelmRepoURL(%q) accepted as %q", input, got)
		}
	}
}

func TestHelmIPAllowed(t *testing.T) {
	for _, raw := range []string{
		"10.0.0.1", "172.16.0.1", "192.168.1.1", "fd00::1", "8.8.8.8",
	} {
		ip := netip.MustParseAddr(raw)
		if !helmIPAllowed(ip) {
			t.Errorf("expected %s to be allowed", ip)
		}
	}
	for _, raw := range []string{
		"127.0.0.1", "::1", "0.0.0.0", "169.254.1.1", "169.254.169.254",
		"100.100.100.200", "224.0.0.1", "ff02::1", "fd00:ec2::254",
	} {
		ip := netip.MustParseAddr(raw)
		if helmIPAllowed(ip) {
			t.Errorf("expected %s to be rejected", ip)
		}
	}
}

func TestHelmRepoRedirectValidation(t *testing.T) {
	client := newHelmRepoHTTPClient(nil)
	check := func(target string, wantErr bool) {
		t.Helper()
		u, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}
		req := &http.Request{URL: u}
		via := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "charts.example.com"}}}
		err = client.CheckRedirect(req, via)
		if (err != nil) != wantErr {
			t.Errorf("redirect to %q error = %v, wantErr=%t", target, err, wantErr)
		}
	}
	check("https://mirror.internal/charts", false)
	check("https://10.0.0.10/charts", false)
	check("http://charts.example.com/index.yaml", true)
	check("https://127.0.0.1/index.yaml", true)
	check("https://169.254.169.254/latest", true)

	u, _ := url.Parse("https://charts.example.com")
	req := &http.Request{URL: u}
	via := []*http.Request{
		{URL: &url.URL{Scheme: "https", Host: "charts.example.com"}},
		{URL: &url.URL{Scheme: "https", Host: "mirror1.example.com"}},
		{URL: &url.URL{Scheme: "https", Host: "mirror2.example.com"}},
		{URL: &url.URL{Scheme: "https", Host: "mirror3.example.com"}},
	}
	if err := client.CheckRedirect(req, via); err == nil {
		t.Fatal("redirect chain beyond three hops was accepted")
	}
}

func TestDecodeHelmIndex(t *testing.T) {
	now := time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC)
	raw := `apiVersion: v1
entries:
  redis:
    - name: ignored-name
      version: 2.1.0
      appVersion: "7.4"
      description: In-memory database
    - version: 2.0.0
  bad/name:
    - version: 1.0.0
`
	charts, err := decodeHelmIndex([]byte(raw), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(charts) != 2 {
		t.Fatalf("got %d charts, want 2", len(charts))
	}
	if charts[0].Name != "redis" || charts[0].Version != "2.1.0" ||
		charts[0].AppVersion != "7.4" || charts[0].Description != "In-memory database" ||
		!charts[0].UpdatedAt.Equal(now) {
		t.Fatalf("unexpected parsed chart: %+v", charts[0])
	}
	if charts[1].Name != "redis" || charts[1].Version != "2.0.0" {
		t.Fatalf("versions are not sorted: %+v", charts)
	}
	if _, err := decodeHelmIndex([]byte("entries: ["), now); err == nil {
		t.Fatal("invalid YAML index was accepted")
	}
}

func TestFetchHelmIndexUsesConfiguredBasePath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/team/charts/index.yaml" {
			t.Errorf("request path = %q, want /team/charts/index.yaml", r.URL.Path)
		}
		_, _ = w.Write([]byte("entries:\n  demo:\n    - version: 1.0.0\n"))
	}))
	defer server.Close()
	charts, err := fetchHelmIndex(context.Background(), server.Client(), server.URL+"/team/charts")
	if err != nil {
		t.Fatal(err)
	}
	if len(charts) != 1 || charts[0].Name != "demo" || charts[0].Version != "1.0.0" {
		t.Fatalf("unexpected index result: %+v", charts)
	}
}

func TestSafeHelmSyncErrorBounded(t *testing.T) {
	got := safeHelmSyncError(errors.New(strings.Repeat("x", 300)))
	if len(got) != 256 {
		t.Fatalf("error length = %d, want 256", len(got))
	}
}
