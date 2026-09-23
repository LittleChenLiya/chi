package chi

import (
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

// Route explanation statuses reported by RouteExplanation.Status.
const (
	ExplainMatched          = "matched"
	ExplainMethodNotAllowed = "method-not-allowed"
	ExplainNotFound         = "not-found"
	ExplainInvalidMethod    = "invalid-method"
)

// RouteExplanation describes how a request would be routed, without
// executing any handler or middleware. It is produced by Mux.ExplainRoute.
type RouteExplanation struct {
	// Method and Path are the explained request target.
	Method string
	Path   string

	// Status is one of the Explain* constants.
	Status string

	// Pattern is the canonical routing pattern that matched, including any
	// Mount/Route prefixes of parent routers.
	Pattern string

	// MatchKind classifies the matched pattern: "static", "param",
	// "regexp" or "wildcard".
	MatchKind string

	// Params holds the URL parameters captured for this explanation only.
	// On duplicate keys the last capture wins, mirroring Context.URLParam.
	Params map[string]string

	// AllowedMethods lists the methods that would be accepted for the
	// matched path when Status is ExplainMethodNotAllowed.
	AllowedMethods []string

	// Middlewares lists the effective middleware chain for the matched
	// route, from the outermost Mux down to inline With/Group middlewares.
	Middlewares []string

	// Steps records the routing tree decision steps in traversal order.
	Steps []string
}

// ExplainRoute reports which route a request with the given method and path
// would match, following the same tree lookup order as ServeHTTP. It never
// executes handlers or middlewares, uses a private routing context, and does
// not affect subsequent ServeHTTP behavior.
func (mx *Mux) ExplainRoute(method, path string) *RouteExplanation {
	ex := &RouteExplanation{
		Method: method,
		Path:   path,
		Status: ExplainNotFound,
		Params: map[string]string{},
	}
	m, ok := methodMap[strings.ToUpper(method)]
	if !ok {
		ex.Status = ExplainInvalidMethod
		return ex
	}
	if path == "" {
		path = "/"
	}

	rctx := NewRouteContext()
	rctx.trace = &ex.Steps

	pattern, ok := mx.explain(rctx, m, path, ex)
	if ok {
		ex.Status = ExplainMatched
		ex.Pattern = pattern
		ex.MatchKind = explainMatchKind(pattern)
		for i, k := range rctx.URLParams.Keys {
			ex.Params[k] = rctx.URLParams.Values[i]
		}
		return ex
	}

	if rctx.methodNotAllowed {
		ex.Status = ExplainMethodNotAllowed
		for _, mt := range rctx.methodsAllowed {
			if name, ok := reverseMethodMap[mt]; ok {
				ex.AllowedMethods = append(ex.AllowedMethods, name)
			}
		}
		slices.Sort(ex.AllowedMethods)
		ex.AllowedMethods = slices.Compact(ex.AllowedMethods)
	}
	return ex
}

// explain mirrors Mux.Find, recursing into mounted subrouters while
// recording middleware layers and decision steps. It executes nothing.
func (mx *Mux) explain(rctx *Context, method methodTyp, path string, ex *RouteExplanation) (string, bool) {
	for _, mw := range mx.middlewares {
		ex.Middlewares = append(ex.Middlewares, middlewareName(mw))
	}

	node, _, _ := mx.tree.FindRoute(rctx, method, path)
	if node == nil {
		return "", false
	}

	pattern := rctx.routePattern

	if node.subroutes == nil {
		e := node.endpoints[method]
		if ch, ok := e.handler.(*ChainHandler); ok {
			for _, mw := range ch.Middlewares {
				ex.Middlewares = append(ex.Middlewares, middlewareName(mw))
			}
		}
		return e.pattern, true
	}

	// Mounted subrouter: mirror the mount handler's bookkeeping (wildcard
	// param reset and route path shift) without executing it.
	ex.Steps = append(ex.Steps, fmt.Sprintf("mount: descend into subrouter matched by %q", pattern))

	n := len(rctx.URLParams.Keys) - 1
	if n >= 0 && rctx.URLParams.Keys[n] == "*" && len(rctx.URLParams.Values) > n {
		rctx.URLParams.Values[n] = ""
	}
	rctx.RoutePath = mx.nextRoutePath(rctx)

	subMux, ok := node.subroutes.(*Mux)
	if !ok {
		// A non-chi handler is mounted here; the mount itself matched.
		return pattern, true
	}

	subPattern, ok := subMux.explain(rctx, method, rctx.RoutePath, ex)
	if !ok {
		return "", false
	}

	pattern = strings.TrimSuffix(pattern, "/*")
	pattern += subPattern
	return pattern, true
}

// explainMatchKind classifies a matched pattern by its most dynamic segment.
func explainMatchKind(pattern string) string {
	if strings.Contains(pattern, "*") {
		return "wildcard"
	}
	if strings.Contains(pattern, "{") {
		if strings.Contains(pattern, ":") {
			return "regexp"
		}
		return "param"
	}
	return "static"
}

// middlewareName returns a stable, human-readable name for a middleware.
func middlewareName(mw func(http.Handler) http.Handler) string {
	name := runtime.FuncForPC(reflect.ValueOf(mw).Pointer()).Name()
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// tracef appends a routing decision step when tracing is enabled.
func (x *Context) tracef(format string, args ...any) {
	if x.trace != nil {
		*x.trace = append(*x.trace, fmt.Sprintf(format, args...))
	}
}

// Shadow report kinds reported by ShadowReport.Kind.
const (
	// ShadowDuplicate reports an identical method+pattern registered more
	// than once; the later registration silently replaces the earlier one.
	ShadowDuplicate = "duplicate"
	// ShadowOverlap reports two patterns that can match a common path; the
	// report includes a concrete example path when one can be determined.
	ShadowOverlap = "overlap"
	// ShadowUnknown reports a potential overlap that cannot be decided
	// statically, e.g. the containment of two arbitrary regexps. It never
	// asserts that two patterns are disjoint.
	ShadowUnknown = "unknown"
)

// ShadowReport describes a pair of registered routes whose patterns may
// compete for the same request path.
type ShadowReport struct {
	Kind    string   // one of the Shadow* constants
	Methods []string // shared methods of the two registrations
	Pattern string   // the earlier registration
	Other   string   // the later registration
	Example string   // a concrete path matching both, when determinable
	Note    string
}

// CheckShadowing statically analyzes the registered routes of the mux and
// its mounted subrouters, reporting duplicate method+pattern registrations
// and overlapping patterns (with concrete example paths where decidable).
// Overlaps that depend on arbitrary regexp containment are reported as
// ShadowUnknown rather than assumed disjoint.
func (mx *Mux) CheckShadowing() []ShadowReport {
	var regs []shadowReg
	mx.collectShadowRegs("", &regs)

	var reports []ShadowReport
	for i := 0; i < len(regs); i++ {
		for j := i + 1; j < len(regs); j++ {
			shared := regs[i].methods & regs[j].methods
			if shared == 0 {
				continue
			}
			rel, example, note := compareRoutePatterns(regs[i].pattern, regs[j].pattern)
			if rel == "disjoint" {
				continue
			}
			kind := rel
			if rel == "equal" {
				kind = ShadowDuplicate
			}
			reports = append(reports, ShadowReport{
				Kind:    kind,
				Methods: methodNames(shared),
				Pattern: regs[i].pattern,
				Other:   regs[j].pattern,
				Example: example,
				Note:    note,
			})
		}
	}
	return reports
}

// shadowReg is a route registration expanded with its mount prefixes.
type shadowReg struct {
	methods methodTyp
	pattern string
}

// collectShadowRegs gathers this mux's registrations, recursing into
// mounted chi subrouters and expanding their patterns with the mount prefix.
func (mx *Mux) collectShadowRegs(prefix string, out *[]shadowReg) {
	for _, reg := range mx.tree.regs {
		if reg.methods&mSTUB != 0 {
			// Mount stub registrations are not real endpoints; the
			// mounted routes are collected via the subrouter below.
			continue
		}
		*out = append(*out, shadowReg{methods: reg.methods, pattern: replaceWildcards(prefix + reg.pattern)})
	}
	for _, rt := range mx.tree.routes() {
		subMux, ok := rt.SubRoutes.(*Mux)
		if !ok {
			continue
		}
		subPrefix := strings.TrimSuffix(prefix+rt.Pattern, "/*")
		subMux.collectShadowRegs(subPrefix, out)
	}
}

// methodNames returns the sorted names of the methods set in mask.
func methodNames(mask methodTyp) []string {
	var names []string
	for mt, name := range reverseMethodMap {
		if mask&mt != 0 {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// Segment kinds used by compareRoutePatterns.
const (
	segStatic = iota
	segParam
	segRegexp
	segComplex // mixed static+param segment, treated as undecidable
)

// classifySegment classifies one path segment of a routing pattern. For
// regexp segments it returns the unanchored regexp source.
func classifySegment(seg string) (int, string) {
	if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
		_, rexpat, isRegexp := strings.Cut(seg[1:len(seg)-1], ":")
		if isRegexp {
			return segRegexp, rexpat
		}
		return segParam, ""
	}
	if strings.ContainsAny(seg, "{}*") {
		return segComplex, ""
	}
	return segStatic, ""
}

// compareRoutePatterns statically compares two routing patterns, returning
// their relation ("equal", "overlap", "unknown" or "disjoint"), a concrete
// example path matching both when determinable, and an explanatory note.
// Relations involving arbitrary regexp containment are reported as
// "unknown" instead of being assumed disjoint.
func compareRoutePatterns(a, b string) (rel, example, note string) {
	if a == b {
		return "equal", a, "identical method and pattern registered more than once"
	}

	sa := strings.Split(strings.TrimPrefix(a, "/"), "/")
	sb := strings.Split(strings.TrimPrefix(b, "/"), "/")

	var ex []string
	exOK := true
	staticScore := 0 // positive: a is more static; negative: b is more static

	for i := 0; ; i++ {
		ha, hb := i < len(sa), i < len(sb)

		if !ha && !hb {
			return finishCompare("overlap", ex, exOK, staticScore)
		}
		if ha && sa[i] == "*" {
			if hb && sb[i] == "*" {
				return finishCompare("overlap", ex, exOK, staticScore)
			}
			if !hb {
				// The catch-all requires a trailing slash the other
				// pattern does not have.
				return "disjoint", "", ""
			}
			rest, ok := sampleSegments(sb[i:])
			if !ok {
				return "unknown", "", "cannot determine overlap involving regexp segments"
			}
			return finishCompare("overlap", append(ex, rest...), exOK, staticScore)
		}
		if hb && sb[i] == "*" {
			if !ha {
				return "disjoint", "", ""
			}
			rest, ok := sampleSegments(sa[i:])
			if !ok {
				return "unknown", "", "cannot determine overlap involving regexp segments"
			}
			return finishCompare("overlap", append(ex, rest...), exOK, staticScore)
		}
		if !ha || !hb {
			return "disjoint", "", ""
		}

		ka, rexpa := classifySegment(sa[i])
		kb, rexpb := classifySegment(sb[i])

		switch {
		case ka == segStatic && kb == segStatic:
			if sa[i] != sb[i] {
				return "disjoint", "", ""
			}
			ex = append(ex, sa[i])
		case ka == segStatic && kb == segParam:
			ex = append(ex, sa[i])
			staticScore++
		case ka == segParam && kb == segStatic:
			ex = append(ex, sb[i])
			staticScore--
		case ka == segStatic && kb == segRegexp:
			if !matchRexpat(rexpb, sa[i]) {
				return "disjoint", "", ""
			}
			ex = append(ex, sa[i])
			staticScore++
		case ka == segRegexp && kb == segStatic:
			if !matchRexpat(rexpa, sb[i]) {
				return "disjoint", "", ""
			}
			ex = append(ex, sb[i])
			staticScore--
		case ka == segParam && kb == segParam:
			ex = append(ex, "x")
		case ka == segRegexp && kb == segRegexp && rexpa == rexpb:
			// Identical regexps certainly overlap, but a concrete
			// sample value cannot be synthesized.
			exOK = false
			ex = append(ex, "?")
		default:
			// regexp vs regexp with different sources, param vs regexp,
			// or a complex mixed segment: containment is undecidable
			// here, so report unknown rather than assume disjoint.
			return "unknown", "", "cannot determine overlap involving regexp or mixed segments"
		}
	}
}

// finishCompare builds the final relation result, attaching the example
// path and a static-priority note when applicable.
func finishCompare(rel string, ex []string, exOK bool, staticScore int) (string, string, string) {
	example := ""
	if exOK {
		example = "/" + strings.Join(ex, "/")
	}
	note := ""
	switch {
	case staticScore > 0:
		note = "static segments take priority over params; the example path routes to the first pattern"
	case staticScore < 0:
		note = "static segments take priority over params; the example path routes to the second pattern"
	}
	return rel, example, note
}

// sampleSegments returns concrete sample values for pattern segments, or
// ok=false when a segment cannot be sampled (regexp or mixed segments).
func sampleSegments(segs []string) ([]string, bool) {
	out := make([]string, 0, len(segs))
	for _, seg := range segs {
		switch kind, _ := classifySegment(seg); {
		case seg == "*":
			out = append(out, "x")
		case kind == segStatic:
			out = append(out, seg)
		case kind == segParam:
			out = append(out, "x")
		default:
			return nil, false
		}
	}
	return out, true
}

// matchRexpat reports whether s matches the given route param regexp,
// anchoring it the same way chi does when compiling route patterns.
func matchRexpat(rexpat, s string) bool {
	if !strings.HasPrefix(rexpat, "^") {
		rexpat = "^" + rexpat
	}
	if !strings.HasSuffix(rexpat, "$") {
		rexpat += "$"
	}
	re, err := regexp.Compile(rexpat)
	if err != nil {
		return false
	}
	return re.MatchString(s)
}
