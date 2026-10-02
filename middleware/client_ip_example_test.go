package middleware_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Example_clientIP shows how to choose a ClientIPFrom* middleware. There is
// no safe default — pick exactly ONE based on your network setup:
//
//   - [middleware.ClientIPFromRemoteAddr] — this server is directly on the
//     public internet, with no reverse proxy in front.
//
//   - [middleware.ClientIPFromHeader] — your reverse proxy sets a dedicated
//     single-IP header (X-Real-IP, CF-Connecting-IP, X-Client-IP) and
//     overwrites any client-supplied value for every request.
//
//   - [middleware.ClientIPFromXFF] — you sit behind one or more reverse
//     proxies whose IP ranges you can enumerate (your VPC CIDR, Cloudflare's
//     published IP list, etc.).
//
//   - [middleware.ClientIPFromXFFTrustedPeer] — like ClientIPFromXFF, but
//     additionally requires the direct TCP peer itself to be inside a
//     trusted CIDR before X-Forwarded-For is consulted at all, and falls
//     back to the peer on any degenerate header.
//
//   - [middleware.ClientIPFromXFFTrustedProxies] — you sit behind a known,
//     fixed number of reverse proxies whose IPs are dynamic (autoscaling
//     pools, ephemeral containers).
//
// Read the resulting IP with [middleware.GetClientIP] (string) or
// [middleware.GetClientIPAddr] ([net/netip.Addr]). These middlewares never
// mutate [net/http.Request.RemoteAddr].
//
// The legacy [middleware.RealIP] middleware is deprecated; it is vulnerable
// to IP spoofing (GHSA-3fxj-6jh8-hvhx, GHSA-rjr7-jggh-pgcp, GHSA-9g5q-2w5x-hmxf).
//
// Background:
// https://github.com/go-chi/chi/pull/967
// https://adam-p.ca/blog/2022/03/x-forwarded-for/
func Example_clientIP() {
	r := chi.NewRouter()

	// Pick ONE; the others are shown commented for reference.

	// (1) Behind a reverse proxy that sets a single-IP header for every
	// request and overwrites any client-supplied value (Cloudflare, Nginx
	// with ngx_http_realip_module, Apache with mod_remoteip, ...).
	r.Use(middleware.ClientIPFromHeader("CF-Connecting-IP"))

	// (2) Behind reverse proxies whose IP ranges you can enumerate as CIDRs.
	// The middleware walks X-Forwarded-For right-to-left, skipping trusted
	// entries; fail-closed on garbage.
	//
	// r.Use(middleware.ClientIPFromXFF(
	//     "13.32.0.0/15",   // CloudFront IPv4.
	//     "2600:9000::/28", // CloudFront IPv6.
	// ))

	// (3) Behind a known number of trusted reverse proxies whose IPs are
	// dynamic.
	//
	// r.Use(middleware.ClientIPFromXFFTrustedProxies(2))

	// (4) Directly on the public internet, no reverse proxy in front.
	//
	// r.Use(middleware.ClientIPFromRemoteAddr)

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, middleware.GetClientIP(r.Context()))
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("CF-Connecting-IP", "198.51.100.42")
	req.Header.Set("X-Forwarded-For", "1.2.3.4") // attacker-supplied; ignored.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	fmt.Print(w.Body.String())
	// Output: 198.51.100.42
}

// Example_clientIPFromXFFTrustedProxies shows that with two proxies the
// client sits at XFF[len-2] and any client-prepended entries to the left of
// that position are ignored.
func Example_clientIPFromXFFTrustedProxies() {
	r := chi.NewRouter()
	r.Use(middleware.ClientIPFromXFFTrustedProxies(2)) // P1 (inner) + P2 (outer).
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, middleware.GetClientIP(r.Context()))
	})

	// Each chain ends in "<client>, <P1>"; P2's address is in RemoteAddr,
	// not in X-Forwarded-For. Leading entries are client-controlled.
	for _, xff := range []string{
		"109.81.118.93, 3.172.119.73",
		"8.8.8.8, 109.81.118.93, 3.172.119.73",
		"8.8.8.8, 8.8.8.8, 8.8.8.8, 109.81.118.93, 3.172.119.73",
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("X-Forwarded-For", xff)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		fmt.Print(w.Body.String())
	}
	// Output:
	// 109.81.118.93
	// 109.81.118.93
	// 109.81.118.93
}

// Example_clientIPFromXFFTrustedPeer resolves the client IP locally with
// httptest and prints it. X-Forwarded-For is only honoured because the
// direct peer (RemoteAddr) is inside the trusted CIDR 10.0.0.0/8; the
// trusted hop 10.0.0.1 is stripped right-to-left and the first untrusted
// entry, 203.0.113.7, is the client. From an untrusted peer the same
// header is ignored and the peer itself is reported.
func Example_clientIPFromXFFTrustedPeer() {
	r := chi.NewRouter()
	r.Use(middleware.ClientIPFromXFFTrustedPeer("10.0.0.0/8"))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, middleware.GetClientIP(r.Context()))
	})

	// Trusted direct peer: header honoured.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.2:8080"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	fmt.Print(w.Body.String())

	// Untrusted direct peer: same header ignored, peer reported.
	req = httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.99:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	fmt.Print(w.Body.String())

	// Output:
	// 203.0.113.7
	// 203.0.113.99
}
