package chi

import (
	"net/http"
	"strings"
	"testing"
)

func testMiddleware(name string, called *([]string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if called != nil {
				*called = append(*called, name)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// TestMiddlewareRegistry_OrderOK verifies that registering auth before audit
// (with audit declared After("auth")) passes the audit.
func TestMiddlewareRegistry_OrderOK(t *testing.T) {
	reg := NewMiddlewareRegistry()
	auth := reg.Register("auth", testMiddleware("auth", nil))
	audit := reg.Register("audit", testMiddleware("audit", nil), After("auth"))

	r := NewRouter()
	r.Use(auth)
	r.Use(audit)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	if err := reg.Audit(r); err != nil {
		t.Fatalf("expected clean audit, got: %v", err)
	}
}

// TestMiddlewareRegistry_ReversedUse verifies that using audit before auth
// is reported as an order violation that includes the actual chain.
func TestMiddlewareRegistry_ReversedUse(t *testing.T) {
	reg := NewMiddlewareRegistry()
	auth := reg.Register("auth", testMiddleware("auth", nil))
	audit := reg.Register("audit", testMiddleware("audit", nil), After("auth"))

	r := NewRouter()
	r.Use(audit) // wrong order on purpose
	r.Use(auth)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	err := reg.Audit(r)
	if err == nil {
		t.Fatal("expected audit error for reversed Use order")
	}
	report, ok := err.(*MiddlewareAuditReport)
	if !ok {
		t.Fatalf("expected *MiddlewareAuditReport, got %T", err)
	}
	if len(report.Violations) != 1 {
		t.Fatalf("expected 1 order violation, got %d (%v)", len(report.Violations), report.Violations)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("expected no missing dependencies, got %v", report.Missing)
	}
	v := report.Violations[0]
	if v.Scope != "/" {
		t.Fatalf("expected scope %q, got %q", "/", v.Scope)
	}
	got := strings.Join(v.Chain, ", ")
	if got != "audit, auth" {
		t.Fatalf("expected actual chain %q, got %q", "audit, auth", got)
	}
	if !strings.Contains(err.Error(), "[audit, auth]") {
		t.Fatalf("expected error to report the actual chain, got:\n%s", err)
	}
}

// TestMiddlewareRegistry_ParentAuthChildAudit verifies the Mount path: auth
// on the parent router and audit on a mounted subrouter is legal, because
// the effective chain of the subrouter includes the parent layers.
func TestMiddlewareRegistry_ParentAuthChildAudit(t *testing.T) {
	reg := NewMiddlewareRegistry()
	auth := reg.Register("auth", testMiddleware("auth", nil))
	audit := reg.Register("audit", testMiddleware("audit", nil), After("auth"))

	sub := NewRouter()
	sub.Use(audit)
	sub.Get("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	r := NewRouter()
	r.Use(auth)
	r.Mount("/api", sub)

	if err := reg.Audit(r); err != nil {
		t.Fatalf("expected clean audit for parent auth + child audit, got: %v", err)
	}
}

// TestMiddlewareRegistry_MountScopeReported verifies that a reversed chain
// inside a mounted subrouter is reported with the mount path as scope and
// the effective chain including parent layers.
func TestMiddlewareRegistry_MountScopeReported(t *testing.T) {
	reg := NewMiddlewareRegistry()
	auth := reg.Register("auth", testMiddleware("auth", nil))
	audit := reg.Register("audit", testMiddleware("audit", nil), After("auth"))

	sub := NewRouter()
	sub.Use(audit, auth) // reversed on purpose
	sub.Get("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	r := NewRouter()
	r.Use(testMiddleware("logger", nil)) // unnamed: unknown position
	r.Mount("/api", sub)

	err := reg.Audit(r)
	if err == nil {
		t.Fatal("expected audit error for reversed chain in mounted subrouter")
	}
	report := err.(*MiddlewareAuditReport)
	if len(report.Violations) != 1 {
		t.Fatalf("expected 1 violation, got %d (%v)", len(report.Violations), report.Violations)
	}
	v := report.Violations[0]
	if v.Scope != "/api" {
		t.Fatalf("expected scope %q, got %q", "/api", v.Scope)
	}
	got := strings.Join(v.Chain, ", ")
	if got != "?, audit, auth" {
		t.Fatalf("expected effective chain %q, got %q", "?, audit, auth", got)
	}
}

// TestMiddlewareRegistry_MissingDependency verifies that using audit without
// auth is reported separately as a missing dependency, not as an order
// violation.
func TestMiddlewareRegistry_MissingDependency(t *testing.T) {
	reg := NewMiddlewareRegistry()
	reg.Register("auth", testMiddleware("auth", nil))
	audit := reg.Register("audit", testMiddleware("audit", nil), After("auth"))

	r := NewRouter()
	r.Use(audit)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	err := reg.Audit(r)
	if err == nil {
		t.Fatal("expected audit error for missing auth")
	}
	report := err.(*MiddlewareAuditReport)
	if len(report.Violations) != 0 {
		t.Fatalf("expected no order violations, got %v", report.Violations)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("expected 1 missing dependency, got %v", report.Missing)
	}
	m := report.Missing[0]
	if m.Missing != "auth" {
		t.Fatalf("expected missing dependency %q, got %q", "auth", m.Missing)
	}
	if !strings.Contains(err.Error(), "missing dependencies:") {
		t.Fatalf("expected separate missing-dependency section, got:\n%s", err)
	}
}

// TestMiddlewareRegistry_UnknownDoesNotSatisfy verifies that an unnamed
// middleware stays an unknown position ("?") in the chain and never
// satisfies a dependency.
func TestMiddlewareRegistry_UnknownDoesNotSatisfy(t *testing.T) {
	reg := NewMiddlewareRegistry()
	audit := reg.Register("audit", testMiddleware("audit", nil), After("auth"))
	// Note: "auth" is declared as a dependency but never registered nor used;
	// an unnamed middleware in the chain must not stand in for it.
	unnamed := testMiddleware("auth-but-unregistered", nil)

	r := NewRouter()
	r.Use(unnamed)
	r.Use(audit)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	err := reg.Audit(r)
	if err == nil {
		t.Fatal("expected audit error: unknown middleware must not satisfy the auth dependency")
	}
	report := err.(*MiddlewareAuditReport)
	if len(report.Missing) != 1 {
		t.Fatalf("expected 1 missing dependency, got %v", report.Missing)
	}
	got := strings.Join(report.Missing[0].Chain, ", ")
	if got != "?, audit" {
		t.Fatalf("expected actual chain %q, got %q", "?, audit", got)
	}
}

// TestMiddlewareRegistry_DoesNotInvokeHandlers verifies the audit never
// invokes any middleware or handler. Note that chi itself invokes middleware
// functions once while building the route chains (at Get/Mount time), so the
// counters are snapshot after router construction: the audit must not move
// them.
func TestMiddlewareRegistry_DoesNotInvokeHandlers(t *testing.T) {
	var builds, serves int
	counting := func(next http.Handler) http.Handler {
		builds++
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serves++
			next.ServeHTTP(w, r)
		})
	}
	// Distinct func values per name: a func value can only carry one name.
	authMw := counting
	auditMw := func(next http.Handler) http.Handler { return counting(next) }

	reg := NewMiddlewareRegistry()
	auth := reg.Register("auth", authMw)
	audit := reg.Register("audit", auditMw, After("auth"))

	handler := func(w http.ResponseWriter, r *http.Request) {
		serves++
		w.WriteHeader(200)
	}

	r := NewRouter()
	r.Use(auth)
	r.Use(audit)
	r.Get("/", handler)

	sub := NewRouter()
	sub.Use(audit)
	sub.Get("/", handler)
	r.Mount("/api", sub)

	buildsBefore, servesBefore := builds, serves

	if err := reg.Audit(r); err != nil {
		t.Fatalf("expected clean audit, got: %v", err)
	}
	if builds != buildsBefore {
		t.Fatalf("audit invoked middleware functions %d time(s)", builds-buildsBefore)
	}
	if serves != servesBefore {
		t.Fatalf("audit served %d request(s)", serves-servesBefore)
	}
}

// TestMiddlewareRegistry_BeforeConstraint verifies the Before() form of
// declaring constraints.
func TestMiddlewareRegistry_BeforeConstraint(t *testing.T) {
	reg := NewMiddlewareRegistry()
	auth := reg.Register("auth", testMiddleware("auth", nil), Before("audit"))
	audit := reg.Register("audit", testMiddleware("audit", nil))

	r := NewRouter()
	r.Use(audit)
	r.Use(auth)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	err := reg.Audit(r)
	if err == nil {
		t.Fatal("expected audit error for reversed order declared via Before")
	}
	report := err.(*MiddlewareAuditReport)
	if len(report.Violations) != 1 {
		t.Fatalf("expected 1 violation, got %v", report.Violations)
	}
}

// TestMiddlewareRegistry_MustAuditPanics verifies the startup wiring helper.
func TestMiddlewareRegistry_MustAuditPanics(t *testing.T) {
	reg := NewMiddlewareRegistry()
	audit := reg.Register("audit", testMiddleware("audit", nil), After("auth"))

	r := NewRouter()
	r.Use(audit)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	defer func() {
		if recover() == nil {
			t.Fatal("expected MustAudit to panic on findings")
		}
	}()
	reg.MustAudit(r)
}
