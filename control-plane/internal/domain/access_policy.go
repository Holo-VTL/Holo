package domain

import "time"

type PolicyScope string
type PolicyPermission string

const (
	ScopeGlobal  PolicyScope = "global"
	ScopeLibrary PolicyScope = "library"
	ScopeDrive   PolicyScope = "drive"
)

const (
	PermissionAllow PolicyPermission = "allow"
	PermissionDeny  PolicyPermission = "deny"
)

type TargetAccessPolicy struct {
	PolicyID      string           `json:"policyId"`
	Scope         PolicyScope      `json:"scope"`
	Subject       string           `json:"subject"`
	Permission    PolicyPermission `json:"permission"`
	EffectiveFrom time.Time        `json:"effectiveFrom"`
	EffectiveTo   time.Time        `json:"effectiveTo"`
}

func (p TargetAccessPolicy) Validate() error {
	if p.PolicyID == "" || p.Subject == "" {
		return ErrInvalidInput
	}
	if ValidatePermission(p.Permission) != nil {
		return ErrInvalidInput
	}
	if !p.EffectiveTo.IsZero() && p.EffectiveTo.Before(p.EffectiveFrom) {
		return ErrInvalidInput
	}
	return nil
}
