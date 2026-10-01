package memory

import (
	"context"
	"sort"
	"sync"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo"
)

const maxISCSISecuritySnapshotHistory = 20

var _ repo.ISCSISecurityRepository = (*ISCSISecurityRepo)(nil)

type ISCSISecurityRepo struct {
	mu          sync.RWMutex
	bindings    map[string]domain.ISCSISecurityBinding
	credentials map[string]domain.ISCSICredential
	snapshots   map[string][]domain.ISCSISecuritySnapshot
}

func NewISCSISecurityRepo() *ISCSISecurityRepo {
	return &ISCSISecurityRepo{
		bindings:    make(map[string]domain.ISCSISecurityBinding),
		credentials: make(map[string]domain.ISCSICredential),
		snapshots:   make(map[string][]domain.ISCSISecuritySnapshot),
	}
}

func (r *ISCSISecurityRepo) SaveBinding(_ context.Context, binding domain.ISCSISecurityBinding) error {
	if binding.Validate() != nil {
		return domain.ErrInvalidInput
	}
	key := securityBindingKey(binding.Scope, binding.OwnerID)
	r.mu.Lock()
	defer r.mu.Unlock()
	previous, exists := r.bindings[key]
	if binding.Scope == domain.SecurityScopeTarget && !exists {
		return domain.ErrNotFound
	}
	if (!exists && binding.Generation != 1) || (exists && binding.Generation != previous.Generation+1) {
		return domain.ErrConflict
	}
	if binding.Authentication != nil && binding.Authentication.CredentialID != "" {
		if _, ok := r.credentials[binding.Authentication.CredentialID]; !ok {
			return domain.ErrNotFound
		}
	}
	if binding.Scope == domain.SecurityScopeTarget {
		if binding.LibraryID != previous.LibraryID || binding.DriveID != previous.DriveID || binding.DeviceRole != previous.DeviceRole {
			return domain.ErrConflict
		}
		binding.AdministrativeOffline = previous.AdministrativeOffline
	}
	r.bindings[key] = cloneISCSIBinding(binding)
	return nil
}

func (r *ISCSISecurityRepo) FindBinding(_ context.Context, scope domain.SecurityScope, ownerID string) (domain.ISCSISecurityBinding, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	binding, ok := r.bindings[securityBindingKey(scope, ownerID)]
	if !ok {
		return domain.ISCSISecurityBinding{}, domain.ErrNotFound
	}
	return cloneISCSIBinding(binding), nil
}

func (r *ISCSISecurityRepo) RegisterTarget(_ context.Context, targetIQN, libraryID, driveID, role string) error {
	binding := domain.ISCSISecurityBinding{
		Scope: domain.SecurityScopeTarget, OwnerID: targetIQN, TargetIQN: targetIQN,
		LibraryID: libraryID, DriveID: driveID, DeviceRole: role, Generation: 1,
	}
	if binding.Validate() != nil {
		return domain.ErrInvalidInput
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := securityBindingKey(binding.Scope, binding.OwnerID)
	if old, ok := r.bindings[key]; ok {
		if old.LibraryID != libraryID || old.DriveID != driveID || old.DeviceRole != role {
			return domain.ErrConflict
		}
		return nil
	}
	r.bindings[key] = binding
	return nil
}

func (r *ISCSISecurityRepo) SetTargetOffline(_ context.Context, targetIQN string, offline bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := securityBindingKey(domain.SecurityScopeTarget, targetIQN)
	binding, ok := r.bindings[key]
	if !ok {
		return domain.ErrNotFound
	}
	binding.AdministrativeOffline = offline
	r.bindings[key] = binding
	return nil
}

func (r *ISCSISecurityRepo) FindTarget(ctx context.Context, targetIQN string) (domain.ISCSISecurityBinding, error) {
	return r.FindBinding(ctx, domain.SecurityScopeTarget, targetIQN)
}

func (r *ISCSISecurityRepo) ListTargets(_ context.Context) ([]domain.ISCSISecurityBinding, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	iqns := make([]string, 0, len(r.bindings))
	for _, binding := range r.bindings {
		if binding.Scope == domain.SecurityScopeTarget {
			iqns = append(iqns, binding.TargetIQN)
		}
	}
	sort.Strings(iqns)
	out := make([]domain.ISCSISecurityBinding, 0, len(iqns))
	for _, iqn := range iqns {
		out = append(out, cloneISCSIBinding(r.bindings[securityBindingKey(domain.SecurityScopeTarget, iqn)]))
	}
	return out, nil
}

func (r *ISCSISecurityRepo) CreateCredential(_ context.Context, credential domain.ISCSICredential) error {
	if credential.Version != 1 || credential.Validate() != nil {
		return domain.ErrInvalidInput
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.credentials[credential.CredentialID]; exists {
		return domain.ErrConflict
	}
	r.credentials[credential.CredentialID] = cloneISCSICredential(credential)
	return nil
}

func (r *ISCSISecurityRepo) FindCredential(_ context.Context, credentialID string) (domain.ISCSICredential, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	credential, ok := r.credentials[credentialID]
	if !ok {
		return domain.ISCSICredential{}, domain.ErrNotFound
	}
	return cloneISCSICredential(credential), nil
}

func (r *ISCSISecurityRepo) ListCredentials(_ context.Context) ([]domain.ISCSICredential, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.credentials))
	for id := range r.credentials {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]domain.ISCSICredential, 0, len(ids))
	for _, id := range ids {
		out = append(out, cloneISCSICredential(r.credentials[id]))
	}
	return out, nil
}

