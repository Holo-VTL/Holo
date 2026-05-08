package storageutil

import "testing"

func TestIsSafeDevicePath(t *testing.T) {
	for _, path := range []string{"/dev/sdb", "/dev/nvme0n1", "/dev/dm-0", "/dev/mapper.fake"} {
		if !IsSafeDevicePath(path) {
			t.Fatalf("expected %q to be safe", path)
		}
	}
	for _, path := range []string{"", "/dev/disk/by-id/example", "/tmp/sdb", "/dev/../sdb", "/dev/sdb/child"} {
		if IsSafeDevicePath(path) {
			t.Fatalf("expected %q to be unsafe", path)
		}
	}
}
