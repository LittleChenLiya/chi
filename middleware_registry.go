package chi

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
)

// MiddlewareConstraint declares an ordering constraint between two named
// middlewares, expressed at registration time. See Before and After.
type MiddlewareConstraint struct {
	before string // the registered middleware must run before this name
	after  string // the registered middleware must run after this name
}

// Before declares that the middleware being registered must run before the
// middleware named `name` in any effective middleware chain.
func Before(name string) MiddlewareConstraint { return MiddlewareConstraint{before: name} }

// After declares that the middleware being registered must run after the
// middleware named `name` in any effective middleware chain.
func After(name string) MiddlewareConstraint { return MiddlewareConstraint{after: name} }

// middlewareRule is a normalized ordering constraint: `before` must appear
// earlier than `after` in an effective middleware chain.
type middlewareRule struct {
	before string
	after  string
}

func (r middlewareRule) String() string {
	return fmt.Sprintf("middleware %q must run before %q", r.before, r.after)
}

// MiddlewareRegistry is an explicit, named middleware registration layer.
//
// Middlewares are registered under a name together with optional ordering
// constraints (Before/After) against other names. Register returns the
// middleware function to hand to Router.Use, so the registry can later
// recognize it in the actual middleware stack.
//
// At startup, Check (or the short Audit entry point) validates the real
// Use/Mount chains of a router against the declared constraints. The audit
// only inspects the actually registered order - it never reorders
// middlewares and never invokes any middleware or handler. Middlewares used
// without a registered name are kept in the chain as unknown positions
// (reported as "?") and never satisfy a dependency.
//
// For subrouters mounted via Mount/Route, the effective chain includes the
// parent layers, so a constraint may be satisfied across a mount boundary
// (e.g. "auth" on the parent and "audit" on the child is valid).
type MiddlewareRegistry struct {
	names map[uintptr]string // middleware func identity -> registered name
	rules []middlewareRule   // deduplicated ordering constraints
	seen  map[middlewareRule]struct{}
}

// NewMiddlewareRegistry returns an empty named-middleware registry.
func NewMiddlewareRegistry() *MiddlewareRegistry {
	return &MiddlewareRegistry{
		names: map[uintptr]string{},
		seen:  map[middlewareRule]struct{}{},
	}
}

// Register records `mw` under `name` along with any ordering constraints,
// and returns `mw` so it can be passed directly to Router.Use:
//
//	reg := chi.NewMiddlewareRegistry()
//	r := chi.NewRouter()
//	r.Use(reg.Register("auth", authMiddleware))
//	r.Use(reg.Register("audit", auditMiddleware, chi.After("auth")))
//	if err := reg.Audit(r); err != nil { log.Fatal(err) }
//
// Registering the same middleware function under a second name replaces the
// previous name. Register panics on a nil middleware or empty name.
func (reg *MiddlewareRegistry) Register(name string, mw func(http.Handler) http.Handler, constraints ...MiddlewareConstraint) func(http.Handler) http.Handler {
	if mw == nil {
		panic("chi: cannot register a nil middleware")
	}
	if name == "" {
		panic("chi: cannot register a middleware with an empty name")
	}
	if reg.names == nil {
		reg.names = map[uintptr]string{}
	}
	if reg.seen == nil {
		reg.seen = map[middlewareRule]struct{}{}
	}
	reg.names[reflect.ValueOf(mw).Pointer()] = name
	for _, c := range constraints {
		var rule middlewareRule
		switch {
		case c.before != "":
			rule = middlewareRule{before: name, after: c.before}
		case c.after != "":
			rule = middlewareRule{before: c.after, after: name}
		default:
			continue
		}
		if rule.before == rule.after {
			panic(fmt.Sprintf("chi: middleware %q cannot be ordered against itself", name))
		}
		if _, ok := reg.seen[rule]; !ok {
			reg.seen[rule] = struct{}{}
			reg.rules = append(reg.rules, rule)
		}
	}
	return mw
}

// MiddlewareMissingDependency reports a chain that uses a constrained
// middleware without the middleware it depends on.
type MiddlewareMissingDependency struct {
	Rule    string   // human-readable ordering rule
	Missing string   // the dependency name absent from the chain
	Scope   string   // mount path of the offending chain, "/" for the root router
	Chain   []string // actual effective chain; unnamed middlewares appear as "?"
}

// MiddlewareOrderViolation reports a chain where two constrained middlewares
// are both present but registered in the wrong order.
type MiddlewareOrderViolation struct {
	Rule  string   // human-readable ordering rule
	Scope string   // mount path of the offending chain, "/" for the root router
	Chain []string // actual effective chain; unnamed middlewares appear as "?"
}

// MiddlewareAuditReport is the result of checking a router's real middleware
// chains against the declared constraints of a registry. Missing
// dependencies and order violations are reported separately.
type MiddlewareAuditReport struct {
	Missing    []MiddlewareMissingDependency
	Violations []MiddlewareOrderViolation
}

