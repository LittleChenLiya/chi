package chi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// captureHandler records the pattern and URL params of a real request.
type capture struct {
	pattern string
	params  map[string]string
}

func (c *capture) handler(w http.ResponseWriter, r *http.Request) {
	c.pattern = r.Pattern
	rctx := RouteContext(r.Context())
	c.params = map[string]string{}
	for i, k := range rctx.URLParams.Keys {
		c.params[k] = rctx.URLParams.Values[i]
	}
}

func newExplainTestRouter(caps map[string]*capture) *Mux {
	r := NewRouter()
	r.Use(func(next http.Handler) http.Handler { return next })
	r.Get("/users/new", caps["/users/new"].handler)
	r.Get("/users/{id}", caps["/users/{id}"].handler)
	r.Get("/files/*", caps["/files/*"].handler)
	r.Route("/api", func(r Router) {
		r.Get("/articles/{id}", caps["/api/articles/{id}"].handler)
	})
	return r
}

func TestExplainRouteMatchesRealRequests(t *testing.T) {
	patterns := []string{"/users/new", "/users/{id}", "/files/*", "/api/articles/{id}"}
	caps := map[string]*capture{}
	for _, p := range patterns {
		caps[p] = &capture{}
	}
	r := newExplainTestRouter(caps)

	cases := []struct {
		method, path, pattern string
		params                map[string]string
		kind                  string
	}{
		{"GET", "/users/new", "/users/new", map[string]string{}, "static"},
		{"GET", "/users/42", "/users/{id}", map[string]string{"id": "42"}, "param"},
		{"GET", "/files/a/b/c", "/files/*", map[string]string{"*": "a/b/c"}, "wildcard"},
		{"GET", "/api/articles/7", "/api/articles/{id}", map[string]string{"*": "", "id": "7"}, "param"},
	}

	for _, tc := range cases {
		// Real request through httptest.
		cap := caps[tc.pattern]
		cap.pattern, cap.params = "", nil
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s %s: real request got status %d", tc.method, tc.path, rec.Code)
		}

		ex := r.ExplainRoute(tc.method, tc.path)
		if ex.Status != ExplainMatched {
			t.Fatalf("%s %s: explain status %q, want matched", tc.method, tc.path, ex.Status)
		}
		if ex.Pattern != cap.pattern {
			t.Errorf("%s %s: explain pattern %q, real pattern %q", tc.method, tc.path, ex.Pattern, cap.pattern)
		}
		if ex.Pattern != tc.pattern {
			t.Errorf("%s %s: pattern %q, want %q", tc.method, tc.path, ex.Pattern, tc.pattern)
		}
		if !reflect.DeepEqual(ex.Params, cap.params) {
			t.Errorf("%s %s: explain params %v, real params %v", tc.method, tc.path, ex.Params, cap.params)
		}
		if !reflect.DeepEqual(ex.Params, tc.params) {
			t.Errorf("%s %s: params %v, want %v", tc.method, tc.path, ex.Params, tc.params)
		}
		if ex.MatchKind != tc.kind {
			t.Errorf("%s %s: match kind %q, want %q", tc.method, tc.path, ex.MatchKind, tc.kind)
		}
		if len(ex.Steps) == 0 {
			t.Errorf("%s %s: no decision steps recorded", tc.method, tc.path)
		}
		if len(ex.Middlewares) == 0 {
			t.Errorf("%s %s: no middleware layers recorded", tc.method, tc.path)
		}
	}
}

func TestExplainRouteStaticBeatsParam(t *testing.T) {
	r := NewRouter()
	r.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {})
	r.Get("/users/new", func(w http.ResponseWriter, r *http.Request) {})

	ex := r.ExplainRoute("GET", "/users/new")
	if ex.Pattern != "/users/new" || ex.MatchKind != "static" {
		t.Fatalf("got pattern %q kind %q, want static /users/new", ex.Pattern, ex.MatchKind)
	}
	joined := strings.Join(ex.Steps, "\n")
	if !strings.Contains(joined, "static") {
		t.Fatalf("steps should show the static branch taken:\n%s", joined)
	}
}

