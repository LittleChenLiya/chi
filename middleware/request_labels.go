package middleware

import (
	"context"
	"net/http"
)

// Key to use when setting the request labels.
type ctxKeyRequestLabels int

// RequestLabelsKey is the key that holds the request labels in a request context.
const RequestLabelsKey ctxKeyRequestLabels = 0

// RequestLabelsLogger is a callback that receives a snapshot of the request
// labels. The map passed to the callback is a copy owned by the caller of the
// callback, so the callback may retain or mutate it without affecting the
// labels stored in any context.
type RequestLabelsLogger func(labels map[string]string)

// WithRequestLabels returns a new context carrying the given labels merged
// over any labels already present in ctx. On duplicate keys the newly
// provided (inner) labels win over the ones inherited from the parent
// context.
//
// The merge is copy-on-write: the labels stored in the parent context are
// never modified in place, so sibling requests deriving their contexts from
// the same parent cannot observe or corrupt each other's labels, and
// cancelling one request does not clear labels seen by another.
//
// Keys and values are stored verbatim - their raw bytes are preserved with
// no trimming, casing or escaping. A nil labels map is treated as empty.
func WithRequestLabels(ctx context.Context, labels map[string]string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	merged := make(map[string]string, len(labels))
	if parent, ok := ctx.Value(RequestLabelsKey).(map[string]string); ok {
		for k, v := range parent {
			merged[k] = v
		}
	}
	for k, v := range labels {
		merged[k] = v
	}
	return context.WithValue(ctx, RequestLabelsKey, merged)
}

// GetRequestLabels returns a snapshot of the request labels stored in ctx.
//
// The returned map is always a fresh copy: mutating it does not affect the
// labels carried by the context, and subsequent calls return equal but
// independent maps. If ctx is nil or carries no labels, an empty (non-nil)
// map is returned.
func GetRequestLabels(ctx context.Context) map[string]string {
	labels := requestLabels(ctx)
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

// GetRequestLabel returns the value of a single request label and whether it
// was present in ctx.
func GetRequestLabel(ctx context.Context, key string) (string, bool) {
	v, ok := requestLabels(ctx)[key]
	return v, ok
}

// requestLabels returns the internal label map stored in ctx, or nil. The
// returned map must be treated as read-only.
func requestLabels(ctx context.Context) map[string]string {
	if ctx == nil {
		return nil
	}
	labels, _ := ctx.Value(RequestLabelsKey).(map[string]string)
	return labels
}

// RequestLabels returns a middleware that merges the given labels into the
// context of each request it serves. Labels set by an inner (later)
// middleware layer override same-named labels set by outer layers, while the
// labels visible to the outer layers remain untouched.
//
// The labels map is snapshotted when the middleware is constructed, so later
// mutations of the map by the caller have no effect on served requests.
func RequestLabels(labels map[string]string) func(http.Handler) http.Handler {
	return RequestLabelsWithLogger(labels, nil)
}

// RequestLabelsWithLogger is like RequestLabels, but once the wrapped handler
// returns it invokes log with a snapshot of the merged request labels. The
// snapshot is taken from the request context after the handler has run, and
// is a private copy - a handler cannot corrupt the logged labels by mutating
// a map it obtained via GetRequestLabels. A nil log disables the callback.
func RequestLabelsWithLogger(labels map[string]string, log RequestLabelsLogger) func(http.Handler) http.Handler {
	base := make(map[string]string, len(labels))
	for k, v := range labels {
		base[k] = v
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := WithRequestLabels(r.Context(), base)
			next.ServeHTTP(w, r.WithContext(ctx))
			if log != nil {
				log(GetRequestLabels(ctx))
			}
		})
	}
}
