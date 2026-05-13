package orchestration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/metrics"
	"github.com/Holo-VTL/Holo/control-plane/internal/storageutil"
)

type TargetRuntimeAdapter interface {
	Publish(ctx context.Context, publication *domain.TargetPublication) (string, error)
	Unpublish(ctx context.Context, publication *domain.TargetPublication) error
	ListSessions(ctx context.Context) ([]TargetSession, error)
}

type TargetRuntimeConfig struct {
	Mode              string
	PortalHost        string
	PortalPort        int
	BackstoreDir      string
	BackstoreSizeMB   int
	UseSudo           bool
	IscsiConfigfsRoot string
}

type StorageWriteGuard interface {
	ReserveWrite(ctx context.Context, poolID string, bytes int64) (*domain.StoragePoolCapacitySnapshot, bool, error)
	RollbackReservedWrite(ctx context.Context, poolID string, bytes int64) error
}

type StoragePoolReader interface {
	GetPool(ctx context.Context, poolID string) (*domain.StoragePoolRuntime, error)
}

type CoreResourceReader interface {
	FindLibrary(ctx context.Context, libraryID string) (*domain.VirtualLibrary, error)
	FindDrive(ctx context.Context, driveID string) (*domain.VirtualDrive, error)
	FindCartridge(ctx context.Context, cartridgeID string) (*domain.VirtualCartridge, error)
}

type TargetRuntimeRepository interface {
	SavePublication(ctx context.Context, p *domain.TargetPublication) error
	SavePublicationIfIQNAvailable(ctx context.Context, p *domain.TargetPublication) error
	FindPublication(ctx context.Context, publicationID string) (*domain.TargetPublication, error)
	FindPublicationByIQN(ctx context.Context, iqn string) (*domain.TargetPublication, bool)
	ListPublications(ctx context.Context) []*domain.TargetPublication
	ListDiscoverablePublications(ctx context.Context) []*domain.TargetPublication
	SaveValidationRun(ctx context.Context, run *domain.ValidationRun) error
	WriteValidationMedia(ctx context.Context, publicationID string, payload []byte) error
	ReadValidationMedia(ctx context.Context, publicationID string) ([]byte, error)
	ListValidationRuns(ctx context.Context, publicationID string) []*domain.ValidationRun
}

type LocalMountSynchronizer interface {
	Sync(ctx context.Context, actor string) (LocalMountStatus, error)
	SyncAsync(actor string)
}

// DefaultTargetRuntimeConfig returns config driven by environment variables:
// - HOLO_RUNTIME_MODE: "in-memory", "lio-shell", or "tcmu"
// - HOLO_PORTAL_HOST: IP address for targets to bind (default 0.0.0.0)
// - HOLO_PORTAL_PORT: TCP port for targets to bind (default 3260)
// - HOLO_USE_SUDO: set to "1" to prefix shell commands with sudo
func DefaultTargetRuntimeConfig() TargetRuntimeConfig {
	cfg := TargetRuntimeConfig{
		Mode:            "in-memory",
		PortalHost:      "127.0.0.1",
		PortalPort:      3260,
		BackstoreDir:    "/var/lib/holo/targets",
		BackstoreSizeMB: 64,
		UseSudo:         true,
	}
	return normalizeTargetRuntimeConfig(cfg)
}

func normalizeTargetRuntimeConfig(cfg TargetRuntimeConfig) TargetRuntimeConfig {
	cfg.Mode = strings.TrimSpace(strings.ToLower(cfg.Mode))
	if cfg.Mode == "" {
		cfg.Mode = "in-memory"
	}
	cfg.PortalHost = strings.TrimSpace(cfg.PortalHost)
	if cfg.PortalHost == "" {
		cfg.PortalHost = "127.0.0.1"
	}
	if cfg.PortalPort <= 0 {
		cfg.PortalPort = 3260
	}
	cfg.BackstoreDir = strings.TrimSpace(cfg.BackstoreDir)
	if cfg.BackstoreDir == "" {
		cfg.BackstoreDir = "/var/lib/holo/targets"
	}
	if cfg.BackstoreSizeMB <= 0 {
		cfg.BackstoreSizeMB = 64
	}
	cfg.IscsiConfigfsRoot = strings.TrimSpace(cfg.IscsiConfigfsRoot)
	if cfg.IscsiConfigfsRoot == "" {
		cfg.IscsiConfigfsRoot = iscsiConfigfsRoot
	}
	return cfg
}

type inMemoryTargetRuntimeAdapter struct {
	portal string
}

func newInMemoryTargetRuntimeAdapter(cfg TargetRuntimeConfig) *inMemoryTargetRuntimeAdapter {
	return &inMemoryTargetRuntimeAdapter{
		portal: fmt.Sprintf("%s:%d", cfg.PortalHost, cfg.PortalPort),
	}
}

func (a *inMemoryTargetRuntimeAdapter) Publish(_ context.Context, publication *domain.TargetPublication) (string, error) {
	return a.portal, nil
}

func (a *inMemoryTargetRuntimeAdapter) Unpublish(_ context.Context, _ *domain.TargetPublication) error {
	return nil
}

func (a *inMemoryTargetRuntimeAdapter) ListSessions(_ context.Context) ([]TargetSession, error) {
	return nil, nil
}

type TargetSession struct {
	TargetIQN     string
	InitiatorIQN  string
	SourceAddress string
	SessionID     string
}

type commandRunner interface {
	Run(ctx context.Context, command string, args ...string) (string, error)
}

type targetcliRunFunc func(ctx context.Context, args ...string) error
type targetcliDeleteTargetFunc func(ctx context.Context, targetIQN string) error

type osCommandRunner struct{}

func (r *osCommandRunner) Run(ctx context.Context, command string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		if trimmed == "" {
			return "", err
		}
		return "", fmt.Errorf("%w: %s", err, trimmed)
	}
	return trimmed, nil
}

type lioShellTargetRuntimeAdapter struct {
	cfg    TargetRuntimeConfig
	runner commandRunner
}

func newLIOShellTargetRuntimeAdapter(cfg TargetRuntimeConfig, runner commandRunner) *lioShellTargetRuntimeAdapter {
	if runner == nil {
		runner = &osCommandRunner{}
	}
	return &lioShellTargetRuntimeAdapter{
		cfg:    normalizeTargetRuntimeConfig(cfg),
		runner: runner,
	}
}

