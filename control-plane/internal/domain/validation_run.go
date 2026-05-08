package domain

import "time"

type ValidationStatus string

type ValidationScenario string
type ValidationMode string

const (
	ValidationScenarioMinimalWriteRead ValidationScenario = "minimal_write_read"
	ValidationModeFixed                ValidationMode     = "fixed"
	ValidationModeEmpty                ValidationMode     = "empty"

	ValidationRunning ValidationStatus = "running"
	ValidationPassed  ValidationStatus = "passed"
	ValidationFailed  ValidationStatus = "failed"
)

type ValidationRun struct {
	ValidationID  string             `json:"validationId"`
	PublicationID string             `json:"publicationId"`
	Scenario      ValidationScenario `json:"scenario"`
	Status        ValidationStatus   `json:"status"`
	Mode          ValidationMode     `json:"mode"`
	BytesWritten  int64              `json:"bytesWritten"`
	BytesRead     int64              `json:"bytesRead"`
	WriteDigest   string             `json:"writeDigest,omitempty"`
	ReadDigest    string             `json:"readDigest,omitempty"`
	EvidencePath  string             `json:"evidencePath"`
	StartedAt     time.Time          `json:"startedAt"`
	FinishedAt    *time.Time         `json:"finishedAt,omitempty"`
}

func NewValidationRun(validationID, publicationID string) (*ValidationRun, error) {
	if validationID == "" || publicationID == "" {
		return nil, ErrInvalidInput
	}
	return &ValidationRun{
		ValidationID:  validationID,
		PublicationID: publicationID,
		Scenario:      ValidationScenarioMinimalWriteRead,
		Mode:          ValidationModeFixed,
		Status:        ValidationRunning,
		StartedAt:     time.Now().UTC(),
	}, nil
}

func (v *ValidationRun) Complete(written, read int64, writeDigest, readDigest, evidencePath string) error {
	if v.Status != ValidationRunning {
		return ErrInvalidState
	}
	if written < 0 || read < 0 {
		return ErrInvalidInput
	}
	now := time.Now().UTC()
	v.BytesWritten = written
	v.BytesRead = read
	v.WriteDigest = writeDigest
	v.ReadDigest = readDigest
	v.EvidencePath = evidencePath
	v.FinishedAt = &now
	if written == read && writeDigest == readDigest && writeDigest != "" {
		v.Status = ValidationPassed
		return nil
	}
	v.Status = ValidationFailed
	return nil
}
