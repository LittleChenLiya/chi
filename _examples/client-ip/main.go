// This example prints the client IP that [middleware.ClientIPFromTrustedProxies]
// parses out of X-Forwarded-For for a few hand-built requests — no server,
// no network, no DNS. Run it locally:
//
//	$ go run main.go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func main() {
	r := chi.NewRouter()
	r.Use(middleware.ClientIPFromTrustedProxies("10.0.0.0/8"))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, middleware.GetClientIP(r.Context()))
	})

	show := func(remoteAddr, xff string) {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remoteAddr
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		fmt.Printf("remote=%-18s xff=%-40s -> client=%s\n", remoteAddr, xff, w.Body.String())
	}

	// Trusted direct peer: XFF is walked right-to-left; the first entry
	// outside the trusted CIDRs is the client.
	show("10.0.0.2:54321", "203.0.113.7, 10.0.0.1")

	// Untrusted direct peer: XFF is ignored and the peer itself is the client.
	show("192.0.2.5:54321", "203.0.113.7, 10.0.0.1")

	// Trusted peer, but a malformed XFF header: fall back to the peer.
	show("10.0.0.2:54321", "203.0.113.7, garbage, 10.0.0.1")

	// Trusted peer, no XFF header: the peer is the best we have.
	show("10.0.0.2:54321", "")
}
