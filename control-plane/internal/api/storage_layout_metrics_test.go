package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageLayoutPrometheusTextScansSegmentFiles(t *testing.T) {
	root := t.TempDir()
	layoutDir := filepath.Join(root, "pool-a", "cartridges", "lib-a", "cart-a")
	if err := os.MkdirAll(layoutDir, 0o755); err != nil {
		t.Fatalf("create layout dir: %v", err)
	}
	writeSizedFile(t, filepath.Join(layoutDir, "data_000001.seg"), 10)
	writeSizedFile(t, filepath.Join(layoutDir, "dedup.segment"), 20)
	writeSizedFile(t, filepath.Join(layoutDir, "reclaim.segment"), 30)
	writeSizedFile(t, filepath.Join(layoutDir, "notes.txt"), 40)

	out := StorageLayoutPrometheusText(root)
	for _, want := range []string{
		"holo_storage_segment_files 3",
		"holo_storage_segment_bytes 60",
		"holo_storage_data_segment_bytes 10",
		"holo_storage_dedup_segment_bytes 20",
		"holo_storage_reclaim_segment_bytes 30",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in metrics:\n%s", want, out)
		}
	}
}

func TestStorageLayoutPrometheusTextMissingRootIsZero(t *testing.T) {
	out := StorageLayoutPrometheusText(filepath.Join(t.TempDir(), "missing"))
	if !strings.Contains(out, "holo_storage_segment_files 0") {
		t.Fatalf("expected zero segment count for missing root, got:\n%s", out)
	}
}

func writeSizedFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
