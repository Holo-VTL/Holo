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

func TestTargetPublicationInvalidTransitionsAndRuntimeFailure(t *testing.T) {
	p, err := NewTargetPublication("pub-3", "pool-1", "lib-1", "drive-1", "car-1", "iqn.2026-04.ai.holo:drive-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Disable(); err != ErrInvalidState {
		t.Fatalf("creating publication cannot be disabled, got %v", err)
	}
	if err := p.Reopen(); err != ErrInvalidState {
		t.Fatalf("creating publication cannot be reopened, got %v", err)
	}
	if err := p.MarkReady(""); err != ErrInvalidInput {
		t.Fatalf("empty portal should be rejected, got %v", err)
	}
	if err := p.SetDeviceIdentity("", "../unsafe"); err != ErrInvalidInput {
		t.Fatalf("invalid device profile should be rejected, got %v", err)
	}
	p.SetDriveProfile("lto-6")
	if p.DriveProfile != "lto-6" {
		t.Fatalf("valid drive profile was not set: %q", p.DriveProfile)
	}
	p.SetDriveProfile("../unsafe")
	if p.DriveProfile != "lto-6" {
		t.Fatalf("invalid drive profile should preserve prior value: %q", p.DriveProfile)
	}
	if err := p.MarkFailed(""); err != nil {
		t.Fatalf("empty failure message should use its default: %v", err)
	}
	if p.LastError != "publish failed" {
		t.Fatalf("expected default publication failure message, got %q", p.LastError)
	}
	if err := p.MarkFailed("again"); err != ErrInvalidState {
		t.Fatalf("failed publication cannot fail twice, got %v", err)
	}
	if err := p.Disable(); err != nil {
		t.Fatalf("failed publication should be disableable: %v", err)
	}
	if err := p.Reopen(); err != nil {
		t.Fatalf("disabled publication should reopen: %v", err)
	}
	p.MarkRuntimeFailed("")
	if p.State != PublicationFailed || p.LastError != "runtime restore failed" {
		t.Fatalf("runtime failure should set default error state: %+v", p)
	}
}
