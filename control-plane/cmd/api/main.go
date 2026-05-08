package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/api"
	"github.com/Holo-VTL/Holo/control-plane/internal/config"
	"github.com/Holo-VTL/Holo/control-plane/internal/tracing"
)

func main() {
	cfg := config.Load()
	if err := validateStartupConfig(cfg); err != nil {
		log.Fatal(err)
	}
	srv, err := api.NewServerWithConfigE(cfg)
	if err != nil {
		log.Fatalf("initialize server: %v", err)
	}
	defer func() {
		if err := srv.Close(); err != nil {
			tracing.LogError(context.Background(), "control-plane", "close metadata database failed", err)
		}
	}()
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	shutdownCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	tracing.LogInfo(context.Background(), "control-plane", "starting server", "addr", cfg.HTTPAddr, "runtime_mode", cfg.TargetRuntimeMode)
	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			tracing.LogError(context.Background(), "control-plane", "server failed", err)
			log.Fatalf("server failed: %v", err)
		}
	case <-shutdownCtx.Done():
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			tracing.LogError(context.Background(), "control-plane", "http shutdown failed", err)
		}
		if err := srv.Shutdown(ctx); err != nil {
			tracing.LogError(context.Background(), "control-plane", "runtime shutdown failed", err)
		}
		if err := <-errCh; err != nil && err != http.ErrServerClosed {
			tracing.LogError(context.Background(), "control-plane", "server failed during shutdown", err)
		}
	}
}

func validateStartupConfig(cfg config.Config) error {
	mode := strings.TrimSpace(strings.ToLower(cfg.TargetRuntimeMode))
	switch mode {
	case "", "in-memory", "lio-shell", "tcmu":
	default:
		return fmt.Errorf("unsupported target runtime mode %q", cfg.TargetRuntimeMode)
	}
	if cfg.TargetPortalPort <= 0 || cfg.TargetPortalPort > 65535 {
		return fmt.Errorf("invalid target portal port %d", cfg.TargetPortalPort)
	}
	if strings.TrimSpace(cfg.MetadataDSN) == "" {
		return fmt.Errorf("metadata dsn is required")
	}
	return nil
}
