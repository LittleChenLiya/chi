package middleware

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
)

// countReadCloser is a test fixture: a request body that counts how many
// bytes are actually read and how many times Close is called.
type countReadCloser struct {
	data   []byte
	pos    int
	reads  atomic.Int64
	closes atomic.Int64
}

func newCountReadCloser(data string) *countReadCloser {
	return &countReadCloser{data: []byte(data)}
}

func (c *countReadCloser) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := copy(p, c.data[c.pos:])
	c.pos += n
	c.reads.Add(int64(n))
	return n, nil
}

func (c *countReadCloser) Close() error {
	c.closes.Add(1)
	return nil
}

func (c *countReadCloser) readCount() int64  { return c.reads.Load() }
func (c *countReadCloser) closeCount() int64 { return c.closes.Load() }

// readAllHandler drains the body and reports success via the context budget
// state; it does not write a response when the body is too large, leaving the
// 413 to the middleware.
func readAllHandler(w http.ResponseWriter, r *http.Request) {
	_, err := io.Copy(io.Discard, r.Body)
	if err != nil {
		return // middleware emits 413 on the unstarted-response path
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func newNestedBudgetRouter(outer, inner int64) http.Handler {
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(BodyBudget(outer))
		r.Post("/inner", BodyBudget(inner)(http.HandlerFunc(readAllHandler)).ServeHTTP)
	})
	// A route without any BodyBudget middleware.
	r.Post("/open", readAllHandler)
	return r
}

// TestBodyBudgetNestedMin verifies: outer limit 8, sub-route limit 3; a
// 4-byte body is rejected only on the sub-route and a 3-byte body succeeds.
func TestBodyBudgetNestedMin(t *testing.T) {
	r := newNestedBudgetRouter(8, 3)

	// 4 bytes on the sub-route (inner limit 3 wins over outer limit 8).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/inner", strings.NewReader("abcd"))
	req.ContentLength = 4
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("inner 4 bytes: got status %d, want 413", rec.Code)
	}

	// 3 bytes on the sub-route succeeds (boundary is inclusive).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/inner", strings.NewReader("abc"))
	req.ContentLength = 3
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("inner 3 bytes: got status %d body %q, want 200 ok", rec.Code, rec.Body.String())
	}

	// Outer limit 8 applies to a route whose own layer would allow more; here
	// the inner route still caps at 3, so check the outer-only behavior with a
	// dedicated router: 8 bytes allowed, 9 rejected.
	outerOnly := chi.NewRouter()
	outerOnly.Group(func(r chi.Router) {
		r.Use(BodyBudget(8))
		r.Post("/x", readAllHandler)
	})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("12345678"))
	outerOnly.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("outer 8 bytes: got status %d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("123456789"))
	outerOnly.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("outer 9 bytes: got status %d, want 413", rec.Code)
	}
}

// TestBodyBudgetUnknownContentLength ensures an unknown Content-Length
// (chunked transfer encoding) cannot bypass the budget. Exercises a real
// loopback server with httptest.
func TestBodyBudgetUnknownContentLength(t *testing.T) {
	h := BodyBudget(3)(http.HandlerFunc(readAllHandler))
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Chunked request: Go sends 4 bytes without a Content-Length.
	resp, err := srv.Client().Post(srv.URL, "application/octet-stream", strings.NewReader("abcd"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked 4 bytes: got status %d, want 413", resp.StatusCode)
	}

	// 3 bytes chunked succeeds.
	resp2, err := srv.Client().Post(srv.URL, "application/octet-stream", strings.NewReader("abc"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("chunked 3 bytes: got status %d, want 200", resp2.StatusCode)
	}
}

// TestBodyBudgetAlreadyWritten ensures that when the handler writes its own
// response first and only then trips the budget, the middleware does not emit
// a second status or append an error body.
func TestBodyBudgetAlreadyWritten(t *testing.T) {
	h := BodyBudget(3)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		if _, err := io.WriteString(w, "started"); err != nil {
			t.Fatalf("write started: %v", err)
		}
		// Read the oversized body after responding; this trips the budget.
		_, _ = io.Copy(io.Discard, r.Body)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("abcd"))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want 202", rec.Code)
	}
	body := rec.Body.String()
	if body != "started" {
		t.Fatalf("response body changed after handler wrote: got %q, want %q (no appended error page)", body, "started")
	}
}

