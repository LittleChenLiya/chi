package middleware_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// countingParser returns a parser that counts how many times it is invoked and
// delegates the actual conversion to fn.
func countingParser[T any](calls *atomic.Int64, fn func(string) (T, error)) middleware.ParseURLParam[T] {
	return func(raw string) (T, error) {
		calls.Add(1)
		return fn(raw)
	}
}

func parseInt(raw string) (int, error) { return middleware.IntURLParam(raw) }

func parseEcho(raw string) (string, error) { return raw, nil }

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func parseUUID(raw string) (string, error) {
	if !uuidPattern.MatchString(raw) {
		return "", errors.New("not a uuid")
	}
	return raw, nil
}

func doRequest(r http.Handler, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	r.ServeHTTP(w, req)
	return w
}

// TestBindParamsParsedOnce is the headline acceptance case: /users/{id}/items/{item}
// binds two parameters and reading them repeatedly from the handler parses each
// parameter exactly once, while the legacy chi.URLParam string API keeps working.
func TestBindParamsParsedOnce(t *testing.T) {
	var idCalls, itemCalls, handlerCalls atomic.Int64

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls.Add(1)

		for i := 0; i < 3; i++ {
			id, ok := middleware.URLParam[int](r, "id")
			if !ok || id != 7 {
				t.Fatalf("read %d: id = %d, ok = %v, want 7/true", i, id, ok)
			}
			item, ok := middleware.URLParam[string](r, "item")
			if !ok || item != "widget" {
				t.Fatalf("read %d: item = %q, ok = %v, want widget/true", i, item, ok)
			}
		}

		id, ok := middleware.URLParamFromContext[int](r.Context(), "id")
		if !ok || id != 7 {
			t.Fatalf("context id = %d, ok = %v", id, ok)
		}

		if raw := chi.URLParam(r, "id"); raw != "7" {
			t.Fatalf("chi.URLParam(id) = %q, want 7", raw)
		}
		if raw := chi.URLParam(r, "item"); raw != "widget" {
			t.Fatalf("chi.URLParam(item) = %q, want widget", raw)
		}

		w.WriteHeader(http.StatusOK)
	})

	r := chi.NewRouter()
	binder := middleware.BindParams(
		middleware.RequiredURLParam("id", countingParser(&idCalls, parseInt)),
		middleware.RequiredURLParam("item", countingParser(&itemCalls, parseEcho)),
	)
	r.With(binder).Get("/users/{id}/items/{item}", handler)

	res := doRequest(r, "/users/7/items/widget")

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", res.Code, res.Body.String())
	}
	if got := idCalls.Load(); got != 1 {
		t.Errorf("id parser called %d times, want 1", got)
	}
	if got := itemCalls.Load(); got != 1 {
		t.Errorf("item parser called %d times, want 1", got)
	}
	if got := handlerCalls.Load(); got != 1 {
		t.Errorf("handler called %d times, want 1", got)
	}
}

// TestBindParamsNestedRoutes verifies parameters contributed by several nested
// Route() sub-routers are bound from the final match, not read eagerly.
func TestBindParamsNestedRoutes(t *testing.T) {
	var handlerCalls atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls.Add(1)
		id, ok := middleware.URLParam[int](r, "id")
		if !ok || id != 42 {
			t.Fatalf("id = %d, ok = %v", id, ok)
		}
		item, ok := middleware.URLParam[int](r, "item")
		if !ok || item != 99 {
			t.Fatalf("item = %d, ok = %v", item, ok)
		}
		w.WriteHeader(http.StatusOK)
	})

	r := chi.NewRouter()
	r.Route("/users/{id}", func(users chi.Router) {
		users.Route("/items/{item}", func(items chi.Router) {
			binder := middleware.BindParams(
				middleware.RequiredURLParam("id", middleware.IntURLParam),
				middleware.RequiredURLParam("item", middleware.IntURLParam),
			)
			items.With(binder).Get("/detail", handler)
		})
	})

	res := doRequest(r, "/users/42/items/99/detail")
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", res.Code, res.Body.String())
	}
	if handlerCalls.Load() != 1 {
		t.Fatalf("handler called %d times, want 1", handlerCalls.Load())
	}
}

