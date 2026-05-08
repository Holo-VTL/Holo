package config

import (
	"log"
	"os"
	"strconv"

	"github.com/Holo-VTL/Holo/control-plane/internal/metadata"
)

type Config struct {
	HTTPAddr             string
	APIKey               string
	MetadataDSN          string
	LogDir               string
	TelemetryTarget      string
	TargetRuntimeMode    string
	TargetPortalHost     string
	TargetPortalPort     int
	TargetBackstoreDir   string
	TargetBackstoreSize  int
	TargetRuntimeUseSudo bool
	WebUIDistDir         string
	TrustedProxyCIDRs    string
}

func Load() Config {
	return Config{
		HTTPAddr:             getenv("HOLO_HTTP_ADDR", "127.0.0.1:80"),
		APIKey:               getenv("HOLO_API_KEY", ""),
		MetadataDSN:          getenv("HOLO_METADATA_DSN", metadata.DefaultDSN),
		LogDir:               getenv("HOLO_LOG_DIR", "/var/log/holo"),
		TelemetryTarget:      getenv("HOLO_TELEMETRY_TARGET", "stdout"),
		TargetRuntimeMode:    getenv("HOLO_TARGET_RUNTIME_MODE", "in-memory"),
		TargetPortalHost:     getenv("HOLO_TARGET_PORTAL_HOST", "127.0.0.1"),
		TargetPortalPort:     getenvInt("HOLO_TARGET_PORTAL_PORT", 3260),
		TargetBackstoreDir:   getenv("HOLO_TARGET_BACKSTORE_DIR", "/var/lib/holo/targets"),
		TargetBackstoreSize:  getenvInt("HOLO_TARGET_BACKSTORE_SIZE_MB", 64),
		TargetRuntimeUseSudo: getenvBool("HOLO_TARGET_RUNTIME_USE_SUDO", true),
		WebUIDistDir:         getenv("HOLO_WEB_UI_DIST", "./web-console/dist"),
		TrustedProxyCIDRs:    getenv("HOLO_TRUSTED_PROXY_CIDRS", ""),
	}
}

func getenv(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func getenvInt(k string, fallback int) int {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("invalid integer environment value for %s; using default", k)
		return fallback
	}
	return n
}

func getenvBool(k string, fallback bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.Printf("invalid boolean environment value for %s; using default", k)
		return fallback
	}
	return b
}