func (a *lioShellTargetRuntimeAdapter) Publish(ctx context.Context, publication *domain.TargetPublication) (string, error) {
	if err := validateTargetPublicationForRuntime(publication); err != nil {
		return "", err
	}
	backstoreDir, err := lioBackstoreDir(a.cfg, publication)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(backstoreDir, 0o755); err != nil {
		return "", fmt.Errorf("create backstore directory: %w", err)
	}

	backstoreName := runtimeBackstoreName(publication)
	backstorePath, err := runtimeBackstorePath(backstoreDir, backstoreName)
	if err != nil {
		return "", err
	}
	if err := ensureBackstoreImage(backstorePath, a.cfg.BackstoreSizeMB); err != nil {
		return "", err
	}

	if err := a.runTargetcli(ctx, "/backstores/fileio", "create",
		"name="+backstoreName,
		"file_or_dev="+backstorePath,
		fmt.Sprintf("size=%dM", a.cfg.BackstoreSizeMB),
	); err != nil {
		return "", fmt.Errorf("create fileio backstore: %w", err)
	}

	if err := createISCSITargetReplacingExisting(ctx, a.runTargetcli, a.deleteTarget, publication.TargetIQN); err != nil {
		_ = a.deleteBackstore(ctx, backstoreName)
		return "", fmt.Errorf("create iscsi target: %w", err)
	}

	tpgAttributeArgs := append([]string{
		"/iscsi/" + publication.TargetIQN + "/tpg1",
		"set",
		"attribute",
	}, tcmuTargetcliTPGAttributes...)
	if err := a.runTargetcli(ctx, tpgAttributeArgs...); err != nil {
		_ = a.deleteTarget(ctx, publication.TargetIQN)
		_ = a.deleteBackstore(ctx, backstoreName)
		return "", fmt.Errorf("configure iscsi tpg attributes: %w", err)
	}

	lunPath := "/backstores/fileio/" + backstoreName
	if err := a.runTargetcli(ctx, "/iscsi/"+publication.TargetIQN+"/tpg1/luns", "create", lunPath); err != nil {
		_ = a.deleteTarget(ctx, publication.TargetIQN)
		_ = a.deleteBackstore(ctx, backstoreName)
		return "", fmt.Errorf("create iscsi lun: %w", err)
	}

	portal := fmt.Sprintf("%s:%d", a.cfg.PortalHost, a.cfg.PortalPort)
	return portal, nil
}

func (a *lioShellTargetRuntimeAdapter) Unpublish(ctx context.Context, publication *domain.TargetPublication) error {
	if err := validateTargetPublicationForRuntime(publication); err != nil {
		return err
	}
	backstoreName := runtimeBackstoreName(publication)

	if err := a.deleteTarget(ctx, publication.TargetIQN); err != nil {
		return err
	}
	if err := a.deleteBackstore(ctx, backstoreName); err != nil {
		return err
	}

	backstoreDir, err := lioBackstoreDir(a.cfg, publication)
	if err != nil {
		return err
	}
	backstorePath, err := runtimeBackstorePath(backstoreDir, backstoreName)
	if err != nil {
		return err
	}
	if err := os.Remove(backstorePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove backstore image: %w", err)
	}
	return nil
}

func (a *lioShellTargetRuntimeAdapter) deleteTarget(ctx context.Context, targetIQN string) error {
	err := a.runTargetcli(ctx, "/iscsi", "delete", targetIQN)
	if err != nil && !isIgnorableTargetcliError(err) {
		return fmt.Errorf("delete iscsi target: %w", err)
	}
	return nil
}

func (a *lioShellTargetRuntimeAdapter) deleteBackstore(ctx context.Context, backstoreName string) error {
	err := a.runTargetcli(ctx, "/backstores/fileio", "delete", backstoreName)
	if err != nil && !isIgnorableTargetcliError(err) {
		return fmt.Errorf("delete fileio backstore: %w", err)
	}
	return nil
}

func (a *lioShellTargetRuntimeAdapter) runTargetcli(ctx context.Context, args ...string) error {
	_, err := a.runTargetcliOutput(ctx, args...)
	return err
}

func (a *lioShellTargetRuntimeAdapter) runTargetcliOutput(ctx context.Context, args ...string) (string, error) {
	if err := validateTargetcliArgs(args...); err != nil {
		return "", err
	}
	timeout := targetcliTimeout()
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, cmdArgs := targetcliCommand(a.cfg.UseSudo, args...)
	out, err := a.runner.Run(timeoutCtx, cmd, cmdArgs...)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(timeoutCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("targetcli timed out after %s: %s", timeout, strings.Join(args, " "))
		}
		return "", err
	}
	return out, nil
}

func (a *lioShellTargetRuntimeAdapter) ListSessions(ctx context.Context) ([]TargetSession, error) {
	sessions, err := listConfigfsTargetSessions(a.cfg.IscsiConfigfsRoot)
	if err == nil && sessions != nil {
		return sessions, nil
	}
	if err != nil {
		log.Printf("configfs target session discovery unavailable err=%v", err)
	}

	out, err := a.runTargetcliOutput(ctx, "sessions", "detail")
	if err != nil {
		return fallbackIscsiadmSessions(ctx, a.runner, a.cfg.UseSudo, nil), nil
	}
	sessions = parseTargetcliSessions(out)
	if len(sessions) > 0 {
		return sessions, nil
	}
	return fallbackIscsiadmSessions(ctx, a.runner, a.cfg.UseSudo, sessions), nil
}

func targetcliCommand(useSudo bool, args ...string) (string, []string) {
	cmdArgs := append([]string(nil), args...)
	if !useSudo {
		return "targetcli", cmdArgs
	}
	target := strings.TrimSpace(os.Getenv("HOLO_TARGETCLI_PRIVILEGED_HELPER"))
	if !isSafeTargetcliHelperPath(target) {
		if target != "" {
			warnInvalidTargetcliHelper(target)
		}
		target = "targetcli"
	}
	return "sudo", append([]string{"-n", target}, cmdArgs...)
}

func iscsiadmCommand(useSudo bool, args ...string) (string, []string) {
	cmdArgs := append([]string(nil), args...)
	if !useSudo {
		return "iscsiadm", cmdArgs
	}
	return "sudo", append([]string{"-n", "iscsiadm"}, cmdArgs...)
}

var invalidTargetcliHelperWarnings sync.Map

func warnInvalidTargetcliHelper(path string) {
	if _, loaded := invalidTargetcliHelperWarnings.LoadOrStore(path, struct{}{}); loaded {
		return
	}
	log.Printf("WARNING: ignoring unsafe HOLO_TARGETCLI_PRIVILEGED_HELPER=%q; using targetcli fallback", path)
}

func isSafeTargetcliHelperPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	cleaned := filepath.Clean(path)
	if filepath.Base(cleaned) != "holo-targetcli-helper" || filepath.Base(filepath.Dir(cleaned)) != "bin" {
		return false
	}
	for _, unsafeRoot := range []string{"/tmp", "/var/tmp", "/dev/shm"} {
		if cleaned == unsafeRoot || strings.HasPrefix(cleaned, unsafeRoot+string(os.PathSeparator)) {
			return false
		}
	}
	exe, err := os.Executable()
	if err == nil && cleaned == filepath.Join(filepath.Dir(exe), "holo-targetcli-helper") {
		return true
	}
	return strings.HasPrefix(cleaned, string(os.PathSeparator)+"opt"+string(os.PathSeparator)) ||
		strings.HasPrefix(cleaned, string(os.PathSeparator)+"usr"+string(os.PathSeparator))
}

func runtimeBackstoreName(publication *domain.TargetPublication) string {
	base := publication.PublicationID
	if strings.TrimSpace(base) == "" {
		base = publication.TargetIQN
	}
	base = strings.ToLower(base)
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	name := strings.Trim(b.String(), "_")
	if name == "" {
		name = "holo_pub"
	}
	if len(name) > 48 {
		name = name[:48]
	}
	return "holo_" + name
}

func runtimeBackstorePath(backstoreDir, backstoreName string) (string, error) {
	if !isSafeTargetcliToken(backstoreName) {
		return "", domain.ErrInvalidInput
	}
	return storageutil.SafeJoin(backstoreDir, backstoreName+".img")
}

func lioBackstoreDir(cfg TargetRuntimeConfig, publication *domain.TargetPublication) (string, error) {
	if publication != nil && strings.TrimSpace(publication.PoolID) != "" {
		poolBase := storageutil.ResolvePoolStorageBaseDir()
		if err := storageutil.ValidateRoot(storageutil.RootKindPool, poolBase); err != nil {
			return "", err
		}
		poolRoot, err := storageutil.SafeJoin(poolBase, storageutil.SanitizeLayoutID(publication.PoolID))
		if err != nil {
			return "", err
		}
		return storageutil.SafeJoin(poolRoot, "targets")
	}
	if err := storageutil.ValidateRoot(storageutil.RootKindBackstore, cfg.BackstoreDir); err != nil {
		return "", err
	}
	return filepath.Clean(cfg.BackstoreDir), nil
}

func ensureBackstoreImage(path string, sizeMB int) error {
	if sizeMB <= 0 {
		sizeMB = 64
	}
	sizeBytes := int64(sizeMB) * 1024 * 1024
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return fmt.Errorf("open backstore image: %w", err)
	}
	defer f.Close()
	if err := f.Truncate(sizeBytes); err != nil {
		return fmt.Errorf("resize backstore image: %w", err)
	}
	return nil
}

func createISCSITargetReplacingExisting(ctx context.Context, run targetcliRunFunc, deleteTarget targetcliDeleteTargetFunc, targetIQN string) error {
	err := run(ctx, "/iscsi", "create", targetIQN)
	if err == nil {
		return nil
	}
	if !isAlreadyExistsTargetcliError(err) {
		return err
	}
	if deleteErr := deleteTarget(ctx, targetIQN); deleteErr != nil {
		return fmt.Errorf("replace existing iscsi target: %w", deleteErr)
	}
	if retryErr := run(ctx, "/iscsi", "create", targetIQN); retryErr != nil {
		return fmt.Errorf("create iscsi target after replacing existing target: %w", retryErr)
	}
	return nil
}

func isIgnorableTargetcliError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "no such target in configfs") ||
		strings.Contains(msg, "no such object in configfs") ||
		strings.Contains(msg, "no storage object named") ||
		strings.Contains(msg, "already exists")
}

func isAlreadyExistsTargetcliError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists")
}

type PublishRequest struct {
	LibraryID     string `json:"libraryId"`
	DriveID       string `json:"driveId"`
	CartridgeID   string `json:"cartridgeId"`
	TargetIQN     string `json:"targetIqn"`
	DeviceRole    string `json:"deviceRole,omitempty"`
	DeviceProfile string `json:"deviceProfile,omitempty"`
	DriveProfile  string `json:"driveProfile,omitempty"`
	Actor         string `json:"actor"`
}

type TargetRuntimeService struct {
	coreRepo    CoreResourceReader
	runtimeRepo TargetRuntimeRepository
	adapter     TargetRuntimeAdapter
	auditW      audit.Writer
	cfg         TargetRuntimeConfig
	metrics     *metrics.MetricsRegistry
	storageWg   StorageWriteGuard
	poolReader  StoragePoolReader
	localMount  LocalMountSynchronizer

	sessionMu       sync.Mutex
	sessionCache    targetSessionCache
	sessionInflight *targetSessionInflight
}

type targetSessionCache struct {
	expiresAt time.Time
	result    targetSessionResult
}

type targetSessionInflight struct {
	done   chan struct{}
	result targetSessionResult
}

type targetSessionResult struct {
	hosts map[string]*domain.ConnectedHosts
	err   error
}

const connectedHostsCacheTTL = 3 * time.Second
const maxTargetSessionRows = 10000
const iscsiConfigfsRoot = "/sys/kernel/config/target/iscsi"

func NewTargetRuntimeService(coreRepo CoreResourceReader, runtimeRepo TargetRuntimeRepository, auditW audit.Writer, m *metrics.MetricsRegistry) *TargetRuntimeService {
	return NewTargetRuntimeServiceWithConfig(coreRepo, runtimeRepo, auditW, m, DefaultTargetRuntimeConfig())
}

func NewTargetRuntimeServiceWithConfig(coreRepo CoreResourceReader, runtimeRepo TargetRuntimeRepository, auditW audit.Writer, m *metrics.MetricsRegistry, cfg TargetRuntimeConfig) *TargetRuntimeService {
	cfg = normalizeTargetRuntimeConfig(cfg)
	return &TargetRuntimeService{
		coreRepo:    coreRepo,
		runtimeRepo: runtimeRepo,
		adapter:     newTargetRuntimeAdapter(cfg, auditW),
		auditW:      auditW,
		cfg:         cfg,
		metrics:     m,
	}
}

func newTargetRuntimeAdapter(cfg TargetRuntimeConfig, auditW audit.Writer) TargetRuntimeAdapter {
	switch cfg.Mode {
	case "lio-shell":
		return newLIOShellTargetRuntimeAdapter(cfg, nil)
	case "tcmu":
		return newTcmuAdapter(cfg, nil, auditW)
	default:
		return newInMemoryTargetRuntimeAdapter(cfg)
	}
}

func newTargetRuntimeServiceWithAdapter(coreRepo CoreResourceReader, runtimeRepo TargetRuntimeRepository, auditW audit.Writer, m *metrics.MetricsRegistry, cfg TargetRuntimeConfig, adapter TargetRuntimeAdapter) *TargetRuntimeService {
	cfg = normalizeTargetRuntimeConfig(cfg)
	if adapter == nil {
		adapter = newTargetRuntimeAdapter(cfg, auditW)
	}
	return &TargetRuntimeService{
		coreRepo:    coreRepo,
		runtimeRepo: runtimeRepo,
		adapter:     adapter,
		auditW:      auditW,
		cfg:         cfg,
		metrics:     m,
	}
}

