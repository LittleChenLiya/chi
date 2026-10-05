package chi

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A param value containing "/" must be encoded into a single path segment
// ("%2F") and still match the original route it was built from.
func TestBuildURLSlashInParamStaysInSegment(t *testing.T) {
	r := NewRouter()
	calls := 0
	r.Get("/files/{name}", func(w http.ResponseWriter, req *http.Request) { calls++ })

	path, err := r.BuildURL("/files/{name}", map[string]string{"name": "a/b"})
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	if path != "/files/a%2Fb" {
		t.Fatalf("expected /files/a%%2Fb, got %q", path)
	}
	if calls != 0 {
		t.Fatalf("handler executed during BuildURL (%d calls)", calls)
	}

	// The built path must match the original route as a single segment,
	// without executing the handler.
	rctx := NewRouteContext()
	if !r.Match(rctx, "GET", path) {
		t.Fatalf("built path %q does not match", path)
	}
	if got := rctx.RoutePattern(); got != "/files/{name}" {
		t.Fatalf("matched pattern %q, expected /files/{name}", got)
	}
	if got := rctx.URLParam("name"); got != "a%2Fb" {
		t.Fatalf("param captured as %q, expected a%%2Fb (single segment)", got)
	}
	if calls != 0 {
		t.Fatalf("handler executed during Match (%d calls)", calls)
	}

	// End-to-end: a real request carrying the encoded path (RawPath) routes
	// to the same handler and captures one segment.
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if calls != 1 {
		t.Fatalf("expected handler to run once, got %d", calls)
	}
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

// Regexp constraints of the original template are enforced on the raw value.
func TestBuildURLRegexpConstraint(t *testing.T) {
	r := NewRouter()
	r.Get("/users/{id:[0-9]+}", func(w http.ResponseWriter, req *http.Request) {})

	if _, err := r.BuildURL("/users/{id:[0-9]+}", map[string]string{"id": "abc"}); !errors.Is(err, ErrURLParamConstraint) {
		t.Fatalf("expected ErrURLParamConstraint for id=abc, got %v", err)
	}

	path, err := r.BuildURL("/users/{id:[0-9]+}", map[string]string{"id": "123"})
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	if path != "/users/123" {
		t.Fatalf("expected /users/123, got %q", path)
	}
}

// The trailing wildcard accepts a multi-segment tail; each segment is
// encoded individually and "/" remains the segment separator.
func TestBuildURLWildcardTail(t *testing.T) {
	r := NewRouter()
	r.Get("/static/*", func(w http.ResponseWriter, req *http.Request) {})

	path, err := r.BuildURL("/static/*", map[string]string{"*": "css/site.css"})
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	if path != "/static/css/site.css" {
		t.Fatalf("expected /static/css/site.css, got %q", path)
	}

	rctx := NewRouteContext()
	if !r.Match(rctx, "GET", path) || rctx.RoutePattern() != "/static/*" {
		t.Fatalf("built path %q does not match /static/*", path)
	}
	if got := rctx.URLParam("*"); got != "css/site.css" {
		t.Fatalf("wildcard captured as %q", got)
	}

	// Segments needing escaping are encoded per segment.
	path, err = r.BuildURL("/static/*", map[string]string{"*": "my docs/a b.txt"})
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	if path != "/static/my%20docs/a%20b.txt" {
		t.Fatalf("expected /static/my%%20docs/a%%20b.txt, got %q", path)
	}
}

// Missing and extra parameters are explicit errors.
func TestBuildURLMissingAndExtraParams(t *testing.T) {
	r := NewRouter()
	r.Get("/a/{x}/b/{y}", func(w http.ResponseWriter, req *http.Request) {})

	if _, err := r.BuildURL("/a/{x}/b/{y}", map[string]string{"x": "1"}); !errors.Is(err, ErrURLMissingParam) {
		t.Fatalf("expected ErrURLMissingParam, got %v", err)
	}
	if _, err := r.BuildURL("/a/{x}/b/{y}", map[string]string{"x": "1", "y": "2", "z": "3"}); !errors.Is(err, ErrURLExtraParam) {
		t.Fatalf("expected ErrURLExtraParam, got %v", err)
	}
	if _, err := r.BuildURL("/a/{x}/b/{y}", map[string]string{"x": "1", "y": "2"}); err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
}

// Unicode values are percent-encoded (UTF-8) within their segment and the
// built path still matches the original route.
func TestBuildURLUnicodeParam(t *testing.T) {
	r := NewRouter()
	r.Get("/files/{name}", func(w http.ResponseWriter, req *http.Request) {})

	path, err := r.BuildURL("/files/{name}", map[string]string{"name": "你好 世界"})
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	want := "/files/%E4%BD%A0%E5%A5%BD%20%E4%B8%96%E7%95%8C"
	if path != want {
		t.Fatalf("expected %q, got %q", want, path)
	}

	rctx := NewRouteContext()
	if !r.Match(rctx, "GET", path) || rctx.RoutePattern() != "/files/{name}" {
		t.Fatalf("built path %q does not match /files/{name}", path)
	}
}

// Building a URL for a pattern that is not registered is an error.
func TestBuildURLUnregisteredPattern(t *testing.T) {
	r := NewRouter()
	r.Get("/files/{name}", func(w http.ResponseWriter, req *http.Request) {})

	if _, err := r.BuildURL("/nope/{name}", map[string]string{"name": "x"}); !errors.Is(err, ErrRoutePatternNotFound) {
		t.Fatalf("expected ErrRoutePatternNotFound, got %v", err)
	}
}

// If the built path would resolve to a different route (here an encoded
// value collides with a static route, which matches first), the identity
// check rejects it.
func TestBuildURLIdentityMismatch(t *testing.T) {
	r := NewRouter()
	r.Get("/files/a%2Fb", func(w http.ResponseWriter, req *http.Request) {})
	r.Get("/files/{name}", func(w http.ResponseWriter, req *http.Request) {})

	if _, err := r.BuildURL("/files/{name}", map[string]string{"name": "a/b"}); !errors.Is(err, ErrURLIdentityMismatch) {
		t.Fatalf("expected ErrURLIdentityMismatch, got %v", err)
	}

	// Other values still build and match the param route fine.
	path, err := r.BuildURL("/files/{name}", map[string]string{"name": "c/d"})
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	if path != "/files/c%2Fd" {
		t.Fatalf("expected /files/c%%2Fd, got %q", path)
	}
}

// BuildURL works for routes registered with Handle (all methods).
func TestBuildURLHandleRoute(t *testing.T) {
	r := NewRouter()
	r.Handle("/things/{id}", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {}))

	path, err := r.BuildURL("/things/{id}", map[string]string{"id": "42"})
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	if path != "/things/42" {
		t.Fatalf("expected /things/42, got %q", path)
	}
}

func ExampleMux_BuildURL() {
	r := NewRouter()
	r.Get("/files/{name}", func(w http.ResponseWriter, r *http.Request) {})
	r.Get("/users/{id:[0-9]+}", func(w http.ResponseWriter, r *http.Request) {})
	r.Get("/static/*", func(w http.ResponseWriter, r *http.Request) {})

	// A "/" inside a single-segment parameter is encoded, not split.
	path, _ := r.BuildURL("/files/{name}", map[string]string{"name": "a/b"})
	fmt.Println(path)

	// Regexp constraints from the pattern are enforced.
	path, _ = r.BuildURL("/users/{id:[0-9]+}", map[string]string{"id": "123"})
	fmt.Println(path)

	// The wildcard tail may span multiple segments.
	path, _ = r.BuildURL("/static/*", map[string]string{"*": "css/site.css"})
	fmt.Println(path)

	// Output:
	// /files/a%2Fb
	// /users/123
	// /static/css/site.css
}
