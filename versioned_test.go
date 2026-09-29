package chi

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recoverMiddleware is a minimal local Recoverer (the middleware package
// imports chi, so it cannot be used from this internal test file).
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func versionMux(body string, mws ...func(http.Handler) http.Handler) *Mux {
	m := NewMux()
	m.Use(mws...)
	m.Get("/v", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	})
	return m
}

func serveString(h http.Handler, target string) string {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec.Body.String()
}

// Acceptance: an in-flight request pinned to v1 keeps the old tree while
// new requests use v2 after Swap, and the retired version drains only
// after the old request is released.
func TestVersionedMuxSwapWhileRequestInFlight(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	blocking := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-release
			next.ServeHTTP(w, r)
		})
	}

	vx := NewVersionedMux(versionMux("v1", blocking))

	blocked := make(chan string, 1)
	go func() { blocked <- serveString(vx, "/v") }()
	<-entered // the request is pinned to v1 inside its middleware

	retired := vx.Swap(versionMux("v2"))

	if got := serveString(vx, "/v"); got != "v2" {
		t.Fatalf("new request after Swap = %q, want v2", got)
	}
	if got := retired.Active(); got != 1 {
		t.Fatalf("retired.Active() = %d, want 1", got)
	}
	select {
	case <-retired.Drained():
		t.Fatal("retired version drained while a request is still blocked")
	default:
	}

	close(release)
	if got := <-blocked; got != "v1" {
		t.Fatalf("released old request = %q, want v1", got)
	}

	<-retired.Drained()
	// Re-verify the drain bookkeeping after the barrier fired.
	if got := retired.Active(); got != 0 {
		t.Fatalf("retired.Active() after drain = %d, want 0", got)
	}
	if got := vx.Current().Active(); got != 0 {
		t.Fatalf("current.Active() after drain = %d, want 0", got)
	}
	if !retired.Retired() {
		t.Fatal("retired version reports Retired() = false")
	}
	if retired.Mux() == vx.Current().Mux() {
		t.Fatal("retired and current versions share the same *Mux")
	}
}

// Wait must unblock on context cancellation without disturbing the
// in-flight requests holding the version.
func TestVersionedMuxWaitCancelKeepsRequests(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	blocking := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-release
			next.ServeHTTP(w, r)
		})
	}

	vx := NewVersionedMux(versionMux("v1", blocking))

	done := make(chan string, 1)
	go func() { done <- serveString(vx, "/v") }()
	<-entered

	retired := vx.Swap(versionMux("v2"))

	ctx, cancel := context.WithCancel(context.Background())
	waitErr := make(chan error, 1)
	go func() { waitErr <- retired.Wait(ctx) }()

	cancel()
	if err := <-waitErr; err != context.Canceled {
		t.Fatalf("Wait after cancel = %v, want context.Canceled", err)
	}

	// The cancelled wait must not have terminated the pinned request.
	select {
	case <-retired.Drained():
		t.Fatal("version drained by a cancelled wait")
	default:
	}
	if got := retired.Active(); got != 1 {
		t.Fatalf("retired.Active() after cancelled wait = %d, want 1", got)
	}

	close(release)
	if got := <-done; got != "v1" {
		t.Fatalf("request after cancelled wait = %q, want v1", got)
	}
	if err := retired.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after release = %v, want nil", err)
	}
	if got := retired.Active(); got != 0 {
		t.Fatalf("retired.Active() after drain = %d, want 0", got)
	}
}

// A panic converted to a 500 by a Recoverer-style middleware inside the
// published mux must still release the version reference.
func TestVersionedMuxPanicWithRecoverer(t *testing.T) {
	m := NewMux()
	m.Use(recoverMiddleware)
	m.Get("/boom", func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	vx := NewVersionedMux(m)

	rec := httptest.NewRecorder()
	vx.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := vx.Current().Active(); got != 0 {
		t.Fatalf("Active() after recovered panic = %d, want 0", got)
	}

	retired := vx.Swap(versionMux("v2"))
	select {
	case <-retired.Drained():
	default:
		t.Fatal("version with only panicking requests did not drain")
	}
}

// A panic propagating out of ServeHTTP (no recoverer installed) must
// still release the version reference while unwinding.
func TestVersionedMuxPanicPropagateReleases(t *testing.T) {
	m := NewMux()
	m.Get("/boom", func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	vx := NewVersionedMux(m)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected panic to propagate out of ServeHTTP")
			}
		}()
		vx.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))
	}()

	if got := vx.Current().Active(); got != 0 {
		t.Fatalf("Active() after propagated panic = %d, want 0", got)
	}
}

