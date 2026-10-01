package middleware

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

var (
	signatureTestKey   = []byte("test-secret-key-0123456789abcdef")
	signatureTestKeyID = "test-key"
	signatureTestNow   = time.Unix(1700000000, 0)
)

func signatureTestConfig(overrides func(*RequestSignatureConfig)) RequestSignatureConfig {
	cfg := RequestSignatureConfig{
		KeyLookup: func(keyID string) ([]byte, bool) {
			if keyID == signatureTestKeyID {
				return signatureTestKey, true
			}
			return nil, false
		},
		NonceStore: NewMemoryNonceStore(func() time.Time { return signatureTestNow }),
		Clock:      func() time.Time { return signatureTestNow },
		TimeWindow: 5 * time.Minute,
	}
	if overrides != nil {
		overrides(&cfg)
	}
	return cfg
}

// newSignatureLoopbackServer starts a loopback HTTP server with a chi router
// protected by the RequestSignature middleware. The handler echoes the full
// request body and counts invocations.
func newSignatureLoopbackServer(t *testing.T, cfg RequestSignatureConfig) (srv *httptest.Server, handlerCount *int64) {
	t.Helper()
	count := new(int64)
	r := chi.NewRouter()
	r.Use(RequestSignature(cfg))
	r.Post("/submit", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(count, 1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write(body)
	})
	r.Get("/items/{name}", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(count, 1)
		w.Write([]byte(chi.URLParam(r, "name")))
	})
	srv = httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, count
}

func newSignedRequest(t *testing.T, method, rawURL string, body []byte, nonce string, ts time.Time) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	SignRequest(signatureTestKey, signatureTestKeyID, nonce, ts, req, body)
	return req
}

func doRequest(t *testing.T, req *http.Request) (int, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, respBody
}

func TestRequestSignatureLoopback(t *testing.T) {
	srv, count := newSignatureLoopbackServer(t, signatureTestConfig(nil))

	body := []byte("{\"hello\":\"world\"}")
	// Duplicate query parameter "a" plus an empty value.
	req := newSignedRequest(t, http.MethodPost, srv.URL+"/submit?b=2&a=1&a=", body, "nonce-1", signatureTestNow)
	status, respBody := doRequest(t, req)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", status, respBody)
	}
	if !bytes.Equal(respBody, body) {
		t.Fatalf("handler did not receive the full body: got %q want %q", respBody, body)
	}
	if got := atomic.LoadInt64(count); got != 1 {
		t.Fatalf("handler count = %d, want 1", got)
	}
}

func TestRequestSignatureTamperedBody(t *testing.T) {
	srv, count := newSignatureLoopbackServer(t, signatureTestConfig(nil))

	body := []byte("{\"hello\":\"world\"}")
	tampered := append([]byte(nil), body...)
	tampered[3] ^= 0x01

	// Sign the original body, then send a body differing by one byte.
	req := newSignedRequest(t, http.MethodPost, srv.URL+"/submit?b=2&a=1&a=", body, "nonce-tamper", signatureTestNow)
	req.Body = io.NopCloser(bytes.NewReader(tampered))
	req.ContentLength = int64(len(tampered))
	status, _ := doRequest(t, req)
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", status)
	}
	if got := atomic.LoadInt64(count); got != 0 {
		t.Fatalf("handler count = %d, want 0", got)
	}

	// A failed verification releases the nonce, so the correctly signed
	// request with the same nonce is accepted afterwards.
	req = newSignedRequest(t, http.MethodPost, srv.URL+"/submit?b=2&a=1&a=", body, "nonce-tamper", signatureTestNow)
	status, _ = doRequest(t, req)
	if status != http.StatusOK {
		t.Fatalf("expected 200 after release, got %d", status)
	}
	if got := atomic.LoadInt64(count); got != 1 {
		t.Fatalf("handler count = %d, want 1", got)
	}
}

func TestRequestSignatureReplay(t *testing.T) {
	srv, count := newSignatureLoopbackServer(t, signatureTestConfig(nil))

	body := []byte("replay-me")
	req := newSignedRequest(t, http.MethodPost, srv.URL+"/submit", body, "nonce-replay", signatureTestNow)
	status, _ := doRequest(t, req)
	if status != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", status)
	}

	// Replaying the exact same signed request must be rejected.
	req = newSignedRequest(t, http.MethodPost, srv.URL+"/submit", body, "nonce-replay", signatureTestNow)
	status, _ = doRequest(t, req)
	if status != http.StatusUnauthorized {
		t.Fatalf("replayed request: expected 401, got %d", status)
	}
	if got := atomic.LoadInt64(count); got != 1 {
		t.Fatalf("handler count = %d, want 1", got)
	}
}

func TestRequestSignatureExpired(t *testing.T) {
	srv, count := newSignatureLoopbackServer(t, signatureTestConfig(nil))

	body := []byte("too-late")
	for i, ts := range []time.Time{
		signatureTestNow.Add(-10 * time.Minute), // too old
		signatureTestNow.Add(10 * time.Minute),  // too far in the future
	} {
		req := newSignedRequest(t, http.MethodPost, srv.URL+"/submit", body, fmt.Sprintf("nonce-expired-%d", i), ts)
		status, _ := doRequest(t, req)
		if status != http.StatusUnauthorized {
			t.Fatalf("timestamp %v: expected 401, got %d", ts, status)
		}
	}
	if got := atomic.LoadInt64(count); got != 0 {
		t.Fatalf("handler count = %d, want 0", got)
	}
}