// OK reports whether the audit found no problems.
func (r *MiddlewareAuditReport) OK() bool {
	return len(r.Missing) == 0 && len(r.Violations) == 0
}

// Error implements the error interface, describing every finding.
func (r *MiddlewareAuditReport) Error() string {
	var b strings.Builder
	b.WriteString("chi: middleware audit failed")
	if len(r.Missing) > 0 {
		b.WriteString("\n  missing dependencies:")
		for _, m := range r.Missing {
			fmt.Fprintf(&b, "\n    - scope %q: %s, but %q is missing; actual chain: [%s]",
				m.Scope, m.Rule, m.Missing, strings.Join(m.Chain, ", "))
		}
	}
	if len(r.Violations) > 0 {
		b.WriteString("\n  order violations:")
		for _, v := range r.Violations {
			fmt.Fprintf(&b, "\n    - scope %q: %s; actual chain: [%s]",
				v.Scope, v.Rule, strings.Join(v.Chain, ", "))
		}
	}
	return b.String()
}

// Audit is the short audit entry point: it checks the router's actual
// Use/Mount middleware chains against the registry's constraints and returns
// nil when everything is in order, or a *MiddlewareAuditReport as the error.
func (reg *MiddlewareRegistry) Audit(r Routes) error {
	report := reg.Check(r)
	if report.OK() {
		return nil
	}
	return report
}

// MustAudit is like Audit but panics on any finding. It is intended for
// wiring validation at startup.
func (reg *MiddlewareRegistry) MustAudit(r Routes) {
	if err := reg.Audit(r); err != nil {
		panic(err)
	}
}

// Check inspects the effective middleware chain of the router and of every
// mounted subrouter (parent layers included) and reports missing
// dependencies and order violations separately. It only compares function
// identities and never invokes any middleware or handler.
func (reg *MiddlewareRegistry) Check(r Routes) *MiddlewareAuditReport {
	report := &MiddlewareAuditReport{}
	for _, scope := range reg.collectScopes(r) {
		for _, rule := range reg.rules {
			beforeIdx := indexOfName(scope.chain, rule.before)
			afterIdx := indexOfName(scope.chain, rule.after)
			if beforeIdx < 0 && afterIdx < 0 {
				// Neither middleware participates in this chain; the
				// rule does not apply here.
				continue
			}
			if beforeIdx < 0 {
				report.Missing = append(report.Missing, MiddlewareMissingDependency{
					Rule:    rule.String(),
					Missing: rule.before,
					Scope:   scope.scope,
					Chain:   scope.chain,
				})
				continue
			}
			if afterIdx < 0 {
				// Only the dependency is present; nothing is required of it.
				continue
			}
			if beforeIdx > afterIdx {
				report.Violations = append(report.Violations, MiddlewareOrderViolation{
					Rule:  rule.String(),
					Scope: scope.scope,
					Chain: scope.chain,
				})
			}
		}
	}
	return report
}

// middlewareChainScope is one effective middleware chain: the chain of a
// router at a given mount path, with parent layers prepended.
type middlewareChainScope struct {
	scope string
	chain []string
}

// collectScopes walks the router tree via the Routes interface and computes
// the effective named chain for the root and every mounted subrouter.
func (reg *MiddlewareRegistry) collectScopes(r Routes) []middlewareChainScope {
	var scopes []middlewareChainScope
	var walk func(r Routes, prefix string, parent []string)
	walk = func(r Routes, prefix string, parent []string) {
		chain := make([]string, 0, len(parent)+len(r.Middlewares()))
		chain = append(chain, parent...)
		for _, mw := range r.Middlewares() {
			chain = append(chain, reg.nameOf(mw))
		}
		scopes = append(scopes, middlewareChainScope{scope: prefix, chain: chain})
		for _, route := range r.Routes() {
			if route.SubRoutes == nil {
				continue
			}
			walk(route.SubRoutes, joinScopePath(prefix, route.Pattern), chain)
		}
	}
	walk(r, "/", nil)
	return scopes
}

// nameOf resolves a middleware to its registered name, or "?" when the
// middleware was used without registration (an unknown position).
func (reg *MiddlewareRegistry) nameOf(mw func(http.Handler) http.Handler) string {
	if name, ok := reg.names[reflect.ValueOf(mw).Pointer()]; ok {
		return name
	}
	return "?"
}

// joinScopePath joins a mount pattern onto a parent scope path, normalizing
// the wildcard suffix chi adds for mounted subrouters.
func joinScopePath(parent, pattern string) string {
	p := strings.TrimSuffix(pattern, "*")
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return parent
	}
	if parent == "/" {
		return p
	}
	return parent + p
}

func indexOfName(chain []string, name string) int {
	for i, n := range chain {
		if n == name {
			return i
		}
	}
	return -1
}
