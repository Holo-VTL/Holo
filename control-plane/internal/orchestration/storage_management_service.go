package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/storageutil"
)

type storageCommandRunner interface {
	Run(ctx context.Context, command string, args ...string) (string, error)
}

type osStorageCommandRunner struct{}

func (r *osStorageCommandRunner) Run(ctx context.Context, command string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		trimmed := strings.TrimSpace(strings.Join([]string{string(out), stderr.String()}, "\n"))
		if trimmed == "" {
			return "", err
		}
		return "", fmt.Errorf("%w: %s", err, trimmed)
	}
	return string(out), nil
}

type CreateStoragePoolRequest struct {
	PoolID              string
	Name                string
	WarningThresholdPct int
	Actor               string
}

type StorageManagementService struct {
	repo   storagePoolRepo
	auditW audit.Writer
	runner storageCommandRunner
	nowFn  func() time.Time
}

type storagePoolRepo interface {
	CreatePool(ctx context.Context, pool *domain.StoragePoolRuntime) error
	SavePool(ctx context.Context, pool *domain.StoragePoolRuntime) error
	FindPool(ctx context.Context, poolID string) (*domain.StoragePoolRuntime, error)
	ListPools(ctx context.Context) []*domain.StoragePoolRuntime
	DeletePool(ctx context.Context, poolID string) error
	DiskOwner(ctx context.Context, devicePath string) (string, bool)
	AttachDisk(ctx context.Context, poolID string, disk domain.StoragePoolDisk) (*domain.StoragePoolRuntime, error)
	DetachDisk(ctx context.Context, poolID, devicePath string) (*domain.StoragePoolRuntime, error)
	ReserveWrite(ctx context.Context, poolID string, bytes int64) (*domain.StoragePoolRuntime, bool, error)
	RollbackReservedWrite(ctx context.Context, poolID string, bytes int64) (*domain.StoragePoolRuntime, error)
	SetUsedBytes(ctx context.Context, poolID string, usedBytes int64) (*domain.StoragePoolRuntime, error)
	RecordDiscovery(ctx context.Context, disks []domain.StorageManagedDisk)
	LatestDiscovery(ctx context.Context) (domain.DiskDiscoveryRecord, bool)
}

const storagePoolFilesystem = "xfs"

type lsblkNode struct {
	Name       string      `json:"name"`
	Path       string      `json:"path"`
	Type       string      `json:"type"`
	Size       any         `json:"size"`
	Mountpoint any         `json:"mountpoint"`
	Fstype     string      `json:"fstype"`
	Model      string      `json:"model"`
	Serial     string      `json:"serial"`
	Vendor     string      `json:"vendor"`
	Children   []lsblkNode `json:"children"`
}

func NewStorageManagementService(repo storagePoolRepo, auditW audit.Writer, runner storageCommandRunner) *StorageManagementService {
	if runner == nil {
		runner = &osStorageCommandRunner{}
	}
	return &StorageManagementService{
		repo:   repo,
		auditW: auditW,
		runner: runner,
		nowFn: func() time.Time {
			return time.Now().UTC()
		},
	}
}