// TestBodyBudgetCloseOnce verifies the original request body is closed
// exactly once despite nested BodyBudget layers, including repeated Close.
func TestBodyBudgetCloseOnce(t *testing.T) {
	body := newCountReadCloser("abcd")

	var innerClosed atomic.Bool
	var wrappedType string

	h := BodyBudget(8)(
		BodyBudget(3)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			// Handler closes the body itself.
			_ = r.Body.Close()
			_ = r.Body.Close() // duplicate close must not reach the original body
			switch r.Body.(type) {
			case *budgetReader:
				wrappedType = "budgetReader"
			}
			innerClosed.Store(true)
			w.WriteHeader(http.StatusOK)
		})))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Body = body
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !innerClosed.Load() {
		t.Fatal("inner handler did not run")
	}
	if wrappedType != "budgetReader" {
		t.Fatalf("body was re-wrapped by inner layer: %q", wrappedType)
	}
	if got := body.closeCount(); got != 1 {
		t.Fatalf("original body Close called %d times, want 1", got)
	}
	if got := body.readCount(); got != 4 {
		t.Fatalf("charged bytes = %d, want 4 actual reads", got)
	}
}

// TestBodyBudgetNoPreread ensures the middleware does not consume the body
// before the handler reads it: zero bytes are charged until the handler reads.
func TestBodyBudgetNoPreread(t *testing.T) {
	body := newCountReadCloser("abcdefgh")
	var usedBefore, limit int64

	h := BodyBudget(3)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usedBefore = BodyBudgetUsed(r.Context())
		limit = BodyBudgetLimit(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Body = body
	h.ServeHTTP(httptest.NewRecorder(), req)

	if usedBefore != 0 {
		t.Fatalf("middleware charged %d bytes before handler read, want 0 (no pre-read)", usedBefore)
	}
	if limit != 3 {
		t.Fatalf("effective limit = %d, want 3", limit)
	}
	if got := body.readCount(); got != 0 {
		t.Fatalf("middleware consumed %d bytes of the body, want 0", got)
	}
}

// TestBodyBudgetContextError checks the context error query API and the
// recognizable sentinel/typed error.
func TestBodyBudgetContextError(t *testing.T) {
	var seen error
	var used int64
	h := BodyBudget(3)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		seen = BodyBudgetError(r.Context())
		used = BodyBudgetUsed(r.Context())
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("abcd"))
	h.ServeHTTP(rec, req)

	if seen == nil {
		t.Fatal("BodyBudgetError returned nil after exceeding budget")
	}
	if !errors.Is(seen, ErrBodyBudgetExceeded) {
		t.Fatalf("error %v does not match ErrBodyBudgetExceeded", seen)
	}
	var bee *BodyBudgetExceededError
	if !errors.As(seen, &bee) {
		t.Fatalf("error %T is not *BodyBudgetExceededError", seen)
	}
	if bee.Limit != 3 || bee.Read != 4 {
		t.Fatalf("exceeded error = %+v, want limit 3 read 4", bee)
	}
	if used != 4 {
		t.Fatalf("BodyBudgetUsed = %d, want 4", used)
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

// TestBodyBudgetAbsent verifies routes without the middleware are unaffected.
func TestBodyBudgetAbsent(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/open", strings.NewReader("a much longer body than any budget"))
	newNestedBudgetRouter(8, 3).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("unprotected route: status %d body %q, want 200 ok", rec.Code, rec.Body.String())
	}
}

// TestBodyBudgetReaderError verifies the reader itself receives the
// recognizable error, including on subsequent reads.
func TestBodyBudgetReaderError(t *testing.T) {
	body := newCountReadCloser("abcd")
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Body = body
	B := &bodyBudget{}
	B.tighten(3)
	br := &budgetReader{rc: &onceReadCloser{rc: r.Body}, budget: B}

	buf := make([]byte, 8)
	n, err := br.Read(buf)
	if err == nil || !errors.Is(err, ErrBodyBudgetExceeded) {
		t.Fatalf("first crossing read: n=%d err=%v, want budget exceeded", n, err)
	}
	if n != 4 {
		t.Fatalf("crossing read delivered %d bytes, want 4", n)
	}
	n2, err2 := br.Read(buf)
	if n2 != 0 || !errors.Is(err2, ErrBodyBudgetExceeded) {
		t.Fatalf("subsequent read: n=%d err=%v, want 0 + budget exceeded", n2, err2)
	}
	if err := br.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := body.closeCount(); got != 1 {
		t.Fatalf("original body Close called %d times, want 1", got)
	}
}

// TestBodyBudgetLooseOuterMin verifies the minimum is taken even when the
// outer layer is looser than the inner one and vice-versa (inner 8, outer 3).
func TestBodyBudgetLooseOuterMin(t *testing.T) {
	// Outer 3, inner 8: the stricter outer limit still applies.
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(BodyBudget(3))
		r.Post("/in", BodyBudget(8)(http.HandlerFunc(readAllHandler)).ServeHTTP)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/in", strings.NewReader("abcd"))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("min(3,8) on 4 bytes: got %d, want 413", rec.Code)
	}
}
