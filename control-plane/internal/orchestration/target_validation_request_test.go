package orchestration

import (
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestValidationRunRequestValidate(t *testing.T) {
	if err := (ValidationRunRequest{Mode: domain.ValidationModeFixed, Bytes: 4096}).Validate(); err != nil {
		t.Fatalf("expected fixed mode valid, got %v", err)
	}
	if err := (ValidationRunRequest{Mode: domain.ValidationModeEmpty}).Validate(); err != nil {
		t.Fatalf("expected empty mode valid, got %v", err)
	}
	if err := (ValidationRunRequest{Mode: "unknown"}).Validate(); err == nil {
		t.Fatal("expected unknown mode invalid")
	}
	if err := (ValidationRunRequest{Mode: domain.ValidationModeFixed, Bytes: -1}).Validate(); err == nil {
		t.Fatal("expected negative bytes invalid")
	}
}

func TestValidationRunRequestNormalizeDefaults(t *testing.T) {
	normalized := (ValidationRunRequest{}).Normalize()
	if normalized.Mode != domain.ValidationModeFixed {
		t.Fatalf("expected default mode fixed, got %s", normalized.Mode)
	}
	if normalized.Pattern != "HOLO" {
		t.Fatalf("expected default pattern HOLO, got %s", normalized.Pattern)
	}
}
