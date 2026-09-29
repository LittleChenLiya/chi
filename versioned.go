package chi

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
)

// VersionedMux is an atomic, versioned container for a *Mux. It lets callers
// build a complete new router off to the side and publish it with Swap,
// without ever mutating a tree that in-flight requests are still using.
//
// # Lifecycle
//
//  1. Build a fully configured *Mux (routes, mounts, middlewares) exactly as
//     you would for a plain chi server. A published *Mux must be treated as
//     immutable: never register routes or middlewares on it after Swap.
//  2. Publish it with Swap. Swap is atomic and never modifies the previous
//     tree; it only retires the previous *Version and returns it.
//  3. Every request that enters ServeHTTP pins the *Version that is current
//     at that instant. Nested Mount'ed sub-routers and every middleware in
//     that tree run against the pinned version, so a single request always
//     sees one consistent routing tree.
//  4. A retired version is kept alive until its last active request
//     finishes. Version.Wait / Version.Drained report when a version has
//     fully drained, at which point it can no longer be reached by any
//     request and is eligible for garbage collection.
//
// # Request reference release
//
// The pinned version reference is released exactly when the top-level
// ServeHTTP call returns, on every exit path:
//
//   - normal completion of the response stream,
//   - a panic unwinding through the handler chain (install
//     middleware.Recoverer inside each published *Mux to convert panics
//     into 500 responses; VersionedMux itself never swallows panics),
//   - client disconnects (net/http cancels the request context but keeps
//     running the handler; the reference is still held until ServeHTTP
//     returns, so handlers should honor r.Context().Done() to drain
//     promptly).
//
// # Hijacked connections
//
// The drain cutoff point of this API is the return of the top-level
// ServeHTTP call. If a handler calls http.Hijacker.Hijack and takes over
// the connection, the version reference is still released as soon as the
// handler returns - a hijacked connection does NOT pin the version (and
// therefore the *Mux) beyond that point, so old routers can always drain
// and be collected. Hijacking handlers must not touch the router, its
// contexts, or the http.ResponseWriter after returning.
//
// VersionedMux is safe for concurrent use. Plain *Mux usage is unaffected
// by this type.
type VersionedMux struct {
	cur atomic.Pointer[Version]
}

// NewVersionedMux returns a VersionedMux serving the initial *Mux. The
// initial mux must be fully configured and non-nil.
func NewVersionedMux(initial *Mux) *VersionedMux {
	if initial == nil {
		panic("chi: NewVersionedMux requires a non-nil initial *Mux")
	}
	vx := &VersionedMux{}
	vx.cur.Store(newVersion(initial))
	return vx
}

// Swap atomically publishes next and retires the previously current
// version. It returns the retired *Version, which drains once its
// in-flight requests finish. Swap never mutates the retired tree.
func (vx *VersionedMux) Swap(next *Mux) (retired *Version) {
	if next == nil {
		panic("chi: cannot Swap a nil *Mux")
	}
	old := vx.cur.Swap(newVersion(next))
	old.retire()
	return old
}

// Current returns the version that new requests will be pinned to.
func (vx *VersionedMux) Current() *Version {
	return vx.cur.Load()
}

// ServeHTTP pins the current version for the duration of the request and
// serves it. The pin is released when ServeHTTP returns, on every exit
// path including panics.
func (vx *VersionedMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v := vx.acquire()
	defer v.release()
	v.mux.ServeHTTP(w, r)
}

// acquire pins the current version, reloading if the loaded version was
// retired by a concurrent Swap before the pin could be taken.
func (vx *VersionedMux) acquire() *Version {
	for {
		v := vx.cur.Load()
		if v.tryAcquire() {
			return v
		}
	}
}

// Version is a single published routing tree plus its drain state. A
// Version is retired by Swap and drained once its last active request
// finishes. The zero value is not usable; obtain Versions from
// VersionedMux.Swap or VersionedMux.Current.
type Version struct {
	mux *Mux

	mu      sync.Mutex
	active  int64
	retired bool
	drained chan struct{}
}

func newVersion(mux *Mux) *Version {
	return &Version{mux: mux, drained: make(chan struct{})}
}

// Mux returns the immutable routing tree published for this version.
func (v *Version) Mux() *Mux {
	return v.mux
}

// Active reports the number of requests currently pinned to this version.
func (v *Version) Active() int64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.active
}

// Retired reports whether this version has been swapped out. Retired
// versions accept no new requests and drain as in-flight ones finish.
func (v *Version) Retired() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.retired
}

// Drained returns a channel that is closed once the version has been
// retired and its active request count has reached zero.
func (v *Version) Drained() <-chan struct{} {
	return v.drained
}

// Wait blocks until the version has drained or ctx is done. Cancelling
// ctx only stops the wait; it never interrupts the in-flight requests
// holding this version. Note that waiting on the current (not yet
// retired) version blocks until a future Swap retires it and its
// requests drain.
func (v *Version) Wait(ctx context.Context) error {
	select {
	case <-v.drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// tryAcquire pins the version for one request, reporting false if the
// version has already been retired.
func (v *Version) tryAcquire() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.retired {
		return false
	}
	v.active++
	return true
}

// release drops one request pin, closing the drained channel when the
// last active request of a retired version finishes.
func (v *Version) release() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.active--
	if v.retired && v.active == 0 {
		close(v.drained)
	}
}

// retire marks the version as swapped out, closing the drained channel
// immediately if no requests are in flight.
func (v *Version) retire() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.retired = true
	if v.active == 0 {
		close(v.drained)
	}
}
