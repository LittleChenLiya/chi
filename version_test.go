package chi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(body)
}

// TestVersionedMuxConsecutiveSwaps publishes three versions back to back
// with one parked request per version and checks the drain accounting of
// every retired version independently.
func TestVersionedMuxConsecutiveSwaps(t *testing.T) {
	type gate struct {
		entered chan struct{}
		release chan struct{}
	}
	gates := make([]gate, 3)
	muxes := make([]*Mux, 3)
	for i := range muxes {
		gates[i] = gate{entered: make(chan struct{}), release: make(chan struct{})}
		m := NewMux()
		m.Use(blockingMiddleware(gates[i].entered, gates[i].release))
		body := fmt.Sprintf("v%d", i+1)
		m.Get("/v", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(body))
		})
		muxes[i] = m
	}

	vm := NewVersionedMux(muxes[0])
	srv := httptest.NewServer(vm)
	defer srv.Close()

	parked := make([]chan string, 3)
	for i := range parked {
		parked[i] = make(chan string, 1)
	}

	// Park one request on v1, then swap to v2, park one, swap to v3.
	go func() { parked[0] <- getBody(t, srv.URL+"/v") }()
	<-gates[0].entered
	retired1 := vm.Swap(muxes[1])

	go func() { parked[1] <- getBody(t, srv.URL+"/v") }()
	<-gates[1].entered
	retired2 := vm.Swap(muxes[2])

	go func() { parked[2] <- getBody(t, srv.URL+"/v") }()
	<-gates[2].entered

	// Swap returns the just-retired version; Current returns the new one.
	versions := []*Version{retired1, retired2, vm.Current()}

	// Drain accounting: each retired version holds exactly its own request.
	for i, v := range versions {
		if got := v.Active(); got != 1 {
			t.Fatalf("version %d: expected 1 active request, got %d", v.ID(), got)
		}
		if v.Drained() {
			t.Fatalf("version %d must not be drained yet", v.ID())
		}
		if i < 2 && !v.Retired() {
			t.Fatalf("version %d must be retired", v.ID())
		}
	}
	if versions[2].Retired() {
		t.Fatal("current version must not be retired")
	}

	// Release in publish order; each version drains exactly after its own
	// request leaves, and answers with its own body.
	for i, v := range versions {
		close(gates[i].release)
		want := fmt.Sprintf("v%d", i+1)
		if body := <-parked[i]; body != want {
			t.Fatalf("version %d: expected %q, got %q", v.ID(), want, body)
		}
		if got := v.Active(); got != 0 {
			t.Fatalf("version %d: expected 0 active after release, got %d", v.ID(), got)
		}
		if i < 2 {
			if err := v.Wait(context.Background()); err != nil {
				t.Fatalf("version %d drain wait: %v", v.ID(), err)
			}
			if !v.Drained() {
				t.Fatalf("version %d must be drained", v.ID())
			}
		}
	}
}

// TestVersionedMuxPanicReleases ensures a panicking handler still releases
// its version reference while the panic propagates to the caller.
func TestVersionedMuxPanicReleases(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})

	mux1 := NewMux()
	mux1.Use(blockingMiddleware(entered, release))
	mux1.Get("/boom", func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	})

	vm := NewVersionedMux(mux1)

	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		vm.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/boom", nil))
	}()
	<-entered

	mux2 := NewMux()
	mux2.Get("/boom", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	retired := vm.Swap(mux2)
	if got := retired.Active(); got != 1 {
		t.Fatalf("expected 1 active request before panic, got %d", got)
	}

	close(release)
	if r := <-panicked; r != "kaboom" {
		t.Fatalf("panic must propagate to the caller, got %v", r)
	}

	// The panic released the version reference, so the retired version
	// drains without any further help.
	if err := retired.Wait(context.Background()); err != nil {
		t.Fatalf("drain wait after panic: %v", err)
	}
	if got := retired.Active(); got != 0 {
		t.Fatalf("panic must release the version reference, active=%d", got)
	}
}

// TestVersionedMuxClientDisconnect ensures a request whose client goes away
// still releases its version reference once the handler gives up.
func TestVersionedMuxClientDisconnect(t *testing.T) {
	ctxDone := make(chan struct{})

	mux1 := NewMux()
	mux1.Get("/slow", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // client disconnected
		close(ctxDone)
	})

	vm := NewVersionedMux(mux1)
	srv := httptest.NewServer(vm)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/slow", nil)
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

	// Wait until the server is actually serving the request, then drop the
	// client. The handler observes the cancelled request context.
	for vm.Current().Active() != 1 {
	}
	cancel()
	<-ctxDone
	<-respErr

	mux2 := NewMux()
	mux2.Get("/slow", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	retired := vm.Swap(mux2)
	if err := retired.Wait(context.Background()); err != nil {
		t.Fatalf("client disconnect must release the version: %v", err)
	}
}

// TestVersionedMuxHijack documents the drain cutoff for hijacked
// connections: the version reference is released when the hijacking
// handler returns, not when the hijacked connection is closed.
func TestVersionedMuxHijack(t *testing.T) {
	handlerReturned := make(chan struct{})
	hijacked := make(chan struct{})

	mux1 := NewMux()
	mux1.Get("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		close(hijacked)
		// Returning from ServeHTTP is the drain cutoff point: the version
		// reference is released here even though the connection outlives
		// the handler (the client may keep it open).
		close(handlerReturned)
	})

	vm := NewVersionedMux(mux1)
	srv := httptest.NewServer(vm)
	defer srv.Close()

	if body := getBody(t, srv.URL+"/ws"); body != "ok" {
		t.Fatalf("expected ok, got %q", body)
	}
	<-hijacked
	<-handlerReturned

	// The handler has returned, so the pinned version must already be
	// released even though nothing closed the underlying connection first.
	if got := vm.Current().Active(); got != 0 {
		t.Fatalf("hijacking handler returned, version must be released, active=%d", got)
	}
}