// TestBindParamsOptional covers an optional parameter present or absent. When
// absent it must remain unprovided rather than being stored as a zero value.
func TestBindParamsOptional(t *testing.T) {
	var handlerCalls atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls.Add(1)
		id, ok := middleware.URLParam[int](r, "id")
		if !ok || id != 5 {
			t.Fatalf("id = %d, ok = %v", id, ok)
		}
		page, ok := middleware.URLParam[int](r, "page")
		if ok {
			t.Fatalf("missing optional page reported as provided: %d", page)
		}
		if page != 0 {
			t.Fatalf("absent optional leaked a value: %d", page)
		}
		w.WriteHeader(http.StatusOK)
	})

	r := chi.NewRouter()
	binder := middleware.BindParams(
		middleware.RequiredURLParam("id", middleware.IntURLParam),
		middleware.OptionalURLParam("page", middleware.IntURLParam),
	)
	r.With(binder).Get("/users/{id}", handler)

	res := doRequest(r, "/users/5")
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", res.Code, res.Body.String())
	}
	if handlerCalls.Load() != 1 {
		t.Fatalf("handler called %d times, want 1", handlerCalls.Load())
	}

	// When present and valid, the optional parameter is provided normally.
	handlerPresent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, ok := middleware.URLParam[int](r, "page")
		if !ok || page != 3 {
			t.Fatalf("page = %d, ok = %v, want 3/true", page, ok)
		}
		w.WriteHeader(http.StatusOK)
	})
	r2 := chi.NewRouter()
	r2.With(binder).Get("/users/{id}/pages/{page}", handlerPresent)

	res = doRequest(r2, "/users/5/pages/3")
	if res.Code != http.StatusOK {
		t.Fatalf("present optional: status = %d, body = %q", res.Code, res.Body.String())
	}
}

// TestBindParamsOptionalInvalid covers a present-but-invalid optional param:
// it is still a 400 and the handler must not run.
func TestBindParamsOptionalInvalid(t *testing.T) {
	var handlerCalls atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls.Add(1)
	})

	r := chi.NewRouter()
	binder := middleware.BindParams(
		middleware.RequiredURLParam("id", middleware.IntURLParam),
		middleware.OptionalURLParam("page", middleware.IntURLParam),
	)
	r.With(binder).Get("/users/{id}/pages/{page}", handler)

	res := doRequest(r, "/users/5/pages/NaN")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %q", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "page") {
		t.Fatalf("error body does not name the parameter: %q", res.Body.String())
	}
	if strings.Contains(res.Body.String(), "NaN") {
		t.Fatalf("error body echoes raw value: %q", res.Body.String())
	}
	if handlerCalls.Load() != 0 {
		t.Fatalf("handler called %d times, want 0", handlerCalls.Load())
	}
}

// TestBindParamsRequestIsolation verifies two requests carrying the same raw
// values each parse independently (two requests -> two parser invocations) and
// values never leak between requests.
func TestBindParamsRequestIsolation(t *testing.T) {
	var idCalls atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := middleware.URLParam[int](r, "id")
		if !ok {
			t.Fatal("id not provided")
		}
		_, _ = io.WriteString(w, fmt.Sprintf("%d", id))
	})

	r := chi.NewRouter()
	binder := middleware.BindParams(
		middleware.RequiredURLParam("id", countingParser(&idCalls, parseInt)),
	)
	r.With(binder).Get("/users/{id}", handler)

	for i := 0; i < 2; i++ {
		res := doRequest(r, "/users/7")
		if res.Code != http.StatusOK || res.Body.String() != "7" {
			t.Fatalf("request %d: code = %d body = %q", i, res.Code, res.Body.String())
		}
	}
	if got := idCalls.Load(); got != 2 {
		t.Errorf("parser called %d times across 2 requests, want 2", got)
	}
}

// TestBindParamsUUID verifies a realistic UUID parser used for a required
// parameter, including the invalid-value path.
func TestBindParamsUUID(t *testing.T) {
	var handlerCalls atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls.Add(1)
		id, ok := middleware.URLParam[string](r, "id")
		if !ok || !uuidPattern.MatchString(id) {
			t.Fatalf("id = %q, ok = %v", id, ok)
		}
		w.WriteHeader(http.StatusOK)
	})

	r := chi.NewRouter()
	binder := middleware.BindParams(middleware.RequiredURLParam("id", parseUUID))
	r.With(binder).Get("/users/{id}", handler)

	valid := doRequest(r, "/users/11111111-2222-3333-4444-555555555555")
	if valid.Code != http.StatusOK {
		t.Fatalf("valid uuid: status = %d", valid.Code)
	}
	if handlerCalls.Load() != 1 {
		t.Fatalf("handler called %d times, want 1", handlerCalls.Load())
	}

	invalid := doRequest(r, "/users/not-a-uuid")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid uuid: status = %d, want 400", invalid.Code)
	}
	if handlerCalls.Load() != 1 {
		t.Fatalf("handler should not run for invalid uuid, calls = %d", handlerCalls.Load())
	}
}