func (r *ISCSISecurityRepo) DeleteCredential(_ context.Context, credentialID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.credentials[credentialID]; !ok {
		return domain.ErrNotFound
	}
	for _, binding := range r.bindings {
		if binding.Authentication != nil && binding.Authentication.CredentialID == credentialID {
			return domain.ErrConflict
		}
	}
	for _, history := range r.snapshots {
		for _, snapshot := range history {
			if containsISCSIRef(snapshot.CredentialRefs, credentialID) {
				return domain.ErrConflict
			}
		}
	}
	delete(r.credentials, credentialID)
	return nil
}

func (r *ISCSISecurityRepo) AppendSnapshot(_ context.Context, snapshot domain.ISCSISecuritySnapshot) error {
	if snapshot.Validate() != nil {
		return domain.ErrInvalidInput
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := securityHistoryKey(snapshot.Scope, snapshot.OwnerID)
	history := r.snapshots[key]
	expected := int64(1)
	if len(history) > 0 {
		expected = history[len(history)-1].Version + 1
	}
	if snapshot.Version != expected {
		return domain.ErrConflict
	}
	for _, id := range uniqueISCSIRefs(snapshot.CredentialRefs) {
		if _, ok := r.credentials[id]; !ok {
			return domain.ErrNotFound
		}
	}
	snapshot = cloneISCsiSnapshot(snapshot)
	history = append(history, snapshot)
	if len(history) > maxISCSISecuritySnapshotHistory {
		history = append([]domain.ISCSISecuritySnapshot(nil), history[len(history)-maxISCSISecuritySnapshotHistory:]...)
	}
	r.snapshots[key] = history
	return nil
}

func (r *ISCSISecurityRepo) ListSnapshots(_ context.Context, scope, ownerID string) ([]domain.ISCSISecuritySnapshot, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	history := r.snapshots[securityHistoryKey(scope, ownerID)]
	out := make([]domain.ISCSISecuritySnapshot, len(history))
	for i := range history {
		out[i] = cloneISCsiSnapshot(history[i])
	}
	return out, nil
}

func securityBindingKey(scope domain.SecurityScope, ownerID string) string {
	return string(scope) + "\x00" + ownerID
}

func securityHistoryKey(scope, ownerID string) string {
	return scope + "\x00" + ownerID
}

func cloneISCSIBinding(in domain.ISCSISecurityBinding) domain.ISCSISecurityBinding {
	out := in
	if in.Authentication != nil {
		auth := *in.Authentication
		auth.Initiators = append([]string(nil), in.Authentication.Initiators...)
		out.Authentication = &auth
	}
	return out
}

func cloneISCSICredential(in domain.ISCSICredential) domain.ISCSICredential {
	out := in
	out.EncryptedSecret = append([]byte(nil), in.EncryptedSecret...)
	return out
}

func cloneISCsiSnapshot(in domain.ISCSISecuritySnapshot) domain.ISCSISecuritySnapshot {
	out := in
	out.CredentialRefs = append([]string(nil), in.CredentialRefs...)
	out.Payload = append([]byte(nil), in.Payload...)
	return out
}

func uniqueISCSIRefs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func containsISCSIRef(values []string, id string) bool {
	for _, value := range values {
		if value == id {
			return true
		}
	}
	return false
}
