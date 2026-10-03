package orchestration

import (
	"context"
	"errors"
	"fmt"
	"os"
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

type PublishRequest struct {
	LibraryID     string `json:"libraryId"`
	DriveID       string `json:"driveId"`
	CartridgeID   string `json:"cartridgeId"`
	TargetIQN     string `json:"targetIqn"`
	DeviceRole    string `json:"deviceRole,omitempty"`
	DeviceProfile string `json:"deviceProfile,omitempty"`
	DriveProfile  string `json:"driveProfile,omitempty"`
	Actor         string `json:"actor"`
	Auto          bool
}

type TargetRuntimeService struct {
	coreRepo       CoreResourceReader
	runtimeRepo    TargetRuntimeRepository
	adapter        TargetRuntimeAdapter
	auditW         audit.Writer
	cfg            TargetRuntimeConfig
	metrics        *metrics.MetricsRegistry
	storageWg      StorageWriteGuard
	poolReader     StoragePoolReader
	localMount     LocalMountSynchronizer
	localMountRepo LocalMountRepository
	deviceLocksMu  sync.Mutex
	deviceLocks    map[string]*sync.Mutex
	iscsiSecurity  *ISCSISecurityService
	// securityMu is the outer lock for iSCSI security mutations. When both
	// locks are needed, acquire securityMu before ISCSISecurityService.mu.
	// Never acquire securityMu while holding the security service mutex.
	securityMu sync.Mutex

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

func (s *TargetRuntimeService) SyncLocalMountAsync(actor string) {
	s.syncLocalMount(context.Background(), actor)
}

func (s *TargetRuntimeService) SetLocalMountRepository(repository LocalMountRepository) {
	s.localMountRepo = repository
	if adapter, ok := s.adapter.(*TcmuAdapter); ok {
		adapter.SetLocalMountRepository(repository)
	}
}

func (s *TargetRuntimeService) EnsureLocalMountBackend(ctx context.Context, descriptor domain.VTLDeviceDescriptor) error {
	if s.cfg.Mode == "in-memory" {
		return nil
	}
	if adapter, ok := s.adapter.(*TcmuAdapter); ok {
		return adapter.EnsureLocalMountBackend(ctx, descriptor)
	}
	return ErrLocalLoopbackHelperUnavailable
}

func (s *TargetRuntimeService) ResolveLocalMountIdentity(ctx context.Context, descriptor domain.VTLDeviceDescriptor, publication *domain.TargetPublication) (string, error) {
	if s.cfg.Mode == "in-memory" {
		return descriptor.IdentityRef, nil
	}
	if adapter, ok := s.adapter.(*TcmuAdapter); ok {
		return adapter.ResolveLocalMountIdentity(ctx, descriptor, publication)
	}
	return "", ErrLocalLoopbackHelperUnavailable
}

func (s *TargetRuntimeService) WithLocalMountDeviceLocks(ctx context.Context, deviceKeys []string, operation func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	keys := append([]string(nil), deviceKeys...)
	sort.Strings(keys)
	unique := keys[:0]
	for _, key := range keys {
		if strings.TrimSpace(key) == "" || len(unique) > 0 && unique[len(unique)-1] == key {
			continue
		}
		unique = append(unique, key)
	}
	locks := make([]*sync.Mutex, len(unique))
	s.deviceLocksMu.Lock()
	if s.deviceLocks == nil {
		s.deviceLocks = make(map[string]*sync.Mutex)
	}
	for i, key := range unique {
		lock := s.deviceLocks[key]
		if lock == nil {
			lock = &sync.Mutex{}
			s.deviceLocks[key] = lock
		}
		locks[i] = lock
	}
	s.deviceLocksMu.Unlock()
	for _, lock := range locks {
		lock.Lock()
	}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}()
	return operation()
}

func (s *TargetRuntimeService) ReleaseLocalMountBackend(ctx context.Context, mapping domain.LocalLoopbackDeviceMapping) error {
	return s.WithLocalMountDeviceLocks(ctx, []string{mapping.DeviceKey}, func() error {
		for _, publication := range s.runtimeRepo.ListPublications(ctx) {
			if publication != nil && publication.State != domain.PublicationDisabled && publicationDeviceKey(publication) == mapping.DeviceKey {
				return nil
			}
		}
		if s.cfg.Mode == "in-memory" {
			return nil
		}
		if adapter, ok := s.adapter.(*TcmuAdapter); ok {
			return adapter.ReleaseLocalMountBackend(ctx, mapping)
		}
		return ErrLocalLoopbackHelperUnavailable
	})
}

func (s *TargetRuntimeService) SetISCSISecurityService(service *ISCSISecurityService) {
	s.iscsiSecurity = service
}

func (s *TargetRuntimeService) WithISCSISecurityLock(_ context.Context, operation func() error) error {
	s.securityMu.Lock()
	defer s.securityMu.Unlock()
	return operation()
}

func (s *TargetRuntimeService) TargetRuntimeAbsent(ctx context.Context, targetIQN string) (bool, error) {
	publication, found := s.runtimeRepo.FindPublicationByIQN(ctx, targetIQN)
	if found && publication != nil && publication.State != domain.PublicationDisabled {
		return false, nil
	}
	if s.cfg.Mode == "in-memory" {
		return true, nil
	}
	if domain.ValidateTargetIQN(targetIQN) != nil {
		return false, domain.ErrInvalidInput
	}
	if _, err := os.Stat(s.cfg.IscsiConfigfsRoot); err != nil {
		return false, err
	}
	_, err := os.Stat(filepath.Join(s.cfg.IscsiConfigfsRoot, targetIQN))
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func (s *TargetRuntimeService) Publish(ctx context.Context, req PublishRequest) (*domain.TargetPublication, error) {
	s.securityMu.Lock()
	defer s.securityMu.Unlock()
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
	securityContext := ISCSIResolvedPublicationSecurity{}
	if s.iscsiSecurity != nil {
		var err error
		securityContext, err = s.iscsiSecurity.ResolvePublicationUnderRuntimeLock(ctx, publication, req.Auto)
		if err != nil {
			return nil, err
		}
	}
	publication.SecurityEnforcement = pendingSecurityEnforcement(s.cfg.Mode, securityContext.Policy)
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

	portal := ""
	err = s.WithLocalMountDeviceLocks(ctx, []string{publicationDeviceKey(publication)}, func() error {
		var publishErr error
		portal, publishErr = s.publishWithSecurity(ctx, publication, securityContext)
		if publishErr != nil {
			return publishErr
		}
		if markErr := publication.MarkReady(portal); markErr != nil {
			return markErr
		}
		if s.iscsiSecurity != nil && resolvedPolicyRequiresProtection(securityContext.Policy) {
			publication.SecurityEnforcement = successfulSecurityEnforcement(s.cfg.Mode)
		}
		return s.runtimeRepo.SavePublication(ctx, publication)
	})
	if err != nil {
		if publication.State != domain.PublicationCreating {
			return publication, err
		}
		if s.metrics != nil && resolvedPolicyRequiresProtection(securityContext.Policy) {
			s.metrics.RecordISCSISecurityApplyFailure()
		}
		_ = publication.MarkFailed(err.Error())
		if saveErr := s.runtimeRepo.SavePublication(ctx, publication); saveErr != nil {
			return nil, saveErr
		}
		if s.iscsiSecurity != nil {
			if offlineErr := s.iscsiSecurity.SetTargetOfflineUnderRuntimeLock(ctx, publication.TargetIQN, true, req.Actor); offlineErr != nil {
				return nil, errors.Join(err, offlineErr)
			}
		}
		if s.iscsiSecurity != nil && resolvedPolicyRequiresProtection(securityContext.Policy) {
			publication.SecurityEnforcement = domain.SecurityEnforcementBlocked
		}
		audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(req.Actor), "publish", publication.PublicationID, "failure", map[string]any{"error": err.Error(), "runtimeMode": s.cfg.Mode})
		return publication, err
	}

	audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(req.Actor), "publish", publication.PublicationID, "success", map[string]any{"targetIqn": publication.TargetIQN, "portal": publication.Portal, "runtimeMode": s.cfg.Mode})

	if s.metrics != nil {
		s.metrics.RecordPublicationPublish()
	}
	s.syncLocalMount(ctx, req.Actor)

	return publication, nil
}

