package orchestration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

type LocalMountSettingsRepository interface {
	Enabled(ctx context.Context) (bool, error)
	SetEnabled(ctx context.Context, enabled bool) error
}

type LocalMountRepository interface {
	SaveLibraryMapping(context.Context, domain.LocalLoopbackLibraryMapping) error
	ListLibraryMappings(context.Context) ([]domain.LocalLoopbackLibraryMapping, error)
	DeleteLibraryMapping(context.Context, string) error
	SaveDeviceMapping(context.Context, domain.LocalLoopbackDeviceMapping) error
	ListDeviceMappings(context.Context, string) ([]domain.LocalLoopbackDeviceMapping, error)
	MarkDeviceCleanupPending(context.Context, string) error
	MarkDeviceActive(context.Context, string) error
	MarkDeviceInactive(context.Context, string) error
	DeleteDeviceMapping(context.Context, string) error
}

type LocalMountRuntime interface {
	Probe(context.Context) (bool, domain.LocalMountReasonCode, error)
	ListOwned(context.Context) ([]LocalLoopbackOwner, error)
	Ensure(context.Context, domain.LocalLoopbackLibraryMapping, []domain.LocalLoopbackDeviceMapping, []domain.VTLDeviceDescriptor) ([]LocalLoopbackDeviceObservation, error)
	Remove(context.Context, domain.LocalLoopbackLibraryMapping, []domain.LocalLoopbackDeviceMapping) ([]LocalLoopbackDeviceObservation, error)
	ReleaseBackend(context.Context, domain.LocalLoopbackDeviceMapping) error
}

type LocalMountStatus = domain.LocalMountStatus

var ErrLocalMountRestartCleanup = errors.New("local mappings could not be safely detached before target restore")

type LocalMountService struct {
	settings    LocalMountSettingsRepository
	targets     TargetRuntimeRepository
	resources   LocalMountInventoryRepository
	pools       LocalMountPoolReader
	mappings    LocalMountRepository
	runtime     LocalMountRuntime
	auditW      audit.Writer
	syncMu      sync.Mutex
	lastMu      sync.RWMutex
	last        LocalMountStatus
	asyncMu     sync.Mutex
	asyncRun    bool
	asyncNext   bool
	asyncActor  string
	stopped     bool
	asyncCancel context.CancelFunc
	asyncDone   chan struct{}
}

func NewLocalMountService(settings LocalMountSettingsRepository, targets TargetRuntimeRepository, auditW audit.Writer, _ TargetRuntimeConfig) *LocalMountService {
	return &LocalMountService{settings: settings, targets: targets, auditW: auditW, last: emptyLocalMountStatus()}
}

func (s *LocalMountService) SetRuntime(resources LocalMountInventoryRepository, pools LocalMountPoolReader, mappings LocalMountRepository, runtime LocalMountRuntime) {
	s.resources = resources
	s.pools = pools
	s.mappings = mappings
	s.runtime = runtime
}

func (s *LocalMountService) SetTargetRuntimeRepository(targets TargetRuntimeRepository) {
	s.targets = targets
}

func (s *LocalMountService) Status(ctx context.Context) (LocalMountStatus, error) {
	s.asyncMu.Lock()
	defer s.asyncMu.Unlock()
	enabled, err := s.settings.Enabled(ctx)
	if err != nil {
		return LocalMountStatus{}, err
	}
	status := s.getLast()
	if enabled && (!status.Enabled || s.asyncNext) {
		status.State = domain.LocalMountStateConnecting
	}
	if !enabled && (status.Enabled || s.asyncNext) {
		status.State = domain.LocalMountStateDisconnecting
	}
	status.Enabled = enabled
	if !enabled && status.State == "" {
		status.State = domain.LocalMountStateDisabled
	}
	return status, nil
}

