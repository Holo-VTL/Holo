package audit

import (
	"errors"
	"strings"
	"testing"
)

func TestSelfAssertedActorValidatesAndLabelsClaims(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "trimmed claim", input: " operator ", want: "self-asserted:operator"},
		{name: "empty claim", input: "", want: "self-asserted:unspecified"},
		{name: "system claim remains untrusted", input: "system", want: "self-asserted:system"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SelfAssertedActor(tt.input)
			if err != nil || got != tt.want {
				t.Fatalf("SelfAssertedActor(%q) = %q, %v; want %q", tt.input, got, err, tt.want)
			}
		})
	}
	for _, input := range []string{"operator\nforged", "operator\rforged", strings.Repeat("a", maxActorClaimBytes+1)} {
		if _, err := SelfAssertedActor(input); !errors.Is(err, ErrInvalidActorClaim) {
			t.Fatalf("SelfAssertedActor(%q) error = %v; want ErrInvalidActorClaim", input, err)
		}
	}
}

func TestNormalizeServiceActorLabelsClaimsAndPreservesSystem(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "operator", want: "self-asserted:operator"},
		{input: "self-asserted:operator", want: "self-asserted:operator"},
		{input: "system", want: "system"},
		{input: "", want: "system"},
		{input: "unsafe\nclaim", want: "self-asserted:invalid"},
	}
	for _, tt := range tests {
		if got := NormalizeServiceActor(tt.input); got != tt.want {
			t.Errorf("NormalizeServiceActor(%q) = %q; want %q", tt.input, got, tt.want)
		}
	}
}
