package chi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// countingHandler is an http.Handler that records how many times it actually
// served a request. Tests assert it stays at zero after building a summary,
// proving summarization never executes handlers.
type countingHandler struct{ n atomic.Int64 }

func (c *countingHandler) ServeHTTP(http.ResponseWriter, *http.Request) { c.n.Add(1) }

func (c *countingHandler) calls() int64 { return c.n.Load() }

func noopHandler(w http.ResponseWriter, r *http.Request) {}

// findEndpoint returns the RouteEndpoint with the given pattern, or nil.
func findEndpoint(t *testing.T, s *RouteSummary, pattern string) *RouteEndpoint {
	t.Helper()
	for i := range s.Endpoints {
		if s.Endpoints[i].Pattern == pattern {
			return &s.Endpoints[i]
		}
	}
	return nil
}

func requireEndpoint(t *testing.T, s *RouteSummary, pattern string, methods []string) {
	t.Helper()
	ep := findEndpoint(t, s, pattern)
	if ep == nil {
		t.Fatalf("expected endpoint %q in summary, got: %+v", pattern, s.Endpoints)
	}
	if !reflect.DeepEqual(ep.Methods, methods) {
		t.Fatalf("endpoint %q methods = %v, want %v", pattern, ep.Methods, methods)
	}
}

// TestSummarizeRoutesAcceptance covers the stated acceptance scenario: GET
// /items/{id} and POST /items mounted under /v1 must keep their full prefix
// and actual methods.
func TestSummarizeRoutesAcceptance(t *testing.T) {
	r := NewRouter()
	r.Route("/v1", func(r Router) {
		r.Get("/items/{id}", noopHandler)
		r.Post("/items", noopHandler)
	})

	s, err := SummarizeRoutes(r)
	if err != nil {
		t.Fatal(err)
	}

	requireEndpoint(t, s, "/v1/items/{id}", []string{http.MethodGet})
	requireEndpoint(t, s, "/v1/items", []string{http.MethodPost})
	if len(s.UnexpandedMounts) != 0 {
		t.Fatalf("expected no unexpanded mounts, got %+v", s.UnexpandedMounts)
	}
}

// TestSummarizeRoutesKeepsDifferentParamNamesApart ensures templates that
// differ only in a parameter name are never hard-merged, whether they live in
// the same router family or in separate mounted routers.
func TestSummarizeRoutesKeepsDifferentParamNamesApart(t *testing.T) {
	t.Run("different mount prefixes", func(t *testing.T) {
		r := NewRouter()
		r.Route("/v1", func(r Router) {
			r.Get("/items/{id}", noopHandler)
		})
		r.Route("/v2", func(r Router) {
			r.Get("/items/{name}", noopHandler)
		})

		s, err := SummarizeRoutes(r)
		if err != nil {
			t.Fatal(err)
		}
		requireEndpoint(t, s, "/v1/items/{id}", []string{http.MethodGet})
		requireEndpoint(t, s, "/v2/items/{name}", []string{http.MethodGet})
	})

	t.Run("merge keeps full templates as keys", func(t *testing.T) {
		// mergeEndpoints is the same keying the traversal uses: exact full
		// templates, so a different param name yields a different entry.
		merged := map[string]map[string]struct{}{}
		addMethods(merged, "/x/{id}", []string{http.MethodGet})
		addMethods(merged, "/x/{id}", []string{http.MethodHead})
		addMethods(merged, "/x/{name}", []string{http.MethodGet})

		if _, ok := merged["/x/{id}"]; !ok {
			t.Fatalf("/x/{id} lost during merge")
		}
		if _, ok := merged["/x/{name}"]; !ok {
			t.Fatalf("/x/{name} must not merge into /x/{id}")
		}
		if len(merged) != 2 {
			t.Fatalf("expected 2 distinct templates, got %d: %v", len(merged), merged)
		}
	})
}

