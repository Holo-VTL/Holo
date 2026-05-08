package config

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/metadata"
)

func TestLoad_DefaultAPIKeyIsEmpty(t *testing.T) {
	t.Setenv("HOLO_API_KEY", "")
	cfg := Load()
	if cfg.APIKey != "" {
		t.Fatalf("expected empty API key default, got %q", cfg.APIKey)
	}
}

func TestLoad_DefaultHTTPAddrUsesPort80(t *testing.T) {
	t.Setenv("HOLO_HTTP_ADDR", "")
	cfg := Load()
	if cfg.HTTPAddr != "127.0.0.1:80" {
		t.Fatalf("expected default http addr on port 80, got %q", cfg.HTTPAddr)
	}
}

func TestLoad_DefaultMetadataDSNIsLocalCatalog(t *testing.T) {
	t.Setenv("HOLO_METADATA_DSN", "")
	cfg := Load()
	if cfg.MetadataDSN != metadata.DefaultDSN {
		t.Fatalf("expected default metadata dsn %q, got %q", metadata.DefaultDSN, cfg.MetadataDSN)
	}
}

func TestLoad_UsesProvidedMetadataDSN(t *testing.T) {
	t.Setenv("HOLO_METADATA_DSN", "/tmp/holo-test.db")
	cfg := Load()
	if cfg.MetadataDSN != "/tmp/holo-test.db" {
		t.Fatalf("expected configured metadata dsn, got %q", cfg.MetadataDSN)
	}
}

func TestLoad_UsesProvidedAPIKey(t *testing.T) {
	t.Setenv("HOLO_API_KEY", "unit-test-key")
	cfg := Load()
	if cfg.APIKey != "unit-test-key" {
		t.Fatalf("expected configured API key, got %q", cfg.APIKey)
	}
}

func TestLoad_UsesTrustedProxyCIDRs(t *testing.T) {
	t.Setenv("HOLO_TRUSTED_PROXY_CIDRS", "127.0.0.1/32,10.0.0.0/8")
	cfg := Load()
	if cfg.TrustedProxyCIDRs != "127.0.0.1/32,10.0.0.0/8" {
		t.Fatalf("expected trusted proxy CIDRs to load, got %q", cfg.TrustedProxyCIDRs)
	}
}

func TestLoad_InvalidNumericAndBoolEnvLogAndFallback(t *testing.T) {
	t.Setenv("HOLO_TARGET_PORTAL_PORT", "bad-port\nsecret")
	t.Setenv("HOLO_TARGET_RUNTIME_USE_SUDO", "not-bool\nsecret")

	var buf bytes.Buffer
	original := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(original)

	cfg := Load()
	if cfg.TargetPortalPort != 3260 {
		t.Fatalf("expected invalid portal port to use fallback, got %d", cfg.TargetPortalPort)
	}
	if !cfg.TargetRuntimeUseSudo {
		t.Fatalf("expected invalid sudo flag to use true fallback")
	}
	got := buf.String()
	if !strings.Contains(got, "HOLO_TARGET_PORTAL_PORT") || !strings.Contains(got, "HOLO_TARGET_RUNTIME_USE_SUDO") {
		t.Fatalf("expected parse failure logs for invalid env vars, got %q", got)
	}
	if strings.Contains(got, "secret") {
		t.Fatalf("parse failure log should not include raw env values, got %q", got)
	}
}
