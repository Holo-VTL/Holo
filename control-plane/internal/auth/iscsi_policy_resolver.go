package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

type securityBlockSource struct {
	policy *domain.ISCSISecurityBinding
	name   string
}

func ResolveISCSISecurity(
	deviceRole, targetIQN, libraryID, driveID string,
	target, drive, library *domain.ISCSISecurityBinding,
) (domain.ResolvedISCSISecurity, error) {
	if domain.ValidateTargetIQN(targetIQN) != nil || domain.ValidateManagementID(libraryID) != nil {
		return domain.ResolvedISCSISecurity{}, domain.ErrInvalidInput
	}
	if deviceRole != "drive" && deviceRole != "changer" {
		return domain.ResolvedISCSISecurity{}, domain.ErrInvalidInput
	}
	if deviceRole == "drive" && domain.ValidateManagementID(driveID) != nil {
		return domain.ResolvedISCSISecurity{}, domain.ErrInvalidInput
	}
	if library != nil {
		if library.Validate() != nil || library.Scope != domain.SecurityScopeLibrary || library.OwnerID != libraryID {
			return domain.ResolvedISCSISecurity{}, domain.ErrInvalidInput
		}
	}
	if deviceRole == "drive" && drive != nil {
		if drive.Validate() != nil || drive.Scope != domain.SecurityScopeDrive || drive.OwnerID != driveID || drive.LibraryID != libraryID {
			return domain.ResolvedISCSISecurity{}, domain.ErrInvalidInput
		}
	}
	if target != nil {
		if target.Validate() != nil || target.Scope != domain.SecurityScopeTarget ||
			target.TargetIQN != targetIQN || target.LibraryID != libraryID || target.DeviceRole != deviceRole {
			return domain.ResolvedISCSISecurity{}, domain.ErrInvalidInput
		}
		if deviceRole == "drive" && target.DriveID != driveID {
			return domain.ResolvedISCSISecurity{}, domain.ErrInvalidInput
		}
	}

	authSources := []securityBlockSource{{name: "default"}}
	if library != nil {
		authSources = append([]securityBlockSource{{policy: library, name: "library"}}, authSources...)
	}
	if deviceRole == "drive" && drive != nil {
		authSources = append([]securityBlockSource{{policy: drive, name: "drive"}}, authSources...)
	}
	if target != nil {
		authSources = append([]securityBlockSource{{policy: target, name: "target"}}, authSources...)
	}

	resolved := domain.ResolvedISCSISecurity{
		Authentication:       domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthNone},
		AuthenticationSource: domain.ISCSISecurityOrigin{Scope: "default"},
	}
	for _, source := range authSources {
		if source.policy == nil || source.policy.Authentication == nil {
			continue
		}
		resolved.Authentication = *source.policy.Authentication
		resolved.Authentication.Initiators = append([]string(nil), source.policy.Authentication.Initiators...)
		resolved.AuthenticationSource = origin(source)
		break
	}
	revisionInput := struct {
		TargetIQN string
		Role      string
		Auth      domain.ISCSIAuthenticationPolicy
		AuthFrom  domain.ISCSISecurityOrigin
	}{targetIQN, deviceRole, resolved.Authentication, resolved.AuthenticationSource}
	// Initiator order has no policy meaning; canonicalize it for a stable revision.
	sort.Strings(revisionInput.Auth.Initiators)
	payload, err := json.Marshal(revisionInput)
	if err != nil {
		return domain.ResolvedISCSISecurity{}, domain.ErrInvalidInput
	}
	hash := sha256.Sum256(payload)
	resolved.EffectiveRevision = hex.EncodeToString(hash[:])
	return resolved, nil
}

func origin(source securityBlockSource) domain.ISCSISecurityOrigin {
	if source.policy == nil {
		return domain.ISCSISecurityOrigin{Scope: "default"}
	}
	return domain.ISCSISecurityOrigin{
		Scope:      source.name,
		OwnerID:    source.policy.OwnerID,
		Generation: source.policy.Generation,
	}
}