// TestSummarizeRoutesMergesMethodsOnSameTemplate ensures methods registered on
// the exact same full template are merged into one endpoint with a stable
// method order.
func TestSummarizeRoutesMergesMethodsOnSameTemplate(t *testing.T) {
	r := NewRouter()
	r.Get("/things/{id}", noopHandler)
	r.Head("/things/{id}", noopHandler)
	r.Delete("/things/{id}", noopHandler)

	s, err := SummarizeRoutes(r)
	if err != nil {
		t.Fatal(err)
	}
	requireEndpoint(t, s, "/things/{id}",
		[]string{http.MethodGet, http.MethodHead, http.MethodDelete})
}

// TestSummarizeRoutesExplicitHEADVsGETOnly verifies that an explicitly
// registered HEAD is distinguishable from a GET-only route, and that HEAD is
// never invented for GET by the summary even when GetHead-style behaviour is
// available at runtime.
func TestSummarizeRoutesExplicitHEADVsGETOnly(t *testing.T) {
	r := NewRouter()
	r.Get("/get-only", noopHandler)
	r.Get("/both", noopHandler)
	r.Head("/both", noopHandler)

	s, err := SummarizeRoutes(r)
	if err != nil {
		t.Fatal(err)
	}

	getOnly := findEndpoint(t, s, "/get-only")
	if getOnly == nil {
		t.Fatal("missing /get-only")
	}
	if getOnly.ExplicitHEAD() {
		t.Fatalf("GET-only route must not report an explicit HEAD: %+v", getOnly)
	}
	if getOnly.HasMethod(http.MethodHead) {
		t.Fatalf("HEAD must not be fabricated for a GET route: %+v", getOnly)
	}

	both := findEndpoint(t, s, "/both")
	if both == nil {
		t.Fatal("missing /both")
	}
	if !both.ExplicitHEAD() {
		t.Fatalf("/both has an explicit Head() registration: %+v", both)
	}
	if !both.HasMethod(http.MethodGet) || !both.HasMethod(http.MethodHead) {
		t.Fatalf("/both should list GET and HEAD: %+v", both)
	}

	report := s.Report()
	if !strings.Contains(report, "/get-only") ||
		!strings.Contains(report, "HEAD~GET (no explicit HEAD registration)") {
		t.Fatalf("report must flag the GET-only route, got:\n%s", report)
	}
	for _, line := range strings.Split(strings.TrimSpace(report), "\n") {
		if strings.HasPrefix(line, "HEAD    /both") || strings.HasPrefix(line, "GET     /both") {
			if strings.Contains(line, "HEAD~GET") {
				t.Fatalf("/both has explicit HEAD and must not carry HEAD~GET: %q", line)
			}
		}
	}
}

// TestSummarizeRoutesNeverExecutesHandlers uses a counting handler to prove
// that building (and re-building, and rendering) the summary performs zero
// handler or middleware calls. It also proves the routes still serve real
// requests afterwards, i.e. nothing was mutated.
func TestSummarizeRoutesNeverExecutesHandlers(t *testing.T) {
	h := &countingHandler{}
	mwCalls := atomic.Int64{}
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mwCalls.Add(1)
			next.ServeHTTP(w, r)
		})
	}

	r := NewRouter()
	r.Use(mw)
	r.Get("/counted", h.ServeHTTP)
	r.Route("/sub", func(r Router) {
		r.Use(mw)
		r.Get("/counted", h.ServeHTTP)
	})

	for i := 0; i < 3; i++ {
		s, err := SummarizeRoutes(r)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := s.WriteReport(&buf); err != nil {
			t.Fatal(err)
		}
	}

	if got := h.calls(); got != 0 {
		t.Fatalf("handler executed %d times during summarization, want 0", got)
	}
	if got := mwCalls.Load(); got != 0 {
		t.Fatalf("middleware executed %d times during summarization, want 0", got)
	}

	// Routes and middlewares are untouched: a real request still works and
	// increments the counters.
	req := httptest.NewRequest(http.MethodGet, "/sub/counted", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status %d", rec.Code)
	}
	if h.calls() != 1 {
		t.Fatalf("expected exactly 1 handler call after a real request, got %d", h.calls())
	}
	if mwCalls.Load() != 2 {
		t.Fatalf("expected both middlewares to run once, got %d", mwCalls.Load())
	}
}

