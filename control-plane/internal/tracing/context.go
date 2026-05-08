package tracing

import (
	"context"
	"encoding/json"
	"log"
	"time"
)

type traceIDKey struct{}

// TraceID extracts the trace ID from context, or returns empty string if not found.
func TraceID(ctx context.Context) string {
	if val, ok := ctx.Value(traceIDKey{}).(string); ok {
		return val
	}
	return ""
}

// LogInfo writes a structured JSON log entry to stdout.
func LogInfo(ctx context.Context, component, msg string, fields ...any) {
	entry := map[string]any{
		"level":     "info",
		"ts":        time.Now().UTC().Format(time.RFC3339),
		"component": component,
		"msg":       msg,
	}

	if tid := TraceID(ctx); tid != "" {
		entry["traceId"] = tid
	}

	for i := 0; i < len(fields)-1; i += 2 {
		if k, ok := fields[i].(string); ok {
			entry[k] = fields[i+1]
		}
	}

	b, _ := json.Marshal(entry)
	log.Println(string(b))
}

func LogError(ctx context.Context, component, msg string, err error, fields ...any) {
	entry := map[string]any{
		"level":     "error",
		"ts":        time.Now().UTC().Format(time.RFC3339),
		"component": component,
		"msg":       msg,
		"error":     err.Error(),
	}

	if tid := TraceID(ctx); tid != "" {
		entry["traceId"] = tid
	}

	for i := 0; i < len(fields)-1; i += 2 {
		if k, ok := fields[i].(string); ok {
			entry[k] = fields[i+1]
		}
	}

	b, _ := json.Marshal(entry)
	log.Println(string(b))
}
