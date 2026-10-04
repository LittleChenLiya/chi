package chi

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// RouteEntry is an endpoint route discovered while traversing the route tree.
// It carries the fully-qualified path (with mount wildcards collapsed), the
// HTTP methods it handles, and the chain of mounts that lead to it.
type RouteEntry struct {
	FullPath string
	Pattern  string
	Methods  []string
	Handler  http.Handler
	Source   []MountSource
}

// MountSource describes a single mount in the chain leading to a route.
type MountSource struct {
	Pattern string
	Path    string
}

// MountEntry is a mount discovered while traversing the route tree.
type MountEntry struct {
	Path      string
	Pattern   string
	Source    []MountSource
	SubRoutes Routes
}

// RouteGraph is a diagnostic view of a router's route tree. It collects every
// endpoint route and mount reachable through the Route traversal API, along
// with the mount source chain for each, so that duplicates and shadowing can
// be reported without sending any HTTP requests.
type RouteGraph struct {
	entries []RouteEntry
	mounts  []MountEntry
}

// NewRouteGraph builds a RouteGraph by traversing r's route tree. The traversal
// is read-only: it does not modify the router, its registration order, or its
// matching behaviour.
func NewRouteGraph(r Routes) *RouteGraph {
	g := &RouteGraph{}
	g.traverse(r, "", nil)
	sort.Slice(g.entries, func(i, j int) bool {
		if g.entries[i].FullPath != g.entries[j].FullPath {
			return g.entries[i].FullPath < g.entries[j].FullPath
		}
		if c := compareStrings(g.entries[i].Methods, g.entries[j].Methods); c != 0 {
			return c < 0
		}
		return sourceKey(g.entries[i].Source) < sourceKey(g.entries[j].Source)
	})
	sort.Slice(g.mounts, func(i, j int) bool {
		if g.mounts[i].Path != g.mounts[j].Path {
			return g.mounts[i].Path < g.mounts[j].Path
		}
		return sourceKey(g.mounts[i].Source) < sourceKey(g.mounts[j].Source)
	})
	return g
}

// Entries returns the endpoint routes in stable order.
func (g *RouteGraph) Entries() []RouteEntry {
	return append([]RouteEntry{}, g.entries...)
}

// Mounts returns the mounts in stable order.
func (g *RouteGraph) Mounts() []MountEntry {
	return append([]MountEntry{}, g.mounts...)
}

func (g *RouteGraph) traverse(r Routes, parentPath string, source []MountSource) {
	for _, rt := range r.Routes() {
		fullPath := cleanPath(parentPath + rt.Pattern)
		if rt.SubRoutes != nil {
			mountPath := mountPathOf(fullPath)
			ms := make([]MountSource, 0, len(source)+1)
			ms = append(ms, source...)
			ms = append(ms, MountSource{Pattern: rt.Pattern, Path: mountPath})
			g.mounts = append(g.mounts, MountEntry{
				Path: mountPath, Pattern: rt.Pattern, Source: ms, SubRoutes: rt.SubRoutes,
			})
			g.traverse(rt.SubRoutes, parentPath+rt.Pattern, ms)
			continue
		}
		g.entries = append(g.entries, RouteEntry{
			FullPath: fullPath, Pattern: rt.Pattern,
			Methods: sortedMethods(rt.Handlers), Handler: firstHandler(rt.Handlers),
			Source: append([]MountSource{}, source...),
		})
	}
}

// DiagnosticKind classifies a diagnostic reported by Diagnostics.
type DiagnosticKind int

const (
	// DiagConflict marks two routes with the same matching shape and
	// overlapping methods whose parameters are determinably equivalent.
	DiagConflict DiagnosticKind = iota

	// DiagPotentialConflict marks two routes with the same matching shape
	// whose parameters are not determinably equivalent (e.g. two different
	// regexps). They may or may not overlap at runtime.
	DiagPotentialConflict

	// DiagCoveredByMount marks a route whose full path is covered by a
	// mount wildcard that is not its own ancestor mount.
	DiagCoveredByMount
)

// Diagnostic is a single issue found while analysing the route graph.
type Diagnostic struct {
	Kind    DiagnosticKind
	Path    string
	Methods []string
	Entries []RouteEntry
	Message string
}