// TestVersionedMuxRace hammers the container with requests while versions
// are swapped concurrently. Run with -race.
func TestVersionedMuxRace(t *testing.T) {
	mkMux := func(body string) *Mux {
		m := NewMux()
		m.Get("/v", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(body))
		})
		return m
	}

	vm := NewVersionedMux(mkMux("v1"))
	srv := httptest.NewServer(vm)
	defer srv.Close()

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 25; j++ {
				resp, err := http.Get(srv.URL + "/v")
				if err != nil {
					t.Errorf("GET: %v", err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 2; i <= 20; i++ {
			retired := vm.Swap(mkMux(fmt.Sprintf("v%d", i)))
			_ = retired.Active()
			_ = retired.Drained()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 50; i++ {
			_ = vm.Current().ID()
		}
	}()

	close(start)
	wg.Wait()
}

// blockingMiddleware returns a middleware that parks a request until the
// release channel is closed. entered is closed when the first request
// arrives, giving the test a barrier without any sleeps.
func blockingMiddleware(entered chan<- struct{}, release <-chan struct{}) func(http.Handler) http.Handler {
	var once sync.Once
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			once.Do(func() { close(entered) })
			<-release
			next.ServeHTTP(w, r)
		})
	}
}

// TestVersionedMuxSwap is the core acceptance flow: the old version serves
// "v1", a request is parked inside a middleware, v2 is published, new
// requests see "v2", and the parked request still completes as "v1".
func TestVersionedMuxSwap(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})

	mux1 := NewMux()
	mux1.Use(blockingMiddleware(entered, release))
	mux1.Get("/v", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("v1"))
	})

	vm := NewVersionedMux(mux1)
	srv := httptest.NewServer(vm)
	defer srv.Close()

	// Pin a request to v1 inside the blocking middleware.
	parked := make(chan string, 1)
	go func() { parked <- getBody(t, srv.URL+"/v") }()
	<-entered

	v1 := vm.Current()
	if v1.ID() != 1 {
		t.Fatalf("expected version ID 1, got %d", v1.ID())
	}
	if got := v1.Active(); got != 1 {
		t.Fatalf("expected 1 active request on v1, got %d", got)
	}

	// Publish v2 atomically; the old tree must not be modified.
	mux2 := NewMux()
	mux2.Get("/v", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("v2"))
	})
	retired := vm.Swap(mux2)
	if retired != v1 {
		t.Fatal("Swap must return the retired v1 version")
	}
	if !retired.Retired() {
		t.Fatal("v1 must be marked retired after Swap")
	}
	if vm.Current().ID() != 2 {
		t.Fatalf("expected current version ID 2, got %d", vm.Current().ID())
	}

	// New requests are pinned to v2.
	if body := getBody(t, srv.URL+"/v"); body != "v2" {
		t.Fatalf("expected v2 after swap, got %q", body)
	}

	// The retired version still has one active request and is not drained.
	if got := retired.Active(); got != 1 {
		t.Fatalf("expected 1 active request on retired v1, got %d", got)
	}
	if retired.Drained() {
		t.Fatal("v1 must not be drained while a request is parked")
	}

	// Cancelling the wait must not terminate the parked request.
	ctx, cancel := context.WithCancel(context.Background())
	waitErr := make(chan error, 1)
	go func() { waitErr <- retired.Wait(ctx) }()
	cancel()
	if err := <-waitErr; err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if got := retired.Active(); got != 1 {
		t.Fatalf("cancelled wait must not terminate requests, active=%d", got)
	}

	// Release the parked request: it entered on v1, so it answers "v1".
	close(release)
	if body := <-parked; body != "v1" {
		t.Fatalf("parked request must keep its pinned version, got %q", body)
	}

	// Now the retired version drains; an uncancelled wait observes it.
	if err := retired.Wait(context.Background()); err != nil {
		t.Fatalf("drain wait: %v", err)
	}
	if got := retired.Active(); got != 0 {
		t.Fatalf("expected 0 active requests after drain, got %d", got)
	}
	if !retired.Drained() {
		t.Fatal("v1 must report drained after its last request finished")
	}
}

// TestVersionedMuxNestedMount verifies that a request pinned to a version
// keeps that version through nested Mounts and sub-router middlewares.
func TestVersionedMuxNestedMount(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})

	sub1 := NewMux()
	sub1.Use(blockingMiddleware(entered, release))
	sub1.Get("/v", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("sub-v1"))
	})
	mux1 := NewMux()
	mux1.Mount("/api", sub1)

	vm := NewVersionedMux(mux1)
	srv := httptest.NewServer(vm)
	defer srv.Close()

	parked := make(chan string, 1)
	go func() { parked <- getBody(t, srv.URL+"/api/v") }()
	<-entered

	sub2 := NewMux()
	sub2.Get("/v", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("sub-v2"))
	})
	mux2 := NewMux()
	mux2.Mount("/api", sub2)
	retired := vm.Swap(mux2)

	if body := getBody(t, srv.URL+"/api/v"); body != "sub-v2" {
		t.Fatalf("expected sub-v2 after swap, got %q", body)
	}

	close(release)
	if body := <-parked; body != "sub-v1" {
		t.Fatalf("nested mount must follow the pinned version, got %q", body)
	}
	if err := retired.Wait(context.Background()); err != nil {
		t.Fatalf("drain wait: %v", err)
	}
}
