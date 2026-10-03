package middleware

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
)

// ErrBodyBudgetExceeded is the error returned to a request body reader once
// the BodyBudget layer with the smallest budget has been read past its limit.
// It is safe to test with errors.Is.
var ErrBodyBudgetExceeded = errors.New("request body budget exceeded")

// bodyBudgetCtxKey stores the per-request budget state in the request context.
type bodyBudgetCtxKey struct{}

// bodyBudgetState is shared by every BodyBudget layer wrapping the same
// request body. It closes the original body exactly once and records whether
// any layer has been exceeded while reading.
type bodyBudgetState struct {
	orig   io.ReadCloser
	closer sync.Once

	mu       sync.Mutex
	exceeded bool
}

// Close closes the original body exactly once, no matter how many BodyBudget
// layers wrap it or how often the outermost reader is closed.
func (s *bodyBudgetState) Close() error {
	var err error
	s.closer.Do(func() {
		err = s.orig.Close()
	})
	return err
}

func (s *bodyBudgetState) markExceeded() {
	s.mu.Lock()
	s.exceeded = true
	s.mu.Unlock()
}

func (s *bodyBudgetState) failed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exceeded
}

// budgetReader counts the bytes actually read through it and returns
// ErrBodyBudgetExceeded to the reader once its layer's budget is spent.
// It never reads ahead: bytes are charged only when a Read actually returns
// them, so chunked/unknown-length bodies cannot bypass the budget.
type budgetReader struct {
	inner io.Reader
	state *bodyBudgetState
	limit int64
	used  int64
	hit   bool
}

func (r *budgetReader) Read(p []byte) (int, error) {
	if r.hit {
		return 0, ErrBodyBudgetExceeded
	}
	n, err := r.inner.Read(p)
	r.used += int64(n)
	if r.used > r.limit {
		r.hit = true
		r.state.markExceeded()
		err = errors.Join(err, ErrBodyBudgetExceeded)
	}
	return n, err
}

func (r *budgetReader) Close() error {
	return r.state.Close()
}

// BodyBudget returns a middleware that limits the number of request body
// bytes a handler may read along the wrapped processing path.
//
// Unlike http.MaxBytesReader it never writes a response itself: when a read
// exceeds the budget the error is returned to the reader, and only the
// processing path that has not yet started a response turns it into a 413.
// A handler that already wrote a response keeps it; no error page is ever
// appended after the fact.
//
// BodyBudget layers are composable. When several layers wrap the same route
// the effective budget is the smallest limit, because each layer charges the
// same byte stream independently. Routes not wrapped by any layer are
// unaffected.
func BodyBudget(limit int64) func(http.Handler) http.Handler {
	if limit < 0 {
		panic("chi/middleware: BodyBudget expects a non-negative limit")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body == nil {
				r.Body = http.NoBody
			}

			state, _ := r.Context().Value(bodyBudgetCtxKey{}).(*bodyBudgetState)
			if state == nil {
				state = &bodyBudgetState{orig: r.Body}
				r = r.WithContext(context.WithValue(r.Context(), bodyBudgetCtxKey{}, state))
			}

			r.Body = &budgetReader{inner: r.Body, state: state, limit: limit}

			ww := NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			if state.failed() && ww.Status() == 0 {
				http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
			}
		})
	}
}

// BodyBudgetErr returns the body budget error if any BodyBudget layer wrapping
// the request body was exceeded while the handler read it, or nil otherwise.
// Handlers can use it to decide how to respond after a read error.
func BodyBudgetErr(ctx context.Context) error {
	state, _ := ctx.Value(bodyBudgetCtxKey{}).(*bodyBudgetState)
	if state == nil || !state.failed() {
		return nil
	}
	return ErrBodyBudgetExceeded
}
