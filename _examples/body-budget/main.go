// This example demonstrates the composable BodyBudget middleware, which
// limits request body size per sub-route without changing any global server
// setting.
//
//   - The whole router carries an outer budget of 8 bytes.
//   - The /small sub-route adds a tighter budget of 3 bytes; nested layers
//     take the minimum.
//   - Only bytes the handler actually reads are charged (the body is never
//     pre-read).
//   - Unknown Content-Length (chunked) requests cannot bypass the budget.
//   - An oversized body yields 413 when the response has not started; if the
//     handler already wrote a response, no error page is appended.
//
// Run with: go run ./_examples/body-budget
package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func main() {
	r := chi.NewRouter()

	// Site-wide outer budget of 8 bytes.
	r.Use(middleware.BodyBudget(8))

	// Sub-route tightens the budget to 3 bytes.
	r.Route("/small", func(r chi.Router) {
		r.Use(middleware.BodyBudget(3))
		r.Post("/", func(w http.ResponseWriter, r *http.Request) {
			n, err := io.Copy(io.Discard, r.Body)
			if err != nil {
				if errors.Is(err, middleware.ErrBodyBudgetExceeded) {
					// Recognizable error; returning here leaves the 413 to the
					// middleware because no response has been written.
					fmt.Printf("  reader got recognizable budget error after %d bytes\n", n)
				}
				return
			}
			fmt.Fprintf(w, "accepted %d bytes", n)
		})
	})

	// Handler that writes first and only then trips the budget: the middleware
	// must not append an error body.
	r.Post("/early", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, "already-started")
		_, _ = io.Copy(io.Discard, r.Body) // may exceed, but response is done
	})

	// Route with no additional middleware: still bounded by the outer 8.
	r.Post("/outer", func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		fmt.Printf("POST %-7s body=%-10q -> %d %q\n", path, body, rec.Code, rec.Body.String())
		return rec
	}

	fmt.Println("outer limit 8, /small limit 3:")
	post("/small", "abc")         // 3 bytes: success
	post("/small", "abcd")        // 4 bytes: rejected only here (outer allows 8)
	post("/outer", "12345678")    // 8 bytes: within outer budget
	post("/outer", "123456789")   // 9 bytes: outer budget rejects
	rec := post("/early", "abcd") // handler already wrote 202; no appended page
	if rec.Body.String() != "already-started" {
		panic("unexpected second body appended!")
	}
	fmt.Println("no second error body appended on /early: OK")
}
