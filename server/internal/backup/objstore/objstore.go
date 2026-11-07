// Package objstore is a tiny blob store abstraction used by platform backups
// and audit archives: an S3-compatible bucket (AWS S3, MinIO, Ceph RGW, ...)
// signed with AWS Signature V4 using only the standard library, or a local
// directory when no bucket is configured.
package objstore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Store persists named blobs.
type Store interface {
	// Kind is "s3" or "local".
	Kind() string
	// Describe is a display string without credentials ("s3://bucket/prefix", "/var/lib/...").
	Describe() string
	// Put writes size bytes from r under key and returns the object location.
	Put(ctx context.Context, key string, r io.Reader, size int64) (string, error)
	// Get opens the object; the caller closes the reader.
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
	// Delete removes the object; deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error
}

// ErrNotFound is returned by Get for a missing object.
var ErrNotFound = errors.New("objstore: object not found")

// ValidKey reports whether key is a safe relative object name.
func ValidKey(key string) bool {
	if key == "" || len(key) > 512 || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return false
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- local

// Local stores blobs as files under Dir.
type Local struct{ Dir string }

func (l *Local) Kind() string     { return "local" }
func (l *Local) Describe() string { return l.Dir }

func (l *Local) path(key string) (string, error) {
	if !ValidKey(key) {
		return "", fmt.Errorf("objstore: invalid key %q", key)
	}
	return filepath.Join(l.Dir, filepath.FromSlash(key)), nil
}

// Put writes to a temp file and renames it into place (atomic on one filesystem).
func (l *Local) Put(ctx context.Context, key string, r io.Reader, _ int64) (string, error) {
	p, err := l.path(key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, ctxReader{ctx, r}); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return "", err
	}
	return p, nil
}

func (l *Local) Get(_ context.Context, key string) (io.ReadCloser, int64, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// ---------------------------------------------------------------- S3

// S3 talks to an S3-compatible endpoint with Signature V4.
type S3 struct {
	Endpoint  string // http(s)://host[:port]
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	Prefix    string
	PathStyle bool
	HTTP      *http.Client
	now       func() time.Time
}

func (s *S3) Kind() string { return "s3" }
func (s *S3) Describe() string {
	return "s3://" + s.Bucket + "/" + strings.TrimPrefix(s.Prefix, "/")
}

func (s *S3) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 30 * time.Minute}
}

func (s *S3) objectURL(key string) (*url.URL, error) {
	if !ValidKey(key) {
		return nil, fmt.Errorf("objstore: invalid key %q", key)
	}
	u, err := url.Parse(strings.TrimRight(s.Endpoint, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("objstore: bad endpoint")
	}
	full := strings.TrimPrefix(path.Join(strings.Trim(s.Prefix, "/"), key), "/")
	if s.PathStyle {
		u.Path = "/" + s.Bucket + "/" + full
	} else {
		u.Host = s.Bucket + "." + u.Host
		u.Path = "/" + full
	}
	u.RawPath = uriEncode(u.Path, false)
	return u, nil
}

func (s *S3) do(ctx context.Context, method, key string, body io.Reader, size int64) (*http.Response, error) {
	u, err := s.objectURL(key)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	signV4(req, s.AccessKey, s.SecretKey, s.Region, "UNSIGNED-PAYLOAD", now().UTC())
	return s.client().Do(req)
}

func s3Error(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	msg := string(b)
	if i := strings.Index(msg, "<Code>"); i >= 0 {
		if j := strings.Index(msg[i:], "</Code>"); j > 0 {
			msg = msg[i+6 : i+j]
		}
	}
	return fmt.Errorf("objstore: s3 %s: %s", resp.Status, strings.TrimSpace(msg))
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) (string, error) {
	if size < 0 {
		return "", errors.New("objstore: s3 put needs a known size")
	}
	resp, err := s.do(ctx, http.MethodPut, key, r, size)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", s3Error(resp)
	}
	return "s3://" + s.Bucket + "/" + strings.TrimPrefix(path.Join(strings.Trim(s.Prefix, "/"), key), "/"), nil
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	resp, err := s.do(ctx, http.MethodGet, key, nil, 0)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, 0, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, 0, s3Error(resp)
	}
	return resp.Body, resp.ContentLength, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, key, nil, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return s3Error(resp)
	}
	return nil
}

// ---------------------------------------------------------------- SigV4

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// uriEncode implements the AWS UriEncode rules.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// signV4 adds x-amz-date, x-amz-content-sha256 and Authorization headers.
// Every header already on the request (plus host) is signed.
func signV4(req *http.Request, accessKey, secretKey, region, payloadHash string, t time.Time) {
	amzDate := t.Format("20060102T150405Z")
	date := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "user-agent" {
			continue
		}
		headers[lk] = strings.Join(strings.Fields(strings.Join(v, ",")), " ")
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")

	q := req.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var qs []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			qs = append(qs, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	canonPath := req.URL.EscapedPath()
	if canonPath == "" {
		canonPath = "/"
	}
	canonical := strings.Join([]string{req.Method, canonPath, strings.Join(qs, "&"), canonHeaders.String(), signed, payloadHash}, "\n")
	scope := date + "/" + region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex(canonical)
	k := hmacSHA256([]byte("AWS4"+secretKey), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}
