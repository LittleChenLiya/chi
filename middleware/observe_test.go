package middleware

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// fakeBaseWriter is a bare http.ResponseWriter with no optional interfaces.
type fakeBaseWriter struct {
	header http.Header
	codes  []int
	body   []byte
	// writePlan controls successive Write results: bytes accepted and error.
	// When nil, every Write accepts the whole buffer and succeeds.
	writePlan []fakeWriteResult
	writes    int
}

type fakeWriteResult struct {
	n   int
	err error
}

func newFakeBaseWriter() *fakeBaseWriter {
	return &fakeBaseWriter{header: http.Header{}}
}

func (w *fakeBaseWriter) Header() http.Header { return w.header }

func (w *fakeBaseWriter) WriteHeader(code int) { w.codes = append(w.codes, code) }

func (w *fakeBaseWriter) Write(p []byte) (int, error) {
	if len(w.writePlan) > 0 {
		plan := w.writePlan[w.writes]
		w.writes++
		n := plan.n
		if n > len(p) {
			n = len(p)
		}
		w.body = append(w.body, p[:n]...)
		return n, plan.err
	}
	w.body = append(w.body, p...)
	return len(p), nil
}

// Optional-interface fakes. They wrap any ResponseWriter, so any subset of
// capabilities can be assembled by stacking them around fakeBaseWriter.

type fakeFlusherLayer struct {
	http.ResponseWriter
	flushed int
}

func (f *fakeFlusherLayer) Flush() { f.flushed++ }

type fakePusherLayer struct {
	http.ResponseWriter
}

func (f *fakePusherLayer) Push(string, *http.PushOptions) error { return nil }

type fakeReaderFromLayer struct {
	http.ResponseWriter
	n   int64
	err error
}

func (f *fakeReaderFromLayer) ReadFrom(r io.Reader) (int64, error) {
	n, err := io.Copy(f.ResponseWriter, r)
	f.n += n
	if err == nil {
		err = f.err
	}
	return n, err
}

type fakeHijackerLayer struct {
	http.ResponseWriter
	hijacked  int
	hijackErr error
}

func (f *fakeHijackerLayer) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if f.hijackErr != nil {
		return nil, nil, f.hijackErr
	}
	f.hijacked++
	client, server := net.Pipe()
	_ = client.Close()
	brw := bufio.NewReadWriter(bufio.NewReader(strings.NewReader("")), bufio.NewWriter(io.Discard))
	return pipeWithCloser{server, func() { _ = server.Close() }}, brw, nil
}

type pipeWithCloser struct {
	net.Conn
	onClose func()
}

func (p pipeWithCloser) Close() error {
	p.onClose()
	return p.Conn.Close()
}

// stackCaps wraps base with layers for the requested capability bits.
func stackCaps(base http.ResponseWriter, caps int) http.ResponseWriter {
	w := base
	if caps&capFlush != 0 {
		w = &fakeFlusherLayer{ResponseWriter: w}
	}
	if caps&capHijack != 0 {
		w = &fakeHijackerLayer{ResponseWriter: w}
	}
	if caps&capPush != 0 {
		w = &fakePusherLayer{ResponseWriter: w}
	}
	if caps&capReaderFrom != 0 {
		w = &fakeReaderFromLayer{ResponseWriter: w}
	}
	return w
}

// runObserve executes h behind a fresh Observe middleware and returns the
// single recorded observation.
func runObserve(t *testing.T, base http.ResponseWriter, h http.HandlerFunc) Observation {
	t.Helper()
	var got []Observation
	mw := Observe(func(_ *http.Request, obs Observation) {
		got = append(got, obs)
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	mw(h).ServeHTTP(base, r)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 observation, got %d", len(got))
	}
	return got[0]
}

func TestObserveOrdinaryResponse(t *testing.T) {
	base := newFakeBaseWriter()
	obs := runObserve(t, base, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		n, err := w.Write([]byte("created!"))
		if err != nil || n != len("created!") {
			t.Fatalf("unexpected write result n=%d err=%v", n, err)
		}
	})
	if obs.Status != http.StatusCreated {
		t.Errorf("Status = %d, want 201", obs.Status)
	}
	if obs.Bytes != int64(len("created!")) {
		t.Errorf("Bytes = %d, want %d", obs.Bytes, len("created!"))
	}
	if obs.Reason != ReasonComplete || obs.Err != nil || obs.Panic != nil {
		t.Errorf("unexpected observation: %+v", obs)
	}
	if len(base.codes) != 1 || base.codes[0] != http.StatusCreated {
		t.Errorf("underlying codes = %v, want [201]", base.codes)
	}
}