func TestRequestSignatureConcurrentDuplicateNonce(t *testing.T) {
	srv, count := newSignatureLoopbackServer(t, signatureTestConfig(nil))

	body := []byte("concurrent")
	const goroutines = 16
	statuses := make([]int, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := newSignedRequest(t, http.MethodPost, srv.URL+"/submit", body, "nonce-race", signatureTestNow)
			status, _ := doRequest(t, req)
			statuses[i] = status
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, s := range statuses {
		if s == http.StatusOK {
			ok++
		} else if s != http.StatusUnauthorized {
			t.Fatalf("unexpected status %d", s)
		}
	}
	if ok != 1 {
		t.Fatalf("expected exactly one 200, got %d", ok)
	}
	if got := atomic.LoadInt64(count); got != 1 {
		t.Fatalf("handler count = %d, want 1", got)
	}
}

func TestRequestSignatureTempFileSpoolAndCleanup(t *testing.T) {
	tempDir := t.TempDir()
	cfg := signatureTestConfig(func(c *RequestSignatureConfig) {
		c.MemoryThreshold = 64 // force spooling to a temp file
		c.TempDir = tempDir
	})
	srv, count := newSignatureLoopbackServer(t, cfg)

	body := make([]byte, 256*1024)
	for i := range body {
		body[i] = byte(i)
	}
	req := newSignedRequest(t, http.MethodPost, srv.URL+"/submit", body, "nonce-spool", signatureTestNow)
	status, respBody := doRequest(t, req)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if !bytes.Equal(respBody, body) {
		t.Fatalf("handler did not receive the full spooled body (%d bytes)", len(respBody))
	}
	if got := atomic.LoadInt64(count); got != 1 {
		t.Fatalf("handler count = %d, want 1", got)
	}

	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp dir not cleaned up, %d entries remain", len(entries))
	}
}

func TestRequestSignatureEscapedPath(t *testing.T) {
	srv, count := newSignatureLoopbackServer(t, signatureTestConfig(nil))

	// The raw escaped path /items/a%2Fb is signed, not the decoded /items/a/b.
	rawURL := srv.URL + "/items/a%2Fb"
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	if u.RawPath != "/items/a%2Fb" {
		t.Fatalf("expected RawPath to be preserved, got %q", u.RawPath)
	}

	req := newSignedRequest(t, http.MethodGet, rawURL, nil, "nonce-escaped", signatureTestNow)
	status, respBody := doRequest(t, req)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", status, respBody)
	}

	// A signature computed over the decoded path must not validate.
	req, err = http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	bodyHash := sha256.Sum256(nil)
	ts := fmt.Sprintf("%d", signatureTestNow.Unix())
	canonicalQuery, _ := CanonicalQuery(req.URL.RawQuery)
	signature := SignString(signatureTestKey, StringToSign(http.MethodGet, "/items/a/b", canonicalQuery, ts, "nonce-escaped-2", bodyHash))
	req.Header.Set(SignatureKeyIDHeader, signatureTestKeyID)
	req.Header.Set(SignatureTimestampHeader, ts)
	req.Header.Set(SignatureNonceHeader, "nonce-escaped-2")
	req.Header.Set(SignatureHeader, signature)
	status, _ = doRequest(t, req)
	if status != http.StatusUnauthorized {
		t.Fatalf("decoded-path signature: expected 401, got %d", status)
	}
	if got := atomic.LoadInt64(count); got != 1 {
		t.Fatalf("handler count = %d, want 1", got)
	}
}

func TestRequestSignatureBodyTooLarge(t *testing.T) {
	cfg := signatureTestConfig(func(c *RequestSignatureConfig) {
		c.MaxBodySize = 100
	})
	srv, count := newSignatureLoopbackServer(t, cfg)

	body := make([]byte, 200)
	req := newSignedRequest(t, http.MethodPost, srv.URL+"/submit", body, "nonce-large", signatureTestNow)
	status, _ := doRequest(t, req)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", status)
	}
	if got := atomic.LoadInt64(count); got != 0 {
		t.Fatalf("handler count = %d, want 0", got)
	}
}

func TestRequestSignatureErrorResponseNoLeak(t *testing.T) {
	srv, _ := newSignatureLoopbackServer(t, signatureTestConfig(nil))

	body := []byte("leak-check")
	req := newSignedRequest(t, http.MethodPost, srv.URL+"/submit", body, "nonce-leak", signatureTestNow)
	req.Header.Set(SignatureHeader, strings.Repeat("0", 64)) // invalid signature
	status, respBody := doRequest(t, req)
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", status)
	}
	if bytes.Contains(respBody, signatureTestKey) {
		t.Fatalf("error response leaks key material: %q", respBody)
	}
	if bytes.Contains(respBody, []byte(req.Header.Get(SignatureHeader))) {
		t.Fatalf("error response echoes signature material: %q", respBody)
	}
}

func TestCanonicalQuery(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"", ""},
		{"b=2&a=1", "a=1&b=2"},
		{"b=2&a=1&a=", "a=&a=1&b=2"},         // duplicates and empty values kept
		{"a=%7E&a=+x", "a=%20x&a=~"},         // decoded sort, fixed re-encode
		{"z=1&a=%2F&a=/", "a=%2F&a=%2F&z=1"}, // equivalent encodings canonicalize
		{"A=1&a=2", "A=1&a=2"},               // byte-wise key ordering
		{"b&a=1", "a=1&b="},                  // valueless key signs as empty value
		{"k=%E4%B8%AD", "k=%E4%B8%AD"},       // multi-byte UTF-8
		{"k=a*b", "k=a%2Ab"},                 // '*' is not unreserved
	}
	for _, tt := range tests {
		got, err := CanonicalQuery(tt.raw)
		if err != nil {
			t.Fatalf("CanonicalQuery(%q): %v", tt.raw, err)
		}
		if got != tt.want {
			t.Errorf("CanonicalQuery(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}
