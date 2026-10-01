package orchestration

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo/memory"
)

type fakeProtectedHelperRunner struct {
	path  string
	input []byte
	reply []byte
}

func TestNewDefaultISCSISecurityHelperUsesConfiguredPath(t *testing.T) {
	const customPath = "/opt/holo-custom/bin/holo-iscsi-security-helper"
	t.Setenv("HOLO_ISCSI_SECURITY_HELPER", customPath)

	helper := NewDefaultISCSISecurityHelper(false)
	if helper.binaryPath != customPath {
		t.Fatalf("expected configured security helper path %q, got %q", customPath, helper.binaryPath)
	}
}

func (r *fakeProtectedHelperRunner) Run(_ context.Context, path string, input []byte) ([]byte, error) {
	r.path = path
	r.input = append([]byte(nil), input...)
	return r.reply, nil
}

func TestProtectedTargetRequestCarriesSecretsOnlyOnStdinAndUsesFiniteACLs(t *testing.T) {
	publication := tcmuTestPublication()
	secret := &domain.ISCSISecret{Username: "backup-user", Secret: "canary-chap-secret"}
	security := ISCSIResolvedPublicationSecurity{
		Policy: domain.ResolvedISCSISecurity{
			Authentication: domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthCHAP, CredentialID: "cred-a", Initiators: []string{"iqn.1991-05.com.microsoft:backup-a"}},
		},
		Credential: secret,
	}
	request, err := protectedTargetRequest(publication, security, "holo_pub_a", "fileio", "192.0.2.10", 3260)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), secret.Secret) || strings.Contains(string(encoded), "backstorePath") {
		t.Fatalf("helper stdin should contain validated secret fields only: %s", encoded)
	}
	if _, err := protectedTargetRequest(publication, security, "other", "fileio", "192.0.2.10", 3260); err == nil {
		t.Fatal("non-Holo backstore names must be rejected")
	}
	if _, err := protectedTargetRequest(publication, security, "holo_pub_a", "fileio", "0.0.0.0", 3260); err == nil {
		t.Fatal("wildcard portal address must be rejected")
	}
}

func TestLIOShellProtectedPublishCreatesNoTargetThroughTargetcli(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pools")
	t.Setenv("HOLO_STORAGE_POOL_ROOT_BASE", root)
	runner := &fakeCommandRunner{}
	adapter := newLIOShellTargetRuntimeAdapter(TargetRuntimeConfig{Mode: "lio-shell", PortalHost: "192.0.2.10", PortalPort: 3260}, runner)
	helperRunner := &fakeProtectedHelperRunner{reply: []byte("{\"ok\":true,\"code\":\"ok\",\"revision\":\"r1\",\"ready\":true}")}
	adapter.securityHelper.runner = helperRunner
	publication := tcmuTestPublication()
	security := ISCSIResolvedPublicationSecurity{
		Policy: domain.ResolvedISCSISecurity{Authentication: domain.ISCSIAuthenticationPolicy{
			Mode: domain.ISCSIAuthMutualCHAP, CredentialID: "cred-a", Initiators: []string{"iqn.1991-05.com.microsoft:backup-a"},
		}},
		Credential: &domain.ISCSISecret{Username: "backup-user", Secret: "forward-secret-123", MutualUsername: "holo-target", MutualSecret: "reverse-secret-456"},
	}
	portal, err := adapter.PublishProtected(context.Background(), publication, security)
	if err != nil || portal != "192.0.2.10:3260" {
		t.Fatalf("protected publish failed: portal=%q err=%v", portal, err)
	}
	if len(runner.calls) != 1 || !strings.Contains(strings.Join(runner.calls[0], " "), "/backstores/fileio create") {
		t.Fatalf("targetcli should create only the backstore: %v", runner.calls)
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call, " "), "/iscsi") || strings.Contains(strings.Join(call, " "), security.Credential.Secret) {
			t.Fatalf("target or secret escaped through targetcli argv: %v", runner.calls)
		}
	}
	if helperRunner.path != defaultISCSISecurityHelperPath {
		t.Fatalf("wrong privileged helper path: %q", helperRunner.path)
	}
	if !strings.Contains(string(helperRunner.input), security.Credential.Secret) || strings.Contains(string(helperRunner.input), "file_or_dev") {
		t.Fatalf("protected declarative request missing secret or contains raw path: %s", helperRunner.input)
	}
}

func TestSecurityHelperResponseRejectsUntrustedRevisionOrOversizedOutput(t *testing.T) {
	for _, test := range []struct {
		name  string
		reply []byte
	}{
		{name: "unknown field", reply: []byte("{\"ok\":true,\"code\":\"ok\",\"secret\":\"canary\"}")},
		{name: "oversized", reply: make([]byte, maxISCSISecurityHelperInput+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeProtectedHelperRunner{reply: test.reply}
			helper := &ISCSISecurityHelper{binaryPath: defaultISCSISecurityHelperPath, runner: runner}
			if _, err := helper.Call(context.Background(), map[string]any{"version": 1, "operation": "capabilities"}); err == nil {
				t.Fatal("unsafe helper output should be rejected")
			}
		})
	}
}