func TestObserveImplicit200WhenOnlyBodyWritten(t *testing.T) {
	base := newFakeBaseWriter()
	obs := runObserve(t, base, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})
	if obs.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", obs.Status)
	}
	if obs.Bytes != 5 {
		t.Errorf("Bytes = %d, want 5", obs.Bytes)
	}
	if len(base.codes) != 1 || base.codes[0] != http.StatusOK {
		t.Errorf("underlying codes = %v, want [200]", base.codes)
	}
}

func TestObserveImplicit200WhenNothingWritten(t *testing.T) {
	base := newFakeBaseWriter()
	obs := runObserve(t, base, func(http.ResponseWriter, *http.Request) {})
	if obs.Status != http.StatusOK || obs.Bytes != 0 {
		t.Errorf("obs = %+v, want status 200 and 0 bytes", obs)
	}
	if len(base.codes) != 0 {
		t.Errorf("underlying codes = %v, want none; middleware must not write", base.codes)
	}
}

func TestObserveStreamingFlush(t *testing.T) {
	stacked := &fakeFlusherLayer{ResponseWriter: newFakeBaseWriter()}
	obs := runObserve(t, stacked, func(w http.ResponseWriter, _ *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("wrapper lost http.Flusher")
		}
		_, _ = w.Write([]byte("chunk-1"))
		fl.Flush()
		_, _ = w.Write([]byte("chunk-2"))
		fl.Flush()
	})
	base := stacked.ResponseWriter.(*fakeBaseWriter)
	if obs.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", obs.Status)
	}
	if obs.Bytes != int64(len("chunk-1chunk-2")) {
		t.Errorf("Bytes = %d, want %d", obs.Bytes, len("chunk-1chunk-2"))
	}
	if stacked.flushed != 2 {
		t.Errorf("flushed = %d, want 2", stacked.flushed)
	}
	if len(base.codes) != 1 || base.codes[0] != http.StatusOK {
		t.Errorf("underlying codes = %v, want a single implicit 200", base.codes)
	}
}

var errShortWrite = errors.New("short write")

func TestObserveShortWriteAndError(t *testing.T) {
	base := newFakeBaseWriter()
	base.writePlan = []fakeWriteResult{
		{n: 3, err: nil},
		{n: 2, err: errShortWrite},
	}
	obs := runObserve(t, base, func(w http.ResponseWriter, _ *http.Request) {
		n1, err1 := w.Write([]byte("abcdef"))
		if n1 != 3 || err1 != nil {
			t.Fatalf("first write = (%d, %v), want (3, nil)", n1, err1)
		}
		n2, err2 := w.Write([]byte("ghij"))
		if n2 != 2 || !errors.Is(err2, errShortWrite) {
			t.Fatalf("second write = (%d, %v), want (2, errShortWrite)", n2, err2)
		}
	})
	if obs.Bytes != 5 {
		t.Errorf("Bytes = %d, want 5 (accepted bytes only)", obs.Bytes)
	}
	if !errors.Is(obs.Err, errShortWrite) {
		t.Errorf("Err = %v, want errShortWrite", obs.Err)
	}
	if string(base.body) != "abcgh" {
		t.Errorf("underlying body = %q, want %q", base.body, "abcgh")
	}
}

func TestObserve103Then200(t *testing.T) {
	base := newFakeBaseWriter()
	obs := runObserve(t, base, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if obs.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200; 103 must not be final", obs.Status)
	}
	if obs.Bytes != 2 {
		t.Errorf("Bytes = %d, want 2", obs.Bytes)
	}
	if len(base.codes) != 2 || base.codes[0] != 103 || base.codes[1] != 200 {
		t.Errorf("underlying codes = %v, want [103 200]", base.codes)
	}
}

func TestObserveRepeatedWriteHeaderFirstWins(t *testing.T) {
	base := newFakeBaseWriter()
	obs := runObserve(t, base, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("x"))
	})
	if obs.Status != http.StatusTeapot {
		t.Errorf("Status = %d, want first final status 418", obs.Status)
	}
	if len(base.codes) != 2 || base.codes[0] != 418 || base.codes[1] != 404 {
		t.Errorf("underlying codes = %v, want both forwarded [418 404]", base.codes)
	}
}

