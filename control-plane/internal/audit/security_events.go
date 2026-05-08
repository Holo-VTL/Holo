package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"
)

var fallbackEventCounter uint64

func EmitDenyEvent(ctx context.Context, writer Writer, actor, objectID string, details map[string]any) {
	if writer == nil {
		return
	}
	writeEventWithFailureLog(ctx, writer, Event{
		EventID:    NewEventID("deny", objectID),
		Actor:      actor,
		Action:     "access_denied",
		ObjectType: "target",
		ObjectID:   objectID,
		Result:     "failure",
		Details:    details,
		OccurredAt: time.Now().UTC(),
	})
}

func EmitRetentionBlockedEvent(ctx context.Context, writer Writer, actor, objectID string) {
	if writer == nil {
		return
	}
	writeEventWithFailureLog(ctx, writer, Event{
		EventID:    NewEventID("retention", objectID),
		Actor:      actor,
		Action:     "retention_blocked",
		ObjectType: "cartridge",
		ObjectID:   objectID,
		Result:     "failure",
		OccurredAt: time.Now().UTC(),
	})
}

func EmitTargetRuntimeEvent(ctx context.Context, writer Writer, actor, action, objectID, result string, details map[string]any) {
	if writer == nil {
		return
	}
	if actor == "" {
		actor = "system"
	}
	if result == "" {
		result = "success"
	}
	writeEventWithFailureLog(ctx, writer, Event{
		EventID:    NewEventID(action, objectID),
		Actor:      actor,
		Action:     action,
		ObjectType: "target_publication",
		ObjectID:   objectID,
		Result:     result,
		Details:    details,
		OccurredAt: time.Now().UTC(),
	})
}

func EmitTargetAccessPolicyEvent(ctx context.Context, writer Writer, actor, action, objectID, result string, details map[string]any) {
	if writer == nil {
		return
	}
	if actor == "" {
		actor = "system"
	}
	if result == "" {
		result = "success"
	}
	writeEventWithFailureLog(ctx, writer, Event{
		EventID:    NewEventID(action, objectID),
		Actor:      actor,
		Action:     action,
		ObjectType: "target_access_policy",
		ObjectID:   objectID,
		Result:     result,
		Details:    details,
		OccurredAt: time.Now().UTC(),
	})
}

func EmitTargetDiscoveryEvent(ctx context.Context, writer Writer, actor, action, objectID, result string, details map[string]any) {
	if writer == nil {
		return
	}
	if actor == "" {
		actor = "system"
	}
	if result == "" {
		result = "success"
	}
	writeEventWithFailureLog(ctx, writer, Event{
		EventID:    NewEventID(action, objectID),
		Actor:      actor,
		Action:     action,
		ObjectType: "target_discovery",
		ObjectID:   objectID,
		Result:     result,
		Details:    details,
		OccurredAt: time.Now().UTC(),
	})
}

func writeEventWithFailureLog(ctx context.Context, writer Writer, event Event) {
	if err := writer.Write(ctx, event); err != nil {
		log.Printf(
			"AUDIT WRITE FAILURE: %v (event: %s/%s)",
			err,
			event.Action,
			event.ObjectID,
		)
	}
}

func NewEventID(prefix, objectID string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "event"
	}
	objectID = strings.TrimSpace(objectID)
	if objectID == "" {
		objectID = "unknown"
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		seq := atomic.AddUint64(&fallbackEventCounter, 1)
		return fmt.Sprintf("%s-%s-%d-%d", prefix, objectID, time.Now().UTC().UnixNano(), seq)
	}
	return fmt.Sprintf("%s-%s-%d-%s", prefix, objectID, time.Now().UTC().UnixNano(), hex.EncodeToString(nonce[:]))
}