func (s *TargetRuntimeService) SetStorageWriteGuard(guard StorageWriteGuard) {
	s.storageWg = guard
}

func (s *TargetRuntimeService) SetStoragePoolReader(reader StoragePoolReader) {
	s.poolReader = reader
}

func (s *TargetRuntimeService) SetLocalMountSynchronizer(syncer LocalMountSynchronizer) {
	s.localMount = syncer
}

func (s *TargetRuntimeService) Publish(ctx context.Context, req PublishRequest) (*domain.TargetPublication, error) {
	if req.LibraryID == "" || req.DriveID == "" || req.CartridgeID == "" {
		return nil, domain.ErrInvalidInput
	}
	targetIQN, err := normalizeTargetIQN(req.TargetIQN, req.DriveID)
	if err != nil {
		return nil, domain.ErrInvalidInput
	}
	req.TargetIQN = targetIQN

	library, err := s.coreRepo.FindLibrary(ctx, req.LibraryID)
	if err != nil {
		return nil, err
	}
	drive, err := s.coreRepo.FindDrive(ctx, req.DriveID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(drive.LibraryID) != strings.TrimSpace(req.LibraryID) {
		return nil, domain.ErrInvalidInput
	}
	cartridge, err := s.coreRepo.FindCartridge(ctx, req.CartridgeID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cartridge.LibraryID) != strings.TrimSpace(req.LibraryID) {
		return nil, domain.ErrInvalidInput
	}
	poolID := strings.TrimSpace(cartridge.PoolID)
	if poolID == "" {
		return nil, domain.ErrInvalidState
	}
	if s.poolReader != nil {
		pool, err := s.poolReader.GetPool(ctx, poolID)
		if err != nil {
			return nil, err
		}
		if storageutil.StrictStorageFlowEnabled() && len(pool.Disks) == 0 {
			return nil, domain.ErrInvalidState
		}
	}

	publicationID, err := newRuntimeID("pub")
	if err != nil {
		return nil, fmt.Errorf("generate publication id: %w", err)
	}
	publication, err := domain.NewTargetPublication(publicationID, poolID, req.LibraryID, req.DriveID, req.CartridgeID, req.TargetIQN)
	if err != nil {
		return nil, err
	}
	if err := publication.SetDeviceIdentity(req.DeviceRole, req.DeviceProfile); err != nil {
		return nil, err
	}
	publication.CompressionEnabled = library.CompressionEnabled
	publication.DedupEnabled = library.DedupEnabled
	publication.SetDriveProfile(strings.TrimSpace(req.DriveProfile))
	if err := s.runtimeRepo.SavePublicationIfIQNAvailable(ctx, publication); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			existing, _ := s.runtimeRepo.FindPublicationByIQN(ctx, req.TargetIQN)
			existingID := ""
			if existing != nil {
				existingID = existing.PublicationID
			}
			audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(req.Actor), "publish", existingID, "failure", map[string]any{"reason": "duplicate_target_iqn", "targetIqn": req.TargetIQN, "runtimeMode": s.cfg.Mode})
			return nil, domain.ErrConflict
		}
		return nil, err
	}

	portal, err := s.adapter.Publish(ctx, publication)
	if err != nil {
		_ = publication.MarkFailed(err.Error())
		if saveErr := s.runtimeRepo.SavePublication(ctx, publication); saveErr != nil {
			return nil, saveErr
		}
		audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(req.Actor), "publish", publication.PublicationID, "failure", map[string]any{"error": err.Error(), "runtimeMode": s.cfg.Mode})
		return publication, err
	}

	if err := publication.MarkReady(portal); err != nil {
		return nil, err
	}
	if err := s.runtimeRepo.SavePublication(ctx, publication); err != nil {
		return nil, err
	}
	audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(req.Actor), "publish", publication.PublicationID, "success", map[string]any{"targetIqn": publication.TargetIQN, "portal": publication.Portal, "runtimeMode": s.cfg.Mode})

	if s.metrics != nil {
		s.metrics.RecordPublicationPublish()
	}
	s.syncLocalMount(ctx, req.Actor)

	return publication, nil
}

func (s *TargetRuntimeService) Unpublish(ctx context.Context, publicationID, actor string) (*domain.TargetPublication, error) {
	publication, err := s.runtimeRepo.FindPublication(ctx, publicationID)
	if err != nil {
		return nil, err
	}
	if publication.State == domain.PublicationDisabled {
		audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "unpublish", publicationID, "success", map[string]any{"noop": true, "runtimeMode": s.cfg.Mode})
		return publication, nil
	}
	if err := s.adapter.Unpublish(ctx, publication); err != nil {
		audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "unpublish", publicationID, "failure", map[string]any{"error": err.Error(), "runtimeMode": s.cfg.Mode})
		return nil, err
	}
	if err := publication.Disable(); err != nil {
		audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "unpublish", publicationID, "failure", map[string]any{"error": err.Error(), "runtimeMode": s.cfg.Mode})
		return nil, err
	}
	if err := s.runtimeRepo.SavePublication(ctx, publication); err != nil {
		return nil, err
	}
	audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "unpublish", publicationID, "success", map[string]any{"state": publication.State, "noop": false, "runtimeMode": s.cfg.Mode})

	if s.metrics != nil {
		s.metrics.RecordPublicationUnpublish()
	}
	s.syncLocalMount(ctx, actor)

	return publication, nil
}

func (s *TargetRuntimeService) Rollback(ctx context.Context, publicationID, actor string) (*domain.TargetPublication, error) {
	publication, err := s.runtimeRepo.FindPublication(ctx, publicationID)
	if err != nil {
		return nil, err
	}
	if publication.State == domain.PublicationDisabled {
		audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "rollback", publicationID, "success", map[string]any{"noop": true})
		return publication, nil
	}
	if publication.State == domain.PublicationCreating {
		if err := publication.MarkFailed("rollback from creating state"); err != nil {
			return nil, err
		}
	}
	if err := publication.Disable(); err != nil {
		return nil, err
	}
	if err := s.runtimeRepo.SavePublication(ctx, publication); err != nil {
		return nil, err
	}
	audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "rollback", publicationID, "success", map[string]any{"state": publication.State})
	return publication, nil
}

