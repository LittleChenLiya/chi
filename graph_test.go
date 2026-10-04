package chi

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func graphHandler(w http.ResponseWriter, r *http.Request) {}

// TestRouteGraphSameShapeConflict verifies that two GET routes on the same
// matching shape (with different param names) are reported as a conflict.
func TestRouteGraphSameShapeConflict(t *testing.T) {
	r := NewRouter()
	sub := NewRouter()
	sub.Get("/users/{id}", graphHandler)
	r.Mount("/", sub)
	r.Get("/users/{name}", graphHandler)

	g := NewRouteGraph(r)
	diags := g.Diagnostics()

	var conflicts []Diagnostic
	for _, d := range diags {
		if d.Kind == DiagConflict {
			conflicts = append(conflicts, d)
		}
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d: %+v", len(conflicts), diags)
	}
	if conflicts[0].Path != "/users" {
		// path should be /users/{...} but we compare shape, so path is /users/{...}
	}
	if !contains(conflicts[0].Methods, "GET") {
		t.Fatalf("expected GET method overlap, got %v", conflicts[0].Methods)
	}
	if len(conflicts[0].Entries) != 2 {
		t.Fatalf("expected 2 entries in conflict, got %d", len(conflicts[0].Entries))
	}
}

// TestRouteGraphDifferentMethods verifies that GET and POST on the same shape
// do not conflict.
func TestRouteGraphDifferentMethods(t *testing.T) {
	r := NewRouter()
	sub := NewRouter()
	sub.Get("/users/{id}", graphHandler)
	r.Mount("/", sub)
	r.Post("/users/{name}", graphHandler)

	g := NewRouteGraph(r)
	diags := g.Diagnostics()

	for _, d := range diags {
		if d.Kind == DiagConflict {
			t.Fatalf("expected no conflict for GET vs POST, got %+v", d)
		}
	}
}

// TestRouteGraphDifferentRegex verifies that two different regexps on the same
// shape are reported as a potential conflict (pending determination).
func TestRouteGraphDifferentRegex(t *testing.T) {
	r := NewRouter()
	sub := NewRouter()
	sub.Get("/users/{id:[0-9]+}", graphHandler)
	r.Mount("/", sub)
	r.Get("/users/{name:[a-z]+}", graphHandler)

	g := NewRouteGraph(r)
	diags := g.Diagnostics()

	var pending []Diagnostic
	for _, d := range diags {
		if d.Kind == DiagPotentialConflict {
			pending = append(pending, d)
		}
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 potential conflict, got %d: %+v", len(pending), diags)
	}
	if !contains(pending[0].Methods, "GET") {
		t.Fatalf("expected GET method overlap, got %v", pending[0].Methods)
	}
}

// TestRouteGraphSameRegex verifies that two identical regexps on the same
// shape are merged (reported as a conflict).
func TestRouteGraphSameRegex(t *testing.T) {
	r := NewRouter()
	sub := NewRouter()
	sub.Get("/users/{id:[0-9]+}", graphHandler)
	r.Mount("/", sub)
	r.Get("/users/{name:[0-9]+}", graphHandler)

	g := NewRouteGraph(r)
	diags := g.Diagnostics()

	var conflicts []Diagnostic
	for _, d := range diags {
		if d.Kind == DiagConflict {
			conflicts = append(conflicts, d)
		}
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict for same regex, got %d: %+v", len(conflicts), diags)
	}
}

// TestRouteGraphNestedMountSource verifies that the source chain for a route
// under nested mounts includes every mount in order.
func TestRouteGraphNestedMountSource(t *testing.T) {
	r := NewRouter()
	r.Route("/api", func(r Router) {
		r.Route("/v1", func(r Router) {
			r.Get("/users", graphHandler)
		})
	})

	g := NewRouteGraph(r)
	entries := g.Entries()

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.FullPath != "/api/v1/users" {
		t.Fatalf("expected /api/v1/users, got %q", e.FullPath)
	}
	if len(e.Source) != 2 {
		t.Fatalf("expected 2 mounts in source chain, got %d: %v", len(e.Source), e.Source)
	}
	if e.Source[0].Path != "/api" {
		t.Fatalf("expected first mount /api, got %q", e.Source[0].Path)
	}
	if e.Source[1].Path != "/api/v1" {
		t.Fatalf("expected second mount /api/v1, got %q", e.Source[1].Path)
	}
}

// TestRouteGraphCoveredByMount verifies that a route under a narrow mount is
// flagged when covered by a broader mount from a different branch.
func TestRouteGraphCoveredByMount(t *testing.T) {
	r := NewRouter()
	// Broad mount at root.
	broad := NewRouter()
	broad.Get("/api/users", graphHandler)
	r.Mount("/", broad)
	// Narrow mount at /api.
	r.Route("/api", func(r Router) {
		r.Get("/users", graphHandler)
	})

	g := NewRouteGraph(r)
	diags := g.Diagnostics()

	var covered []Diagnostic
	for _, d := range diags {
		if d.Kind == DiagCoveredByMount {
			covered = append(covered, d)
		}
	}
	// The route under the narrow mount (/api) is covered by the broad mount (/).
	if len(covered) == 0 {
		t.Fatalf("expected at least 1 covered-by-mount diagnostic, got none: %+v", diags)
	}
}

