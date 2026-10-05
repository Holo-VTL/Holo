package orchestration

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/metrics"
	"github.com/Holo-VTL/Holo/control-plane/internal/storageutil"
)

const (
	storageMaintenancePollInterval  = 30 * time.Second
	storageMaintenanceOutputLimit   = 64 * 1024
	storageMaintenanceProgressLimit = 1024
	storageMaintenanceLogTailLimit  = 16 * 1024
	storageMaintenanceCursorLimit   = 4 * 1024
	storageMaintenanceMaxWorkers    = 2
)

type storageMaintenanceCatalog interface {
	ListLibraries(ctx context.Context) []*domain.VirtualLibrary
	ListDrives(ctx context.Context) []*domain.VirtualDrive
	ListCartridges(ctx context.Context) []*domain.VirtualCartridge
}

type StorageMaintenanceService struct {
	catalog          storageMaintenanceCatalog
	pools            StoragePoolReader
	auditW           audit.Writer
	metrics          *metrics.MetricsRegistry
	binPath          string
	progressTimeout  time.Duration
	cancelGrace      time.Duration
	mu               sync.Mutex
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	warningMu        sync.Mutex
	warnings         map[string]storageMaintenanceWarning
	lastStates       map[string]string
	selectionMu      sync.Mutex
	lastByFilesystem map[uint64]string
	cursors          map[string]string
}

type storageMaintenanceWarning struct {
	consecutiveFailures int
	lastFailure         time.Time
	eligibleSince       time.Time
	warned              bool
}

type storageMaintenanceResult struct {
	SchemaVersion        int    `json:"schema_version"`
	PoolID               string `json:"pool_id"`
	LibraryID            string `json:"library_id"`
	CartridgeID          string `json:"cartridge_id"`
	Status               string `json:"status"`
	Reason               string `json:"reason,omitempty"`
	ProcessedSegments    uint32 `json:"processed_segments"`
	HasMore              bool   `json:"has_more"`
	ScanCursor           string `json:"scan_cursor,omitempty"`
	AllocatedBeforeBytes uint64 `json:"allocated_before_bytes"`
	AllocatedAfterBytes  uint64 `json:"allocated_after_bytes"`
	NetFreedBytes        uint64 `json:"net_freed_bytes"`
	DurationMS           uint64 `json:"duration_ms"`
}

type storageMaintenanceProgress struct {
	SchemaVersion   int    `json:"schema_version"`
	Phase           string `json:"phase"`
	VerifiedBytes   uint64 `json:"verified_bytes"`
	VerifiedRecords uint64 `json:"verified_records"`
}

type maintenanceScanCursor struct {
	SchemaVersion          int                          `json:"schema_version"`
	Snapshot               string                       `json:"snapshot"`
	SegmentOrdinal         uint64                       `json:"segment_ordinal"`
	InSegment              bool                         `json:"in_segment"`
	SegmentSeq             uint32                       `json:"segment_seq"`
	SegmentSequence        uint64                       `json:"segment_sequence"`
	SegmentChecksum        uint32                       `json:"segment_checksum"`
	SegmentFileLen         uint64                       `json:"segment_file_len"`
	ByteOffset             uint64                       `json:"byte_offset"`
	VerifiedHeaders        uint64                       `json:"verified_headers"`
	SegmentSourceBytes     uint64                       `json:"segment_source_bytes"`
	ActiveBytes            uint64                       `json:"active_bytes"`
	SegmentLastBlobID      uint64                       `json:"segment_last_blob_id"`
	BestCandidate          *maintenanceCandidateSummary `json:"best_candidate"`
	EligibleCandidateCount uint32                       `json:"eligible_candidate_count"`
}

type maintenanceCandidateSummary struct {
	SegmentSeq     uint32 `json:"segment_seq"`
	SourceBytes    uint64 `json:"source_bytes"`
	LiveBytes      uint64 `json:"live_bytes"`
	LastBlobID     uint64 `json:"last_blob_id"`
	SourceSequence uint64 `json:"source_sequence"`
	SourceFileLen  uint64 `json:"source_file_len"`
}

type storageMaintenanceCandidate struct {
	poolRoot  string
	poolID    string
	library   string
	cartridge string
	device    uint64
}

