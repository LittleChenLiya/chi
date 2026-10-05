package chi

import (
	"fmt"
	"net/http"
	"strings"
)

// NamedMiddleware is a middleware handler registered under an explicit name
// so that its position in the middleware chain can be validated at startup
// with Mux.Audit.
//
// A named middleware may declare ordering constraints relative to other
// named middlewares using Before and After. Constraints are declarative:
// Audit reports violations but never reorders the chain.
type NamedMiddleware struct {
	name    string
	handler func(http.Handler) http.Handler
	before  []string
	after   []string
}

// Named registers a middleware handler under an explicit name. The name is
// used in Before/After constraints and in Audit diagnostics to identify the
// middleware in the effective chain.
func Named(name string, handler func(http.Handler) http.Handler) *NamedMiddleware {
	return &NamedMiddleware{name: name, handler: handler}
}

// Before declares that this middleware must execute before each of the named
// middlewares. The constraint is checked by Audit against the real
// registered order; it is never enforced by reordering.
func (n *NamedMiddleware) Before(names ...string) *NamedMiddleware {
	n.before = append(n.before, names...)
	return n
}

// After declares that this middleware must execute after each of the named
// middlewares. The constraint is checked by Audit against the real
// registered order; it is never enforced by reordering.
func (n *NamedMiddleware) After(names ...string) *NamedMiddleware {
	n.after = append(n.after, names...)
	return n
}

// Name returns the name the middleware was registered with.
func (n *NamedMiddleware) Name() string {
	return n.name
}

// namedMiddlewareEntry records a NamedMiddleware registered on a Mux and the
// position of its handler in that Mux's middlewares slice.
type namedMiddlewareEntry struct {
	named *NamedMiddleware
	pos   int
}

// chainEntry is a single position in an effective middleware chain. An
// unnamed position has named == nil and is reported as "<unnamed>" in
// diagnostics; it never satisfies a Before/After constraint.
type chainEntry struct {
	pos   int
	named *NamedMiddleware
}

// MiddlewareOrderViolation describes a named middleware that was registered
// in an order conflicting with a Before/After constraint.
type MiddlewareOrderViolation struct {
	// Name is the named middleware that declared the constraint.
	Name string
	// Requires is the name it must precede (Before) or follow (After).
	Requires string
	// Before is true when Name must come before Requires, false when after.
	Before bool
	// Path is the mount path of the router whose effective chain was checked.
	Path string
	// Chain is the effective chain in execution order, with unnamed positions
	// reported as "<unnamed>".
	Chain []string
}

func (v MiddlewareOrderViolation) String() string {
	rel := "before"
	if !v.Before {
		rel = "after"
	}
	return fmt.Sprintf("chi: middleware %q must be registered %s %q on %s, but the effective chain is [%s]",
		v.Name, rel, v.Requires, v.Path, strings.Join(v.Chain, " -> "))
}

// MiddlewareMissingDependency describes a named middleware that references
// another named middleware which is not registered in its effective chain.
type MiddlewareMissingDependency struct {
	// Name is the named middleware that declared the constraint.
	Name string
	// Requires is the name that was not found.
	Requires string
	// Before is true when Name must come before Requires, false when after.
	Before bool
	// Path is the mount path of the router whose effective chain was checked.
	Path string
}

func (d MiddlewareMissingDependency) String() string {
	rel := "before"
	if !d.Before {
		rel = "after"
	}
	return fmt.Sprintf("chi: middleware %q must be registered %s %q on %s, but %q is not registered in the effective chain",
		d.Name, rel, d.Requires, d.Path, d.Requires)
}

// MiddlewareAuditError is returned by Mux.Audit when one or more named
// middlewares violate their ordering constraints or reference missing
// dependencies. Missing dependencies and order violations are reported
// separately.
type MiddlewareAuditError struct {
	// OrderViolations are constraints whose relative order is wrong.
	OrderViolations []MiddlewareOrderViolation
	// MissingDependencies are constraints referencing names not present in
	// the effective chain.
	MissingDependencies []MiddlewareMissingDependency
}

func (e *MiddlewareAuditError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "chi: middleware audit found %d order violation(s) and %d missing dependency(ies):",
		len(e.OrderViolations), len(e.MissingDependencies))
	for _, v := range e.OrderViolations {
		b.WriteString("\n  - ")
		b.WriteString(v.String())
	}
	for _, d := range e.MissingDependencies {
		b.WriteString("\n  - ")
		b.WriteString(d.String())
	}
	return b.String()
}

// Audit validates the ordering constraints of all named middlewares registered
// on the Mux and its mounted subrouters. It checks only the real registered
// order (Use order and Mount composition); it never reorders the chain and
// never invokes any middleware handler.
//
// The effective chain for a mounted subrouter includes the parent Mux's
// middlewares. Unnamed middlewares are kept as unknown positions and never
// satisfy a Before/After constraint.
//
// It returns a *MiddlewareAuditError describing order violations and missing
// dependencies separately, or nil if all constraints are satisfied.
func (mx *Mux) Audit() error {
	a := &middlewareAudit{}
	a.auditMux(mx, nil, "/", false)
	return a.err()
}

