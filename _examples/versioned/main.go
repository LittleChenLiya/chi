// Versioned routing demo: publishes a new *chi.Mux atomically while an old
// request is still in flight, and shows version pinning plus drain status.
//
//	go run ./_examples/versioned
//
// The demo is self-contained (httptest), finishes in well under 90 seconds
// and prints the published version, every request result and the drain
// state of the retired version at each step.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/go-chi/chi/v5"
)

func get(url string) string {
	resp, err := http.Get(url)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}
	return string(body)
}

func drainStatus(v *chi.Version) string {
	return fmt.Sprintf("version=%d retired=%v active=%d drained=%v",
		v.ID(), v.Retired(), v.Active(), v.Drained())
}

func main() {
	// v1: /v answers "v1", but a middleware can park one request.
	entered := make(chan struct{})
	release := make(chan struct{})

	mux1 := chi.NewMux()
	mux1.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-release // parked until the demo lets it go
			next.ServeHTTP(w, r)
		})
	})
	mux1.Get("/v", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("v1"))
	})

	vm := chi.NewVersionedMux(mux1)
	srv := httptest.NewServer(vm)
	defer srv.Close()

	fmt.Printf("published: %s\n", drainStatus(vm.Current()))

	// Park a request inside the v1 middleware.
	parked := make(chan string, 1)
	go func() { parked <- get(srv.URL + "/v") }()
	<-entered
	fmt.Printf("request parked on: %s\n", drainStatus(vm.Current()))

	// Build a complete new mux and publish it atomically.
	mux2 := chi.NewMux()
	mux2.Get("/v", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("v2"))
	})
	retired := vm.Swap(mux2)
	fmt.Printf("swapped: current=%s\n", drainStatus(vm.Current()))
	fmt.Printf("swapped: retired=%s\n", drainStatus(retired))

	// New requests are pinned to v2.
	fmt.Printf("GET /v -> %q (new request, pinned to version %d)\n",
		get(srv.URL+"/v"), vm.Current().ID())

	// Wait for the old version to drain, with a 90s ceiling. The wait is
	// cancellable; cancelling it would not terminate the parked request.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	drained := make(chan error, 1)
	go func() { drained <- retired.Wait(ctx) }()

	// Let the parked request finish: it still answers with its pinned v1.
	close(release)
	fmt.Printf("GET /v -> %q (parked request, pinned to version %d)\n",
		<-parked, retired.ID())

	if err := <-drained; err != nil {
		fmt.Printf("drain wait stopped: %v\n", err)
	} else {
		fmt.Printf("drained: %s\n", drainStatus(retired))
	}
}
