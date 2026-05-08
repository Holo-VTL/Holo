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