func (s *TargetRuntimeService) Unpublish(ctx context.Context, publicationID, actor string) (*domain.TargetPublication, error) {
	s.securityMu.Lock()
	defer s.securityMu.Unlock()
	publication, err := s.runtimeRepo.FindPublication(ctx, publicationID)
	if err != nil {
		return nil, err
	}
	if publication.State == domain.PublicationDisabled {
		audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "unpublish", publicationID, "success", map[string]any{"noop": true, "runtimeMode": s.cfg.Mode})
		return publication, nil
	}
	err = s.WithLocalMountDeviceLocks(ctx, []string{publicationDeviceKey(publication)}, func() error {
		if s.iscsiSecurity != nil {
			if err := s.iscsiSecurity.SetTargetOfflineUnderRuntimeLock(ctx, publication.TargetIQN, true, actor); err != nil {
				return err
			}
		}
		if err := s.adapter.Unpublish(ctx, publication); err != nil {
			return err
		}
		if err := publication.Disable(); err != nil {
			return err
		}
		if publication.SecurityEnforcement != domain.SecurityEnforcementUnprotected {
			publication.SecurityEnforcement = domain.SecurityEnforcementOffline
		}
		return s.runtimeRepo.SavePublication(ctx, publication)
	})
	if err != nil {
		audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "unpublish", publicationID, "failure", map[string]any{"error": err.Error(), "runtimeMode": s.cfg.Mode})
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
	s.securityMu.Lock()
	defer s.securityMu.Unlock()
	publication, err := s.runtimeRepo.FindPublication(ctx, publicationID)
	if err != nil {
		return nil, err
	}
	if publication.State == domain.PublicationDisabled {
		audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "rollback", publicationID, "success", map[string]any{"noop": true})
		return publication, nil
	}
	if s.iscsiSecurity != nil {
		if err := s.iscsiSecurity.SetTargetOfflineUnderRuntimeLock(ctx, publication.TargetIQN, true, actor); err != nil {
			return nil, err
		}
		if err := s.adapter.Unpublish(ctx, publication); err != nil {
			return nil, err
		}
	}
	if publication.State == domain.PublicationCreating {
		if err := publication.MarkFailed("rollback from creating state"); err != nil {
			return nil, err
		}
	}
	if err := publication.Disable(); err != nil {
		return nil, err
	}
	if publication.SecurityEnforcement != domain.SecurityEnforcementUnprotected {
		publication.SecurityEnforcement = domain.SecurityEnforcementOffline
	}
	if err := s.runtimeRepo.SavePublication(ctx, publication); err != nil {
		return nil, err
	}
	audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "rollback", publicationID, "success", map[string]any{"state": publication.State})
	return publication, nil
}

