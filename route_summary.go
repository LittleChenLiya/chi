package chi

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// RouteEndpoint is a read-only summary of one registered path template and
// the HTTP methods explicitly registered on it.
//
// A summary is built by reading the routing structures only. It never sends
// HTTP requests and never invokes a route handler or middleware, so it is safe
// to build against a live, fully configured [Router].
type RouteEndpoint struct {
	// Pattern is the full path template of the endpoint, including every
	// Mount prefix and every named parameter segment, for example
	// "/v1/items/{id}". Templates that differ only in a parameter name (for
	// example "/items/{id}" versus "/items/{name}") are never merged.
	Pattern string `json:"pattern"`

	// Methods is the sorted list of HTTP methods explicitly registered on
	// Pattern. An automatically served HEAD on a GET-only route (for example,
	// when the GetHead middleware is in use) is not an explicit registration
	// and therefore never appears here.
	Methods []string `json:"methods"`
}

// HasMethod reports whether method is explicitly registered on the endpoint.
// The comparison is case-sensitive; pass canonical method names such as
// [http.MethodHead].
func (e RouteEndpoint) HasMethod(method string) bool {
	for _, m := range e.Methods {
		if m == method {
			return true
		}
	}
	return false
}

// ExplicitHEAD reports whether a HEAD handler was explicitly registered on
// the endpoint pattern.
//
// chi itself never synthesizes a HEAD registration for a GET route: an
// explicit [Mux.Head] (or [Mux.Handle]/[Mux.Method]) is the only way HEAD
// shows up in a [RouteEndpoint]. The optional GetHead middleware instead
// reroutes individual HEAD requests at serving time, without registering
// anything, so a GET-only endpoint reports ExplicitHEAD == false even when
// that middleware is installed. API checking tools can use this to decide
// whether to list HEAD as its own contract or merely as a runtime alias of
// GET.
func (e RouteEndpoint) ExplicitHEAD() bool {
	return e.HasMethod(http.MethodHead)
}

// UnexpandedMount records a [Mux.Mount] whose subrouter cannot be traversed.
//
// This happens when the mounted handler is an opaque [http.Handler] (for
// example [http.FileServer]) rather than a chi [Routes], so its inner routes
// are unknown and are intentionally not invented or guessed. Mounts of chi
// routers are fully expanded and never appear here.
//
// Note: Mount documents its argument as a prefix without a trailing slash
// (for example Mount("/files", h)). An opaque handler mounted with an
// explicit trailing slash (Mount("/files/", h)) leaves no internal mount
// marker and is indistinguishable from a catch-all Handle route, so it is
// summarized as a normal "/*" endpoint rather than listed here.
type UnexpandedMount struct {
	// Pattern is the full path template of the mount connector, ending in
	// "/*" (for example "/files/*"). Anything it may serve underneath is
	// unknown.
	Pattern string `json:"pattern"`
}

// RouteSummary is the stable, read-only summary of every route reachable from
// a [Router]. It is JSON-marshalable for API checking tools.
type RouteSummary struct {
	// Endpoints lists each registered path template together with the methods
	// explicitly registered on it. Entries are sorted by Pattern.
	Endpoints []RouteEndpoint `json:"endpoints"`

	// UnexpandedMounts lists mounts backed by a non-[Routes] handler whose
	// contents cannot be inspected. Entries are sorted by Pattern.
	UnexpandedMounts []UnexpandedMount `json:"unexpanded_mounts"`
}

// SummarizeRoutes builds a read-only [RouteSummary] of every route reachable
// from router.
//
// The traversal follows [Routes]/[Walk] and [Mux.Mount] semantics: mount
// prefixes are preserved, parameter segments keep their registered names and
// methods registered on the same full path template are merged into a single
// [RouteEndpoint]. Templates that differ only in a parameter name are never
// collapsed together. A mount backed by an opaque [http.Handler] (anything
// that is not a chi [Routes]) is reported under UnexpandedMounts instead of
// having methods fabricated for it.
//
// SummarizeRoutes only reads routing metadata: it does not issue requests and
// it never calls handlers or middlewares, and it does not register, remove or
// alter any route or middleware.
//
// Only explicitly registered methods are reported. In particular, chi does
// not implicitly register HEAD for GET routes, so a GET-only endpoint never
// lists HEAD; see [RouteEndpoint.ExplicitHEAD].
func SummarizeRoutes(router Routes) (*RouteSummary, error) {
	if router == nil {
		return nil, fmt.Errorf("chi: cannot summarize a nil router")
	}
	if mux, ok := router.(*Mux); ok {
		return summarizeMuxTree(mux), nil
	}
	return summarizeViaWalk(router), nil
}