type middlewareAudit struct {
	orderViolations []MiddlewareOrderViolation
	missingDeps     []MiddlewareMissingDependency
}

func (a *middlewareAudit) err() error {
	if len(a.orderViolations) == 0 && len(a.missingDeps) == 0 {
		return nil
	}
	return &MiddlewareAuditError{OrderViolations: a.orderViolations, MissingDependencies: a.missingDeps}
}

// auditMux checks a single Mux. baseChain is the effective chain of the
// parent (for mounted subrouters) or the non-inline ancestor (for inline
// groups). isInline reports whether mx was derived via With/Group.
func (a *middlewareAudit) auditMux(mx *Mux, baseChain []chainEntry, path string, isInline bool) {
	own := mx.chainEntries()
	effective := make([]chainEntry, 0, len(baseChain)+len(own))
	effective = append(effective, baseChain...)
	effective = append(effective, own...)

	a.check(effective, path)

	if isInline {
		// Inline groups share the parent tree; their own middlewares already
		// include inline ancestors, so keep the same non-inline base chain.
		for _, child := range mx.inlineChildren {
			a.auditMux(child, baseChain, path, true)
		}
		return
	}

	// Mounted subrouters: their effective chain extends this Mux chain.
	seen := make(map[*Mux]bool)
	for _, rt := range mx.tree.routes() {
		sub, ok := rt.SubRoutes.(*Mux)
		if !ok || sub == nil || seen[sub] {
			continue
		}
		seen[sub] = true
		a.auditMux(sub, effective, joinMountPath(path, rt.Pattern), false)
	}

	// Inline groups derived from this Mux: base chain is this Mux effective
	// chain (the non-inline ancestor chain).
	for _, child := range mx.inlineChildren {
		a.auditMux(child, effective, path, true)
	}
}

// check evaluates the Before/After constraints of every named middleware in
// the effective chain against the chain real order. It never invokes
// handlers.
func (a *middlewareAudit) check(chain []chainEntry, path string) {
	for i, e := range chain {
		if e.named == nil {
			continue
		}
		n := e.named
		for _, before := range n.before {
			if before == n.name {
				continue
			}
			j := indexOfName(chain, before)
			if j < 0 {
				a.missingDeps = append(a.missingDeps, MiddlewareMissingDependency{
					Name: n.name, Requires: before, Before: true, Path: path,
				})
				continue
			}
			if i >= j {
				a.orderViolations = append(a.orderViolations, MiddlewareOrderViolation{
					Name: n.name, Requires: before, Before: true, Path: path, Chain: chainNames(chain),
				})
			}
		}
		for _, after := range n.after {
			if after == n.name {
				continue
			}
			j := lastIndexOfName(chain, after)
			if j < 0 {
				a.missingDeps = append(a.missingDeps, MiddlewareMissingDependency{
					Name: n.name, Requires: after, Before: false, Path: path,
				})
				continue
			}
			if i <= j {
				a.orderViolations = append(a.orderViolations, MiddlewareOrderViolation{
					Name: n.name, Requires: after, Before: false, Path: path, Chain: chainNames(chain),
				})
			}
		}
	}
}

// chainEntries returns the middleware positions registered on mx, in order,
// with named markers. Unnamed positions have named == nil.
func (mx *Mux) chainEntries() []chainEntry {
	entries := make([]chainEntry, len(mx.middlewares))
	for i := range entries {
		entries[i] = chainEntry{pos: i}
	}
	for _, ne := range mx.namedMiddlewares {
		if ne.pos >= 0 && ne.pos < len(entries) {
			entries[ne.pos] = chainEntry{pos: ne.pos, named: ne.named}
		}
	}
	return entries
}

func indexOfName(chain []chainEntry, name string) int {
	for i, e := range chain {
		if e.named != nil && e.named.name == name {
			return i
		}
	}
	return -1
}

func lastIndexOfName(chain []chainEntry, name string) int {
	for i := len(chain) - 1; i >= 0; i-- {
		if chain[i].named != nil && chain[i].named.name == name {
			return i
		}
	}
	return -1
}

func chainNames(chain []chainEntry) []string {
	names := make([]string, len(chain))
	for i, e := range chain {
		if e.named != nil {
			names[i] = e.named.name
		} else {
			names[i] = "<unnamed>"
		}
	}
	return names
}

// joinMountPath joins a base mount path with a subrouter pattern for
// diagnostics.
func joinMountPath(base, pattern string) string {
	p := strings.TrimSuffix(pattern, "/*")
	if p == "" || p == "/" {
		return base
	}
	if base == "/" {
		return p
	}
	return strings.TrimSuffix(base, "/") + p
}
