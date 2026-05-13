package orchestration

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo/memory"
)

func seededRuntimeService(t *testing.T) *TargetRuntimeService {
	t.Helper()
	coreRepo := memory.NewCoreResourcesRepo()
	runtimeRepo := memory.NewTargetRuntimeRepo()
	auditWriter := audit.NewMemoryWriter()

	lib, err := domain.NewVirtualLibrary("lib-1", "lib-1")
	if err != nil {
		t.Fatalf("new library failed: %v", err)
	}
	drive, err := domain.NewVirtualDrive("drive-1", "lib-1", 1)
	if err != nil {
		t.Fatalf("new drive failed: %v", err)
	}
	car := domain.NewVirtualCartridge("car-1", "pool-1", "lib-1", "B001", 1<<30)

	ctx := context.Background()
	_ = coreRepo.SaveLibrary(ctx, lib)
	_ = coreRepo.SaveDrive(ctx, drive)
	_ = coreRepo.SaveCartridge(ctx, car)

	return NewTargetRuntimeService(coreRepo, runtimeRepo, auditWriter, nil)
}

func TestPublishAndConflictLifecycle(t *testing.T) {
	svc := seededRuntimeService(t)
	ctx := context.Background()

	pub, err := svc.Publish(ctx, PublishRequest{
		LibraryID:   "lib-1",
		DriveID:     "drive-1",
		CartridgeID: "car-1",
		TargetIQN:   "iqn.2026-04.ai.holo:test-drive-1",
		Actor:       "ops",
	})
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	if pub.State != domain.PublicationReady {
		t.Fatalf("expected ready, got %s", pub.State)
	}

	_, err = svc.Publish(ctx, PublishRequest{
		LibraryID:   "lib-1",
		DriveID:     "drive-1",
		CartridgeID: "car-1",
		TargetIQN:   "iqn.2026-04.ai.holo:test-drive-1",
		Actor:       "ops",
	})
	if err != domain.ErrConflict {
		t.Fatalf("expected conflict, got %v", err)
	}

	pub, err = svc.Unpublish(ctx, pub.PublicationID, "ops")
	if err != nil {
		t.Fatalf("unpublish failed: %v", err)
	}
	if pub.State != domain.PublicationDisabled {
		t.Fatalf("expected disabled, got %s", pub.State)
	}

	pub, err = svc.Unpublish(ctx, pub.PublicationID, "ops")
	if err != nil {
		t.Fatalf("idempotent unpublish failed: %v", err)
	}
	if pub.State != domain.PublicationDisabled {
		t.Fatalf("expected disabled on noop unpublish, got %s", pub.State)
	}
}

func TestPublishPersistsDriveProfile(t *testing.T) {
	svc := seededRuntimeService(t)
	ctx := context.Background()

	pub, err := svc.Publish(ctx, PublishRequest{
		LibraryID:     "lib-1",
		DriveID:       "drive-1",
		CartridgeID:   "car-1",
		TargetIQN:     "iqn.2026-04.ai.holo:test-drive-profile",
		DeviceRole:    "changer",
		DeviceProfile: "adic-scalar-i500",
		DriveProfile:  "quantum-ultrium-td7",
		Actor:         "ops",
	})
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	if pub.DeviceProfile != "adic-scalar-i500" {
		t.Fatalf("expected changer profile, got %q", pub.DeviceProfile)
	}
	if pub.DriveProfile != "quantum-ultrium-td7" {
		t.Fatalf("expected drive profile, got %q", pub.DriveProfile)
	}
}