// summarizeViaWalk is the fallback used when the given Routes is not a *Mux.
// It relies on the public Walk traversal, so it cannot distinguish an opaque
// Mount (a non-Routes http.Handler) from a catch-all route registered with
// Handle/HandleFunc: both appear to Walk as a "/*" route serving every
// method. *Mux inputs use summarizeMuxTree, which has the internal markers
// needed to tell the two apart.
func summarizeViaWalk(router Routes) *RouteSummary {
	merged := make(map[string]map[string]struct{})

	_ = Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		ms, ok := merged[route]
		if !ok {
			ms = make(map[string]struct{})
			merged[route] = ms
		}
		ms[method] = struct{}{}
		return nil
	})

	s := &RouteSummary{Endpoints: []RouteEndpoint{}}
	for pattern, ms := range merged {
		s.Endpoints = append(s.Endpoints, RouteEndpoint{Pattern: pattern, Methods: sortedMethods(ms)})
	}
	sort.Slice(s.Endpoints, func(i, j int) bool { return s.Endpoints[i].Pattern < s.Endpoints[j].Pattern })
	s.UnexpandedMounts = []UnexpandedMount{}
	return s
}

// summarizeMuxTree reads a *Mux routing tree directly so it can use the
// internal mSTUB markers that Mount() installs. The summary is stable: the
// tree is only read, never mutated.
func summarizeMuxTree(mx *Mux) *RouteSummary {
	return summarizeTree(mx.tree, "", collectStubBases(mx.tree))
}

// summarizeTree walks the nodes of one mux tree. parentRoute is the path
// already accumulated from ancestor mounts; stubBases holds every pattern
// (relative to this tree mount point) on which a Mount stub was installed,
// which identifies opaque mount terminals that must stay unexpanded.
func summarizeTree(n *node, parentRoute string, stubBases map[string]struct{}) *RouteSummary {
	merged := make(map[string]map[string]struct{})
	var unexpanded []string

	var recurse func(cn *node, prefix string)
	recurse = func(cn *node, prefix string) {
		if cn.endpoints != nil || cn.subroutes != nil {
			visitSummaryNode(cn, prefix, stubBases, merged, &unexpanded)
		}
		for _, ns := range cn.children {
			for _, child := range ns {
				recurse(child, prefix)
			}
		}
	}
	recurse(n, parentRoute)

	s := &RouteSummary{Endpoints: make([]RouteEndpoint, 0, len(merged))}
	for pattern, ms := range merged {
		s.Endpoints = append(s.Endpoints, RouteEndpoint{Pattern: pattern, Methods: sortedMethods(ms)})
	}
	sort.Slice(s.Endpoints, func(i, j int) bool { return s.Endpoints[i].Pattern < s.Endpoints[j].Pattern })

	sort.Strings(unexpanded)
	s.UnexpandedMounts = make([]UnexpandedMount, 0, len(unexpanded))
	for _, p := range unexpanded {
		s.UnexpandedMounts = append(s.UnexpandedMounts, UnexpandedMount{Pattern: p})
	}
	return s
}

