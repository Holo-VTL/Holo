package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo/memory"
)

type fakeISCSISecurityRuntime struct {
	present   map[string]bool
	locks     int
	absentErr error
	probes    []string
}

func (r *fakeISCSISecurityRuntime) WithISCSISecurityLock(_ context.Context, operation func() error) error {
	r.locks++
	return operation()
}

func (r *fakeISCSISecurityRuntime) TargetRuntimeAbsent(_ context.Context, iqn string) (bool, error) {
	r.probes = append(r.probes, iqn)
	if r.absentErr != nil {
		return false, r.absentErr
	}
	return !r.present[iqn], nil
}

func TestISCSISecurityServiceRequiresOfflineAndRuntimeAbsenceForEffectiveChanges(t *testing.T) {
	ctx := context.Background()
	repository := memory.NewISCSISecurityRepo()
	credential := domain.ISCSICredential{CredentialID: "cred-a", Label: "backup", Username: "backup-user", EncryptedSecret: make([]byte, 64), Version: 1}
	if err := repository.CreateCredential(ctx, credential); err != nil {
		t.Fatal(err)
	}
	iqn := "iqn.2026-04.ai.holo:drive-a"
	if err := repository.RegisterTarget(ctx, iqn, "lib-a", "drive-a", "drive"); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetTargetOffline(ctx, iqn, true); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeISCSISecurityRuntime{present: map[string]bool{}}
	service := NewISCSISecurityService(repository, nil, runtime, nil)
	library := domain.ISCSISecurityBinding{Scope: domain.SecurityScopeLibrary, OwnerID: "lib-a", Generation: 1,
		Authentication: &domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthCHAP, CredentialID: "cred-a", Initiators: []string{"iqn.1991-05.com.microsoft:backup"}}}
	if err := service.PutBinding(ctx, library, "operator"); err != nil {
		t.Fatalf("offline target policy update failed: %v", err)
	}
	runtime.present[iqn] = true
	library.Generation = 2
	library.Authentication = &domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthNone}
	if err := service.PutBinding(ctx, library, "operator"); !errors.Is(err, ErrISCSISecurityBusy) {
		t.Fatalf("present runtime target must reject a weakening edit: %v", err)
	}
	impact, err := service.PreviewBinding(ctx, library)
	if err != nil || len(impact) != 1 || !impact[0].Changed || !impact[0].Busy {
		t.Fatalf("expected busy weakening preview: %+v, %v", impact, err)
	}
	if runtime.locks != 2 {
		t.Fatalf("mutations should use shared security lock, got %d locks", runtime.locks)
	}
}

func TestISCSISecurityServiceRejectsMutualCHAPWithoutMutualCredential(t *testing.T) {
	ctx := context.Background()
	repository := memory.NewISCSISecurityRepo()
	credential := domain.ISCSICredential{
		CredentialID: "one-way", Label: "backup", Username: "backup-user",
		EncryptedSecret: make([]byte, 64), Version: 1,
	}
	if err := repository.CreateCredential(ctx, credential); err != nil {
		t.Fatal(err)
	}
	service := NewISCSISecurityService(repository, nil, nil, nil)
	existing := domain.ISCSISecurityBinding{
		Scope: domain.SecurityScopeLibrary, OwnerID: "lib-a", Generation: 1,
		Authentication: &domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthNone},
	}
	if err := service.PutBinding(ctx, existing, "operator"); err != nil {
		t.Fatalf("seed existing policy: %v", err)
	}
	binding := domain.ISCSISecurityBinding{
		Scope: domain.SecurityScopeLibrary, OwnerID: "lib-a", Generation: 2,
		Authentication: &domain.ISCSIAuthenticationPolicy{
			Mode: domain.ISCSIAuthMutualCHAP, CredentialID: credential.CredentialID,
			Initiators: []string{"iqn.1991-05.com.microsoft:backup"},
		},
	}
	if err := service.PutBinding(ctx, binding, "operator"); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("expected mutual CHAP to reject a one-way credential, got %v", err)
	}
	stored, err := service.GetBinding(ctx, domain.SecurityScopeLibrary, "lib-a")
	if err != nil || stored.Generation != 1 || stored.Authentication.Mode != domain.ISCSIAuthNone {
		t.Fatalf("rejected policy changed the existing binding: %+v, %v", stored, err)
	}
}

