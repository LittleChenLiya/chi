package middleware

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestWithRequestLabelsMergesAndOverrides verifies the core layering
// behavior: the inner layer overrides tenant=B and adds job=1, while the
// outer layer's tenant=A is shadowed.
func TestWithRequestLabelsMergesAndOverrides(t *testing.T) {
	ctx := context.Background()
	ctx = WithRequestLabels(ctx, map[string][]byte{"tenant": []byte("A")})
	ctx = WithRequestLabels(ctx, map[string][]byte{"tenant": []byte("B"), "job": []byte("1")})

	labels := GetRequestLabels(ctx)
	if got := string(labels["tenant"]); got != "B" {
		t.Fatalf("tenant = %q, want B", got)
	}
	if got := string(labels["job"]); got != "1" {
		t.Fatalf("job = %q, want 1", got)
	}
}

// TestWithRequestLabelsDoesNotMutateParent verifies that appending labels
// to a child context leaves the parent context's labels intact.
func TestWithRequestLabelsDoesNotMutateParent(t *testing.T) {
	parent := WithRequestLabels(context.Background(), map[string][]byte{"tenant": []byte("A")})
	_ = WithRequestLabels(parent, map[string][]byte{"tenant": []byte("B"), "job": []byte("1")})

	parentLabels := GetRequestLabels(parent)
	if got := string(parentLabels["tenant"]); got != "A" {
		t.Fatalf("parent tenant = %q, want A (parent must not be modified)", got)
	}
	if _, ok := parentLabels["job"]; ok {
		t.Fatal("parent gained job label; parent must not be modified")
	}
}

// TestGetRequestLabelsReturnsSnapshot verifies that mutating the map
// returned by GetRequestLabels does not affect the stored labels or
// subsequent reads.
func TestGetRequestLabelsReturnsSnapshot(t *testing.T) {
	ctx := WithRequestLabels(context.Background(), map[string][]byte{"tenant": []byte("A")})

	labels := GetRequestLabels(ctx)
	labels["tenant"] = []byte("HACKED")
	labels["injected"] = []byte("yes")
	labels["tenant"][0] = 'Z'

	labels2 := GetRequestLabels(ctx)
	if got := string(labels2["tenant"]); got != "A" {
		t.Fatalf("tenant after mutation = %q, want A", got)
	}
	if _, ok := labels2["injected"]; ok {
		t.Fatal("injected label leaked into stored labels")
	}
}

// TestWithRequestLabelsNilInput verifies that a nil labels map and a nil
// context are treated as empty, returning an empty non-nil map.
func TestWithRequestLabelsNilInput(t *testing.T) {
	ctx := WithRequestLabels(context.Background(), nil)
	labels := GetRequestLabels(ctx)
	if labels == nil {
		t.Fatal("GetRequestLabels returned nil map; want empty non-nil map")
	}
	if len(labels) != 0 {
		t.Fatalf("len(labels) = %d, want 0", len(labels))
	}

	if labels = GetRequestLabels(nil); labels == nil {
		t.Fatal("GetRequestLabels(nil) returned nil map; want empty non-nil map")
	}
}

// TestWithRequestLabelsCopiesBytes verifies that the original bytes of
// values are preserved: later mutations by the caller do not affect stored
// labels.
func TestWithRequestLabelsCopiesBytes(t *testing.T) {
	v := []byte("A")
	ctx := WithRequestLabels(context.Background(), map[string][]byte{"tenant": v})
	v[0] = 'Z'
	if got := string(GetRequestLabels(ctx)["tenant"]); got != "A" {
		t.Fatalf("stored label changed after caller mutation: got %q, want A", got)
	}
}

// TestRequestLabelsWithLoggerGetsSnapshot verifies that the log callback
// receives a snapshot: the handler mutating its own read map does not
// affect what the logger sees.
func TestRequestLabelsWithLoggerGetsSnapshot(t *testing.T) {
	var logged map[string][]byte

	r := chi.NewRouter()
	// Logger is placed after the appending middleware so it sees tenant=A.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := WithRequestLabels(req.Context(), map[string][]byte{"tenant": []byte("A")})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Use(RequestLabelsWithLogger(func(labels map[string][]byte) {
		logged = labels
	}))
	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		labels := GetRequestLabels(req.Context())
		labels["tenant"] = []byte("HACKED")
	})

	ts := httptest.NewServer(r)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if logged == nil {
		t.Fatal("logger was not called")
	}
	if got := string(logged["tenant"]); got != "A" {
		t.Fatalf("logger saw tenant = %q, want A (snapshot must not be affected by handler mutation)", got)
	}
}

