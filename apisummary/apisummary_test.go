package apisummary

import (
	"bytes"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// countingHandler returns a handler that records how many times it was
// invoked, so tests can prove summary generation executes nothing.
func countingHandler(calls *int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*calls++
	}
}

func methodsOfEndpoint(t *testing.T, s *Summary, pattern string) []string {
	t.Helper()
	for _, e := range s.Endpoints {
		if e.Pattern == pattern {
			return e.Methods
		}
	}
	t.Fatalf("no endpoint with pattern %q in summary: %s", pattern, s)
	return nil
}

func hasEndpoint(s *Summary, pattern string) bool {
	for _, e := range s.Endpoints {
		if e.Pattern == pattern {
			return true
		}
	}
	return false
}

func equalMethods(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestGenerate_MergesMethodsPerTemplate(t *testing.T) {
	r := chi.NewRouter()
	var calls int
	r.Get("/items/{id}", countingHandler(&calls))
	r.Post("/items", countingHandler(&calls))

	s := Generate(r)

	if got := methodsOfEndpoint(t, s, "/items/{id}"); !equalMethods(got, []string{"GET"}) {
		t.Fatalf("GET /items/{id}: got %v", got)
	}
	if got := methodsOfEndpoint(t, s, "/items"); !equalMethods(got, []string{"POST"}) {
		t.Fatalf("POST /items: got %v", got)
	}
	if calls != 0 {
		t.Fatalf("handlers were invoked %d times while generating the summary", calls)
	}
}

func TestGenerate_MountPrefixPreserved(t *testing.T) {
	r := chi.NewRouter()
	v1 := chi.NewRouter()
	var calls int
	v1.Get("/items/{id}", countingHandler(&calls))
	v1.Post("/items", countingHandler(&calls))
	r.Mount("/v1", v1)

	s := Generate(r)

	if got := methodsOfEndpoint(t, s, "/v1/items/{id}"); !equalMethods(got, []string{"GET"}) {
		t.Fatalf("GET /v1/items/{id}: got %v", got)
	}
	if got := methodsOfEndpoint(t, s, "/v1/items"); !equalMethods(got, []string{"POST"}) {
		t.Fatalf("POST /v1/items: got %v", got)
	}
	if calls != 0 {
		t.Fatalf("handlers were invoked %d times while generating the summary", calls)
	}
}

func TestGenerate_DifferentParamNamesStaySeparate(t *testing.T) {
	r := chi.NewRouter()
	var calls int
	// Same static prefix, different parameter names: the templates must
	// not be hard-merged into one endpoint.
	r.Get("/items/{id}", countingHandler(&calls))
	r.Get("/items/{itemID}/detail", countingHandler(&calls))
	// Regexp parameter with a different name on the same prefix.
	r.Get("/items/{code:[a-z]+}", countingHandler(&calls))

	s := Generate(r)

	for _, p := range []string{"/items/{id}", "/items/{itemID}/detail", "/items/{code:[a-z]+}"} {
		if !hasEndpoint(s, p) {
			t.Fatalf("expected separate endpoint for template %q, summary: %s", p, s)
		}
	}
	if len(s.Endpoints) != 3 {
		t.Fatalf("expected 3 distinct endpoints, got %d: %s", len(s.Endpoints), s)
	}
	if calls != 0 {
		t.Fatalf("handlers were invoked %d times while generating the summary", calls)
	}
}

func TestGenerate_ExplicitHeadVsGetOnly(t *testing.T) {
	r := chi.NewRouter()
	var calls int
	// Explicit HEAD registration: both methods must appear.
	r.Head("/ping", countingHandler(&calls))
	r.Get("/ping", countingHandler(&calls))
	// GET only: HEAD must NOT be invented.
	r.Get("/only-get", countingHandler(&calls))

	s := Generate(r)

	if got := methodsOfEndpoint(t, s, "/ping"); !equalMethods(got, []string{"GET", "HEAD"}) {
		t.Fatalf("explicit HEAD + GET /ping: got %v, want [GET HEAD]", got)
	}
	if got := methodsOfEndpoint(t, s, "/only-get"); !equalMethods(got, []string{"GET"}) {
		t.Fatalf("GET-only /only-get: got %v, want [GET] (HEAD must not be invented)", got)
	}
	if calls != 0 {
		t.Fatalf("handlers were invoked %d times while generating the summary", calls)
	}
}

func TestGenerate_GetHeadMiddlewareDoesNotRegisterHead(t *testing.T) {
	// middleware.GetHead serves HEAD requests by routing them to the GET
	// handler, but HEAD is not explicitly registered on the tree. The
	// summary must not record it.
	r := chi.NewRouter()
	r.Use(middleware.GetHead)
	var calls int
	r.Get("/only-get", countingHandler(&calls))

	s := Generate(r)

	if got := methodsOfEndpoint(t, s, "/only-get"); !equalMethods(got, []string{"GET"}) {
		t.Fatalf("GET-only route behind GetHead: got %v, want [GET]", got)
	}
	if calls != 0 {
		t.Fatalf("handlers were invoked %d times while generating the summary", calls)
	}
}

func TestGenerate_UnexpandedMountMarked(t *testing.T) {
	r := chi.NewRouter()
	// Mounted handler that is not a chi.Router: cannot be expanded.
	r.Mount("/static", http.HandlerFunc(http.NotFound))
	// Catch-all Handle registration: cannot be expanded either.
	r.Handle("/catch/*", http.HandlerFunc(http.NotFound))

	s := Generate(r)

	var unexpanded []string
	for _, e := range s.Endpoints {
		if e.Unexpanded {
			unexpanded = append(unexpanded, e.Pattern)
			if !equalMethods(e.Methods, []string{"*"}) {
				t.Fatalf("unexpanded %q: methods = %v, want [*]", e.Pattern, e.Methods)
			}
		}
	}
	sort.Strings(unexpanded)
	want := []string{"/catch/*", "/static/*"}
	if !equalMethods(unexpanded, want) {
		t.Fatalf("unexpanded mounts: got %v, want %v", unexpanded, want)
	}
}

func TestGenerate_NestedMounts(t *testing.T) {
	r := chi.NewRouter()
	api := chi.NewRouter()
	v1 := chi.NewRouter()
	var calls int
	v1.Get("/items/{id}", countingHandler(&calls))
	v1.Post("/items", countingHandler(&calls))
	api.Mount("/v1", v1)
	r.Mount("/api", api)

	s := Generate(r)

	if got := methodsOfEndpoint(t, s, "/api/v1/items/{id}"); !equalMethods(got, []string{"GET"}) {
		t.Fatalf("GET /api/v1/items/{id}: got %v", got)
	}
	if got := methodsOfEndpoint(t, s, "/api/v1/items"); !equalMethods(got, []string{"POST"}) {
		t.Fatalf("POST /api/v1/items: got %v", got)
	}
	if calls != 0 {
		t.Fatalf("handlers were invoked %d times while generating the summary", calls)
	}
}

func TestGenerate_HandleAllMethods(t *testing.T) {
	r := chi.NewRouter()
	var calls int
	r.Handle("/all", countingHandler(&calls))

	s := Generate(r)

	if got := methodsOfEndpoint(t, s, "/all"); !equalMethods(got, []string{"*"}) {
		t.Fatalf("Handle /all: got %v, want [*]", got)
	}
	if calls != 0 {
		t.Fatalf("handlers were invoked %d times while generating the summary", calls)
	}
}

func TestGenerate_StableOutput(t *testing.T) {
	r := chi.NewRouter()
	r.Get("/z/{id}", func(w http.ResponseWriter, r *http.Request) {})
	r.Post("/a", func(w http.ResponseWriter, r *http.Request) {})
	r.Mount("/m", chi.NewRouter())

	s1 := Generate(r).String()
	s2 := Generate(r).String()
	if s1 != s2 {
		t.Fatalf("summary output is not stable:\n--- first ---\n%s\n--- second ---\n%s", s1, s2)
	}
}

func TestGenerate_EmptyRouter(t *testing.T) {
	r := chi.NewRouter()
	s := Generate(r)
	if len(s.Endpoints) != 0 {
		t.Fatalf("empty router: got %d endpoints, want 0", len(s.Endpoints))
	}
	if !strings.Contains(s.String(), "(no routes registered)") {
		t.Fatalf("empty router report should note no routes, got: %q", s.String())
	}
}

func TestGenerate_DoesNotMutateRoutes(t *testing.T) {
	r := chi.NewRouter()
	sub := chi.NewRouter()
	sub.Get("/items/{id}", func(w http.ResponseWriter, r *http.Request) {})
	r.Mount("/v1", sub)

	snapshot := func() string {
		var buf bytes.Buffer
		chi.Walk(r, func(method, route string, handler http.Handler, middlewares ...func(http.Handler) http.Handler) error {
			buf.WriteString(method + " " + route + "\n")
			return nil
		})
		return buf.String()
	}

	before := snapshot()
	Generate(r)
	after := snapshot()
	if before != after {
		t.Fatalf("generating the summary changed the routes:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

func TestReport_ContainsEndpoints(t *testing.T) {
	r := chi.NewRouter()
	r.Get("/items/{id}", func(w http.ResponseWriter, r *http.Request) {})
	r.Mount("/static", http.HandlerFunc(http.NotFound))

	report := Generate(r).String()
	for _, want := range []string{"GET", "/items/{id}", "/static/*", "unexpanded"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
}
