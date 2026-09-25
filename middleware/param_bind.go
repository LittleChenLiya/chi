package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// BoundParamsCtxKey is the context.Context key used to store the URL
// parameters bound by the BindParams middleware for a request.
var BoundParamsCtxKey = &contextKey{"BoundParams"}

// ParamParser parses the raw string value of a URL parameter into a
// typed value. It is called at most once per request and parameter by
// the BindParams middleware.
type ParamParser[T any] func(raw string) (T, error)

// ParamDecl declares a single URL parameter to bind for a route.
// Create declarations with Param and OptionalParam.
type ParamDecl struct {
	name     string
	optional bool
	parse    func(raw string) (any, error)
}

// Param declares a required URL parameter. Requests reach the next
// handler only if the parameter is present in the matched route and
// parses successfully.
func Param[T any](name string, parser ParamParser[T]) ParamDecl {
	return ParamDecl{
		name: name,
		parse: func(raw string) (any, error) {
			v, err := parser(raw)
			if err != nil {
				return nil, err
			}
			return v, nil
		},
	}
}

// OptionalParam declares an optional URL parameter. If the parameter is
// absent from the matched route, no value is stored for it and no zero
// value is inserted; ParamValue will report it as not provided. If the
// parameter is present but fails to parse, the request is rejected.
func OptionalParam[T any](name string, parser ParamParser[T]) ParamDecl {
	decl := Param(name, parser)
	decl.optional = true
	return decl
}

// BindParams returns a middleware that parses declared URL parameters
// before the next handler runs and stores the typed values in the
// request context. Each parser is called exactly once per request; the
// cached values can be read any number of times with ParamValue.
//
// The middleware reads chi.URLParam at request time, so parameters are
// bound from the final matched route, including parameters captured by
// parent routes of nested routers and Mount patterns. Attach it to the
// leaf route with Router.With (or inside Route/Group) so it runs after
// routing has completed.
//
// If a required parameter is missing or any parameter fails to parse,
// the middleware responds with 400 Bad Request naming the parameter
// (without echoing its raw value) and the next handler is not called.
//
// BindParams panics if a parameter name is declared twice or is empty.
//
// Sample usage:
//
//	r := chi.NewRouter()
//	r.With(middleware.BindParams(
//		middleware.Param("userID", middleware.ParseInt),
//		middleware.OptionalParam("itemID", middleware.ParseInt64),
//	)).Get("/users/{userID}/items/{itemID}", handler)
//
//	func handler(w http.ResponseWriter, r *http.Request) {
//		userID, _ := middleware.ParamValue[int](r.Context(), "userID")
//		itemID, ok := middleware.ParamValue[int64](r.Context(), "itemID")
//		_ = ok // false when the optional parameter was not provided
//		_ = userID
//	}
func BindParams(decls ...ParamDecl) func(next http.Handler) http.Handler {
	seen := make(map[string]struct{}, len(decls))
	for _, decl := range decls {
		if decl.name == "" {
			panic("middleware: BindParams parameter name must not be empty")
		}
		if decl.parse == nil {
			panic(fmt.Sprintf("middleware: BindParams parameter %q has no parser", decl.name))
		}
		if _, ok := seen[decl.name]; ok {
			panic(fmt.Sprintf("middleware: BindParams duplicate parameter %q", decl.name))
		}
		seen[decl.name] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bound := make(map[string]any, len(decls))
			for _, decl := range decls {
				raw := chi.URLParam(r, decl.name)
				if raw == "" {
					if decl.optional {
						continue
					}
					http.Error(w, fmt.Sprintf("missing required URL parameter %q", decl.name), http.StatusBadRequest)
					return
				}
				v, err := decl.parse(raw)
				if err != nil {
					http.Error(w, fmt.Sprintf("invalid URL parameter %q", decl.name), http.StatusBadRequest)
					return
				}
				bound[decl.name] = v
			}
			ctx := context.WithValue(r.Context(), BoundParamsCtxKey, bound)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ParamValue returns the bound value of the named URL parameter from
// the request context. The boolean reports whether a value was bound;
// it is false when the parameter was not declared, was optional and
// absent, or T does not match the declared value type. The parse
// function is never called here; values are parsed once by BindParams.
func ParamValue[T any](ctx context.Context, name string) (T, bool) {
	var zero T
	bound, _ := ctx.Value(BoundParamsCtxKey).(map[string]any)
	if bound == nil {
		return zero, false
	}
	v, ok := bound[name]
	if !ok {
		return zero, false
	}
	typed, ok := v.(T)
	if !ok {
		return zero, false
	}
	return typed, true
}

// ParseInt is a ParamParser that parses a URL parameter as an int.
func ParseInt(raw string) (int, error) {
	return strconv.Atoi(raw)
}

// ParseInt64 is a ParamParser that parses a URL parameter as an int64.
func ParseInt64(raw string) (int64, error) {
	return strconv.ParseInt(raw, 10, 64)
}

// ParseUUID is a ParamParser that validates a URL parameter as a
// canonical UUID string (8-4-4-4-12 hexadecimal digits) and returns it
// unchanged.
func ParseUUID(raw string) (string, error) {
	if len(raw) != 36 {
		return "", fmt.Errorf("invalid UUID length")
	}
	for i := 0; i < 36; i++ {
		c := raw[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return "", fmt.Errorf("invalid UUID format")
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return "", fmt.Errorf("invalid UUID format")
			}
		}
	}
	return raw, nil
}
