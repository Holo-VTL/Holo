package audit

import (
	"errors"
	"strings"
	"unicode"
)

const (
	SystemActor        = "system"
	selfAssertedPrefix = "self-asserted:"
	maxActorClaimBytes = 128
)

var ErrInvalidActorClaim = errors.New("invalid audit actor claim")

// SelfAssertedActor validates and labels a request-supplied actor claim.
// The result is attribution text, not an authenticated identity.
func SelfAssertedActor(actor string) (string, error) {
	for _, r := range actor {
		if unicode.IsControl(r) {
			return "", ErrInvalidActorClaim
		}
	}
	actor = strings.TrimSpace(actor)
	if len(actor) > maxActorClaimBytes {
		return "", ErrInvalidActorClaim
	}
	if actor == "" {
		return selfAssertedPrefix + "unspecified", nil
	}
	return selfAssertedPrefix + actor, nil
}

// NormalizeServiceActor labels non-system actors reaching audit emitters from
// service calls. HTTP handlers must validate and label untrusted claims before
// calling services so a request cannot claim the reserved system actor.
func NormalizeServiceActor(actor string) string {
	actor = strings.TrimSpace(actor)
	if actor == "" || actor == SystemActor {
		return SystemActor
	}
	if strings.HasPrefix(actor, selfAssertedPrefix) {
		actor = strings.TrimPrefix(actor, selfAssertedPrefix)
	}
	normalized, err := SelfAssertedActor(actor)
	if err != nil {
		return selfAssertedPrefix + "invalid"
	}
	return normalized
}
