// This example builds a read-only method summary of every route registered on
// a chi router, without sending any HTTP request or running any handler or
// middleware. It is meant for API checking and documentation tooling.
//
// Run it from this directory with:
//
//	$ go run .
//
// The summary follows Routes/Walk and Mount semantics: mount prefixes and
// parameter segments are preserved, methods on the same path template are
// merged, templates that differ only in a parameter name stay separate, and a
// mount backed by a non-chi http.Handler is reported as unexpanded rather
// than having methods fabricated. An explicitly registered HEAD is also
// distinguished from a GET-only route.
package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
)

func main() {
	r := chi.NewRouter()

	// A versioned API mounted under /v1.
	r.Route("/v1", func(r chi.Router) {
		r.Get("/items/{id}", func(w http.ResponseWriter, r *http.Request) {})
		r.Post("/items", func(w http.ResponseWriter, r *http.Request) {})

		// Nested mount keeps every prefix.
		r.Route("/admin", func(r chi.Router) {
			r.Get("/users/{userID}", func(w http.ResponseWriter, r *http.Request) {})
		})
	})

	// Explicit HEAD is different from a GET-only route.
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {})
	r.Head("/healthz", func(w http.ResponseWriter, r *http.Request) {})
	r.Get("/ping", func(w http.ResponseWriter, r *http.Request) {})

	// A second API whose parameter happens to use a different name: it must
	// not be hard-merged with /v1/items/{id}.
	r.Route("/v2", func(r chi.Router) {
		r.Get("/items/{name}", func(w http.ResponseWriter, r *http.Request) {})
	})

	// A mount backed by a plain http.Handler (not a chi router): its contents
	// are unknown, so it is flagged unexpanded instead of fabricated.
	r.Mount("/files", http.FileServer(http.Dir(".")))

	summary, err := chi.SummarizeRoutes(r)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Short, stable report for humans and grep-based tooling.
	fmt.Print(summary.Report())

	// The same data is also available as JSON for API checking tools:
	//
	//	enc := json.NewEncoder(os.Stdout)
	//	enc.SetIndent("", "  ")
	//	_ = enc.Encode(summary)
}
