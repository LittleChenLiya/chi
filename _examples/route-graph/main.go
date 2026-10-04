// Command route-graph builds a chi router with several mounted subrouters
// and prints a diagnostic report of the route graph. The report is generated
// entirely from the Route traversal API — no HTTP requests are sent — and is
// stable across runs, so it can be printed repeatedly or checked into source
// control.
//
// Run it with:
//
//	go run .
//
package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
)

func handler(w http.ResponseWriter, r *http.Request) {}

func main() {
	r := chi.NewRouter()

	// A same-shape conflict: GET /users/{id} on a subrouter mounted at /
	// and GET /users/{name} on the parent router match the same shape.
	sub := chi.NewRouter()
	sub.Get("/users/{id}", handler)
	r.Mount("/", sub)
	r.Get("/users/{name}", handler)

	// GET and POST on the same shape do not conflict.
	r.Post("/users/{name}", handler)

	// Two different regexps on the same shape are reported as pending.
	sub2 := chi.NewRouter()
	sub2.Get("/items/{id:[0-9]+}", handler)
	r.Mount("/alt", sub2)
	r.Get("/items/{name:[a-z]+}", handler)

	// Nested mounts: the source chain for /api/v1/accounts is root -> /api -> /api/v1.
	r.Route("/api", func(r chi.Router) {
		r.Route("/v1", func(r chi.Router) {
			r.Get("/accounts", handler)
		})
	})

	// A broad mount at /web and a narrow mount at /web/api. The broad mount's
	// /api/widgets route is covered by the narrow mount.
	broad := chi.NewRouter()
	broad.Get("/api/widgets", handler)
	r.Mount("/web", broad)
	r.Route("/web/api", func(r chi.Router) {
		r.Get("/widgets", handler)
	})

	g := chi.NewRouteGraph(r)
	g.Report(os.Stdout)

	// The report is stable: running it again yields identical output.
	fmt.Println()
	fmt.Println("--- second run (identical) ---")
	g.Report(os.Stdout)
}