// TestRequestLabelsCancellationDoesNotClearSiblingLabels verifies that
// canceling one request's context does not clear the labels of another
// request sharing the same parent context.
func TestRequestLabelsCancellationDoesNotClearSiblingLabels(t *testing.T) {
	parent := context.Background()
	ctx1, cancel1 := context.WithCancel(parent)
	ctx2, cancel2 := context.WithCancel(parent)
	_ = cancel2

	ctx1 = WithRequestLabels(ctx1, map[string][]byte{"a": []byte("1")})
	ctx2 = WithRequestLabels(ctx2, map[string][]byte{"b": []byte("2")})

	cancel1()

	if got := string(GetRequestLabels(ctx2)["b"]); got != "2" {
		t.Fatalf("sibling labels lost after cancel: got %q, want 2", got)
	}
	if got := string(GetRequestLabels(ctx1)["a"]); got != "1" {
		t.Fatalf("own labels lost after cancel: got %q, want 1", got)
	}
}

// TestRequestLabelsRace hammers concurrent appends and reads from a shared
// parent context to ensure labels never mix across goroutines and snapshot
// mutations never leak.
func TestRequestLabelsRace(t *testing.T) {
	parent := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := WithRequestLabels(parent, map[string][]byte{
				"id":    []byte(strconv.Itoa(i)),
				"layer": []byte("1"),
			})
			ctx = WithRequestLabels(ctx, map[string][]byte{
				"layer": []byte("2"),
				"job":   []byte("j"),
			})
			got := GetRequestLabels(ctx)
			if string(got["id"]) != strconv.Itoa(i) {
				t.Errorf("labels mixed: id = %q, want %d", got["id"], i)
			}
			if string(got["layer"]) != "2" {
				t.Errorf("layer = %q, want 2", got["layer"])
			}
			// Mutate the snapshot; subsequent reads must be unchanged.
			got["id"][0] = 'z'
			got["new"] = []byte("x")
			again := GetRequestLabels(ctx)
			if string(again["id"]) != strconv.Itoa(i) {
				t.Errorf("snapshot mutation leaked: id = %q", again["id"])
			}
		}(i)
	}
	wg.Wait()
}

// TestRequestLabelsHTTPRace runs concurrent httptest requests through a
// shared router and verifies that each response carries only its own
// tenant label.
func TestRequestLabelsHTTPRace(t *testing.T) {
	r := chi.NewRouter()
	r.Use(RequestLabels)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := WithRequestLabels(req.Context(), map[string][]byte{
				"tenant": []byte(req.Header.Get("X-Tenant")),
			})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		labels := GetRequestLabels(req.Context())
		w.Write(labels["tenant"])
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tenant := strconv.Itoa(i)
			req, err := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("X-Tenant", tenant)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Error(err)
				return
			}
			if string(body) != tenant {
				t.Errorf("labels mixed: got %q, want %q", body, tenant)
			}
		}(i)
	}
	wg.Wait()
}

func ExampleRequestLabels() {
	r := chi.NewRouter()

	// Layer 1 (outer): append tenant=A.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := WithRequestLabels(req.Context(), map[string][]byte{
				"tenant": []byte("A"),
			})
			req = req.WithContext(ctx)
			fmt.Println("outer:", displayLabels(GetRequestLabels(req.Context())))
			next.ServeHTTP(w, req)
		})
	})

	// Layer 2 (inner): override tenant=B and add job=1.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := WithRequestLabels(req.Context(), map[string][]byte{
				"tenant": []byte("B"),
				"job":    []byte("1"),
			})
			req = req.WithContext(ctx)
			fmt.Println("inner:", displayLabels(GetRequestLabels(req.Context())))
			next.ServeHTTP(w, req)
		})
	})

	// Layer 3 (handler): read the merged snapshot and mutate it.
	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		labels := GetRequestLabels(req.Context())
		fmt.Println("handler:", displayLabels(labels))

		// Mutating the snapshot must not affect subsequent reads.
		labels["tenant"] = []byte("HACKED")
		labels["injected"] = []byte("yes")
		fmt.Println("handler after mutation:", displayLabels(GetRequestLabels(req.Context())))
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		panic(err)
	}
	resp.Body.Close()

	// Output:
	// outer: map[tenant:A]
	// inner: map[job:1 tenant:B]
	// handler: map[job:1 tenant:B]
	// handler after mutation: map[job:1 tenant:B]
}

// displayLabels converts a labels snapshot to a readable map for printing.
func displayLabels(labels map[string][]byte) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = string(v)
	}
	return out
}