func (s *TargetRuntimeService) RestoreReadyPublications(ctx context.Context) error {
	publications := s.runtimeRepo.ListPublications(ctx)
	var firstErr error
	for _, publication := range publications {
		if publication.State != domain.PublicationReady {
			continue
		}
		_ = s.adapter.Unpublish(ctx, publication)
		portal, err := s.adapter.Publish(ctx, publication)
		if err != nil {
			publication.MarkRuntimeFailed(err.Error())
			if saveErr := s.runtimeRepo.SavePublication(ctx, publication); saveErr != nil && firstErr == nil {
				firstErr = saveErr
			}
			audit.EmitTargetRuntimeEvent(ctx, s.auditW, "system", "restore", publication.PublicationID, "failure", map[string]any{"error": err.Error(), "runtimeMode": s.cfg.Mode})
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		publication.Portal = portal
		publication.LastError = ""
		publication.UpdatedAt = time.Now().UTC()
		if err := s.runtimeRepo.SavePublication(ctx, publication); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		audit.EmitTargetRuntimeEvent(ctx, s.auditW, "system", "restore", publication.PublicationID, "success", map[string]any{"targetIqn": publication.TargetIQN, "portal": portal, "runtimeMode": s.cfg.Mode})
	}
	s.syncLocalMount(ctx, "system")
	return firstErr
}

func (s *TargetRuntimeService) Shutdown(ctx context.Context) error {
	if s == nil || s.runtimeRepo == nil || s.adapter == nil {
		return nil
	}
	publications := s.runtimeRepo.ListPublications(ctx)
	var firstErr error
	for _, publication := range publications {
		if publication.State != domain.PublicationReady {
			continue
		}
		if err := s.adapter.Unpublish(ctx, publication); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("shutdown publication %s: %w", publication.PublicationID, err)
		}
	}
	return firstErr
}

func (s *TargetRuntimeService) StartValidationRun(ctx context.Context, publicationID, actor string) (*domain.ValidationRun, error) {
	return s.StartValidationRunWithRequest(ctx, publicationID, actor, ValidationRunRequest{})
}

func (s *TargetRuntimeService) StartValidationRunWithRequest(ctx context.Context, publicationID, actor string, req ValidationRunRequest) (*domain.ValidationRun, error) {
	publication, err := s.runtimeRepo.FindPublication(ctx, publicationID)
	if err != nil {
		return nil, err
	}
	if publication.State != domain.PublicationReady {
		return nil, domain.ErrInvalidState
	}
	req = req.Normalize()
	if err := req.Validate(); err != nil {
		return nil, err
	}

	validationID, err := newRuntimeID("val")
	if err != nil {
		return nil, fmt.Errorf("generate validation id: %w", err)
	}
	run, err := domain.NewValidationRun(validationID, publicationID)
	if err != nil {
		return nil, err
	}
	run.Mode = req.Mode

	payload, err := buildValidationPayload(req)
	if err != nil {
		return nil, err
	}

	committedReservation := false
	if s.storageWg != nil && publication.PoolID != "" && len(payload) > 0 {
		if _, warning, reserveErr := s.storageWg.ReserveWrite(ctx, publication.PoolID, int64(len(payload))); reserveErr != nil {
			if reserveErr != domain.ErrNotFound {
				audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "validate", publicationID, "failure", map[string]any{
					"reason":    "insufficient_storage_capacity",
					"poolId":    publication.PoolID,
					"bytes":     len(payload),
					"mode":      req.Mode,
					"errorHint": reserveErr.Error(),
				})
				return nil, reserveErr
			}
		} else if warning {
			audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "storage_capacity_warning", publication.PoolID, "success", map[string]any{
				"publicationId": publicationID,
				"bytes":         len(payload),
			})
		}
		committedReservation = true
		defer func() {
			if committedReservation {
				if rollbackErr := s.storageWg.RollbackReservedWrite(ctx, publication.PoolID, int64(len(payload))); rollbackErr != nil {
					log.Printf("storage write reservation rollback failed pool=%s publication=%s err=%v", publication.PoolID, publicationID, rollbackErr)
				}
			}
		}()
	}

	if err := s.runtimeRepo.WriteValidationMedia(ctx, publicationID, payload); err != nil {
		return nil, err
	}
	readback, err := s.runtimeRepo.ReadValidationMedia(ctx, publicationID)
	if err != nil {
		return nil, err
	}

	writeDigest := sha256Hex(payload)
	readDigest := sha256Hex(readback)
	written := int64(len(payload))
	read := int64(len(readback))

	// Defensive guard: if readback was modified, preserve failed evidence.
	if !bytes.Equal(payload, readback) {
		readDigest = "mismatch:" + readDigest
	}

	evidence := fmt.Sprintf("tests/compatibility/iscsi/evidence/%s.json", validationID)
	if err := run.Complete(written, read, writeDigest, readDigest, evidence); err != nil {
		return nil, err
	}
	if err := s.runtimeRepo.SaveValidationRun(ctx, run); err != nil {
		return nil, err
	}
	committedReservation = false

	result := "success"
	if run.Status != domain.ValidationPassed {
		result = "failure"
	}
	audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "validate", run.ValidationID, result, map[string]any{"publicationId": publicationID, "status": run.Status, "mode": run.Mode, "bytesWritten": run.BytesWritten, "bytesRead": run.BytesRead, "writeDigest": run.WriteDigest, "readDigest": run.ReadDigest})
	return run, nil
}

func (s *TargetRuntimeService) ListPublications(ctx context.Context) []*domain.TargetPublication {
	return s.runtimeRepo.ListPublications(ctx)
}

func (s *TargetRuntimeService) ListPublicationsWithConnectedHosts(ctx context.Context) []*domain.TargetPublication {
	publications := s.runtimeRepo.ListPublications(ctx)
	if !hasReadyPublications(publications) {
		return publications
	}

	byTarget, err := s.connectedHostSummaries(ctx)
	if err != nil {
		log.Printf("target session discovery unavailable err=%v", err)
		for _, publication := range publications {
			if publication == nil || publication.State != domain.PublicationReady {
				continue
			}
			publication.ConnectedHosts = &domain.ConnectedHosts{
				Available:    false,
				HostCount:    0,
				SessionCount: 0,
				Initiators:   []string{},
				LastError:    "session discovery unavailable",
			}
		}
		return publications
	}

	for _, publication := range publications {
		if publication == nil || publication.State != domain.PublicationReady {
			continue
		}
		summary := byTarget[strings.ToLower(strings.TrimSpace(publication.TargetIQN))]
		if summary == nil {
			summary = &domain.ConnectedHosts{Available: true, Initiators: []string{}}
		}
		publication.ConnectedHosts = cloneConnectedHosts(summary)
	}
	return publications
}

func hasReadyPublications(publications []*domain.TargetPublication) bool {
	for _, publication := range publications {
		if publication != nil && publication.State == domain.PublicationReady {
			return true
		}
	}
	return false
}