func NewStorageMaintenanceService(catalog storageMaintenanceCatalog, pools StoragePoolReader, auditW audit.Writer, registry *metrics.MetricsRegistry) *StorageMaintenanceService {
	binPath := strings.TrimSpace(os.Getenv("HOLO_STORAGE_MAINTENANCE_BINARY"))
	if binPath == "" {
		binPath = "/opt/holo/bin/holo_storage_maintenance"
	}
	return &StorageMaintenanceService{
		catalog:          catalog,
		pools:            pools,
		auditW:           auditW,
		metrics:          registry,
		binPath:          binPath,
		progressTimeout:  30 * time.Second,
		cancelGrace:      2 * time.Second,
		warnings:         make(map[string]storageMaintenanceWarning),
		lastStates:       make(map[string]string),
		lastByFilesystem: make(map[uint64]string),
		cursors:          make(map[string]string),
	}
}

func (s *StorageMaintenanceService) Start(parent context.Context) {
	if s == nil || s.catalog == nil || storageMaintenanceDisabled() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runCycle(ctx)
		ticker := time.NewTicker(storageMaintenancePollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if storageMaintenanceDisabled() {
					continue
				}
				s.runCycle(ctx)
			}
		}
	}()
}

func (s *StorageMaintenanceService) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *StorageMaintenanceService) runCycle(parent context.Context) {
	candidates, err := s.eligibleCandidates(parent)
	if err != nil {
		log.Printf("[storage-maintenance] inventory skipped: %v", err)
		return
	}
	selected := s.nextFilesystemCandidates(candidates)
	active := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		active[storageMaintenanceCandidateKey(candidate)] = struct{}{}
	}
	s.selectionMu.Lock()
	for key := range s.cursors {
		if _, ok := active[key]; !ok {
			delete(s.cursors, key)
		}
	}
	s.selectionMu.Unlock()
	runStorageMaintenanceCandidates(parent, selected, s.runCandidate)
}

func runStorageMaintenanceCandidates(
	ctx context.Context,
	candidates []storageMaintenanceCandidate,
	run func(context.Context, storageMaintenanceCandidate),
) {
	workers := make(chan struct{}, storageMaintenanceMaxWorkers)
	var wg sync.WaitGroup
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		workers <- struct{}{}
		wg.Add(1)
		go func(candidate storageMaintenanceCandidate) {
			defer wg.Done()
			defer func() { <-workers }()
			run(ctx, candidate)
		}(candidate)
	}
	wg.Wait()
}

func (s *StorageMaintenanceService) nextFilesystemCandidates(candidates []storageMaintenanceCandidate) []storageMaintenanceCandidate {
	groups := make(map[uint64][]storageMaintenanceCandidate)
	for _, candidate := range candidates {
		groups[candidate.device] = append(groups[candidate.device], candidate)
	}
	s.selectionMu.Lock()
	defer s.selectionMu.Unlock()
	for device := range s.lastByFilesystem {
		if _, ok := groups[device]; !ok {
			delete(s.lastByFilesystem, device)
		}
	}
	devices := make([]uint64, 0, len(groups))
	for device := range groups {
		devices = append(devices, device)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i] < devices[j] })
	selected := make([]storageMaintenanceCandidate, 0, len(devices))
	for _, device := range devices {
		group := groups[device]
		sort.Slice(group, func(i, j int) bool {
			if group[i].poolID != group[j].poolID {
				return group[i].poolID < group[j].poolID
			}
			if group[i].library != group[j].library {
				return group[i].library < group[j].library
			}
			return group[i].cartridge < group[j].cartridge
		})
		cursor := s.lastByFilesystem[device]
		choice := group[0]
		for _, candidate := range group {
			if storageMaintenanceCandidateKey(candidate) > cursor {
				choice = candidate
				break
			}
		}
		s.lastByFilesystem[device] = storageMaintenanceCandidateKey(choice)
		selected = append(selected, choice)
	}
	return selected
}

func storageMaintenanceCandidateKey(candidate storageMaintenanceCandidate) string {
	return candidate.poolID + "\x00" + candidate.library + "\x00" + candidate.cartridge
}