func TestObserveReaderFrom(t *testing.T) {
	rfLayer := &fakeReaderFromLayer{ResponseWriter: newFakeBaseWriter()}
	obs := runObserve(t, rfLayer, func(w http.ResponseWriter, _ *http.Request) {
		rf, ok := w.(io.ReaderFrom)
		if !ok {
			t.Fatal("wrapper lost io.ReaderFrom")
		}
		n, err := rf.ReadFrom(strings.NewReader("streamed-body"))
		if err != nil || n != int64(len("streamed-body")) {
			t.Fatalf("ReadFrom = (%d, %v)", n, err)
		}
	})
	base := rfLayer.ResponseWriter.(*fakeBaseWriter)
	if obs.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", obs.Status)
	}
	if obs.Bytes != int64(len("streamed-body")) {
		t.Errorf("Bytes = %d, want %d", obs.Bytes, len("streamed-body"))
	}
	if string(base.body) != "streamed-body" {
		t.Errorf("underlying body = %q", base.body)
	}
	if rfLayer.n != int64(len("streamed-body")) {
		t.Errorf("underlying ReadFrom consumed %d, want %d", rfLayer.n, len("streamed-body"))
	}
}

func TestObserveReaderFromShortCount(t *testing.T) {
	// A ReaderFrom that reports fewer consumed bytes than the source holds.
	trunc := &fakeTruncReaderFrom{ResponseWriter: newFakeBaseWriter()}
	obs := runObserve(t, trunc, func(w http.ResponseWriter, _ *http.Request) {
		rf := w.(io.ReaderFrom)
		n, err := rf.ReadFrom(strings.NewReader("abcdef"))
		if n != 3 || err != nil {
			t.Fatalf("ReadFrom = (%d, %v), want (3, nil)", n, err)
		}
	})
	if obs.Bytes != 3 {
		t.Errorf("Bytes = %d, want 3 reported by ReadFrom", obs.Bytes)
	}
}

type fakeTruncReaderFrom struct {
	http.ResponseWriter
}

func (f *fakeTruncReaderFrom) ReadFrom(r io.Reader) (int64, error) {
	p := make([]byte, 3)
	n, err := io.ReadFull(r, p)
	if err == io.ErrUnexpectedEOF {
		err = nil
	}
	_, werr := f.ResponseWriter.Write(p[:n])
	return int64(n), werr
}

func TestObserveHijack(t *testing.T) {
	hjLayer := &fakeHijackerLayer{ResponseWriter: newFakeBaseWriter()}
	obs := runObserve(t, hjLayer, func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("wrapper lost http.Hijacker")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("Hijack: %v", err)
		}
		defer conn.Close()
		// Writing through the ResponseWriter after takeover is the business's
		// own raw-I/O choice; it must not be counted as ordinary HTTP bytes.
		_, _ = w.Write([]byte("after hijack"))
	})
	base := hjLayer.ResponseWriter.(*fakeBaseWriter)
	if obs.Reason != ReasonHijacked {
		t.Errorf("Reason = %q, want hijacked", obs.Reason)
	}
	if obs.Status != 0 {
		t.Errorf("Status = %d, want 0 after hijack", obs.Status)
	}
	if obs.Bytes != 0 {
		t.Errorf("Bytes = %d, want 0 after successful hijack", obs.Bytes)
	}
	if hjLayer.hijacked != 1 {
		t.Errorf("underlying hijack count = %d, want 1", hjLayer.hijacked)
	}
	if string(base.body) != "after hijack" {
		t.Errorf("underlying body = %q; post-hijack writes still pass through", base.body)
	}
}

func TestObserveFailedHijack(t *testing.T) {
	hijackErr := errors.New("hijack unsupported")
	hjLayer := &fakeHijackerLayer{ResponseWriter: newFakeBaseWriter(), hijackErr: hijackErr}
	obs := runObserve(t, hjLayer, func(w http.ResponseWriter, _ *http.Request) {
		_, _, err := w.(http.Hijacker).Hijack()
		if !errors.Is(err, hijackErr) {
			t.Fatalf("Hijack err = %v, want %v", err, hijackErr)
		}
		// Failed hijack: normal HTTP accounting stays in effect.
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("nope"))
	})
	if obs.Reason != ReasonComplete {
		t.Errorf("Reason = %q, want complete after failed hijack", obs.Reason)
	}
	if obs.Status != http.StatusAccepted || obs.Bytes != 4 {
		t.Errorf("obs = %+v, want 202/4 bytes", obs)
	}
}

