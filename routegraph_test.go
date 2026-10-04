package chi

import (
	"net/http"
	"strings"
	"testing"
)

func graphHandler(w http.ResponseWriter, r *http.Request) {}

func diagKinds(diags []Diagnostic) []DiagKind {
	kinds := make([]DiagKind, 0, len(diags))
	for _, d := range diags {
		kinds = append(kinds, d.Kind)
	}
	return kinds
}

func TestInspectShapeConflict(t *testing.T) {
	// GET /users/{id} and GET /users/{name} mounted separately: the same
	// match shape with different placeholder names must be reported.
	r := NewRouter()
	users := NewRouter()
	users.Get("/{id}", graphHandler)
	r.Mount("/users", users)
	r.Get("/users/{name}", graphHandler)

	diags := InspectRoutes(r)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d: %v", len(diags), diagKinds(diags))
	}
	d := diags[0]
	if d.Kind != DiagShapeConflict {
		t.Fatalf("expected shape-conflict, got %v", d.Kind)
	}
	if d.Method != "GET" || d.Shape != "/users/{}" {
		t.Fatalf("unexpected method/shape: %s %s", d.Method, d.Shape)
	}
	if len(d.Routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(d.Routes))
	}
	// Routes are listed in graph traversal (registration) order.
	if d.Routes[0].Pattern != "/users/{name}" || d.Routes[1].Pattern != "/users/{id}" {
		t.Fatalf("unexpected routes: %+v", d.Routes)
	}
	if len(d.Routes[0].Mounts) != 0 {
		t.Fatalf("expected root registration first, got %+v", d.Routes[0])
	}
	if len(d.Routes[1].Mounts) != 1 || d.Routes[1].Mounts[0] != "/users" {
		t.Fatalf("expected mounted registration second, got %+v", d.Routes[1])
	}
}

func TestInspectNoConflictAcrossMethods(t *testing.T) {
	r := NewRouter()
	r.Get("/users/{id}", graphHandler)
	r.Post("/users/{name}", graphHandler)
	if diags := InspectRoutes(r); len(diags) != 0 {
		t.Fatalf("expected no diagnostics across methods, got %v", diagKinds(diags))
	}

	// The symmetric setup yields one diagnostic per method, never mixed.
	r2 := NewRouter()
	users := NewRouter()
	users.Get("/{id}", graphHandler)
	users.Post("/{id}", graphHandler)
	r2.Mount("/users", users)
	r2.Get("/users/{name}", graphHandler)
	r2.Post("/users/{name}", graphHandler)
	diags := InspectRoutes(r2)
	if len(diags) != 2 {
		t.Fatalf("expected 2 diagnostics, got %v", diagKinds(diags))
	}
	for _, d := range diags {
		if d.Kind != DiagShapeConflict {
			t.Fatalf("expected shape-conflict, got %v", d.Kind)
		}
		for _, rt := range d.Routes {
			if rt.Method != d.Method {
				t.Fatalf("mixed methods in diagnostic: %+v", d)
			}
		}
	}
}

func TestInspectRegexPending(t *testing.T) {
	r := NewRouter()
	r.Get(`/users/{id:\d+}`, graphHandler)
	r.Get(`/users/{id:[a-z]+}`, graphHandler)

	diags := InspectRoutes(r)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %v", diagKinds(diags))
	}
	if diags[0].Kind != DiagRegexPending {
		t.Fatalf("expected regex-pending, got %v", diags[0].Kind)
	}
	if len(diags[0].Routes) != 2 {
		t.Fatalf("expected 2 routes, got %+v", diags[0].Routes)
	}
}

func TestInspectEquivalentRegexMerged(t *testing.T) {
	r := NewRouter()
	users := NewRouter()
	users.Get(`/{id:\d+}`, graphHandler)
	r.Mount("/users", users)
	r.Get(`/users/{name:\d+}`, graphHandler)

	diags := InspectRoutes(r)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %v", diagKinds(diags))
	}
	if diags[0].Kind != DiagShapeConflict {
		t.Fatalf("expected shape-conflict for provably equivalent regexes, got %v", diags[0].Kind)
	}
}

func TestInspectPlainVsRegexPending(t *testing.T) {
	r := NewRouter()
	r.Get("/users/{id}", graphHandler)
	r.Get(`/users/{name:\d+}`, graphHandler)

	diags := InspectRoutes(r)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %v", diagKinds(diags))
	}
	if diags[0].Kind != DiagRegexPending {
		t.Fatalf("expected regex-pending for plain vs regex placeholder, got %v", diags[0].Kind)
	}
}

func TestInspectDuplicateMethodViaMount(t *testing.T) {
	r := NewRouter()
	sub := NewRouter()
	sub.Get("/{id}", graphHandler)
	r.Mount("/users", sub)
	r.Get("/users/{id}", graphHandler)

	diags := InspectRoutes(r)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %v", diagKinds(diags))
	}
	d := diags[0]
	if d.Kind != DiagDuplicateMethod {
		t.Fatalf("expected duplicate-method, got %v", d.Kind)
	}
	if d.Shape != "/users/{id}" {
		t.Fatalf("unexpected shape %q", d.Shape)
	}
	if len(d.Routes) != 2 {
		t.Fatalf("expected 2 routes, got %+v", d.Routes)
	}
	var mounted, root int
	for _, rt := range d.Routes {
		if len(rt.Mounts) == 1 && rt.Mounts[0] == "/users" {
			mounted++
		}
		if len(rt.Mounts) == 0 {
			root++
		}
	}
	if mounted != 1 || root != 1 {
		t.Fatalf("expected one mounted and one root route, got %+v", d.Routes)
	}
}

