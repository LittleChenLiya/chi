package middleware

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
)

// countingReadCloser wraps a body and counts how many times Close is called.
type countingReadCloser struct {
	io.Reader
	mu     sync.Mutex
	closes int
}

func (c *countingReadCloser) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes++
	return nil
}

func (c *countingReadCloser) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

// oneByteReader returns one byte per Read and records how many reads happened
// and the buffer size each Read was given. It proves the middleware never
// reads ahead: the original reader sees exactly the handler's Read calls.
type oneByteReader struct {
	data     []byte
	pos      int
	reads    int
	bufSizes []int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	r.reads++
	r.bufSizes = append(r.bufSizes, len(p))
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	n := copy(p, r.data[r.pos:r.pos+1])
	r.pos++
	return n, nil
}

// readHandler reads the whole body. On a read error it returns without writing
// anything so the middleware can produce the error response.
func readHandler(w http.ResponseWriter, r *http.Request) {
	if _, err := io.ReadAll(r.Body); err != nil {
		return
	}
	w.Write([]byte("ok"))
}

// writeThenReadHandler writes a response first, then reads the body. The read
// may fail, but the response has already started so no error page must be
// appended.
func writeThenReadHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("ok"))
	io.ReadAll(r.Body)
}

func newBudgetRouter(outer, inner int64) *chi.Mux {
	r := chi.NewRouter()
	if outer >= 0 {
		r.Use(BodyBudget(outer))
	}
	r.Group(func(r chi.Router) {
		if inner >= 0 {
			r.Use(BodyBudget(inner))
		}
		r.Post("/", readHandler)
		r.Post("/write-then-read", writeThenReadHandler)
	})
	return r
}

// newUnlimitedRouter builds a router with no BodyBudget layer at all.
func newUnlimitedRouter() *chi.Mux {
	r := chi.NewRouter()
	r.Post("/unlimited", readHandler)
	return r
}

func doRequest(t *testing.T, ts *httptest.Server, path, body string, chunked bool) (int, string) {
	t.Helper()
	var reader io.Reader = strings.NewReader(body)
	if chunked {
		// io.NopCloser hides the concrete type from http.NewRequest, forcing
		// ContentLength = -1 and chunked transfer encoding.
		reader = io.NopCloser(strings.NewReader(body))
	}
	req, err := http.NewRequest("POST", ts.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// TestBodyBudget_MinSemantics verifies that nested layers take the smallest
// budget: outer 8 / inner 3 admits 3 bytes but rejects 4, and the reverse
// ordering (outer 3 / inner 8) rejects 4 at the outer layer too.
func TestBodyBudget_MinSemantics(t *testing.T) {
	for _, tc := range []struct {
		name         string
		outer, inner int64
	}{
		{"outer8_inner3", 8, 3},
		{"outer3_inner8", 3, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(newBudgetRouter(tc.outer, tc.inner))
			defer ts.Close()

			status, _ := doRequest(t, ts, "/", "abc", false)
			if status != http.StatusOK {
				t.Fatalf("3 bytes: got status %d, want 200", status)
			}

			status, body := doRequest(t, ts, "/", "abcd", false)
			if status != http.StatusRequestEntityTooLarge {
				t.Fatalf("4 bytes: got status %d, want 413", status)
			}
			_ = body // the 413 may carry a short standard error text
		})
	}
}

// TestBodyBudget_OuterOnly verifies a single outer layer enforces its limit
// and that a route with no layer is unaffected.
func TestBodyBudget_OuterOnly(t *testing.T) {
	ts := httptest.NewServer(newBudgetRouter(8, -1))
	defer ts.Close()

	uts := httptest.NewServer(newUnlimitedRouter())
	defer uts.Close()

	status, _ := doRequest(t, uts, "/unlimited", strings.Repeat("x", 100), false)
	if status != http.StatusOK {
		t.Fatalf("unlimited route: got status %d, want 200", status)
	}

	// inner=-1 skips the inner layer, so "/" is guarded only by the outer 8.
	status, _ = doRequest(t, ts, "/", strings.Repeat("x", 8), false)
	if status != http.StatusOK {
		t.Fatalf("8 bytes under outer 8: got status %d, want 200", status)
	}
	status, _ = doRequest(t, ts, "/", strings.Repeat("x", 9), false)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("9 bytes over outer 8: got status %d, want 413", status)
	}
}

// TestBodyBudget_ChunkedCannotBypass sends a body with unknown Content-Length
// (chunked transfer encoding). The budget counts actual bytes read, so the
// limit still applies.
func TestBodyBudget_ChunkedCannotBypass(t *testing.T) {
	ts := httptest.NewServer(newBudgetRouter(8, 3))
	defer ts.Close()

	status, _ := doRequest(t, ts, "/", "abc", true)
	if status != http.StatusOK {
		t.Fatalf("chunked 3 bytes: got status %d, want 200", status)
	}
	status, body := doRequest(t, ts, "/", "abcd", true)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked 4 bytes: got status %d, want 413", status)
	}
	_ = body // the 413 may carry a short standard error text
}