func (s *LocalMountService) SetEnabled(ctx context.Context, enabled bool, actor string) (LocalMountStatus, error) {
	s.asyncMu.Lock()
	if s.stopped {
		s.asyncMu.Unlock()
		return LocalMountStatus{}, domain.ErrInvalidState
	}
	if err := s.settings.SetEnabled(ctx, enabled); err != nil {
		s.asyncMu.Unlock()
		return LocalMountStatus{}, err
	}
	status := emptyLocalMountStatus()
	status.Enabled = enabled
	if enabled {
		status.State = domain.LocalMountStateConnecting
	} else {
		status.State = domain.LocalMountStateDisconnecting
	}
	s.setLast(status)
	s.scheduleSyncLocked(actor)
	s.asyncMu.Unlock()
	audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "local_mount_setting_changed", "local", "success", map[string]any{"enabled": enabled})
	return status, nil
}

func (s *LocalMountService) Sync(ctx context.Context, actor string) (LocalMountStatus, error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	s.asyncMu.Lock()
	stopped := s.stopped
	s.asyncMu.Unlock()
	if stopped {
		return s.getLast(), domain.ErrInvalidState
	}
	enabled, err := s.settings.Enabled(ctx)
	if err != nil {
		return s.failSync(ctx, actor, s.getLast().Enabled, err)
	}
	if s.resources == nil || s.mappings == nil || s.runtime == nil {
		return s.failSync(ctx, actor, enabled, ErrLocalLoopbackHelperUnavailable)
	}
	if !enabled {
		return s.detach(ctx, actor, false)
	}

	available, probeReason, probeErr := s.runtime.Probe(ctx)
	if probeErr != nil || !available {
		status := emptyLocalMountStatus()
		status.Enabled = true
		status.State = domain.LocalMountStateFailed
		status.LastSyncAt = timePointer(time.Now().UTC())
		if probeReason == "" {
			probeReason = domain.LocalMountReasonLoopbackUnavailable
		}
		status.LastError = string(probeReason)
		s.setLast(status)
		s.emitSyncAudit(ctx, actor, status, probeErr)
		if probeErr != nil {
			return status, probeErr
		}
		return status, ErrLocalLoopbackHelperUnavailable
	}

	descriptors, err := BuildLocalMountInventory(ctx, s.resources, s.pools)
	if err != nil {
		return s.failSync(ctx, actor, enabled, err)
	}
	publications := map[string]*domain.TargetPublication{}
	// A network publication already owns the canonical handler/backstore. Reuse
	// that exact object so a local loopback path cannot fork the tape state.
	for _, publication := range s.targetPublications(ctx) {
		if publication == nil || publication.State != domain.PublicationReady {
			continue
		}
		key := publicationDeviceKey(publication)
		if key != "" && publications[key] == nil {
			publications[key] = publication
		}
	}

	byLibrary := groupDescriptorsByLibrary(descriptors)
	var firstErr error
	identityFailures := make(map[string]error)
	if resolver, ok := s.runtime.(interface {
		ResolveLocalMountIdentity(context.Context, domain.VTLDeviceDescriptor, *domain.TargetPublication) (string, error)
	}); ok {
		resolvedByKey := make(map[string]string, len(descriptors))
		for _, libraryID := range sortedDescriptorLibraries(byLibrary) {
			for i := range byLibrary[libraryID] {
				descriptor := byLibrary[libraryID][i]
				identity, resolveErr := resolver.ResolveLocalMountIdentity(ctx, descriptor, publications[descriptor.DeviceKey])
				if resolveErr != nil {
					identityFailures[libraryID] = resolveErr
					if firstErr == nil {
						firstErr = resolveErr
					}
					break
				}
				byLibrary[libraryID][i].IdentityRef = identity
				resolvedByKey[descriptor.DeviceKey] = identity
			}
		}
		for i := range descriptors {
			if identity, ok := resolvedByKey[descriptors[i].DeviceKey]; ok {
				descriptors[i].IdentityRef = identity
			}
		}
	}
	observations := make(map[string]LocalLoopbackDeviceObservation, len(descriptors))
	for _, libraryID := range sortedDescriptorLibraries(byLibrary) {
		if identityFailures[libraryID] != nil {
			continue
		}
		libraryMapping := localLoopbackLibraryMapping(libraryID)
		if err := s.mappings.SaveLibraryMapping(ctx, libraryMapping); err != nil && firstErr == nil {
			firstErr = err
			continue
		}
		existing, listErr := s.mappings.ListDeviceMappings(ctx, libraryID)
		if listErr != nil {
			if firstErr == nil {
				firstErr = listErr
			}
			continue
		}
		current := byLibrary[libraryID]
		if localMountIdentityChanged(current, existing) {
			cleanupMappings := append([]domain.LocalLoopbackDeviceMapping(nil), existing...)
			cleanupOK := true
			for i := range cleanupMappings {
				if cleanupMappings[i].State != domain.LocalMappingStateCleanupPending {
					if err := s.mappings.MarkDeviceCleanupPending(ctx, cleanupMappings[i].DeviceKey); err != nil {
						if firstErr == nil {
							firstErr = err
						}
						cleanupOK = false
						break
					}
					cleanupMappings[i].State = domain.LocalMappingStateCleanupPending
				}
			}
			if !cleanupOK {
				continue
			}
			if len(cleanupMappings) > 0 {
				if _, removeErr := s.runtime.Remove(ctx, libraryMapping, cleanupMappings); removeErr != nil {
					if firstErr == nil {
						firstErr = removeErr
					}
					continue
				}
			}
			ownersAfterRemove, listErr := s.runtime.ListOwned(ctx)
			if listErr != nil {
				if firstErr == nil {
					firstErr = listErr
				}
				continue
			}
			stillOwned := false
			for _, owner := range ownersAfterRemove {
				if owner.LibraryID == libraryID && owner.Present {
					stillOwned = true
					break
				}
			}
			if stillOwned {
				if firstErr == nil {
					firstErr = LocalLoopbackHelperError{ReasonCode: string(domain.LocalMountReasonCleanupFailed)}
				}
				continue
			}
			for _, mapping := range cleanupMappings {
				if err := s.runtime.ReleaseBackend(ctx, mapping); err != nil {
					if firstErr == nil {
						firstErr = err
					}
					cleanupOK = false
					break
				}
				if err := s.mappings.DeleteDeviceMapping(ctx, mapping.DeviceKey); err != nil {
					if firstErr == nil {
						firstErr = err
					}
					cleanupOK = false
					break
				}
			}
			if !cleanupOK {
				continue
			}
			existing = nil
		}
		wanted, stale := desiredMappings(libraryID, current, existing, publications)
		for _, mapping := range wanted {
			if err := s.mappings.SaveDeviceMapping(ctx, mapping); err != nil {
				// Existing stable identity/LUN mappings may be returning from a
				// completed cleanup. State is intent; the loopback helper below
				// revalidates the actual mapping before success is reported.
				if errors.Is(err, domain.ErrConflict) {
					if activeErr := s.mappings.MarkDeviceActive(ctx, mapping.DeviceKey); activeErr == nil {
						continue
					}
				}
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		resolvedDescriptors := make([]domain.VTLDeviceDescriptor, len(current))
		copy(resolvedDescriptors, current)
		mappingByKey := make(map[string]domain.LocalLoopbackDeviceMapping, len(wanted))
		for _, mapping := range wanted {
			mappingByKey[mapping.DeviceKey] = mapping
		}
		for i := range resolvedDescriptors {
			if mapping, ok := mappingByKey[resolvedDescriptors[i].DeviceKey]; ok {
				resolvedDescriptors[i].BackendRef = mapping.BackendRef
				resolvedDescriptors[i].IdentityRef = mapping.IdentityRef
			}
		}
		for _, mapping := range stale {
			if err := s.mappings.MarkDeviceCleanupPending(ctx, mapping.DeviceKey); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		active := filterMappingState(wanted, domain.LocalMappingStateActive)
		deviceObservations, ensureErr := s.runtime.Ensure(ctx, libraryMapping, active, resolvedDescriptors)
		if ensureErr != nil && firstErr == nil {
			firstErr = ensureErr
		}
		for _, observation := range deviceObservations {
			observations[observation.DeviceKey] = observation
		}
		if len(stale) > 0 {
			cleanupMappings := append(append([]domain.LocalLoopbackDeviceMapping(nil), active...), stale...)
			cleanupObservations, removeErr := s.runtime.Remove(ctx, libraryMapping, cleanupMappings)
			if removeErr != nil && firstErr == nil {
				firstErr = removeErr
			}
			for _, observation := range cleanupObservations {
				observations[observation.DeviceKey] = observation
			}
			if removeErr == nil {
				remainingOwners, listErr := s.runtime.ListOwned(ctx)
				if listErr != nil && firstErr == nil {
					firstErr = listErr
				}
				remaining := ownedDeviceKeys(remainingOwners, libraryID)
				for _, mapping := range stale {
					if remaining[mapping.DeviceKey] {
						if firstErr == nil {
							firstErr = LocalLoopbackHelperError{ReasonCode: string(domain.LocalMountReasonCleanupFailed)}
						}
						continue
					}
					if err := s.runtime.ReleaseBackend(ctx, mapping); err != nil && firstErr == nil {
						firstErr = err
					}
					if err := s.mappings.DeleteDeviceMapping(ctx, mapping.DeviceKey); err != nil && firstErr == nil {
						firstErr = err
					}
				}
			}
		}
	}

	status := aggregateLocalMountStatus(true, descriptors, observations, nil, firstErr)
	status.LastSyncAt = timePointer(time.Now().UTC())
	s.setLast(status)
	s.emitSyncAudit(ctx, actor, status, firstErr)
	return status, firstErr
}

func (s *LocalMountService) detach(ctx context.Context, actor string, enabled bool) (LocalMountStatus, error) {
	owners, ownerErr := s.runtime.ListOwned(ctx)
	if ownerErr != nil {
		// Unknown kernel ownership cannot authorize releasing a shared backend.
		return s.failSync(ctx, actor, enabled, ownerErr)
	}
	libraries, mappingErr := s.mappings.ListLibraryMappings(ctx)
	status := emptyLocalMountStatus()
	status.Enabled = enabled
	status.LastSyncAt = timePointer(time.Now().UTC())
	var firstErr error
	if mappingErr != nil && firstErr == nil {
		firstErr = mappingErr
	}
	ownerByLibrary := make(map[string]LocalLoopbackOwner, len(owners))
	for _, owner := range owners {
		ownerByLibrary[owner.LibraryID] = owner
	}
	knownLibraries := make(map[string]bool, len(libraries))
	for _, library := range libraries {
		knownLibraries[library.LibraryID] = true
		devices, err := s.mappings.ListDeviceMappings(ctx, library.LibraryID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for i := range devices {
			if devices[i].State != domain.LocalMappingStateCleanupPending {
				if err := s.mappings.MarkDeviceCleanupPending(ctx, devices[i].DeviceKey); err != nil && firstErr == nil {
					firstErr = err
				}
				devices[i].State = domain.LocalMappingStateCleanupPending
			}
		}
		owner := ownerByLibrary[library.LibraryID]
		if owner.Present {
			observed, removeErr := s.runtime.Remove(ctx, library, devices)
			if removeErr != nil {
				if firstErr == nil {
					firstErr = removeErr
				}
				status.ResidualDeviceCount += countResidual(observed)
				continue
			}
		}
		if owner.Present {
			ownersAfterRemove, listErr := s.runtime.ListOwned(ctx)
			if listErr != nil {
				if firstErr == nil {
					firstErr = listErr
				}
			} else {
				owner.Present = false
				for _, current := range ownersAfterRemove {
					if current.LibraryID == library.LibraryID && current.Present {
						owner.Present = true
						break
					}
				}
			}
		}
		if owner.Present {
			// The helper's owner record is authoritative. Do not claim a clean
			// disconnect until a subsequent observation confirms it disappeared.
			status.ResidualDeviceCount += len(devices)
			if firstErr == nil {
				firstErr = LocalLoopbackHelperError{ReasonCode: string(domain.LocalMountReasonCleanupFailed)}
			}
			continue
		}
		for _, device := range devices {
			if err := s.runtime.ReleaseBackend(ctx, device); err != nil && firstErr == nil {
				firstErr = err
			}
			if err := s.mappings.MarkDeviceInactive(ctx, device.DeviceKey); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	for _, owner := range owners {
		if owner.Present && !knownLibraries[owner.LibraryID] {
			status.ResidualDeviceCount += len(owner.Devices)
			if firstErr == nil {
				firstErr = LocalLoopbackHelperError{ReasonCode: string(domain.LocalMountReasonCleanupFailed)}
			}
		}
	}
	if firstErr != nil {
		status.State = domain.LocalMountStateFailed
		status.LastError = safeLocalMountError(firstErr)
	} else {
		status.State = domain.LocalMountStateDisabled
		if enabled {
			status.State = domain.LocalMountStateConnecting
		}
	}
	s.setLast(status)
	s.emitSyncAudit(ctx, actor, status, firstErr)
	return status, firstErr
}

func (s *LocalMountService) targetPublications(ctx context.Context) []*domain.TargetPublication {
	if targets, ok := s.targets.(interface {
		ListPublications(context.Context) []*domain.TargetPublication
	}); ok {
		return targets.ListPublications(ctx)
	}
	return nil
}

func (s *LocalMountService) SyncAsync(actor string) {
	s.asyncMu.Lock()
	defer s.asyncMu.Unlock()
	s.scheduleSyncLocked(actor)
}

// PrepareRestart detaches kernel references before shared backstores are rebuilt.
// The saved intent and device identities remain available for the next sync.
func (s *LocalMountService) PrepareRestart(ctx context.Context, actor string) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	enabled, err := s.settings.Enabled(ctx)
	if err != nil {
		return err
	}
	if s.mappings == nil || s.runtime == nil {
		_, err := s.failSync(ctx, actor, enabled, ErrLocalLoopbackHelperUnavailable)
		return err
	}
	_, err = s.detach(ctx, actor, enabled)
	return err
}

// Stop prevents late requests from recreating mappings during network teardown.
func (s *LocalMountService) Stop(ctx context.Context, actor string) error {
	s.asyncMu.Lock()
	s.stopped = true
	s.asyncNext = false
	if s.asyncCancel != nil {
		s.asyncCancel()
	}
	done := s.asyncDone
	s.asyncMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.PrepareRestart(ctx, actor)
}

func (s *LocalMountService) scheduleSyncLocked(actor string) {
	if s.stopped {
		return
	}
	if s.asyncRun {
		s.asyncNext = true
		s.asyncActor = actor
		return
	}
	s.asyncRun = true
	s.asyncActor = actor
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.asyncCancel = cancel
	s.asyncDone = done
	go func() {
		defer close(done)
		defer cancel()
		s.runAsyncSync(ctx, actor)
	}()
}

func (s *LocalMountService) runAsyncSync(parent context.Context, actor string) {
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	defer func() { cancel() }()
	for {
		status, err := s.Sync(ctx, actor)
		if err == nil && status.State == domain.LocalMountStateConnecting {
			// Kernel device enumeration can lag behind a successful mapping.
			// Confirm it without accepting concurrent loopback operations.
			timer := time.NewTimer(2500 * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
			timer.Stop()
		}
		s.asyncMu.Lock()
		if s.asyncNext {
			actor = s.asyncActor
			s.asyncNext = false
			cancel()
			ctx, cancel = context.WithTimeout(parent, time.Minute)
			if enabled, readErr := s.settings.Enabled(ctx); readErr == nil {
				status.Enabled = enabled
				status.State = domain.LocalMountStateConnecting
				if !enabled {
					status.State = domain.LocalMountStateDisconnecting
				}
				status.LastError = ""
				s.setLast(status)
			}
			s.asyncMu.Unlock()
			continue
		}
		if err == nil && status.State == domain.LocalMountStateConnecting {
			if ctx.Err() == nil {
				s.asyncMu.Unlock()
				continue
			}
			status.State = domain.LocalMountStateFailed
			if status.ConnectedDeviceCount > 0 {
				status.State = domain.LocalMountStatePartial
			}
			status.LastError = string(domain.LocalMountReasonOperationTimeout)
			for i := range status.Devices {
				if status.Devices[i].State == domain.LocalMountDeviceStatePending || status.Devices[i].State == domain.LocalMountDeviceStateRemoving {
					status.Devices[i].State = domain.LocalMountDeviceStateFailed
					status.Devices[i].ReasonCode = domain.LocalMountReasonOperationTimeout
				}
			}
			status.LastSyncAt = timePointer(time.Now().UTC())
			s.setLast(status)
			s.emitSyncAudit(context.Background(), actor, status, context.DeadlineExceeded)
		}
		s.asyncRun = false
		s.asyncMu.Unlock()
		return
	}
}

func (s *LocalMountService) failSync(ctx context.Context, actor string, enabled bool, err error) (LocalMountStatus, error) {
	status := s.getLast()
	status.Enabled = enabled
	status.State = domain.LocalMountStateFailed
	status.LastError = safeLocalMountError(err)
	status.LastSyncAt = timePointer(time.Now().UTC())
	s.setLast(status)
	s.emitSyncAudit(ctx, actor, status, err)
	return status, err
}

func (s *LocalMountService) getLast() LocalMountStatus {
	s.lastMu.RLock()
	defer s.lastMu.RUnlock()
	return cloneLocalMountStatus(s.last)
}

func (s *LocalMountService) setLast(status LocalMountStatus) {
	s.lastMu.Lock()
	defer s.lastMu.Unlock()
	s.last = cloneLocalMountStatus(status)
}

func (s *LocalMountService) emitSyncAudit(ctx context.Context, actor string, status LocalMountStatus, err error) {
	result := "success"
	details := map[string]any{"enabled": status.Enabled, "desired": status.DesiredDeviceCount, "connected": status.ConnectedDeviceCount, "residual": status.ResidualDeviceCount, "state": status.State}
	if err != nil {
		result = "failure"
		details["reason"] = safeLocalMountError(err)
	}
	audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "local_mount_sync", "local", result, details)
}

func emptyLocalMountStatus() LocalMountStatus {
	return LocalMountStatus{
		State:          domain.LocalMountStateDisabled,
		Devices:        []domain.LocalMountDeviceStatus{},
		DesiredIQNs:    []string{},
		MountedIQNs:    []string{},
		SkippedTargets: []string{},
	}
}

func localLoopbackLibraryMapping(libraryID string) domain.LocalLoopbackLibraryMapping {
	return domain.LocalLoopbackLibraryMapping{
		LibraryID: libraryID,
		TargetNAA: localMountNAA("target", libraryID),
		NexusNAA:  localMountNAA("nexus", libraryID),
		TPGTag:    1,
	}
}

func localMountNAA(role, libraryID string) string {
	hash := sha256.Sum256([]byte(role + ":" + libraryID))
	return "naa.5" + hex.EncodeToString(hash[:8])[:15]
}

func desiredMappings(libraryID string, descriptors []domain.VTLDeviceDescriptor, existing []domain.LocalLoopbackDeviceMapping, publications map[string]*domain.TargetPublication) ([]domain.LocalLoopbackDeviceMapping, []domain.LocalLoopbackDeviceMapping) {
	existingByKey := make(map[string]domain.LocalLoopbackDeviceMapping, len(existing))
	usedLUNs := map[int]bool{0: true}
	for _, mapping := range existing {
		existingByKey[mapping.DeviceKey] = mapping
		usedLUNs[mapping.LUNIndex] = true
	}
	wantedKeys := make(map[string]bool, len(descriptors))
	wanted := make([]domain.LocalLoopbackDeviceMapping, 0, len(descriptors))
	nextLUN := 1
	for _, descriptor := range descriptors {
		wantedKeys[descriptor.DeviceKey] = true
		mapping, exists := existingByKey[descriptor.DeviceKey]
		if !exists {
			lun := 0
			if descriptor.Kind == domain.LocalDeviceKindDrive {
				for usedLUNs[nextLUN] {
					nextLUN++
				}
				lun = nextLUN
				usedLUNs[lun] = true
				nextLUN++
			}
			mapping = domain.LocalLoopbackDeviceMapping{
				DeviceKey:   descriptor.DeviceKey,
				LibraryID:   libraryID,
				Kind:        descriptor.Kind,
				DriveID:     descriptor.DriveID,
				LUNIndex:    lun,
				IdentityRef: descriptor.IdentityRef,
				BackendRef:  descriptor.BackendRef,
				State:       domain.LocalMappingStateActive,
			}
		} else {
			mapping.State = domain.LocalMappingStateActive
		}
		if !exists {
			if publication := publications[descriptor.DeviceKey]; publication != nil {
				mapping.BackendRef = runtimeBackstoreName(publication)
			}
		}
		if mapping.LibraryID == libraryID && mapping.Validate() == nil {
			wanted = append(wanted, mapping)
		}
	}
	stale := make([]domain.LocalLoopbackDeviceMapping, 0)
	for key, mapping := range existingByKey {
		if !wantedKeys[key] {
			mapping.State = domain.LocalMappingStateCleanupPending
			stale = append(stale, mapping)
		}
	}
	sort.Slice(wanted, func(i, j int) bool { return wanted[i].LUNIndex < wanted[j].LUNIndex })
	sort.Slice(stale, func(i, j int) bool { return stale[i].LUNIndex < stale[j].LUNIndex })
	return wanted, stale
}

func localMountIdentityChanged(descriptors []domain.VTLDeviceDescriptor, mappings []domain.LocalLoopbackDeviceMapping) bool {
	identityByKey := make(map[string]string, len(descriptors))
	for _, descriptor := range descriptors {
		identityByKey[descriptor.DeviceKey] = descriptor.IdentityRef
	}
	for _, mapping := range mappings {
		if identity, ok := identityByKey[mapping.DeviceKey]; ok && mapping.IdentityRef != identity {
			return true
		}
	}
	return false
}

func filterMappingState(mappings []domain.LocalLoopbackDeviceMapping, state domain.LocalMappingState) []domain.LocalLoopbackDeviceMapping {
	result := make([]domain.LocalLoopbackDeviceMapping, 0, len(mappings))
	for _, mapping := range mappings {
		if mapping.State == state {
			result = append(result, mapping)
		}
	}
	return result
}

func groupDescriptorsByLibrary(descriptors []domain.VTLDeviceDescriptor) map[string][]domain.VTLDeviceDescriptor {
	result := make(map[string][]domain.VTLDeviceDescriptor)
	for _, descriptor := range descriptors {
		result[descriptor.LibraryID] = append(result[descriptor.LibraryID], descriptor)
	}
	return result
}

func sortedDescriptorLibraries(grouped map[string][]domain.VTLDeviceDescriptor) []string {
	result := make([]string, 0, len(grouped))
	for libraryID := range grouped {
		result = append(result, libraryID)
	}
	sort.Strings(result)
	return result
}

func publicationDeviceKey(publication *domain.TargetPublication) string {
	if publication == nil {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(publication.DeviceRole), string(domain.LocalDeviceKindChanger)) {
		return domain.ChangerDeviceKey(publication.LibraryID)
	}
	if strings.TrimSpace(publication.DriveID) == "" {
		return ""
	}
	return domain.DriveDeviceKey(publication.DriveID)
}

func aggregateLocalMountStatus(enabled bool, descriptors []domain.VTLDeviceDescriptor, observations map[string]LocalLoopbackDeviceObservation, residuals []domain.LocalMountDeviceStatus, syncErr error) LocalMountStatus {
	status := emptyLocalMountStatus()
	status.Enabled = enabled
	status.DesiredDeviceCount = len(descriptors)
	status.Devices = append(status.Devices, residuals...)
	for _, descriptor := range descriptors {
		observation, ok := observations[descriptor.DeviceKey]
		device := domain.LocalMountDeviceStatus{
			DeviceKey: descriptor.DeviceKey, Kind: descriptor.Kind, LibraryID: descriptor.LibraryID,
			DriveID: descriptor.DriveID, DisplayName: descriptor.DisplayName,
			State: domain.LocalMountDeviceStatePending, ObservedPaths: []string{},
		}
		if device.DisplayName == "" {
			device.DisplayName = descriptor.DeviceKey
		}
		if !descriptor.BackendReady {
			device.State = domain.LocalMountDeviceStateNotReady
			device.ReasonCode = descriptor.ReadinessReason
		} else if ok {
			device.State = observation.State
			device.ObservedPaths = append([]string(nil), observation.ObservedPaths...)
			device.ReasonCode = observation.ReasonCode
		}
		status.Devices = append(status.Devices, device)
		if device.State == domain.LocalMountDeviceStateConnected {
			status.ConnectedDeviceCount++
		}
	}
	status.ResidualDeviceCount += countResidualDevices(residuals)
	status.State = resolveLocalMountState(enabled, status.DesiredDeviceCount, status.ConnectedDeviceCount, status.ResidualDeviceCount, status.Devices, syncErr)
	if syncErr != nil {
		status.LastError = safeLocalMountError(syncErr)
	}
	return status
}

func resolveLocalMountState(enabled bool, desired, connected, residual int, devices []domain.LocalMountDeviceStatus, syncErr error) domain.LocalMountState {
	if !enabled {
		if residual > 0 || syncErr != nil {
			return domain.LocalMountStateFailed
		}
		return domain.LocalMountStateDisabled
	}
	if syncErr != nil {
		return domain.LocalMountStateFailed
	}
	if desired == 0 && connected == 0 && residual == 0 {
		return domain.LocalMountStateConnected
	}
	if connected == desired && residual == 0 {
		return domain.LocalMountStateConnected
	}
	for _, device := range devices {
		if device.State == domain.LocalMountDeviceStatePending || device.State == domain.LocalMountDeviceStateRemoving {
			return domain.LocalMountStateConnecting
		}
	}
	if connected > 0 {
		return domain.LocalMountStatePartial
	}
	return domain.LocalMountStateFailed
}

func countResidual(observations []LocalLoopbackDeviceObservation) int {
	count := 0
	for _, observation := range observations {
		if observation.State == domain.LocalMountDeviceStateResidual {
			count++
		}
	}
	return count
}

func countResidualDevices(devices []domain.LocalMountDeviceStatus) int {
	count := 0
	for _, device := range devices {
		if device.State == domain.LocalMountDeviceStateResidual {
			count++
		}
	}
	return count
}

func ownedDeviceKeys(owners []LocalLoopbackOwner, libraryID string) map[string]bool {
	result := make(map[string]bool)
	for _, owner := range owners {
		if owner.LibraryID != libraryID || !owner.Present {
			continue
		}
		for _, device := range owner.Devices {
			result[device.DeviceKey] = true
		}
	}
	return result
}

func cloneLocalMountStatus(status LocalMountStatus) LocalMountStatus {
	status.Devices = append([]domain.LocalMountDeviceStatus{}, status.Devices...)
	status.DesiredIQNs = []string{}
	status.MountedIQNs = []string{}
	status.SkippedTargets = []string{}
	if status.LastSyncAt != nil {
		copyTime := *status.LastSyncAt
		status.LastSyncAt = &copyTime
	}
	return status
}

func safeLocalMountError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return string(domain.LocalMountReasonOperationTimeout)
	}
	if helperErr, ok := err.(LocalLoopbackHelperError); ok {
		if knownLocalLoopbackHelperReason(helperErr.ReasonCode) {
			return helperErr.ReasonCode
		}
	}
	return "local mount operation failed"
}

func timePointer(value time.Time) *time.Time { return &value }