func TestPublishCarriesLibraryTapePolicy(t *testing.T) {
	coreRepo := memory.NewCoreResourcesRepo()
	runtimeRepo := memory.NewTargetRuntimeRepo()
	adapter := &stubRuntimeAdapter{}
	svc := newTargetRuntimeServiceWithAdapter(coreRepo, runtimeRepo, audit.NewMemoryWriter(), nil, TargetRuntimeConfig{Mode: "tcmu"}, adapter)

	lib, err := domain.NewVirtualLibrary("lib-policy", "lib-policy")
	if err != nil {
		t.Fatalf("new library failed: %v", err)
	}
	lib.CompressionEnabled = false
	lib.DedupEnabled = false
	drive, err := domain.NewVirtualDrive("drive-policy", "lib-policy", 1)
	if err != nil {
		t.Fatalf("new drive failed: %v", err)
	}
	car := domain.NewVirtualCartridge("car-policy", "pool-policy", "lib-policy", "P001", 1<<30)

	ctx := context.Background()
	_ = coreRepo.SaveLibrary(ctx, lib)
	_ = coreRepo.SaveDrive(ctx, drive)
	_ = coreRepo.SaveCartridge(ctx, car)

	pub, err := svc.Publish(ctx, PublishRequest{
		LibraryID:   "lib-policy",
		DriveID:     "drive-policy",
		CartridgeID: "car-policy",
		TargetIQN:   "iqn.2026-04.ai.holo:policy",
		Actor:       "ops",
	})
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	if pub.CompressionEnabled || pub.DedupEnabled {
		t.Fatalf("expected publication policy disabled, got %+v", pub)
	}
	if adapter.lastPublication == nil || adapter.lastPublication.CompressionEnabled || adapter.lastPublication.DedupEnabled {
		t.Fatalf("expected adapter to receive disabled policy, got %+v", adapter.lastPublication)
	}
}

func TestRestoreReadyPublicationsRepublishesRuntimeTargets(t *testing.T) {
	coreRepo := memory.NewCoreResourcesRepo()
	runtimeRepo := memory.NewTargetRuntimeRepo()
	adapter := &recordingRuntimeAdapter{portal: "10.0.0.10:3260"}
	svc := newTargetRuntimeServiceWithAdapter(coreRepo, runtimeRepo, audit.NewMemoryWriter(), nil, TargetRuntimeConfig{Mode: "tcmu"}, adapter)

	pub, err := domain.NewTargetPublication("pub-restore", "pool-1", "lib-1", "drive-1", "cart-1", "iqn.2026-04.ai.holo:restore")
	if err != nil {
		t.Fatalf("new publication: %v", err)
	}
	if err := pub.MarkReady("10.0.0.9:3260"); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	if err := runtimeRepo.SavePublication(context.Background(), pub); err != nil {
		t.Fatalf("save publication: %v", err)
	}

	if err := svc.RestoreReadyPublications(context.Background()); err != nil {
		t.Fatalf("restore ready publications: %v", err)
	}
	if adapter.unpublishCalls != 1 || adapter.publishCalls != 1 {
		t.Fatalf("expected one unpublish and publish, got unpublish=%d publish=%d", adapter.unpublishCalls, adapter.publishCalls)
	}
	reloaded, err := runtimeRepo.FindPublication(context.Background(), "pub-restore")
	if err != nil {
		t.Fatalf("find restored publication: %v", err)
	}
	if reloaded.State != domain.PublicationReady || reloaded.Portal != "10.0.0.10:3260" || reloaded.LastError != "" {
		t.Fatalf("unexpected restored publication: %+v", reloaded)
	}
}

