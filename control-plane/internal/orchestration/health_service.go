package orchestration

import (
	"context"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/metrics"
)

type ComponentHealth struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type HealthSummary struct {
	Status     string            `json:"status"`
	Components []ComponentHealth `json:"components"`
}

type HealthService struct {
	targetProvider  TargetRuntimeHealthProvider
	accessProvider  TargetAccessHealthProvider
	discovery       TargetDiscoveryHealthProvider
	metricsRegistry *metrics.MetricsRegistry
	metadataDSN     string
	runtimeMode     string
	runDir          string
}

type TargetAccessHealthProvider interface {
	AccessPolicySnapshotCount() int
}

func NewHealthService(provider TargetRuntimeHealthProvider, accessProvider TargetAccessHealthProvider, discoveryProvider TargetDiscoveryHealthProvider, m *metrics.MetricsRegistry) *HealthService {
	return NewHealthServiceWithConfig(provider, accessProvider, discoveryProvider, m, "", "")
}

func NewHealthServiceWithConfig(provider TargetRuntimeHealthProvider, accessProvider TargetAccessHealthProvider, discoveryProvider TargetDiscoveryHealthProvider, m *metrics.MetricsRegistry, metadataDSN, runtimeMode string) *HealthService {
	return &HealthService{
		targetProvider:  provider,
		accessProvider:  accessProvider,
		discovery:       discoveryProvider,
		metricsRegistry: m,
		metadataDSN:     strings.TrimSpace(metadataDSN),
		runtimeMode:     strings.ToLower(strings.TrimSpace(runtimeMode)),
		runDir:          envOr("HOLO_RUN_DIR", "/run/holo"),
	}
}

func (s *HealthService) Summary() HealthSummary {
	db := s.databaseComponent()
	dp := s.dataPlaneComponent()
	tcmu := s.tcmuRunnerComponent()
	components := []ComponentHealth{
		db,
		dp,
		tcmu,
		{Name: "control-plane", Status: "ok"},
	}
	if s.metricsRegistry != nil {
		if atomic.LoadInt64(&s.metricsRegistry.AuditJournalFailed) != 0 {
			components = append(components, ComponentHealth{
				Name:    "audit-log",
				Status:  "down",
				Message: "persistent audit journal write failed; events may only be in memory",
			})
		} else {
			components = append(components, ComponentHealth{Name: "audit-log", Status: "ok"})
		}
	}
	if s.targetProvider != nil {
		snap := s.targetProvider.HealthSnapshot()
		components = append(components, ComponentHealth{
			Name:    "target-runtime",
			Status:  "healthy",
			Message: "total=" + itoa(snap.TotalPublications) + ",ready=" + itoa(snap.ReadyPublications) + ",failed=" + itoa(snap.FailedPublications) + ",disabled=" + itoa(snap.DisabledPublications),
		})
	}
	if s.accessProvider != nil {
		components = append(components, ComponentHealth{
			Name:    "target-access-policy",
			Status:  "healthy",
			Message: "snapshots=" + itoa(s.accessProvider.AccessPolicySnapshotCount()),
		})
	}
	if s.discovery != nil {
		snap := s.discovery.DiscoverySnapshot()
		components = append(components, ComponentHealth{
			Name:    "target-discovery",
			Status:  "healthy",
			Message: "queries=" + itoa(snap.TotalQueries) + ",lastVisible=" + itoa(snap.LastVisible) + ",discoverableNow=" + itoa(snap.DiscoverableNow),
		})
	}
	overall := "healthy"
	for _, c := range components {
		if c.Status == "down" {
			overall = "degraded"
			break
		}
	}

	if s.metricsRegistry != nil {
		val := int64(0)
		if overall == "healthy" {
			val = 1
		}
		atomic.StoreInt64(&s.metricsRegistry.HealthStatus, val)
	}

	return HealthSummary{
		Status:     overall,
		Components: components,
	}
}

func (s *HealthService) databaseComponent() ComponentHealth {
	if s.metadataDSN == "" {
		return ComponentHealth{Name: "database", Status: "unknown", Message: "metadata dsn not configured"}
	}
	u, err := url.Parse(s.metadataDSN)
	if err != nil {
		return ComponentHealth{Name: "database", Status: "unknown", Message: "invalid metadata dsn"}
	}
	if u.Scheme == "" || u.Scheme == "file" {
		path := u.Path
		if path == "" {
			path = s.metadataDSN
		}
		if _, err := os.Stat(path); err != nil {
			return ComponentHealth{Name: "database", Status: "down", Message: err.Error()}
		}
		return ComponentHealth{Name: "database", Status: "ok"}
	}
	if u.Host == "" {
		return ComponentHealth{Name: "database", Status: "unknown", Message: "invalid metadata dsn"}
	}
	hostport := u.Host
	if !strings.Contains(hostport, ":") {
		hostport = net.JoinHostPort(hostport, "5432")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", hostport)
	if err != nil {
		return ComponentHealth{Name: "database", Status: "down", Message: err.Error()}
	}
	_ = conn.Close()
	return ComponentHealth{Name: "database", Status: "ok"}
}

func (s *HealthService) dataPlaneComponent() ComponentHealth {
	runDir := strings.TrimSpace(s.runDir)
	if runDir == "" {
		runDir = "/run/holo"
	}
	entries, err := os.ReadDir(runDir)
	if err != nil {
		if os.IsNotExist(err) {
			return ComponentHealth{Name: "dataPlane", Status: "unknown", Message: runDir + " not present"}
		}
		return ComponentHealth{Name: "dataPlane", Status: "unknown", Message: err.Error()}
	}
	var socketErr error
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sock") {
			socketPath := filepath.Join(runDir, e.Name())
			conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return ComponentHealth{Name: "dataPlane", Status: "ok", Message: socketPath}
			}
			socketErr = err
		}
	}
	if socketErr != nil {
		return ComponentHealth{Name: "dataPlane", Status: "down", Message: "cdb socket unreachable: " + socketErr.Error()}
	}
	if s.targetProvider != nil && s.targetProvider.HealthSnapshot().TotalPublications == 0 {
		return ComponentHealth{Name: "dataPlane", Status: "unknown", Message: "no active publications"}
	}
	return ComponentHealth{Name: "dataPlane", Status: "down", Message: "no active cdb socket"}
}

func (s *HealthService) tcmuRunnerComponent() ComponentHealth {
	if s.runtimeMode != "tcmu" {
		return ComponentHealth{Name: "tcmuRunner", Status: "unknown", Message: "runtime mode is not tcmu"}
	}
	if _, err := exec.LookPath("pgrep"); err != nil {
		return ComponentHealth{Name: "tcmuRunner", Status: "unknown", Message: "pgrep not available"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "pgrep", "-x", "tcmu-runner").Run(); err != nil {
		return ComponentHealth{Name: "tcmuRunner", Status: "down", Message: "tcmu-runner process not found"}
	}
	return ComponentHealth{Name: "tcmuRunner", Status: "ok"}
}

func itoa(v int) string {
	return strconv.Itoa(v)
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
