package chi

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// URLBuilder builds URL paths from registered route templates.
//
// A URLBuilder is safe for concurrent use after construction. It validates
// parameter values against the routing tree and verifies the built path
// matches the intended route without executing handlers or making network
// requests.
type URLBuilder struct {
	mux *Mux
}

// NewURLBuilder returns a new URLBuilder for the given router.
//
// The builder uses the router's registered routes to validate and build URLs.
// It does not execute handlers or make network requests.
func NewURLBuilder(r Router) *URLBuilder {
	mux, ok := r.(*Mux)
	if !ok {
		panic("chi: NewURLBuilder requires a *Mux router")
	}
	return &URLBuilder{mux: mux}
}

// Build builds a URL path from a route pattern and parameter values.
//
// The pattern must match a registered route. Parameter values are encoded
// per path segment: slashes in values for {param} placeholders are
// percent-encoded (%2F), while wildcard (*) placeholders allow slashes to
// pass through as path separators.
//
// Regular expression constraints on {param} placeholders are checked against
// the provided values. Missing or extra parameters result in an error.
//
// After building, the URL is verified against the router's routing tree to
// confirm it matches the intended route, without executing the handler.
func (b *URLBuilder) Build(pattern string, params map[string]string) (string, error) {
	// Find the registered route and its supported methods.
	methods, err := b.routeMethods(pattern)
	if err != nil {
		return "", err
	}

	// Build the URL path from the pattern and parameter values.
	path, err := buildPath(pattern, params)
	if err != nil {
		return "", err
	}

	// Verify the built URL matches the intended route without executing
	// the handler. We try each method the route supports.
	for _, method := range methods {
		rctx := NewRouteContext()
		matched := b.mux.Find(rctx, method, path)
		if matched == pattern {
			return path, nil
		}
	}

	return "", fmt.Errorf("chi: built URL %q does not match route %q", path, pattern)
}

// routeMethods returns the HTTP methods supported by the route matching the
// given pattern. It returns an error if the route is not registered.
func (b *URLBuilder) routeMethods(pattern string) ([]string, error) {
	route, err := b.findRoute(pattern)
	if err != nil {
		return nil, err
	}

	methods := make([]string, 0, len(route.Handlers))
	for method := range route.Handlers {
		if method == "*" {
			// The route matches all methods; use GET for verification.
			return []string{http.MethodGet}, nil
		}
		methods = append(methods, method)
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("chi: route %q has no handlers", pattern)
	}
	return methods, nil
}

// findRoute finds the registered route matching the given pattern. It recurses
// into mounted subrouters to find sub-routes.
func (b *URLBuilder) findRoute(pattern string) (*Route, error) {
	for _, route := range b.mux.Routes() {
		if route.Pattern == pattern {
			return &route, nil
		}

		// Recurse into mounted subrouters. A mount point pattern ends with
		// "/*" (e.g. "/api/*"). We match the prefix and recurse with the
		// remaining pattern.
		if route.SubRoutes != nil && strings.HasSuffix(route.Pattern, "/*") {
			prefix := strings.TrimSuffix(route.Pattern, "/*")
			if strings.HasPrefix(pattern, prefix+"/") {
				subPattern := pattern[len(prefix):]
				subBuilder := &URLBuilder{mux: route.SubRoutes.(*Mux)}
				subRoute, err := subBuilder.findRoute(subPattern)
				if err == nil {
					// Combine the mount prefix with the sub-route pattern.
					combined := prefix + subRoute.Pattern
					return &Route{
						SubRoutes: subRoute.SubRoutes,
						Handlers:  subRoute.Handlers,
						Pattern:   combined,
					}, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("chi: route %q not found", pattern)
}

// buildPath builds a URL path from a pattern and parameter values.
//
// Parameter values are encoded per path segment. Wildcard values are appended
// as-is, allowing slashes to act as path separators.
func buildPath(pattern string, params map[string]string) (string, error) {
	// Make a copy of params to detect extra parameters.
	remaining := make(map[string]string, len(params))
	for k, v := range params {
		remaining[k] = v
	}

	var buf strings.Builder
	pat := pattern
	for {
		ptyp, key, rexpat, _, segStart, segEnd := patNextSegment(pat)
		if ptyp == ntStatic {
			buf.WriteString(pat)
			break
		}

		// Write the static part before the param.
		buf.WriteString(pat[:segStart])

		// Get the param value.
		value, ok := remaining[key]
		if !ok {
			return "", fmt.Errorf("chi: missing parameter %q for route %q", key, pattern)
		}
		delete(remaining, key)

		// Validate regexp constraint against the original (unencoded) value.
		if ptyp == ntRegexp {
			rex, err := regexp.Compile(rexpat)
			if err != nil {
				return "", fmt.Errorf("chi: invalid regexp %q for parameter %q: %w", rexpat, key, err)
			}
			if !rex.MatchString(value) {
				return "", fmt.Errorf("chi: parameter %q value %q does not match regexp %q", key, value, rexpat)
			}
		}

		// Encode the value per path segment.
		if ptyp == ntCatchAll {
			// Wildcard: append as-is, allowing slashes as path separators.
			buf.WriteString(value)
		} else {
			// Param/regexp: percent-encode the value so slashes and other
			// special characters are treated as part of a single segment.
			buf.WriteString(url.PathEscape(value))
		}

		pat = pat[segEnd:]
	}

	// Check for extra parameters.
	if len(remaining) > 0 {
		keys := make([]string, 0, len(remaining))
		for k := range remaining {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return "", fmt.Errorf("chi: extra parameters %v for route %q", keys, pattern)
	}

	return buf.String(), nil
}
