package chi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestURLBuilderBasic(t *testing.T) {
	r := NewRouter()
	r.Get("/files/{name}", func(w http.ResponseWriter, r *http.Request) {})

	b := NewURLBuilder(r)

	tests := []struct {
		name     string
		pattern  string
		params   map[string]string
		expected string
	}{
		{
			name:     "simple param",
			pattern:  "/files/{name}",
			params:   map[string]string{"name": "report.txt"},
			expected: "/files/report.txt",
		},
		{
			name:     "slash in value encoded",
			pattern:  "/files/{name}",
			params:   map[string]string{"name": "a/b"},
			expected: "/files/a%2Fb",
		},
		{
			name:     "multiple slashes encoded",
			pattern:  "/files/{name}",
			params:   map[string]string{"name": "a/b/c"},
			expected: "/files/a%2Fb%2Fc",
		},
		{
			name:     "unicode value",
			pattern:  "/files/{name}",
			params:   map[string]string{"name": "日本語"},
			expected: "/files/%E6%97%A5%E6%9C%AC%E8%AA%9E",
		},
		{
			name:     "special chars encoded",
			pattern:  "/files/{name}",
			params:   map[string]string{"name": "a b?c=d&e"},
			expected: "/files/a%20b%3Fc=d&e",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path, err := b.Build(tc.pattern, tc.params)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if path != tc.expected {
				t.Fatalf("expected %q, got %q", tc.expected, path)
			}
		})
	}
}

func TestURLBuilderRegexp(t *testing.T) {
	r := NewRouter()
	r.Get("/files/{id:[0-9]+}", func(w http.ResponseWriter, r *http.Request) {})
	r.Get("/items/{code:[a-z]{3}}", func(w http.ResponseWriter, r *http.Request) {})

	b := NewURLBuilder(r)

	tests := []struct {
		name        string
		pattern     string
		params      map[string]string
		expectError bool
	}{
		{
			name:    "valid numeric id",
			pattern: "/files/{id:[0-9]+}",
			params:  map[string]string{"id": "123"},
		},
		{
			name:        "invalid alpha id rejected",
			pattern:     "/files/{id:[0-9]+}",
			params:      map[string]string{"id": "abc"},
			expectError: true,
		},
		{
			name:        "invalid mixed id rejected",
			pattern:     "/files/{id:[0-9]+}",
			params:      map[string]string{"id": "12abc"},
			expectError: true,
		},
		{
			name:    "valid code",
			pattern: "/items/{code:[a-z]{3}}",
			params:  map[string]string{"code": "abc"},
		},
		{
			name:        "invalid code rejected",
			pattern:     "/items/{code:[a-z]{3}}",
			params:      map[string]string{"code": "ab"},
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := b.Build(tc.pattern, tc.params)
			if tc.expectError && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.expectError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestURLBuilderWildcard(t *testing.T) {
	r := NewRouter()
	r.Get("/files/*", func(w http.ResponseWriter, r *http.Request) {})
	r.Get("/proxy/{name}/*", func(w http.ResponseWriter, r *http.Request) {})

	b := NewURLBuilder(r)

	tests := []struct {
		name     string
		pattern  string
		params   map[string]string
		expected string
	}{
		{
			name:     "wildcard with slashes",
			pattern:  "/files/*",
			params:   map[string]string{"*": "a/b/c"},
			expected: "/files/a/b/c",
		},
		{
			name:     "wildcard single segment",
			pattern:  "/files/*",
			params:   map[string]string{"*": "a"},
			expected: "/files/a",
		},
		{
			name:     "wildcard empty",
			pattern:  "/files/*",
			params:   map[string]string{"*": ""},
			expected: "/files/",
		},
		{
			name:     "param and wildcard",
			pattern:  "/proxy/{name}/*",
			params:   map[string]string{"name": "svc", "*": "path/to/resource"},
			expected: "/proxy/svc/path/to/resource",
		},
		{
			name:     "param encoded but wildcard not",
			pattern:  "/proxy/{name}/*",
			params:   map[string]string{"name": "a/b", "*": "x/y"},
			expected: "/proxy/a%2Fb/x/y",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path, err := b.Build(tc.pattern, tc.params)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if path != tc.expected {
				t.Fatalf("expected %q, got %q", tc.expected, path)
			}
		})
	}
}

func TestURLBuilderMissingParams(t *testing.T) {
	r := NewRouter()
	r.Get("/files/{name}", func(w http.ResponseWriter, r *http.Request) {})
	r.Get("/files/{name}/detail/{id}", func(w http.ResponseWriter, r *http.Request) {})

	b := NewURLBuilder(r)

	tests := []struct {
		name    string
		pattern string
		params  map[string]string
	}{
		{
			name:    "missing single param",
			pattern: "/files/{name}",
			params:  map[string]string{},
		},
		{
			name:    "missing one of multiple params",
			pattern: "/files/{name}/detail/{id}",
			params:  map[string]string{"name": "a"},
		},
		{
			name:    "missing all params",
			pattern: "/files/{name}/detail/{id}",
			params:  map[string]string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := b.Build(tc.pattern, tc.params)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "missing parameter") {
				t.Fatalf("expected missing parameter error, got: %v", err)
			}
		})
	}
}