func (s *StorageMaintenanceService) eligibleCandidates(ctx context.Context) ([]storageMaintenanceCandidate, error) {
	libraries := s.catalog.ListLibraries(ctx)
	drives := s.catalog.ListDrives(ctx)
	cartridges := s.catalog.ListCartridges(ctx)
	blockedLibraries := make(map[string]struct{})
	loadedCartridges := make(map[string]struct{})
	for _, drive := range drives {
		if drive == nil {
			continue
		}
		loaded, err := storageutil.ReadDriveMediaState(drive.LibraryID, drive.DriveID)
		if err != nil {
			blockedLibraries[drive.LibraryID] = struct{}{}
			log.Printf("[storage-maintenance] library postponed because media state is unreadable library=%s drive=%s", drive.LibraryID, drive.DriveID)
			continue
		}
		if loaded != "" {
			loadedCartridges[loaded] = struct{}{}
		}
	}
	libraryIDs := make(map[string]struct{}, len(libraries))
	for _, library := range libraries {
		if library != nil {
			libraryIDs[library.LibraryID] = struct{}{}
		}
	}
	var pools []*domain.StoragePoolRuntime
	if poolReader, ok := s.pools.(storagePoolIdentityReader); ok {
		pools = poolReader.ListPools(ctx)
	}
	candidates := make([]storageMaintenanceCandidate, 0)
	for _, cartridge := range cartridges {
		if cartridge == nil || cartridge.RetentionState == domain.RetentionLocked {
			continue
		}
		if _, ok := blockedLibraries[cartridge.LibraryID]; ok {
			continue
		}
		if _, ok := loadedCartridges[cartridge.CartridgeID]; ok {
			continue
		}
		if _, ok := libraryIDs[cartridge.LibraryID]; !ok {
			continue
		}
		if err := ValidateResourceIdentity(libraries, drives, cartridges, pools, cartridge.LibraryID, "", cartridge.CartridgeID); err != nil {
			if errors.Is(err, domain.ErrIdentityConflict) {
				candidate := storageMaintenanceCandidate{poolID: cartridge.PoolID, library: cartridge.LibraryID, cartridge: cartridge.CartridgeID}
				result := &storageMaintenanceResult{SchemaVersion: 2, Status: "deferred", Reason: "identity_conflict"}
				log.Printf("[storage-maintenance] cartridge postponed reason=identity_conflict pool=%s cartridge=%s", cartridge.PoolID, cartridge.CartridgeID)
				if s.updateWarningState(candidate, result) {
					s.emitReclaimAudit(candidate, result)
				}
			}
			continue
		}
		if s.pools != nil {
			pool, err := s.pools.GetPool(ctx, cartridge.PoolID)
			if err != nil || pool == nil || len(pool.Disks) == 0 {
				continue
			}
		}
		root := storageutil.PoolStorageRoot(cartridge.PoolID)
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		candidates = append(candidates, storageMaintenanceCandidate{
			poolRoot:  root,
			poolID:    cartridge.PoolID,
			library:   cartridge.LibraryID,
			cartridge: cartridge.CartridgeID,
			device:    uint64(stat.Dev),
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].device != candidates[j].device {
			return candidates[i].device < candidates[j].device
		}
		if candidates[i].poolID != candidates[j].poolID {
			return candidates[i].poolID < candidates[j].poolID
		}
		return candidates[i].cartridge < candidates[j].cartridge
	})
	return candidates, nil
}