// visitSummaryNode summarizes a single radix-tree leaf node. It mirrors the
// filtering of node.routes(): Mount synthetic stub handler is hidden, while a
// real handler that happens to share the stub pattern is kept.
func visitSummaryNode(
	cn *node,
	prefix string,
	stubBases map[string]struct{},
	merged map[string]map[string]struct{},
	unexpanded *[]string,
) {
	// A chi subrouter mounted here: recurse into it following Walk path
	// joining rules and do not emit the connector node itself.
	if cn.subroutes != nil {
		if subMux, ok := cn.subroutes.(*Mux); ok {
			child := summarizeTree(subMux.tree, joinTemplates(prefix, endpointPattern(cn)), collectStubBases(subMux.tree))
			mergeSummary(merged, unexpanded, child)
		} else {
			child := summarizeViaWalk(cn.subroutes)
			base := normalizeTemplate(joinTemplates(prefix, endpointPattern(cn)))
			for _, ep := range child.Endpoints {
				// Walk inside the subrouter reports paths relative to its own
				// root; re-prefix them with this mount connector.
				full := normalizeTemplate(strings.TrimSuffix(base, "/*") + ep.Pattern)
				addMethods(merged, full, ep.Methods)
			}
		}
		return
	}

	var stubHandler http.Handler
	if stubEP := cn.endpoints[mSTUB]; stubEP != nil {
		stubHandler = stubEP.handler
	}

	// Group the concrete method endpoints by the pattern they were registered
	// with. Keeping the full template string (parameter name included) as the
	// key is what keeps "/x/{id}" and "/x/{name}" separate.
	pats := make(map[string]map[methodTyp]*endpoint)
	for mt, ep := range cn.endpoints {
		if mt == mSTUB || ep == nil || ep.pattern == "" {
			continue
		}
		group, ok := pats[ep.pattern]
		if !ok {
			group = make(map[methodTyp]*endpoint)
			pats[ep.pattern] = group
		}
		group[mt] = ep
	}

	for pattern, mh := range pats {
		methods := make(map[string]struct{})

		// A catch-all registration serving all methods is either an opaque
		// Mount terminal (Mount("/files", http.FileServer(...))) or a genuine
		// Handle/HandleFunc(".../*", h). Only the former is a stub base with
		// no subroutes, and it must be reported as unexpanded rather than
		// having its methods fabricated.
		catchAll := strings.HasSuffix(pattern, "*")
		base := strings.TrimSuffix(pattern, "*")
		_, isStubBase := stubBases[base]
		if catchAll && isStubBase {
			full := normalizeTemplate(joinTemplates(prefix, pattern))
			*unexpanded = appendUnique(*unexpanded, full)
			continue
		}

		for mt, ep := range mh {
			if ep.handler == nil || equalHandlers(ep.handler, stubHandler) {
				continue
			}
			if mt == mALL {
				// Registering with Handle()/HandleFunc() serves every method
				// explicitly; surface the concrete method set rather than "*".
				for _, m := range allMethodNames() {
					methods[m] = struct{}{}
				}
				continue
			}
			if name, ok := reverseMethodMap[mt]; ok {
				methods[name] = struct{}{}
			}
		}

		if len(methods) == 0 {
			// Stub-only leaf (e.g. the "/mount" or "/mount/" connector of a
			// traversable subrouter): nothing to report.
			continue
		}

		full := normalizeTemplate(joinTemplates(prefix, pattern))
		addMethods(merged, full, keysOf(methods))
	}
}

// endpointPattern returns the registered pattern recorded on a node
// endpoints. A mount connector node always carries the mount own pattern (via
// its mALL/mSTUB endpoints), so any populated endpoint can supply it.
func endpointPattern(cn *node) string {
	for _, ep := range cn.endpoints {
		if ep != nil && ep.pattern != "" {
			return ep.pattern
		}
	}
	return ""
}

// collectStubBases returns the set of mount patterns (with the trailing "*"
// removed, relative to this tree) on which Mount() installed a stub. Every
// Mount writes an mSTUB endpoint on its mount base; only traversable mounts
// also keep an mSTUB on their "/*" terminal, so an opaque terminal is
// recognised by its base appearing here while the terminal itself has no
// subroutes.
//
// The mSTUB endpoint itself stores no pattern (see node.setEndpoint), so the
// base pattern is taken from any concrete endpoint that shares the node.
func collectStubBases(n *node) map[string]struct{} {
	bases := make(map[string]struct{})
	var walk func(cn *node)
	walk = func(cn *node) {
		if cn.endpoints != nil && cn.endpoints[mSTUB] != nil && cn.endpoints[mSTUB].handler != nil {
			for _, ep := range cn.endpoints {
				if ep == nil || ep.pattern == "" {
					continue
				}
				bases[strings.TrimSuffix(ep.pattern, "*")] = struct{}{}
				break
			}
		}
		for _, ns := range cn.children {
			for _, child := range ns {
				walk(child)
			}
		}
	}
	walk(n)
	return bases
}

// joinTemplates concatenates a mount prefix with a sub-pattern. A connector
// pattern always ends in "/*"; joining keeps the connector wildcard as the
// seam so normalizeTemplate can later rewrite "/parent/*" + "/child" into
// "/parent/child", matching Walk semantics.
func joinTemplates(prefix, pattern string) string {
	if prefix == "" {
		return pattern
	}
	return prefix + pattern
}