func TestExplainRouteMethodNotAllowed(t *testing.T) {
	r := NewRouter()
	r.Get("/users/new", func(w http.ResponseWriter, r *http.Request) {})

	ex := r.ExplainRoute("POST", "/users/new")
	if ex.Status != ExplainMethodNotAllowed {
		t.Fatalf("status %q, want %q", ex.Status, ExplainMethodNotAllowed)
	}
	if !reflect.DeepEqual(ex.AllowedMethods, []string{"GET"}) {
		t.Fatalf("allowed methods %v, want [GET]", ex.AllowedMethods)
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/users/new", nil))
	if rec.Code != 405 {
		t.Fatalf("real request status %d, want 405", rec.Code)
	}
}

func TestExplainRouteNotFoundAndInvalidMethod(t *testing.T) {
	r := NewRouter()
	r.Get("/users/new", func(w http.ResponseWriter, r *http.Request) {})

	if ex := r.ExplainRoute("GET", "/nope"); ex.Status != ExplainNotFound {
		t.Fatalf("status %q, want not-found", ex.Status)
	}
	if ex := r.ExplainRoute("BREW", "/users/new"); ex.Status != ExplainInvalidMethod {
		t.Fatalf("status %q, want invalid-method", ex.Status)
	}
}

func TestExplainRouteSentinels(t *testing.T) {
	var mwCalls, hCalls atomic.Int64
	sentinelMw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mwCalls.Add(1)
			next.ServeHTTP(w, r)
		})
	}
	sentinelHandler := func(w http.ResponseWriter, r *http.Request) { hCalls.Add(1) }

	r := NewRouter()
	r.Use(sentinelMw)
	r.Get("/users/{id}", sentinelHandler)
	r.With(sentinelMw).Get("/inline/{id}", sentinelHandler)

	for i := 0; i < 10; i++ {
		ex := r.ExplainRoute("GET", "/users/1")
		if ex.Status != ExplainMatched {
			t.Fatalf("status %q", ex.Status)
		}
		ex = r.ExplainRoute("GET", "/inline/2")
		if len(ex.Middlewares) != 2 {
			t.Fatalf("inline route middlewares %v, want 2 layers", ex.Middlewares)
		}
	}
	if mwCalls.Load() != 0 || hCalls.Load() != 0 {
		t.Fatalf("ExplainRoute executed middleware (%d) or handler (%d)", mwCalls.Load(), hCalls.Load())
	}

	// ServeHTTP still works normally afterwards.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/users/1", nil))
	if rec.Code != 200 || hCalls.Load() != 1 || mwCalls.Load() != 1 {
		t.Fatalf("ServeHTTP after explains: code=%d mw=%d h=%d", rec.Code, mwCalls.Load(), hCalls.Load())
	}
}

func TestExplainRouteSameNameParamsAndIsolation(t *testing.T) {
	r := NewRouter()
	r.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {})
	r.Get("/users/new", func(w http.ResponseWriter, r *http.Request) {})
	r.Route("/orgs/{id}", func(r Router) {
		r.Get("/members/{id}", func(w http.ResponseWriter, r *http.Request) {})
	})

	ex := r.ExplainRoute("GET", "/orgs/5/members/9")
	if ex.Status != ExplainMatched || ex.Pattern != "/orgs/{id}/members/{id}" {
		t.Fatalf("status=%q pattern=%q", ex.Status, ex.Pattern)
	}
	// Last capture wins, mirroring Context.URLParam.
	if ex.Params["id"] != "9" {
		t.Fatalf("param id=%q, want 9", ex.Params["id"])
	}

	// Params from one explanation must not leak into the next.
	ex = r.ExplainRoute("GET", "/users/5")
	if ex.Params["id"] != "5" {
		t.Fatalf("param id=%q, want 5", ex.Params["id"])
	}
	ex = r.ExplainRoute("GET", "/users/new")
	if _, ok := ex.Params["id"]; ok {
		t.Fatalf("stale param id leaked into explanation: %v", ex.Params)
	}

	// And a real request afterwards sees only its own params.
	var got string
	r2 := NewRouter()
	r2.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		got = URLParam(r, "id")
	})
	r2.ExplainRoute("GET", "/users/5")
	r2.ExplainRoute("GET", "/users/6")
	r2.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/users/7", nil))
	if got != "7" {
		t.Fatalf("real request saw param %q, want 7", got)
	}
}