func (s *StorageManagementService) DiscoverDisks(ctx context.Context) ([]domain.StorageManagedDisk, error) {
	payload, err := s.runner.Run(
		ctx,
		"lsblk",
		"-J",
		"-b",
		"-o",
		"NAME,TYPE,SIZE,MOUNTPOINT,FSTYPE,MODEL,SERIAL,VENDOR",
	)
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Blockdevices []lsblkNode `json:"blockdevices"`
	}
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		return nil, fmt.Errorf("parse lsblk output: %w", err)
	}

	disks := make([]domain.StorageManagedDisk, 0, len(parsed.Blockdevices))
	for _, node := range parsed.Blockdevices {
		if strings.ToLower(strings.TrimSpace(node.Type)) != "disk" {
			continue
		}
		path := strings.TrimSpace(node.Path)
		if path == "" {
			path = storageutil.NormalizeDevicePath(node.Name)
		}
		sizeBytes := parseSizeBytes(node.Size)
		record := domain.StorageManagedDisk{
			DevicePath:   path,
			SizeBytes:    sizeBytes,
			Vendor:       strings.TrimSpace(node.Vendor),
			Model:        strings.TrimSpace(node.Model),
			Serial:       strings.TrimSpace(node.Serial),
			Availability: domain.DiskAvailable,
		}

		if owner, ok := s.repo.DiskOwner(ctx, record.DevicePath); ok {
			record.Availability = domain.DiskUnavailable
			record.UnavailableReason = "already attached to pool " + owner
			record.PoolID = owner
		} else if sizeBytes <= 0 {
			record.Availability = domain.DiskUnavailable
			record.UnavailableReason = "invalid disk capacity"
		} else if hasMountedFilesystem(node) {
			record.Availability = domain.DiskUnavailable
			record.UnavailableReason = "mounted filesystem present"
		} else if hasPartitions(node) {
			record.Availability = domain.DiskUnavailable
			record.UnavailableReason = "existing partitions detected"
		} else if hasUnsupportedFilesystemSignature(node) {
			record.Availability = domain.DiskUnavailable
			record.UnavailableReason = "unsupported filesystem signature detected: " + strings.TrimSpace(node.Fstype)
		}

		if shouldHideDiscoveredDisk(record) {
			continue
		}

		disks = append(disks, record)
	}

	sort.Slice(disks, func(i, j int) bool {
		return disks[i].DevicePath < disks[j].DevicePath
	})
	s.repo.RecordDiscovery(ctx, disks)
	return disks, nil
}

func (s *StorageManagementService) CreatePool(ctx context.Context, req CreateStoragePoolRequest) (*domain.StoragePoolRuntime, error) {
	pool, err := domain.NewStoragePoolRuntime(req.PoolID, req.Name, req.WarningThresholdPct)
	if err != nil {
		return nil, err
	}
	if err := s.repo.CreatePool(ctx, pool); err != nil {
		return nil, err
	}
	s.emitStorageAudit(ctx, safeActor(req.Actor), "storage_pool_create", pool.PoolID, "success", map[string]any{
		"warningThresholdPct": pool.WarningThresholdPct,
	})
	return pool, nil
}

func (s *StorageManagementService) ListPools(ctx context.Context) []*domain.StoragePoolRuntime {
	pools := s.repo.ListPools(ctx)
	for _, pool := range pools {
		s.applyMountedCapacitySnapshot(ctx, pool)
	}
	return pools
}

func (s *StorageManagementService) GetPool(ctx context.Context, poolID string) (*domain.StoragePoolRuntime, error) {
	pool, err := s.repo.FindPool(ctx, poolID)
	if err != nil {
		return nil, err
	}
	s.applyMountedCapacitySnapshot(ctx, pool)
	return pool, nil
}

func (s *StorageManagementService) EnsureAttachedPoolsMounted(ctx context.Context) error {
	if !storageutil.StrictStorageFlowEnabled() {
		return nil
	}
	for _, pool := range s.repo.ListPools(ctx) {
		if err := s.ensureAttachedPoolMounted(ctx, pool); err != nil {
			return err
		}
	}
	return nil
}

func (s *StorageManagementService) DeletePool(ctx context.Context, poolID, actor string) error {
	pool, err := s.repo.FindPool(ctx, poolID)
	if err != nil {
		return err
	}
	if pool.Capacity.UsedBytes > 0 {
		return domain.ErrInvalidState
	}
	if storageutil.StrictStorageFlowEnabled() && len(pool.Disks) > 0 {
		if err := s.unmountPoolRoot(ctx, pool.PoolID, storageutil.NormalizeDevicePath(pool.Disks[0].DevicePath)); err != nil {
			return err
		}
	}
	if err := s.repo.DeletePool(ctx, poolID); err != nil {
		return err
	}
	s.emitStorageAudit(ctx, safeActor(actor), "storage_pool_delete", strings.TrimSpace(poolID), "success", map[string]any{
		"detachedDisks": len(pool.Disks),
	})
	return nil
}

