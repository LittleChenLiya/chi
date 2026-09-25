package chi

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
)

// VersionedMux is a versioned routing container. Callers build a complete
// new *Mux (routes, middlewares, mounted sub-routers) and publish it
// atomically with Swap. Every incoming request is pinned to exactly one
// version for its entire lifetime: the routing tree, nested Mount'ed
// sub-routers and the middleware chain baked into that tree all come from
// the version that was current when the request entered.
//
// Publishing never mutates a previously published tree. A retired version
// is kept alive until its last active request finishes, so in-flight
// requests keep observing the old behavior while new requests observe the
// new one.
//
// # Lifecycle and drain semantics
//
// A Version goes through three phases:
//
//  1. Published: it is the container's current version and acquires new
//     requests.
//  2. Retired: a newer version was published via Swap; it no longer
//     acquires new requests but still serves the ones it already pinned.
//  3. Drained: its last active request finished; the version holds no
//     references from in-flight work anymore and can be garbage collected
//     once the caller drops it.
//
// The drain cutoff point of a request is the moment ServeHTTP returns for
// that request: when the handler (and every middleware wrapped around it)
// has returned, the response stream has ended, a panic has unwound through
// the container, or the client disconnected and the handler gave up
// writing. The version reference is always released with a defer, so
// panics and client disconnects cannot leak a version.
//
// Note on Hijack: if a handler calls http.Hijacker.Hijack and takes over
// the connection, the container cannot observe the hijacked connection's
// lifetime. The version reference is released when the hijacking handler's
// ServeHTTP returns, NOT when the hijacked connection is eventually
// closed. Callers that hand connections off (websockets, etc.) must keep
// any state the hijacked connection needs alive themselves; the old Mux
// tree is not retained forever on their behalf.
//
// VersionedMux is safe for concurrent use. Plain *Mux usage is unaffected:
// VersionedMux composes Mux instead of changing it.
type VersionedMux struct {
	current atomic.Pointer[Version]
	nextID  atomic.Uint64
}

// Compile-time assertion that VersionedMux is a standard http.Handler.
var _ http.Handler = (*VersionedMux)(nil)

// NewVersionedMux returns a VersionedMux that serves the given Mux as its
// first published version. It panics if initial is nil.
func NewVersionedMux(initial *Mux) *VersionedMux {
	vm := &VersionedMux{}
	vm.nextID.Store(1)
	vm.current.Store(newVersion(1, mustMux(initial)))
	return vm
}

// Swap atomically publishes mux as the container's current version and
// retires the previously published one. It panics if mux is nil.
//
// Swap never modifies the retired tree. The returned retired Version stays
// alive until its active requests drain; use Version.Wait to block until
// that happens. Swap returns nil the first time it is called on a
// container that had no prior version (never the case for containers made
// with NewVersionedMux).
func (vm *VersionedMux) Swap(mux *Mux) *Version {
	next := newVersion(vm.nextID.Add(1), mustMux(mux))
	prev := vm.current.Swap(next)
	prev.retire()
	return prev
}

// Current returns the currently published Version. New requests are pinned
// to this version.
func (vm *VersionedMux) Current() *Version {
	return vm.current.Load()
}

// ServeHTTP pins the request to the version that is current right now and
// serves it with that version's Mux. The version reference is released
// exactly once, when the request finishes (response stream ended, panic
// unwound, or client disconnected and the handler returned).
func (vm *VersionedMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v := vm.current.Load()
	v.acquire()
	defer v.release()
	v.mux.ServeHTTP(w, r)
}

// Version is one published routing tree of a VersionedMux. It is immutable
// from the container's point of view: callers build the *Mux completely
// before publishing it and must not register routes on it afterwards.
type Version struct {
	id  uint64
	mux *Mux

	active  atomic.Int64
	retired atomic.Bool

	done   chan struct{}
	closeM sync.Mutex
	closed bool
}

func newVersion(id uint64, mux *Mux) *Version {
	return &Version{id: id, mux: mux, done: make(chan struct{})}
}

func mustMux(mux *Mux) *Mux {
	if mux == nil {
		panic("chi: versioned mux requires a non-nil *Mux")
	}
	return mux
}

// ID returns the version's monotonically increasing identifier. The first
// published version of a container has ID 1.
func (v *Version) ID() uint64 {
	return v.id
}

// Mux returns the routing tree pinned to this version.
func (v *Version) Mux() *Mux {
	return v.mux
}

// Active returns the number of requests currently pinned to this version.
func (v *Version) Active() int64 {
	return v.active.Load()
}

// Retired reports whether a newer version has been published since.
func (v *Version) Retired() bool {
	return v.retired.Load()
}

// Drained reports whether the version has no active requests left.
func (v *Version) Drained() bool {
	return v.active.Load() == 0
}

// Wait blocks until the version has no active requests left, the provided
// context is done, or the caller stops waiting. It returns nil once the
// version is drained and the context's error otherwise.
//
// Cancelling the context (or hitting its deadline) only stops the wait; it
// never cancels, interrupts or terminates the requests still pinned to
// this version. They keep running on the old tree and release it when they
// finish on their own.
//
// Wait is typically called on a retired Version. Waiting on the currently
// published version is allowed but only makes sense once no new requests
// can arrive, since a live version keeps acquiring requests.
func (v *Version) Wait(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-v.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// acquire pins one more request to this version.
func (v *Version) acquire() {
	v.active.Add(1)
}

// release unpins a request. When the last active request of a retired
// version leaves, the version is drained and every waiter is unblocked.
func (v *Version) release() {
	if v.active.Add(-1) == 0 && v.retired.Load() {
		v.signalDrained()
	}
}

// retire marks the version superseded. If it has no active requests left
// it drains immediately.
func (v *Version) retire() {
	v.retired.Store(true)
	if v.active.Load() == 0 {
		v.signalDrained()
	}
}

// signalDrained closes the done channel exactly once, unblocking Wait.
func (v *Version) signalDrained() {
	v.closeM.Lock()
	defer v.closeM.Unlock()
	if !v.closed {
		v.closed = true
		close(v.done)
	}
}