func (s *TargetRuntimeService) connectedHostSummaries(ctx context.Context) (map[string]*domain.ConnectedHosts, error) {
	now := time.Now()
	s.sessionMu.Lock()
	if now.Before(s.sessionCache.expiresAt) {
		result := s.sessionCache.result
		s.sessionMu.Unlock()
		return cloneConnectedHostMap(result.hosts), result.err
	}
	if s.sessionInflight != nil {
		inflight := s.sessionInflight
		s.sessionMu.Unlock()
		select {
		case <-inflight.done:
			return cloneConnectedHostMap(inflight.result.hosts), inflight.result.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	inflight := &targetSessionInflight{done: make(chan struct{})}
	s.sessionInflight = inflight
	s.sessionMu.Unlock()

	result := s.discoverConnectedHosts(ctx)

	s.sessionMu.Lock()
	inflight.result = result
	s.sessionCache = targetSessionCache{
		expiresAt: time.Now().Add(connectedHostsCacheTTL),
		result:    result,
	}
	s.sessionInflight = nil
	close(inflight.done)
	s.sessionMu.Unlock()

	return cloneConnectedHostMap(result.hosts), result.err
}

func (s *TargetRuntimeService) discoverConnectedHosts(ctx context.Context) (result targetSessionResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = targetSessionResult{err: fmt.Errorf("target session discovery panic: %v", recovered)}
		}
	}()

	sessions, err := s.adapter.ListSessions(ctx)
	result.err = err
	if err == nil {
		result.hosts = connectedHostsByTarget(sessions)
	}
	return result
}

func cloneConnectedHostMap(in map[string]*domain.ConnectedHosts) map[string]*domain.ConnectedHosts {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*domain.ConnectedHosts, len(in))
	for key, summary := range in {
		out[key] = cloneConnectedHosts(summary)
	}
	return out
}

func cloneConnectedHosts(in *domain.ConnectedHosts) *domain.ConnectedHosts {
	if in == nil {
		return nil
	}
	out := *in
	out.Initiators = append([]string(nil), in.Initiators...)
	return &out
}

func connectedHostsByTarget(sessions []TargetSession) map[string]*domain.ConnectedHosts {
	type accumulator struct {
		initiators map[string]struct{}
		sessions   int
	}
	acc := make(map[string]*accumulator)
	for _, session := range sessions {
		targetIQN := strings.ToLower(strings.TrimSpace(session.TargetIQN))
		initiatorIQN := strings.ToLower(strings.TrimSpace(session.InitiatorIQN))
		if !targetIQNPattern.MatchString(targetIQN) || !iqnValuePattern.MatchString(initiatorIQN) {
			continue
		}
		item := acc[targetIQN]
		if item == nil {
			item = &accumulator{initiators: make(map[string]struct{})}
			acc[targetIQN] = item
		}
		item.sessions++
		item.initiators[initiatorIQN] = struct{}{}
	}

	out := make(map[string]*domain.ConnectedHosts, len(acc))
	for targetIQN, item := range acc {
		initiators := make([]string, 0, len(item.initiators))
		for initiator := range item.initiators {
			initiators = append(initiators, initiator)
		}
		sort.Strings(initiators)
		out[targetIQN] = &domain.ConnectedHosts{
			Available:    true,
			HostCount:    len(initiators),
			SessionCount: item.sessions,
			Initiators:   initiators,
		}
	}
	return out
}

func parseTargetcliSessions(output string) []TargetSession {
	var sessions []TargetSession
	currentTarget := ""
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		iqns := iqnInTextPattern.FindAllString(lower, -1)
		if len(iqns) == 0 {
			continue
		}
		if isTargetcliTargetLine(lower) {
			currentTarget = iqns[0]
			continue
		}
		if isTargetcliInitiatorLine(lower) {
			initiator := iqns[len(iqns)-1]
			target := currentTarget
			if len(iqns) > 1 && strings.Contains(lower, "/acls/iqn.") {
				target = iqns[0]
			} else if target == "" && len(iqns) > 1 {
				target = iqns[0]
			}
			if target != "" && target != initiator {
				sessions = append(sessions, TargetSession{
					TargetIQN:    target,
					InitiatorIQN: initiator,
				})
				if len(sessions) >= maxTargetSessionRows {
					log.Printf("target session discovery truncated at %d rows", maxTargetSessionRows)
					return sessions
				}
			}
		}
	}
	return sessions
}

func listConfigfsTargetSessions(root string) ([]TargetSession, error) {
	pattern := filepath.Join(root, "iqn.*", "tpgt_1", "dynamic_sessions")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, nil
	}

	sessions := make([]TargetSession, 0)
	for _, path := range matches {
		targetIQN := strings.ToLower(filepath.Base(filepath.Dir(filepath.Dir(path))))
		if !targetIQNPattern.MatchString(targetIQN) {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read dynamic sessions for %s: %w", targetIQN, err)
		}
		for _, initiator := range parseConfigfsDynamicSessions(raw) {
			sessions = append(sessions, TargetSession{
				TargetIQN:    targetIQN,
				InitiatorIQN: initiator,
			})
			if len(sessions) >= maxTargetSessionRows {
				log.Printf("configfs target session discovery truncated at %d rows", maxTargetSessionRows)
				return sessions, nil
			}
		}
	}
	return sessions, nil
}

func parseConfigfsDynamicSessions(raw []byte) []string {
	text := strings.ReplaceAll(string(raw), "\x00", "\n")
	var initiators []string
	seen := make(map[string]struct{})
	for _, field := range strings.Fields(text) {
		initiator := strings.ToLower(strings.TrimSpace(field))
		if !iqnValuePattern.MatchString(initiator) {
			continue
		}
		if _, exists := seen[initiator]; exists {
			continue
		}
		seen[initiator] = struct{}{}
		initiators = append(initiators, initiator)
	}
	sort.Strings(initiators)
	return initiators
}

func fallbackIscsiadmSessions(ctx context.Context, runner commandRunner, useSudo bool, current []TargetSession) []TargetSession {
	out, err := runIscsiadmSessionsOutput(ctx, runner, useSudo)
	if err != nil {
		if !isNoActiveIscsiadmSessions(err) && !isIscsiadmUnavailable(err) {
			log.Printf("iscsi session fallback unavailable err=%v", err)
		}
		return current
	}
	sessions := parseIscsiadmSessions(out)
	if len(sessions) == 0 {
		return current
	}
	return sessions
}

