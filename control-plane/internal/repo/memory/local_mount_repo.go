package memory

import (
	"context"
	"sort"
	"sync"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo"
)

type LocalMountRepo struct {
	mu        sync.RWMutex
	enabled   bool
	libraries map[string]domain.LocalLoopbackLibraryMapping
	devices   map[string]domain.LocalLoopbackDeviceMapping
}

func NewLocalMountRepo() *LocalMountRepo {
	return &LocalMountRepo{
		libraries: make(map[string]domain.LocalLoopbackLibraryMapping),
		devices:   make(map[string]domain.LocalLoopbackDeviceMapping),
	}
}

func (r *LocalMountRepo) Enabled(context.Context) (bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.enabled, nil
}

func (r *LocalMountRepo) SetEnabled(_ context.Context, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled = enabled
	return nil
}

func (r *LocalMountRepo) SaveLibraryMapping(_ context.Context, mapping domain.LocalLoopbackLibraryMapping) error {
	if mapping.Validate() != nil {
		return domain.ErrInvalidInput
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.libraries[mapping.LibraryID]; ok {
		if existing != mapping {
			return domain.ErrConflict
		}
		return nil
	}
	for _, existing := range r.libraries {
		if existing.TargetNAA == mapping.TargetNAA || existing.NexusNAA == mapping.NexusNAA {
			return domain.ErrConflict
		}
	}
	r.libraries[mapping.LibraryID] = mapping
	return nil
}

func (r *LocalMountRepo) ListLibraryMappings(context.Context) ([]domain.LocalLoopbackLibraryMapping, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.libraries))
	for id := range r.libraries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]domain.LocalLoopbackLibraryMapping, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.libraries[id])
	}
	return out, nil
}

func (r *LocalMountRepo) DeleteLibraryMapping(_ context.Context, libraryID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.libraries[libraryID]; !ok {
		return domain.ErrNotFound
	}
	for _, mapping := range r.devices {
		if mapping.LibraryID == libraryID {
			return domain.ErrConflict
		}
	}
	delete(r.libraries, libraryID)
	return nil
}

func (r *LocalMountRepo) SaveDeviceMapping(_ context.Context, mapping domain.LocalLoopbackDeviceMapping) error {
	if mapping.Validate() != nil {
		return domain.ErrInvalidInput
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.libraries[mapping.LibraryID]; !ok {
		return domain.ErrNotFound
	}
	if existing, ok := r.devices[mapping.DeviceKey]; ok {
		if existing != mapping {
			return domain.ErrConflict
		}
		return nil
	}
	for _, existing := range r.devices {
		if existing.LibraryID == mapping.LibraryID && existing.LUNIndex == mapping.LUNIndex ||
			existing.IdentityRef == mapping.IdentityRef || existing.BackendRef == mapping.BackendRef {
			return domain.ErrConflict
		}
	}
	r.devices[mapping.DeviceKey] = mapping
	return nil
}

func (r *LocalMountRepo) ListDeviceMappings(_ context.Context, libraryID string) ([]domain.LocalLoopbackDeviceMapping, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := make([]string, 0)
	for key, mapping := range r.devices {
		if mapping.LibraryID == libraryID {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	out := make([]domain.LocalLoopbackDeviceMapping, 0, len(keys))
	for _, key := range keys {
		out = append(out, r.devices[key])
	}
	return out, nil
}

func (r *LocalMountRepo) MarkDeviceCleanupPending(_ context.Context, deviceKey string) error {
	return r.setDeviceState(deviceKey, domain.LocalMappingStateCleanupPending)
}

func (r *LocalMountRepo) MarkDeviceActive(_ context.Context, deviceKey string) error {
	return r.setDeviceState(deviceKey, domain.LocalMappingStateActive)
}

func (r *LocalMountRepo) MarkDeviceInactive(_ context.Context, deviceKey string) error {
	return r.setDeviceState(deviceKey, domain.LocalMappingStateInactive)
}

func (r *LocalMountRepo) setDeviceState(deviceKey string, state domain.LocalMappingState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	mapping, ok := r.devices[deviceKey]
	if !ok {
		return domain.ErrNotFound
	}
	mapping.State = state
	r.devices[deviceKey] = mapping
	return nil
}

func (r *LocalMountRepo) DeleteDeviceMapping(_ context.Context, deviceKey string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.devices[deviceKey]; !ok {
		return domain.ErrNotFound
	}
	delete(r.devices, deviceKey)
	return nil
}

var _ repo.LocalMountRepository = (*LocalMountRepo)(nil)
