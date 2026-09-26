package middleware

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

var hmacTestSecret = []byte("test-secret-key-0123456789abcdef")

const hmacTestKeyID = "test-key"

// hmacTestClock is a fixed, injectable clock shared by the tests.
var hmacTestClock = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

func hmacTestConfig(mutate ...func(*HMACAuthConfig)) HMACAuthConfig {
	cfg := HMACAuthConfig{
		KeyStore: HMACKeyFunc(func(keyID string) ([]byte, bool) {
			if keyID == hmacTestKeyID {
				return hmacTestSecret, true
			}
			return nil, false
		}),
		NonceStore: NewMemoryNonceStore(),
		Clock:      func() time.Time { return hmacTestClock },
		TimeWindow: 5 * time.Minute,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	return cfg
}

// hmacTestRouter mounts the middleware on a chi router and counts how many
// times the downstream handler runs. The handler echoes the request body so
// tests can verify the body is still fully readable downstream.
func hmacTestRouter(cfg HMACAuthConfig, hits *atomic.Int32) http.Handler {
	r := chi.NewRouter()
	r.Use(HMACAuth(cfg))
	echo := func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}
	r.Post("/upload", echo)
	r.Post("/files/*", echo)
	return r
}

func hmacSignedRequest(t *testing.T, method, target string, body []byte, nonce string, ts time.Time) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if err := SignHMACRequest(req, hmacTestSecret, hmacTestKeyID, nonce, ts, body); err != nil {
		t.Fatalf("sign request: %v", err)
	}
	return req
}

func hmacNonce() string {
	return fmt.Sprintf("nonce-%d", time.Now().UnixNano())
}

// TestHMACAuthLoopback exercises the full flow over a real loopback HTTP
// connection: a request with repeated query parameters signed with the
// fixed test key succeeds, and the handler can read the complete body.
func TestHMACAuthLoopback(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(hmacTestRouter(hmacTestConfig(), &hits))
	defer srv.Close()

	body := []byte("{\"hello\":\"world\"}")
	target := srv.URL + "/upload?b=2&a=1&a=2&empty=&b=1"
	req := hmacSignedRequest(t, http.MethodPost, target, body, hmacNonce(), hmacTestClock)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	echoed, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(echoed, body) {
		t.Fatalf("handler did not receive the full body: got %q want %q", echoed, body)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected handler to run once, ran %d times", hits.Load())
	}
}

// TestHMACAuthTamperedBody signs a request, then flips one byte of the
// body: verification must fail with 401 and the handler must not run. The
// nonce must not be consumed by the failed attempt, and a later replay of
// the successful request with the same nonce must be rejected.
func TestHMACAuthTamperedBody(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(hmacTestRouter(hmacTestConfig(), &hits))
	defer srv.Close()

	body := []byte("original-body")
	nonce := hmacNonce()
	target := srv.URL + "/upload"

	// Sign for the original body but send a body with one byte flipped.
	tampered := append([]byte(nil), body...)
	tampered[0] ^= 0x01
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(tampered))
	if err != nil {
		t.Fatal(err)
	}
	if err := SignHMACRequest(req, hmacTestSecret, hmacTestKeyID, nonce, hmacTestClock, body); err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for tampered body, got %d", resp.StatusCode)
	}
	if hits.Load() != 0 {
		t.Fatalf("handler must not run on tampered body, ran %d times", hits.Load())
	}

	// The failed attempt must not have consumed the nonce: the correctly
	// signed request with the same nonce succeeds.
	good := hmacSignedRequest(t, http.MethodPost, target, body, nonce, hmacTestClock)
	resp, err = http.DefaultClient.Do(good)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after failed attempt, got %d (nonce must only be consumed on success)", resp.StatusCode)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected handler to run once, ran %d times", hits.Load())
	}

	// Replaying the very same (valid) request with the same nonce is a
	// replay attack and must be rejected.
	replay := hmacSignedRequest(t, http.MethodPost, target, body, nonce, hmacTestClock)
	resp, err = http.DefaultClient.Do(replay)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for replayed nonce, got %d", resp.StatusCode)
	}
	if hits.Load() != 1 {
		t.Fatalf("handler count must stay 1 after replay, got %d", hits.Load())
	}
}