// TestSummarizeRoutesNestedMounts verifies prefixes accumulate correctly
// across several levels of Mount()/Route(), including an opaque handler
// mounted at the deepest level.
func TestSummarizeRoutesNestedMounts(t *testing.T) {
	deep := NewRouter()
	deep.Get("/leaf/{id}", noopHandler)
	deep.Post("/leaf", noopHandler)

	opaque := &countingHandler{}
	deep.Mount("/assets", opaque)

	mid := NewRouter()
	mid.Mount("/deep", deep)

	r := NewRouter()
	r.Mount("/api/v1", mid)

	s, err := SummarizeRoutes(r)
	if err != nil {
		t.Fatal(err)
	}

	requireEndpoint(t, s, "/api/v1/deep/leaf/{id}", []string{http.MethodGet})
	requireEndpoint(t, s, "/api/v1/deep/leaf", []string{http.MethodPost})

	if len(s.UnexpandedMounts) != 1 ||
		s.UnexpandedMounts[0].Pattern != "/api/v1/deep/assets/*" {
		t.Fatalf("expected nested opaque mount at /api/v1/deep/assets/*, got %+v", s.UnexpandedMounts)
	}
	if opaque.calls() != 0 {
		t.Fatalf("opaque mounted handler must never execute, calls=%d", opaque.calls())
	}
}

// TestSummarizeRoutesOpaqueMountMarkedUnexpanded verifies a Mount() backed by
// a plain http.Handler (not a chi Routes) is reported as unexpanded rather
// than having methods fabricated, while a genuine catch-all Handle() route is
// still summarized normally.
func TestSummarizeRoutesOpaqueMountMarkedUnexpanded(t *testing.T) {
	r := NewRouter()
	r.Mount("/files", http.StripPrefix("/files", http.FileServer(http.Dir("."))))
	r.HandleFunc("/raw/*", noopHandler)

	s, err := SummarizeRoutes(r)
	if err != nil {
		t.Fatal(err)
	}

	if len(s.UnexpandedMounts) != 1 || s.UnexpandedMounts[0].Pattern != "/files/*" {
		t.Fatalf("expected only /files/* unexpanded, got %+v", s.UnexpandedMounts)
	}
	if findEndpoint(t, s, "/files/*") != nil {
		t.Fatalf("opaque mount must not be fabricated into method endpoints: %+v", s.Endpoints)
	}
	// A real catch-all registration still reports all methods explicitly.
	requireEndpoint(t, s, "/raw/*", allMethodNames())
}

// TestSummarizeRoutesEmptySubrouterMount verifies an empty mounted chi router
// contributes nothing, rather than a fabricated wildcard.
func TestSummarizeRoutesEmptySubrouterMount(t *testing.T) {
	r := NewRouter()
	r.Mount("/empty", NewRouter())
	r.Get("/ok", noopHandler)

	s, err := SummarizeRoutes(r)
	if err != nil {
		t.Fatal(err)
	}
	requireEndpoint(t, s, "/ok", []string{http.MethodGet})
	if len(s.Endpoints) != 1 {
		t.Fatalf("expected exactly 1 endpoint, got %+v", s.Endpoints)
	}
	if len(s.UnexpandedMounts) != 0 {
		t.Fatalf("expected no unexpanded mounts, got %+v", s.UnexpandedMounts)
	}
}

