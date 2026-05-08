package domain

import "testing"

func TestTargetDiscoveryRequestValidate(t *testing.T) {
	valid := TargetDiscoveryRequest{Initiator: "iqn.1993-08.org.debian:01:test", Portal: "127.0.0.1:3260"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("expected valid request: %v", err)
	}

	invalidInitiator := TargetDiscoveryRequest{Initiator: "   "}
	if err := invalidInitiator.Validate(); err == nil {
		t.Fatal("expected invalid initiator error")
	}

	invalidPortal := TargetDiscoveryRequest{Initiator: "iqn.1993-08.org.debian:01:test", Portal: "127.0.0.1 :3260"}
	if err := invalidPortal.Validate(); err == nil {
		t.Fatal("expected invalid portal error")
	}
}