func TestInspectNestedMountChain(t *testing.T) {
	r := NewRouter()
	admin := NewRouter()
	admin.Get("/{id}", graphHandler)
	api := NewRouter()
	api.Mount("/admin", admin)
	r.Mount("/api", api)
	r.Get("/api/admin/{id}", graphHandler)

	diags := InspectRoutes(r)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %v", diagKinds(diags))
	}
	d := diags[0]
	if d.Kind != DiagDuplicateMethod {
		t.Fatalf("expected duplicate-method, got %v", d.Kind)
	}
	var found bool
	for _, rt := range d.Routes {
		if len(rt.Mounts) == 2 && rt.Mounts[0] == "/api" && rt.Mounts[1] == "/admin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected mount chain /api -> /admin, got %+v", d.Routes)
	}

	report := FormatReport(diags)
	if !strings.Contains(report, "/api -> /admin") {
		t.Fatalf("report missing mount chain:\n%s", report)
	}
	if !strings.Contains(report, "(root)") {
		t.Fatalf("report missing root marker:\n%s", report)
	}
}

func TestInspectShadowedByMount(t *testing.T) {
	r := NewRouter()
	sub := NewRouter()
	sub.Get("/{tab}", graphHandler)
	r.Mount("/users/{id}", sub)
	r.Get("/users/{id}/posts", graphHandler)

	diags := InspectRoutes(r)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %v", diagKinds(diags))
	}
	d := diags[0]
	if d.Kind != DiagShadowedByMount {
		t.Fatalf("expected shadowed-by-mount, got %v", d.Kind)
	}
	if d.Shape != "/users/{id}/{tab}" {
		t.Fatalf("unexpected shadowed shape %q", d.Shape)
	}
	if len(d.Routes) != 2 {
		t.Fatalf("expected 2 routes, got %+v", d.Routes)
	}
	shadowed, cover := d.Routes[0], d.Routes[1]
	if len(shadowed.Mounts) != 1 || shadowed.Mounts[0] != "/users/{id}" {
		t.Fatalf("expected shadowed route under /users/{id}, got %+v", shadowed)
	}
	if len(cover.Mounts) != 0 || cover.Pattern != "/users/{id}/posts" {
		t.Fatalf("expected covering route at root, got %+v", cover)
	}
}

func TestInspectCleanRouter(t *testing.T) {
	r := NewRouter()
	r.Get("/", graphHandler)
	r.Get("/users/{id}", graphHandler)
	r.Post("/users/{id}", graphHandler)
	r.Get("/users/{id}/posts", graphHandler)
	r.Get("/health", graphHandler)

	if diags := InspectRoutes(r); len(diags) != 0 {
		t.Fatalf("expected no diagnostics, got %v", diagKinds(diags))
	}
}

func TestInspectStableOrder(t *testing.T) {
	build := func() *Mux {
		r := NewRouter()
		z := NewRouter()
		z.Get("/{a}", graphHandler)
		r.Mount("/z", z)
		r.Get("/z/{b}", graphHandler)
		r.Get(`/a/{x:\d+}`, graphHandler)
		r.Get(`/a/{y:[a-z]+}`, graphHandler)
		sub := NewRouter()
		sub.Get("/{id}", graphHandler)
		r.Mount("/m", sub)
		r.Get("/m/{id}", graphHandler)
		return r
	}

	first := FormatReport(InspectRoutes(build()))
	for i := 0; i < 5; i++ {
		if got := FormatReport(InspectRoutes(build())); got != first {
			t.Fatalf("report not stable across runs:\nfirst:\n%s\ngot:\n%s", first, got)
		}
	}

	diags := InspectRoutes(build())
	if len(diags) != 3 {
		t.Fatalf("expected 3 diagnostics, got %v", diagKinds(diags))
	}
	// Stable kind order: duplicate-method < shape-conflict < regex-pending.
	want := []DiagKind{DiagDuplicateMethod, DiagShapeConflict, DiagRegexPending}
	for i, d := range diags {
		if d.Kind != want[i] {
			t.Fatalf("diagnostic %d: expected %v, got %v", i, want[i], d.Kind)
		}
	}
}

func TestInspectDoesNotChangeMatching(t *testing.T) {
	r := NewRouter()
	users := NewRouter()
	users.Get("/{id}", graphHandler)
	r.Mount("/users", users)
	r.Get("/users/{name}", graphHandler)

	before := make([]string, 0)
	for _, rt := range r.Routes() {
		before = append(before, rt.Pattern)
	}
	_ = InspectRoutes(r)
	after := make([]string, 0)
	for _, rt := range r.Routes() {
		after = append(after, rt.Pattern)
	}
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatalf("route graph changed: %v -> %v", before, after)
	}

	rctx := NewRouteContext()
	if !r.Match(rctx, "GET", "/users/123") {
		t.Fatal("router no longer matches /users/123 after inspection")
	}
}
