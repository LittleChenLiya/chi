package chi_test

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Inspect a router before startup and print the route-graph report. The
// inspection only reads the routing tree; no HTTP requests are sent and the
// report is deterministic, so it can be diffed in tests or CI.
func ExampleInspectRoutes() {
	h := func(w http.ResponseWriter, r *http.Request) {}

	r := chi.NewRouter()

	// Same match shape mounted separately, with different placeholder names.
	users := chi.NewRouter()
	users.Get("/{id}", h)
	r.Mount("/users", users)
	r.Get("/users/{name}", h)

	// Regex placeholders whose equivalence cannot be determined statically.
	r.Get(`/files/{name:\d+}`, h)
	r.Get(`/files/{name:[a-z]+}`, h)

	// A duplicate registration reached through nested mounts.
	admin := chi.NewRouter()
	admin.Get("/{id}", h)
	api := chi.NewRouter()
	api.Mount("/admin", admin)
	r.Mount("/api", api)
	r.Get("/api/admin/{id}", h)

	fmt.Print(chi.FormatReport(chi.InspectRoutes(r)))
	// Output:
	// route graph: 3 diagnostic(s)
	//
	// [duplicate-method] GET /api/admin/{id}
	//   2 registrations resolve to the same method and path; only one of them can serve a request
	//   - GET /api/admin/{id} (mounted at: (root))
	//   - GET /api/admin/{id} (mounted at: /api -> /admin)
	//
	// [shape-conflict] GET /users/{}
	//   patterns have the same match shape; a request matching one also matches the other, so only one registration can serve it
	//   - GET /users/{name} (mounted at: (root))
	//   - GET /users/{id} (mounted at: /users)
	//
	// [regex-pending] GET /files/{}
	//   placeholder regexes differ and could not be proven equivalent; potential overlap is undetermined
	//   - GET /files/{name:\d+} (mounted at: (root))
	//   - GET /files/{name:[a-z]+} (mounted at: (root))
}
