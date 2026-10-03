package middleware_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Example_clientIPFromTrustedProxies shows the trusted-proxy deployment:
// X-Forwarded-For is only honored when the direct connection peer is a
// trusted proxy. The parsed result is printed locally for each request.
func Example_clientIPFromTrustedProxies() {
	r := chi.NewRouter()
	r.Use(middleware.ClientIPFromTrustedProxies("10.0.0.0/8"))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, middleware.GetClientIP(r.Context()))
	})

	// The direct peer (10.0.0.2) is trusted, so X-Forwarded-For is walked
	// right-to-left: 10.0.0.1 is a trusted hop, 203.0.113.7 is the client.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.2:54321"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	fmt.Print(w.Body.String())

	// A direct peer that is not trusted cannot spoof X-Forwarded-For; the
	// header is ignored and the peer itself is the client IP.
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.RemoteAddr = "192.0.2.5:54321"
	req2.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	fmt.Print(w2.Body.String())

	// A trusted peer with a malformed XFF header falls back to the peer.
	req3 := httptest.NewRequest("GET", "/", nil)
	req3.RemoteAddr = "10.0.0.2:54321"
	req3.Header.Set("X-Forwarded-For", "203.0.113.7, garbage, 10.0.0.1")
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)
	fmt.Print(w3.Body.String())

	// Output:
	// 203.0.113.7
	// 192.0.2.5
	// 10.0.0.2
}