func (s *StorageManagementService) AttachDisk(ctx context.Context, poolID, devicePath, actor string) (*domain.StoragePoolRuntime, error) {
	devicePath = storageutil.NormalizeDevicePath(devicePath)
	if devicePath == "" || !storageutil.IsSafeDevicePath(devicePath) {
		return nil, domain.ErrInvalidInput
	}
	pool, err := s.repo.FindPool(ctx, poolID)
	if err != nil {
		return nil, err
	}
	available, reason, sizeBytes, err := s.ensureDiskAttachable(ctx, devicePath)
	if err != nil {
		return nil, err
	}
	if !available {
		return nil, fmt.Errorf("%w: %s", domain.ErrInvalidState, reason)
	}
	needsMount := len(pool.Disks) == 0
	if storageutil.StrictStorageFlowEnabled() && !needsMount {
		return nil, fmt.Errorf("%w: strict storage flow supports one mounted disk per pool", domain.ErrInvalidState)
	}
	if storageutil.StrictStorageFlowEnabled() && needsMount {
		if err := s.mountPoolRootToDisk(ctx, pool.PoolID, devicePath); err != nil {
			_ = s.unmountPoolRoot(ctx, pool.PoolID, devicePath)
			return nil, err
		}
	}
	updated, err := s.repo.AttachDisk(ctx, poolID, domain.StoragePoolDisk{
		DevicePath: devicePath,
		SizeBytes:  sizeBytes,
		AttachedAt: s.nowFn(),
	})
	if err != nil {
		if storageutil.StrictStorageFlowEnabled() && needsMount {
			_ = s.unmountPoolRoot(ctx, pool.PoolID, devicePath)
		}
		return nil, err
	}
	s.emitStorageAudit(ctx, safeActor(actor), "storage_disk_attach", strings.TrimSpace(poolID), "success", map[string]any{
		"devicePath": devicePath,
		"sizeBytes":  sizeBytes,
	})
	return updated, nil
}

func (s *StorageManagementService) DetachDisk(ctx context.Context, poolID, devicePath, actor string) (*domain.StoragePoolRuntime, error) {
	devicePath = storageutil.NormalizeDevicePath(devicePath)
	if devicePath == "" || !storageutil.IsSafeDevicePath(devicePath) {
		return nil, domain.ErrInvalidInput
	}
	pool, err := s.repo.FindPool(ctx, poolID)
	if err != nil {
		return nil, err
	}
	if pool.Capacity.UsedBytes > 0 && len(pool.Disks) <= 1 {
		return nil, domain.ErrInvalidState
	}
	if storageutil.StrictStorageFlowEnabled() && len(pool.Disks) == 1 && storageutil.NormalizeDevicePath(pool.Disks[0].DevicePath) == devicePath {
		if err := s.unmountPoolRoot(ctx, poolID, devicePath); err != nil {
			return nil, err
		}
	}
	updated, err := s.repo.DetachDisk(ctx, poolID, devicePath)
	if err != nil {
		return nil, err
	}
	s.emitStorageAudit(ctx, safeActor(actor), "storage_disk_detach", strings.TrimSpace(poolID), "success", map[string]any{
		"devicePath": devicePath,
	})
	return updated, nil
}

func (s *StorageManagementService) GetCapacity(ctx context.Context, poolID string) (*domain.StoragePoolCapacitySnapshot, error) {
	pool, err := s.repo.FindPool(ctx, poolID)
	if err != nil {
		return nil, err
	}
	s.applyMountedCapacitySnapshot(ctx, pool)
	snapshot := pool.Capacity
	return &snapshot, nil
}

func (s *StorageManagementService) applyMountedCapacitySnapshot(ctx context.Context, pool *domain.StoragePoolRuntime) {
	if pool == nil || len(pool.Disks) == 0 || !storageutil.StrictStorageFlowEnabled() {
		return
	}
	if err := s.ensureAttachedPoolMounted(ctx, pool); err != nil {
		return
	}
	snapshot, ok := s.mountedCapacitySnapshot(ctx, pool)
	if ok {
		pool.Capacity = snapshot
	}
}

