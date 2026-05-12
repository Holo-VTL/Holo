package api

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type storageLayoutSnapshot struct {
	SegmentFiles      int64
	SegmentBytes      int64
	DataSegmentBytes  int64
	DedupSegmentBytes int64
	ReclaimBytes      int64
}

func StorageLayoutPrometheusText(rootBase string) string {
	snapshot := collectStorageLayoutSnapshot(rootBase)
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "# HELP holo_storage_segment_files Number of storage segment files under the pool root base\n")
	fmt.Fprintf(&buf, "# TYPE holo_storage_segment_files gauge\n")
	fmt.Fprintf(&buf, "holo_storage_segment_files %d\n", snapshot.SegmentFiles)

	fmt.Fprintf(&buf, "# HELP holo_storage_segment_bytes Total bytes used by storage segment files under the pool root base\n")
	fmt.Fprintf(&buf, "# TYPE holo_storage_segment_bytes gauge\n")
	fmt.Fprintf(&buf, "holo_storage_segment_bytes %d\n", snapshot.SegmentBytes)

	fmt.Fprintf(&buf, "# HELP holo_storage_data_segment_bytes Total bytes used by tape data segment files under the pool root base\n")
	fmt.Fprintf(&buf, "# TYPE holo_storage_data_segment_bytes gauge\n")
	fmt.Fprintf(&buf, "holo_storage_data_segment_bytes %d\n", snapshot.DataSegmentBytes)

	fmt.Fprintf(&buf, "# HELP holo_storage_dedup_segment_bytes Total bytes used by dedup index segment files under the pool root base\n")
	fmt.Fprintf(&buf, "# TYPE holo_storage_dedup_segment_bytes gauge\n")
	fmt.Fprintf(&buf, "holo_storage_dedup_segment_bytes %d\n", snapshot.DedupSegmentBytes)

	fmt.Fprintf(&buf, "# HELP holo_storage_reclaim_segment_bytes Total bytes used by reclaim segment files under the pool root base\n")
	fmt.Fprintf(&buf, "# TYPE holo_storage_reclaim_segment_bytes gauge\n")
	fmt.Fprintf(&buf, "holo_storage_reclaim_segment_bytes %d\n", snapshot.ReclaimBytes)

	return buf.String()
}

func collectStorageLayoutSnapshot(rootBase string) storageLayoutSnapshot {
	rootBase = strings.TrimSpace(rootBase)
	if rootBase == "" {
		return storageLayoutSnapshot{}
	}
	var snapshot storageLayoutSnapshot
	_ = filepath.WalkDir(rootBase, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, statErr := entry.Info()
		if statErr != nil || !info.Mode().IsRegular() {
			return nil
		}
		name := entry.Name()
		if !isStorageSegmentFile(name) {
			return nil
		}
		size := info.Size()
		snapshot.SegmentFiles++
		snapshot.SegmentBytes += size
		switch {
		case name == "data.segment" || (strings.HasPrefix(name, "data_") && strings.HasSuffix(name, ".seg")):
			snapshot.DataSegmentBytes += size
		case name == "dedup.segment":
			snapshot.DedupSegmentBytes += size
		case name == "reclaim.segment":
			snapshot.ReclaimBytes += size
		}
		return nil
	})
	return snapshot
}

func isStorageSegmentFile(name string) bool {
	if strings.HasSuffix(name, ".segment") {
		return true
	}
	return strings.HasPrefix(name, "data_") && strings.HasSuffix(name, ".seg")
}