func (s *TargetRuntimeService) RestoreReadyPublications(ctx context.Context) error {
	if lifecycle, ok := s.localMount.(interface {
		PrepareRestart(context.Context, string) error
	}); ok {
		if err := lifecycle.PrepareRestart(ctx, "system"); err != nil {
			return fmt.Errorf("%w: %w", ErrLocalMountRestartCleanup, err)
		}
	}
	s.securityMu.Lock()
	defer s.securityMu.Unlock()
	publications := s.runtimeRepo.ListPublications(ctx)
	var firstErr error
	for _, publication := range publications {
		if publication.State != domain.PublicationReady {
			continue
		}
		securityContext := ISCSIResolvedPublicationSecurity{}
		var err error
		if s.iscsiSecurity != nil {
			securityContext, err = s.iscsiSecurity.ResolvePublicationUnderRuntimeLock(ctx, publication, true)
			if err != nil {
				publication.SecurityEnforcement = domain.SecurityEnforcementBlocked
				if cleanupErr := s.adapter.Unpublish(ctx, publication); cleanupErr != nil {
					publication.MarkRuntimeFailed("iSCSI security unavailable; target cleanup is unverified")
					if firstErr == nil {
						firstErr = ErrISCSISecurityRuntimeUnknown
					}
				} else if errors.Is(err, ErrISCSISecurityBusy) {
					_ = publication.Disable()
					publication.SecurityEnforcement = domain.SecurityEnforcementOffline
				} else {
					publication.MarkRuntimeFailed("iSCSI security prerequisites are unavailable")
				}
				if saveErr := s.runtimeRepo.SavePublication(ctx, publication); saveErr != nil && firstErr == nil {
					firstErr = saveErr
				}
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
		}
		if err := s.adapter.Unpublish(ctx, publication); err != nil {
			publication.MarkRuntimeFailed("target cleanup before restore is unverified")
			publication.SecurityEnforcement = domain.SecurityEnforcementBlocked
			if saveErr := s.runtimeRepo.SavePublication(ctx, publication); saveErr != nil && firstErr == nil {
				firstErr = saveErr
			}
			if firstErr == nil {
				firstErr = ErrISCSISecurityRuntimeUnknown
			}
			continue
		}
		portal, err := s.publishWithSecurity(ctx, publication, securityContext)
		if err != nil {
			publication.MarkRuntimeFailed(err.Error())
			if s.iscsiSecurity != nil && resolvedPolicyRequiresProtection(securityContext.Policy) {
				publication.SecurityEnforcement = domain.SecurityEnforcementBlocked
			}
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
		publication.SecurityEnforcement = successfulSecurityEnforcement(s.cfg.Mode)
		if s.iscsiSecurity == nil || !resolvedPolicyRequiresProtection(securityContext.Policy) {
			publication.SecurityEnforcement = domain.SecurityEnforcementUnprotected
		}
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

func (s *TargetRuntimeService) publishWithSecurity(ctx context.Context, publication *domain.TargetPublication, security ISCSIResolvedPublicationSecurity) (string, error) {
	if s.iscsiSecurity == nil || !resolvedPolicyRequiresProtection(security.Policy) {
		return s.adapter.Publish(ctx, publication)
	}
	if s.cfg.Mode == "in-memory" {
		return s.adapter.Publish(ctx, publication)
	}
	protectedAdapter, ok := s.adapter.(ProtectedTargetRuntimeAdapter)
	if !ok {
		return "", ErrISCSISecurityHelperUnavailable
	}
	return protectedAdapter.PublishProtected(ctx, publication, security)
}

func pendingSecurityEnforcement(mode string, policy domain.ResolvedISCSISecurity) string {
	if !resolvedPolicyRequiresProtection(policy) {
		return domain.SecurityEnforcementUnprotected
	}
	if mode == "in-memory" {
		return domain.SecurityEnforcementSimulated
	}
	return domain.SecurityEnforcementBlocked
}

func successfulSecurityEnforcement(mode string) string {
	if mode == "in-memory" {
		return domain.SecurityEnforcementSimulated
	}
	return domain.SecurityEnforcementEnforcing
}

func (s *TargetRuntimeService) Shutdown(ctx context.Context) error {
	if s == nil || s.runtimeRepo == nil || s.adapter == nil {
		return nil
	}
	if lifecycle, ok := s.localMount.(interface {
		Stop(context.Context, string) error
	}); ok {
		if err := lifecycle.Stop(ctx, "system"); err != nil {
			return fmt.Errorf("stop local mappings before target shutdown: %w", err)
		}
	}
	s.securityMu.Lock()
	defer s.securityMu.Unlock()
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

func (s *TargetRuntimeService) ListPublications(ctx context.Context) []*domain.TargetPublication {
	return s.runtimeRepo.ListPublications(ctx)
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
	return audit.NormalizeServiceActor(actor)
}

var targetcliTokenPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
var targetcliSubtypePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
