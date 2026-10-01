package api

import (
	"strings"
	"unicode"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

const maxClaimedAuditActorLength = 128

func selfAssertedAuditActor(actor string) (string, error) {
	for _, r := range actor {
		if unicode.IsControl(r) {
			return "", domain.ErrInvalidInput
		}
	}
	actor = strings.TrimSpace(actor)
	if len(actor) > maxClaimedAuditActorLength {
		return "", domain.ErrInvalidInput
	}
	if actor == "" {
		return "self-asserted:unspecified", nil
	}
	return "self-asserted:" + actor, nil
}
