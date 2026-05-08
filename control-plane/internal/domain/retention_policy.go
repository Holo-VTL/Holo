package domain

import "time"

type RetentionMode string

const (
	RetentionModeWORM       RetentionMode = "worm"
	RetentionModeGovernance RetentionMode = "governance"
)

type RetentionPolicy struct {
	RetentionID string        `json:"retentionId"`
	CartridgeID string        `json:"cartridgeId"`
	Mode        RetentionMode `json:"mode"`
	LockUntil   time.Time     `json:"lockUntil"`
	CreatedBy   string        `json:"createdBy"`
}

func NewRetentionPolicy(id, cartridgeID string, mode RetentionMode, lockUntil time.Time, actor string) RetentionPolicy {
	return RetentionPolicy{RetentionID: id, CartridgeID: cartridgeID, Mode: mode, LockUntil: lockUntil, CreatedBy: actor}
}

func (p RetentionPolicy) Validate() error {
	if p.RetentionID == "" || p.CartridgeID == "" || p.CreatedBy == "" {
		return ErrInvalidInput
	}
	if p.Mode != RetentionModeWORM && p.Mode != RetentionModeGovernance {
		return ErrInvalidInput
	}
	if !p.LockUntil.After(time.Now().UTC()) {
		return ErrInvalidInput
	}
	return nil
}