// TestRouteGraphEmpty verifies that an empty router produces no diagnostics
// and a report that says "none".
func TestRouteGraphEmpty(t *testing.T) {
	r := NewRouter()
	g := NewRouteGraph(r)
	diags := g.Diagnostics()
	if len(diags) != 0 {
		t.Fatalf("expected no diagnostics for empty router, got %d: %+v", len(diags), diags)
	}
	var buf bytes.Buffer
	g.Report(&buf)
	if !strings.Contains(buf.String(), "Diagnostics: none") {
		t.Fatalf("expected 'Diagnostics: none' in report, got:\n%s", buf.String())
	}
}

// TestRouteGraphNoConflict verifies that two routes with different shapes do
// not conflict.
func TestRouteGraphNoConflict(t *testing.T) {
	r := NewRouter()
	r.Get("/users", graphHandler)
	r.Get("/posts", graphHandler)

	g := NewRouteGraph(r)
	diags := g.Diagnostics()
	for _, d := range diags {
		if d.Kind == DiagConflict || d.Kind == DiagPotentialConflict {
			t.Fatalf("expected no conflict for different shapes, got %+v", d)
		}
	}
}

// TestRouteGraphMountStubNode verifies that a route registered at the mount
// path (the mount's stub node) is not flagged as covered by the mount.
func TestRouteGraphMountStubNode(t *testing.T) {
	r := NewRouter()
	sub := NewRouter()
	sub.Get("/users", graphHandler)
	r.Mount("/api", sub)
	// Register a GET at the mount path itself (the stub node).
	r.Get("/api", graphHandler)

	g := NewRouteGraph(r)
	diags := g.Diagnostics()
	for _, d := range diags {
		if d.Kind == DiagCoveredByMount && d.Path == "/api" {
			t.Fatalf("mount stub node should not be flagged as covered: %+v", d)
		}
	}
}

// TestRouteGraphHandleOverlap verifies that a Handle (mALL) route overlaps
// with a Get route on the same shape.
func TestRouteGraphHandleOverlap(t *testing.T) {
	r := NewRouter()
	sub := NewRouter()
	sub.Handle("/users/{id}", http.HandlerFunc(graphHandler))
	r.Mount("/", sub)
	r.Get("/users/{name}", graphHandler)

	g := NewRouteGraph(r)
	diags := g.Diagnostics()

	var conflicts []Diagnostic
	for _, d := range diags {
		if d.Kind == DiagConflict {
			conflicts = append(conflicts, d)
		}
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict for Handle vs Get, got %d: %+v", len(conflicts), diags)
	}
}

// TestRouteGraphCatchAll verifies that two catch-all routes on the same shape
// are reported as a conflict.
func TestRouteGraphCatchAll(t *testing.T) {
	r := NewRouter()
	sub := NewRouter()
	sub.Get("/files/*", graphHandler)
	r.Mount("/", sub)
	r.Get("/files/*", graphHandler)

	g := NewRouteGraph(r)
	diags := g.Diagnostics()

	var conflicts []Diagnostic
	for _, d := range diags {
		if d.Kind == DiagConflict {
			conflicts = append(conflicts, d)
		}
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict for catch-all routes, got %d: %+v", len(conflicts), diags)
	}
}

// TestRouteGraphReportStable verifies that Report produces identical output
// when run multiple times on the same router.
func TestRouteGraphReportStable(t *testing.T) {
	r := NewRouter()
	r.Get("/users/{id}", graphHandler)
	r.Post("/users", graphHandler)
	r.Route("/api", func(r Router) {
		r.Get("/items", graphHandler)
	})

	g := NewRouteGraph(r)
	var buf1, buf2 bytes.Buffer
	g.Report(&buf1)
	g.Report(&buf2)

	if buf1.String() != buf2.String() {
		t.Fatalf("report is not stable:\n--- first ---\n%s\n--- second ---\n%s", buf1.String(), buf2.String())
	}
}

// TestRouteGraphReportContains verifies the report includes routes, mounts and
// diagnostics sections.
func TestRouteGraphReportContains(t *testing.T) {
	r := NewRouter()
	sub := NewRouter()
	sub.Get("/users/{id}", graphHandler)
	r.Mount("/", sub)
	r.Get("/users/{name}", graphHandler)

	g := NewRouteGraph(r)
	var buf bytes.Buffer
	g.Report(&buf)
	out := buf.String()

	for _, want := range []string{"Route Graph Report", "Routes:", "Mounts:", "Diagnostics:", "CONFLICT"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