// Barrier-driven chain of consecutive swaps: each generation blocks one
// request in its middleware, and every retired generation must drain in
// order without sleeps.
func TestVersionedMuxContinuousSwap(t *testing.T) {
	const generations = 5

	type barrier struct {
		entered chan struct{}
		release chan struct{}
	}
	barriers := make([]barrier, generations)
	for i := range barriers {
		barriers[i] = barrier{entered: make(chan struct{}), release: make(chan struct{})}
	}

	build := func(gen int) *Mux {
		b := barriers[gen]
		mw := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(b.entered)
				<-b.release
				next.ServeHTTP(w, r)
			})
		}
		return versionMux(fmt.Sprintf("v%d", gen+1), mw)
	}

	vx := NewVersionedMux(build(0))

	results := make(chan string, generations)
	retired := make([]*Version, 0, generations-1)
	for gen := 0; gen < generations; gen++ {
		go func() { results <- serveString(vx, "/v") }()
		<-barriers[gen].entered

		if got := vx.Current().Active(); got != 1 {
			t.Fatalf("gen %d: current.Active() = %d, want 1", gen, got)
		}
		if gen+1 < generations {
			retired = append(retired, vx.Swap(build(gen+1)))
		}
	}

	// Release the generations in publish order; each retired version must
	// drain exactly when its own request finishes.
	for gen := 0; gen < generations-1; gen++ {
		close(barriers[gen].release)
		if got := <-results; got != fmt.Sprintf("v%d", gen+1) {
			t.Fatalf("gen %d request = %q", gen, got)
		}
		<-retired[gen].Drained()
		if got := retired[gen].Active(); got != 0 {
			t.Fatalf("gen %d: retired.Active() after drain = %d, want 0", gen, got)
		}
	}

	close(barriers[generations-1].release)
	if got := <-results; got != fmt.Sprintf("v%d", generations) {
		t.Fatalf("final request = %q", got)
	}
	if got := vx.Current().Active(); got != 0 {
		t.Fatalf("final current.Active() = %d, want 0", got)
	}
}

// A client disconnect cancels the request context; once the handler
// honors it and returns, the version reference is released.
func TestVersionedMuxClientDisconnect(t *testing.T) {
	entered := make(chan struct{})
	left := make(chan struct{})

	m := NewMux()
	m.Get("/v", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(left)
	})

	vx := NewVersionedMux(m)
	srv := httptest.NewServer(vx)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v", nil)
	if err != nil {
		t.Fatal(err)
	}
	respErr := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		respErr <- err
	}()

	<-entered
	if got := vx.Current().Active(); got != 1 {
		t.Fatalf("Active() with connected client = %d, want 1", got)
	}

	cancel() // client goes away
	<-left   // handler observed the disconnect and returned
	<-respErr

	retired := vx.Swap(versionMux("v2"))
	select {
	case <-retired.Drained():
	default:
		t.Fatal("version not drained after disconnected client request returned")
	}
	if got := retired.Active(); got != 0 {
		t.Fatalf("retired.Active() = %d, want 0", got)
	}
}

// A handler that hijacks the connection releases its version pin when
// the handler returns; the hijacked connection does not keep the old
// mux alive.
func TestVersionedMuxHijackDrains(t *testing.T) {
	hijacked := make(chan struct{})

	m := NewMux()
	m.Get("/v", func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nv1")
		rw.Flush()
		close(hijacked)
		// Returning here ends the version pin even though the hijacked
		// connection is still open until the deferred Close runs.
	})

	vx := NewVersionedMux(m)
	srv := httptest.NewServer(vx)
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "GET /v HTTP/1.1\r\nHost: %s\r\n\r\n", srv.Listener.Addr()); err != nil {
		t.Fatal(err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "v1" {
		t.Fatalf("hijacked response = %q, want v1", string(body))
	}
	<-hijacked

	retired := vx.Swap(versionMux("v2"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := retired.Wait(ctx); err != nil {
		t.Fatalf("version pinned by hijacked connection did not drain: %v", err)
	}
	if got := retired.Active(); got != 0 {
		t.Fatalf("retired.Active() after drain = %d, want 0", got)
	}
}

// Concurrent serves and swaps must keep the bookkeeping exact; run with
// -race to catch synchronization regressions.
func TestVersionedMuxConcurrentSwapServe(t *testing.T) {
	vx := NewVersionedMux(versionMux("v0"))

	const workers = 8
	const rounds = 50

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				rec := httptest.NewRecorder()
				vx.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v", nil))
				if !strings.HasPrefix(rec.Body.String(), "v") {
					t.Errorf("unexpected body %q", rec.Body.String())
					return
				}
			}
		}()
	}

	retired := make([]*Version, 0, rounds)
	for i := 0; i < rounds; i++ {
		retired = append(retired, vx.Swap(versionMux(fmt.Sprintf("v%d", i+1))))
	}
	wg.Wait()

	for i, v := range retired {
		if err := v.Wait(context.Background()); err != nil {
			t.Fatalf("retired version %d did not drain: %v", i, err)
		}
		if got := v.Active(); got != 0 {
			t.Fatalf("retired version %d Active() = %d, want 0", i, got)
		}
	}
	if got := vx.Current().Active(); got != 0 {
		t.Fatalf("current.Active() = %d, want 0", got)
	}
}

func TestVersionedMuxNilGuards(t *testing.T) {
	assertPanic := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: expected panic", name)
			}
		}()
		fn()
	}
	assertPanic("NewVersionedMux(nil)", func() { NewVersionedMux(nil) })
	assertPanic("Swap(nil)", func() { NewVersionedMux(versionMux("v1")).Swap(nil) })
}
