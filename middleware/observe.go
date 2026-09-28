package middleware

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"runtime/debug"
	"time"
)

// EndReason describes why the observed response processing ended. It is
// reported in the Observation passed to an ObserverFunc.
type EndReason string

const (
	// ReasonComplete means the handler returned normally, without hijacking
	// the connection.
	ReasonComplete EndReason = "complete"
	// ReasonPanic means the handler panicked. The middleware records the
	// observation and then re-panics, so it does not alter the response.
	ReasonPanic EndReason = "panic"
	// ReasonClientCanceled means the request's context was canceled while the
	// handler was running (typically because the client went away).
	ReasonClientCanceled EndReason = "client_canceled"
	// ReasonHijacked means the handler successfully took over the connection
	// via http.Hijacker. Ordinary HTTP byte accounting stops at that point.
	ReasonHijacked EndReason = "hijacked"
)

// Observation is the record emitted exactly once by the Observe middleware
// for each request, after the downstream handler finishes: a normal return,
// a panic, a client cancellation, or a return following a successful
// http.Hijacker.Hijack all produce exactly one record.
//
// Status is the final response status code. Informational (1xx) responses
// such as 103 Early Hints are passed through but never reported as final.
// When a body is written without an explicit status, Status is 200 matching
// net/http; a handler that returns normally without writing also reports 200.
// Status is 0 after a successful hijack, and when a panic or client
// cancellation occurs before any final status was produced.
//
// Bytes counts only the bytes accepted by the underlying ResponseWriter's
// Write or ReadFrom methods (short writes included) and never includes bytes
// written directly to a hijacked connection.
type Observation struct {
	// Status is the final (non-1xx) HTTP status code, or 0 if none was sent.
	Status int
	// Bytes is the number of response-body bytes actually accepted by the
	// underlying writer. Short writes are reflected exactly.
	Bytes int64
	// Reason is why the response processing ended.
	Reason EndReason
	// Err is the first error reported by an underlying Write/ReadFrom, if any.
	Err error
	// Panic holds the recovered panic value when Reason is ReasonPanic.
	Panic any
	// Stack is the stack trace captured when Reason is ReasonPanic.
	Stack []byte
	// Elapsed is the wall-clock time spent in the downstream handler chain.
	// It is provided for convenience and is generally too unstable to assert
	// on in tests.
	Elapsed time.Duration
}

// ObserverFunc receives exactly one Observation per request. It must not
// panic.
type ObserverFunc func(r *http.Request, obs Observation)

// Observe returns a middleware that observes the response produced by the
// next handler and reports it exactly once through observe. Normal handler
// returns, panics and client cancellations each produce a single record. A
// panic is observed first and then re-raised unchanged; the middleware never
// writes or rewrites the response itself.
//
// The ResponseWriter handed downstream exposes exactly the same optional
// interfaces as the underlying writer: http.Flusher, http.Hijacker,
// http.Pusher and io.ReaderFrom. No capability is ever invented. A
// successful Hijack marks the connection as taken over (ReasonHijacked) and
// stops ordinary HTTP byte accounting.
func Observe(observe ObserverFunc) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		fn := func(w http.ResponseWriter, r *http.Request) {
			ow := newObservedResponseWriter(w)
			start := time.Now()

			defer func() {
				if rv := recover(); rv != nil {
					obs := ow.finish()
					obs.Reason = ReasonPanic
					obs.Panic = rv
					obs.Stack = debug.Stack()
					obs.Elapsed = time.Since(start)
					observe(r, obs)
					panic(rv)
				}
			}()

			next.ServeHTTP(ow, r)

			obs := ow.finish()
			switch {
			case obs.Reason == ReasonHijacked:
				// Reason is set by finish; nothing more to determine.
			case r.Context().Err() != nil:
				obs.Reason = ReasonClientCanceled
			default:
				obs.Reason = ReasonComplete
				if !ow.finalized() {
					// net/http itself writes 200 for a handler that simply returns
					// without sending anything.
					obs.Status = http.StatusOK
				}
			}
			obs.Elapsed = time.Since(start)
			observe(r, obs)
		}
		return http.HandlerFunc(fn)
	}
}

// observedResponseWriter is implemented by every concrete writer produced by
// newObservedResponseWriter.
type observedResponseWriter interface {
	http.ResponseWriter
	finish() Observation
	finalized() bool
}

// Optional-interface capability bits detected on the underlying writer.
const (
	capFlush      = 1 << iota // http.Flusher
	capHijack                 // http.Hijacker
	capPush                   // http.Pusher
	capReaderFrom             // io.ReaderFrom
)

