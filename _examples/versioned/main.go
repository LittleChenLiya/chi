// Versioned router demo: publishes a new *Mux with Swap while a request
// is still in flight on the old tree, then waits for the old version to
// drain. Run with:
//
//	go run ./_examples/versioned
//
// The demo is self-contained, finishes in a few seconds, and prints the
// served version, each request result, and the drain status.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

func buildMux(version string, onEnter func()) *chi.Mux {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if onEnter != nil {
				onEnter()
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Get("/v", func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte(version))
	})
	return r
}

func get(url string) string {
	resp, err := http.Get(url)
	if err != nil {
		return "error: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func main() {
	// Gate lets the demo hold one v1 request inside its middleware.
	gate := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once

	v1 := buildMux("v1", func() {
		once.Do(func() {
			close(entered)
			<-gate
		})
	})
	vx := chi.NewVersionedMux(v1)

	srv := httptest.NewServer(vx)
	defer srv.Close()
	fmt.Println("[demo] server started, current version serves: v1")

	// 1. Start a request that gets stuck inside the v1 middleware.
	blocked := make(chan string, 1)
	go func() { blocked <- get(srv.URL + "/v") }()
	<-entered
	fmt.Println("[demo] request A pinned to v1 and blocked in middleware")

	// 2. Publish v2 while request A is still in flight.
	retired := vx.Swap(buildMux("v2", nil))
	fmt.Printf("[demo] swapped to v2, retired version active=%d retired=%v\n",
		retired.Active(), retired.Retired())

	// 3. New requests go to v2; the blocked one still belongs to v1.
	fmt.Printf("[demo] request B (new)  -> %q\n", get(srv.URL+"/v"))

	// 4. Wait for the old version to drain in the background.
	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		drained <- retired.Wait(ctx)
	}()
	select {
	case <-retired.Drained():
		fmt.Println("[demo] unexpected: v1 drained before gate opened")
	default:
		fmt.Println("[demo] v1 not drained yet, active request still holds it")
	}

	// 5. Release the stuck request; it still answers with v1.
	close(gate)
	fmt.Printf("[demo] request A (old)  -> %q\n", <-blocked)

	// 6. The retired version is now fully drained.
	if err := <-drained; err != nil {
		fmt.Println("[demo] drain wait failed:", err)
		return
	}
	fmt.Printf("[demo] v1 drained: active=%d, current version serves: %q\n",
		retired.Active(), get(srv.URL+"/v"))
}
