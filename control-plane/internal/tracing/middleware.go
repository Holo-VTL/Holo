package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"
)

var traceparentRegex = regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$`)

// TraceMiddleware extracts W3C traceparent header or generates a new trace ID.
func TraceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := extractTraceID(r)
		if traceID == "" {
			traceID = generateTraceID()
		}

		ctx := context.WithValue(r.Context(), traceIDKey{}, traceID)
		r = r.WithContext(ctx)

		next.ServeHTTP(w, r)
	})
}

func extractTraceID(r *http.Request) string {
	tp := r.Header.Get("traceparent")
	if tp == "" {
		return ""
	}
	matches := traceparentRegex.FindStringSubmatch(tp)
	if len(matches) == 4 {
		return matches[1]
	}
	return ""
}

func generateTraceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
