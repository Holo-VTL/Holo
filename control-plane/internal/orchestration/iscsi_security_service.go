package orchestration

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/auth"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo"
)

var (
	ErrISCSISecurityBusy           = errors.New("affected iSCSI targets must be offline")
	ErrISCSISecurityRuntimeUnknown = errors.New("iSCSI target runtime state unavailable")
)

type ISCSISecurityRuntimeCoordinator interface {
	WithISCSISecurityLock(context.Context, func() error) error
	TargetRuntimeAbsent(context.Context, string) (bool, error)
}

type ISCSISecretKeyProvisioner interface {
	ProvisionEmptyVault(context.Context) error
}

type ISCSIResolvedPublicationSecurity struct {
	Policy     domain.ResolvedISCSISecurity
	Credential *domain.ISCSISecret
}

type ProtectedTargetRuntimeAdapter interface {
	PublishProtected(context.Context, *domain.TargetPublication, ISCSIResolvedPublicationSecurity) (string, error)
}

type ISCSISecurityService struct {
	repo            repo.ISCSISecurityRepository
	secretStore     *ISCSISecretStore
	keyProvisioner  ISCSISecretKeyProvisioner
	runtime         ISCSISecurityRuntimeCoordinator
	auditW          audit.Writer
	mu              sync.Mutex
}

type ISCSICredentialInput struct {
	CredentialID string
	Label        string
	Secret       domain.ISCSISecret
}

type ISCSICredentialMetadata struct {
	CredentialID   string
	Label          string
	Username       string
	MutualUsername string
	Version        int64
	CreatedAt      time.Time
	SecretPresent  bool
}

type ISCSISecurityTargetView struct {
	Binding  domain.ISCSISecurityBinding  `json:"binding"`
	Resolved domain.ResolvedISCSISecurity `json:"resolved"`
}

type ISCSISecurityImpact struct {
	TargetIQN             string                       `json:"targetIqn"`
	DeviceRole            string                       `json:"deviceRole"`
	AdministrativeOffline bool                         `json:"administrativeOffline"`
	Absent                bool                         `json:"absent"`
	Busy                  bool                         `json:"busy"`
	Changed               bool                         `json:"changed"`
	Before                domain.ResolvedISCSISecurity `json:"before"`
	After                 domain.ResolvedISCSISecurity `json:"after"`
}

func NewISCSISecurityService(repository repo.ISCSISecurityRepository, secretStore *ISCSISecretStore, runtime ISCSISecurityRuntimeCoordinator, auditW audit.Writer) *ISCSISecurityService {
	return &ISCSISecurityService{repo: repository, secretStore: secretStore, runtime: runtime, auditW: auditW}
}

func (s *ISCSISecurityService) SetSecretKeyProvisioner(provisioner ISCSISecretKeyProvisioner) {
	s.keyProvisioner = provisioner
}

func (s *ISCSISecurityService) ResolveTarget(ctx context.Context, targetIQN string) (ISCSISecurityTargetView, error) {
	target, err := s.repo.FindTarget(ctx, targetIQN)
	if err != nil {
		return ISCSISecurityTargetView{}, err
	}
	resolved, err := s.resolveWith(ctx, target, nil)
	if err != nil {
		return ISCSISecurityTargetView{}, err
	}
	return ISCSISecurityTargetView{Binding: target, Resolved: resolved}, nil
}

func (s *ISCSISecurityService) InitiatorAllowed(ctx context.Context, targetIQN, initiator string) (bool, error) {
	initiator = strings.ToLower(strings.TrimSpace(initiator))
	if domain.ValidateTargetIQN(initiator) != nil {
		return false, domain.ErrInvalidInput
	}
	view, err := s.ResolveTarget(ctx, targetIQN)
	if err != nil {
		return false, err
	}
	policy := view.Resolved.Authentication
	if policy.Mode == domain.ISCSIAuthNone && !policy.RestrictInitiators && len(policy.Initiators) == 0 {
		return true, nil
	}
	for _, allowed := range policy.Initiators {
		if strings.EqualFold(allowed, initiator) {
			return true, nil
		}
	}
	return false, nil
}

func (s *ISCSISecurityService) GetBinding(ctx context.Context, scope domain.SecurityScope, ownerID string) (domain.ISCSISecurityBinding, error) {
	binding, err := s.repo.FindBinding(ctx, scope, ownerID)
	if errors.Is(err, domain.ErrNotFound) && scope != domain.SecurityScopeTarget {
		return domain.ISCSISecurityBinding{Scope: scope, OwnerID: ownerID}, nil
	}
	return binding, err
}

