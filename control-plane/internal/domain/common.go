package domain

import (
	"errors"
	"time"
)

var (
	ErrInvalidState     = errors.New("invalid state transition")
	ErrInvalidInput     = errors.New("invalid input")
	ErrNotFound         = errors.New("resource not found")
	ErrConflict         = errors.New("resource conflict")
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