// TestBindParamsDuplicatePanic verifies registration-time validation: a
// duplicate parameter declaration panics.
func TestBindParamsDuplicatePanic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate parameter declaration")
		}
	}()
	middleware.BindParams(
		middleware.RequiredURLParam("id", middleware.IntURLParam),
		middleware.OptionalURLParam("id", middleware.IntURLParam),
	)
}

// TestBindParamsEmptyNamePanic verifies an empty parameter name is rejected at
// registration time.
func TestBindParamsEmptyNamePanic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on empty parameter name")
		}
	}()
	middleware.BindParams(middleware.RequiredURLParam("", middleware.IntURLParam))
}

// TestBindParamsNilParserPanic verifies a nil parser is rejected at
// registration time.
func TestBindParamsNilParserPanic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil parser")
		}
	}()
	middleware.BindParams(middleware.RequiredURLParam[int]("id", nil))
}

// newParamRouter builds the router exercised over a real local HTTP listener.
func newParamRouter() http.Handler {
	r := chi.NewRouter()

	itemBinder := middleware.BindParams(
		middleware.RequiredURLParam("id", middleware.IntURLParam),
		middleware.RequiredURLParam("item", parseUUID),
	)
	r.With(itemBinder).Get("/users/{id}/items/{item}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := middleware.URLParam[int](r, "id")
		item, _ := middleware.URLParam[string](r, "item")
		// Repeated reads must stay consistent and cheap.
		id2, _ := middleware.URLParam[int](r, "id")
		if id != id2 {
			http.Error(w, "inconsistent id", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "user %d item %s", id, item)
	})

	return r
}

// TestBindParamsOverHTTP drives the binder through a real local HTTP server
// (httptest.NewServer binds a TCP listener) to validate status codes, bodies
// and request isolation end to end.
func TestBindParamsOverHTTP(t *testing.T) {
	server := httptest.NewServer(newParamRouter())
	t.Cleanup(server.Close)

	client := server.Client()

	get := func(path string) (int, string) {
		resp, err := client.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp.StatusCode, string(body)
	}

	validPath := "/users/7/items/11111111-2222-3333-4444-555555555555"

	code, body := get(validPath)
	if code != http.StatusOK || body != "user 7 item 11111111-2222-3333-4444-555555555555" {
		t.Fatalf("valid: code = %d body = %q", code, body)
	}

	// Same values in a second request must remain isolated and succeed again.
	code, body = get(validPath)
	if code != http.StatusOK || body != "user 7 item 11111111-2222-3333-4444-555555555555" {
		t.Fatalf("second valid: code = %d body = %q", code, body)
	}

	// Invalid integer id: 400, no raw value echoed.
	code, body = get("/users/abc/items/11111111-2222-3333-4444-555555555555")
	if code != http.StatusBadRequest || !strings.Contains(body, "id") || strings.Contains(body, "abc") {
		t.Fatalf("bad int: code = %d body = %q", code, body)
	}

	// Parser error (bad UUID): 400, no raw value echoed.
	code, body = get("/users/7/items/not-a-uuid")
	if code != http.StatusBadRequest || !strings.Contains(body, "item") || strings.Contains(body, "not-a-uuid") {
		t.Fatalf("bad uuid: code = %d body = %q", code, body)
	}

	// Unknown route: 404.
	code, _ = get("/does-not-exist")
	if code != http.StatusNotFound {
		t.Fatalf("not found: code = %d, want 404", code)
	}
}

