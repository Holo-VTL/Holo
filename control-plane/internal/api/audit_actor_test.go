package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestSelfAssertedAuditActorLabelsClaimsAndRejectsUnsafeValues(t *testing.T) {
	got, err := selfAssertedAuditActor(" operator ")
	if err != nil || got != "self-asserted:operator" {
		t.Fatalf("expected explicit self-asserted actor label, got %q, %v", got, err)
	}
	got, err = selfAssertedAuditActor("")
	if err != nil || got != "self-asserted:unspecified" {
		t.Fatalf("expected unspecified external actor label, got %q, %v", got, err)
	}
	for _, actor := range []string{"operator\nforged", "operator\rforged", strings.Repeat("a", maxClaimedAuditActorLength+1)} {
		if _, err := selfAssertedAuditActor(actor); !errors.Is(err, domain.ErrInvalidInput) {
			t.Fatalf("expected unsafe actor %q to be rejected, got %v", actor, err)
		}
	}
}
