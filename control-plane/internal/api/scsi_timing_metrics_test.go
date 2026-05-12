package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSCSITimingPrometheusTextAggregatesProcessFiles(t *testing.T) {
	dir := t.TempDir()
	writeTimingFile(t, filepath.Join(dir, "pub-a.prom"), `
holo_scsi_commands_total{bucket="read"} 2
holo_scsi_command_latency_microseconds_sum{bucket="read"} 30
holo_scsi_command_latency_microseconds_max{bucket="read"} 20
holo_scsi_commands_total{bucket="write"} 1
holo_scsi_command_latency_microseconds_sum{bucket="write"} 40
holo_scsi_command_latency_microseconds_max{bucket="write"} 40
`)
	writeTimingFile(t, filepath.Join(dir, "pub-b.prom"), `
holo_scsi_commands_total{bucket="read"} 3
holo_scsi_command_latency_microseconds_sum{bucket="read"} 70
holo_scsi_command_latency_microseconds_max{bucket="read"} 50
`)

	out := SCSITimingPrometheusText(dir)
	for _, want := range []string{
		`holo_scsi_commands_total{bucket="read"} 5`,
		`holo_scsi_command_latency_microseconds_sum{bucket="read"} 100`,
		`holo_scsi_command_latency_microseconds_max{bucket="read"} 50`,
		`holo_scsi_commands_total{bucket="write"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in metrics:\n%s", want, out)
		}
	}
}

func TestSCSITimingPrometheusTextMissingDirIsZero(t *testing.T) {
	out := SCSITimingPrometheusText(filepath.Join(t.TempDir(), "missing"))
	if !strings.Contains(out, `holo_scsi_commands_total{bucket="read"} 0`) {
		t.Fatalf("expected zero read count for missing dir, got:\n%s", out)
	}
}

func writeTimingFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write timing file: %v", err)
	}
}
