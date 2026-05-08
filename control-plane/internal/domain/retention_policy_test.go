package domain

import (
	"testing"
	"time"
)

func TestRetentionPolicyValidation(t *testing.T) {
	p := NewRetentionPolicy("r1", "car-1", RetentionModeWORM, time.Now().UTC().Add(time.Hour), "ops")
	if err := p.Validate(); err != nil {
		t.Fatalf("expected valid policy: %v", err)
	}
}
