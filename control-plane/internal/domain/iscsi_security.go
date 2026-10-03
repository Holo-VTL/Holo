package domain

import (
	"strings"
	"time"
	"unicode/utf8"
)

const maxISCSISecurityBlobBytes = 1 << 20

type ISCSICredential struct {
	CredentialID    string    `json:"credentialId"`
	Label           string    `json:"label"`
	Username        string    `json:"username"`
	MutualUsername  string    `json:"mutualUsername,omitempty"`
	EncryptedSecret []byte    `json:"-"`
	Version         int64     `json:"version"`
	CreatedAt       time.Time `json:"createdAt"`
}

func (c ISCSICredential) Validate() error {
	if ValidateManagementID(c.CredentialID) != nil || ValidateManagementLabel(c.Label, true) != nil || len(c.Label) > 128 ||
		!validISCSIToken(c.Username, 255) ||
		(c.MutualUsername != "" && !validISCSIToken(c.MutualUsername, 255)) ||
		len(c.EncryptedSecret) < 33 || len(c.EncryptedSecret) > maxISCSISecurityBlobBytes || c.Version <= 0 {
		return ErrInvalidInput
	}
	return nil
}

type ISCSISecuritySnapshot struct {
	SnapshotID     string    `json:"snapshotId"`
	Scope          string    `json:"scope"`
	OwnerID        string    `json:"ownerId"`
	Version        int64     `json:"version"`
	CredentialRefs []string  `json:"credentialRefs,omitempty"`
	Payload        []byte    `json:"payload"`
	CreatedBy      string    `json:"createdBy"`
	CreatedAt      time.Time `json:"createdAt"`
}

func (s ISCSISecuritySnapshot) Validate() error {
	if ValidateManagementID(s.SnapshotID) != nil || ValidateManagementID(s.OwnerID) != nil ||
		(s.Scope != string(SecurityScopeLibrary) && s.Scope != string(SecurityScopeDrive) && s.Scope != string(SecurityScopeTarget)) ||
		s.Version <= 0 || len(s.Payload) == 0 || len(s.Payload) > maxISCSISecurityBlobBytes {
		return ErrInvalidInput
	}
	return nil
}

type SecurityScope string
type ISCSIAuthMode string

const (
	SecurityScopeLibrary SecurityScope = "library"
	SecurityScopeDrive   SecurityScope = "drive"
	SecurityScopeTarget  SecurityScope = "target"
)

const (
	ISCSIAuthNone       ISCSIAuthMode = "none"
	ISCSIAuthCHAP       ISCSIAuthMode = "chap"
	ISCSIAuthMutualCHAP ISCSIAuthMode = "mutual_chap"
)

const maxISCSIInitiators = 256

type ISCSIAuthenticationPolicy struct {
	Mode               ISCSIAuthMode `json:"mode"`
	CredentialID       string        `json:"credentialId,omitempty"`
	Initiators         []string      `json:"initiators,omitempty"`
	RestrictInitiators bool          `json:"restrictInitiators,omitempty"`
}

func (p ISCSIAuthenticationPolicy) Validate() error {
	if len(p.Initiators) > maxISCSIInitiators {
		return ErrInvalidInput
	}
	seen := make(map[string]struct{}, len(p.Initiators))
	for _, raw := range p.Initiators {
		initiator := strings.ToLower(strings.TrimSpace(raw))
		if raw != strings.TrimSpace(raw) || ValidateTargetIQN(initiator) != nil {
			return ErrInvalidInput
		}
		if _, exists := seen[initiator]; exists {
			return ErrInvalidInput
		}
		seen[initiator] = struct{}{}
	}
	switch p.Mode {
	case ISCSIAuthNone:
		if p.CredentialID != "" {
			return ErrInvalidInput
		}
	case ISCSIAuthCHAP, ISCSIAuthMutualCHAP:
		if ValidateManagementID(p.CredentialID) != nil || len(p.Initiators) == 0 || p.RestrictInitiators {
			return ErrInvalidInput
		}
	default:
		return ErrInvalidInput
	}
	return nil
}

