package middleware_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Example_bodyBudget shows how to compose BodyBudget layers so that a
// sub-route enforces a smaller budget than the router-wide one. The effective
// budget is the smallest limit among all layers wrapping a route: the outer
// 8-byte layer admits an 8-byte body, but the inner 3-byte layer rejects a
// 4-byte one with 413. A handler that already wrote a response keeps it; no
// error page is appended.
func Example_bodyBudget() {
	r := chi.NewRouter()

	// Router-wide budget of 8 bytes.
	r.Use(middleware.BodyBudget(8))

	r.Post("/", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			// Read error: return without writing. The middleware turns the
			// budget error into a 413 because no response has started.
			return
		}
		fmt.Fprintf(w, "got %d bytes", len(body))
	})

	// A sub-route with a tighter 3-byte budget.
	r.With(middleware.BodyBudget(3)).Post("/tight", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			// The context error query tells the handler why the read failed.
			if err := middleware.BodyBudgetErr(r.Context()); err != nil {
				return // middleware will produce the 413
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, "got %d bytes", len(body))
	})

	serve := func(path, body string) (int, string) {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	fmt.Println(serve("/", "123"))       // 3 bytes, under both limits
	fmt.Println(serve("/tight", "123"))  // 3 bytes, under the inner limit
	fmt.Println(serve("/tight", "1234")) // 4 bytes, over the inner limit
	// Output:
	// 200 got 3 bytes
	// 200 got 3 bytes
	// 413 Request Entity Too Large
}
