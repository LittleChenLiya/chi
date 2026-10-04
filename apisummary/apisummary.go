// Package apisummary produces a read-only summary of the HTTP methods
// registered on a chi.Router, for API inspection and tooling.
//
// The summary is built solely from the routing tree exposed by
// [chi.Routes] — the same traversal mechanism [chi.Walk] uses. No HTTP
// requests are sent and no route handlers are ever executed, so the
// result is safe to generate from analysis tooling without side effects.
//
// # Traversal
//
// Registered routes are traversed along the tree returned by
// [chi.Routes], following [chi.Mount] semantics: mounted chi routers are
// expanded recursively under their mount prefix, and the original path
// parameter segments (for example "/v1/items/{id}" or
// "/users/{id:[0-9]+}") are preserved in full.
//
// Mounted handlers that do not implement [chi.Routes] (a plain
// [http.Handler] attached through [chi.Router.Mount], or a catch-all
// [chi.Router.Handle] registration) cannot be introspected. Rather than
// being silently dropped, such entries are reported with
// [Endpoint.Unexpanded] set to true and their mount pattern as
// registered.
//
// # Methods
//
// Methods are merged per path template: an endpoint registered with both
// Get and Post reports both methods under the same pattern. Templates
// that differ only in parameter names (for example "/items/{id}" versus
// "/items/{itemID}") are kept as separate endpoints — they are never
// normalized or hard-merged.
//
// Only methods explicitly registered on the router are reported. In
// particular, a route registered with Get is reported as GET only: a
// HEAD request that chi serves implicitly through the GetHead
// middleware is not recorded as an explicit HEAD registration, and the
// summary never invents methods that were not registered.
//
// The output is stable: endpoints are sorted by pattern and then by
// method set.
package apisummary

import (
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Endpoint is a single summarized route.
type Endpoint struct {
	// Pattern is the full path template of the route, including any mount
	// prefixes and the original parameter segments, e.g.
	// "/v1/items/{id}".
	Pattern string

	// Methods are the explicitly registered HTTP methods for Pattern,
	// sorted alphabetically. A catch-all registration (Router.Handle or
	// Router.HandleFunc) is reported as "*". Methods that chi serves only
	// implicitly — for example HEAD routed to a GET handler through the
	// GetHead middleware — are never included.
	Methods []string

	// Unexpanded reports that Pattern is a mounted handler which does not
	// implement chi.Routes, so its sub-routes cannot be inspected. The
	// endpoint is reported with method "*" and the mount pattern as
	// registered.
	Unexpanded bool
}

// Summary is the read-only result of inspecting a router.
type Summary struct {
	// Endpoints are the summarized routes, sorted by Pattern and then by
	// method set for stable output.
	Endpoints []Endpoint
}

// Generate inspects r and returns a stable, read-only method summary.
// It never sends a request or invokes a handler.
func Generate(r chi.Routes) *Summary {
	s := &Summary{}
	s.walk(r, "")
	sort.SliceStable(s.Endpoints, func(i, j int) bool {
		if s.Endpoints[i].Pattern != s.Endpoints[j].Pattern {
			return s.Endpoints[i].Pattern < s.Endpoints[j].Pattern
		}
		return strings.Join(s.Endpoints[i].Methods, ",") < strings.Join(s.Endpoints[j].Methods, ",")
	})
	return s
}

// walk mirrors the recursion of chi.walk (used by chi.Walk) over the
// Routes tree, expanding mounted chi routers under their prefix and
// flagging mounted handlers that cannot be expanded.
func (s *Summary) walk(r chi.Routes, prefix string) {
	for _, route := range r.Routes() {
		full := prefix + route.Pattern
		full = strings.ReplaceAll(full, "/*/", "/")

		if route.SubRoutes != nil {
			// Mounted chi.Router: expand it under the mount prefix.
			s.walk(route.SubRoutes, full)
			continue
		}

		if route.Handlers["*"] != nil && strings.HasSuffix(route.Pattern, "/*") {
			// A mounted handler that does not implement chi.Routes (or a
			// catch-all Handle registration) cannot be introspected.
			s.Endpoints = append(s.Endpoints, Endpoint{
				Pattern:    full,
				Methods:    []string{"*"},
				Unexpanded: true,
			})
			continue
		}

		s.Endpoints = append(s.Endpoints, Endpoint{
			Pattern: full,
			Methods: methodsOf(route.Handlers),
		})
	}
}

// methodsOf extracts the explicitly registered methods from a route's
// handler map. A catch-all registration ("*") covers every method, so it
// is reported as "*" together with any specific method whose handler
// overrides the catch-all handler.
func methodsOf(handlers map[string]http.Handler) []string {
	methods := make([]string, 0, len(handlers))
	wildcard := handlers["*"]
	for m, h := range handlers {
		if m == "*" || h == nil {
			continue
		}
		methods = append(methods, m)
	}
	sort.Strings(methods)

	if wildcard != nil {
		out := []string{"*"}
		for _, m := range methods {
			if !sameHandler(wildcard, handlers[m]) {
				out = append(out, m)
			}
		}
		return out
	}
	return methods
}

// sameHandler reports whether a and b reference the same handler value.
// Handlers are commonly funcs (http.HandlerFunc), which cannot be
// compared directly, so funcs are compared by pointer instead.
func sameHandler(a, b http.Handler) bool {
	if a == nil || b == nil {
		return a == b
	}
	av := reflect.ValueOf(a)
	bv := reflect.ValueOf(b)
	if av.Type() != bv.Type() {
		return false
	}
	if av.Kind() == reflect.Func {
		return av.Pointer() == bv.Pointer()
	}
	if av.Type().Comparable() {
		return a == b
	}
	return false
}

// String renders the summary as a human-readable report.
func (s *Summary) String() string {
	var b strings.Builder
	s.WriteTo(&b)
	return b.String()
}

// WriteTo writes a stable, human-readable report of the summary to w.
// It implements [io.WriterTo].
func (s *Summary) WriteTo(w io.Writer) (int64, error) {
	var b strings.Builder
	b.WriteString("API route summary (read-only; no requests sent, no handlers executed)\n")
	if len(s.Endpoints) == 0 {
		b.WriteString("  (no routes registered)\n")
	} else {
		b.WriteString("  METHODS  PATTERN\n")
		for _, e := range s.Endpoints {
			if e.Unexpanded {
				fmt.Fprintf(&b, "  %-8s %s  [unexpanded: mounted handler does not implement chi.Routes]\n",
					strings.Join(e.Methods, ","), e.Pattern)
				continue
			}
			fmt.Fprintf(&b, "  %-8s %s\n", strings.Join(e.Methods, ","), e.Pattern)
		}
	}
	n, err := io.WriteString(w, b.String())
	return int64(n), err
}