func runIscsiadmSessionsOutput(ctx context.Context, runner commandRunner, useSudo bool) (string, error) {
	timeout := targetcliTimeout()
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, args := iscsiadmCommand(useSudo, "-m", "session", "-P", "3")
	out, err := runner.Run(timeoutCtx, cmd, args...)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(timeoutCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("iscsiadm timed out after %s", timeout)
		}
		return "", err
	}
	return out, nil
}

func parseIscsiadmSessions(output string) []TargetSession {
	var sessions []TargetSession
	current := TargetSession{}
	flush := func() {
		if current.TargetIQN != "" && current.InitiatorIQN != "" && len(sessions) < maxTargetSessionRows {
			sessions = append(sessions, current)
		}
		current = TargetSession{}
	}

	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "target:"):
			flush()
			iqns := iqnInTextPattern.FindAllString(lower, -1)
			if len(iqns) > 0 {
				current.TargetIQN = iqns[0]
			}
		case strings.HasPrefix(lower, "iface initiatorname:"):
			iqns := iqnInTextPattern.FindAllString(lower, -1)
			if len(iqns) > 0 {
				current.InitiatorIQN = iqns[0]
			}
		case strings.HasPrefix(lower, "iface ipaddress:"):
			current.SourceAddress = strings.TrimSpace(strings.TrimPrefix(line, "Iface IPaddress:"))
		case strings.HasPrefix(lower, "sid:"):
			current.SessionID = strings.TrimSpace(strings.TrimPrefix(line, "SID:"))
		}
		if len(sessions) >= maxTargetSessionRows {
			log.Printf("iscsi session discovery truncated at %d rows", maxTargetSessionRows)
			return sessions
		}
	}
	flush()
	if len(sessions) > maxTargetSessionRows {
		log.Printf("iscsi session discovery truncated at %d rows", maxTargetSessionRows)
		return sessions[:maxTargetSessionRows]
	}
	return sessions
}

func isNoActiveIscsiadmSessions(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no active sessions")
}

func isIscsiadmUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "executable file not found") ||
		strings.Contains(msg, "command not found") ||
		strings.Contains(msg, "no such file or directory")
}

func isTargetcliTargetLine(lower string) bool {
	return strings.HasPrefix(lower, "target:") ||
		strings.HasPrefix(lower, "o- iqn.") ||
		strings.HasPrefix(lower, "+- iqn.") ||
		(strings.Contains(lower, "/iscsi/iqn.") && !strings.Contains(lower, "/acls/iqn."))
}

func isTargetcliInitiatorLine(lower string) bool {
	return strings.HasPrefix(lower, "initiator:") ||
		strings.HasPrefix(lower, "acl:") ||
		strings.Contains(lower, "/acls/iqn.") ||
		strings.Contains(lower, "mapped lun")
}

func (s *TargetRuntimeService) syncLocalMount(ctx context.Context, actor string) {
	if s.localMount == nil {
		return
	}
	s.localMount.SyncAsync(actor)
}

func (s *TargetRuntimeService) GetPublication(ctx context.Context, publicationID string) (*domain.TargetPublication, error) {
	return s.runtimeRepo.FindPublication(ctx, publicationID)
}

func (s *TargetRuntimeService) ListValidationRuns(ctx context.Context, publicationID string) ([]*domain.ValidationRun, error) {
	if _, err := s.runtimeRepo.FindPublication(ctx, publicationID); err != nil {
		return nil, err
	}
	return s.runtimeRepo.ListValidationRuns(ctx, publicationID), nil
}

func (s *TargetRuntimeService) HealthSnapshot() TargetRuntimeHealth {
	publications := s.runtimeRepo.ListPublications(context.Background())
	snapshot := TargetRuntimeHealth{TotalPublications: len(publications)}
	for _, p := range publications {
		switch p.State {
		case domain.PublicationReady:
			snapshot.ReadyPublications++
		case domain.PublicationFailed:
			snapshot.FailedPublications++
		case domain.PublicationDisabled:
			snapshot.DisabledPublications++
		}
	}
	return snapshot
}

func safeActor(actor string) string {
	if actor == "" {
		return "system"
	}
	return actor
}

var targetcliTokenPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
var targetcliSubtypePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validateTargetPublicationForRuntime(publication *domain.TargetPublication) error {
	if publication == nil {
		return domain.ErrInvalidInput
	}
	if _, err := normalizeTargetIQN(publication.TargetIQN, publication.DriveID); err != nil {
		return err
	}
	if strings.TrimSpace(publication.PublicationID) == "" || strings.TrimSpace(publication.PoolID) == "" {
		return domain.ErrInvalidInput
	}
	if !isSafeTargetcliToken(storageutil.SanitizeLayoutID(publication.PoolID)) {
		return domain.ErrInvalidInput
	}
	if !isSafeTargetcliToken(runtimeBackstoreName(publication)) {
		return domain.ErrInvalidInput
	}
	return nil
}

func validateTargetcliArgs(args ...string) error {
	if err := validateTargetcliArgTokens(args...); err != nil {
		return err
	}
	if len(args) < 2 {
		return domain.ErrInvalidInput
	}
	path, action := args[0], args[1]
	rest := args[2:]

	switch {
	case path == "sessions" && action == "detail":
		return requireNoTargetcliArgs(rest)
	case path == "/backstores" && action == "ls":
		return requireNoTargetcliArgs(rest)
	case path == "/backstores/fileio":
		return validateFileioTargetcliCommand(action, rest)
	case strings.HasPrefix(path, "/backstores/user:"):
		return validateUserBackstoreTargetcliCommand(path, action, rest)
	case path == "/iscsi":
		return validateISCSITargetcliCommand(action, rest)
	case strings.HasPrefix(path, "/iscsi/") && strings.HasSuffix(path, "/tpg1"):
		return validateTPGTargetcliCommand(path, action, rest)
	case strings.HasPrefix(path, "/iscsi/") && strings.HasSuffix(path, "/tpg1/luns"):
		return validateLUNTargetcliCommand(path, action, rest)
	default:
		return domain.ErrInvalidInput
	}
}

func validateTargetcliArgTokens(args ...string) error {
	for _, arg := range args {
		if strings.TrimSpace(arg) == "" || strings.ContainsRune(arg, '\x00') || strings.ContainsAny(arg, "\r\n") {
			return domain.ErrInvalidInput
		}
		if strings.Contains(arg, "..") {
			return domain.ErrInvalidInput
		}
	}
	return nil
}

func requireNoTargetcliArgs(args []string) error {
	if len(args) != 0 {
		return domain.ErrInvalidInput
	}
	return nil
}

