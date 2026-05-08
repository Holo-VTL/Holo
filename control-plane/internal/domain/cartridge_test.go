package domain

import "testing"

func TestCartridgeLifecycleTransitions(t *testing.T) {
	c := NewVirtualCartridge("car-1", "pool-1", "lib-1", "B001", 1024)
	if err := c.TransitionTo(CartridgeMounted); err != nil {
		t.Fatalf("mount transition failed: %v", err)
	}
	if err := c.TransitionTo(CartridgeAvailable); err != nil {
		t.Fatalf("return to available failed: %v", err)
	}
	if err := c.TransitionTo(CartridgeRetired); err != nil {
		t.Fatalf("retire transition failed: %v", err)
	}
}