func TestURLBuilderExtraParams(t *testing.T) {
	r := NewRouter()
	r.Get("/files/{name}", func(w http.ResponseWriter, r *http.Request) {})

	b := NewURLBuilder(r)

	tests := []struct {
		name    string
		pattern string
		params  map[string]string
	}{
		{
			name:    "extra param",
			pattern: "/files/{name}",
			params:  map[string]string{"name": "a", "extra": "b"},
		},
		{
			name:    "multiple extra params",
			pattern: "/files/{name}",
			params:  map[string]string{"name": "a", "zzz": "b", "aaa": "c"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := b.Build(tc.pattern, tc.params)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "extra parameters") {
				t.Fatalf("expected extra parameters error, got: %v", err)
			}
		})
	}
}

func TestURLBuilderRouteNotFound(t *testing.T) {
	r := NewRouter()
	r.Get("/files/{name}", func(w http.ResponseWriter, r *http.Request) {})

	b := NewURLBuilder(r)

	_, err := b.Build("/other/{id}", map[string]string{"id": "1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
}

func TestURLBuilderVerification(t *testing.T) {
	r := NewRouter()
	r.Get("/files/{name}", func(w http.ResponseWriter, r *http.Request) {})
	r.Get("/files/{id:[0-9]+}", func(w http.ResponseWriter, r *http.Request) {})
	r.Get("/files/*", func(w http.ResponseWriter, r *http.Request) {})

	b := NewURLBuilder(r)

	tests := []struct {
		name    string
		pattern string
		params  map[string]string
	}{
		{
			name:    "encoded slash matches original route",
			pattern: "/files/{name}",
			params:  map[string]string{"name": "a/b"},
		},
		{
			name:    "unicode matches original route",
			pattern: "/files/{name}",
			params:  map[string]string{"name": "日本語"},
		},
		{
			name:    "regexp matches original route",
			pattern: "/files/{id:[0-9]+}",
			params:  map[string]string{"id": "123"},
		},
		{
			name:    "wildcard matches original route",
			pattern: "/files/*",
			params:  map[string]string{"*": "a/b/c"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path, err := b.Build(tc.pattern, tc.params)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			rctx := NewRouteContext()
			matched := r.Find(rctx, "GET", path)
			if matched != tc.pattern {
				t.Fatalf("expected matched pattern %q, got %q (path=%q)", tc.pattern, matched, path)
			}
		})
	}
}

func TestURLBuilderNoHandlerExecution(t *testing.T) {
	var count uint64
	countingMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count++
			next.ServeHTTP(w, r)
		})
	}

	handlerCalled := false
	r := NewRouter()
	r.Use(countingMiddleware)
	r.Get("/files/{name}", func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	})

	b := NewURLBuilder(r)

	_, err := b.Build("/files/{name}", map[string]string{"name": "a/b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if count != 0 {
		t.Fatalf("middleware was triggered during build: count=%d", count)
	}
	if handlerCalled {
		t.Fatal("handler was executed during build")
	}
}

func TestURLBuilderMultipleRoutes(t *testing.T) {
	r := NewRouter()
	r.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {})
	r.Post("/users/{id}", func(w http.ResponseWriter, r *http.Request) {})
	r.Get("/files/{name}", func(w http.ResponseWriter, r *http.Request) {})

	b := NewURLBuilder(r)

	path, err := b.Build("/users/{id}", map[string]string{"id": "42"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "/users/42" {
		t.Fatalf("expected /users/42, got %q", path)
	}

	path, err = b.Build("/files/{name}", map[string]string{"name": "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "/files/test" {
		t.Fatalf("expected /files/test, got %q", path)
	}
}

func TestURLBuilderMountedRoutes(t *testing.T) {
	r := NewRouter()
	sub := NewRouter()
	sub.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {})
	r.Mount("/api", sub)

	b := NewURLBuilder(r)

	path, err := b.Build("/api/users/{id}", map[string]string{"id": "42"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "/api/users/42" {
		t.Fatalf("expected /api/users/42, got %q", path)
	}

	rctx := NewRouteContext()
	matched := r.Find(rctx, "GET", path)
	if matched != "/api/users/{id}" {
		t.Fatalf("expected matched pattern /api/users/{id}, got %q", matched)
	}
}

func TestURLBuilderWithServer(t *testing.T) {
	r := NewRouter()
	r.Get("/files/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := URLParam(r, "name")
		w.Write([]byte(name))
	})

	b := NewURLBuilder(r)
	path, err := b.Build("/files/{name}", map[string]string{"name": "a/b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ts := httptest.NewServer(r)
	defer ts.Close()

	_, body := testRequest(t, ts, "GET", path, nil)
	if body != "a%2Fb" {
		t.Fatalf("expected body %q, got %q", "a%2Fb", body)
	}
}

func TestURLBuilderConcurrency(t *testing.T) {
	r := NewRouter()
	r.Get("/files/{name}", func(w http.ResponseWriter, r *http.Request) {})

	b := NewURLBuilder(r)

	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func(i int) {
			defer func() { done <- true }()
			params := map[string]string{"name": fmt.Sprintf("file%d.txt", i)}
			_, err := b.Build("/files/{name}", params)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}
