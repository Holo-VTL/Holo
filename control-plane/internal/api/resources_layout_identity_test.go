package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestCartridgeLayoutArtifactResolutionRejectsMultipleLegacyLayouts(t *testing.T) {
	root := t.TempDir()
	cartridge := domain.NewVirtualCartridge("VTA001L06", "pool-a", "library-a", "VTA001L06", 1024)
	first := filepath.Join(root, "drive-a", "vta001l06")
	second := filepath.Join(root, "drive-b", "vta001l06")
	for _, path := range []string{first, second} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(first, "data.segment"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "data.segment"), []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := resolveCartridgeLayoutArtifactDirs(cartridge, []string{root}); !errors.Is(err, domain.ErrAmbiguousLayout) {
		t.Fatalf("expected ambiguous legacy layout error, got %v", err)
	}
	for path, want := range map[string]string{
		filepath.Join(first, "data.segment"):  "first",
		filepath.Join(second, "data.segment"): "second",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("ambiguous resolver changed %s: got=%q err=%v", path, got, err)
		}
	}
}

func TestCartridgeLayoutArtifactResolutionAcceptsOnlyOneExistingLayout(t *testing.T) {
	root := t.TempDir()
	cartridge := domain.NewVirtualCartridge("VTA001L06", "pool-a", "library-a", "VTA001L06", 1024)
	legacy := filepath.Join(root, "drive-a", "vta001l06")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	targets, err := resolveCartridgeLayoutArtifactDirs(cartridge, []string{root})
	if err != nil {
		t.Fatalf("resolve one legacy layout: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected only the existing layout, got %v", targets)
	}
	if _, ok := targets[legacy]; !ok {
		t.Fatalf("expected unique legacy layout %q, got %v", legacy, targets)
	}
}