func (s *StorageMaintenanceService) runCandidate(parent context.Context, candidate storageMaintenanceCandidate) {
	runtimeDir := strings.TrimSpace(os.Getenv("HOLO_RUN_DIR"))
	if runtimeDir == "" {
		runtimeDir = "/run/holo"
	}
	key := storageMaintenanceCandidateKey(candidate)
	s.selectionMu.Lock()
	cursor := s.cursors[key]
	s.selectionMu.Unlock()
	args := []string{
		"--pool-root", candidate.poolRoot,
		"--pool-id", candidate.poolID,
		"--library-id", candidate.library,
		"--cartridge-id", candidate.cartridge,
		"--runtime-dir", runtimeDir,
	}
	if cursor != "" {
		args = append(args, "--scan-cursor", cursor)
	}
	cmd := storageMaintenanceCommand(s.binPath, "/usr/bin/ionice", args...)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		s.recordWorkerFailure(candidate, "io_error")
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		s.recordWorkerFailure(candidate, "io_error")
		return
	}
	if err := cmd.Start(); err != nil {
		s.recordWorkerFailure(candidate, "io_error")
		return
	}
	stdout := &boundedOutput{limit: storageMaintenanceOutputLimit}
	stderrTail := &boundedTail{limit: storageMaintenanceLogTailLimit}
	monitor := newMaintenanceProgressMonitor(time.Now())
	stdoutDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(stdout, stdoutPipe)
		stdoutDone <- copyErr
	}()
	stderrDone := make(chan struct{})
	go func() {
		consumeMaintenanceStderr(stderrPipe, monitor, stderrTail)
		close(stderrDone)
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	watchdogInterval := 250 * time.Millisecond
	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()
	var waitErr error
	var stopCause string
	waited := false
	for !waited {
		select {
		case waitErr = <-waitDone:
			waited = true
		case <-parent.Done():
			stopCause = "cancelled"
			waitErr = stopMaintenanceWorker(cmd, waitDone, s.cancelGrace)
			waited = true
		case <-ticker.C:
			if monitor.failure() != "" || time.Since(monitor.lastProgress()) >= s.progressTimeout {
				stopCause = "no_progress"
				waitErr = stopMaintenanceWorker(cmd, waitDone, s.cancelGrace)
				waited = true
			}
		}
	}
	stdoutReadErr := <-stdoutDone
	<-stderrDone
	if stopCause == "cancelled" {
		return
	}
	if stopCause == "no_progress" {
		s.recordWorkerFailure(candidate, "no_progress")
		return
	}
	if stdout.overflow || stdoutReadErr != nil || monitor.failure() != "" {
		s.recordWorkerFailure(candidate, "io_error")
		return
	}
	result, decodeErr := decodeMaintenanceResult(stdout.Bytes())
	if decodeErr != nil || result.PoolID != candidate.poolID || result.LibraryID != candidate.library || result.CartridgeID != candidate.cartridge {
		s.recordWorkerFailure(candidate, "io_error")
		return
	}
	if !maintenanceExitMatches(result, workerExitCode(waitErr)) {
		s.recordWorkerFailure(candidate, "io_error")
		return
	}
	s.selectionMu.Lock()
	if result.Status == "completed" {
		if result.HasMore && result.ScanCursor != "" {
			s.cursors[key] = result.ScanCursor
		} else {
			delete(s.cursors, key)
		}
	}
	s.selectionMu.Unlock()
	if result.Status == "failed" {
		log.Printf("[storage-maintenance] worker failed pool=%s cartridge=%s reason=%s", candidate.poolID, candidate.cartridge, result.Reason)
	}
	if s.metrics != nil {
		s.metrics.RecordStorageReclaim(result.Status, result.NetFreedBytes, result.DurationMS)
	}
	if s.updateWarningState(candidate, result) {
		s.emitReclaimAudit(candidate, result)
	}
}

func storageMaintenanceCommand(binaryPath, ionicePath string, args ...string) *exec.Cmd {
	if filepath.IsAbs(ionicePath) {
		if info, err := os.Stat(ionicePath); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			workerArgs := make([]string, 0, len(args)+3)
			workerArgs = append(workerArgs, "-c3", "--", binaryPath)
			workerArgs = append(workerArgs, args...)
			return exec.Command(ionicePath, workerArgs...)
		}
	}
	return exec.Command(binaryPath, args...)
}

func (s *StorageMaintenanceService) recordWorkerFailure(candidate storageMaintenanceCandidate, reason string) {
	result := &storageMaintenanceResult{
		SchemaVersion: 2,
		PoolID:        candidate.poolID,
		LibraryID:     candidate.library,
		CartridgeID:   candidate.cartridge,
		Status:        "failed",
		Reason:        reason,
		HasMore:       true,
	}
	log.Printf("[storage-maintenance] worker failed pool=%s cartridge=%s reason=%s", candidate.poolID, candidate.cartridge, reason)
	if s.metrics != nil {
		s.metrics.RecordStorageReclaim(result.Status, 0, 0)
	}
	if s.updateWarningState(candidate, result) {
		s.emitReclaimAudit(candidate, result)
	}
}

func workerExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func maintenanceExitMatches(result *storageMaintenanceResult, exitCode int) bool {
	switch result.Status {
	case "completed", "deferred":
		return exitCode == 0
	case "failed", "cancelled":
		return exitCode == 1
	default:
		return false
	}
}

func stopMaintenanceWorker(cmd *exec.Cmd, waitDone <-chan error, grace time.Duration) error {
	if cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-waitDone:
		return err
	case <-timer.C:
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return <-waitDone
	}
}

type maintenanceProgressMonitor struct {
	mu              sync.Mutex
	lastProgressAt  time.Time
	verifiedBytes   uint64
	verifiedRecords uint64
	protocolError   string
}

func newMaintenanceProgressMonitor(started time.Time) *maintenanceProgressMonitor {
	return &maintenanceProgressMonitor{lastProgressAt: started}
}

