// Command audit demonstrates chi's named middleware audit.
//
// Named middlewares declare Before/After ordering constraints. Call
// chi.Router.Audit at startup to validate the real Use and Mount chain:
// missing dependencies and order violations are reported separately, the
// chain is never reordered, and no middleware handler is invoked.
package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
)

func main() {
	r := chi.NewRouter()

	// Register named middlewares with ordering constraints.
	r.UseNamed(
		chi.Named("auth", middleware("auth")).Before("audit"),
		chi.Named("audit", middleware("audit")).After("auth"),
	)

	// A mounted subrouter whose effective chain includes the parent chain.
	r.Route("/api", func(r chi.Router) {
		r.UseNamed(chi.Named("ratelimit", middleware("ratelimit")).After("auth"))
		r.Get("/users", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("users"))
		})
	})

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	// Validate the real registered chain at startup.
	if err := r.Audit(); err != nil {
		var ae *chi.MiddlewareAuditError
		if errors.As(err, &ae) {
			for _, v := range ae.OrderViolations {
				fmt.Fprintf(os.Stderr, "order violation: %s\n", v)
			}
			for _, d := range ae.MissingDependencies {
				fmt.Fprintf(os.Stderr, "missing dependency: %s\n", d)
			}
		}
		os.Exit(1)
	}

	fmt.Println("middleware chain OK")
	if err := http.ListenAndServe(":3333", r); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func middleware(name string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Middleware", name)
			next.ServeHTTP(w, r)
		})
	}
}
