package storageutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadDriveMediaStateUsesSharedHandlerFormat(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", dir)

	path, err := MediaStatePath("Library-A", "Drive 1")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "library-a__drive_1.state"); path != want {
		t.Fatalf("MediaStatePath() = %q, want %q", path, want)
	}
	if loaded, err := ReadDriveMediaState("Library-A", "Drive 1"); err != nil || loaded != "" {
		t.Fatalf("missing shared state = (%q, %v), want empty and nil", loaded, err)
	}
	if err := os.WriteFile(path, []byte("cartridge=VTL000123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if loaded, err := ReadDriveMediaState("Library-A", "Drive 1"); err != nil || loaded != "VTL000123" {
		t.Fatalf("shared state = (%q, %v), want VTL000123 and nil", loaded, err)
	}
	if err := os.WriteFile(path, []byte("cartridge=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if loaded, err := ReadDriveMediaState("Library-A", "Drive 1"); err != nil || loaded != "" {
		t.Fatalf("empty cartridge state = (%q, %v), want empty and nil", loaded, err)
	}
}

func TestReadDriveMediaStateFailsClosedOnMalformedOrUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", dir)
	path, err := MediaStatePath("library-a", "drive-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("cartridge=cart-a\nextra=unexpected\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDriveMediaState("library-a", "drive-a"); err == nil {
		t.Fatal("expected malformed shared state to fail")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDriveMediaState("library-a", "drive-a"); err == nil {
		t.Fatal("expected unreadable state path to fail")
	}
}

func TestMediaStatePathSanitizesAllPathComponents(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", dir)
	path, err := MediaStatePath("lib/../escape", "drive-a")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("MediaStatePath escaped runtime directory: %q", path)
	}
}
