package chi

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
)

// DiagKind classifies a route-graph diagnostic produced by InspectRoutes.
type DiagKind int

const (
	// DiagDuplicateMethod reports two or more registrations of the same HTTP
	// method on the identical full route path, e.g. a route reached both
	// directly and through a Mount. Only one of them can serve a request.
	DiagDuplicateMethod DiagKind = iota

	// DiagShapeConflict reports registrations of the same HTTP method whose
	// patterns share the same match shape: identical static segments and
	// placeholders in the same positions. Placeholder names may differ and
	// placeholder regexes are merged only when they are provably equivalent,
	// so both registrations match the same set of requests.
	DiagShapeConflict

	// DiagRegexPending reports registrations of the same HTTP method whose
	// patterns share the same skeleton (static segments and placeholder
	// positions) but carry regex placeholders whose equivalence could not be
	// determined statically. The overlap is a potential conflict left for
	// manual review.
	DiagRegexPending

	// DiagShadowedByMount reports a registration reached through a Mount
	// chain whose requests are partially covered by a registration at a
	// shallower scope. chi consults the shallower routing tree first, so
	// requests matching both never reach the mounted registration.
	DiagShadowedByMount
)

// String returns a stable, lowercase identifier for the diagnostic kind.
func (k DiagKind) String() string {
	switch k {
	case DiagDuplicateMethod:
		return "duplicate-method"
	case DiagShapeConflict:
		return "shape-conflict"
	case DiagRegexPending:
		return "regex-pending"
	case DiagShadowedByMount:
		return "shadowed-by-mount"
	default:
		return "unknown"
	}
}

// RouteRef identifies a single route registration involved in a Diagnostic.
type RouteRef struct {
	Method  string   // HTTP method, e.g. "GET"
	Pattern string   // full route pattern, e.g. "/api/users/{id}"
	Mounts  []string // mount source chain, root first; empty when registered on the root router
}

// Diagnostic describes one route-graph finding. Routes lists the
// registrations involved, in registration (graph traversal) order.
type Diagnostic struct {
	Kind   DiagKind
	Method string // HTTP method shared by the involved routes
	Shape  string // full pattern or normalized match shape, e.g. "/users/{}"
	Routes []RouteRef
	Detail string
}

// graphEntry is one leaf registration collected from the route graph.
type graphEntry struct {
	method  string
	pattern string
	mounts  []string
	order   int
}

// InspectRoutes statically inspects the route graph reachable from r and
// reports duplicate methods on the same full path, same-shape overlaps,
// undetermined regex overlaps and registrations shadowed by broader mounts.
//
// The inspection only traverses the routing information exposed by the
// Routes interface (Routes()); it does not send HTTP requests, does not
// execute handlers and does not modify the router or its matching behavior.
// Diagnostics are returned in a stable order: by kind, then method, then
// shape.
func InspectRoutes(r Routes) []Diagnostic {
	entries := collectGraphEntries(r)
	segs := make([][]segment, len(entries))
	skels := make([]string, len(entries))
	for i, e := range entries {
		segs[i] = parseSegments(e.pattern)
		skels[i] = skeletonKey(segs[i])
	}

	var diags []Diagnostic

	// Group registrations by method and match skeleton.
	type groupKey struct{ method, skel string }
	groups := map[groupKey][]int{}
	var groupOrder []groupKey
	for i, e := range entries {
		k := groupKey{e.method, skels[i]}
		if _, ok := groups[k]; !ok {
			groupOrder = append(groupOrder, k)
		}
		groups[k] = append(groups[k], i)
	}

	for _, k := range groupOrder {
		idxs := groups[k]
		if len(idxs) < 2 {
			continue
		}

		// Sub-group by the exact pattern to find duplicate methods landing
		// on the same full path.
		byPattern := map[string][]int{}
		var patterns []string
		for _, i := range idxs {
			p := entries[i].pattern
			if _, ok := byPattern[p]; !ok {
				patterns = append(patterns, p)
			}
			byPattern[p] = append(byPattern[p], i)
		}
		for _, p := range patterns {
			if len(byPattern[p]) < 2 {
				continue
			}
			diags = append(diags, Diagnostic{
				Kind:   DiagDuplicateMethod,
				Method: k.method,
				Shape:  p,
				Routes: routeRefs(entries, byPattern[p]),
				Detail: fmt.Sprintf("%d registrations resolve to the same method and path; only one of them can serve a request", len(byPattern[p])),
			})
		}

		if len(patterns) < 2 {
			continue
		}

		// Distinct patterns sharing one skeleton: a same-shape overlap when
		// every placeholder pair is provably equivalent, otherwise an
		// undetermined (pending) potential conflict.
		compatible := true
		for a := 0; a < len(patterns) && compatible; a++ {
			for b := a + 1; b < len(patterns); b++ {
				if !placeholdersEquivalent(segs[byPattern[patterns[a]][0]], segs[byPattern[patterns[b]][0]]) {
					compatible = false
					break
				}
			}
		}
		var all []int
		for _, p := range patterns {
			all = append(all, byPattern[p]...)
		}
		sort.Ints(all)
		if compatible {
			diags = append(diags, Diagnostic{
				Kind:   DiagShapeConflict,
				Method: k.method,
				Shape:  k.skel,
				Routes: routeRefs(entries, all),
				Detail: "patterns have the same match shape; a request matching one also matches the other, so only one registration can serve it",
			})
		} else {
			diags = append(diags, Diagnostic{
				Kind:   DiagRegexPending,
				Method: k.method,
				Shape:  k.skel,
				Routes: routeRefs(entries, all),
				Detail: "placeholder regexes differ and could not be proven equivalent; potential overlap is undetermined",
			})
		}
	}

	// Registrations reached through a mount chain that are covered by a
	// registration at a shallower scope. Pairs already covered by a
	// same-skeleton group diagnostic above are skipped to keep the report
	// free of redundant findings.
	for i, re := range entries {
		if len(re.mounts) == 0 {
			continue
		}
		for j, ae := range entries {
			if i == j || ae.method != re.method || ae.pattern == re.pattern {
				continue
			}
			if skels[j] == skels[i] {
				continue
			}
			if len(ae.mounts) >= len(re.mounts) || !isMountPrefix(ae.mounts, re.mounts) {
				continue
			}
			if !provablyIntersect(segs[j], segs[i]) {
				continue
			}
			diags = append(diags, Diagnostic{
				Kind:   DiagShadowedByMount,
				Method: re.method,
				Shape:  re.pattern,
				Routes: []RouteRef{routeRef(re), routeRef(ae)},
				Detail: fmt.Sprintf("mounted registration is covered by %s %s at a shallower scope; requests matching both are served by the shallower registration", ae.method, ae.pattern),
			})
			break
		}
	}

	sort.SliceStable(diags, func(a, b int) bool {
		if diags[a].Kind != diags[b].Kind {
			return diags[a].Kind < diags[b].Kind
		}
		if diags[a].Method != diags[b].Method {
			return diags[a].Method < diags[b].Method
		}
		return diags[a].Shape < diags[b].Shape
	})
	return diags
}

