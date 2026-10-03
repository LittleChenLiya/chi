package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

func TestWithRequestLabelsOverride(t *testing.T) {
	outer := WithRequestLabels(context.Background(), map[string]string{"tenant": "A"})
	inner := WithRequestLabels(outer, map[string]string{"tenant": "B", "job": "1"})

	if got := GetRequestLabels(inner); !reflect.DeepEqual(got, map[string]string{"tenant": "B", "job": "1"}) {
		t.Fatalf("inner labels = %v, want tenant=B job=1", got)
	}
	if got := GetRequestLabels(outer); !reflect.DeepEqual(got, map[string]string{"tenant": "A"}) {
		t.Fatalf("outer labels = %v, want tenant=A only", got)
	}
	if v, ok := GetRequestLabel(inner, "tenant"); !ok || v != "B" {
		t.Fatalf("GetRequestLabel(inner, tenant) = %q, %v; want B, true", v, ok)
	}
	if _, ok := GetRequestLabel(outer, "job"); ok {
		t.Fatal("outer context unexpectedly carries job label")
	}
}

func TestWithRequestLabelsNilAndEmpty(t *testing.T) {
	// nil labels map is treated as empty.
	ctx := WithRequestLabels(context.Background(), nil)
	if got := GetRequestLabels(ctx); len(got) != 0 {
		t.Fatalf("labels after nil merge = %v, want empty", got)
	}
	// nil context is treated as background.
	ctx = WithRequestLabels(nil, map[string]string{"k": "v"})
	if got := GetRequestLabels(ctx); !reflect.DeepEqual(got, map[string]string{"k": "v"}) {
		t.Fatalf("labels with nil ctx = %v, want k=v", got)
	}
	// reading from a nil context yields an empty, non-nil map.
	if got := GetRequestLabels(nil); got == nil || len(got) != 0 {
		t.Fatalf("GetRequestLabels(nil) = %v, want empty non-nil map", got)
	}
	// keys and values are preserved verbatim, raw bytes untouched.
	raw := map[string]string{"  Spaced Key  ": "v\x00al\n"}
	ctx = WithRequestLabels(context.Background(), raw)
	if got := GetRequestLabels(ctx); !reflect.DeepEqual(got, raw) {
		t.Fatalf("raw labels = %q, want %q", got, raw)
	}
}

func TestRequestLabelsMiddlewareChain(t *testing.T) {
	r := chi.NewRouter()
	r.Use(RequestLabels(map[string]string{"tenant": "A"}))
	r.Route("/inner", func(r chi.Router) {
		r.Use(RequestLabels(map[string]string{"tenant": "B", "job": "1"}))
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			labels := GetRequestLabels(r.Context())
			if labels["tenant"] != "B" || labels["job"] != "1" {
				t.Errorf("inner handler labels = %v, want tenant=B job=1", labels)
			}
			w.WriteHeader(http.StatusOK)
		})
	})
	r.Get("/outer", func(w http.ResponseWriter, r *http.Request) {
		labels := GetRequestLabels(r.Context())
		if !reflect.DeepEqual(labels, map[string]string{"tenant": "A"}) {
			t.Errorf("outer handler labels = %v, want tenant=A only", labels)
		}
		w.WriteHeader(http.StatusOK)
	})

	for _, path := range []string{"/inner", "/outer"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d", path, rec.Code)
		}
	}
}

func TestGetRequestLabelsSnapshotIsolation(t *testing.T) {
	ctx := WithRequestLabels(context.Background(), map[string]string{"tenant": "A"})

	first := GetRequestLabels(ctx)
	first["tenant"] = "hacked"
	first["extra"] = "x"

	second := GetRequestLabels(ctx)
	if !reflect.DeepEqual(second, map[string]string{"tenant": "A"}) {
		t.Fatalf("labels after mutating snapshot = %v, want tenant=A only", second)
	}
}

func TestRequestLabelsLoggerGetsSnapshot(t *testing.T) {
	var logged map[string]string
	mw := RequestLabelsWithLogger(map[string]string{"tenant": "A"}, func(labels map[string]string) {
		logged = labels
	})

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handler mutates its own snapshot; this must not corrupt the
		// snapshot later handed to the log callback.
		labels := GetRequestLabels(r.Context())
		labels["tenant"] = "hacked"
		labels["leak"] = "true"
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if !reflect.DeepEqual(logged, map[string]string{"tenant": "A"}) {
		t.Fatalf("logged labels = %v, want tenant=A only", logged)
	}
	// Mutating the logged snapshot must not affect the stored labels either.
	logged["tenant"] = "hacked"
	var secondPass map[string]string
	handler = mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondPass = GetRequestLabels(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !reflect.DeepEqual(secondPass, map[string]string{"tenant": "A"}) {
		t.Fatalf("labels after mutating logged snapshot = %v, want tenant=A only", secondPass)
	}
}

func TestRequestLabelsConcurrentSharedParent(t *testing.T) {
	parent := WithRequestLabels(context.Background(), map[string]string{"tenant": "root"})

	var mu sync.Mutex
	seen := map[string]map[string]string{}

	// A single handler chain serving both requests concurrently.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		// Give the sibling request a chance to run and (not) interfere.
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		seen[name] = GetRequestLabels(r.Context())
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	serve := func(name string, cancelBeforeRead bool) {
		// Each request derives its own context from the shared parent and
		// merges its own label over the parent's labels.
		ctx, cancel := context.WithCancel(WithRequestLabels(parent, map[string]string{"job": name}))
		defer cancel()
		req := httptest.NewRequest(http.MethodGet, "/?name="+name, nil).WithContext(ctx)
		if cancelBeforeRead {
			// Cancelling this request's context must not clear labels
			// visible to the sibling request sharing the same parent.
			cancel()
		}
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	var wg sync.WaitGroup
	for i, name := range []string{"one", "two"} {
		wg.Add(1)
		go func(name string, cancelBeforeRead bool) {
			defer wg.Done()
			serve(name, cancelBeforeRead)
		}(name, i == 0)
	}
	wg.Wait()

	for name, labels := range seen {
		want := map[string]string{"tenant": "root", "job": name}
		if !reflect.DeepEqual(labels, want) {
			t.Fatalf("request %q saw labels %v, want %v", name, labels, want)
		}
	}
	if got := GetRequestLabels(parent); !reflect.DeepEqual(got, map[string]string{"tenant": "root"}) {
		t.Fatalf("parent labels after concurrent requests = %v, want tenant=root only", got)
	}
}

// TestRequestLabelsRace exercises concurrent merges and reads over a shared
// parent context. Run with `go test -race`.
func TestRequestLabelsRace(t *testing.T) {
	parent := WithRequestLabels(context.Background(), map[string]string{"tenant": "root"})

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("job-%d", i%4)
			ctx := WithRequestLabels(parent, map[string]string{key: "1"})
			ctx2 := WithRequestLabels(ctx, map[string]string{"tenant": "leaf"})
			if got := GetRequestLabels(ctx2); got["tenant"] != "leaf" || got[key] != "1" {
				t.Errorf("goroutine %d: labels = %v", i, got)
			}
			snap := GetRequestLabels(parent)
			snap["corrupt"] = "attempt" // must be a private copy
			if _, ok := GetRequestLabel(parent, "corrupt"); ok {
				t.Errorf("goroutine %d: parent labels corrupted via snapshot", i)
			}
		}(i)
	}
	wg.Wait()

	if got := GetRequestLabels(parent); !reflect.DeepEqual(got, map[string]string{"tenant": "root"}) {
		t.Fatalf("parent labels = %v, want tenant=root only", got)
	}
}