func (s *ISCSISecurityService) ListTargets(ctx context.Context) ([]ISCSISecurityTargetView, error) {
	targets, err := s.repo.ListTargets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ISCSISecurityTargetView, 0, len(targets))
	for _, target := range targets {
		resolved, err := s.resolveWith(ctx, target, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, ISCSISecurityTargetView{Binding: target, Resolved: resolved})
	}
	return out, nil
}

func (s *ISCSISecurityService) PutBinding(ctx context.Context, binding domain.ISCSISecurityBinding, actor string) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	if err := s.validateReferences(ctx, binding); err != nil {
		return err
	}
	mutate := func() error {
		return s.putBindingLocked(ctx, binding, actor)
	}
	if s.runtime == nil {
		return mutate()
	}
	return s.runtime.WithISCSISecurityLock(ctx, mutate)
}

func (s *ISCSISecurityService) putBindingLocked(ctx context.Context, binding domain.ISCSISecurityBinding, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	targets, err := s.affectedTargets(ctx, binding)
	if err != nil {
		return err
	}
	for _, target := range targets {
		before, err := s.resolveWith(ctx, target, nil)
		if err != nil {
			return err
		}
		after, err := s.resolveWith(ctx, target, &binding)
		if err != nil {
			return err
		}
		if !sameEffectiveISCSIPolicy(before, after) {
			absent := false
			if s.runtime != nil {
				absent, err = s.runtime.TargetRuntimeAbsent(ctx, target.TargetIQN)
				if err != nil {
					return ErrISCSISecurityRuntimeUnknown
				}
			}
			if !target.AdministrativeOffline || !absent {
				return ErrISCSISecurityBusy
			}
		}
	}
	if err := s.repo.SaveBinding(ctx, binding); err != nil {
		return err
	}
	return s.writeAudit(ctx, actor, "binding_updated", string(binding.Scope), binding.OwnerID, "success", map[string]any{"generation": binding.Generation})
}

func (s *ISCSISecurityService) PreviewBinding(ctx context.Context, binding domain.ISCSISecurityBinding) ([]ISCSISecurityImpact, error) {
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	if err := s.validateReferences(ctx, binding); err != nil {
		return nil, err
	}
	targets, err := s.affectedTargets(ctx, binding)
	if err != nil {
		return nil, err
	}
	out := make([]ISCSISecurityImpact, 0, len(targets))
	for _, target := range targets {
		before, err := s.resolveWith(ctx, target, nil)
		if err != nil {
			return nil, err
		}
		after, err := s.resolveWith(ctx, target, &binding)
		if err != nil {
			return nil, err
		}
		changed := !sameEffectiveISCSIPolicy(before, after)
		absent := false
		if s.runtime != nil {
			absent, err = s.runtime.TargetRuntimeAbsent(ctx, target.TargetIQN)
			if err != nil {
				absent = false
			}
		}
		out = append(out, ISCSISecurityImpact{
			TargetIQN: target.TargetIQN, DeviceRole: target.DeviceRole,
			AdministrativeOffline: target.AdministrativeOffline, Absent: absent,
			Busy: changed && (!target.AdministrativeOffline || !absent), Changed: changed,
			Before: before, After: after,
		})
	}
	return out, nil
}

func (s *ISCSISecurityService) RegisterTarget(ctx context.Context, targetIQN, libraryID, driveID, role string) error {
	mutate := func() error {
		return s.RegisterTargetLocked(ctx, targetIQN, libraryID, driveID, role)
	}
	if s.runtime == nil {
		return mutate()
	}
	return s.runtime.WithISCSISecurityLock(ctx, mutate)
}

func (s *ISCSISecurityService) RegisterTargetLocked(ctx context.Context, targetIQN, libraryID, driveID, role string) error {
	if role == "changer" {
		driveID = ""
	}
	return s.repo.RegisterTarget(ctx, targetIQN, libraryID, driveID, role)
}

func (s *ISCSISecurityService) SetTargetOffline(ctx context.Context, targetIQN string, offline bool, actor string) error {
	mutate := func() error {
		return s.SetTargetOfflineLocked(ctx, targetIQN, offline, actor)
	}
	if s.runtime == nil {
		return mutate()
	}
	return s.runtime.WithISCSISecurityLock(ctx, mutate)
}

func (s *ISCSISecurityService) SetTargetOfflineLocked(ctx context.Context, targetIQN string, offline bool, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.repo.SetTargetOffline(ctx, targetIQN, offline); err != nil {
		return err
	}
	return s.writeAudit(ctx, actor, "target_offline_intent_changed", "target", targetIQN, "success", map[string]any{"offline": offline})
}

func (s *ISCSISecurityService) PreparePublicationLocked(ctx context.Context, publication *domain.TargetPublication, automatic bool) error {
	_, err := s.ResolvePublicationLocked(ctx, publication, automatic)
	return err
}

