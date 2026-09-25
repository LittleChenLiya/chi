package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestBindParamsParsesOncePerRequest(t *testing.T) {
	var idCalls, itemCalls atomic.Int32
	parseID := func(raw string) (int, error) {
		idCalls.Add(1)
		return ParseInt(raw)
	}
	parseItem := func(raw string) (int64, error) {
		itemCalls.Add(1)
		return ParseInt64(raw)
	}

	var handlerCalls atomic.Int32
	r := chi.NewRouter()
	r.With(BindParams(Param("id", parseID), Param("item", parseItem))).
		Get("/users/{id}/items/{item}", func(w http.ResponseWriter, r *http.Request) {
			handlerCalls.Add(1)
			// Read the bound values multiple times; parsers must not run again.
			for i := 0; i < 3; i++ {
				id, ok := ParamValue[int](r.Context(), "id")
				if !ok || id != 42 {
					t.Errorf("read %d: id = %v, %v; want 42, true", i, id, ok)
				}
				item, ok := ParamValue[int64](r.Context(), "item")
				if !ok || item != 7 {
					t.Errorf("read %d: item = %v, %v; want 7, true", i, item, ok)
				}
			}
			// The classic chi.URLParam interface keeps working.
			if got := chi.URLParam(r, "id"); got != "42" {
				t.Errorf("chi.URLParam(id) = %q; want %q", got, "42")
			}
			w.WriteHeader(http.StatusOK)
		})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/users/42/items/7", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if got := handlerCalls.Load(); got != 1 {
		t.Fatalf("handler calls = %d; want 1", got)
	}
	if got := idCalls.Load(); got != 1 {
		t.Fatalf("id parser calls = %d; want 1", got)
	}
	if got := itemCalls.Load(); got != 1 {
		t.Fatalf("item parser calls = %d; want 1", got)
	}
}

func TestBindParamsNestedRoutes(t *testing.T) {
	r := chi.NewRouter()
	r.Route("/orgs/{org}", func(r chi.Router) {
		r.Route("/users/{id}", func(r chi.Router) {
			r.With(BindParams(Param("org", ParseUUID), Param("id", ParseInt))).
				Get("/items/{item}", func(w http.ResponseWriter, r *http.Request) {
					org, ok := ParamValue[string](r.Context(), "org")
					if !ok || org != "123e4567-e89b-12d3-a456-426614174000" {
						t.Errorf("org = %v, %v", org, ok)
					}
					id, ok := ParamValue[int](r.Context(), "id")
					if !ok || id != 9 {
						t.Errorf("id = %v, %v", id, ok)
					}
					w.WriteHeader(http.StatusOK)
				})
		})
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/orgs/123e4567-e89b-12d3-a456-426614174000/users/9/items/3", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200, body: %s", w.Code, w.Body.String())
	}
}

func TestBindParamsMount(t *testing.T) {
	sub := chi.NewRouter()
	sub.With(BindParams(Param("id", ParseInt), Param("item", ParseInt))).
		Get("/items/{item}", func(w http.ResponseWriter, r *http.Request) {
			id, _ := ParamValue[int](r.Context(), "id")
			item, _ := ParamValue[int](r.Context(), "item")
			fmt.Fprintf(w, "%d/%d", id, item)
		})

	r := chi.NewRouter()
	r.Mount("/users/{id}", sub)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/users/5/items/11", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200, body: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "5/11" {
		t.Fatalf("body = %q; want %q", got, "5/11")
	}
}

func TestBindParamsOptionalMissing(t *testing.T) {
	var handlerCalls atomic.Int32
	r := chi.NewRouter()
	r.With(BindParams(Param("id", ParseInt), OptionalParam("item", ParseInt))).
		Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
			handlerCalls.Add(1)
			id, ok := ParamValue[int](r.Context(), "id")
			if !ok || id != 3 {
				t.Errorf("id = %v, %v; want 3, true", id, ok)
			}
			item, ok := ParamValue[int](r.Context(), "item")
			if ok {
				t.Errorf("optional item reported provided with value %v; want absent", item)
			}
			if item != 0 {
				t.Errorf("optional item = %v; want zero value %v", item, 0)
			}
			w.WriteHeader(http.StatusNoContent)
		})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/users/3", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d; want 204, body: %s", w.Code, w.Body.String())
	}
	if got := handlerCalls.Load(); got != 1 {
		t.Fatalf("handler calls = %d; want 1", got)
	}
}

