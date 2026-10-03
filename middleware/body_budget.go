package middleware

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
)

// ErrBodyBudgetExceeded is the sentinel error returned to anyone reading the
// request body once a per-route body budget has been exceeded.
//
// Detect it with errors.Is(err, middleware.ErrBodyBudgetExceeded), or use
// errors.As to inspect the *BodyBudgetExceededError for the effective limit
// and the number of bytes actually read.
var ErrBodyBudgetExceeded = errors.New("middleware: request body budget exceeded")

// BodyBudgetExceededError is returned by body reads that cross a body
// budget. It wraps ErrBodyBudgetExceeded.
type BodyBudgetExceededError struct {
	// Limit is the effective budget in bytes (the minimum of every nested
	// BodyBudget middleware on the route).
	Limit int64
	// Read is the number of body bytes handed out at the point the budget was
	// exceeded. Only bytes actually read are charged.
	Read int64
}

func (e *BodyBudgetExceededError) Error() string {
	return fmt.Sprintf("middleware: request body budget exceeded: limit %d bytes, read %d", e.Limit, e.Read)
}

// Is reports whether the target is ErrBodyBudgetExceeded.
func (e *BodyBudgetExceededError) Is(target error) bool { return target == ErrBodyBudgetExceeded }

type bodyBudgetCtxKey struct{}

// bodyBudget is the shared, per-request accounting state. Nested BodyBudget
// middlewares on the same route share one instance and race to tighten its
// limit, so the effective limit is the minimum of every layer.
type bodyBudget struct {
	limit atomic.Int64
	read  atomic.Int64

	exceeded atomic.Bool
	err      atomic.Pointer[BodyBudgetExceededError]
}

// tighten lowers the effective limit. A limit <= 0 leaves the current limit
// untouched (a zero limit means "unlimited").
func (b *bodyBudget) tighten(limit int64) {
	if limit <= 0 {
		return
	}
	for {
		cur := b.limit.Load()
		if cur > 0 && cur <= limit {
			return
		}
		if b.limit.CompareAndSwap(cur, limit) {
			return
		}
	}
}

// charge accounts n bytes that were actually handed to a reader and reports
// whether the budget is now exceeded. It is called only after the wrapped
// body's Read has delivered bytes, so the whole request body is never
// pre-read.
func (b *bodyBudget) charge(n int) bool {
	if n <= 0 {
		return b.exceeded.Load()
	}
	total := b.read.Add(int64(n))
	limit := b.limit.Load()
	if limit > 0 && total > limit {
		return b.markExceeded()
	}
	return b.exceeded.Load()
}

func (b *bodyBudget) markExceeded() bool {
	if b.exceeded.CompareAndSwap(false, true) {
		b.err.Store(&BodyBudgetExceededError{Limit: b.limit.Load(), Read: b.read.Load()})
	}
	return true
}

// Err returns the exceeded error, or nil while the budget still holds.
func (b *bodyBudget) Err() error {
	if e := b.err.Load(); e != nil {
		return e
	}
	return nil
}

// budgetReader wraps the request body and charges only the bytes it actually
// hands out. Following the same boundary convention as http.MaxBytesReader,
// the single byte that crosses the limit is still delivered (paired with the
// exceeded error); every subsequent read fails without touching the body.
type budgetReader struct {
	rc     io.ReadCloser
	budget *bodyBudget
}

var _ io.ReadCloser = (*budgetReader)(nil)

func (r *budgetReader) Read(p []byte) (int, error) {
	if r.budget.exceeded.Load() {
		return 0, r.budget.Err()
	}
	n, err := r.rc.Read(p)
	// Charge only the bytes the underlying read actually delivered.
	if r.budget.charge(n) {
		// The budget error wins so a reader can recognize an oversized body
		// even if the underlying read also reported EOF.
		return n, r.budget.Err()
	}
	return n, err
}

// Close delegates to the wrapped body. Nested BodyBudget layers share this
// single wrapper, so the original request body is closed exactly once.
func (r *budgetReader) Close() error {
	return r.rc.Close()
}