func TestExplainRouteEscapedPath(t *testing.T) {
	var realPattern, realStar string
	r := NewRouter()
	r.Get("/files/*", func(w http.ResponseWriter, r *http.Request) {
		realPattern = r.Pattern
		realStar = URLParam(r, "*")
	})

	req := httptest.NewRequest("GET", "/files/a%2Fb", nil)
	if req.URL.RawPath == "" {
		t.Skip("net/http did not preserve RawPath for this URL")
	}
	r.ServeHTTP(httptest.NewRecorder(), req)

	ex := r.ExplainRoute("GET", "/files/a%2Fb")
	if ex.Status != ExplainMatched || ex.MatchKind != "wildcard" {
		t.Fatalf("status=%q kind=%q", ex.Status, ex.MatchKind)
	}
	if ex.Pattern != realPattern {
		t.Fatalf("explain pattern %q, real %q", ex.Pattern, realPattern)
	}
	if ex.Params["*"] != realStar {
		t.Fatalf("explain param %q, real %q", ex.Params["*"], realStar)
	}
}

func TestExplainRouteConcurrent(t *testing.T) {
	r := NewRouter()
	r.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {})
	r.Get("/users/new", func(w http.ResponseWriter, r *http.Request) {})
	r.Get("/files/*", func(w http.ResponseWriter, r *http.Request) {})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ex := r.ExplainRoute("GET", fmt.Sprintf("/users/%d", j))
				if ex.Status != ExplainMatched || ex.Params["id"] != fmt.Sprintf("%d", j) {
					t.Errorf("bad explanation: %+v", ex)
					return
				}
				r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/users/new", nil))
			}
		}(i)
	}
	wg.Wait()
}

func TestCheckShadowing(t *testing.T) {
	r := NewRouter()
	h := func(w http.ResponseWriter, r *http.Request) {}
	r.Get("/users/new", h)
	r.Get("/users/{id}", h)
	r.Get("/dup", h)
	r.Get("/dup", h) // identical method+pattern registered twice
	r.Get("/re/{x:[0-9]+}", h)
	r.Get("/re/{y:[a-z]+}", h)
	r.Route("/api", func(r Router) {
		r.Get("/users/new", h)
	})
	r.Get("/api/users/{id}", h)

	reports := r.CheckShadowing()
	byKind := map[string][]ShadowReport{}
	for _, rep := range reports {
		byKind[rep.Kind] = append(byKind[rep.Kind], rep)
	}

	// Exact duplicate method+pattern must be reported.
	dups := byKind[ShadowDuplicate]
	if len(dups) != 1 || dups[0].Pattern != "/dup" || dups[0].Other != "/dup" {
		t.Fatalf("duplicate reports: %+v", dups)
	}
	if !reflect.DeepEqual(dups[0].Methods, []string{"GET"}) {
		t.Fatalf("duplicate methods: %v", dups[0].Methods)
	}

	// Static-over-param overlaps must carry a concrete example.
	overlaps := byKind[ShadowOverlap]
	examples := map[string]string{}
	for _, rep := range overlaps {
		examples[rep.Pattern+"|"+rep.Other] = rep.Example
	}
	if got := examples["/users/new|/users/{id}"]; got != "/users/new" {
		t.Errorf("static/param overlap example %q, want /users/new", got)
	}
	if got := examples["/api/users/{id}|/api/users/new"]; got != "/api/users/new" {
		t.Errorf("mount-prefix overlap example %q, want /api/users/new", got)
	}

	// Regexp containment must be reported as unknown, never as disjoint.
	unknowns := byKind[ShadowUnknown]
	found := false
	for _, rep := range unknowns {
		if rep.Pattern == "/re/{x:[0-9]+}" && rep.Other == "/re/{y:[a-z]+}" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected unknown report for regexp pair, got %+v", reports)
	}
}

func ExampleMux_ExplainRoute() {
	r := NewRouter()
	r.Get("/users/new", func(w http.ResponseWriter, r *http.Request) {})
	r.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {})

	ex := r.ExplainRoute("GET", "/users/new")
	fmt.Println("status:", ex.Status)
	fmt.Println("pattern:", ex.Pattern)
	fmt.Println("kind:", ex.MatchKind)
	fmt.Println("steps:")
	for _, s := range ex.Steps {
		fmt.Println(" ", s)
	}

	for _, rep := range r.CheckShadowing() {
		fmt.Printf("shadow: %s %s vs %s (example %s)\n", rep.Kind, rep.Pattern, rep.Other, rep.Example)
	}

	// Output:
	// status: matched
	// pattern: /users/new
	// kind: static
	// steps:
	//   static: matched prefix "/users/"
	//   static: matched prefix "new"
	// shadow: overlap /users/new vs /users/{id} (example /users/new)
}
