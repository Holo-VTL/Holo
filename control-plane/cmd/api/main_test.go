package main

import (
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/config"
)

func validStartupTestConfig() config.Config {
	return config.Config{
		MetadataDSN:       "file:test.db",
		TargetRuntimeMode: "in-memory",
		TargetPortalPort:  3260,
	}
}

func TestValidateStartupConfigAcceptsInternalNoLoginMode(t *testing.T) {
	cfg := validStartupTestConfig()
	cfg.APIKey = ""
	if err := validateStartupConfig(cfg); err != nil {
		t.Fatalf("expected empty API key to allow internal no-login mode, got %v", err)
	}
}

func TestValidateStartupConfigAcceptsWhitespaceAPIKey(t *testing.T) {
	cfg := validStartupTestConfig()
	cfg.APIKey = "   "
	if err := validateStartupConfig(cfg); err != nil {
		t.Fatalf("expected whitespace API key to allow internal no-login mode, got %v", err)
	}
}

func TestValidateStartupConfigAcceptsNonEmptyAPIKey(t *testing.T) {
	cfg := validStartupTestConfig()
	cfg.APIKey = "test-api-key"
	if err := validateStartupConfig(cfg); err != nil {
		t.Fatalf("expected valid api key to pass, got %v", err)
	}
}

func TestValidateStartupConfigRejectsInvalidRuntimeMode(t *testing.T) {
	cfg := validStartupTestConfig()
	cfg.TargetRuntimeMode = "unsupported"
	if err := validateStartupConfig(cfg); err == nil {
		t.Fatal("expected invalid runtime mode to fail")
	}
}

func TestValidateStartupConfigRejectsInvalidPortalPort(t *testing.T) {
	cfg := validStartupTestConfig()
	cfg.TargetPortalPort = 70000
	if err := validateStartupConfig(cfg); err == nil {
		t.Fatal("expected invalid portal port to fail")
	}
}
