// Command demo prints a read-only method summary of a small chi router,
// demonstrating the apisummary package. No HTTP requests are sent and no
// route handlers are executed.
package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/apisummary"
	"github.com/go-chi/chi/v5/middleware"
)

func main() {
	// calls counts handler invocations; it must stay 0.
	var calls int
	count := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			calls++
			h(w, r)
		}
	}

	r := chi.NewRouter()
	// GetHead serves HEAD requests by routing them to the GET handler,
	// but HEAD is never explicitly registered.
	r.Use(middleware.GetHead)

	// Explicit HEAD + GET on the same path: both methods must appear.
	r.Head("/ping", count(func(w http.ResponseWriter, r *http.Request) {}))
	r.Get("/ping", count(func(w http.ResponseWriter, r *http.Request) {}))

	// GET-only: must report GET only, even though GetHead serves HEAD.
	r.Get("/only-get", count(func(w http.ResponseWriter, r *http.Request) {}))

	// /v1 mount with parameter segments and a nested mount.
	v1 := chi.NewRouter()
	v1.Get("/items/{id}", count(func(w http.ResponseWriter, r *http.Request) {}))
	v1.Post("/items", count(func(w http.ResponseWriter, r *http.Request) {}))
	// Different parameter name: must stay a separate template.
	v1.Get("/items/{itemID}/detail", count(func(w http.ResponseWriter, r *http.Request) {}))

	admin := chi.NewRouter()
	admin.Get("/users", count(func(w http.ResponseWriter, r *http.Request) {}))
	v1.Mount("/admin", admin)

	r.Mount("/v1", v1)

	// A mounted handler that is not a chi.Router cannot be expanded.
	r.Mount("/static", http.HandlerFunc(http.NotFound))

	sum := apisummary.Generate(r)
	sum.WriteTo(os.Stdout)

	fmt.Println()
	fmt.Printf("handlers executed while generating summary: %d\n", calls)
	if calls != 0 {
		fmt.Println("ERROR: summary generation invoked handlers")
		os.Exit(1)
	}
}