func (s *StorageManagementService) mountedCapacitySnapshot(ctx context.Context, pool *domain.StoragePoolRuntime) (domain.StoragePoolCapacitySnapshot, bool) {
	poolRoot := filepath.Clean(storageutil.PoolStorageRoot(pool.PoolID))
	source, err := s.findMountSourceByTarget(ctx, poolRoot)
	if err != nil || strings.TrimSpace(source) == "" {
		return domain.StoragePoolCapacitySnapshot{}, false
	}

	out, err := s.runner.Run(ctx, "df", "-B1", "--output=size,used,avail", poolRoot)
	if err != nil {
		return domain.StoragePoolCapacitySnapshot{}, false
	}
	total, used, free, ok := parseDFCapacity(out)
	if !ok {
		return domain.StoragePoolCapacitySnapshot{}, false
	}

	usedPercent := 0
	if total > 0 {
		usedPercent = int((used * 100) / total)
	}
	return domain.StoragePoolCapacitySnapshot{
		TotalBytes:          total,
		UsedBytes:           used,
		FreeBytes:           free,
		UsedPercent:         usedPercent,
		WarningThresholdPct: pool.WarningThresholdPct,
		Warning:             total > 0 && usedPercent >= pool.WarningThresholdPct,
		Exhausted:           total > 0 && free <= 0,
	}, true
}

func (s *StorageManagementService) ReserveWrite(ctx context.Context, poolID string, bytes int64) (*domain.StoragePoolCapacitySnapshot, bool, error) {
	if storageutil.StrictStorageFlowEnabled() {
		pool, err := s.repo.FindPool(ctx, poolID)
		if err != nil {
			return nil, false, err
		}
		if err := s.ensureAttachedPoolMounted(ctx, pool); err != nil {
			return nil, false, err
		}
		if snapshot, ok := s.mountedCapacitySnapshot(ctx, pool); ok {
			if bytes <= 0 {
				return nil, false, domain.ErrInvalidInput
			}
			if snapshot.TotalBytes <= 0 || snapshot.FreeBytes < bytes {
				s.emitStorageAudit(ctx, "system", "storage_write_rejected", strings.TrimSpace(poolID), "failure", map[string]any{
					"poolId":        strings.TrimSpace(poolID),
					"bytes":         bytes,
					"mountedFree":   snapshot.FreeBytes,
					"mountedTotal":  snapshot.TotalBytes,
					"mountedSource": "df",
				})
				return nil, false, domain.ErrCapacityExceeded
			}
		}
	}

	pool, warningTriggered, err := s.repo.ReserveWrite(ctx, poolID, bytes)
	if err != nil {
		details := map[string]any{"poolId": strings.TrimSpace(poolID), "bytes": bytes}
		if err == domain.ErrCapacityExceeded {
			s.emitStorageAudit(ctx, "system", "storage_write_rejected", strings.TrimSpace(poolID), "failure", details)
		}
		return nil, false, err
	}
	if warningTriggered {
		s.emitStorageAudit(ctx, "system", "storage_capacity_warning", pool.PoolID, "success", map[string]any{
			"usedPercent": pool.Capacity.UsedPercent,
			"usedBytes":   pool.Capacity.UsedBytes,
			"totalBytes":  pool.Capacity.TotalBytes,
		})
	}
	snapshot := pool.Capacity
	return &snapshot, warningTriggered, nil
}

func (s *StorageManagementService) RollbackReservedWrite(ctx context.Context, poolID string, bytes int64) error {
	_, err := s.repo.RollbackReservedWrite(ctx, poolID, bytes)
	if err != nil {
		return err
	}
	s.emitStorageAudit(ctx, "system", "storage_write_reservation_rollback", strings.TrimSpace(poolID), "success", map[string]any{
		"bytes": bytes,
	})
	return nil
}

func (s *StorageManagementService) ReconcilePoolUsedBytes(ctx context.Context, poolID string, usedBytes int64) error {
	_, err := s.repo.SetUsedBytes(ctx, poolID, usedBytes)
	return err
}