// Diagnostics analyses the route graph and returns a stable-ordered list of
// issues: same-shape conflicts, potential conflicts (regexps that cannot be
// proven equivalent), and routes covered by a mount from a different branch.
func (g *RouteGraph) Diagnostics() []Diagnostic {
	var diags []Diagnostic
	for i := 0; i < len(g.entries); i++ {
		for j := i + 1; j < len(g.entries); j++ {
			a, b := g.entries[i], g.entries[j]
			sa := parseSegments(a.FullPath)
			sb := parseSegments(b.FullPath)
			sameShape, equivalent := compareSegments(sa, sb)
			if !sameShape {
				continue
			}
			overlap := methodOverlap(a, b)
			if len(overlap) == 0 {
				continue
			}
			if equivalent {
				diags = append(diags, Diagnostic{
					Kind: DiagConflict, Path: a.FullPath, Methods: overlap,
					Entries: []RouteEntry{a, b},
					Message: fmt.Sprintf("conflicting %s route on %s", strings.Join(overlap, ","), a.FullPath),
				})
			} else {
				diags = append(diags, Diagnostic{
					Kind: DiagPotentialConflict, Path: a.FullPath, Methods: overlap,
					Entries: []RouteEntry{a, b},
					Message: fmt.Sprintf("potentially conflicting %s route on %s (parameters are not determinably equivalent)", strings.Join(overlap, ","), a.FullPath),
				})
			}
		}
	}
	for i := range g.entries {
		e := &g.entries[i]
		for j := range g.mounts {
			m := &g.mounts[j]
			// Skip mounts that are the route's own ancestor chain.
			if isAncestor(m.Source, e.Source) {
				continue
			}
			// Only flag mounts that are broader than the route's own mount:
			// a mount is broader when its path is a strict prefix of the
			// route's innermost mount path. Routes without a mount are
			// covered by any mount that matches their path.
			if len(e.Source) > 0 {
				ownMount := e.Source[len(e.Source)-1].Path
				if ownMount != m.Path && !strings.HasPrefix(ownMount, m.Path) {
					continue
				}
			}
			// Skip the mount's own stub node (the route path equals the
			// mount path); it is served by the mount, not obscured by it.
			if e.FullPath == m.Path {
				continue
			}
			if covers(m.Path, e.FullPath) {
				diags = append(diags, Diagnostic{
					Kind: DiagCoveredByMount, Path: e.FullPath, Methods: e.Methods,
					Entries: []RouteEntry{*e},
					Message: fmt.Sprintf("route %s is covered by mount %s", e.FullPath, m.Path),
				})
			}
		}
	}
	sort.Slice(diags, func(i, j int) bool {
		if diags[i].Kind != diags[j].Kind {
			return diags[i].Kind < diags[j].Kind
		}
		if diags[i].Path != diags[j].Path {
			return diags[i].Path < diags[j].Path
		}
		return compareStrings(diags[i].Methods, diags[j].Methods) < 0
	})
	return diags
}

