package domain

import "testing"

func TestNewVirtualDriveSetsDefaultIQN(t *testing.T) {
	drive, err := NewVirtualDrive("Drive A.01", "lib-1", 1)
	if err != nil {
		t.Fatalf("new drive failed: %v", err)
	}
	if drive.IQN != "iqn.2026-04.cloud.backupnext.holo:drive-drive-a.01" {
		t.Fatalf("unexpected drive iqn: %s", drive.IQN)
	}
}

func TestVirtualDriveMountAndUnmountTransitions(t *testing.T) {
	drive, err := NewVirtualDrive("drive-a", "lib-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := drive.Mount(""); err != ErrInvalidState {
		t.Fatalf("empty cartridge id should be rejected, got %v", err)
	}
	if err := drive.Mount("cart-a"); err != nil {
		t.Fatalf("mount cartridge: %v", err)
	}
	if drive.MountState != MountLoaded || drive.MountedCartridgeID != "cart-a" {
		t.Fatalf("mount should record cartridge state: %+v", drive)
	}
	if err := drive.Mount("cart-b"); err != ErrInvalidState {
		t.Fatalf("mounting over loaded media should fail, got %v", err)
	}
	drive.MountState = MountBusy
	if err := drive.Unmount(); err != ErrInvalidState {
		t.Fatalf("busy drive should not unmount, got %v", err)
	}
	drive.MountState = MountError
	if err := drive.Unmount(); err != nil {
		t.Fatalf("recover drive by unmounting: %v", err)
	}
	if drive.MountState != MountEmpty || drive.MountedCartridgeID != "" {
		t.Fatalf("unmount should clear drive state: %+v", drive)
	}
}
