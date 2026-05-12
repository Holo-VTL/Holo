package api

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type scsiTimingStats struct {
	Commands   uint64
	LatencySum uint64
	LatencyMax uint64
}

func SCSITimingPrometheusText(metricsDir string) string {
	stats := collectSCSITimingStats(metricsDir)
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# HELP holo_scsi_commands_total Total SCSI commands handled by data-plane processes\n")
	fmt.Fprintf(&buf, "# TYPE holo_scsi_commands_total counter\n")
	fmt.Fprintf(&buf, "# HELP holo_scsi_command_latency_microseconds_sum Total SCSI command latency in microseconds\n")
	fmt.Fprintf(&buf, "# TYPE holo_scsi_command_latency_microseconds_sum counter\n")
	fmt.Fprintf(&buf, "# HELP holo_scsi_command_latency_microseconds_max Maximum observed SCSI command latency in microseconds\n")
	fmt.Fprintf(&buf, "# TYPE holo_scsi_command_latency_microseconds_max gauge\n")
	for _, bucket := range []string{"read", "write", "other"} {
		s := stats[bucket]
		fmt.Fprintf(&buf, "holo_scsi_commands_total{bucket=%q} %d\n", bucket, s.Commands)
		fmt.Fprintf(&buf, "holo_scsi_command_latency_microseconds_sum{bucket=%q} %d\n", bucket, s.LatencySum)
		fmt.Fprintf(&buf, "holo_scsi_command_latency_microseconds_max{bucket=%q} %d\n", bucket, s.LatencyMax)
	}
	return buf.String()
}

func collectSCSITimingStats(metricsDir string) map[string]scsiTimingStats {
	out := map[string]scsiTimingStats{
		"read":  {},
		"write": {},
		"other": {},
	}
	metricsDir = strings.TrimSpace(metricsDir)
	if metricsDir == "" {
		return out
	}
	entries, err := os.ReadDir(metricsDir)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".prom") {
			continue
		}
		path := filepath.Join(metricsDir, entry.Name())
		info, statErr := entry.Info()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
			continue
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		mergeSCSITimingPrometheus(out, string(raw))
	}
	return out
}

func mergeSCSITimingPrometheus(out map[string]scsiTimingStats, text string) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		bucket := metricBucket(fields[0])
		if bucket == "" {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		stats := out[bucket]
		switch {
		case strings.HasPrefix(fields[0], "holo_scsi_commands_total"):
			stats.Commands += value
		case strings.HasPrefix(fields[0], "holo_scsi_command_latency_microseconds_sum"):
			stats.LatencySum += value
		case strings.HasPrefix(fields[0], "holo_scsi_command_latency_microseconds_max"):
			if value > stats.LatencyMax {
				stats.LatencyMax = value
			}
		}
		out[bucket] = stats
	}
}

func metricBucket(sample string) string {
	for _, bucket := range []string{"read", "write", "other"} {
		if strings.Contains(sample, `bucket="`+bucket+`"`) {
			return bucket
		}
	}
	return ""
}

func defaultCDBMetricsDir() string {
	runDir := strings.TrimSpace(os.Getenv("HOLO_RUN_DIR"))
	if runDir == "" {
		runDir = "/run/holo"
	}
	return filepath.Join(runDir, "cdb-metrics")
}