func TestShutdownUnpublishesReadyRuntimeTargetsWithoutDisablingMetadata(t *testing.T) {
	coreRepo := memory.NewCoreResourcesRepo()
	runtimeRepo := memory.NewTargetRuntimeRepo()
	adapter := &recordingRuntimeAdapter{portal: "10.0.0.10:3260"}
	svc := newTargetRuntimeServiceWithAdapter(coreRepo, runtimeRepo, audit.NewMemoryWriter(), nil, TargetRuntimeConfig{Mode: "tcmu"}, adapter)

	ready, err := domain.NewTargetPublication("pub-ready", "pool-1", "lib-1", "drive-1", "cart-1", "iqn.2026-04.ai.holo:ready")
	if err != nil {
		t.Fatalf("new ready publication: %v", err)
	}
	if err := ready.MarkReady("10.0.0.9:3260"); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	disabled, err := domain.NewTargetPublication("pub-disabled", "pool-1", "lib-1", "drive-2", "cart-2", "iqn.2026-04.ai.holo:disabled")
	if err != nil {
		t.Fatalf("new disabled publication: %v", err)
	}
	if err := disabled.MarkFailed("disabled test fixture"); err != nil {
		t.Fatalf("mark failed publication: %v", err)
	}
	if err := disabled.Disable(); err != nil {
		t.Fatalf("disable publication: %v", err)
	}
	ctx := context.Background()
	if err := runtimeRepo.SavePublication(ctx, ready); err != nil {
		t.Fatalf("save ready publication: %v", err)
	}
	if err := runtimeRepo.SavePublication(ctx, disabled); err != nil {
		t.Fatalf("save disabled publication: %v", err)
	}

	if err := svc.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown runtime: %v", err)
	}
	if adapter.unpublishCalls != 1 {
		t.Fatalf("expected one ready publication to be unpublished, got %d", adapter.unpublishCalls)
	}
	reloaded, err := runtimeRepo.FindPublication(ctx, "pub-ready")
	if err != nil {
		t.Fatalf("find ready publication: %v", err)
	}
	if reloaded.State != domain.PublicationReady {
		t.Fatalf("expected shutdown to leave publication metadata ready, got %s", reloaded.State)
	}
}

type recordingRuntimeAdapter struct {
	portal         string
	publishCalls   int
	unpublishCalls int
}

func (a *recordingRuntimeAdapter) Publish(_ context.Context, _ *domain.TargetPublication) (string, error) {
	a.publishCalls++
	return a.portal, nil
}

func (a *recordingRuntimeAdapter) Unpublish(_ context.Context, _ *domain.TargetPublication) error {
	a.unpublishCalls++
	return nil
}

func TestListDiscoverablePublicationsOnlyReady(t *testing.T) {
	svc := seededRuntimeService(t)
	ctx := context.Background()

	pubA, err := svc.Publish(ctx, PublishRequest{
		LibraryID:   "lib-1",
		DriveID:     "drive-1",
		CartridgeID: "car-1",
		TargetIQN:   "iqn.2026-04.ai.holo:discoverable-a",
		Actor:       "ops",
	})
	if err != nil {
		t.Fatalf("publish A failed: %v", err)
	}
	pubB, err := svc.Publish(ctx, PublishRequest{
		LibraryID:   "lib-1",
		DriveID:     "drive-1",
		CartridgeID: "car-1",
		TargetIQN:   "iqn.2026-04.ai.holo:discoverable-b",
		Actor:       "ops",
	})
	if err != nil {
		t.Fatalf("publish B failed: %v", err)
	}

	_, err = svc.Unpublish(ctx, pubB.PublicationID, "ops")
	if err != nil {
		t.Fatalf("unpublish B failed: %v", err)
	}

	discoverable := svc.runtimeRepo.ListDiscoverablePublications(ctx)
	if len(discoverable) != 1 || discoverable[0].PublicationID != pubA.PublicationID {
		t.Fatalf("expected only pubA discoverable, got %+v", discoverable)
	}
}

type stubRuntimeAdapter struct {
	publishPortal   string
	publishErr      error
	unpublishErr    error
	lastPublication *domain.TargetPublication
}

func (a *stubRuntimeAdapter) Publish(_ context.Context, p *domain.TargetPublication) (string, error) {
	if p != nil {
		cp := *p
		a.lastPublication = &cp
	}
	if a.publishErr != nil {
		return "", a.publishErr
	}
	if a.publishPortal == "" {
		return "127.0.0.1:3260", nil
	}
	return a.publishPortal, nil
}

func (a *stubRuntimeAdapter) Unpublish(_ context.Context, _ *domain.TargetPublication) error {
	return a.unpublishErr
}