// newObservedResponseWriter wraps w while preserving exactly the optional
// interfaces (Flusher, Hijacker, Pusher, ReaderFrom) that w implements.
func newObservedResponseWriter(w http.ResponseWriter) observedResponseWriter {
	var caps int
	if _, ok := w.(http.Flusher); ok {
		caps |= capFlush
	}
	if _, ok := w.(http.Hijacker); ok {
		caps |= capHijack
	}
	if _, ok := w.(http.Pusher); ok {
		caps |= capPush
	}
	if _, ok := w.(io.ReaderFrom); ok {
		caps |= capReaderFrom
	}

	core := &obsWriterCore{ResponseWriter: w}

	switch caps {
	case 0:
		return core
	case capFlush:
		return &obsFlushOnly{obsWriterCore: core, flushMixin: flushMixin{core}}
	case capHijack:
		return &obsHijackOnly{obsWriterCore: core, hijackMixin: hijackMixin{core}}
	case capPush:
		return &obsPushOnly{obsWriterCore: core, pushMixin: pushMixin{core}}
	case capReaderFrom:
		return &obsReaderFromOnly{obsWriterCore: core, readerFromMixin: readerFromMixin{core}}
	case capFlush | capHijack:
		return &obsFlushHijack{obsWriterCore: core, flushMixin: flushMixin{core}, hijackMixin: hijackMixin{core}}
	case capFlush | capPush:
		return &obsFlushPush{obsWriterCore: core, flushMixin: flushMixin{core}, pushMixin: pushMixin{core}}
	case capFlush | capReaderFrom:
		return &obsFlushReaderFrom{obsWriterCore: core, flushMixin: flushMixin{core}, readerFromMixin: readerFromMixin{core}}
	case capHijack | capPush:
		return &obsHijackPush{obsWriterCore: core, hijackMixin: hijackMixin{core}, pushMixin: pushMixin{core}}
	case capHijack | capReaderFrom:
		return &obsHijackReaderFrom{obsWriterCore: core, hijackMixin: hijackMixin{core}, readerFromMixin: readerFromMixin{core}}
	case capPush | capReaderFrom:
		return &obsPushReaderFrom{obsWriterCore: core, pushMixin: pushMixin{core}, readerFromMixin: readerFromMixin{core}}
	case capFlush | capHijack | capPush:
		return &obsFlushHijackPush{obsWriterCore: core, flushMixin: flushMixin{core}, hijackMixin: hijackMixin{core}, pushMixin: pushMixin{core}}
	case capFlush | capHijack | capReaderFrom:
		return &obsFlushHijackReaderFrom{obsWriterCore: core, flushMixin: flushMixin{core}, hijackMixin: hijackMixin{core}, readerFromMixin: readerFromMixin{core}}
	case capFlush | capPush | capReaderFrom:
		return &obsFlushPushReaderFrom{obsWriterCore: core, flushMixin: flushMixin{core}, pushMixin: pushMixin{core}, readerFromMixin: readerFromMixin{core}}
	case capHijack | capPush | capReaderFrom:
		return &obsHijackPushReaderFrom{obsWriterCore: core, hijackMixin: hijackMixin{core}, pushMixin: pushMixin{core}, readerFromMixin: readerFromMixin{core}}
	case capFlush | capHijack | capPush | capReaderFrom:
		return &obsAllCaps{obsWriterCore: core, flushMixin: flushMixin{core}, hijackMixin: hijackMixin{core}, pushMixin: pushMixin{core}, readerFromMixin: readerFromMixin{core}}
	default:
		panic("observe: unreachable capability combination")
	}
}

// obsWriterCore implements the mandatory http.ResponseWriter methods and
// holds the shared observation state. Optional methods live on the concrete
// wrapper types, composed from the mixin types below.
type obsWriterCore struct {
	http.ResponseWriter
	status     int
	bytes      int64
	wroteFinal bool
	hijacked   bool
	err        error
}

// WriteHeader passes informational (1xx) responses straight through without
// recording them. The first final status wins: later WriteHeader calls are
// still forwarded to the underlying writer (sharing its behavior) but never
// change the recorded status.
func (c *obsWriterCore) WriteHeader(code int) {
	if c.hijacked {
		c.ResponseWriter.WriteHeader(code)
		return
	}
	if code >= 100 && code <= 199 {
		c.ResponseWriter.WriteHeader(code)
		return
	}
	if !c.wroteFinal {
		c.status = code
		c.wroteFinal = true
	}
	c.ResponseWriter.WriteHeader(code)
}