// FormatReport renders diagnostics in a stable, human-readable form. The
// output is deterministic for a given route graph, so it can be printed at
// startup or captured in tests.
func FormatReport(diags []Diagnostic) string {
	var b strings.Builder
	fmt.Fprintf(&b, "route graph: %d diagnostic(s)\n", len(diags))
	for _, d := range diags {
		fmt.Fprintf(&b, "\n[%s] %s %s\n", d.Kind, d.Method, d.Shape)
		fmt.Fprintf(&b, "  %s\n", d.Detail)
		for _, rt := range d.Routes {
			fmt.Fprintf(&b, "  - %s %s (mounted at: %s)\n", rt.Method, rt.Pattern, mountChainString(rt.Mounts))
		}
	}
	return b.String()
}

// collectGraphEntries flattens the route graph into leaf registrations,
// tracking the mount source chain of every registration. Unlike Walk it
// preserves which mounts a registration was reached through.
func collectGraphEntries(r Routes) []graphEntry {
	var entries []graphEntry
	var walk func(r Routes, parentPath string, mounts []string)
	walk = func(rr Routes, parentPath string, mounts []string) {
		for _, rt := range rr.Routes() {
			if rt.SubRoutes != nil {
				mount := cleanMountPattern(rt.Pattern)
				sub := append(append([]string{}, mounts...), mount)
				walk(rt.SubRoutes, parentPath+mount, sub)
				continue
			}
			methods := make([]string, 0, len(rt.Handlers))
			for m := range rt.Handlers {
				if m == "*" {
					// Skip catch-all method entries, mirroring Walk.
					continue
				}
				methods = append(methods, m)
			}
			sort.Strings(methods)
			for _, m := range methods {
				entries = append(entries, graphEntry{
					method:  m,
					pattern: parentPath + rt.Pattern,
					mounts:  mounts,
					order:   len(entries),
				})
			}
		}
	}
	walk(r, "", nil)
	return entries
}