// BodyBudget returns a composable middleware that applies a per-route budget
// (in bytes) to the request body. It changes no global server setting and
// never pre-reads the body: only the bytes a handler actually reads are
// counted.
//
// BodyBudget middlewares compose. When several are mounted on nested routers
// or groups, the effective budget for a request is the minimum limit of every
// layer: each nested middleware tightens the same per-request budget instead
// of installing a second, independent limiter.
//
// Once the budget is exceeded:
//
//   - Any further read of r.Body fails with an error that satisfies
//     errors.Is(err, ErrBodyBudgetExceeded). Handlers can also query it via
//     BodyBudgetError(r.Context()).
//   - If the handler returns without starting the response, the middleware
//     answers with 413 Request Entity Too Large.
//   - If the handler has already started the response, the middleware does
//     nothing; it never appends an error body to a response that was already
//     written.
//
// A non-positive limit means "unlimited" for that layer (it cannot loosen a
// tighter outer budget). Routes without the middleware are unaffected.
func BodyBudget(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := r.Context().Value(bodyBudgetCtxKey{}).(*bodyBudget)
			if b == nil {
				// Outermost BodyBudget layer: create the shared budget and
				// wrap the body once.
				b = &bodyBudget{}
				r = r.WithContext(context.WithValue(r.Context(), bodyBudgetCtxKey{}, b))
				r.Body = &budgetReader{rc: &onceReadCloser{rc: r.Body}, budget: b}
			}
			// Every layer tightens the shared limit, so nested layers can only
			// reduce it.
			b.tighten(limit)

			bw := newBudgetResponseWriter(w, b)
			next.ServeHTTP(bw, r)

			if b.Err() != nil && !bw.started() {
				// The handler never began a response, so this path owns the
				// 413. WriteHeader commits the status; no later code can append
				// a second body.
				http.Error(bw, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
			}
		})
	}
}

// onceReadCloser makes closing the original request body idempotent.
type onceReadCloser struct {
	rc     io.ReadCloser
	mu     sync.Mutex
	closed bool
}

var _ io.ReadCloser = (*onceReadCloser)(nil)

func (o *onceReadCloser) Read(p []byte) (int, error) { return o.rc.Read(p) }

func (o *onceReadCloser) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.closed = true
	return o.rc.Close()
}

// BodyBudgetError returns the body-budget error recorded on ctx, or nil if the
// body budget has not been exceeded. The returned error satisfies
// errors.Is(err, ErrBodyBudgetExceeded) and is a *BodyBudgetExceededError.
func BodyBudgetError(ctx context.Context) error {
	b, _ := ctx.Value(bodyBudgetCtxKey{}).(*bodyBudget)
	if b == nil {
		return nil
	}
	return b.Err()
}

// BodyBudgetUsed reports how many request body bytes have actually been read
// (and therefore charged) on ctx. It returns 0 for requests without a
// BodyBudget middleware.
func BodyBudgetUsed(ctx context.Context) int64 {
	b, _ := ctx.Value(bodyBudgetCtxKey{}).(*bodyBudget)
	if b == nil {
		return 0
	}
	return b.read.Load()
}

// BodyBudgetLimit reports the effective (minimum) body budget in bytes for
// ctx, or 0 when there is no BodyBudget middleware or the budget is
// unlimited.
func BodyBudgetLimit(ctx context.Context) int64 {
	b, _ := ctx.Value(bodyBudgetCtxKey{}).(*bodyBudget)
	if b == nil {
		return 0
	}
	return b.limit.Load()
}

// budgetResponseWriter tracks whether the response has started, so the
// middleware can decide whether it still owns the 413 response. It forwards
// Flusher and Hijacker capabilities so streaming or hijacking handlers keep
// working.
type budgetResponseWriter struct {
	http.ResponseWriter
	budget *bodyBudget

	mu        sync.Mutex
	headerSet bool
	status    int
}

var (
	_ http.ResponseWriter = (*budgetResponseWriter)(nil)
	_ http.Flusher        = (*budgetResponseWriter)(nil)
	_ http.Hijacker       = (*budgetResponseWriter)(nil)
)

func newBudgetResponseWriter(w http.ResponseWriter, b *bodyBudget) *budgetResponseWriter {
	return &budgetResponseWriter{ResponseWriter: w, budget: b}
}

func (w *budgetResponseWriter) started() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.headerSet
}

func (w *budgetResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	if w.headerSet {
		w.mu.Unlock()
		return
	}
	w.headerSet = true
	w.status = status
	w.mu.Unlock()
	w.ResponseWriter.WriteHeader(status)
}

func (w *budgetResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if !w.headerSet {
		w.headerSet = true
		w.status = http.StatusOK
	}
	w.mu.Unlock()
	return w.ResponseWriter.Write(p)
}

// Unwrap returns the underlying ResponseWriter (supports
// http.ResponseController and unwrapping-aware tooling).
func (w *budgetResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush forwards the flush and marks the response as started.
func (w *budgetResponseWriter) Flush() {
	w.mu.Lock()
	if !w.headerSet {
		w.headerSet = true
		w.status = http.StatusOK
	}
	w.mu.Unlock()
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// Hijack forwards the hijack and marks the response as started (the
// connection has been taken over).
func (w *budgetResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.mu.Lock()
	w.headerSet = true
	w.mu.Unlock()
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hj.Hijack()
}