// TestSummarizeRoutesStable verifies repeated summaries are identical and
// deterministically ordered.
func TestSummarizeRoutesStable(t *testing.T) {
	build := func() *Mux {
		r := NewRouter()
		r.Post("/z", noopHandler)
		r.Get("/a", noopHandler)
		r.Route("/m", func(r Router) {
			r.Delete("/{id}", noopHandler)
			r.Get("/{id}", noopHandler)
		})
		return r
	}

	s1, err := SummarizeRoutes(build())
	if err != nil {
		t.Fatal(err)
	}
	s2, err := SummarizeRoutes(build())
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(s1, s2) {
		t.Fatalf("summaries not stable:\n%+v\nvs\n%+v", s1, s2)
	}
	if s1.Report() != s2.Report() {
		t.Fatalf("reports not stable:\n%s\nvs\n%s", s1.Report(), s2.Report())
	}

	for i := 1; i < len(s1.Endpoints); i++ {
		if s1.Endpoints[i-1].Pattern > s1.Endpoints[i].Pattern {
			t.Fatalf("endpoints not sorted: %q before %q", s1.Endpoints[i-1].Pattern, s1.Endpoints[i].Pattern)
		}
	}
}

// TestSummarizeRoutesJSON verifies the summary serializes cleanly for API
// checking tooling.
func TestSummarizeRoutesJSON(t *testing.T) {
	r := NewRouter()
	r.Route("/v1", func(r Router) {
		r.Get("/items/{id}", noopHandler)
	})
	r.Mount("/files", http.NotFoundHandler())

	s, err := SummarizeRoutes(r)
	if err != nil {
		t.Fatal(err)
	}

	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		t.Fatalf("summary must JSON-encode: %v", err)
	}

	var decoded RouteSummary
	if err := json.Unmarshal(raw.Bytes(), &decoded); err != nil {
		t.Fatalf("summary JSON must round-trip: %v\n%s", err, raw.String())
	}
	if !reflect.DeepEqual(decoded, *s) {
		t.Fatalf("JSON round-trip mismatch:\n%s", raw.String())
	}
}

// TestSummarizeRoutesRootMount verifies Mount("/", sub) prefix handling and
// same-template method merging.
func TestSummarizeRoutesRootMount(t *testing.T) {
	sub := NewRouter()
	sub.Get("/a/{id}", noopHandler)
	sub.Post("/a/{id}", noopHandler)
	r := NewRouter()
	r.Mount("/", sub)

	s, err := SummarizeRoutes(r)
	if err != nil {
		t.Fatal(err)
	}
	requireEndpoint(t, s, "/a/{id}", []string{http.MethodGet, http.MethodPost})
}

// TestSummarizeRoutesNilRouter verifies the error contract on nil input.
func TestSummarizeRoutesNilRouter(t *testing.T) {
	if _, err := SummarizeRoutes(nil); err == nil {
		t.Fatal("expected error summarizing a nil router")
	}
}

// TestSummarizeRoutesHandlerAndSubrouterCollision mirrors the existing Walk
// regression (issue #830): a node that both mounts a subrouter and carries a
// real endpoint on its connector pattern reports both.
func TestSummarizeRoutesHandlerAndSubrouterCollision(t *testing.T) {
	r := NewRouter()
	r.Route("/foo", func(r Router) {
		r.Route("/bar", func(r Router) {
			r.Get("/{id}", noopHandler)
		})
		r.Get("/bar", noopHandler)
	})

	s, err := SummarizeRoutes(r)
	if err != nil {
		t.Fatal(err)
	}
	requireEndpoint(t, s, "/foo/bar", []string{http.MethodGet})
	requireEndpoint(t, s, "/foo/bar/{id}", []string{http.MethodGet})
}

// TestSummarizeRoutesDoesNotMutateRouter ensures summarizing leaves the
// underlying Routes()/Walk output and middleware stack untouched.
func TestSummarizeRoutesDoesNotMutateRouter(t *testing.T) {
	r := NewRouter()
	r.Use(func(next http.Handler) http.Handler { return next })
	r.Get("/x", noopHandler)
	r.Route("/y", func(r Router) {
		r.Get("/z", noopHandler)
	})

	before := walkAll(r)
	if _, err := SummarizeRoutes(r); err != nil {
		t.Fatal(err)
	}
	if _, err := SummarizeRoutes(r); err != nil {
		t.Fatal(err)
	}
	after := walkAll(r)

	if !reflect.DeepEqual(before, after) {
		t.Fatalf("Walk output changed after summarizing:\nbefore=%v\nafter=%v", before, after)
	}
	if len(r.Middlewares()) != 1 {
		t.Fatalf("middleware stack altered, got %d middlewares", len(r.Middlewares()))
	}
}