func TestSecurityTargetEndpointValidationUsesIPv4Only(t *testing.T) {
	if net.ParseIP("192.0.2.10").To4() == nil {
		t.Fatal("test fixture IPv4 invalid")
	}
	publication := tcmuTestPublication()
	security := ISCSIResolvedPublicationSecurity{Policy: domain.ResolvedISCSISecurity{Authentication: domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthNone}}}
	if _, err := protectedTargetRequest(publication, security, "holo_pub_a", "fileio", "::1", 3260); err == nil {
		t.Fatal("IPv6 protected portal must be rejected")
	}
}

func TestProtectedBackstoreImageIsContainedAndRemovedOnHelperFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pools")
	t.Setenv("HOLO_STORAGE_POOL_ROOT_BASE", root)
	runner := &fakeCommandRunner{}
	adapter := newLIOShellTargetRuntimeAdapter(TargetRuntimeConfig{Mode: "lio-shell", PortalHost: "192.0.2.10", PortalPort: 3260}, runner)
	adapter.securityHelper.runner = &fakeProtectedHelperRunner{reply: []byte("{\"ok\":false,\"code\":\"operation_failed\"}")}
	publication := tcmuTestPublication()
	security := ISCSIResolvedPublicationSecurity{Policy: domain.ResolvedISCSISecurity{Authentication: domain.ISCSIAuthenticationPolicy{
		Mode: domain.ISCSIAuthNone, Initiators: []string{"iqn.1991-05.com.microsoft:backup-a"},
	}}}
	if _, err := adapter.PublishProtected(context.Background(), publication, security); err == nil {
		t.Fatal("helper failure should fail protected publication")
	}
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".img") {
			t.Errorf("failed secure publish left a backstore image: %s", path)
		}
		return nil
	}); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestInMemoryPublicationReportsSimulatedSecurityAndPersistsOfflineState(t *testing.T) {
	ctx := context.Background()
	core := memory.NewCoreResourcesRepo()
	library, _ := domain.NewVirtualLibrary("lib-secure", "Secure Library")
	drive, _ := domain.NewVirtualDrive("drive-secure", "lib-secure", 1)
	cartridge := domain.NewVirtualCartridge("cart-secure", "pool-secure", "lib-secure", "VTA901L06", 1<<20)
	if err := core.CreateLibrary(ctx, library); err != nil {
		t.Fatal(err)
	}
	if err := core.CreateDrive(ctx, drive); err != nil {
		t.Fatal(err)
	}
	if err := core.CreateCartridge(ctx, cartridge); err != nil {
		t.Fatal(err)
	}
	runtimeRepo := memory.NewTargetRuntimeRepo()
	runtime := NewTargetRuntimeServiceWithConfig(core, runtimeRepo, nil, nil, TargetRuntimeConfig{Mode: "in-memory"})
	securityRepo := memory.NewISCSISecurityRepo()
	if err := securityRepo.SaveBinding(ctx, domain.ISCSISecurityBinding{
		Scope: domain.SecurityScopeLibrary, OwnerID: library.LibraryID, Generation: 1,
		Authentication: &domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthNone, Initiators: []string{"iqn.1991-05.com.microsoft:backup"}},
	}); err != nil {
		t.Fatal(err)
	}
	security := NewISCSISecurityService(securityRepo, nil, runtime, nil)
	runtime.SetISCSISecurityService(security)
	publication, err := runtime.Publish(ctx, PublishRequest{
		LibraryID: library.LibraryID, DriveID: drive.DriveID, CartridgeID: cartridge.CartridgeID,
		TargetIQN: drive.IQN, Actor: "operator",
	})
	if err != nil || publication.SecurityEnforcement != domain.SecurityEnforcementSimulated {
		t.Fatalf("expected simulated ACL enforcement, publication=%+v err=%v", publication, err)
	}
	offline, err := runtime.Unpublish(ctx, publication.PublicationID, "operator")
	if err != nil || offline.SecurityEnforcement != domain.SecurityEnforcementOffline {
		t.Fatalf("expected persisted offline status, publication=%+v err=%v", offline, err)
	}
	target, err := securityRepo.FindTarget(ctx, drive.IQN)
	if err != nil || !target.AdministrativeOffline {
		t.Fatalf("offline intent was not persisted: %+v err=%v", target, err)
	}
	target.Generation++
	target.Authentication = &domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthNone, RestrictInitiators: true}
	if err := security.PutBinding(ctx, target, "operator"); err != nil {
		t.Fatalf("offline target override update failed: %v", err)
	}
	republished, err := runtime.Publish(ctx, PublishRequest{
		LibraryID: library.LibraryID, DriveID: drive.DriveID, CartridgeID: cartridge.CartridgeID,
		TargetIQN: drive.IQN, Actor: "operator",
	})
	if err != nil || republished.PublicationID == publication.PublicationID {
		t.Fatalf("republish did not create a new publication for the same stable target: %+v %v", republished, err)
	}
	resolved, err := security.ResolveTarget(ctx, drive.IQN)
	if err != nil || resolved.Binding.Authentication == nil || !resolved.Binding.Authentication.RestrictInitiators {
		t.Fatalf("stable target override did not survive republish: %+v %v", resolved, err)
	}
}
