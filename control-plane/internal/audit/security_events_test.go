package audit

import (
	"context"
	"testing"
)

func TestSecurityEventEmittersWriteExpectedEvents(t *testing.T) {
	ctx := context.Background()
	EmitDenyEvent(ctx, nil, "operator", "target-a", nil)
	EmitRetentionBlockedEvent(ctx, nil, "operator", "cart-a")
	EmitTargetRuntimeEvent(ctx, nil, "operator", "publish", "pub-a", "success", nil)
	EmitTargetDiscoveryEvent(ctx, nil, "operator", "discovery", "target-a", "success", nil)

	writer := NewMemoryWriter()
	EmitDenyEvent(ctx, writer, "operator", "target-a", map[string]any{"reason": "acl"})
	EmitRetentionBlockedEvent(ctx, writer, "operator", "cart-a")
	EmitTargetRuntimeEvent(ctx, writer, "", "publish", "pub-a", "", nil)
	EmitTargetDiscoveryEvent(ctx, writer, "scanner", "discovery", "target-a", "failure", nil)

	events := writer.Events()
	if len(events) != 4 {
		t.Fatalf("expected four audit events, got %d", len(events))
	}
	want := []Event{
		{Actor: "self-asserted:operator", Action: "access_denied", ObjectType: "target", ObjectID: "target-a", Result: "failure"},
		{Actor: "self-asserted:operator", Action: "retention_blocked", ObjectType: "cartridge", ObjectID: "cart-a", Result: "failure"},
		{Actor: "system", Action: "publish", ObjectType: "target_publication", ObjectID: "pub-a", Result: "success"},
		{Actor: "self-asserted:scanner", Action: "discovery", ObjectType: "target_discovery", ObjectID: "target-a", Result: "failure"},
	}
	for i := range want {
		if events[i].Actor != want[i].Actor || events[i].Action != want[i].Action || events[i].ObjectType != want[i].ObjectType || events[i].ObjectID != want[i].ObjectID || events[i].Result != want[i].Result {
			t.Errorf("event %d mismatch: got %+v, want %+v", i, events[i], want[i])
		}
		if events[i].EventID == "" {
			t.Errorf("event %d should receive an event id", i)
		}
	}
}
