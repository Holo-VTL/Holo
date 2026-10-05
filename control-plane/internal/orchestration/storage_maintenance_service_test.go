package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/storageutil"
)

type testMaintenanceCatalog struct {
	libraries  []*domain.VirtualLibrary
	drives     []*domain.VirtualDrive
	cartridges []*domain.VirtualCartridge
}

func (c testMaintenanceCatalog) ListLibraries(context.Context) []*domain.VirtualLibrary {
	return c.libraries
}
func (c testMaintenanceCatalog) ListDrives(context.Context) []*domain.VirtualDrive { return c.drives }
func (c testMaintenanceCatalog) ListCartridges(context.Context) []*domain.VirtualCartridge {
	return c.cartridges
}

func TestStorageMaintenanceUsesFreshMediaStateAndFailsClosed(t *testing.T) {
	base := t.TempDir()
	mediaDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaDir)
	t.Setenv("HOLO_STORAGE_POOL_ROOT_BASE", base)
	poolRoot := filepath.Join(base, "pool-a")
	if err := os.Mkdir(poolRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	library, err := domain.NewVirtualLibrary("library-a", "Library A")
	if err != nil {
		t.Fatal(err)
	}
	drive, err := domain.NewVirtualDrive("drive-a", "library-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	cartridge := domain.NewVirtualCartridge("cart-a", "pool-a", "library-a", "VTA000L06", 1<<30)
	service := NewStorageMaintenanceService(testMaintenanceCatalog{
		libraries:  []*domain.VirtualLibrary{library},
		drives:     []*domain.VirtualDrive{drive},
		cartridges: []*domain.VirtualCartridge{cartridge},
	}, nil, nil, nil)
	statePath, err := storageutil.MediaStatePath("library-a", "drive-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("cartridge=cart-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidates, err := service.eligibleCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("loaded cartridge appeared as a reclaim candidate: %+v", candidates)
	}
	if err := os.WriteFile(statePath, []byte("cartridge=cart-a\nextra=bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidates, err = service.eligibleCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("malformed media state did not fail closed: %+v", candidates)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	candidates, err = service.eligibleCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].cartridge != "cart-a" {
		t.Fatalf("missing media state should use existing unloaded semantics: %+v", candidates)
	}
}

func TestDecodeMaintenanceResultRejectsUnknownOrInconsistentFields(t *testing.T) {
	valid := []byte(`{"schema_version":2,"pool_id":"pool-a","library_id":"library-a","cartridge_id":"cart-a","status":"completed","processed_segments":1,"has_more":false,"allocated_before_bytes":20,"allocated_after_bytes":10,"net_freed_bytes":10,"duration_ms":2}`)
	if _, err := decodeMaintenanceResult(valid); err != nil {
		t.Fatalf("valid maintenance result rejected: %v", err)
	}
	unknown := append([]byte(nil), valid[:len(valid)-1]...)
	unknown = append(unknown, []byte(`,"unknown":true}`)...)
	if _, err := decodeMaintenanceResult(unknown); err == nil {
		t.Fatal("unknown worker result field should be rejected")
	}
	inconsistent := []byte(`{"schema_version":2,"pool_id":"pool-a","library_id":"library-a","cartridge_id":"cart-a","status":"completed","processed_segments":1,"has_more":false,"allocated_before_bytes":20,"allocated_after_bytes":10,"net_freed_bytes":0,"duration_ms":2}`)
	if _, err := decodeMaintenanceResult(inconsistent); err == nil {
		t.Fatal("inconsistent net-free amount should be rejected")
	}
}

func TestDecodeMaintenanceResultEnforcesV2StatusCursorAndExitContract(t *testing.T) {
	base := `"schema_version":2,"pool_id":"pool-a","library_id":"library-a","cartridge_id":"cart-a"`
	cases := []struct {
		name  string
		json  string
		valid bool
	}{
		{"deferred-budget-remains-pending", `{` + base + `,"status":"deferred","reason":"metadata_budget_exceeded","processed_segments":0,"has_more":true,"allocated_before_bytes":0,"allocated_after_bytes":0,"net_freed_bytes":0,"duration_ms":1}`, true},
		{"failed-must-not-claim-work", `{` + base + `,"status":"failed","reason":"io_error","processed_segments":0,"has_more":true,"allocated_before_bytes":0,"allocated_after_bytes":0,"net_freed_bytes":0,"duration_ms":1}`, true},
		{"cancelled", `{` + base + `,"status":"cancelled","reason":"cancelled","processed_segments":0,"has_more":true,"allocated_before_bytes":0,"allocated_after_bytes":0,"net_freed_bytes":0,"duration_ms":1}`, true},
		{"wrong-version", `{` + strings.Replace(base, `"schema_version":2`, `"schema_version":1`, 1) + `,"status":"completed","processed_segments":0,"has_more":false,"allocated_before_bytes":0,"allocated_after_bytes":0,"net_freed_bytes":0,"duration_ms":1}`, false},
		{"cursor-without-more", `{` + base + `,"status":"completed","processed_segments":0,"has_more":false,"scan_cursor":"{}","allocated_before_bytes":0,"allocated_after_bytes":0,"net_freed_bytes":0,"duration_ms":1}`, false},
		{"non-completed-processed", `{` + base + `,"status":"deferred","reason":"busy","processed_segments":1,"has_more":true,"allocated_before_bytes":0,"allocated_after_bytes":0,"net_freed_bytes":0,"duration_ms":1}`, false},
		{"invalid-reason-combination", `{` + base + `,"status":"failed","reason":"busy","processed_segments":0,"has_more":true,"allocated_before_bytes":0,"allocated_after_bytes":0,"net_freed_bytes":0,"duration_ms":1}`, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeMaintenanceResult([]byte(test.json))
			if (err == nil) != test.valid {
				t.Fatalf("valid=%t, error=%v", test.valid, err)
			}
		})
	}
	oversizedCursor := `{"schema_version":2,"pool_id":"pool-a","library_id":"library-a","cartridge_id":"cart-a","status":"completed","processed_segments":0,"has_more":true,"scan_cursor":"` + strings.Repeat("x", storageMaintenanceCursorLimit+1) + `","allocated_before_bytes":0,"allocated_after_bytes":0,"net_freed_bytes":0,"duration_ms":1}`
	if _, err := decodeMaintenanceResult([]byte(oversizedCursor)); err == nil {
		t.Fatal("worker cursor beyond 4 KiB should fail closed")
	}
	if _, err := decodeMaintenanceResult(bytes.Repeat([]byte("x"), storageMaintenanceOutputLimit+1)); err == nil {
		t.Fatal("worker stdout beyond 64 KiB should fail closed")
	}
	validCursor := maintenanceCursorFixture()
	withCursor := `{"schema_version":2,"pool_id":"pool-a","library_id":"library-a","cartridge_id":"cart-a","status":"completed","processed_segments":0,"has_more":true,"scan_cursor":` + string(mustJSON(t, validCursor)) + `,"allocated_before_bytes":0,"allocated_after_bytes":0,"net_freed_bytes":0,"duration_ms":1}`
	if _, err := decodeMaintenanceResult([]byte(withCursor)); err != nil {
		t.Fatalf("valid bounded cursor rejected: %v", err)
	}
	invalidCursor := strings.Replace(validCursor, strings.Repeat("a", 64), "bad", 1)
	withCursor = `{"schema_version":2,"pool_id":"pool-a","library_id":"library-a","cartridge_id":"cart-a","status":"completed","processed_segments":0,"has_more":true,"scan_cursor":` + string(mustJSON(t, invalidCursor)) + `,"allocated_before_bytes":0,"allocated_after_bytes":0,"net_freed_bytes":0,"duration_ms":1}`
	if _, err := decodeMaintenanceResult([]byte(withCursor)); err == nil {
		t.Fatal("cursor with invalid snapshot digest should fail closed")
	}
}

func maintenanceCursorFixture() string {
	return `{"schema_version":2,"snapshot":"` + strings.Repeat("a", 64) + `","segment_ordinal":0,"in_segment":false,"segment_seq":0,"segment_sequence":0,"segment_checksum":0,"segment_file_len":0,"byte_offset":0,"verified_headers":0,"segment_source_bytes":0,"active_bytes":0,"segment_last_blob_id":0,"best_candidate":null,"eligible_candidate_count":0}`
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestMaintenanceProgressOnlyRefreshesWatchdogOnVerifiedWork(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	monitor := newMaintenanceProgressMonitor(started)
	monitor.accept([]byte(`HOLO_PROGRESS {"schema_version":2,"phase":"scan","verified_bytes":0,"verified_records":0}`))
	if !monitor.lastProgress().Equal(started) {
		t.Fatal("empty progress heartbeat refreshed the watchdog")
	}
	monitor.accept([]byte(`HOLO_PROGRESS {"schema_version":2,"phase":"verify","verified_bytes":1024,"verified_records":1}`))
	if time.Since(monitor.lastProgress()) > time.Second {
		t.Fatal("verified work did not refresh the watchdog")
	}
	monitor.accept([]byte(`HOLO_PROGRESS {"schema_version":2,"phase":"copy","verified_bytes":512,"verified_records":1}`))
	if monitor.failure() == "" {
		t.Fatal("regressing counters should be rejected")
	}
}

func TestMaintenanceStderrProgressLineAndTailStayBounded(t *testing.T) {
	monitor := newMaintenanceProgressMonitor(time.Now())
	tail := &boundedTail{limit: 16}
	line := []byte("HOLO_PROGRESS " + strings.Repeat("x", storageMaintenanceProgressLimit+20) + "\n")
	consumeMaintenanceStderr(strings.NewReader(string(line)+strings.Repeat("z", 64)), monitor, tail)
	if monitor.failure() == "" {
		t.Fatal("oversized progress line should be rejected")
	}
	if tail.Len() != 16 {
		t.Fatalf("stderr diagnostic tail is not bounded: %d", tail.Len())
	}
}

func TestStorageMaintenanceWatchdogStopsWorkerWithoutVerifiedProgress(t *testing.T) {
	worker := filepath.Join(t.TempDir(), "worker.sh")
	if err := os.WriteFile(worker, []byte("#!/bin/sh\nexec sleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	service := NewStorageMaintenanceService(testMaintenanceCatalog{}, nil, nil, nil)
	service.binPath = worker
	service.progressTimeout = 80 * time.Millisecond
	service.cancelGrace = 80 * time.Millisecond
	candidate := storageMaintenanceCandidate{poolID: "pool-a", library: "library-a", cartridge: "cart-a"}
	started := time.Now()
	service.runCandidate(context.Background(), candidate)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("no-progress worker was not stopped promptly: %s", elapsed)
	}
	if state := service.lastStates["pool-a/cart-a"]; state != "failed/no_progress" {
		t.Fatalf("watchdog failure was not recorded: %q", state)
	}
}

func TestStorageMaintenanceAllowsVerifiedProgressBeyondNinetySeconds(t *testing.T) {
	if os.Getenv("HOLO_LONG_RECOVERY_ACCEPTANCE") != "1" {
		t.Skip("set HOLO_LONG_RECOVERY_ACCEPTANCE=1 for the 95-second watchdog acceptance")
	}
	worker := filepath.Join(t.TempDir(), "worker.sh")
	script := `#!/bin/sh
i=0
while [ "$i" -lt 19 ]; do
  i=$((i + 1))
  printf 'HOLO_PROGRESS {"schema_version":2,"phase":"verify","verified_bytes":%s,"verified_records":%s}\n' "$i" "$i" >&2
  sleep 5
done
printf '%s\n' '{"schema_version":2,"pool_id":"pool-a","library_id":"library-a","cartridge_id":"cart-a","status":"completed","processed_segments":1,"has_more":false,"allocated_before_bytes":1,"allocated_after_bytes":0,"net_freed_bytes":1,"duration_ms":95000}'
`
	if err := os.WriteFile(worker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	service := NewStorageMaintenanceService(testMaintenanceCatalog{}, nil, nil, nil)
	service.binPath = worker
	candidate := storageMaintenanceCandidate{poolID: "pool-a", library: "library-a", cartridge: "cart-a"}
	started := time.Now()
	service.runCandidate(context.Background(), candidate)
	if elapsed := time.Since(started); elapsed < 90*time.Second {
		t.Fatalf("verified worker did not run through the long-recovery window: %s", elapsed)
	}
	if state := service.lastStates["pool-a/cart-a"]; state != "completed/" {
		t.Fatalf("worker was not accepted after sustained verified progress: %q", state)
	}
}

func TestStorageMaintenanceUsesIdleIOPriorityWhenIoniceIsAvailable(t *testing.T) {
	binDir := t.TempDir()
	worker := filepath.Join(binDir, "worker.sh")
	if err := os.WriteFile(worker, []byte("#!/bin/sh\nprintf worker-ran\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(binDir, "ionice-args")
	ionice := filepath.Join(binDir, "ionice")
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" \"$2\" > \"$IONICE_ARGS_PATH\"\nshift 2\nexec \"$@\"\n"
	if err := os.WriteFile(ionice, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IONICE_ARGS_PATH", argsPath)

	cmd := storageMaintenanceCommand(worker, ionice, "--test-arg")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("run maintenance worker through ionice shim: %v", err)
	}
	if string(output) != "worker-ran" {
		t.Fatalf("unexpected worker output: %q", output)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "-c3\n--\n" {
		t.Fatalf("maintenance was not started with idle I/O class: %q", args)
	}
}

func TestStorageMaintenanceFallsBackWhenIoniceIsUnavailable(t *testing.T) {
	binDir := t.TempDir()
	worker := filepath.Join(binDir, "worker.sh")
	if err := os.WriteFile(worker, []byte("#!/bin/sh\nprintf fallback-ran\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	cmd := storageMaintenanceCommand(worker, filepath.Join(binDir, "missing-ionice"))
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("run maintenance worker fallback: %v", err)
	}
	if string(output) != "fallback-ran" {
		t.Fatalf("unexpected fallback output: %q", output)
	}
}

func TestStorageMaintenanceShutdownCancelsWorkerWithinGrace(t *testing.T) {
	worker := filepath.Join(t.TempDir(), "worker.sh")
	if err := os.WriteFile(worker, []byte("#!/bin/sh\nexec sleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	service := NewStorageMaintenanceService(testMaintenanceCatalog{}, nil, nil, nil)
	service.binPath = worker
	service.cancelGrace = 80 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	service.runCandidate(ctx, storageMaintenanceCandidate{poolID: "pool-a", library: "library-a", cartridge: "cart-a"})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("shutdown cancellation exceeded grace: %s", elapsed)
	}
	if len(service.lastStates) != 0 {
		t.Fatalf("shutdown cancellation was incorrectly recorded as a worker failure: %+v", service.lastStates)
	}
}

func TestStorageMaintenanceRunsAtMostTwoWorkersAtOnce(t *testing.T) {
	candidates := []storageMaintenanceCandidate{
		{device: 1, poolID: "pool-a", library: "library-a", cartridge: "cart-a"},
		{device: 2, poolID: "pool-b", library: "library-b", cartridge: "cart-b"},
		{device: 3, poolID: "pool-c", library: "library-c", cartridge: "cart-c"},
	}
	var mu sync.Mutex
	active, maximum := 0, 0
	runStorageMaintenanceCandidates(context.Background(), candidates, func(_ context.Context, _ storageMaintenanceCandidate) {
		mu.Lock()
		active++
		if active > maximum {
			maximum = active
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
	})
	if maximum != storageMaintenanceMaxWorkers {
		t.Fatalf("worker ceiling mismatch: got %d, expected %d", maximum, storageMaintenanceMaxWorkers)
	}
}

func TestStorageMaintenanceExitStatusMustMatchTypedResult(t *testing.T) {
	cases := []struct {
		status string
		code   int
		valid  bool
	}{
		{"completed", 0, true},
		{"completed", 1, false},
		{"deferred", 0, true},
		{"failed", 1, true},
		{"failed", 0, false},
		{"cancelled", 1, true},
		{"cancelled", 0, false},
	}
	for _, test := range cases {
		result := &storageMaintenanceResult{Status: test.status}
		if got := maintenanceExitMatches(result, test.code); got != test.valid {
			t.Errorf("status=%s code=%d: got %t, want %t", test.status, test.code, got, test.valid)
		}
	}
}

func TestStorageMaintenancePassesAndPersistsValidatedScanCursor(t *testing.T) {
	worker := filepath.Join(t.TempDir(), "worker.sh")
	candidate := storageMaintenanceCandidate{poolID: "pool-a", library: "library-a", cartridge: "cart-a"}
	key := storageMaintenanceCandidateKey(candidate)
	cursor := maintenanceCursorFixture()
	report := storageMaintenanceResult{
		SchemaVersion: 2,
		PoolID:        candidate.poolID,
		LibraryID:     candidate.library,
		CartridgeID:   candidate.cartridge,
		Status:        "completed",
		HasMore:       true,
		ScanCursor:    cursor,
	}
	reportJSON := string(mustJSON(t, report))
	script := "#!/bin/sh\nfound=0\nwhile [ \"$#\" -gt 0 ]; do\n  if [ \"$1\" = \"--scan-cursor\" ]; then found=1; fi\n  shift\ndone\n[ \"$found\" -eq 1 ] || exit 9\nprintf '%s\\n' 'HOLO_PROGRESS {\"schema_version\":2,\"phase\":\"scan\",\"verified_bytes\":24,\"verified_records\":1}' >&2\nprintf '%s\\n' '" + reportJSON + "'\n"
	if err := os.WriteFile(worker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	service := NewStorageMaintenanceService(testMaintenanceCatalog{}, nil, nil, nil)
	service.binPath = worker
	service.cursors[key] = cursor
	service.runCandidate(context.Background(), candidate)
	if service.cursors[key] != cursor {
		t.Fatal("validated worker cursor was not retained for the next bounded scan")
	}
}

func TestStorageMaintenanceRotatesCandidatesPerFilesystem(t *testing.T) {
	service := &StorageMaintenanceService{lastByFilesystem: make(map[uint64]string)}
	candidates := []storageMaintenanceCandidate{
		{device: 7, poolID: "pool-a", library: "library-a", cartridge: "cart-a"},
		{device: 7, poolID: "pool-a", library: "library-a", cartridge: "cart-b"},
		{device: 9, poolID: "pool-b", library: "library-b", cartridge: "cart-c"},
		{device: 9, poolID: "pool-b", library: "library-b", cartridge: "cart-d"},
	}
	first := service.nextFilesystemCandidates(candidates)
	if len(first) != 2 || first[0].cartridge != "cart-a" || first[1].cartridge != "cart-c" {
		t.Fatalf("unexpected initial filesystem candidates: %+v", first)
	}
	second := service.nextFilesystemCandidates(candidates)
	if len(second) != 2 || second[0].cartridge != "cart-b" || second[1].cartridge != "cart-d" {
		t.Fatalf("filesystem candidates did not rotate: %+v", second)
	}
	third := service.nextFilesystemCandidates(candidates)
	if len(third) != 2 || third[0].cartridge != "cart-a" || third[1].cartridge != "cart-c" {
		t.Fatalf("filesystem rotation did not wrap: %+v", third)
	}
}