func (m *maintenanceProgressMonitor) accept(line []byte) {
	const prefix = "HOLO_PROGRESS "
	line = bytes.TrimSuffix(line, []byte("\r"))
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return
	}
	if len(line) > storageMaintenanceProgressLimit {
		m.fail("progress line exceeded limit")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(line[len(prefix):]))
	decoder.DisallowUnknownFields()
	var progress storageMaintenanceProgress
	if err := decoder.Decode(&progress); err != nil {
		m.fail("invalid progress JSON")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || progress.SchemaVersion != 2 || !oneOf(progress.Phase, "scan", "verify", "copy", "commit") {
		m.fail("invalid progress contract")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if progress.VerifiedBytes < m.verifiedBytes || progress.VerifiedRecords < m.verifiedRecords {
		m.protocolError = "progress counters regressed"
		return
	}
	if progress.VerifiedBytes > m.verifiedBytes || progress.VerifiedRecords > m.verifiedRecords {
		m.verifiedBytes = progress.VerifiedBytes
		m.verifiedRecords = progress.VerifiedRecords
		m.lastProgressAt = time.Now()
	}
}

func (m *maintenanceProgressMonitor) fail(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.protocolError == "" {
		m.protocolError = reason
	}
}

func (m *maintenanceProgressMonitor) failure() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.protocolError
}

func (m *maintenanceProgressMonitor) lastProgress() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastProgressAt
}

type boundedTail struct {
	bytes.Buffer
	limit int
}

func (b *boundedTail) Write(p []byte) (int, error) {
	_, _ = b.Buffer.Write(p)
	if b.Len() > b.limit {
		remaining := append([]byte(nil), b.Bytes()[b.Len()-b.limit:]...)
		b.Reset()
		_, _ = b.Buffer.Write(remaining)
	}
	return len(p), nil
}

func consumeMaintenanceStderr(reader io.Reader, monitor *maintenanceProgressMonitor, tail *boundedTail) {
	buffer := make([]byte, 4096)
	line := make([]byte, 0, storageMaintenanceProgressLimit)
	dropping := false
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			_, _ = tail.Write(buffer[:count])
			for _, value := range buffer[:count] {
				if value == '\n' {
					if !dropping {
						monitor.accept(line)
					}
					line = line[:0]
					dropping = false
					continue
				}
				if dropping {
					continue
				}
				if len(line) == storageMaintenanceProgressLimit {
					if bytes.HasPrefix(line, []byte("HOLO_PROGRESS ")) {
						monitor.fail("progress line exceeded limit")
					}
					dropping = true
					continue
				}
				line = append(line, value)
			}
		}
		if err != nil {
			if err == io.EOF && len(line) > 0 && !dropping {
				monitor.accept(line)
			}
			return
		}
	}
}

func (s *StorageMaintenanceService) updateWarningState(candidate storageMaintenanceCandidate, result *storageMaintenanceResult) bool {
	key := candidate.poolID + "/" + candidate.cartridge
	s.warningMu.Lock()
	defer s.warningMu.Unlock()
	state := s.warnings[key]
	now := time.Now().UTC()
	switch {
	case result.Status == "failed":
		if state.lastFailure.IsZero() || now.Sub(state.lastFailure) > 5*time.Minute {
			state.consecutiveFailures = 0
		}
		state.consecutiveFailures++
		state.lastFailure = now
		if state.eligibleSince.IsZero() {
			state.eligibleSince = now
		}
	case result.Status == "deferred" && result.Reason == "busy":
		// Busy work is retried without counting as an error.
	case result.Status == "deferred" && result.Reason != "retention_locked" && result.Reason != "pool_unavailable":
		if state.eligibleSince.IsZero() {
			state.eligibleSince = now
		}
	case result.Status == "completed":
		state = storageMaintenanceWarning{}
	default:
		state = storageMaintenanceWarning{}
	}
	if !state.warned && (state.consecutiveFailures >= 3 || (!state.eligibleSince.IsZero() && now.Sub(state.eligibleSince) >= 5*time.Minute)) {
		log.Printf("[storage-maintenance][warning] reclaim repeatedly failed or remained eligible pool=%s cartridge=%s failures=%d", candidate.poolID, candidate.cartridge, state.consecutiveFailures)
		state.warned = true
	}
	s.warnings[key] = state
	statusKey := result.Status + "/" + result.Reason
	previous := s.lastStates[key]
	s.lastStates[key] = statusKey
	return (statusKey != previous && !(result.Status == "completed" && result.ProcessedSegments == 0)) ||
		(result.Status == "completed" && result.ProcessedSegments > 0)
}

