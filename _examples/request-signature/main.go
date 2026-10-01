// Command request-signature is a short loopback demo of the
// middleware.RequestSignature middleware. It starts a local chi server,
// sends signed and tampered requests, and prints the resulting status codes
// and handler invocation counts. It runs in well under 90 seconds.
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

var (
	demoKey   = []byte("demo-secret-key-0123456789abcdef")
	demoKeyID = "demo-key"
)

func main() {
	var handlerCount int64

	r := chi.NewRouter()
	r.Use(middleware.RequestSignature(middleware.RequestSignatureConfig{
		KeyLookup: func(keyID string) ([]byte, bool) {
			if keyID == demoKeyID {
				return demoKey, true
			}
			return nil, false
		},
		NonceStore: middleware.NewMemoryNonceStore(),
	}))
	r.Post("/submit", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&handlerCount, 1)
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	})

	srv := httptest.NewServer(r)
	defer srv.Close()
	fmt.Println("loopback server:", srv.URL)

	send := func(label string, build func() *http.Request) {
		resp, err := http.DefaultClient.Do(build())
		if err != nil {
			fmt.Printf("%-28s error: %v\n", label, err)
			return
		}
		resp.Body.Close()
		fmt.Printf("%-28s status=%d handler-count=%d\n", label, resp.StatusCode, atomic.LoadInt64(&handlerCount))
	}

	body := []byte("{\"hello\":\"world\"}")
	nonce := 0
	signed := func(mutate func(req *http.Request)) *http.Request {
		nonce++
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/submit?b=2&a=1&a=", bytes.NewReader(body))
		if err != nil {
			panic(err)
		}
		middleware.SignRequest(demoKey, demoKeyID, fmt.Sprintf("nonce-%d", nonce), time.Now(), req, body)
		if mutate != nil {
			mutate(req)
		}
		return req
	}

	send("valid signed request", func() *http.Request { return signed(nil) })
	send("tampered body (1 byte)", func() *http.Request {
		return signed(func(req *http.Request) {
			tampered := append([]byte(nil), body...)
			tampered[3] ^= 0x01
			req.Body = io.NopCloser(bytes.NewReader(tampered))
		})
	})

	replayNonce := fmt.Sprintf("nonce-%d", nonce+1)
	replayTS := time.Now()
	send("first request (nonce A)", func() *http.Request {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/submit", bytes.NewReader(body))
		if err != nil {
			panic(err)
		}
		middleware.SignRequest(demoKey, demoKeyID, replayNonce, replayTS, req, body)
		return req
	})
	send("replayed same nonce", func() *http.Request {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/submit", bytes.NewReader(body))
		if err != nil {
			panic(err)
		}
		middleware.SignRequest(demoKey, demoKeyID, replayNonce, replayTS, req, body)
		return req
	})

	send("expired timestamp", func() *http.Request {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/submit", bytes.NewReader(body))
		if err != nil {
			panic(err)
		}
		middleware.SignRequest(demoKey, demoKeyID, "nonce-expired", time.Now().Add(-time.Hour), req, body)
		return req
	})

	fmt.Printf("final handler count: %d\n", atomic.LoadInt64(&handlerCount))
}