func TestObservePanicReportedAndReraised(t *testing.T) {
	base := newFakeBaseWriter()
	var got []Observation
	mw := Observe(func(_ *http.Request, obs Observation) {
		got = append(got, obs)
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	boom := errors.New("boom")
	func() {
		defer func() {
			if rv := recover(); !errors.Is(rv.(error), boom) {
				t.Fatalf("recovered = %v, want boom (panic must propagate)", rv)
			}
		}()
		mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte("partial"))
			panic(boom)
		})).ServeHTTP(base, r)
	}()
	if len(got) != 1 {
		t.Fatalf("observations = %d, want exactly 1", len(got))
	}
	obs := got[0]
	if obs.Reason != ReasonPanic {
		t.Errorf("Reason = %q, want panic", obs.Reason)
	}
	if obs.Status != http.StatusTeapot || obs.Bytes != int64(len("partial")) {
		t.Errorf("obs = %+v, want 418/7 bytes from pre-panic writes", obs)
	}
	if !errors.Is(obs.Panic.(error), boom) || len(obs.Stack) == 0 {
		t.Errorf("panic/stack not recorded: %+v", obs)
	}
	// The middleware itself must not rewrite the response on panic.
	if len(base.codes) != 1 || base.codes[0] != http.StatusTeapot {
		t.Errorf("underlying codes = %v, want only the handler's 418", base.codes)
	}
}

func TestObservePanicBeforeAnyWriteRecordsNoStatus(t *testing.T) {
	base := newFakeBaseWriter()
	var got Observation
	mw := Observe(func(_ *http.Request, obs Observation) { got = obs })
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic to propagate")
			}
		}()
		mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("early")
		})).ServeHTTP(base, r)
	}()
	if got.Reason != ReasonPanic || got.Status != 0 || got.Bytes != 0 {
		t.Errorf("obs = %+v, want panic with status 0 and 0 bytes", got)
	}
	if len(base.codes) != 0 {
		t.Errorf("underlying codes = %v, middleware must not fabricate a status", base.codes)
	}
}

func TestObserveClientCanceled(t *testing.T) {
	base := newFakeBaseWriter()
	ctx, cancel := context.WithCancel(httptest.NewRequest(http.MethodGet, "/", nil).Context())
	var got Observation
	mw := Observe(func(_ *http.Request, obs Observation) { got = obs })
	r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("partial"))
		cancel()
		<-r.Context().Done()
	})).ServeHTTP(base, r)
	if got.Reason != ReasonClientCanceled {
		t.Errorf("Reason = %q, want client_canceled", got.Reason)
	}
	if got.Status != http.StatusOK || got.Bytes != 7 {
		t.Errorf("obs = %+v, want 200/7 bytes sent before cancel", got)
	}
}

func TestObserveClientCanceledBeforeAnyWrite(t *testing.T) {
	base := newFakeBaseWriter()
	ctx, cancel := context.WithCancel(httptest.NewRequest(http.MethodGet, "/", nil).Context())
	var got Observation
	mw := Observe(func(_ *http.Request, obs Observation) { got = obs })
	r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	})).ServeHTTP(base, r)
	if got.Reason != ReasonClientCanceled || got.Status != 0 || got.Bytes != 0 {
		t.Errorf("obs = %+v, want canceled with no final status/bytes", got)
	}
}

// TestObserveCapabilityMatrix verifies, for all 16 subsets of the four
// optional interfaces, that the wrapper exposes exactly the same set.
func TestObserveCapabilityMatrix(t *testing.T) {
	hasFlush := func(w http.ResponseWriter) bool { _, ok := w.(http.Flusher); return ok }
	hasHijack := func(w http.ResponseWriter) bool { _, ok := w.(http.Hijacker); return ok }
	hasPush := func(w http.ResponseWriter) bool { _, ok := w.(http.Pusher); return ok }
	hasReaderFrom := func(w http.ResponseWriter) bool { _, ok := w.(io.ReaderFrom); return ok }

	for caps := 0; caps < 1<<4; caps++ {
		base := stackCaps(newFakeBaseWriter(), caps)
		w := newObservedResponseWriter(base)
		wantF, wantH, wantP, wantR := hasFlush(base), hasHijack(base), hasPush(base), hasReaderFrom(base)
		if hasFlush(w) != wantF || hasHijack(w) != wantH || hasPush(w) != wantP || hasReaderFrom(w) != wantR {
			t.Errorf("caps=%04b: wrapper capabilities = flush=%v hijack=%v push=%v readerfrom=%v, want %v %v %v %v",
				caps, hasFlush(w), hasHijack(w), hasPush(w), hasReaderFrom(w), wantF, wantH, wantP, wantR)
		}
		if u, ok := w.(interface{ Unwrap() http.ResponseWriter }); !ok || u.Unwrap() != base {
			t.Errorf("caps=%04b: Unwrap did not return the underlying writer", caps)
		}
	}
}

