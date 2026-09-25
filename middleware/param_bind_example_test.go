package middleware_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// ExampleBindParams binds typed URL parameters for a route and serves
// it over a local HTTP server. Each parser runs once per request; the
// handler can read the bound values as many times as it needs.
func ExampleBindParams() {
	r := chi.NewRouter()
	r.With(middleware.BindParams(
		middleware.Param("id", middleware.ParseInt),
		middleware.Param("item", middleware.ParseUUID),
	)).Get("/users/{id}/items/{item}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := middleware.ParamValue[int](r.Context(), "id")
		item, _ := middleware.ParamValue[string](r.Context(), "item")
		fmt.Fprintf(w, "user=%d item=%s", id, item)
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	resp, _ := http.Get(ts.URL + "/users/42/items/123e4567-e89b-12d3-a456-426614174000")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("%d %s\n", resp.StatusCode, body)

	resp, _ = http.Get(ts.URL + "/users/oops/items/123e4567-e89b-12d3-a456-426614174000")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("%d %s", resp.StatusCode, body)

	// Output:
	// 200 user=42 item=123e4567-e89b-12d3-a456-426614174000
	// 400 invalid URL parameter "id"
}
