package storageutil

import "testing"

func TestIsSafeDevicePath(t *testing.T) {
	for _, path := range []string{"/dev/sdb", "/dev/nvme0n1", "/dev/dm-0", "/dev/mapper.fake"} {
		if !IsSafeDevicePath(path) {
			t.Fatalf("expected %q to be safe", path)
		}
	}
	for _, path := range []string{"", "/dev/disk/by-id/example", "/tmp/sdb", "/dev/../sdb", "/dev/sdb/child", "/dev/.", "/dev/.."} {
		if IsSafeDevicePath(path) {
			t.Fatalf("expected %q to be unsafe", path)
		}
	}
}

func TestPhysicalIdentityProjectionsExposeFilesystemAliases(t *testing.T) {
	cases := []struct {
		name    string
		left    string
		right   string
		project func(string) string
	}{
		{name: "pool root", left: "pool.a", right: "pool_a", project: PoolRootProjection},
		{name: "library directory", left: "lib.a", right: "lib_a", project: LibraryDirectoryProjection},
		{name: "drive legacy directory", left: "drive.a", right: "drive_a", project: DriveDirectoryProjection},
		{name: "cartridge metadata", left: "cart.a", right: "cart_a", project: CartridgeMetadataProjection},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.project(tc.left) != tc.project(tc.right) {
				t.Fatalf("expected %q and %q to alias", tc.left, tc.right)
			}
		})
	}
	if gotLeft, gotRight := MediaStatePathProjection("a_", "_b"), MediaStatePathProjection("a__", "b"); gotLeft != gotRight {
		t.Fatalf("expected delimiter-shifted drive pairs to alias, got %q and %q", gotLeft, gotRight)
	}
	if gotLeft, gotRight := CartridgeLayoutProjection("lib.a", "VTA001L06"), CartridgeLayoutProjection("lib_a", "VTA001L06"); gotLeft != gotRight {
		t.Fatalf("expected library-scoped cartridge paths to alias, got %q and %q", gotLeft, gotRight)
	}
}