func decodeMaintenanceResult(payload []byte) (*storageMaintenanceResult, error) {
	if len(payload) == 0 || len(payload) > storageMaintenanceOutputLimit {
		return nil, errors.New("invalid worker output length")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var result storageMaintenanceResult
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("unexpected trailing worker output")
	}
	if result.SchemaVersion != 2 || result.ProcessedSegments > 1 || len(result.ScanCursor) > storageMaintenanceCursorLimit {
		return nil, errors.New("worker output version or segment count is invalid")
	}
	switch result.Status {
	case "completed":
		if result.Reason != "" {
			return nil, errors.New("completed worker result has an invalid reason")
		}
		if result.ScanCursor != "" && !result.HasMore {
			return nil, errors.New("worker cursor exists without remaining work")
		}
		if result.ScanCursor != "" {
			if err := validateMaintenanceCursor(result.ScanCursor); err != nil {
				return nil, err
			}
		}
	case "deferred":
		if !oneOf(result.Reason, "busy", "pool_unavailable", "identity_conflict", "ambiguous_layout", "metadata_budget_exceeded", "space_unavailable") {
			return nil, errors.New("deferred worker result has an invalid reason")
		}
	case "failed":
		if !oneOf(result.Reason, "integrity_error", "io_error", "no_progress") {
			return nil, errors.New("failed worker result has an invalid reason")
		}
	case "cancelled":
		if result.Reason != "cancelled" {
			return nil, errors.New("cancelled worker result has an invalid reason")
		}
	default:
		return nil, errors.New("worker result has an unknown status")
	}
	if result.Status != "completed" && (result.ProcessedSegments != 0 || result.NetFreedBytes != 0 || result.ScanCursor != "") {
		return nil, errors.New("non-completed worker result reports committed work")
	}
	if result.AllocatedBeforeBytes >= result.AllocatedAfterBytes {
		if result.NetFreedBytes != result.AllocatedBeforeBytes-result.AllocatedAfterBytes {
			return nil, errors.New("worker net-free amount is inconsistent")
		}
	} else if result.NetFreedBytes != 0 {
		return nil, errors.New("worker net-free amount is inconsistent")
	}
	return &result, nil
}

func validateMaintenanceCursor(raw string) error {
	if len(raw) == 0 || len(raw) > storageMaintenanceCursorLimit {
		return errors.New("worker cursor length is invalid")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var cursor maintenanceScanCursor
	if err := decoder.Decode(&cursor); err != nil {
		return errors.New("worker cursor is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("worker cursor has trailing data")
	}
	snapshot, err := hex.DecodeString(cursor.Snapshot)
	if err != nil || cursor.SchemaVersion != 2 || len(snapshot) != 32 {
		return errors.New("worker cursor snapshot or version is invalid")
	}
	if cursor.InSegment {
		if cursor.SegmentFileLen == 0 || cursor.ByteOffset == 0 || cursor.ActiveBytes > cursor.SegmentSourceBytes {
			return errors.New("worker partial-segment cursor is invalid")
		}
	}
	if cursor.BestCandidate != nil && cursor.BestCandidate.LiveBytes > cursor.BestCandidate.SourceBytes {
		return errors.New("worker candidate summary is invalid")
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func (s *StorageMaintenanceService) emitReclaimAudit(candidate storageMaintenanceCandidate, result *storageMaintenanceResult) {
	if s.auditW == nil {
		return
	}
	event := audit.Event{
		EventID:    "storage-reclaim-" + candidate.cartridge + "-" + time.Now().UTC().Format("20060102150405.000000000"),
		Actor:      "system",
		Action:     "storage_reclaim_" + result.Status,
		ObjectType: "cartridge",
		ObjectID:   candidate.cartridge,
		Result:     result.Status,
		Details: map[string]any{
			"poolId":            candidate.poolID,
			"processedSegments": result.ProcessedSegments,
			"netFreedBytes":     result.NetFreedBytes,
			"durationMs":        result.DurationMS,
			"reason":            result.Reason,
		},
		OccurredAt: time.Now().UTC(),
	}
	if err := s.auditW.Write(context.Background(), event); err != nil {
		log.Printf("[storage-maintenance] audit write failed cartridge=%s", candidate.cartridge)
	}
}

type boundedOutput struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		b.overflow = true
		remaining := b.limit - b.Len()
		if remaining > 0 {
			_, _ = b.Buffer.Write(p[:remaining])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

func storageMaintenanceDisabled() bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv("HOLO_SPACE_RECLAIM_ENABLED")))
	return raw == "0" || raw == "false" || raw == "off" || raw == "no"
}
