package domain

import "testing"

func TestTargetPublicationTransitions(t *testing.T) {
	p, err := NewTargetPublication("pub-1", "pool-1", "lib-1", "drive-1", "car-1", "iqn.2026-04.ai.holo:drive-1")
	if err != nil {
		t.Fatalf("new publication failed: %v", err)
	}
	if p.DeviceRole != "drive" {
		t.Fatalf("expected default device role drive, got %s", p.DeviceRole)
	}
	if p.CompressionEnabled || p.DedupEnabled {
		t.Fatalf("expected compression and dedup to default off, got compression=%v dedup=%v", p.CompressionEnabled, p.DedupEnabled)
	}
	if err := p.MarkReady("127.0.0.1:3260"); err != nil {
		t.Fatalf("mark ready failed: %v", err)
	}
	if err := p.Disable(); err != nil {
		t.Fatalf("disable failed: %v", err)
	}
	if err := p.Reopen(); err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	if err := p.MarkFailed("publish error"); err != nil {
		t.Fatalf("mark failed failed: %v", err)
	}
}

func TestTargetPublicationSetDeviceIdentity(t *testing.T) {
	p, err := NewTargetPublication("pub-2", "pool-1", "lib-1", "drive-1", "car-1", "iqn.2026-04.ai.holo:dev")
	if err != nil {
		t.Fatalf("new publication failed: %v", err)
	}
	if err := p.SetDeviceIdentity("changer", "ibm-03584l32"); err != nil {
		t.Fatalf("set device identity failed: %v", err)
	}
	if p.DeviceRole != "changer" {
		t.Fatalf("expected changer role, got %s", p.DeviceRole)
	}
	if p.DeviceProfile != "ibm-03584l32" {
		t.Fatalf("expected profile ibm-03584l32, got %s", p.DeviceProfile)
	}
	if err := p.SetDeviceIdentity("bad-role", "x"); err != ErrInvalidInput {
		t.Fatalf("expected invalid input for bad role, got %v", err)
	}
}
