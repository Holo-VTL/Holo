package api

import (
	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func selfAssertedAuditActor(actor string) (string, error) {
	value, err := audit.SelfAssertedActor(actor)
	if err != nil {
		return "", domain.ErrInvalidInput
	}
	return value, nil
}