func (s *StorageManagementService) ensureDiskAttachable(ctx context.Context, devicePath string) (bool, string, int64, error) {
	disks, err := s.DiscoverDisks(ctx)
	if err != nil {
		return false, "", 0, err
	}
	for _, disk := range disks {
		if disk.DevicePath != devicePath {
			continue
		}
		if disk.Availability == domain.DiskAvailable {
			return true, "", disk.SizeBytes, nil
		}
		return false, disk.UnavailableReason, disk.SizeBytes, nil
	}
	return false, "disk not found", 0, domain.ErrNotFound
}

func (s *StorageManagementService) emitStorageAudit(ctx context.Context, actor, action, objectID, result string, details map[string]any) {
	if s.auditW == nil {
		return
	}
	evt := audit.Event{
		EventID:    audit.NewEventID(action, objectID),
		Actor:      safeActor(actor),
		Action:     action,
		ObjectType: "storage_pool",
		ObjectID:   strings.TrimSpace(objectID),
		Result:     result,
		Details:    details,
		OccurredAt: s.nowFn(),
	}
	if err := s.auditW.Write(ctx, evt); err != nil {
		log.Printf("AUDIT WRITE FAILURE: %v (event: %s/%s)", err, evt.Action, evt.ObjectID)
	}
}

func parseSizeBytes(raw any) int64 {
	switch v := raw.(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			log.Printf("invalid storage size value; using 0")
			return 0
		}
		return n
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			log.Printf("invalid storage size value; using 0")
			return 0
		}
		return n
	default:
		return 0
	}
}

func parseDFCapacity(output string) (int64, int64, int64, bool) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		fields := strings.Fields(lines[i])
		if len(fields) < 3 {
			continue
		}
		total, totalErr := strconv.ParseInt(fields[0], 10, 64)
		used, usedErr := strconv.ParseInt(fields[1], 10, 64)
		free, freeErr := strconv.ParseInt(fields[2], 10, 64)
		if totalErr == nil && usedErr == nil && freeErr == nil && total >= 0 && used >= 0 && free >= 0 {
			return total, used, free, true
		}
	}
	return 0, 0, 0, false
}

func shouldHideDiscoveredDisk(disk domain.StorageManagedDisk) bool {
	if disk.Availability != domain.DiskUnavailable {
		return false
	}
	switch strings.TrimSpace(disk.UnavailableReason) {
	case "mounted filesystem present", "existing partitions detected", "invalid disk capacity":
		return true
	default:
		return false
	}
}

func (s *StorageManagementService) ensureAttachedPoolMounted(ctx context.Context, pool *domain.StoragePoolRuntime) error {
	if pool == nil || len(pool.Disks) == 0 || !storageutil.StrictStorageFlowEnabled() {
		return nil
	}
	if len(pool.Disks) > 1 {
		return fmt.Errorf("%w: strict storage flow supports one mounted disk per pool", domain.ErrInvalidState)
	}
	devicePath := storageutil.NormalizeDevicePath(pool.Disks[0].DevicePath)
	if devicePath == "" {
		return domain.ErrInvalidInput
	}
	return s.mountPoolRootToDisk(ctx, pool.PoolID, devicePath)
}

func (s *StorageManagementService) mountPoolRootToDisk(ctx context.Context, poolID, devicePath string) error {
	poolRoot := filepath.Clean(storageutil.PoolStorageRoot(poolID))
	if _, err := s.runPrivileged(ctx, "mkdir", "-p", poolRoot); err != nil {
		return fmt.Errorf("prepare pool root %s: %w", poolRoot, err)
	}

	mountedTarget, err := s.findMountTargetBySource(ctx, devicePath)
	if err != nil {
		return err
	}
	if mountedTarget != "" && filepath.Clean(mountedTarget) != poolRoot {
		return fmt.Errorf("%w: disk already mounted at %s", domain.ErrInvalidState, mountedTarget)
	}

	currentSource, err := s.findMountSourceByTarget(ctx, poolRoot)
	if err != nil {
		return err
	}
	if currentSource != "" {
		if currentSource == devicePath {
			return s.ensureWritableOwnership(ctx, poolRoot)
		}
		return fmt.Errorf("%w: pool root already mounted by %s", domain.ErrInvalidState, currentSource)
	}

	fsType, err := s.detectFilesystemType(ctx, devicePath)
	if err != nil {
		return err
	}
	if fsType == "" {
		if _, err := s.runPrivileged(ctx, "mkfs.xfs", "-f", devicePath); err != nil {
			return fmt.Errorf("format disk %s: %w", devicePath, err)
		}
	} else if !strings.EqualFold(fsType, storagePoolFilesystem) {
		return fmt.Errorf("%w: storage pool disk must use %s, found %s", domain.ErrInvalidState, storagePoolFilesystem, fsType)
	}
	if _, err := s.runPrivileged(ctx, "mount", "-o", "noatime,nodiratime", devicePath, poolRoot); err != nil {
		return fmt.Errorf("mount disk %s on %s: %w", devicePath, poolRoot, err)
	}
	return s.ensureWritableOwnership(ctx, poolRoot)
}