func (s *ISCSISecurityService) ResolvePublicationLocked(ctx context.Context, publication *domain.TargetPublication, automatic bool) (ISCSIResolvedPublicationSecurity, error) {
	if publication == nil {
		return ISCSIResolvedPublicationSecurity{}, domain.ErrInvalidInput
	}
	driveID := publication.DriveID
	if publication.DeviceRole == "changer" {
		driveID = ""
	}
	if err := s.RegisterTargetLocked(ctx, publication.TargetIQN, publication.LibraryID, driveID, publication.DeviceRole); err != nil {
		return ISCSIResolvedPublicationSecurity{}, err
	}
	target, err := s.repo.FindTarget(ctx, publication.TargetIQN)
	if err != nil {
		return ISCSIResolvedPublicationSecurity{}, err
	}
	if automatic && target.AdministrativeOffline {
		return ISCSIResolvedPublicationSecurity{}, ErrISCSISecurityBusy
	}
	resolved, err := s.ResolveTarget(ctx, publication.TargetIQN)
	if err != nil {
		return ISCSIResolvedPublicationSecurity{}, err
	}
	result := ISCSIResolvedPublicationSecurity{Policy: resolved.Resolved}
	if resolved.Resolved.Authentication.Mode == domain.ISCSIAuthCHAP || resolved.Resolved.Authentication.Mode == domain.ISCSIAuthMutualCHAP {
		credential, err := s.OpenCredential(ctx, resolved.Resolved.Authentication.CredentialID)
		if err != nil {
			return ISCSIResolvedPublicationSecurity{}, err
		}
		if resolved.Resolved.Authentication.Mode == domain.ISCSIAuthMutualCHAP && credential.MutualSecret == "" {
			return ISCSIResolvedPublicationSecurity{}, domain.ErrInvalidState
		}
		result.Credential = &credential
	}
	if !automatic && target.AdministrativeOffline {
		if err := s.repo.SetTargetOffline(ctx, publication.TargetIQN, false); err != nil {
			return ISCSIResolvedPublicationSecurity{}, err
		}
	}
	return result, nil
}

func resolvedPolicyRequiresProtection(policy domain.ResolvedISCSISecurity) bool {
	return policy.Authentication.Mode != domain.ISCSIAuthNone ||
		policy.Authentication.CredentialID != "" || policy.Authentication.RestrictInitiators || len(policy.Authentication.Initiators) > 0
}

func (s *ISCSISecurityService) CreateCredential(ctx context.Context, input ISCSICredentialInput, actor string) (ISCSICredentialMetadata, error) {
	if s.secretStore == nil || domain.ValidateManagementID(input.CredentialID) != nil ||
		domain.ValidateManagementLabel(input.Label, true) != nil || len(input.Label) > 128 || input.Secret.Validate() != nil {
		return ISCSICredentialMetadata{}, domain.ErrInvalidInput
	}
	credentials, err := s.repo.ListCredentials(ctx)
	if err != nil {
		return ISCSICredentialMetadata{}, err
	}
	vaultEmpty := len(credentials) == 0
	if vaultEmpty && s.keyProvisioner != nil {
		if err := s.keyProvisioner.ProvisionEmptyVault(ctx); err != nil {
			return ISCSICredentialMetadata{}, err
		}
	}
	encrypted, err := s.secretStore.SealCredential(input.Secret, input.CredentialID, 1, vaultEmpty && s.keyProvisioner == nil)
	if err != nil {
		return ISCSICredentialMetadata{}, err
	}
	credential := domain.ISCSICredential{
		CredentialID: input.CredentialID, Label: input.Label, Username: input.Secret.Username,
		MutualUsername: input.Secret.MutualUsername, EncryptedSecret: encrypted, Version: 1, CreatedAt: time.Now().UTC(),
	}
	if err := s.repo.CreateCredential(ctx, credential); err != nil {
		return ISCSICredentialMetadata{}, err
	}
	if err := s.writeAudit(ctx, actor, "credential_created", "iscsi_credential", input.CredentialID, "success", map[string]any{"version": credential.Version}); err != nil {
		return ISCSICredentialMetadata{}, err
	}
	return credentialMetadata(credential), nil
}

func (s *ISCSISecurityService) ListCredentials(ctx context.Context) ([]ISCSICredentialMetadata, error) {
	credentials, err := s.repo.ListCredentials(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ISCSICredentialMetadata, 0, len(credentials))
	for _, credential := range credentials {
		out = append(out, credentialMetadata(credential))
	}
	return out, nil
}

