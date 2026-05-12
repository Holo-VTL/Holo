package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/auth"
	"github.com/Holo-VTL/Holo/control-plane/internal/config"
	"github.com/Holo-VTL/Holo/control-plane/internal/metrics"
	"github.com/Holo-VTL/Holo/control-plane/internal/orchestration"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo/memory"
	sqliterepo "github.com/Holo-VTL/Holo/control-plane/internal/repo/sqlite"
	"github.com/Holo-VTL/Holo/control-plane/internal/storageutil"
	"github.com/Holo-VTL/Holo/control-plane/internal/tracing"
)

type Server struct {
	mux        *http.ServeMux
	uiDistDir  string
	resources  *ResourcesHandler
	storage    *StorageHandler
	policy     *PolicyHandler
	ops        *OpsHandler
	targets    *TargetHandler
	access     *TargetAccessHandler
	discovery  *TargetDiscoveryHandler
	metricsHD  *MetricsHandler
	auditHD    *AuditHandler
	runtime    *orchestration.TargetRuntimeService
	apiKey     string
	metadataDB *sql.DB
	limiter    *rateLimiter
	journal    *audit.JournalStore
}

func NewServer() *Server {
	return NewServerWithConfig(config.Load())
}

// NewServerWithConfig preserves the historical panic-on-startup contract.
// New startup paths should call NewServerWithConfigE and handle errors.
func NewServerWithConfig(cfg config.Config) *Server {
	srv, err := NewServerWithConfigE(cfg)
	if err != nil {
		panic(err)
	}
	return srv
}

func NewServerWithConfigE(cfg config.Config) (*Server, error) {
	ctx := context.Background()
	metadataDB, err := sqliterepo.Open(ctx, cfg.MetadataDSN)
	if err != nil {
		return nil, err
	}
	coreRepo := sqliterepo.NewCoreResourcesRepo(metadataDB)
	storageRepo := sqliterepo.NewStoragePoolRepo(metadataDB)
	targetRepo := sqliterepo.NewTargetRuntimeRepo(metadataDB)
	accessRepo := memory.NewTargetAccessRepo()
	accessPolicyRepo := sqliterepo.NewAccessPolicyRepo(metadataDB)
	retentionPolicyRepo := sqliterepo.NewRetentionPolicyRepo(metadataDB)

	registry := metrics.NewMetricsRegistry()
	memW := audit.NewMemoryWriter()
	auditPath := filepath.Join(nonEmptyString(cfg.LogDir, "/var/log/holo"), "audit.jsonl")
	journal, err := audit.NewJournalStore(auditPath)
	if err != nil {
		tracing.LogError(context.Background(), "audit", "init audit journal failed; falling back to memory writer", err)
		registry.RecordAuditWriteFailure()
	}
	if journal != nil {
		events, readErr := journal.ReadAll(10000)
		if readErr != nil {
			tracing.LogError(context.Background(), "audit", "load audit journal failed", readErr)
		} else {
			for _, evt := range events {
				if writeErr := memW.Write(context.Background(), evt); writeErr != nil {
					tracing.LogError(context.Background(), "audit", "replay audit event failed", writeErr)
				}
			}
			registry.RecordAuditJournalParseFailures(journal.ParseErrors())
			registry.SetAuditEventsTotal(int64(len(events)))
		}
	}
	auditWriter := audit.NewPersistentWriter(memW, journal, registry)
	storageSvc := orchestration.NewStorageManagementService(storageRepo, auditWriter, nil)

	query := audit.NewQueryService(memW)
	evaluator := auth.NewAccessEvaluator()
	targetRuntime := orchestration.NewTargetRuntimeServiceWithConfig(coreRepo, targetRepo, auditWriter, registry, orchestration.TargetRuntimeConfig{
		Mode:            cfg.TargetRuntimeMode,
		PortalHost:      cfg.TargetPortalHost,
		PortalPort:      cfg.TargetPortalPort,
		BackstoreDir:    cfg.TargetBackstoreDir,
		BackstoreSizeMB: cfg.TargetBackstoreSize,
		UseSudo:         cfg.TargetRuntimeUseSudo,
	})
	if strings.TrimSpace(strings.ToLower(cfg.TargetRuntimeMode)) != "in-memory" {
		targetRuntime.SetStorageWriteGuard(storageSvc)
	}
	targetRuntime.SetStoragePoolReader(storageSvc)
	if strings.TrimSpace(strings.ToLower(cfg.TargetRuntimeMode)) != "in-memory" {
		if err := storageSvc.EnsureAttachedPoolsMounted(ctx); err != nil {
			tracing.LogError(context.Background(), "storage", "restore attached pool mounts failed", err)
		}
		if err := targetRuntime.RestoreReadyPublications(ctx); err != nil {
			tracing.LogError(context.Background(), "target-runtime", "restore ready publications failed", err)
		}
	}
	targetAccess := orchestration.NewTargetAccessService(targetRepo, accessRepo, evaluator, auditWriter)
	accessHandler := NewTargetAccessHandler(targetAccess)
	targetDiscovery := orchestration.NewTargetDiscoveryService(targetRepo, accessRepo, evaluator, auditWriter)
	discoveryHandler := NewTargetDiscoveryHandler(targetDiscovery)
	resourcesHandler := NewResourcesHandlerWithAudit(coreRepo, storageSvc, targetRuntime, auditWriter)
	if strings.TrimSpace(strings.ToLower(cfg.TargetRuntimeMode)) != "in-memory" {
		if err := resourcesHandler.ensureUpgradeRuntimePublications(ctx); err != nil {
			tracing.LogError(context.Background(), "target-runtime", "ensure upgrade publications failed", err)
		}
	}
	health := orchestration.NewHealthServiceWithConfig(
		targetRuntime,
		targetAccess,
		targetDiscovery,
		registry,
		cfg.MetadataDSN,
		cfg.TargetRuntimeMode,
	)

	s := &Server{
		mux:        http.NewServeMux(),
		uiDistDir:  strings.TrimSpace(cfg.WebUIDistDir),
		resources:  resourcesHandler,
		storage:    NewStorageHandler(storageSvc, resourcesHandler),
		policy:     NewPolicyHandler(accessPolicyRepo, retentionPolicyRepo),
		ops:        NewOpsHandler(health, query, cfg.TargetPortalPort, registry),
		access:     accessHandler,
		discovery:  discoveryHandler,
		targets:    NewTargetHandler(targetRuntime, accessHandler),
		metricsHD:  NewMetricsHandler(registry, storageutil.ResolvePoolStorageBaseDir()),
		auditHD:    NewAuditHandler(query, auditWriter),
		runtime:    targetRuntime,
		apiKey:     strings.TrimSpace(cfg.APIKey),
		metadataDB: metadataDB,
		limiter:    newRateLimiter(cfg.TrustedProxyCIDRs),
		journal:    journal,
	}
	if s.apiKey == "" {
		tracing.LogInfo(context.Background(), "control-plane", "management API key is not configured; internal no-login mode is enabled")
	}
	if err := auditWriter.Write(context.Background(), audit.Event{
		EventID:    "bootstrap",
		Actor:      "system",
		Action:     "startup",
		ObjectType: "service",
		ObjectID:   "control-plane",
		Result:     "success",
		OccurredAt: time.Now().UTC(),
	}); err != nil {
		tracing.LogError(context.Background(), "audit", "write startup audit event failed", err)
	}
	s.registerRoutes()
	return s, nil
}