func TestUnpublishFailureKeepsPublicationState(t *testing.T) {
	svc := seededRuntimeService(t)
	svc.adapter = &stubRuntimeAdapter{
		publishPortal: "127.0.0.1:3260",
		unpublishErr:  errors.New("runtime unpublish failed"),
	}

	pub, err := svc.Publish(context.Background(), PublishRequest{
		LibraryID:   "lib-1",
		DriveID:     "drive-1",
		CartridgeID: "car-1",
		TargetIQN:   "iqn.2026-04.ai.holo:unpublish-fail",
		Actor:       "ops",
	})
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}

	_, err = svc.Unpublish(context.Background(), pub.PublicationID, "ops")
	if err == nil {
		t.Fatal("expected unpublish to fail")
	}

	saved, err := svc.GetPublication(context.Background(), pub.PublicationID)
	if err != nil {
		t.Fatalf("get publication failed: %v", err)
	}
	if saved.State != domain.PublicationReady {
		t.Fatalf("expected publication to remain ready after failed unpublish, got %s", saved.State)
	}
}

func TestPublishAllowsRetryAfterFailedPublication(t *testing.T) {
	svc := seededRuntimeService(t)
	adapter := &stubRuntimeAdapter{
		publishErr: errors.New("simulated publish failure"),
	}
	svc.adapter = adapter

	_, err := svc.Publish(context.Background(), PublishRequest{
		LibraryID:   "lib-1",
		DriveID:     "drive-1",
		CartridgeID: "car-1",
		TargetIQN:   "iqn.2026-04.ai.holo:retry-after-failed",
		Actor:       "ops",
	})
	if err == nil {
		t.Fatal("expected initial publish to fail")
	}

	adapter.publishErr = nil
	adapter.publishPortal = "127.0.0.1:3260"
	pub, err := svc.Publish(context.Background(), PublishRequest{
		LibraryID:   "lib-1",
		DriveID:     "drive-1",
		CartridgeID: "car-1",
		TargetIQN:   "iqn.2026-04.ai.holo:retry-after-failed",
		Actor:       "ops",
	})
	if err != nil {
		t.Fatalf("retry publish should succeed, got %v", err)
	}
	if pub.State != domain.PublicationReady {
		t.Fatalf("expected ready publication on retry, got %s", pub.State)
	}
}

func TestPublishRejectsUnsafeTargetIQN(t *testing.T) {
	svc := seededRuntimeService(t)

	_, err := svc.Publish(context.Background(), PublishRequest{
		LibraryID:   "lib-1",
		DriveID:     "drive-1",
		CartridgeID: "car-1",
		TargetIQN:   "iqn.2026-04.ai.holo:../../etc/passwd",
		Actor:       "ops",
	})
	if err != domain.ErrInvalidInput {
		t.Fatalf("expected invalid input for unsafe IQN, got %v", err)
	}
}

func TestPublishConcurrentDuplicateIQNIsAtomic(t *testing.T) {
	svc := seededRuntimeService(t)
	svc.adapter = &stubRuntimeAdapter{publishPortal: "127.0.0.1:3260"}

	const attempts = 2
	var wg sync.WaitGroup
	results := make(chan error, attempts)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Publish(context.Background(), PublishRequest{
				LibraryID:   "lib-1",
				DriveID:     "drive-1",
				CartridgeID: "car-1",
				TargetIQN:   "iqn.2026-04.ai.holo:atomic-race",
				Actor:       "ops",
			})
			results <- err
		}()
	}

	wg.Wait()
	close(results)

	success := 0
	conflicts := 0
	for err := range results {
		switch err {
		case nil:
			success++
		case domain.ErrConflict:
			conflicts++
		default:
			t.Fatalf("unexpected publish result: %v", err)
		}
	}

	if success != 1 || conflicts != 1 {
		t.Fatalf("expected 1 success and 1 conflict, got success=%d conflicts=%d", success, conflicts)
	}
}