// TestBindParamsInvalidInt covers a non-numeric integer parameter: 400, the
// parameter name is reported, the raw value is not echoed, and the handler
// does not run.
func TestBindParamsInvalidInt(t *testing.T) {
	var handlerCalls atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls.Add(1)
	})

	r := chi.NewRouter()
	binder := middleware.BindParams(
		middleware.RequiredURLParam("id", middleware.IntURLParam),
		middleware.RequiredURLParam("item", middleware.IntURLParam),
	)
	r.With(binder).Get("/users/{id}/items/{item}", handler)

	res := doRequest(r, "/users/abc/items/2")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
	body := res.Body.String()
	if !strings.Contains(body, "id") {
		t.Errorf("error body does not name the parameter: %q", body)
	}
	if strings.Contains(body, "abc") {
		t.Errorf("error body echoes the raw parameter value: %q", body)
	}
	if handlerCalls.Load() != 0 {
		t.Fatalf("handler called %d times, want 0", handlerCalls.Load())
	}
}

// TestBindParamsParserError covers a custom parser returning an error: 400,
// handler not called, parser error text and raw value never surfaced.
func TestBindParamsParserError(t *testing.T) {
	var parserCalls, handlerCalls atomic.Int64
	failingParser := func(raw string) (string, error) {
		parserCalls.Add(1)
		return "", errors.New("boom: secret-detail-" + raw)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls.Add(1)
	})

	r := chi.NewRouter()
	binder := middleware.BindParams(middleware.RequiredURLParam("id", failingParser))
	r.With(binder).Get("/users/{id}", handler)

	res := doRequest(r, "/users/not-a-uuid")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
	body := res.Body.String()
	if strings.Contains(body, "boom") || strings.Contains(body, "secret") || strings.Contains(body, "not-a-uuid") {
		t.Errorf("error body leaks parser details/raw value: %q", body)
	}
	if parserCalls.Load() != 1 {
		t.Fatalf("parser called %d times, want 1", parserCalls.Load())
	}
	if handlerCalls.Load() != 0 {
		t.Fatalf("handler called %d times, want 0", handlerCalls.Load())
	}
}

// TestBindParamsMissingRequired verifies a required param the matched route
// does not provide yields 400 without calling the handler.
func TestBindParamsMissingRequired(t *testing.T) {
	var handlerCalls atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls.Add(1)
	})

	r := chi.NewRouter()
	binder := middleware.BindParams(
		middleware.RequiredURLParam("id", middleware.IntURLParam),
		middleware.RequiredURLParam("item", middleware.IntURLParam),
	)
	// This route only provides "id"; the binder still requires "item".
	r.With(binder).Get("/users/{id}", handler)

	res := doRequest(r, "/users/7")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %q", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "item") {
		t.Fatalf("error body does not name missing parameter: %q", res.Body.String())
	}
	if handlerCalls.Load() != 0 {
		t.Fatalf("handler called %d times, want 0", handlerCalls.Load())
	}
}

// TestBindParamsNotFound ensures unmatched routes get the normal 404 and never
// reach the binder or the handler.
func TestBindParamsNotFound(t *testing.T) {
	var parserCalls, handlerCalls atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls.Add(1)
	})

	r := chi.NewRouter()
	binder := middleware.BindParams(
		middleware.RequiredURLParam("id", countingParser(&parserCalls, parseInt)),
	)
	r.With(binder).Get("/users/{id}", handler)

	res := doRequest(r, "/nope")
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.Code)
	}
	if parserCalls.Load() != 0 {
		t.Fatalf("parser called %d times, want 0", parserCalls.Load())
	}
	if handlerCalls.Load() != 0 {
		t.Fatalf("handler called %d times, want 0", handlerCalls.Load())
	}
}

// TestBindParamsMount verifies parameters captured inside a Mount-ed sub-router
// are available to a route-scoped binder in the sub-router.
func TestBindParamsMount(t *testing.T) {
	var handlerCalls atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls.Add(1)
		id, _ := middleware.URLParam[int](r, "id")
		item, _ := middleware.URLParam[int](r, "item")
		fmt.Fprintf(w, "id=%d item=%d", id, item)
	})

	sub := chi.NewRouter()
	binder := middleware.BindParams(
		middleware.RequiredURLParam("id", middleware.IntURLParam),
		middleware.RequiredURLParam("item", middleware.IntURLParam),
	)
	sub.With(binder).Get("/users/{id}/items/{item}", handler)

	r := chi.NewRouter()
	r.Mount("/api", sub)

	res := doRequest(r, "/api/users/42/items/99")
	if res.Code != http.StatusOK || res.Body.String() != "id=42 item=99" {
		t.Fatalf("code = %d, body = %q", res.Code, res.Body.String())
	}
	if handlerCalls.Load() != 1 {
		t.Fatalf("handler called %d times, want 1", handlerCalls.Load())
	}
}
