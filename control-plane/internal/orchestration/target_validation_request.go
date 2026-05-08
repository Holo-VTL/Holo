package orchestration

import (
	"strings"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

type ValidationRunRequest struct {
	Mode    domain.ValidationMode `json:"mode"`
	Bytes   int64                 `json:"bytes,omitempty"`
	Pattern string                `json:"pattern,omitempty"`
}

func (r ValidationRunRequest) Normalize() ValidationRunRequest {
	mode := r.Mode
	if mode == "" {
		mode = domain.ValidationModeFixed
	}
	pattern := strings.TrimSpace(r.Pattern)
	if pattern == "" {
		pattern = "HOLO"
	}
	return ValidationRunRequest{
		Mode:    mode,
		Bytes:   r.Bytes,
		Pattern: pattern,
	}
}

func (r ValidationRunRequest) Validate() error {
	normalized := r.Normalize()
	switch normalized.Mode {
	case domain.ValidationModeFixed:
		if normalized.Bytes < 0 {
			return domain.ErrInvalidInput
		}
	case domain.ValidationModeEmpty:
		if normalized.Bytes < 0 || normalized.Bytes > 0 {
			return domain.ErrInvalidInput
		}
	default:
		return domain.ErrInvalidInput
	}
	return nil
}