func TestRuntimeModeSelection(t *testing.T) {
	coreRepo := memory.NewCoreResourcesRepo()
	runtimeRepo := memory.NewTargetRuntimeRepo()
	auditWriter := audit.NewMemoryWriter()

	inMemorySvc := NewTargetRuntimeServiceWithConfig(coreRepo, runtimeRepo, auditWriter, nil, TargetRuntimeConfig{Mode: "in-memory"})
	if _, ok := inMemorySvc.adapter.(*inMemoryTargetRuntimeAdapter); !ok {
		t.Fatalf("expected in-memory adapter, got %T", inMemorySvc.adapter)
	}

	lioSvc := NewTargetRuntimeServiceWithConfig(coreRepo, runtimeRepo, auditWriter, nil, TargetRuntimeConfig{
		Mode:            "lio-shell",
		PortalHost:      "10.10.1.184",
		PortalPort:      3260,
		BackstoreDir:    t.TempDir(),
		BackstoreSizeMB: 8,
		UseSudo:         false,
	})
	if _, ok := lioSvc.adapter.(*lioShellTargetRuntimeAdapter); !ok {
		t.Fatalf("expected lio-shell adapter, got %T", lioSvc.adapter)
	}

	fallbackSvc := NewTargetRuntimeServiceWithConfig(coreRepo, runtimeRepo, auditWriter, nil, TargetRuntimeConfig{Mode: "unsupported"})
	if _, ok := fallbackSvc.adapter.(*inMemoryTargetRuntimeAdapter); !ok {
		t.Fatalf("expected unsupported mode to fallback to in-memory, got %T", fallbackSvc.adapter)
	}
}

type fakeCommandRunner struct {
	calls   [][]string
	failOn  int
	failErr error
}

func (r *fakeCommandRunner) Run(_ context.Context, command string, args ...string) (string, error) {
	call := append([]string{command}, args...)
	r.calls = append(r.calls, call)
	if r.failOn > 0 && len(r.calls) == r.failOn {
		if r.failErr != nil {
			return "", r.failErr
		}
		return "", errors.New("forced command failure")
	}
	return "ok", nil
}

func TestLIOShellAdapterRunTargetcliUsesPrivilegedHelper(t *testing.T) {
	t.Setenv("HOLO_TARGETCLI_PRIVILEGED_HELPER", "/opt/holo/bin/holo-targetcli-helper")
	runner := &fakeCommandRunner{}
	adapter := newLIOShellTargetRuntimeAdapter(TargetRuntimeConfig{
		Mode:    "lio-shell",
		UseSudo: true,
	}, runner)

	if err := adapter.runTargetcli(context.Background(), "/iscsi", "create", "iqn.2026-04.ai.holo:helper-test"); err != nil {
		t.Fatalf("runTargetcli failed: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected one call, got %v", runner.calls)
	}
	got := strings.Join(runner.calls[0], " ")
	want := "sudo -n /opt/holo/bin/holo-targetcli-helper /iscsi create iqn.2026-04.ai.holo:helper-test"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestTargetcliCommandAcceptsCustomInstallPrefixHelper(t *testing.T) {
	t.Setenv("HOLO_TARGETCLI_PRIVILEGED_HELPER", "/usr/local/holo/bin/holo-targetcli-helper")
	cmd, args := targetcliCommand(true, "/iscsi", "ls")
	got := append([]string{cmd}, args...)
	want := []string{"sudo", "-n", "/usr/local/holo/bin/holo-targetcli-helper", "/iscsi", "ls"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("expected %q, got %q", strings.Join(want, " "), strings.Join(got, " "))
	}
}

func TestTargetcliCommandPreservesFallbackWhenHelperUnset(t *testing.T) {
	t.Setenv("HOLO_TARGETCLI_PRIVILEGED_HELPER", "")
	cmd, args := targetcliCommand(true, "/iscsi", "ls")
	got := append([]string{cmd}, args...)
	want := []string{"sudo", "-n", "targetcli", "/iscsi", "ls"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("expected %q, got %q", strings.Join(want, " "), strings.Join(got, " "))
	}
}

func TestTargetcliCommandIgnoresUnsafeHelperValues(t *testing.T) {
	for _, helper := range []string{
		"relative/holo-targetcli-helper",
		"/tmp/holo-targetcli-helper",
		"/tmp/holo/bin/holo-targetcli-helper",
		"/var/tmp/holo-targetcli-helper",
		"/opt/holo/bin/not-holo-targetcli-helper",
	} {
		t.Run(helper, func(t *testing.T) {
			t.Setenv("HOLO_TARGETCLI_PRIVILEGED_HELPER", helper)
			cmd, args := targetcliCommand(true, "/iscsi", "ls")
			got := append([]string{cmd}, args...)
			want := []string{"sudo", "-n", "targetcli", "/iscsi", "ls"}
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Fatalf("expected unsafe helper %q to fall back to %q, got %q", helper, strings.Join(want, " "), strings.Join(got, " "))
			}
		})
	}
}

