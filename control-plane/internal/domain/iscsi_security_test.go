package domain

import (
	"strings"
	"testing"
)

func validSecurityCredential() *ISCSISecret {
	return &ISCSISecret{
		Username:       "backup-a",
		Secret:         "0123456789abcdef",
		MutualUsername: "holo-a",
		MutualSecret:   "fedcba9876543210",
	}
}

func TestAuthenticationPolicyValidateModes(t *testing.T) {
	tests := []struct {
		name    string
		policy  ISCSIAuthenticationPolicy
		wantErr bool
	}{
		{name: "open", policy: ISCSIAuthenticationPolicy{Mode: ISCSIAuthNone}},
		{name: "acl only", policy: ISCSIAuthenticationPolicy{Mode: ISCSIAuthNone, Initiators: []string{"iqn.1991-05.com.microsoft:backup-a"}}},
		{name: "chap", policy: ISCSIAuthenticationPolicy{Mode: ISCSIAuthCHAP, CredentialID: "cred-1", Initiators: []string{"iqn.1991-05.com.microsoft:backup-a"}}},
		{name: "mutual", policy: ISCSIAuthenticationPolicy{Mode: ISCSIAuthMutualCHAP, CredentialID: "cred-1", Initiators: []string{"iqn.1991-05.com.microsoft:backup-a"}}},
		{name: "unknown mode", policy: ISCSIAuthenticationPolicy{Mode: "password"}, wantErr: true},
		{name: "chap needs credential", policy: ISCSIAuthenticationPolicy{Mode: ISCSIAuthCHAP, Initiators: []string{"iqn.1991-05.com.microsoft:backup-a"}}, wantErr: true},
		{name: "chap needs finite allowlist", policy: ISCSIAuthenticationPolicy{Mode: ISCSIAuthCHAP, CredentialID: "cred-1"}, wantErr: true},
		{name: "none rejects credentials", policy: ISCSIAuthenticationPolicy{Mode: ISCSIAuthNone, CredentialID: "cred-1"}, wantErr: true},
		{name: "duplicate initiator", policy: ISCSIAuthenticationPolicy{Mode: ISCSIAuthCHAP, CredentialID: "cred-1", Initiators: []string{"iqn.1991-05.com.microsoft:backup-a", "IQN.1991-05.COM.MICROSOFT:BACKUP-A"}}, wantErr: true},
		{name: "wildcard initiator", policy: ISCSIAuthenticationPolicy{Mode: ISCSIAuthCHAP, CredentialID: "cred-1", Initiators: []string{"*"}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestAuthenticationPolicyValidateInitiatorLimit(t *testing.T) {
	initiators := make([]string, 256)
	// Use distinct valid IQNs without relying on any synthetic non-IQN syntax.
	for i := range initiators {
		initiators[i] = "iqn.1991-05.com.microsoft:host-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
	}
	policy := ISCSIAuthenticationPolicy{Mode: ISCSIAuthCHAP, CredentialID: "cred-1", Initiators: initiators}
	if err := policy.Validate(); err != nil {
		t.Fatalf("256 initiators should be accepted: %v", err)
	}
	policy.Initiators = append(policy.Initiators, "iqn.1991-05.com.microsoft:host-zz")
	if err := policy.Validate(); err == nil {
		t.Fatal("expected 257 initiators to be rejected")
	}
}

func TestISCSISecretValidateBoundsAndUnsetMarker(t *testing.T) {
	tests := []struct {
		name    string
		secret  *ISCSISecret
		wantErr bool
	}{
		{name: "valid portable pair", secret: validSecurityCredential()},
		{name: "forward/reverse same", secret: &ISCSISecret{Username: "u", Secret: "0123456789abcdef", MutualUsername: "v", MutualSecret: "0123456789abcdef"}, wantErr: true},
		{name: "mutual username without secret", secret: &ISCSISecret{Username: "u", Secret: "0123456789abcdef", MutualUsername: "v"}, wantErr: true},
		{name: "too short", secret: &ISCSISecret{Username: "u", Secret: "12345678901"}, wantErr: true},
		{name: "too long", secret: &ISCSISecret{Username: "u", Secret: strings.Repeat("a", 256)}, wantErr: true},
		{name: "non-ASCII secret", secret: &ISCSISecret{Username: "u", Secret: "0123456789abcdeé"}, wantErr: true},
		{name: "control", secret: &ISCSISecret{Username: "u", Secret: "0123456789abcde\n"}, wantErr: true},
		{name: "leading whitespace", secret: &ISCSISecret{Username: "u", Secret: " 0123456789abcde"}, wantErr: true},
		{name: "NULL prefix", secret: &ISCSISecret{Username: "u", Secret: "NULL234567890123"}, wantErr: true},
		{name: "username too long", secret: &ISCSISecret{Username: strings.Repeat("u", 256), Secret: "0123456789abcdef"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.secret.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestISCSISecurityBindingValidateOwnership(t *testing.T) {
	for _, tt := range []struct {
		name    string
		binding ISCSISecurityBinding
		wantErr bool
	}{
		{name: "library", binding: ISCSISecurityBinding{Scope: SecurityScopeLibrary, OwnerID: "lib-a", Generation: 1}},
		{name: "drive", binding: ISCSISecurityBinding{Scope: SecurityScopeDrive, OwnerID: "drive-a", LibraryID: "lib-a", Generation: 1}},
		{name: "drive requires library", binding: ISCSISecurityBinding{Scope: SecurityScopeDrive, OwnerID: "drive-a", Generation: 1}, wantErr: true},
		{name: "target drive", binding: ISCSISecurityBinding{Scope: SecurityScopeTarget, OwnerID: "iqn.2026-04.ai.holo:drive-a", TargetIQN: "iqn.2026-04.ai.holo:drive-a", LibraryID: "lib-a", DriveID: "drive-a", DeviceRole: "drive", Generation: 1}},
		{name: "target changer", binding: ISCSISecurityBinding{Scope: SecurityScopeTarget, OwnerID: "iqn.2026-04.ai.holo:changer-a", TargetIQN: "iqn.2026-04.ai.holo:changer-a", LibraryID: "lib-a", DeviceRole: "changer", Generation: 1}},
		{name: "changer drive ignored", binding: ISCSISecurityBinding{Scope: SecurityScopeTarget, OwnerID: "iqn.2026-04.ai.holo:changer-a", TargetIQN: "iqn.2026-04.ai.holo:changer-a", LibraryID: "lib-a", DriveID: "incidental", DeviceRole: "changer", Generation: 1}, wantErr: true},
		{name: "nonpositive generation", binding: ISCSISecurityBinding{Scope: SecurityScopeLibrary, OwnerID: "lib-a"}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.binding.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