// TestHMACAuthExpiredTimestamp rejects timestamps outside the time window
// using the injected clock.
func TestHMACAuthExpiredTimestamp(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(hmacTestRouter(hmacTestConfig(), &hits))
	defer srv.Close()

	body := []byte("payload")
	for _, ts := range []time.Time{
		hmacTestClock.Add(-10 * time.Minute), // too old
		hmacTestClock.Add(10 * time.Minute),  // too far in the future
	} {
		req := hmacSignedRequest(t, http.MethodPost, srv.URL+"/upload", body, hmacNonce(), ts)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 for timestamp %v, got %d", ts, resp.StatusCode)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("handler must not run for expired timestamps, ran %d times", hits.Load())
	}
}

// TestHMACAuthConcurrentDuplicateNonce fires many concurrent requests with
// the same nonce; exactly one may win the atomic nonce registration.
func TestHMACAuthConcurrentDuplicateNonce(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(hmacTestRouter(hmacTestConfig(), &hits))
	defer srv.Close()

	const workers = 16
	body := []byte("concurrent-body")
	nonce := hmacNonce()
	target := srv.URL + "/upload"

	var wg sync.WaitGroup
	var okCount, unauthorizedCount atomic.Int32
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := hmacSignedRequest(t, http.MethodPost, target, body, nonce, hmacTestClock)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("do request: %v", err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK:
				okCount.Add(1)
			case http.StatusUnauthorized:
				unauthorizedCount.Add(1)
			default:
				t.Errorf("unexpected status %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()

	if okCount.Load() != 1 {
		t.Fatalf("expected exactly 1 success among concurrent duplicate nonces, got %d", okCount.Load())
	}
	if unauthorizedCount.Load() != workers-1 {
		t.Fatalf("expected %d rejections, got %d", workers-1, unauthorizedCount.Load())
	}
	if hits.Load() != 1 {
		t.Fatalf("expected handler to run exactly once, ran %d times", hits.Load())
	}
}

// TestHMACAuthTempFileSpoolAndCleanup forces large bodies to spool to a
// temporary file and verifies the file is removed after the request while
// the handler still reads the complete body.
func TestHMACAuthTempFileSpoolAndCleanup(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := hmacTestConfig(func(c *HMACAuthConfig) {
		c.MemoryBodyBytes = 16 // everything larger spools to disk
		c.TempDir = tmpDir
	})
	var hits atomic.Int32
	srv := httptest.NewServer(hmacTestRouter(cfg, &hits))
	defer srv.Close()

	body := bytes.Repeat([]byte("0123456789abcdef"), 8192) // 128 KiB
	req := hmacSignedRequest(t, http.MethodPost, srv.URL+"/upload", body, hmacNonce(), hmacTestClock)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	echoed, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if !bytes.Equal(echoed, body) {
		t.Fatalf("spooled body corrupted: got %d bytes, want %d", len(echoed), len(body))
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temporary body files not cleaned up: %v", names)
	}
}

// TestHMACAuthBodyTooLarge rejects bodies above MaxBodyBytes with 413
// before any signature verification happens.
func TestHMACAuthBodyTooLarge(t *testing.T) {
	cfg := hmacTestConfig(func(c *HMACAuthConfig) {
		c.MaxBodyBytes = 100
	})
	var hits atomic.Int32
	srv := httptest.NewServer(hmacTestRouter(cfg, &hits))
	defer srv.Close()

	body := bytes.Repeat([]byte("x"), 200)
	req := hmacSignedRequest(t, http.MethodPost, srv.URL+"/upload", body, hmacNonce(), hmacTestClock)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", resp.StatusCode)
	}
	if hits.Load() != 0 {
		t.Fatalf("handler must not run for oversized body, ran %d times", hits.Load())
	}
}

// TestHMACAuthRawEscapedPath signs the raw escaped path: a signature made
// for /files/a%2Fb is valid only for that exact escaped path, not for the
// decoded equivalent /files/a/b.
func TestHMACAuthRawEscapedPath(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(hmacTestRouter(hmacTestConfig(), &hits))
	defer srv.Close()

	body := []byte("path-body")

	// Signed for the escaped path, sent to the escaped path: valid.
	req := hmacSignedRequest(t, http.MethodPost, srv.URL+"/files/a%2Fb", body, hmacNonce(), hmacTestClock)
	if got := req.URL.EscapedPath(); got != "/files/a%2Fb" {
		t.Fatalf("expected escaped path to be preserved, got %q", got)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for matching escaped path, got %d", resp.StatusCode)
	}

	// Same signature material but the decoded path: the signing string
	// differs, so verification must fail.
	req2, err := http.NewRequest(http.MethodPost, srv.URL+"/files/a/b", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	// Sign as if the path were the escaped one.
	escapedReq := hmacSignedRequest(t, http.MethodPost, srv.URL+"/files/a%2Fb", body, hmacNonce(), hmacTestClock)
	req2.Header = escapedReq.Header.Clone()
	resp, err = http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for decoded path with escaped-path signature, got %d", resp.StatusCode)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected handler to run once, ran %d times", hits.Load())
	}
}

