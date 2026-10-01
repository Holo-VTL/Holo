package auth

import (
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func authBinding(scope domain.SecurityScope, owner, library, drive, iqn, role string, auth *domain.ISCSIAuthenticationPolicy) *domain.ISCSISecurityBinding {
	return &domain.ISCSISecurityBinding{
		Scope:          scope,
		OwnerID:        owner,
		LibraryID:      library,
		DriveID:        drive,
		TargetIQN:      iqn,
		DeviceRole:     role,
		Authentication: auth,
		Generation:     1,
	}
}

func TestResolveISCSISecurityUsesIndependentDriveTargetLibraryPrecedence(t *testing.T) {
	libraryAuth := &domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthCHAP, CredentialID: "lib-cred", Initiators: []string{"iqn.1991-05.com.microsoft:lib"}}
	targetAuth := &domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthMutualCHAP, CredentialID: "target-cred", Initiators: []string{"iqn.1991-05.com.microsoft:target"}}
	library := authBinding(domain.SecurityScopeLibrary, "lib-a", "", "", "", "", libraryAuth)
	drive := authBinding(domain.SecurityScopeDrive, "drive-a", "lib-a", "", "", "", nil)
	target := authBinding(domain.SecurityScopeTarget, "iqn.2026-04.ai.holo:drive-a", "lib-a", "drive-a", "iqn.2026-04.ai.holo:drive-a", "drive", targetAuth)

	resolved, err := ResolveISCSISecurity("drive", target.TargetIQN, "lib-a", "drive-a", target, drive, library)
	if err != nil {
		t.Fatalf("ResolveISCSISecurity() error = %v", err)
	}
	if resolved.Authentication.CredentialID != "target-cred" || resolved.AuthenticationSource.Scope != "target" {
		t.Fatalf("authentication should use Target block: %+v", resolved)
	}
	if resolved.EffectiveRevision == "" {
		t.Fatal("expected an effective policy revision")
	}
}

func TestResolveISCSISecurityChangerIgnoresIncidentalDriveBinding(t *testing.T) {
	libraryAuth := &domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthCHAP, CredentialID: "lib-cred", Initiators: []string{"iqn.1991-05.com.microsoft:library"}}
	driveAuth := &domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthCHAP, CredentialID: "drive-cred", Initiators: []string{"iqn.1991-05.com.microsoft:drive"}}
	library := authBinding(domain.SecurityScopeLibrary, "lib-a", "", "", "", "", libraryAuth)
	drive := authBinding(domain.SecurityScopeDrive, "drive-a", "lib-a", "", "", "", driveAuth)
	target := authBinding(domain.SecurityScopeTarget, "iqn.2026-04.ai.holo:changer-a", "lib-a", "", "iqn.2026-04.ai.holo:changer-a", "changer", nil)

	resolved, err := ResolveISCSISecurity("changer", target.TargetIQN, "lib-a", "incidental", target, drive, library)
	if err != nil {
		t.Fatalf("ResolveISCSISecurity() error = %v", err)
	}
	if resolved.Authentication.CredentialID != "lib-cred" || resolved.AuthenticationSource.Scope != "library" {
		t.Fatalf("changer must ignore the incidental Drive binding: %+v", resolved)
	}
}

func TestResolveISCSISecurityDefaultsOpenAndCopiesPolicy(t *testing.T) {
	library := authBinding(domain.SecurityScopeLibrary, "lib-a", "", "", "", "", nil)
	targetIQN := "iqn.2026-04.ai.holo:drive-a"
	target := authBinding(domain.SecurityScopeTarget, targetIQN, "lib-a", "drive-a", targetIQN, "drive", nil)
	resolved, err := ResolveISCSISecurity("drive", targetIQN, "lib-a", "drive-a", target, nil, library)
	if err != nil {
		t.Fatalf("ResolveISCSISecurity() error = %v", err)
	}
	if resolved.Authentication.Mode != domain.ISCSIAuthNone {
		t.Fatalf("default policy should remain open and unencrypted: %+v", resolved)
	}
	if resolved.AuthenticationSource.Scope != "default" {
		t.Fatalf("default provenance is missing: %+v", resolved)
	}
}

func TestResolveISCSISecurityRejectsWrongTargetAndRole(t *testing.T) {
	target := authBinding(domain.SecurityScopeTarget, "iqn.2026-04.ai.holo:drive-a", "lib-a", "drive-a", "iqn.2026-04.ai.holo:drive-a", "drive", nil)
	for _, tt := range []struct {
		name string
		role string
		iqn  string
	}{
		{name: "IQN mismatch", role: "drive", iqn: "iqn.2026-04.ai.holo:drive-b"},
		{name: "unknown role", role: "tape", iqn: target.TargetIQN},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ResolveISCSISecurity(tt.role, tt.iqn, "lib-a", "drive-a", target, nil, nil); err == nil {
				t.Fatal("expected invalid target identity or role to be rejected")
			}
		})
	}
}
