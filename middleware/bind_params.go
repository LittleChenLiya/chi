package middleware

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// ParseURLParam parses the raw string value captured for a URL parameter into
// a value of type T. Returning a non-nil error marks the parameter invalid;
// the error text is never written to the response so that the original
// parameter value cannot leak back to the client.
type ParseURLParam[T any] func(raw string) (T, error)

// URLParamSpec is the declaration of a single URL parameter. Concrete
// instances are created with RequiredURLParam and OptionalURLParam.
type URLParamSpec interface {
	name() string
	optional() bool
	hasParser() bool
	parse(raw string) (any, error)
}

type urlParamSpec[T any] struct {
	paramName  string
	isOptional bool
	parseFn    ParseURLParam[T]
}

func (s urlParamSpec[T]) name() string { return s.paramName }

func (s urlParamSpec[T]) optional() bool { return s.isOptional }

func (s urlParamSpec[T]) hasParser() bool { return s.parseFn != nil }

func (s urlParamSpec[T]) parse(raw string) (any, error) {
	return s.parseFn(raw)
}

// RequiredURLParam declares a mandatory URL parameter. When the matched route
// does not provide the parameter, the binder responds with 400 and does not
// run the next handler.
func RequiredURLParam[T any](paramName string, parse ParseURLParam[T]) URLParamSpec {
	return urlParamSpec[T]{paramName: paramName, parseFn: parse}
}

// OptionalURLParam declares an optional URL parameter. When the parameter is
// absent from the matched route the parameter is left unprovided; no zero value
// is ever stored. When the parameter is present but fails to parse, the binder
// responds with 400 just like a required parameter.
func OptionalURLParam[T any](paramName string, parse ParseURLParam[T]) URLParamSpec {
	return urlParamSpec[T]{paramName: paramName, isOptional: true, parseFn: parse}
}

// IntURLParam is a convenience parser for base-10 integer parameters.
func IntURLParam(raw string) (int, error) {
	return strconv.Atoi(raw)
}

// boundURLParams is the per-request result of a BindParams middleware. Every
// request gets a fresh instance through the request context, so parsed values
// never leak between requests that share the same raw value.
type boundURLParams struct {
	// values holds the successfully parsed values keyed by parameter name.
	values map[string]any
}

var boundURLParamsCtxKey = &contextKey{"BoundURLParams"}

// BindParams returns a middleware that parses the declared URL parameters once
// the final route (including nested routers and Mount points) has been matched.
// Successfully parsed values are stored on the request context and can be read
// repeatedly with URLParam without invoking the parser more than once.
//
// Duplicate parameter names, empty names and nil parsers panic at registration
// time.
//
// Because parameters contributed by nested routers and Mount points only exist
// after the complete route is matched, BindParams must be attached as a
// route-scoped middleware, i.e. through Router.With / Router.Group or directly
// around a handler, never through a parent Router.Use that runs before the
// nested routes it declares are matched. For example:
//
//	r.Route("/users/{userID}", func(r chi.Router) {
//		binder := middleware.BindParams(
//			middleware.RequiredURLParam("userID", middleware.IntURLParam),
//		)
//		r.With(binder).Get("/items/{itemID}", itemHandler)
//	})
//
// If a required parameter is missing or any present parameter fails to parse,
// the middleware responds with HTTP 400, names the offending parameter and
// does not run the next handler. The raw parameter value and the parser error
// are deliberately omitted from the response.
func BindParams(specs ...URLParamSpec) func(http.Handler) http.Handler {
	seen := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		specName := spec.name()
		if specName == "" {
			panic("chi/middleware: BindParams requires a non-empty parameter name")
		}
		if !spec.hasParser() {
			panic("chi/middleware: BindParams requires a non-nil parser for parameter '" + specName + "'")
		}
		if _, dup := seen[specName]; dup {
			panic("chi/middleware: BindParams parameter '" + specName + "' is declared more than once")
		}
		seen[specName] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bound := &boundURLParams{values: make(map[string]any, len(specs))}

			for _, spec := range specs {
				raw, provided := lookupURLParam(r, spec.name())
				if !provided {
					if !spec.optional() {
						missingURLParam(w, spec.name())
						return
					}
					// Optional and absent: leave it unprovided rather than storing a zero value.
					continue
				}

				value, err := spec.parse(raw)
				if err != nil {
					invalidURLParam(w, spec.name())
					return
				}
				bound.values[spec.name()] = value
			}

			r = r.WithContext(context.WithValue(r.Context(), boundURLParamsCtxKey, bound))
			next.ServeHTTP(w, r)
		})
	}
}

// lookupURLParam reports whether a parameter with the given name was captured
// by the matched route, together with its raw value. The key stack is scanned
// backwards so the most specific (deepest) sub-router wins, matching the
// behaviour of chi.URLParam.
func lookupURLParam(r *http.Request, key string) (string, bool) {
	rctx := chi.RouteContext(r.Context())
	if rctx == nil {
		return "", false
	}
	keys := rctx.URLParams.Keys
	for i := len(keys) - 1; i >= 0; i-- {
		if keys[i] == key {
			return rctx.URLParams.Values[i], true
		}
	}
	return "", false
}

// URLParam returns the bound value of a URL parameter from the request.
//
// The parser is invoked at most once per request by BindParams; repeated calls
// only read the cached value. ok is false when the parameter is an unprovided
// optional parameter, when it was never declared to BindParams, or when the
// stored value is not of type T. A failed or missing required parameter never
// reaches the handler because BindParams responds with 400 first.
func URLParam[T any](r *http.Request, name string) (value T, ok bool) {
	return URLParamFromContext[T](r.Context(), name)
}

// URLParamFromContext returns the bound value of a URL parameter from a
// context.Context. It behaves like URLParam and is useful when only the context
// is available.
func URLParamFromContext[T any](ctx context.Context, name string) (value T, ok bool) {
	bound, _ := ctx.Value(boundURLParamsCtxKey).(*boundURLParams)
	if bound == nil {
		return value, false
	}
	raw, exists := bound.values[name]
	if !exists {
		return value, false
	}
	value, ok = raw.(T)
	return value, ok
}

func missingURLParam(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	w.Write([]byte("missing required url parameter: " + safeParamName(name) + "\n"))
}

func invalidURLParam(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	w.Write([]byte("invalid url parameter: " + safeParamName(name) + "\n"))
}

// safeParamName prevents parameter names from injecting header or response
// framing through CR/LF characters. The parameter name is registered by the
// server itself, but escaping keeps the generated message well-formed.
func safeParamName(name string) string {
	name = strings.ReplaceAll(name, "\r", "%0d")
	name = strings.ReplaceAll(name, "\n", "%0a")
	return strconv.Quote(name)
}