func TestTargetcliCommandWarnsOnceForUnsafeHelper(t *testing.T) {
	helper := "/opt/holo/bin/invalid-helper-warn-once"
	t.Setenv("HOLO_TARGETCLI_PRIVILEGED_HELPER", helper)
	var logs bytes.Buffer
	original := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(original) })

	for i := 0; i < 2; i++ {
		cmd, args := targetcliCommand(true, "/iscsi", "create", "iqn.2026-04.ai.holo:warn-once")
		got := append([]string{cmd}, args...)
		want := []string{"sudo", "-n", "targetcli", "/iscsi", "create", "iqn.2026-04.ai.holo:warn-once"}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("expected %q, got %q", strings.Join(want, " "), strings.Join(got, " "))
		}
	}
	if got := strings.Count(logs.String(), "ignoring unsafe HOLO_TARGETCLI_PRIVILEGED_HELPER"); got != 1 {
		t.Fatalf("expected one unsafe helper warning, got %d logs=%q", got, logs.String())
	}
}

func TestLIOShellAdapterPublishUnpublishCommands(t *testing.T) {
	backstoreDir := t.TempDir()
	poolRootBase := t.TempDir()
	t.Setenv("HOLO_STORAGE_POOL_ROOT_BASE", poolRootBase)
	runner := &fakeCommandRunner{}
	adapter := newLIOShellTargetRuntimeAdapter(TargetRuntimeConfig{
		Mode:            "lio-shell",
		PortalHost:      "10.10.1.184",
		PortalPort:      3260,
		BackstoreDir:    backstoreDir,
		BackstoreSizeMB: 8,
		UseSudo:         false,
	}, runner)

	pub, err := domain.NewTargetPublication("pub-1", "pool-1", "lib-1", "drive-1", "car-1", "iqn.2026-04.ai.holo:test-lio")
	if err != nil {
		t.Fatalf("new publication failed: %v", err)
	}

	portal, err := adapter.Publish(context.Background(), pub)
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	if portal != "10.10.1.184:3260" {
		t.Fatalf("unexpected portal: %s", portal)
	}
	if len(runner.calls) < 3 {
		t.Fatalf("expected at least 3 targetcli calls, got %d", len(runner.calls))
	}

	backstoreName := runtimeBackstoreName(pub)
	resolvedBackstoreDir, err := lioBackstoreDir(adapter.cfg, pub)
	if err != nil {
		t.Fatalf("backstore dir: %v", err)
	}
	backstorePath, err := runtimeBackstorePath(resolvedBackstoreDir, backstoreName)
	if err != nil {
		t.Fatalf("backstore path: %v", err)
	}
	if _, err := os.Stat(backstorePath); err != nil {
		t.Fatalf("expected backstore image created, err=%v", err)
	}
	if !strings.HasPrefix(backstorePath, poolRootBase) {
		t.Fatalf("expected backstore path under pool root %q, got %q", poolRootBase, backstorePath)
	}

	if err := adapter.Unpublish(context.Background(), pub); err != nil {
		t.Fatalf("unpublish failed: %v", err)
	}
	if _, err := os.Stat(backstorePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected backstore image removed, err=%v", err)
	}

	joined := flattenCalls(runner.calls)
	if !strings.Contains(joined, "/iscsi create iqn.2026-04.ai.holo:test-lio") {
		t.Fatalf("expected target create command, got calls=%s", joined)
	}
	if !strings.Contains(joined, "/iscsi delete iqn.2026-04.ai.holo:test-lio") {
		t.Fatalf("expected target delete command, got calls=%s", joined)
	}
}

