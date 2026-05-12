package config

import (
	"errors"
	"fmt"
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
	cfg, _ := load(false)
	return cfg
}

func LoadE() (Config, error) {
	return load(true)
}

func load(strict bool) (Config, error) {
	targetPortalPort, portalErr := getenvInt("HOLO_TARGET_PORTAL_PORT", 3260, strict)
	targetBackstoreSize, backstoreErr := getenvInt("HOLO_TARGET_BACKSTORE_SIZE_MB", 64, strict)
	targetRuntimeUseSudo, sudoErr := getenvBool("HOLO_TARGET_RUNTIME_USE_SUDO", true, strict)
	return Config{
		HTTPAddr:             getenv("HOLO_HTTP_ADDR", "127.0.0.1:80"),
		APIKey:               getenv("HOLO_API_KEY", ""),
		MetadataDSN:          getenv("HOLO_METADATA_DSN", metadata.DefaultDSN),
		LogDir:               getenv("HOLO_LOG_DIR", "/var/log/holo"),
		TelemetryTarget:      getenv("HOLO_TELEMETRY_TARGET", "stdout"),
		TargetRuntimeMode:    getenv("HOLO_TARGET_RUNTIME_MODE", "in-memory"),
		TargetPortalHost:     getenv("HOLO_TARGET_PORTAL_HOST", "127.0.0.1"),
		TargetPortalPort:     targetPortalPort,
		TargetBackstoreDir:   getenv("HOLO_TARGET_BACKSTORE_DIR", "/var/lib/holo/targets"),
		TargetBackstoreSize:  targetBackstoreSize,
		TargetRuntimeUseSudo: targetRuntimeUseSudo,
		WebUIDistDir:         getenv("HOLO_WEB_UI_DIST", "./web-console/dist"),
		TrustedProxyCIDRs:    getenv("HOLO_TRUSTED_PROXY_CIDRS", ""),
	}, errors.Join(portalErr, backstoreErr, sudoErr)
}

func getenv(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func getenvInt(k string, fallback int, strict bool) (int, error) {
	v := os.Getenv(k)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		err = fmt.Errorf("invalid integer environment value for %s=%q", k, v)
		if strict {
			return fallback, err
		}
		log.Printf("%v; using default", err)
		return fallback, nil
	}
	return n, nil
}

func getenvBool(k string, fallback bool, strict bool) (bool, error) {
	v := os.Getenv(k)
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		err = fmt.Errorf("invalid boolean environment value for %s=%q", k, v)
		if strict {
			return fallback, err
		}
		log.Printf("%v; using default", err)
		return fallback, nil
	}
	return b, nil
}
