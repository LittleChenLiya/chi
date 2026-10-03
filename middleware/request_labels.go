package middleware

import (
	"context"
	"net/http"
)

// RequestLabelsKey is the context key under which the per-request label
// container is stored. It is exported so that custom code can inspect the
// container via context.Value, but callers should prefer WithRequestLabels
// and GetRequestLabels, which merge and snapshot safely.
var RequestLabelsKey = &contextKey{"RequestLabels"}

// LabelLogger is a callback invoked by the RequestLabels middleware after
// the handler chain completes. labels is a snapshot copy of the request
// labels: mutating it has no effect on the labels stored in the request
// context or on subsequent reads.
type LabelLogger func(labels map[string][]byte)

// RequestLabels is a middleware that maintains an independent label
// container in the request context. Labels are appended with
// WithRequestLabels and read with GetRequestLabels.
//
// The container is immutable per layer: appending labels merges them into
// a new map stored in a new context value, so the parent context is never
// modified in place and concurrent requests never share label state.
// Duplicate keys are overridden by the nearer (inner) layer.
//
// RequestLabels itself does not append any labels; it only ensures the
// container is present. Use RequestLabelsWithLogger to receive a snapshot
// of the labels after the handler chain completes.
func RequestLabels(next http.Handler) http.Handler {
	return RequestLabelsWithLogger(nil)(next)
}

// RequestLabelsWithLogger is like RequestLabels but also calls logger after
// the handler chain returns, passing a snapshot copy of the request labels.
// The snapshot is safe to retain or mutate.
//
// Place this middleware after the label-appending middlewares whose labels
// you want to observe; because labels live in immutable context layers, a
// logger placed before an appending middleware will not see that layer's
// labels.
func RequestLabelsWithLogger(logger LabelLogger) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		fn := func(w http.ResponseWriter, r *http.Request) {
			if logger != nil {
				defer func() {
					logger(GetRequestLabels(r.Context()))
				}()
			}
			next.ServeHTTP(w, r)
		}
		return http.HandlerFunc(fn)
	}
}

// WithRequestLabels returns a new context with labels appended on top of
// the parent context's labels. Labels from the parent are merged into a
// new map; keys present in labels override keys from the parent context
// (the nearer layer wins). The parent context and its labels are never
// modified in place. A nil labels map is treated as empty.
//
// The byte slices in labels are copied, so later mutations by the caller
// do not affect the labels stored in the returned context. Keys are
// preserved verbatim as strings.
func WithRequestLabels(ctx context.Context, labels map[string][]byte) context.Context {
	parent := lookupRequestLabels(ctx)
	merged := make(map[string][]byte, len(parent)+len(labels))
	for k, v := range parent {
		merged[k] = v
	}
	for k, v := range labels {
		merged[k] = copyBytes(v)
	}
	return context.WithValue(ctx, RequestLabelsKey, merged)
}

// GetRequestLabels returns a snapshot copy of the request labels stored in
// the context. The returned map and its byte slices are independent copies:
// mutating them has no effect on the labels stored in the context or on
// subsequent reads. If no labels are present, an empty (non-nil) map is
// returned.
func GetRequestLabels(ctx context.Context) map[string][]byte {
	labels := lookupRequestLabels(ctx)
	out := make(map[string][]byte, len(labels))
	for k, v := range labels {
		out[k] = copyBytes(v)
	}
	return out
}

// lookupRequestLabels returns the internal label map, or nil if none is
// present. The returned map must not be mutated; callers needing a copy
// should use GetRequestLabels.
func lookupRequestLabels(ctx context.Context) map[string][]byte {
	if ctx == nil {
		return nil
	}
	labels, _ := ctx.Value(RequestLabelsKey).(map[string][]byte)
	return labels
}

// copyBytes returns a copy of b. A nil slice stays nil; an empty non-nil
// slice stays empty non-nil.
func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