func (s *Server) Router() http.Handler {
	return tracing.TraceMiddleware(s.metricsMiddleware(s.securityHeadersMiddleware(s.rateLimitMiddleware(s.authMiddleware(s.mux)))))
}

func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.metricsHD == nil || s.metricsHD.registry == nil {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		next.ServeHTTP(w, r)
		s.metricsHD.registry.RecordAPIRequestDuration(time.Since(start))
	})
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.runtime == nil {
		return nil
	}
	return s.runtime.Shutdown(ctx)
}

func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	var closeErr error
	if s.metadataDB != nil {
		closeErr = s.metadataDB.Close()
	}
	if s.journal != nil {
		if err := s.journal.Close(); closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/healthz", s.ops.handleHealth)
	s.mux.HandleFunc("/ui", s.handleUIRoot)
	s.mux.HandleFunc("/ui/", s.handleUIAssets)

	s.mux.HandleFunc("/v1/storage/disks/discovery", s.storage.handleDisksDiscovery)
	s.mux.HandleFunc("/v1/storage/pools", s.storage.handlePools)
	s.mux.HandleFunc("/v1/storage/pools/", s.storage.handlePoolSubresource)
	s.mux.HandleFunc("/v1/libraries", s.resources.handleLibraries)
	s.mux.HandleFunc("/v1/libraries/", s.resources.handleLibraryByID)
	s.mux.HandleFunc("/v1/drives", s.resources.handleDrives)
	s.mux.HandleFunc("/v1/drives/", s.resources.handleDriveByID)
	s.mux.HandleFunc("/v1/cartridges", s.resources.handleCartridges)
	s.mux.HandleFunc("/v1/cartridges/", s.resources.handleCartridgeByID)
	s.mux.HandleFunc("/v1/resources/chain", s.resources.handleCreateChain)
	s.mux.HandleFunc("/v1/access-policies", s.policy.handleCreateAccessPolicy)
	s.mux.HandleFunc("/v1/retention-policies", s.policy.handleCreateRetentionPolicy)
	s.mux.HandleFunc("/v1/targets/publications", s.targets.handlePublications)
	s.mux.HandleFunc("/v1/targets/publications/", s.targets.handlePublicationSubresource)
	s.mux.HandleFunc("/v1/targets/visible", s.access.handleVisible)
	s.mux.HandleFunc("/v1/targets/discovery", s.discovery.handleDiscovery)
	s.mux.HandleFunc("/v1/audit/events", s.ops.handleAuditEvents)
	s.mux.HandleFunc("/v1/system/overview", s.ops.handleSystemOverview)
	s.mux.HandleFunc("/v1/ops/cdb-trace", s.ops.handleCDBTrace)
	s.mux.HandleFunc("/v1/support/bundle", s.ops.handleSupportBundle)

	s.mux.HandleFunc("/metrics", s.metricsHD.ServeHTTP)
	s.mux.HandleFunc("/api/v1/audit", s.auditHD.ServeHTTP)
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health endpoint remains intentionally unauthenticated for liveness probing.
		if r.URL.Path == "/healthz" || isUIRoute(r) {
			next.ServeHTTP(w, r)
			return
		}
		if s.apiKey == "" {
			next.ServeHTTP(w, r)
			return
		}

		token := strings.TrimSpace(r.Header.Get("X-HOLO-API-Key"))
		if token == "" {
			authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
			if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
				token = strings.TrimSpace(authHeader[7:])
			}
		}

		if subtle.ConstantTimeCompare([]byte(token), []byte(s.apiKey)) != 1 {
			respondError(w, http.StatusUnauthorized, "unauthorized", nil)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || isUIRoute(r) {
			next.ServeHTTP(w, r)
			return
		}
		if s.limiter != nil {
			allowed, retryAfter := s.limiter.allow(s.limiter.clientIDFromRequest(r), r.URL.Path, time.Now())
			if !allowed {
				w.Header().Set("Retry-After", retryAfterSeconds(retryAfter))
				respondError(w, http.StatusTooManyRequests, "rate limit exceeded", nil)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'self'")
		if r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func isUIRoute(r *http.Request) bool {
	cleaned, ok := normalizedEscapedPath(r)
	if !ok {
		return false
	}
	return cleaned == "/ui" || strings.HasPrefix(cleaned, "/ui/")
}

func normalizedEscapedPath(r *http.Request) (string, bool) {
	if r == nil || r.URL == nil {
		return "", false
	}
	raw := r.URL.EscapedPath()
	if raw == "" {
		raw = r.URL.Path
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil || strings.ContainsRune(decoded, '\x00') {
		return "", false
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(decoded, "/"))
	return cleaned, true
}

func (s *Server) handleUIRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/", http.StatusTemporaryRedirect)
}

func (s *Server) handleUIAssets(w http.ResponseWriter, r *http.Request) {
	if !isUIRoute(r) {
		respondError(w, http.StatusNotFound, "not found", nil)
		return
	}
	distDir := strings.TrimSpace(s.uiDistDir)
	if distDir == "" {
		respondError(w, http.StatusServiceUnavailable, "ui dist path is not configured", nil)
		return
	}
	distInfo, err := os.Stat(distDir)
	if err != nil || !distInfo.IsDir() {
		if err == nil {
			err = errors.New("ui dist path is not a directory")
		}
		respondError(w, http.StatusServiceUnavailable, "ui dist directory is unavailable", err)
		return
	}

	cleaned, ok := normalizedEscapedPath(r)
	if !ok || (cleaned != "/ui" && !strings.HasPrefix(cleaned, "/ui/")) {
		respondError(w, http.StatusNotFound, "not found", nil)
		return
	}
	relative := strings.TrimPrefix(cleaned, "/ui/")
	if relative == "" {
		http.ServeFile(w, r, filepath.Join(distDir, "index.html"))
		return
	}

	candidate := filepath.Clean(filepath.Join(distDir, relative))
	if !pathWithinBase(filepath.Clean(distDir), candidate) {
		respondError(w, http.StatusNotFound, "not found", nil)
		return
	}
	if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
		http.ServeFile(w, r, candidate)
		return
	}
	http.ServeFile(w, r, filepath.Join(distDir, "index.html"))
}

func pathWithinBase(base, candidate string) bool {
	rel, err := filepath.Rel(base, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func nonEmptyString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}