// TestBodyBudget_ResponseStartedNotAppended verifies that when the handler
// writes a response before reading past the budget, no error page is appended
// after the response has started.
func TestBodyBudget_ResponseStartedNotAppended(t *testing.T) {
	ts := httptest.NewServer(newBudgetRouter(8, 3))
	defer ts.Close()

	status, body := doRequest(t, ts, "/write-then-read", "abcd", false)
	if status != http.StatusOK {
		t.Fatalf("got status %d, want 200", status)
	}
	if body != "ok" {
		t.Fatalf("got body %q, want %q (no error page appended)", body, "ok")
	}
}

// TestBodyBudget_ClosesOriginalOnce verifies the original body is closed
// exactly once even though multiple layers wrap it.
func TestBodyBudget_ClosesOriginalOnce(t *testing.T) {
	orig := &countingReadCloser{Reader: strings.NewReader("hello")}
	req := httptest.NewRequest("POST", "/", orig)
	w := httptest.NewRecorder()

	var wrapped io.Closer
	r := chi.NewRouter()
	r.Use(BodyBudget(8))
	r.Use(BodyBudget(3))
	r.Post("/", func(w http.ResponseWriter, r *http.Request) {
		wrapped = r.Body // capture the outermost wrapped body
		io.ReadAll(r.Body)
	})
	r.ServeHTTP(w, req)

	if wrapped == nil {
		t.Fatal("handler did not capture wrapped body")
	}
	// Simulate the server closing the request body after the handler.
	wrapped.Close()
	wrapped.Close() // second close must not close the original again

	if got := orig.closeCount(); got != 1 {
		t.Fatalf("original body closed %d times, want 1", got)
	}
}

// TestBodyBudget_ContextError verifies the budget error is queryable from the
// request context after a read exceeds the limit, and absent otherwise.
func TestBodyBudget_ContextError(t *testing.T) {
	var ctxErr error
	r := chi.NewRouter()
	r.Use(BodyBudget(8))
	r.Use(BodyBudget(3))
	r.Post("/", func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		ctxErr = BodyBudgetErr(r.Context())
	})

	// A 4-byte body exceeds the inner 3-byte budget.
	orig := strings.NewReader("abcd")
	req := httptest.NewRequest("POST", "/", orig)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if !errors.Is(ctxErr, ErrBodyBudgetExceeded) {
		t.Fatalf("context error = %v, want ErrBodyBudgetExceeded", ctxErr)
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}

	// A 2-byte body is within budget: no error, 200.
	ctxErr = nil
	orig2 := strings.NewReader("ab")
	req2 := httptest.NewRequest("POST", "/", orig2)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if ctxErr != nil {
		t.Fatalf("context error = %v, want nil", ctxErr)
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec2.Code)
	}
}

// TestBodyBudget_NoPreRead verifies the middleware never reads ahead: the
// original reader sees exactly the handler's Read calls, in order, with the
// same buffer sizes.
func TestBodyBudget_NoPreRead(t *testing.T) {
	orig := &oneByteReader{data: []byte("hello")}
	req := httptest.NewRequest("POST", "/", orig)
	w := httptest.NewRecorder()

	r := chi.NewRouter()
	r.Use(BodyBudget(100))
	r.Post("/", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("unexpected read error: %v", err)
				return
			}
		}
	})
	r.ServeHTTP(w, req)

	if got := w.Body.String(); got != "hello" {
		t.Fatalf("got body %q, want %q", got, "hello")
	}
	// The handler read 5 data bytes + 1 EOF with a 1-byte buffer; the original
	// reader must have been called exactly 6 times with a 1-byte buffer.
	if orig.reads != 6 {
		t.Fatalf("original reader called %d times, want 6 (no pre-read)", orig.reads)
	}
	for i, sz := range orig.bufSizes {
		if sz != 1 {
			t.Fatalf("read %d: buffer size %d, want 1 (no pre-read)", i, sz)
		}
	}
}

// TestBodyBudget_RoutesWithoutMiddlewareUnaffected verifies a route with no
// BodyBudget layer accepts any body size.
func TestBodyBudget_RoutesWithoutMiddlewareUnaffected(t *testing.T) {
	uts := httptest.NewServer(newUnlimitedRouter())
	defer uts.Close()

	status, _ := doRequest(t, uts, "/unlimited", strings.Repeat("z", 1<<20), false)
	if status != http.StatusOK {
		t.Fatalf("unlimited route: got status %d, want 200", status)
	}
}
