// Observe middleware demonstration.
//
// Run with:
//
//	go run ./_examples/observe
//
// Then try, in another terminal:
//
//	curl -i http://localhost:3333/
//	curl -i http://localhost:3333/stream
//	curl -i http://localhost:3333/early-hints
//	curl -i http://localhost:3333/panic
//
// Every request prints exactly one observation line with the final status
// code, the bytes the underlying writer actually accepted, and the reason the
// response processing ended (complete, client_canceled, panic or hijacked).
package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func main() {
	r := chi.NewRouter()

	r.Use(middleware.Observe(func(req *http.Request, obs middleware.Observation) {
		extra := ""
		if obs.Err != nil {
			extra = " err=" + obs.Err.Error()
		}
		if obs.Panic != nil {
			extra = fmt.Sprintf(" panic=%v", obs.Panic)
		}
		log.Printf("observed method=%s path=%s status=%d bytes=%d reason=%s%s",
			req.Method, req.URL.Path, obs.Status, obs.Bytes, obs.Reason, extra)
	}))

	r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ordinary response\n"))
	})

	r.Get("/stream", func(w http.ResponseWriter, _ *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		for i := 0; i < 3; i++ {
			_, _ = fmt.Fprintf(w, "chunk %d at %s\n", i+1, time.Now().Format(time.RFC3339))
			fl.Flush()
			time.Sleep(500 * time.Millisecond)
		}
	})

	r.Get("/early-hints", func(w http.ResponseWriter, _ *http.Request) {
		// 103 is informational: it is forwarded but never becomes the final
		// recorded status.
		w.Header().Set("Link", "</style.css>; rel=preload; as=style")
		w.WriteHeader(http.StatusEarlyHints)
		_, _ = w.Write([]byte("final body after 103\n"))
	})

	r.Get("/panic", func(http.ResponseWriter, *http.Request) {
		panic("intentional demo panic; the observation is still emitted once")
	})

	addr := ":3333"
	log.Printf("observe demo listening on http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, r))
}