// TestCanonicalHMACQuery pins the canonicalization rules: decode, sort by
// decoded key then value, re-encode with the fixed percent-encoding, keep
// duplicates, keep empty values.
func TestCanonicalHMACQuery(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", ""},
		{"b=2&a=1&a=2&empty=&b=1", "a=1&a=2&b=1&b=2&empty="},
		{"a=1&a=1", "a=1&a=1"},         // duplicates kept
		{"flag", "flag="},              // valueless key participates
		{"a=&b=2", "a=&b=2"},           // empty value participates
		{"a=%2F&a=b", "a=%2F&a=b"},     // sorted on decoded values
		{"k=a+b", "k=a%20b"},           // "+" decodes to space, re-encoded as %20
		{"k=%7E", "k=~"},               // unreserved char normalized
		{"k=%e4%b8%ad", "k=%E4%B8%AD"}, // uppercase hex, per-byte encoding
		{"B=1&a=2&A=3", "A=3&B=1&a=2"}, // byte-wise key ordering
		{"a=2&a=10", "a=10&a=2"},       // byte-wise value ordering
	}
	for _, tc := range cases {
		got, err := CanonicalHMACQuery(tc.raw)
		if err != nil {
			t.Fatalf("CanonicalHMACQuery(%q): %v", tc.raw, err)
		}
		if got != tc.want {
			t.Errorf("CanonicalHMACQuery(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
	if _, err := CanonicalHMACQuery("a=%zz"); err == nil {
		t.Errorf("expected error for invalid percent-encoding")
	}
}

// TestHMACAuthErrorResponseNoLeak ensures error responses never echo the
// secret key or any signature material.
func TestHMACAuthErrorResponseNoLeak(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(hmacTestRouter(hmacTestConfig(), &hits))
	defer srv.Close()

	body := []byte("leak-check")
	req := hmacSignedRequest(t, http.MethodPost, srv.URL+"/upload", body, hmacNonce(), hmacTestClock)
	req.Header.Set(HMACSignatureHeader, strings.Repeat("00", 32)) // wrong signature
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	leaks := [][]byte{hmacTestSecret, []byte(hmacTestKeyID), []byte(HMACSignatureHeader)}
	for _, leak := range leaks {
		if bytes.Contains(respBody, leak) {
			t.Fatalf("error response leaks %q: %q", leak, respBody)
		}
	}
}