// normalizeTemplate turns a joined traversal path, such as
// "/v1/*/items/{id}", into the externally visible template "/v1/items/{id}",
// applying the same "/*/" to "/" rewrite Walk uses.
func normalizeTemplate(p string) string {
	return strings.ReplaceAll(p, "/*/", "/")
}

func addMethods(merged map[string]map[string]struct{}, pattern string, methods []string) {
	ms, ok := merged[pattern]
	if !ok {
		ms = make(map[string]struct{})
		merged[pattern] = ms
	}
	for _, m := range methods {
		ms[m] = struct{}{}
	}
}

func mergeSummary(merged map[string]map[string]struct{}, unexpanded *[]string, child *RouteSummary) {
	for _, ep := range child.Endpoints {
		addMethods(merged, ep.Pattern, ep.Methods)
	}
	for _, um := range child.UnexpandedMounts {
		*unexpanded = appendUnique(*unexpanded, um.Pattern)
	}
}

func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}

func keysOf(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// canonicalMethodOrder ranks the methods chi knows about, grouped in the
// order API tools conventionally display them. Custom methods registered via
// RegisterMethod sort after the built-in ones (handled in sortedMethods).
var canonicalMethodOrder = []string{
	http.MethodGet,
	http.MethodHead,
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
	http.MethodDelete,
	http.MethodOptions,
	http.MethodConnect,
	methodQuery,
	http.MethodTrace,
}

// allMethodNames returns the methods currently served by an mALL ("match
// every method") registration. RegisterMethod() can extend mALL at runtime,
// so this is derived from the method bits rather than a fixed list; callers
// order the result with sortedMethods.
func allMethodNames() []string {
	out := make([]string, 0, len(reverseMethodMap))
	for mt, name := range reverseMethodMap {
		if mALL&mt == mt {
			out = append(out, name)
		}
	}
	return sortedMethodsStringSet(out)
}

// sortedMethodsStringSet orders method names using the same rules as
// sortedMethods, which works on a set.
func sortedMethodsStringSet(methods []string) []string {
	set := make(map[string]struct{}, len(methods))
	for _, m := range methods {
		set[m] = struct{}{}
	}
	return sortedMethods(set)
}

// sortedMethods returns the methods in set in stable order: built-in methods
// first (in canonical display order), followed by any custom registered
// methods sorted alphabetically.
func sortedMethods(set map[string]struct{}) []string {
	known := make(map[string]int, len(canonicalMethodOrder))
	for i, m := range canonicalMethodOrder {
		known[m] = i
	}

	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, iok := known[out[i]]
		rj, jok := known[out[j]]
		if iok && jok {
			return ri < rj
		}
		if iok != jok {
			return iok
		}
		return out[i] < out[j]
	})
	return out
}

// WriteReport writes a short, stable, human-readable digest of the summary
// suitable for inclusion in API check reports.
//
// Endpoints are printed one per line as "METHOD TEMPLATE". Every
// endpoint template is repeated on its own line even when it serves several
// methods, so the difference between an explicitly registered HEAD and a
// GET-only route is visible at a glance; a GET endpoint without an explicit
// HEAD carries the marker "HEAD~GET" (HEAD served only at runtime, never
// registered). Opaque mounts are listed separately as "MOUNT TEMPLATE" with
// an explicit "(unexpanded)" note.
func (s *RouteSummary) WriteReport(w io.Writer) error {
	if s == nil {
		return fmt.Errorf("chi: cannot write a nil route summary")
	}
	if _, err := io.WriteString(w, "Route method summary\n"); err != nil {
		return err
	}

	for _, ep := range s.Endpoints {
		marker := ""
		if ep.HasMethod(http.MethodGet) && !ep.ExplicitHEAD() {
			marker = "\t# HEAD~GET (no explicit HEAD registration)"
		}
		for _, m := range ep.Methods {
			if _, err := fmt.Fprintf(w, "%-7s %s%s\n", m, ep.Pattern, marker); err != nil {
				return err
			}
		}
	}

	for _, um := range s.UnexpandedMounts {
		if _, err := fmt.Fprintf(w, "%-7s %s (unexpanded: mounted handler is not a chi router)\n", "MOUNT", um.Pattern); err != nil {
			return err
		}
	}
	return nil
}

// Report returns the same text as [RouteSummary.WriteReport] as a string.
func (s *RouteSummary) Report() string {
	var b strings.Builder
	_ = s.WriteReport(&b)
	return b.String()
}