// cleanMountPattern strips the catch-all suffix from a mount point pattern,
// e.g. "/users/{id}/*" -> "/users/{id}".
func cleanMountPattern(pattern string) string {
	p := strings.TrimSuffix(pattern, "*")
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

func routeRef(e graphEntry) RouteRef {
	return RouteRef{Method: e.method, Pattern: e.pattern, Mounts: e.mounts}
}

func routeRefs(entries []graphEntry, idxs []int) []RouteRef {
	refs := make([]RouteRef, 0, len(idxs))
	for _, i := range idxs {
		refs = append(refs, routeRef(entries[i]))
	}
	return refs
}

func mountChainString(mounts []string) string {
	if len(mounts) == 0 {
		return "(root)"
	}
	return strings.Join(mounts, " -> ")
}

// isMountPrefix reports whether a is a prefix of b.
func isMountPrefix(a, b []string) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// segKind classifies one path segment of a route pattern.
type segKind int

const (
	segStatic segKind = iota
	segParam
	segRegexp
	segCatchAll
	segMixed // a segment mixing literals and placeholders; compared literally
)

type segment struct {
	kind  segKind
	lit   string // static literal (or raw text for segMixed)
	name  string // placeholder name
	regex string // placeholder regex, empty for plain placeholders
}

// parseSegments splits a full route pattern into path segments.
func parseSegments(pattern string) []segment {
	parts := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	segs := make([]segment, 0, len(parts))
	for _, p := range parts {
		segs = append(segs, parseSegment(p))
	}
	return segs
}

func parseSegment(p string) segment {
	switch {
	case p == "*":
		return segment{kind: segCatchAll, lit: "*"}
	case strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}"):
		inner := p[1 : len(p)-1]
		if i := strings.IndexByte(inner, ':'); i >= 0 {
			return segment{kind: segRegexp, name: inner[:i], regex: inner[i+1:]}
		}
		return segment{kind: segParam, name: inner}
	case strings.ContainsAny(p, "{}*"):
		return segment{kind: segMixed, lit: p}
	default:
		return segment{kind: segStatic, lit: p}
	}
}

// skeletonKey normalizes a parsed pattern into its match shape: static
// segments are kept, placeholders collapse to {} and the catch-all to *.
func skeletonKey(segs []segment) string {
	var b strings.Builder
	for _, s := range segs {
		switch s.kind {
		case segParam, segRegexp:
			b.WriteString("/{}")
		case segCatchAll:
			b.WriteString("/*")
		default:
			b.WriteByte('/')
			b.WriteString(s.lit)
		}
	}
	return b.String()
}

// placeholdersEquivalent reports whether two same-skeleton patterns match
// the same requests: every placeholder pair must be provably equivalent.
// Plain placeholders are equivalent to plain placeholders; regex
// placeholders only when their regexes are provably equivalent.
func placeholdersEquivalent(a, b []segment) bool {
	if len(a) != len(b) {
		return false
	}
	isPh := func(k segKind) bool { return k == segParam || k == segRegexp }
	for i := range a {
		if !isPh(a[i].kind) && !isPh(b[i].kind) {
			continue
		}
		if !isPh(a[i].kind) || !isPh(b[i].kind) {
			return false
		}
		if !provablyEquivalentRegex(a[i].regex, b[i].regex) {
			return false
		}
	}
	return true
}

// provablyEquivalentRegex reports whether two placeholder regexes are
// provably equivalent. It is deliberately conservative: regexes are
// equivalent only when their normalized (parsed and simplified) forms are
// identical. Anything else is left undetermined.
func provablyEquivalentRegex(a, b string) bool {
	if a == b {
		return true
	}
	na, err := normalizeRegex(a)
	if err != nil {
		return false
	}
	nb, err := normalizeRegex(b)
	if err != nil {
		return false
	}
	return na == nb
}

func normalizeRegex(re string) (string, error) {
	s, err := syntax.Parse(re, syntax.Perl)
	if err != nil {
		return "", err
	}
	return s.Simplify().String(), nil
}

// provablyIntersect reports whether the request sets matched by two parsed
// patterns provably overlap. It is conservative: whenever an intersection
// cannot be proven (e.g. two different regexes), it reports no overlap.
func provablyIntersect(a, b []segment) bool {
	i := 0
	for i < len(a) && i < len(b) {
		if a[i].kind == segCatchAll || b[i].kind == segCatchAll {
			return true
		}
		if !segmentsIntersect(a[i], b[i]) {
			return false
		}
		i++
	}
	return i == len(a) && i == len(b)
}

func segmentsIntersect(a, b segment) bool {
	if a.kind == segMixed || b.kind == segMixed {
		return a.kind == b.kind && a.lit == b.lit
	}
	if a.kind == segStatic && b.kind == segStatic {
		return a.lit == b.lit
	}
	if a.kind == segParam || b.kind == segParam {
		// A plain placeholder matches any single segment.
		return true
	}
	if a.kind == segRegexp && b.kind == segRegexp {
		return provablyEquivalentRegex(a.regex, b.regex)
	}
	// static vs regexp: provable by matching the literal against the regex.
	st, rx := a, b
	if st.kind != segStatic {
		st, rx = b, a
	}
	re, err := regexp.Compile(rx.regex)
	if err != nil {
		return false
	}
	return re.MatchString(st.lit)
}