func (s *ISCSISecurityService) DeleteCredential(ctx context.Context, credentialID, actor string) error {
	if err := s.repo.DeleteCredential(ctx, credentialID); err != nil {
		return err
	}
	return s.writeAudit(ctx, actor, "credential_deleted", "iscsi_credential", credentialID, "success", nil)
}

func (s *ISCSISecurityService) OpenCredential(ctx context.Context, credentialID string) (domain.ISCSISecret, error) {
	if s.secretStore == nil {
		return domain.ISCSISecret{}, ErrISCSISecretKeyMissing
	}
	credential, err := s.repo.FindCredential(ctx, credentialID)
	if err != nil {
		return domain.ISCSISecret{}, err
	}
	return s.secretStore.OpenCredential(credential)
}

func (s *ISCSISecurityService) validateReferences(ctx context.Context, binding domain.ISCSISecurityBinding) error {
	if binding.Authentication != nil && binding.Authentication.CredentialID != "" {
		if _, err := s.repo.FindCredential(ctx, binding.Authentication.CredentialID); err != nil {
			return err
		}
	}
	return nil
}

func (s *ISCSISecurityService) affectedTargets(ctx context.Context, binding domain.ISCSISecurityBinding) ([]domain.ISCSISecurityBinding, error) {
	if binding.Scope == domain.SecurityScopeTarget {
		target, err := s.repo.FindTarget(ctx, binding.TargetIQN)
		if err != nil {
			return nil, err
		}
		return []domain.ISCSISecurityBinding{target}, nil
	}
	targets, err := s.repo.ListTargets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ISCSISecurityBinding, 0)
	for _, target := range targets {
		if binding.Scope == domain.SecurityScopeLibrary && target.LibraryID == binding.OwnerID ||
			binding.Scope == domain.SecurityScopeDrive && target.DeviceRole == "drive" && target.DriveID == binding.OwnerID {
			out = append(out, target)
		}
	}
	return out, nil
}

func (s *ISCSISecurityService) resolveWith(ctx context.Context, target domain.ISCSISecurityBinding, candidate *domain.ISCSISecurityBinding) (domain.ResolvedISCSISecurity, error) {
	targetBinding := target
	driveBinding, err := s.optionalBinding(ctx, domain.SecurityScopeDrive, target.DriveID, candidate)
	if err != nil {
		return domain.ResolvedISCSISecurity{}, err
	}
	libraryBinding, err := s.optionalBinding(ctx, domain.SecurityScopeLibrary, target.LibraryID, candidate)
	if err != nil {
		return domain.ResolvedISCSISecurity{}, err
	}
	if candidate != nil && candidate.Scope == domain.SecurityScopeTarget && candidate.TargetIQN == target.TargetIQN {
		targetBinding = *candidate
	}
	return auth.ResolveISCSISecurity(target.DeviceRole, target.TargetIQN, target.LibraryID, target.DriveID,
		&targetBinding, driveBinding, libraryBinding)
}

func (s *ISCSISecurityService) optionalBinding(ctx context.Context, scope domain.SecurityScope, ownerID string, candidate *domain.ISCSISecurityBinding) (*domain.ISCSISecurityBinding, error) {
	if scope == domain.SecurityScopeDrive && ownerID == "" {
		return nil, nil
	}
	if candidate != nil && candidate.Scope == scope && candidate.OwnerID == ownerID {
		return candidate, nil
	}
	binding, err := s.repo.FindBinding(ctx, scope, ownerID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &binding, nil
}

func sameEffectiveISCSIPolicy(left, right domain.ResolvedISCSISecurity) bool {
	leftAuth := left.Authentication
	rightAuth := right.Authentication
	sort.Strings(leftAuth.Initiators)
	sort.Strings(rightAuth.Initiators)
	return reflect.DeepEqual(leftAuth, rightAuth)
}

func credentialMetadata(credential domain.ISCSICredential) ISCSICredentialMetadata {
	return ISCSICredentialMetadata{
		CredentialID: credential.CredentialID, Label: credential.Label,
		Username: credential.Username, MutualUsername: credential.MutualUsername,
		Version: credential.Version, CreatedAt: credential.CreatedAt, SecretPresent: len(credential.EncryptedSecret) > 0,
	}
}

func (s *ISCSISecurityService) writeAudit(ctx context.Context, actor, action, objectType, objectID, result string, details map[string]any) error {
	if s.auditW == nil {
		return nil
	}
	eventID, err := newRuntimeID("sec")
	if err != nil {
		return err
	}
	return s.auditW.Write(ctx, audit.Event{
		EventID: eventID, Actor: safeActor(actor), Action: action, ObjectType: objectType,
		ObjectID: objectID, Result: result, Details: details, OccurredAt: time.Now().UTC(),
	})
}