func TestLIOShellAdapterPublishFailureCleansBackstore(t *testing.T) {
	backstoreDir := t.TempDir()
	t.Setenv("HOLO_STORAGE_POOL_ROOT_BASE", t.TempDir())
	runner := &fakeCommandRunner{failOn: 2}
	adapter := newLIOShellTargetRuntimeAdapter(TargetRuntimeConfig{
		Mode:            "lio-shell",
		PortalHost:      "10.10.1.184",
		PortalPort:      3260,
		BackstoreDir:    backstoreDir,
		BackstoreSizeMB: 8,
		UseSudo:         false,
	}, runner)

	pub, err := domain.NewTargetPublication("pub-2", "pool-1", "lib-1", "drive-1", "car-1", "iqn.2026-04.ai.holo:test-lio-fail")
	if err != nil {
		t.Fatalf("new publication failed: %v", err)
	}

	if _, err := adapter.Publish(context.Background(), pub); err == nil {
		t.Fatal("expected publish to fail")
	}

	joined := flattenCalls(runner.calls)
	if !strings.Contains(joined, "/backstores/fileio delete") {
		t.Fatalf("expected backstore cleanup after publish failure, got calls=%s", joined)
	}
}

func TestLIOShellAdapterRejectsUnsafeRuntimeInputs(t *testing.T) {
	runner := &fakeCommandRunner{}
	adapter := newLIOShellTargetRuntimeAdapter(TargetRuntimeConfig{
		Mode:            "lio-shell",
		BackstoreDir:    t.TempDir(),
		BackstoreSizeMB: 8,
		UseSudo:         false,
	}, runner)

	pub, err := domain.NewTargetPublication("pub-unsafe", "pool-1", "lib-1", "drive-1", "car-1", "iqn.2026-04.ai.holo:safe")
	if err != nil {
		t.Fatalf("new publication: %v", err)
	}
	pub.TargetIQN = "iqn.2026-04.ai.holo:bad\nvalue"

	if _, err := adapter.Publish(context.Background(), pub); err != domain.ErrInvalidInput {
		t.Fatalf("expected invalid input for malformed target IQN, got %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("expected no targetcli calls for invalid publication, got %v", runner.calls)
	}

	if err := adapter.runTargetcli(context.Background(), "/iscsi", "create", "iqn.2026-04.ai.holo:bad\nvalue"); err != domain.ErrInvalidInput {
		t.Fatalf("expected invalid input for unsafe targetcli arg, got %v", err)
	}
	if err := adapter.runTargetcli(context.Background(), "/iscsi/../backstores", "ls"); err != domain.ErrInvalidInput {
		t.Fatalf("expected invalid input for targetcli path traversal, got %v", err)
	}
	if err := adapter.runTargetcli(context.Background(), "/iscsi", "ls"); err != domain.ErrInvalidInput {
		t.Fatalf("expected invalid input for unsupported targetcli command shape, got %v", err)
	}
	if err := adapter.runTargetcli(context.Background(), "/iscsi/iqn.2026-04.ai.holo:safe/tpg1", "set", "attribute", "authentication=1"); err != domain.ErrInvalidInput {
		t.Fatalf("expected invalid input for unsupported targetcli attribute, got %v", err)
	}
}

func TestLIOShellBackstorePathValidationPreservesValidPublication(t *testing.T) {
	poolRootBase := t.TempDir()
	t.Setenv("HOLO_STORAGE_POOL_ROOT_BASE", poolRootBase)
	pub, err := domain.NewTargetPublication("pub-valid", "Pool-A", "lib-1", "drive-1", "car-1", "iqn.2026-04.ai.holo:valid")
	if err != nil {
		t.Fatalf("new publication: %v", err)
	}

	backstoreDir, err := lioBackstoreDir(TargetRuntimeConfig{BackstoreDir: t.TempDir()}, pub)
	if err != nil {
		t.Fatalf("valid pool backstore dir rejected: %v", err)
	}
	if !strings.HasPrefix(backstoreDir, poolRootBase) {
		t.Fatalf("expected pool backstore dir under %q, got %q", poolRootBase, backstoreDir)
	}
	backstorePath, err := runtimeBackstorePath(backstoreDir, runtimeBackstoreName(pub))
	if err != nil {
		t.Fatalf("valid backstore path rejected: %v", err)
	}
	if !strings.HasSuffix(backstorePath, ".img") {
		t.Fatalf("expected image path suffix, got %q", backstorePath)
	}
}

func flattenCalls(calls [][]string) string {
	lines := make([]string, 0, len(calls))
	for _, call := range calls {
		lines = append(lines, strings.Join(call, " "))
	}
	return strings.Join(lines, "\n")
}

type fakeStorageWriteGuard struct {
	reserveErr     error
	reserveCalled  int
	rollbackCalled int
	reservedBytes  int64
	reservedPoolID string
}

func (g *fakeStorageWriteGuard) ReserveWrite(_ context.Context, poolID string, bytes int64) (*domain.StoragePoolCapacitySnapshot, bool, error) {
	g.reserveCalled++
	g.reservedPoolID = poolID
	g.reservedBytes = bytes
	if g.reserveErr != nil {
		return nil, false, g.reserveErr
	}
	return &domain.StoragePoolCapacitySnapshot{TotalBytes: 1024, UsedBytes: bytes, FreeBytes: 1024 - bytes, UsedPercent: int((bytes * 100) / 1024), WarningThresholdPct: 90}, false, nil
}

func (g *fakeStorageWriteGuard) RollbackReservedWrite(_ context.Context, _ string, _ int64) error {
	g.rollbackCalled++
	return nil
}

func TestValidationRun_ReservesStorageCapacity(t *testing.T) {
	svc := seededRuntimeService(t)
	guard := &fakeStorageWriteGuard{}
	svc.SetStorageWriteGuard(guard)

	pub, err := svc.Publish(context.Background(), PublishRequest{
		LibraryID:   "lib-1",
		DriveID:     "drive-1",
		CartridgeID: "car-1",
		TargetIQN:   "iqn.2026-04.ai.holo:validation-capacity",
		Actor:       "ops",
	})
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}

	run, err := svc.StartValidationRunWithRequest(context.Background(), pub.PublicationID, "ops", ValidationRunRequest{
		Mode:  domain.ValidationModeFixed,
		Bytes: 128,
	})
	if err != nil {
		t.Fatalf("validation run failed: %v", err)
	}
	if run.Status != domain.ValidationPassed {
		t.Fatalf("expected validation passed, got %s", run.Status)
	}
	if guard.reserveCalled != 1 {
		t.Fatalf("expected one reserve call, got %d", guard.reserveCalled)
	}
	if guard.rollbackCalled != 0 {
		t.Fatalf("did not expect rollback on successful validation, got %d", guard.rollbackCalled)
	}
}

func TestValidationRun_CapacityExceededReturnsError(t *testing.T) {
	svc := seededRuntimeService(t)
	guard := &fakeStorageWriteGuard{reserveErr: domain.ErrCapacityExceeded}
	svc.SetStorageWriteGuard(guard)

	pub, err := svc.Publish(context.Background(), PublishRequest{
		LibraryID:   "lib-1",
		DriveID:     "drive-1",
		CartridgeID: "car-1",
		TargetIQN:   "iqn.2026-04.ai.holo:validation-capacity-fail",
		Actor:       "ops",
	})
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}

	_, err = svc.StartValidationRunWithRequest(context.Background(), pub.PublicationID, "ops", ValidationRunRequest{
		Mode:  domain.ValidationModeFixed,
		Bytes: 128,
	})
	if err != domain.ErrCapacityExceeded {
		t.Fatalf("expected capacity exceeded, got %v", err)
	}
	if guard.reserveCalled != 1 {
		t.Fatalf("expected one reserve call, got %d", guard.reserveCalled)
	}
}