func walkAll(r Routes) []string {
	var out []string
	_ = Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out = append(out, method+" "+route)
		return nil
	})
	return out
}

// fakeRoutes is a Routes implementation that is not a *Mux. It exercises the
// public Walk-based fallback used when SummarizeRoutes is handed a custom
// router implementation.
type fakeRoutes struct {
	routes      []Route
	middlewares Middlewares
}

func (f *fakeRoutes) Routes() []Route                      { return f.routes }
func (f *fakeRoutes) Middlewares() Middlewares             { return f.middlewares }
func (f *fakeRoutes) Match(*Context, string, string) bool  { return false }
func (f *fakeRoutes) Find(*Context, string, string) string { return "" }

// TestSummarizeRoutesCustomRoutesFallback verifies a non-*Mux Routes value is
// summarized through the public Walk traversal, including a nested subroute.
func TestSummarizeRoutesCustomRoutesFallback(t *testing.T) {
	h := http.HandlerFunc(noopHandler)
	inner := &fakeRoutes{
		routes: []Route{{Handlers: map[string]http.Handler{http.MethodGet: h}, Pattern: "/child"}},
	}
	top := &fakeRoutes{
		routes: []Route{
			{
				SubRoutes: inner,
				Handlers:  map[string]http.Handler{"*": h},
				Pattern:   "/mount/*",
			},
			{Handlers: map[string]http.Handler{http.MethodPost: h}, Pattern: "/root"},
		},
	}

	s, err := SummarizeRoutes(top)
	if err != nil {
		t.Fatal(err)
	}
	requireEndpoint(t, s, "/mount/child", []string{http.MethodGet})
	requireEndpoint(t, s, "/root", []string{http.MethodPost})
}

// TestSummarizeRoutesReport verifies the short report text format, including
// the explicit-HEAD vs GET-only difference and the unexpanded note.
func TestSummarizeRoutesReport(t *testing.T) {
	r := NewRouter()
	r.Get("/v1/items/{id}", noopHandler)
	r.Get("/healthz", noopHandler)
	r.Head("/healthz", noopHandler)
	r.Mount("/files", http.NotFoundHandler())

	s, err := SummarizeRoutes(r)
	if err != nil {
		t.Fatal(err)
	}

	report := s.Report()
	for _, want := range []string{
		"Route method summary",
		"GET     /healthz",
		"HEAD    /healthz",
		"GET     /v1/items/{id}\t# HEAD~GET (no explicit HEAD registration)",
		"MOUNT   /files/* (unexpanded: mounted handler is not a chi router)",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}

	// WriteReport to a failing writer surfaces the error.
	if err := s.WriteReport(errWriter{}); err == nil {
		t.Fatal("expected WriteReport to propagate a writer error")
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, bytes.ErrTooLarge }

// ExampleSummarizeRoutes is the API-facing usage example demonstrating the
// read-only method summary and short report.
func ExampleSummarizeRoutes() {
	r := NewRouter()
	r.Route("/v1", func(r Router) {
		r.Get("/items/{id}", func(w http.ResponseWriter, r *http.Request) {})
		r.Post("/items", func(w http.ResponseWriter, r *http.Request) {})
	})
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {})
	r.Head("/healthz", func(w http.ResponseWriter, r *http.Request) {})

	s, _ := SummarizeRoutes(r)

	for _, ep := range s.Endpoints {
		fmt.Printf("%s %s\n", strings.Join(ep.Methods, ","), ep.Pattern)
	}
	// Output:
	// GET,HEAD /healthz
	// POST /v1/items
	// GET /v1/items/{id}
}
