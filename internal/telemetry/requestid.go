package telemetry

import (
	"context"
	"crypto/rand"
	"net/http"
)

type requestIDKey struct{}

// RequestID returns the id the server assigned to the request whose context
// is ctx, or "" outside a request (or on /healthz, which gets none).
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// withRequestID gives every request except /healthz a fresh server-generated
// id, stored in its context and returned as X-Request-ID. A client-sent
// X-Request-ID is ignored: the id names this server's handling of the
// request, and the same id tags the request's Sentry events.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		id := rand.Text()
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}