func (s *StorageManagementService) unmountPoolRoot(ctx context.Context, poolID, expectedDevice string) error {
	poolRoot := filepath.Clean(storageutil.PoolStorageRoot(poolID))
	currentSource, err := s.findMountSourceByTarget(ctx, poolRoot)
	if err != nil {
		return err
	}
	if currentSource == "" {
		return nil
	}
	if expectedDevice != "" && currentSource != expectedDevice {
		return fmt.Errorf("%w: pool root mounted by %s", domain.ErrInvalidState, currentSource)
	}
	if _, err := s.runPrivileged(ctx, "umount", poolRoot); err != nil {
		return fmt.Errorf("unmount pool root %s: %w", poolRoot, err)
	}
	return nil
}

func (s *StorageManagementService) ensureWritableOwnership(ctx context.Context, path string) error {
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if _, err := s.runPrivileged(ctx, "chown", owner, path); err != nil {
		return fmt.Errorf("set pool root owner: %w", err)
	}
	return nil
}

func (s *StorageManagementService) detectFilesystemType(ctx context.Context, devicePath string) (string, error) {
	out, err := s.runPrivileged(ctx, "lsblk", "-no", "FSTYPE", devicePath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (s *StorageManagementService) findMountTargetBySource(ctx context.Context, devicePath string) (string, error) {
	out, err := s.runPrivileged(ctx, "findmnt", "-rn", "-S", devicePath, "-o", "TARGET")
	if err != nil {
		if isCommandNoMatch(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (s *StorageManagementService) findMountSourceByTarget(ctx context.Context, target string) (string, error) {
	out, err := s.runPrivileged(ctx, "findmnt", "-rn", "-M", target, "-o", "SOURCE")
	if err != nil {
		if isCommandNoMatch(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (s *StorageManagementService) runPrivileged(ctx context.Context, command string, args ...string) (string, error) {
	useSudo := true
	if raw := strings.TrimSpace(os.Getenv("HOLO_STORAGE_USE_SUDO")); raw != "" {
		if parsed, err := strconv.ParseBool(raw); err == nil {
			useSudo = parsed
		}
	}
	if useSudo {
		if helper := strings.TrimSpace(os.Getenv("HOLO_STORAGE_PRIVILEGED_HELPER")); helper != "" {
			helperArgs := append([]string{command}, args...)
			return s.runner.Run(ctx, "sudo", append([]string{helper}, helperArgs...)...)
		}
		sudoArgs := append([]string{command}, args...)
		return s.runner.Run(ctx, "sudo", sudoArgs...)
	}
	return s.runner.Run(ctx, command, args...)
}

func isCommandNoMatch(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "exit status 1")
}

func hasPartitions(node lsblkNode) bool {
	return len(node.Children) > 0
}

func hasMountedFilesystem(node lsblkNode) bool {
	if mountpointPresent(node.Mountpoint) {
		return true
	}
	for _, child := range node.Children {
		if hasMountedFilesystem(child) {
			return true
		}
	}
	return false
}

func hasUnsupportedFilesystemSignature(node lsblkNode) bool {
	fsType := strings.TrimSpace(node.Fstype)
	if fsType == "" {
		return false
	}
	return !strings.EqualFold(fsType, storagePoolFilesystem)
}

func mountpointPresent(raw any) bool {
	switch v := raw.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				return true
			}
		}
		return false
	default:
		return false
	}
}