func TestBindParamsInvalidInt(t *testing.T) {
	var handlerCalls atomic.Int32
	r := chi.NewRouter()
	r.With(BindParams(Param("id", ParseInt))).
		Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
			handlerCalls.Add(1)
		})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/users/abc", nil))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", w.Code)
	}
	if got := handlerCalls.Load(); got != 0 {
		t.Fatalf("handler calls = %d; want 0", got)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"id"`) {
		t.Errorf("body %q does not name the parameter", body)
	}
	if strings.Contains(body, "abc") {
		t.Errorf("body %q echoes the raw parameter value", body)
	}
}

func TestBindParamsParserError(t *testing.T) {
	boom := errors.New("boom")
	parse := func(raw string) (int, error) { return 0, boom }

	var handlerCalls atomic.Int32
	r := chi.NewRouter()
	r.With(BindParams(Param("id", parse))).
		Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
			handlerCalls.Add(1)
		})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/users/42", nil))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", w.Code)
	}
	if got := handlerCalls.Load(); got != 0 {
		t.Fatalf("handler calls = %d; want 0", got)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"id"`) {
		t.Errorf("body %q does not name the parameter", body)
	}
	if strings.Contains(body, "42") {
		t.Errorf("body %q echoes the raw parameter value", body)
	}
}

func TestBindParamsMissingRequired(t *testing.T) {
	var handlerCalls atomic.Int32
	r := chi.NewRouter()
	// The route pattern has no {item} placeholder, so the required
	// parameter is missing from the matched route.
	r.With(BindParams(Param("id", ParseInt), Param("item", ParseInt))).
		Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
			handlerCalls.Add(1)
		})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/users/1", nil))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", w.Code)
	}
	if got := handlerCalls.Load(); got != 0 {
		t.Fatalf("handler calls = %d; want 0", got)
	}
	if body := w.Body.String(); !strings.Contains(body, `"item"`) {
		t.Errorf("body %q does not name the missing parameter", body)
	}
}

func TestBindParamsNotFound(t *testing.T) {
	var handlerCalls atomic.Int32
	r := chi.NewRouter()
	r.With(BindParams(Param("id", ParseInt))).
		Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
			handlerCalls.Add(1)
		})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/nope", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", w.Code)
	}
	if got := handlerCalls.Load(); got != 0 {
		t.Fatalf("handler calls = %d; want 0", got)
	}
}

func TestBindParamsRequestIsolation(t *testing.T) {
	var parseCalls atomic.Int32
	parse := func(raw string) (int, error) {
		parseCalls.Add(1)
		return ParseInt(raw)
	}

	r := chi.NewRouter()
	r.With(BindParams(Param("id", parse))).
		Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
			id, _ := ParamValue[int](r.Context(), "id")
			fmt.Fprint(w, id)
		})

	// Two requests with the same parameter value must each parse their
	// own value; nothing is cached across requests.
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/users/42", nil))
		if w.Code != http.StatusOK || w.Body.String() != "42" {
			t.Fatalf("request %d: status = %d, body = %q", i, w.Code, w.Body.String())
		}
	}
	if got := parseCalls.Load(); got != 2 {
		t.Fatalf("parser calls = %d; want 2 (one per request)", got)
	}

	// Concurrent requests with different values stay isolated.
	parseCalls.Store(0)
	results := make(chan string, 2)
	for _, path := range []string{"/users/1", "/users/2"} {
		go func(path string) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			results <- w.Body.String()
		}(path)
	}
	got := map[string]bool{<-results: true, <-results: true}
	if !got["1"] || !got["2"] {
		t.Fatalf("concurrent results = %v; want 1 and 2", got)
	}
}

func TestBindParamsDuplicateDeclarationPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate parameter declaration")
		}
	}()
	BindParams(Param("id", ParseInt), Param("id", ParseInt))
}

func TestBindParamsEmptyNamePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on empty parameter name")
		}
	}()
	BindParams(Param("", ParseInt))
}

func TestParamValueWithoutBinding(t *testing.T) {
	r := chi.NewRouter()
	r.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := ParamValue[int](r.Context(), "id"); ok {
			t.Error("ParamValue reported a bound value without BindParams")
		}
		w.WriteHeader(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/users/1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
}

func TestParseUUID(t *testing.T) {
	if _, err := ParseUUID("123e4567-e89b-12d3-a456-426614174000"); err != nil {
		t.Errorf("valid UUID rejected: %v", err)
	}
	for _, raw := range []string{"", "not-a-uuid", "123e4567e89b12d3a456426614174000", "123e4567-e89b-12d3-a456-42661417400g"} {
		if _, err := ParseUUID(raw); err == nil {
			t.Errorf("invalid UUID %q accepted", raw)
		}
	}
}
