package domain

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrInvalidState     = errors.New("invalid state transition")
	ErrInvalidInput     = errors.New("invalid input")
	ErrNotFound         = errors.New("resource not found")
	ErrConflict         = errors.New("resource conflict")
	ErrIdentityConflict = fmt.Errorf("%w: identity_conflict", ErrConflict)
	ErrAmbiguousLayout  = fmt.Errorf("%w: ambiguous_layout", ErrConflict)
	ErrUnauthorized     = errors.New("unauthorized")
	ErrRetentionLock    = errors.New("retention lock active")
	ErrCapacityExceeded = errors.New("insufficient storage capacity")
)

type Timestamped struct {
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type OperationResult string

const (
	ResultSuccess OperationResult = "success"
	ResultFailure OperationResult = "failure"
)
