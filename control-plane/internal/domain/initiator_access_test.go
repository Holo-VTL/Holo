package domain

import (
	"testing"
	"time"
)

func TestInitiatorRuleValidate(t *testing.T) {
	rule := InitiatorRule{
		RuleID:        "rule-1",
		PublicationID: "pub-1",
		Initiator:     "iqn.1993-08.org.debian:01:abc",
		Permission:    PermissionAllow,
		Priority:      10,
		CreatedAt:     time.Now().UTC(),
	}
	if err := rule.Validate(); err != nil {
		t.Fatalf("expected valid rule: %v", err)
	}

	rule.RuleID = ""
	if err := rule.Validate(); err == nil {
		t.Fatal("expected invalid rule error")
	}
}
