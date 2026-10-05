package chi

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Errors returned by Mux.BuildURL. Use errors.Is to test for them.
var (
	// ErrRoutePatternNotFound is returned when the requested route pattern
	// is not registered on the mux.
	ErrRoutePatternNotFound = errors.New("chi: route pattern is not registered on this mux")

	// ErrURLMissingParam is returned when the route pattern declares a
	// parameter for which no value was supplied.
	ErrURLMissingParam = errors.New("chi: missing value for URL parameter")

	// ErrURLExtraParam is returned when a supplied value has no matching
	// parameter in the route pattern.
	ErrURLExtraParam = errors.New("chi: no matching parameter in route pattern")

	// ErrURLParamConstraint is returned when a value does not satisfy the
	// regexp constraint declared by the route pattern, e.g. {id:[0-9]+}.
	ErrURLParamConstraint = errors.New("chi: value does not satisfy the route pattern regexp")

	// ErrURLIdentityMismatch is returned when the built URL does not route
	// back to the same pattern it was built from.
	ErrURLIdentityMismatch = errors.New("chi: built URL does not route back to the same pattern")
)

// BuildURL builds a URL path for a route registered on this mux, substituting
// the given parameter values into the route's pattern.
//
// The pattern must be registered on this mux exactly as passed (e.g. via
// Get/Post/Handle/...), otherwise ErrRoutePatternNotFound is returned.
//
// Parameter semantics:
//
//   - "{name}" placeholders are single path segments. The value is
//     percent-encoded as one segment, so an embedded "/" becomes "%2F" and
//     can never be reinterpreted as an additional path segment.
//   - "{name:regexp}" placeholders additionally validate the raw value
//     against the pattern's own regexp constraint (anchored); a mismatch is
//     rejected with ErrURLParamConstraint.
//   - The trailing wildcard "*" accepts a multi-segment tail value under the
//     key "*"; each segment is percent-encoded individually and the segments
//     are joined with "/".
//
// Every declared parameter must be supplied (ErrURLMissingParam) and every
// supplied value must be consumed (ErrURLExtraParam).
//
// Before returning, the built path is matched against this mux's own routing
// tree (see Mux.Find) to confirm it resolves back to the very same pattern.
// This identity check only walks the routing tree: it never executes any
// handler or middleware and performs no network I/O.
func (mx *Mux) BuildURL(pattern string, params map[string]string) (string, error) {
	method, err := mx.routeMethodForPattern(pattern)
	if err != nil {
		return "", err
	}

	path, err := buildRoutePath(pattern, params)
	if err != nil {
		return "", err
	}

	// Identity check: route the built path through this mux's own routing
	// tree. Find only resolves the matching route pattern into the fresh
	// context; it never executes handlers, middleware or any network I/O.
	rctx := NewRouteContext()
	if matched := mx.Find(rctx, method, path); matched != pattern {
		return "", fmt.Errorf("%w: built %q for pattern %q but it matches %q",
			ErrURLIdentityMismatch, path, pattern, matched)
	}
	return path, nil
}

// routeMethodForPattern returns an HTTP method registered for the exact route
// pattern on this mux, used to drive the identity check in BuildURL.
func (mx *Mux) routeMethodForPattern(pattern string) (string, error) {
	for _, r := range mx.Routes() {
		if r.Pattern != pattern {
			continue
		}
		methods := make([]string, 0, len(r.Handlers))
		for m := range r.Handlers {
			if m != "*" {
				methods = append(methods, m)
			}
		}
		if len(methods) == 0 {
			if _, ok := r.Handlers["*"]; ok {
				// Registered via Handle: every method is served.
				return http.MethodGet, nil
			}
			continue // subrouter stub without its own endpoint
		}
		sort.Strings(methods)
		return methods[0], nil
	}
	return "", fmt.Errorf("%w: %q", ErrRoutePatternNotFound, pattern)
}

// buildRoutePath substitutes params into pattern, encoding each value per
// path segment and validating regexp constraints against the raw values.
func buildRoutePath(pattern string, params map[string]string) (string, error) {
	var sb strings.Builder
	sb.Grow(len(pattern) + 16)

	used := make(map[string]struct{}, len(params))

	pat := pattern
	for {
		ntyp, key, rexpat, _, start, end := patNextSegment(pat)
		if ntyp == ntStatic {
			sb.WriteString(pat)
			break
		}
		sb.WriteString(pat[:start])

		val, ok := params[key]
		if !ok {
			return "", fmt.Errorf("%w: %q", ErrURLMissingParam, key)
		}
		used[key] = struct{}{}

		if ntyp == ntCatchAll {
			// Wildcard tail: the value may span multiple path segments;
			// encode each segment and keep "/" as the segment separator.
			sb.WriteString(escapePathSegments(val))
			break
		}

		if ntyp == ntRegexp {
			rex, err := regexp.Compile(rexpat)
			if err != nil {
				return "", fmt.Errorf("chi: invalid regexp in route pattern %q: %w", pattern, err)
			}
			if !rex.MatchString(val) {
				return "", fmt.Errorf("%w: parameter %q value %q does not match %q",
					ErrURLParamConstraint, key, val, rexpat)
			}
		}

		// Single path segment: escape so an embedded "/" stays inside the
		// segment ("%2F") and is never treated as a new segment.
		sb.WriteString(url.PathEscape(val))
		pat = pat[end:]
	}

	if len(used) != len(params) {
		extra := make([]string, 0, len(params)-len(used))
		for k := range params {
			if _, ok := used[k]; !ok {
				extra = append(extra, k)
			}
		}
		sort.Strings(extra)
		return "", fmt.Errorf("%w: %q", ErrURLExtraParam, strings.Join(extra, "\", \""))
	}

	return sb.String(), nil
}

// escapePathSegments percent-encodes each "/"-separated segment of s,
// preserving the segment separators themselves.
func escapePathSegments(s string) string {
	segs := strings.Split(s, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}