// Report writes a human-readable, repeatably printable report of the route
// graph and its diagnostics to w. Running it multiple times on the same
// router yields identical output.
func (g *RouteGraph) Report(w io.Writer) {
	fmt.Fprintln(w, "Route Graph Report")
	fmt.Fprintln(w, "==================")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Routes:")
	for _, e := range g.entries {
		fmt.Fprintf(w, "  %-8s %s  %s\n", strings.Join(e.Methods, ","), e.FullPath, formatSource(e.Source))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Mounts:")
	for _, m := range g.mounts {
		fmt.Fprintf(w, "  %-8s %s  %s\n", "*", m.Path, formatSource(m.Source))
	}
	fmt.Fprintln(w)
	diags := g.Diagnostics()
	if len(diags) == 0 {
		fmt.Fprintln(w, "Diagnostics: none")
		return
	}
	fmt.Fprintln(w, "Diagnostics:")
	for _, d := range diags {
		switch d.Kind {
		case DiagConflict:
			fmt.Fprintf(w, "  [CONFLICT] %s\n", d.Message)
		case DiagPotentialConflict:
			fmt.Fprintf(w, "  [PENDING]  %s\n", d.Message)
		case DiagCoveredByMount:
			fmt.Fprintf(w, "  [COVERED]  %s\n", d.Message)
		}
		for _, e := range d.Entries {
			fmt.Fprintf(w, "      %-8s %s  %s\n", strings.Join(e.Methods, ","), e.FullPath, formatSource(e.Source))
		}
	}
}

// segment is a single piece of a parsed route pattern.
type segment struct {
	static   string
	param    string
	regex    string
	catchAll bool
}

// parseSegments splits a full path into static, param, regex and catch-all
// segments using the same rules as the router.
func parseSegments(pattern string) []segment {
	var segs []segment
	p := pattern
	for len(p) > 0 {
		ntyp, key, rexpat, _, start, end := patNextSegment(p)
		switch ntyp {
		case ntStatic:
			segs = append(segs, segment{static: p})
			return segs
		case ntCatchAll:
			if start > 0 {
				segs = append(segs, segment{static: p[:start]})
			}
			segs = append(segs, segment{catchAll: true})
			return segs
		case ntParam, ntRegexp:
			if start > 0 {
				segs = append(segs, segment{static: p[:start]})
			}
			s := segment{param: key}
			if ntyp == ntRegexp {
				s.regex = rexpat
			}
			segs = append(segs, s)
			p = p[end:]
		}
	}
	return segs
}

// compareSegments compares two parsed patterns for matching shape. It returns
// sameShape=true when the static segments and param positions match, and
// equivalent=true when every param is determinably equivalent (plain params
// match each other, regex params match only when their normalised patterns are
// identical).
func compareSegments(a, b []segment) (sameShape, equivalent bool) {
	if len(a) != len(b) {
		return false, false
	}
	equivalent = true
	for i := range a {
		sa, sb := a[i], b[i]
		if sa.catchAll != sb.catchAll {
			return false, false
		}
		if sa.catchAll {
			continue
		}
		if (sa.static == "") != (sb.static == "") {
			return false, false
		}
		if sa.static != "" {
			if sa.static != sb.static {
				return false, false
			}
			continue
		}
		// Both segments are params.
		if sa.regex != sb.regex {
			equivalent = false
		}
	}
	return true, equivalent
}

// methodOverlap returns the sorted list of HTTP methods shared by two routes.
func methodOverlap(a, b RouteEntry) []string {
	am := methodSet(a.Methods)
	bm := methodSet(b.Methods)
	var overlap []string
	for m := range am {
		if bm[m] {
			overlap = append(overlap, m)
		}
	}
	sort.Strings(overlap)
	return overlap
}

func methodSet(methods []string) map[string]bool {
	set := make(map[string]bool, len(methods))
	for _, m := range methods {
		set[m] = true
	}
	return set
}

// sortedMethods returns the sorted list of methods a route handles. A route
// registered with Handle (mALL) reports every method.
func sortedMethods(handlers map[string]http.Handler) []string {
	if handlers["*"] != nil {
		methods := make([]string, 0, len(methodMap))
		for m := range methodMap {
			methods = append(methods, m)
		}
		sort.Strings(methods)
		return methods
	}
	methods := make([]string, 0, len(handlers))
	for m := range handlers {
		if m != "*" {
			methods = append(methods, m)
		}
	}
	sort.Strings(methods)
	return methods
}

func firstHandler(handlers map[string]http.Handler) http.Handler {
	for _, h := range handlers {
		return h
	}
	return nil
}

// cleanPath collapses nested mount wildcards in a full path, mirroring the
// cleanup Walk applies: "/api/*/users" -> "/api/users".
func cleanPath(p string) string {
	return strings.ReplaceAll(p, "/*/", "/")
}

// mountPathOf returns the mount path with the trailing wildcard stripped:
// "/api/*" -> "/api", "/*" -> "/".
func mountPathOf(p string) string {
	p = strings.TrimSuffix(p, "/*")
	if p == "" {
		return "/"
	}
	return p
}

// covers reports whether mount path m covers full path p.
func covers(m, p string) bool {
	if m == "/" {
		return true
	}
	return p == m || strings.HasPrefix(p, m+"/")
}

// isAncestor reports whether mount a is an ancestor of route source b. A
// mount is also considered its own ancestor (when a and b are equal).
func isAncestor(a []MountSource, b []MountSource) bool {
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

func sourceKey(s []MountSource) string {
	parts := make([]string, len(s))
	for i, ms := range s {
		parts[i] = ms.Path
	}
	return strings.Join(parts, "/")
}

func formatSource(s []MountSource) string {
	if len(s) == 0 {
		return "[root]"
	}
	parts := make([]string, 0, len(s)+1)
	parts = append(parts, "root")
	for _, ms := range s {
		parts = append(parts, ms.Path)
	}
	return "[" + strings.Join(parts, " -> ") + "]"
}

func compareStrings(a, b []string) int {
	la, lb := len(a), len(b)
	n := la
	if lb < n {
		n = lb
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case la < lb:
		return -1
	case la > lb:
		return 1
	}
	return 0
}
