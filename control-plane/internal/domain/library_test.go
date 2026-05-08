package domain

import "testing"

func TestNewVirtualLibraryDefaultsToPerformanceFirstPolicy(t *testing.T) {
	library, err := NewVirtualLibrary("lib-1", "Library 1")
	if err != nil {
		t.Fatalf("new library failed: %v", err)
	}
	if library.CompressionEnabled || library.DedupEnabled {
		t.Fatalf("expected compression and dedup to default off, got compression=%v dedup=%v", library.CompressionEnabled, library.DedupEnabled)
	}
}
