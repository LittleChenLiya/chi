package chi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// namedMW returns a middleware that records its name into the supplied log so
// tests can observe execution order and verify Audit does not call handlers.
func namedMW(name string, log *[]string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if log != nil {
				*log = append(*log, name)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func auditErr(t *testing.T, r *Mux) *MiddlewareAuditError {
	t.Helper()
	err := r.Audit()
	if err == nil {
		t.Fatalf("expected audit error, got nil")
	}
	var ae *MiddlewareAuditError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *MiddlewareAuditError, got %T: %v", err, err)
	}
	return ae
}

func TestNamedMiddlewareLegalOrder(t *testing.T) {
	r := NewRouter()
	r.UseNamed(Named("auth", namedMW("auth", nil)).Before("audit"))
	r.UseNamed(Named("audit", namedMW("audit", nil)))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	if err := r.Audit(); err != nil {
		t.Fatalf("expected no audit error, got %v", err)
	}
}

func TestNamedMiddlewareLegalAfter(t *testing.T) {
	r := NewRouter()
	r.UseNamed(Named("auth", namedMW("auth", nil)))
	r.UseNamed(Named("audit", namedMW("audit", nil)).After("auth"))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	if err := r.Audit(); err != nil {
		t.Fatalf("expected no audit error, got %v", err)
	}
}

func TestNamedMiddlewareReversedUseReportsChain(t *testing.T) {
	r := NewRouter()
	// audit is registered before auth, but auth must precede audit.
	r.UseNamed(Named("audit", namedMW("audit", nil)))
	r.UseNamed(Named("auth", namedMW("auth", nil)).Before("audit"))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	ae := auditErr(t, r)
	if len(ae.OrderViolations) != 1 {
		t.Fatalf("expected 1 order violation, got %d", len(ae.OrderViolations))
	}
	v := ae.OrderViolations[0]
	if v.Name != "auth" || v.Requires != "audit" || !v.Before {
		t.Errorf("unexpected violation: %+v", v)
	}
	want := []string{"audit", "auth"}
	if strings.Join(v.Chain, ",") != strings.Join(want, ",") {
		t.Errorf("expected chain %v, got %v", want, v.Chain)
	}
	if len(ae.MissingDependencies) != 0 {
		t.Errorf("expected no missing dependencies, got %v", ae.MissingDependencies)
	}
}

func TestNamedMiddlewareReversedAfterReportsChain(t *testing.T) {
	r := NewRouter()
	// auth is registered after audit, but audit must follow auth.
	r.UseNamed(Named("audit", namedMW("audit", nil)).After("auth"))
	r.UseNamed(Named("auth", namedMW("auth", nil)))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	ae := auditErr(t, r)
	if len(ae.OrderViolations) != 1 {
		t.Fatalf("expected 1 order violation, got %d", len(ae.OrderViolations))
	}
	v := ae.OrderViolations[0]
	if v.Name != "audit" || v.Requires != "auth" || v.Before {
		t.Errorf("unexpected violation: %+v", v)
	}
	want := []string{"audit", "auth"}
	if strings.Join(v.Chain, ",") != strings.Join(want, ",") {
		t.Errorf("expected chain %v, got %v", want, v.Chain)
	}
}

func TestNamedMiddlewareMissingDependency(t *testing.T) {
	r := NewRouter()
	r.UseNamed(Named("audit", namedMW("audit", nil)).After("auth"))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	ae := auditErr(t, r)
	if len(ae.MissingDependencies) != 1 {
		t.Fatalf("expected 1 missing dependency, got %d", len(ae.MissingDependencies))
	}
	d := ae.MissingDependencies[0]
	if d.Name != "audit" || d.Requires != "auth" || d.Before {
		t.Errorf("unexpected missing dependency: %+v", d)
	}
	if len(ae.OrderViolations) != 0 {
		t.Errorf("expected no order violations, got %v", ae.OrderViolations)
	}
}

func TestNamedMiddlewareMissingBeforeDependency(t *testing.T) {
	r := NewRouter()
	r.UseNamed(Named("auth", namedMW("auth", nil)).Before("audit"))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	ae := auditErr(t, r)
	if len(ae.MissingDependencies) != 1 {
		t.Fatalf("expected 1 missing dependency, got %d", len(ae.MissingDependencies))
	}
	d := ae.MissingDependencies[0]
	if d.Name != "auth" || d.Requires != "audit" || !d.Before {
		t.Errorf("unexpected missing dependency: %+v", d)
	}
}

func TestNamedMiddlewareUnnamedDoesNotSatisfy(t *testing.T) {
	r := NewRouter()
	// An unnamed middleware must not satisfy the auth dependency.
	r.Use(namedMW("unnamed", nil))
	r.UseNamed(Named("audit", namedMW("audit", nil)).After("auth"))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	ae := auditErr(t, r)
	if len(ae.MissingDependencies) != 1 {
		t.Fatalf("expected 1 missing dependency, got %d", len(ae.MissingDependencies))
	}
	if ae.MissingDependencies[0].Requires != "auth" {
		t.Errorf("expected missing auth, got %q", ae.MissingDependencies[0].Requires)
	}
}

func TestNamedMiddlewareChainReportsUnnamed(t *testing.T) {
	r := NewRouter()
	r.Use(namedMW("unnamed", nil))
	r.UseNamed(Named("audit", namedMW("audit", nil)))
	r.UseNamed(Named("auth", namedMW("auth", nil)).Before("audit"))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	ae := auditErr(t, r)
	if len(ae.OrderViolations) != 1 {
		t.Fatalf("expected 1 order violation, got %d", len(ae.OrderViolations))
	}
	want := []string{"<unnamed>", "audit", "auth"}
	if strings.Join(ae.OrderViolations[0].Chain, ",") != strings.Join(want, ",") {
		t.Errorf("expected chain %v, got %v", want, ae.OrderViolations[0].Chain)
	}
}

func TestNamedMiddlewareParentChildMountLegal(t *testing.T) {
	r := NewRouter()
	r.UseNamed(Named("auth", namedMW("auth", nil)))
	sub := NewRouter()
	sub.UseNamed(Named("audit", namedMW("audit", nil)).After("auth"))
	sub.Get("/x", func(w http.ResponseWriter, r *http.Request) {})
	r.Mount("/api", sub)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	if err := r.Audit(); err != nil {
		t.Fatalf("expected no audit error for parent auth + child audit, got %v", err)
	}
}

func TestNamedMiddlewareParentChildMountViolation(t *testing.T) {
	r := NewRouter()
	// Parent has audit, child has auth before audit: child effective chain is
	// [audit, auth], which violates auth before audit.
	r.UseNamed(Named("audit", namedMW("audit", nil)))
	sub := NewRouter()
	sub.UseNamed(Named("auth", namedMW("auth", nil)).Before("audit"))
	sub.Get("/x", func(w http.ResponseWriter, r *http.Request) {})
	r.Mount("/api", sub)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	ae := auditErr(t, r)
	if len(ae.OrderViolations) != 1 {
		t.Fatalf("expected 1 order violation, got %d", len(ae.OrderViolations))
	}
	v := ae.OrderViolations[0]
	if v.Path != "/api" {
		t.Errorf("expected mount path /api, got %q", v.Path)
	}
	want := []string{"audit", "auth"}
	if strings.Join(v.Chain, ",") != strings.Join(want, ",") {
		t.Errorf("expected chain %v, got %v", want, v.Chain)
	}
}

func TestNamedMiddlewareMountPathDiagnostics(t *testing.T) {
	r := NewRouter()
	sub := NewRouter()
	sub.UseNamed(Named("audit", namedMW("audit", nil)).After("auth"))
	sub.Get("/x", func(w http.ResponseWriter, r *http.Request) {})
	r.Mount("/api", sub)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	ae := auditErr(t, r)
	if len(ae.MissingDependencies) != 1 {
		t.Fatalf("expected 1 missing dependency, got %d", len(ae.MissingDependencies))
	}
	if ae.MissingDependencies[0].Path != "/api" {
		t.Errorf("expected mount path /api, got %q", ae.MissingDependencies[0].Path)
	}
}

func TestNamedMiddlewareNestedMountPath(t *testing.T) {
	r := NewRouter()
	sub := NewRouter()
	sub2 := NewRouter()
	sub2.UseNamed(Named("audit", namedMW("audit", nil)).After("auth"))
	sub2.Get("/y", func(w http.ResponseWriter, r *http.Request) {})
	sub.Mount("/v1", sub2)
	sub.Get("/x", func(w http.ResponseWriter, r *http.Request) {})
	r.Mount("/api", sub)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	ae := auditErr(t, r)
	if len(ae.MissingDependencies) != 1 {
		t.Fatalf("expected 1 missing dependency, got %d", len(ae.MissingDependencies))
	}
	if ae.MissingDependencies[0].Path != "/api/v1" {
		t.Errorf("expected nested mount path /api/v1, got %q", ae.MissingDependencies[0].Path)
	}
}

func TestNamedMiddlewareNestedMountLegal(t *testing.T) {
	r := NewRouter()
	r.UseNamed(Named("auth", namedMW("auth", nil)))
	sub := NewRouter()
	sub.UseNamed(Named("session", namedMW("session", nil)).After("auth"))
	sub2 := NewRouter()
	sub2.UseNamed(Named("audit", namedMW("audit", nil)).After("session"))
	sub2.Get("/y", func(w http.ResponseWriter, r *http.Request) {})
	sub.Mount("/v1", sub2)
	sub.Get("/x", func(w http.ResponseWriter, r *http.Request) {})
	r.Mount("/api", sub)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	if err := r.Audit(); err != nil {
		t.Fatalf("expected no audit error for nested mount chain, got %v", err)
	}
}

func TestNamedMiddlewareAuditDoesNotInvokeHandlers(t *testing.T) {
	log := []string{}
	r := NewRouter()
	r.UseNamed(Named("auth", namedMW("auth", &log)).Before("audit"))
	r.UseNamed(Named("audit", namedMW("audit", &log)))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	_ = r.Audit()
	if len(log) != 0 {
		t.Fatalf("Audit invoked middleware handlers: %v", log)
	}
}

func TestNamedMiddlewareInlineGroup(t *testing.T) {
	r := NewRouter()
	r.Group(func(r Router) {
		r.UseNamed(Named("auth", namedMW("auth", nil)))
		r.Group(func(r Router) {
			r.UseNamed(Named("audit", namedMW("audit", nil)).After("auth"))
			r.Get("/x", func(w http.ResponseWriter, r *http.Request) {})
		})
	})

	if err := r.Audit(); err != nil {
		t.Fatalf("expected no audit error for inline group, got %v", err)
	}
}

func TestNamedMiddlewareInlineGroupReversed(t *testing.T) {
	r := NewRouter()
	r.Group(func(r Router) {
		r.UseNamed(Named("audit", namedMW("audit", nil)))
		r.UseNamed(Named("auth", namedMW("auth", nil)).Before("audit"))
		r.Get("/x", func(w http.ResponseWriter, r *http.Request) {})
	})

	ae := auditErr(t, r)
	if len(ae.OrderViolations) != 1 {
		t.Fatalf("expected 1 order violation, got %d", len(ae.OrderViolations))
	}
	want := []string{"audit", "auth"}
	if strings.Join(ae.OrderViolations[0].Chain, ",") != strings.Join(want, ",") {
		t.Errorf("expected chain %v, got %v", want, ae.OrderViolations[0].Chain)
	}
}

func TestNamedMiddlewareMultipleViolationsAndMissing(t *testing.T) {
	r := NewRouter()
	r.UseNamed(Named("audit", namedMW("audit", nil)).After("auth"))
	r.UseNamed(Named("auth", namedMW("auth", nil)).Before("audit"))
	r.UseNamed(Named("log", namedMW("log", nil)).After("missing"))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	ae := auditErr(t, r)
	if len(ae.OrderViolations) != 2 {
		t.Errorf("expected 2 order violations, got %d", len(ae.OrderViolations))
	}
	if len(ae.MissingDependencies) != 1 {
		t.Errorf("expected 1 missing dependency, got %d", len(ae.MissingDependencies))
	}
}

func TestNamedMiddlewareSelfReferenceIgnored(t *testing.T) {
	r := NewRouter()
	r.UseNamed(Named("auth", namedMW("auth", nil)).Before("auth").After("auth"))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	if err := r.Audit(); err != nil {
		t.Fatalf("expected self-reference to be ignored, got %v", err)
	}
}

func TestNamedMiddlewareEmptyRouter(t *testing.T) {
	r := NewRouter()
	if err := r.Audit(); err != nil {
		t.Fatalf("expected no audit error for empty router, got %v", err)
	}
}

func TestNamedMiddlewareExecutionOrder(t *testing.T) {
	log := []string{}
	r := NewRouter()
	r.UseNamed(Named("auth", namedMW("auth", &log)).Before("audit"))
	r.UseNamed(Named("audit", namedMW("audit", &log)))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	// Audit must not change execution order.
	if err := r.Audit(); err != nil {
		t.Fatalf("unexpected audit error: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	want := []string{"auth", "audit"}
	if strings.Join(log, ",") != strings.Join(want, ",") {
		t.Errorf("expected execution order %v, got %v", want, log)
	}
}

func TestNamedMiddlewareMountExecutionOrder(t *testing.T) {
	log := []string{}
	r := NewRouter()
	r.UseNamed(Named("auth", namedMW("auth", &log)))
	sub := NewRouter()
	sub.UseNamed(Named("audit", namedMW("audit", &log)).After("auth"))
	sub.Get("/x", func(w http.ResponseWriter, r *http.Request) {})
	r.Mount("/api", sub)

	if err := r.Audit(); err != nil {
		t.Fatalf("unexpected audit error: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	want := []string{"auth", "audit"}
	if strings.Join(log, ",") != strings.Join(want, ",") {
		t.Errorf("expected execution order %v, got %v", want, log)
	}
}

func TestNamedMiddlewareErrorString(t *testing.T) {
	r := NewRouter()
	r.UseNamed(Named("audit", namedMW("audit", nil)))
	r.UseNamed(Named("auth", namedMW("auth", nil)).Before("audit"))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})

	err := r.Audit()
	if err == nil {
		t.Fatalf("expected audit error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "order violation") || !strings.Contains(msg, "missing dependency") {
		t.Errorf("error message should mention both categories, got: %s", msg)
	}
	if !strings.Contains(msg, "[audit -> auth]") {
		t.Errorf("error message should contain the actual chain, got: %s", msg)
	}
}

func TestNamedMiddlewareUseNamedPanicsAfterRoutes(t *testing.T) {
	r := NewRouter()
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})
	defer func() {
		if recover() == nil {
			t.Fatal("expected UseNamed to panic after routes are registered")
		}
	}()
	r.UseNamed(Named("auth", namedMW("auth", nil)))
}

func TestNamedMiddlewareNilPanics(t *testing.T) {
	r := NewRouter()
	defer func() {
		if recover() == nil {
			t.Fatal("expected UseNamed(nil) to panic")
		}
	}()
	r.UseNamed(nil)
}
