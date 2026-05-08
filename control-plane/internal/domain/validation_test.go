package domain

import "testing"

func TestValidateProfileTokenAcceptsWebConsolePresetNames(t *testing.T) {
	valid := []string{
		"IBM TS3500",
		"Quantum Scalar i500",
		"Quantum LTO-5 Tape Drive",
		"HP/HPE MSL2024",
		"Oracle / StorageTek",
		"Spectra TFinity ExaScale",
	}
	for _, value := range valid {
		if err := ValidateProfileToken(value); err != nil {
			t.Fatalf("ValidateProfileToken(%q) returned %v, want nil", value, err)
		}
	}
}

func TestValidateProfileTokenRejectsUnsafeValues(t *testing.T) {
	invalid := []string{
		"bad\nprofile",
		"../profile",
		"/absolute",
		"trailing/",
	}
	for _, value := range invalid {
		if err := ValidateProfileToken(value); err == nil {
			t.Fatalf("ValidateProfileToken(%q) returned nil, want error", value)
		}
	}
}