func TestObservePlainWriterExposesNoOptionalInterface(t *testing.T) {
	base := newFakeBaseWriter()
	w := newObservedResponseWriter(base)
	if _, ok := w.(http.Flusher); ok {
		t.Error("bare writer wrapper unexpectedly implements http.Flusher")
	}
	if _, ok := w.(http.Hijacker); ok {
		t.Error("bare writer wrapper unexpectedly implements http.Hijacker")
	}
	if _, ok := w.(http.Pusher); ok {
		t.Error("bare writer wrapper unexpectedly implements http.Pusher")
	}
	if _, ok := w.(io.ReaderFrom); ok {
		t.Error("bare writer wrapper unexpectedly implements io.ReaderFrom")
	}
}

func TestObserveResponseControllerFlush(t *testing.T) {
	stacked := &fakeFlusherLayer{ResponseWriter: newFakeBaseWriter()}
	obs := runObserve(t, stacked, func(w http.ResponseWriter, _ *http.Request) {
		c := http.NewResponseController(w)
		if err := c.Flush(); err != nil {
			t.Fatalf("Flush via ResponseController: %v", err)
		}
	})
	if stacked.flushed != 1 {
		t.Errorf("flushed = %d, want 1 via Unwrap chain", stacked.flushed)
	}
	if obs.Status != http.StatusOK {
		t.Errorf("Status = %d, want implicit 200 on flush", obs.Status)
	}
}

// TestObserveOverRealServer exercises the concrete writers handed out by
// net/http (which implement Flusher, Hijacker and ReaderFrom over HTTP/1.1).
func TestObserveOverRealServer(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		handler    http.HandlerFunc
		wantStatus int
		wantBody   string
	}{
		{
			name: "ordinary",
			path: "/",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte("created"))
			},
			wantStatus: http.StatusCreated,
			wantBody:   "created",
		},
		{
			name: "streaming-flush",
			path: "/stream",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				fl := w.(http.Flusher)
				_, _ = w.Write([]byte("a"))
				fl.Flush()
				_, _ = w.Write([]byte("bc"))
				fl.Flush()
			},
			wantStatus: http.StatusOK,
			wantBody:   "abc",
		},
		{
			name: "103-then-200",
			path: "/hints",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Link", "</x>; rel=preload")
				w.WriteHeader(http.StatusEarlyHints)
				_, _ = w.Write([]byte("final"))
			},
			wantStatus: http.StatusOK,
			wantBody:   "final",
		},
		{
			name: "io-copy-readerfrom",
			path: "/copy",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.Copy(w, strings.NewReader("copied-via-readerfrom"))
			},
			wantStatus: http.StatusOK,
			wantBody:   "copied-via-readerfrom",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCh := make(chan Observation, 1)
			r := chi.NewRouter()
			r.Use(Observe(func(_ *http.Request, obs Observation) { gotCh <- obs }))
			r.Get(tc.path, tc.handler)

			srv := httptest.NewServer(r)
			defer srv.Close()

			req, _ := http.NewRequest(http.MethodGet, srv.URL+tc.path, nil)
			resp, err := http.DefaultTransport.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			defer resp.Body.Close()
			respBody, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("on-wire status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			got := <-gotCh
			if string(respBody) != tc.wantBody {
				t.Errorf("on-wire body = %q, want %q", respBody, tc.wantBody)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("observed status = %d, want %d", got.Status, tc.wantStatus)
			}
			if got.Bytes != int64(len(tc.wantBody)) {
				t.Errorf("observed bytes = %d, want %d", got.Bytes, len(tc.wantBody))
			}
			if got.Reason != ReasonComplete || got.Err != nil {
				t.Errorf("observation = %+v, want complete/no error", got)
			}
		})
	}
}

func TestObserveRealServerHijack(t *testing.T) {
	gotCh := make(chan Observation, 1)
	r := chi.NewRouter()
	r.Use(Observe(func(_ *http.Request, obs Observation) { gotCh <- obs }))
	r.Get("/hijack", func(w http.ResponseWriter, _ *http.Request) {
		hj := w.(http.Hijacker)
		conn, brw, err := hj.Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 200 OK" + "\r\n" +
			"Content-Length: 3" + "\r\n\r\n" + "raw")
		_ = brw.Flush()
	})

	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/hijack")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK || string(respBody) != "raw" {
		t.Fatalf("on-wire = %d %q, want 200/raw", resp.StatusCode, respBody)
	}
	got := <-gotCh
	if got.Reason != ReasonHijacked || got.Status != 0 || got.Bytes != 0 {
		t.Errorf("observation = %+v, want hijacked/0/0", got)
	}
}