func validateFileioTargetcliCommand(action string, args []string) error {
	switch action {
	case "create":
		if len(args) != 3 || !hasSafeAssignment(args[0], "name", isSafeTargetcliToken) ||
			!hasAbsolutePathAssignment(args[1], "file_or_dev") || !isSizeArg(args[2]) {
			return domain.ErrInvalidInput
		}
		return nil
	case "delete":
		if len(args) != 1 || !isSafeTargetcliToken(args[0]) {
			return domain.ErrInvalidInput
		}
		return nil
	default:
		return domain.ErrInvalidInput
	}
}

func validateUserBackstoreTargetcliCommand(path, action string, args []string) error {
	subtype := strings.TrimPrefix(path, "/backstores/user:")
	if !targetcliSubtypePattern.MatchString(subtype) {
		return domain.ErrInvalidInput
	}
	switch action {
	case "create":
		if len(args) != 3 || !hasSafeAssignment(args[0], "name", isSafeTargetcliToken) ||
			!isSizeArg(args[1]) || !hasAbsolutePathAssignment(args[2], "cfgstring") {
			return domain.ErrInvalidInput
		}
		return nil
	case "delete":
		if len(args) != 1 || !isSafeTargetcliToken(args[0]) {
			return domain.ErrInvalidInput
		}
		return nil
	default:
		return domain.ErrInvalidInput
	}
}

func validateISCSITargetcliCommand(action string, args []string) error {
	if len(args) != 1 || (action != "create" && action != "delete") {
		return domain.ErrInvalidInput
	}
	_, err := normalizeTargetIQN(args[0], "")
	return err
}

func validateTPGTargetcliCommand(path, action string, args []string) error {
	iqn, ok := iqnFromTPGPath(path, "/tpg1")
	if !ok {
		return domain.ErrInvalidInput
	}
	if _, err := normalizeTargetIQN(iqn, ""); err != nil {
		return err
	}
	if action != "set" || len(args) < 2 {
		return domain.ErrInvalidInput
	}
	switch args[0] {
	case "attribute":
		for _, item := range args[1:] {
			if !containsString(tcmuTargetcliTPGAttributes, item) {
				return domain.ErrInvalidInput
			}
		}
		return nil
	case "parameter":
		if len(args) != 2 {
			return domain.ErrInvalidInput
		}
		if containsString(tcmuISCSIDataPathParameters, args[1]) {
			return nil
		}
		return domain.ErrInvalidInput
	default:
		return domain.ErrInvalidInput
	}
}

func validateLUNTargetcliCommand(path, action string, args []string) error {
	iqn, ok := iqnFromTPGPath(path, "/tpg1/luns")
	if !ok {
		return domain.ErrInvalidInput
	}
	if _, err := normalizeTargetIQN(iqn, ""); err != nil {
		return err
	}
	if action != "create" || len(args) != 1 {
		return domain.ErrInvalidInput
	}
	return validateBackstoreRef(args[0])
}

func iqnFromTPGPath(path, suffix string) (string, bool) {
	if !strings.HasPrefix(path, "/iscsi/") || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	iqn := strings.TrimSuffix(strings.TrimPrefix(path, "/iscsi/"), suffix)
	iqn = strings.Trim(iqn, "/")
	return iqn, iqn != ""
}

func validateBackstoreRef(ref string) error {
	if strings.HasPrefix(ref, "/backstores/fileio/") {
		name := strings.TrimPrefix(ref, "/backstores/fileio/")
		if isSafeTargetcliToken(name) {
			return nil
		}
		return domain.ErrInvalidInput
	}
	if strings.HasPrefix(ref, "/backstores/user:") {
		parts := strings.Split(strings.TrimPrefix(ref, "/backstores/user:"), "/")
		if len(parts) == 2 && targetcliSubtypePattern.MatchString(parts[0]) && isSafeTargetcliToken(parts[1]) {
			return nil
		}
	}
	return domain.ErrInvalidInput
}

func hasSafeAssignment(arg, key string, validator func(string) bool) bool {
	value, ok := strings.CutPrefix(arg, key+"=")
	return ok && validator(value)
}

func hasAbsolutePathAssignment(arg, key string) bool {
	value, ok := strings.CutPrefix(arg, key+"=")
	return ok && strings.HasPrefix(value, "/") && !strings.Contains(value, "..") && !strings.ContainsAny(value, "\x00\r\n")
}

func isSizeArg(arg string) bool {
	value, ok := strings.CutPrefix(arg, "size=")
	if !ok || !strings.HasSuffix(value, "M") {
		return false
	}
	digits := strings.TrimSuffix(value, "M")
	if digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isSafeTargetcliToken(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && targetcliTokenPattern.MatchString(value)
}

var targetIQNPattern = regexp.MustCompile(`^iqn\.\d{4}-\d{2}\.[a-z0-9][a-z0-9.-]*:[a-z0-9][a-z0-9:.-]*$`)
var iqnValuePattern = regexp.MustCompile(`^iqn\.\d{4}-\d{2}\.[a-z0-9][a-z0-9.-]*:[a-z0-9][a-z0-9:._-]*$`)
var iqnInTextPattern = regexp.MustCompile(`iqn\.\d{4}-\d{2}\.[a-z0-9][a-z0-9.-]*:[a-z0-9][a-z0-9:._-]*`)

func normalizeTargetIQN(targetIQN, driveID string) (string, error) {
	iqn := strings.TrimSpace(strings.ToLower(targetIQN))
	if iqn == "" {
		token := sanitizeIQNToken(driveID)
		iqn = fmt.Sprintf("iqn.2026-04.cloud.backupnext.holo:%s", token)
	}

	if domain.ValidateTargetIQN(iqn) != nil || !targetIQNPattern.MatchString(iqn) {
		return "", domain.ErrInvalidInput
	}
	return iqn, nil
}

func sanitizeIQNToken(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	token := strings.Trim(b.String(), "-.")
	if token == "" {
		token = "drive"
	}
	return token
}

func newRuntimeID(prefix string) (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(entropy[:]), nil
}

const (
	defaultValidationBytes int64 = 4096
	maxValidationBytes     int64 = 1 << 20
)

func buildValidationPayload(req ValidationRunRequest) ([]byte, error) {
	switch req.Mode {
	case domain.ValidationModeFixed:
		size := req.Bytes
		if size == 0 {
			size = defaultValidationBytes
		}
		if size < 0 || size > maxValidationBytes {
			return nil, domain.ErrInvalidInput
		}
		pattern := []byte(req.Pattern)
		if len(pattern) == 0 {
			pattern = []byte("HOLO")
		}
		payload := make([]byte, int(size))
		for i := range payload {
			payload[i] = pattern[i%len(pattern)]
		}
		return payload, nil
	case domain.ValidationModeEmpty:
		if req.Bytes > 0 {
			return nil, domain.ErrInvalidInput
		}
		return []byte{}, nil
	default:
		return nil, domain.ErrInvalidInput
	}
}

func sha256Hex(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}