func TestISCSISecurityServiceCreatesWriteOnlyEncryptedCredential(t *testing.T) {
	ctx := context.Background()
	repository := memory.NewISCSISecurityRepo()
	store := newTestISCSISecretStore(t, t.TempDir()+"/iscsi-secrets.key")
	writer := audit.NewMemoryWriter()
	service := NewISCSISecurityService(repository, store, nil, writer)
	secret := domain.ISCSISecret{Username: "backup-user", Secret: "canary-secret-123"}
	metadata, err := service.CreateCredential(ctx, ISCSICredentialInput{CredentialID: "cred-a", Label: "Backup host", Secret: secret}, "operator")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret.Secret) || strings.Contains(string(encoded), "EncryptedSecret") {
		t.Fatalf("credential API metadata disclosed secret or ciphertext: %s", encoded)
	}
	stored, err := repository.FindCredential(ctx, "cred-a")
	if err != nil || strings.Contains(string(stored.EncryptedSecret), secret.Secret) {
		t.Fatalf("credential was not stored as ciphertext: %+v, %v", stored, err)
	}
	opened, err := service.OpenCredential(ctx, "cred-a")
	if err != nil || opened != secret {
		t.Fatalf("stored credential could not be used by runtime: %#v, %v", opened, err)
	}
	events := writer.Events()
	if len(events) != 1 {
		t.Fatalf("expected one audit event, got %d", len(events))
	}
	auditJSON, _ := json.Marshal(events[0])
	if strings.Contains(string(auditJSON), secret.Secret) {
		t.Fatalf("audit event disclosed secret: %s", auditJSON)
	}
}

func TestISCSISecurityServiceFailsClosedOnRuntimeProbeError(t *testing.T) {
	ctx := context.Background()
	repository := memory.NewISCSISecurityRepo()
	credential := domain.ISCSICredential{CredentialID: "cred-probe", Label: "backup", Username: "backup-user", EncryptedSecret: make([]byte, 64), Version: 1}
	if err := repository.CreateCredential(ctx, credential); err != nil {
		t.Fatal(err)
	}
	const iqn = "iqn.2026-04.ai.holo:probe-error"
	if err := repository.RegisterTarget(ctx, iqn, "lib-probe", "drive-probe", "drive"); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetTargetOffline(ctx, iqn, true); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeISCSISecurityRuntime{present: map[string]bool{}, absentErr: errors.New("configfs unavailable")}
	service := NewISCSISecurityService(repository, nil, runtime, nil)
	binding := domain.ISCSISecurityBinding{
		Scope: domain.SecurityScopeLibrary, OwnerID: "lib-probe", Generation: 1,
		Authentication: &domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthCHAP, CredentialID: credential.CredentialID, Initiators: []string{"iqn.1991-05.com.microsoft:backup"}},
	}
	if err := service.PutBinding(ctx, binding, "operator"); !errors.Is(err, ErrISCSISecurityRuntimeUnknown) {
		t.Fatalf("runtime probe failure must block policy mutation: %v", err)
	}
	if _, err := repository.FindBinding(ctx, domain.SecurityScopeLibrary, "lib-probe"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed probe must not persist the policy: err=%v", err)
	}
}

func TestISCSISecurityServiceIgnoresUnaffectedDriveOverrideDuringLibraryEdit(t *testing.T) {
	ctx := context.Background()
	repository := memory.NewISCSISecurityRepo()
	credential := domain.ISCSICredential{CredentialID: "cred-shared", Label: "backup", Username: "backup-user", EncryptedSecret: make([]byte, 64), Version: 1}
	if err := repository.CreateCredential(ctx, credential); err != nil {
		t.Fatal(err)
	}
	const overriddenIQN = "iqn.2026-04.ai.holo:drive-overridden"
	const inheritedIQN = "iqn.2026-04.ai.holo:drive-inherited"
	if err := repository.RegisterTarget(ctx, overriddenIQN, "lib-shared", "drive-overridden", "drive"); err != nil {
		t.Fatal(err)
	}
	if err := repository.RegisterTarget(ctx, inheritedIQN, "lib-shared", "drive-inherited", "drive"); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetTargetOffline(ctx, inheritedIQN, true); err != nil {
		t.Fatal(err)
	}
	chap := &domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthCHAP, CredentialID: credential.CredentialID, Initiators: []string{"iqn.1991-05.com.microsoft:backup"}}
	if err := repository.SaveBinding(ctx, domain.ISCSISecurityBinding{Scope: domain.SecurityScopeDrive, OwnerID: "drive-overridden", LibraryID: "lib-shared", Generation: 1, Authentication: chap}); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeISCSISecurityRuntime{present: map[string]bool{overriddenIQN: true}}
	service := NewISCSISecurityService(repository, nil, runtime, nil)
	libraryBinding := domain.ISCSISecurityBinding{Scope: domain.SecurityScopeLibrary, OwnerID: "lib-shared", Generation: 1, Authentication: chap}
	if err := service.PutBinding(ctx, libraryBinding, "operator"); err != nil {
		t.Fatalf("unaffected online override should not block an offline inherited descendant: %v", err)
	}
	if len(runtime.probes) != 1 || runtime.probes[0] != inheritedIQN {
		t.Fatalf("runtime probes should include only the changed descendant: %v", runtime.probes)
	}
}
