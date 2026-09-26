// hmac-auth is a ~90 second self-contained demo of the middleware.HMACAuth
// request-signing middleware. It spins up a loopback server, signs requests
// with a demo key and prints the HTTP status code and the downstream
// handler count for each scenario:
//
//  1. valid signed request (repeated query params) -> 200
//  2. one flipped body byte after signing          -> 401, handler count frozen
//  3. replay of the valid request (same nonce)     -> 401
//  4. expired timestamp                            -> 401
//
// Run it with:
//
//	go run ./_examples/hmac-auth
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

var demoSecret = []byte("demo-secret-key-0123456789abcdef")

func main() {
	var hits atomic.Int32

	r := chi.NewRouter()
	r.Use(middleware.HMACAuth(middleware.HMACAuthConfig{
		KeyStore: middleware.HMACKeyFunc(func(keyID string) ([]byte, bool) {
			if keyID == "demo-key" {
				return demoSecret, true
			}
			return nil, false
		}),
		NonceStore: middleware.NewMemoryNonceStore(),
		TimeWindow: time.Minute,
	}))
	r.Post("/upload", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body) // body is still fully readable here
		hits.Add(1)
		fmt.Printf("    handler received %d bytes\n", len(body))
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(r)
	defer srv.Close()
	fmt.Printf("loopback server: %s\n", srv.URL)

	send := func(label, target string, body, signBody []byte, nonce string, ts time.Time) {
		req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			panic(err)
		}
		if err := middleware.SignHMACRequest(req, demoSecret, "demo-key", nonce, ts, signBody); err != nil {
			panic(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			panic(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		fmt.Printf("%-52s -> status %d, handler count %d\n", label, resp.StatusCode, hits.Load())
	}

	url := srv.URL + "/upload?b=2&a=1&a=2&empty=&b=1"
	body := []byte("hello signed world")

	// 1. Valid signed request with repeated query parameters.
	send("1. valid signed request (repeated query params)", url, body, body, "nonce-1", time.Now())

	// 2. Same signature, but one body byte flipped.
	tampered := append([]byte(nil), body...)
	tampered[0] ^= 0x01
	send("2. one body byte flipped after signing", url, tampered, body, "nonce-2", time.Now())

	// 3. Replay the valid request: same nonce as step 1.
	send("3. replay with already-used nonce", url, body, body, "nonce-1", time.Now())

	// 4. Expired timestamp (outside the one-minute window).
	send("4. expired timestamp", url, body, body, "nonce-3", time.Now().Add(-10*time.Minute))

	fmt.Printf("done: %d of 4 requests reached the handler\n", hits.Load())
}
