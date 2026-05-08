package storageutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeLayoutID(t *testing.T) {
	if got := SanitizeLayoutID("VTA000L06"); got != "vta000l06" {
		t.Fatalf("expected vta000l06 got %q", got)
	}
	if got := SanitizeLayoutID("  "); got != "unknown" {
		t.Fatalf("expected unknown got %q", got)
	}
	if got := SanitizeLayoutID("lib/a:b"); got != "lib_a_b" {
		t.Fatalf("expected lib_a_b got %q", got)
	}
}

func TestCanonicalCartridgeLayoutDir(t *testing.T) {
	got := CanonicalCartridgeLayoutDir("/tmp/root", "UAT-LIB", "VTA000L06")
	expected := filepath.Join("/tmp/root", "cartridges", "uat-lib", "vta000l06")
	if got != expected {
		t.Fatalf("expected %q got %q", expected, got)
	}
}

func TestPoolStorageRoot(t *testing.T) {
	t.Setenv("HOLO_STORAGE_POOL_ROOT_BASE", "/var/lib/holo/pools")
	got := PoolStorageRoot("Pool-A")
	expected := filepath.Join("/var/lib/holo/pools", "pool-a")
	if got != expected {
		t.Fatalf("expected %q got %q", expected, got)
	}
}

func TestLegacyCartridgeLayoutDirs(t *testing.T) {
	root := t.TempDir()
	legacyKeep := filepath.Join(root, "drive-a", "vta000l06")
	legacyOther := filepath.Join(root, "drive-b", "vta000l07")
	canonical := filepath.Join(root, "cartridges", "uat-lib", "vta000l06")
	if err := os.MkdirAll(legacyKeep, 0o755); err != nil {
		t.Fatalf("mkdir legacy keep: %v", err)
	}
	if err := os.MkdirAll(legacyOther, 0o755); err != nil {
		t.Fatalf("mkdir legacy other: %v", err)
	}
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatalf("mkdir canonical: %v", err)
	}

	dirs, err := LegacyCartridgeLayoutDirs(root, "VTA000L06")
	if err != nil {
		t.Fatalf("legacy dirs failed: %v", err)
	}
	if len(dirs) != 1 || dirs[0] != legacyKeep {
		t.Fatalf("unexpected legacy dirs: %#v", dirs)
	}
}