// Write implicitly sends 200 with the first body byte, exactly as net/http
// does. Only the bytes the underlying writer actually accepts are counted,
// and short writes or errors are recorded faithfully.
func (c *obsWriterCore) Write(buf []byte) (int, error) {
	if c.hijacked {
		return c.ResponseWriter.Write(buf)
	}
	c.ensureFinalStatus()
	n, err := c.ResponseWriter.Write(buf)
	c.bytes += int64(n)
	if err != nil && c.err == nil {
		c.err = err
	}
	return n, err
}

// ensureFinalStatus records an implicit 200 before the first body write or
// flush when no final status has been set, matching net/http semantics.
func (c *obsWriterCore) ensureFinalStatus() {
	if !c.wroteFinal && !c.hijacked {
		c.WriteHeader(http.StatusOK)
	}
}

// finish snapshots the observation. It is called once per request.
func (c *obsWriterCore) finish() Observation {
	if c.hijacked {
		return Observation{Reason: ReasonHijacked, Bytes: c.bytes, Err: c.err}
	}
	// Status stays 0 when no final status was produced; the caller applies
	// net/http's implicit-200 rule only for genuinely complete responses.
	return Observation{Status: c.status, Bytes: c.bytes, Err: c.err}
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (c *obsWriterCore) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// finalized reports whether a final status was produced.
func (c *obsWriterCore) finalized() bool { return c.wroteFinal }

// flushMixin provides http.Flusher.
type flushMixin struct{ core *obsWriterCore }

func (m flushMixin) Flush() {
	if m.core.hijacked {
		m.core.ResponseWriter.(http.Flusher).Flush()
		return
	}
	m.core.ensureFinalStatus()
	m.core.ResponseWriter.(http.Flusher).Flush()
}

// hijackMixin provides http.Hijacker. A successful hijack marks the
// connection as taken over and stops ordinary HTTP byte accounting; a failed
// hijack changes nothing.
type hijackMixin struct{ core *obsWriterCore }

func (m hijackMixin) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := m.core.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil {
		m.core.hijacked = true
	}
	return conn, brw, err
}

// pushMixin provides http.Pusher.
type pushMixin struct{ core *obsWriterCore }

func (m pushMixin) Push(target string, opts *http.PushOptions) error {
	return m.core.ResponseWriter.(http.Pusher).Push(target, opts)
}

// readerFromMixin provides io.ReaderFrom, the interface io.Copy uses for the
// sendfile fast path. Only bytes the underlying ReadFrom reports as consumed
// are counted.
type readerFromMixin struct{ core *obsWriterCore }

func (m readerFromMixin) ReadFrom(r io.Reader) (int64, error) {
	if m.core.hijacked {
		return m.core.ResponseWriter.(io.ReaderFrom).ReadFrom(r)
	}
	m.core.ensureFinalStatus()
	n, err := m.core.ResponseWriter.(io.ReaderFrom).ReadFrom(r)
	m.core.bytes += n
	if err != nil && m.core.err == nil {
		m.core.err = err
	}
	return n, err
}

// The 16 concrete wrapper types, one per capability combination. Each embeds
// obsWriterCore for the ResponseWriter methods plus only the mixins for the
// capabilities the underlying writer actually supports, so a wrapper never
// advertises an interface its underlying writer lacks.

type obsFlushOnly struct {
	*obsWriterCore
	flushMixin
}

type obsHijackOnly struct {
	*obsWriterCore
	hijackMixin
}

type obsPushOnly struct {
	*obsWriterCore
	pushMixin
}

type obsReaderFromOnly struct {
	*obsWriterCore
	readerFromMixin
}

type obsFlushHijack struct {
	*obsWriterCore
	flushMixin
	hijackMixin
}

type obsFlushPush struct {
	*obsWriterCore
	flushMixin
	pushMixin
}

type obsFlushReaderFrom struct {
	*obsWriterCore
	flushMixin
	readerFromMixin
}

type obsHijackPush struct {
	*obsWriterCore
	hijackMixin
	pushMixin
}

type obsHijackReaderFrom struct {
	*obsWriterCore
	hijackMixin
	readerFromMixin
}

type obsPushReaderFrom struct {
	*obsWriterCore
	pushMixin
	readerFromMixin
}

type obsFlushHijackPush struct {
	*obsWriterCore
	flushMixin
	hijackMixin
	pushMixin
}

type obsFlushHijackReaderFrom struct {
	*obsWriterCore
	flushMixin
	hijackMixin
	readerFromMixin
}

type obsFlushPushReaderFrom struct {
	*obsWriterCore
	flushMixin
	pushMixin
	readerFromMixin
}

type obsHijackPushReaderFrom struct {
	*obsWriterCore
	hijackMixin
	pushMixin
	readerFromMixin
}

type obsAllCaps struct {
	*obsWriterCore
	flushMixin
	hijackMixin
	pushMixin
	readerFromMixin
}