// ISCSISecret is private material only. API models must copy data from a
// separately decoded write-only request and must never marshal this value.
type ISCSISecret struct {
	Username       string `json:"-"`
	Secret         string `json:"-"`
	MutualUsername string `json:"-"`
	MutualSecret   string `json:"-"`
}

func (s ISCSISecret) Validate() error {
	if !validISCSIToken(s.Username, 255) || !validISCSISecret(s.Secret) {
		return ErrInvalidInput
	}
	if s.MutualUsername == "" && s.MutualSecret == "" {
		return nil
	}
	if !validISCSIToken(s.MutualUsername, 255) || !validISCSISecret(s.MutualSecret) || s.Secret == s.MutualSecret {
		return ErrInvalidInput
	}
	return nil
}

func validISCSIToken(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || strings.TrimSpace(value) != value ||
		strings.HasPrefix(strings.ToUpper(value), "NULL") {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e || r == ' ' {
			return false
		}
	}
	return true
}

func validISCSISecret(value string) bool {
	return len(value) >= 12 && len(value) <= 255 && utf8.ValidString(value) && validISCSIToken(value, 255)
}

type ISCSISecurityBinding struct {
	Scope                 SecurityScope              `json:"scope"`
	OwnerID               string                     `json:"ownerId"`
	LibraryID             string                     `json:"libraryId,omitempty"`
	DriveID               string                     `json:"driveId,omitempty"`
	TargetIQN             string                     `json:"targetIqn,omitempty"`
	DeviceRole            string                     `json:"deviceRole,omitempty"`
	Authentication        *ISCSIAuthenticationPolicy `json:"auth"`
	Generation            int64                      `json:"generation"`
	AdministrativeOffline bool                       `json:"administrativeOffline,omitempty"`
}

func (b ISCSISecurityBinding) Validate() error {
	if b.Generation <= 0 || ValidateManagementID(b.OwnerID) != nil && b.Scope != SecurityScopeTarget {
		return ErrInvalidInput
	}
	if b.Authentication != nil && b.Authentication.Validate() != nil {
		return ErrInvalidInput
	}
	switch b.Scope {
	case SecurityScopeLibrary:
		if ValidateManagementID(b.OwnerID) != nil || b.LibraryID != "" || b.DriveID != "" || b.TargetIQN != "" || b.DeviceRole != "" {
			return ErrInvalidInput
		}
	case SecurityScopeDrive:
		if ValidateManagementID(b.OwnerID) != nil || ValidateManagementID(b.LibraryID) != nil || b.DriveID != "" || b.TargetIQN != "" || b.DeviceRole != "" {
			return ErrInvalidInput
		}
	case SecurityScopeTarget:
		if ValidateTargetIQN(b.TargetIQN) != nil || strings.ToLower(b.OwnerID) != strings.ToLower(b.TargetIQN) || ValidateManagementID(b.LibraryID) != nil {
			return ErrInvalidInput
		}
		switch b.DeviceRole {
		case "drive":
			if ValidateManagementID(b.DriveID) != nil {
				return ErrInvalidInput
			}
		case "changer":
			if b.DriveID != "" {
				return ErrInvalidInput
			}
		default:
			return ErrInvalidInput
		}
	default:
		return ErrInvalidInput
	}
	return nil
}

type ISCSISecurityOrigin struct {
	Scope      string `json:"scope"`
	OwnerID    string `json:"ownerId,omitempty"`
	Generation int64  `json:"generation,omitempty"`
}

type ResolvedISCSISecurity struct {
	Authentication       ISCSIAuthenticationPolicy `json:"auth"`
	AuthenticationSource ISCSISecurityOrigin       `json:"authSource"`
	EffectiveRevision    string                    `json:"effectiveRevision"`
}
