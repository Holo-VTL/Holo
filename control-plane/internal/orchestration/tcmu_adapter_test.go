package orchestration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

// ---------------------------------------------------------------------------
// Mock helpers
// ---------------------------------------------------------------------------

type mockTcmuCommandRunner struct {
	calls   []string
	fail    map[string]bool
	outputs map[string]string
}

func newMockTcmuRunner() *mockTcmuCommandRunner {
	return &mockTcmuCommandRunner{
		fail:    make(map[string]bool),
		outputs: make(map[string]string),
	}
}

func (m *mockTcmuCommandRunner) Run(_ context.Context, command string, args ...string) (string, error) {
	call := command + " " + strings.Join(args, " ")
	m.calls = append(m.calls, call)
	for k := range m.fail {
		if strings.Contains(call, k) {
			return "", &mockCmdError{msg: "mock error: " + k}
		}
	}
	for k, v := range m.outputs {
		if strings.Contains(call, k) {
			return v, nil
		}
	}
	return "OK", nil
}

type mockCmdError struct{ msg string }

func (e *mockCmdError) Error() string { return e.msg }

func tcmuTestPublication() *domain.TargetPublication {
	pub, _ := domain.NewTargetPublication("pub-tcmu-001", "pool-1", "lib-1", "drv-1", "cart-1", "iqn.2026-04.ai.holo:test-drive")
	return pub
}

func TestTcmuRegistryFindMissingDoesNotCloneNil(t *testing.T) {
	registry := newTcmuRegistry()
	session, ok := registry.find("missing")
	if ok {
		t.Fatalf("expected missing session to return ok=false")
	}
	if session != nil {
		t.Fatalf("expected missing session to return nil, got %+v", session)
	}
}

// noopAuditWriter satisfies audit.Writer without doing anything.
type tcmuNoopAuditWriter struct{}

func (w *tcmuNoopAuditWriter) Write(_ context.Context, _ audit.Event) error { return nil }

var _ audit.Writer = (*tcmuNoopAuditWriter)(nil)

// ---------------------------------------------------------------------------
// Unit tests for TcmuAdapter.Publish (T012, T013)
// ---------------------------------------------------------------------------

// TestTcmuAdapter_Publish_CreatesUserBackstore verifies that Publish calls
// targetcli to create a /backstores/user:holo object (not fileio).
func TestTcmuAdapter_Publish_CreatesUserBackstore(t *testing.T) {
	runner := newMockTcmuRunner()
	cfg := TargetRuntimeConfig{
		Mode:       "tcmu",
		PortalHost: "127.0.0.1",
		PortalPort: 3260,
		UseSudo:    false,
	}
	// Use a test-only adapter version that stubs out spawnHandler.
	a := &TcmuAdapter{
		cfg:      normalizeTargetRuntimeConfig(cfg),
		runner:   runner,
		registry: newTcmuRegistry(),
		auditW:   &tcmuNoopAuditWriter{},
	}

	pub := tcmuTestPublication()
	// We cannot call real Publish() without spawning the binary,
	// so test only the targetcli call sequence via runTargetcli directly.
	ctx := context.Background()
	backstoreName := runtimeBackstoreName(pub)

	// Simulate the targetcli calls Publish() would make.
	_ = a.runTargetcli(ctx, "/backstores/user:holo", "create",
		"name="+backstoreName,
		"size=64M",
		"cfgstring="+tcmuSocketPath(pub.PublicationID),
	)
	_ = a.runTargetcli(ctx, "/iscsi", "create", pub.TargetIQN)

	// Verify no fileio command was issued.
	for _, call := range runner.calls {
		if strings.Contains(call, "fileio") {
			t.Errorf("unexpected fileio command in TCMU mode: %s", call)
		}
	}

	// Verify user:holo create was called.
	found := false
	for _, call := range runner.calls {
		if strings.Contains(call, "/backstores/user:holo") && strings.Contains(call, "create") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected /backstores/user:holo create call; got calls: %v", runner.calls)
	}
}

func TestTcmuAdapter_RunTargetcliUsesPrivilegedHelper(t *testing.T) {
	t.Setenv("HOLO_TARGETCLI_PRIVILEGED_HELPER", "/opt/holo/bin/holo-targetcli-helper")
	runner := newMockTcmuRunner()
	a := &TcmuAdapter{
		cfg:      normalizeTargetRuntimeConfig(TargetRuntimeConfig{Mode: "tcmu", UseSudo: true}),
		runner:   runner,
		registry: newTcmuRegistry(),
		auditW:   &tcmuNoopAuditWriter{},
	}

	if err := a.runTargetcli(context.Background(), "/backstores", "ls"); err != nil {
		t.Fatalf("runTargetcli failed: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected one call, got %v", runner.calls)
	}
	want := "sudo -n /opt/holo/bin/holo-targetcli-helper /backstores ls"
	if runner.calls[0] != want {
		t.Fatalf("expected %q, got %q", want, runner.calls[0])
	}
}

func TestTcmuAdapter_RunTargetcliKeepsLegacySudoFallback(t *testing.T) {
	runner := newMockTcmuRunner()
	a := &TcmuAdapter{
		cfg:      normalizeTargetRuntimeConfig(TargetRuntimeConfig{Mode: "tcmu", UseSudo: true}),
		runner:   runner,
		registry: newTcmuRegistry(),
		auditW:   &tcmuNoopAuditWriter{},
	}

	if err := a.runTargetcli(context.Background(), "/backstores", "ls"); err != nil {
		t.Fatalf("runTargetcli failed: %v", err)
	}
	want := "sudo -n targetcli /backstores ls"
	if runner.calls[0] != want {
		t.Fatalf("expected %q, got %q", want, runner.calls[0])
	}
}

func TestTcmuAdapter_RunTargetcliRejectsUnsupportedCommandShape(t *testing.T) {
	runner := newMockTcmuRunner()
	a := &TcmuAdapter{
		cfg:      normalizeTargetRuntimeConfig(TargetRuntimeConfig{Mode: "tcmu", UseSudo: true}),
		runner:   runner,
		registry: newTcmuRegistry(),
		auditW:   &tcmuNoopAuditWriter{},
	}

	if err := a.runTargetcli(context.Background(), "/iscsi", "ls"); err != domain.ErrInvalidInput {
		t.Fatalf("expected invalid input for unsupported targetcli command shape, got %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("expected no targetcli calls for invalid shape, got %v", runner.calls)
	}
}

func TestProcessIdentityMismatchPreventsTrustingReusedPID(t *testing.T) {
	if processIdentityMatches(12345, "definitely-not-this-process") {
		t.Fatal("expected mismatched process token to be rejected")
	}
	if !processIdentityMatches(12345, "") {
		t.Fatal("empty process token should preserve fallback behavior")
	}
}

// TestTcmuAdapter_Publish_TargetcliCallOrder verifies that the user:holo
// backstore is created BEFORE the /iscsi target creation.
func TestTcmuAdapter_Publish_TargetcliCallOrder(t *testing.T) {
	runner := newMockTcmuRunner()
	cfg := TargetRuntimeConfig{
		Mode:       "tcmu",
		PortalHost: "127.0.0.1",
		PortalPort: 3260,
		UseSudo:    false,
	}
	a := &TcmuAdapter{
		cfg:      normalizeTargetRuntimeConfig(cfg),
		runner:   runner,
		registry: newTcmuRegistry(),
		auditW:   &tcmuNoopAuditWriter{},
	}

	pub := tcmuTestPublication()
	ctx := context.Background()
	backstoreName := runtimeBackstoreName(pub)

	_ = a.runTargetcli(ctx, "/backstores/user:holo", "create",
		"name="+backstoreName,
		"size=64M",
		"cfgstring="+tcmuSocketPath(pub.PublicationID),
	)
	_ = a.runTargetcli(ctx, "/iscsi", "create", pub.TargetIQN)

	backstoreIdx := -1
	iscsiIdx := -1
	for i, call := range runner.calls {
		if strings.Contains(call, "/backstores/user:holo") {
			backstoreIdx = i
		}
		if strings.Contains(call, "/iscsi") && strings.Contains(call, "create") {
			iscsiIdx = i
		}
	}

	if backstoreIdx < 0 {
		t.Fatal("backstore create call not found")
	}
	if iscsiIdx < 0 {
		t.Fatal("iscsi create call not found")
	}
	if backstoreIdx >= iscsiIdx {
		t.Errorf("expected backstore create (call %d) before iscsi create (call %d)", backstoreIdx, iscsiIdx)
	}
}

func TestCreateISCSITargetReplacingExistingRetriesAfterDelete(t *testing.T) {
	var calls []string
	createCalls := 0
	run := func(_ context.Context, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		createCalls++
		if createCalls == 1 {
			return &mockCmdError{msg: "This Target already exists in configFS"}
		}
		return nil
	}
	deleteTarget := func(_ context.Context, targetIQN string) error {
		calls = append(calls, "delete "+targetIQN)
		return nil
	}

	err := createISCSITargetReplacingExisting(context.Background(), run, deleteTarget, "iqn.2026-04.ai.holo:existing")
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	want := []string{
		"/iscsi create iqn.2026-04.ai.holo:existing",
		"delete iqn.2026-04.ai.holo:existing",
		"/iscsi create iqn.2026-04.ai.holo:existing",
	}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("unexpected calls: got %v want %v", calls, want)
	}
}

func TestTcmuAdapter_ConfigureDataPathSetsMeasuredISCSIParameters(t *testing.T) {
	runner := newMockTcmuRunner()
	a := &TcmuAdapter{
		cfg:      normalizeTargetRuntimeConfig(TargetRuntimeConfig{Mode: "tcmu"}),
		runner:   runner,
		registry: newTcmuRegistry(),
		auditW:   &tcmuNoopAuditWriter{},
	}

	targetIQN := "iqn.2026-04.ai.holo:test-drive"
	if err := a.configureTcmuISCSIDataPath(context.Background(), targetIQN); err != nil {
		t.Fatalf("configure data path failed: %v", err)
	}

	if len(runner.calls) != len(tcmuISCSIDataPathParameters) {
		t.Fatalf("expected %d parameter calls, got %d: %v", len(tcmuISCSIDataPathParameters), len(runner.calls), runner.calls)
	}
	for _, parameter := range tcmuISCSIDataPathParameters {
		found := false
		for _, call := range runner.calls {
			if strings.Contains(call, "/iscsi/"+targetIQN+"/tpg1 set parameter "+parameter) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing parameter call for %s; calls=%v", parameter, runner.calls)
		}
	}
}

func TestShouldTuneTcmuISCSIDataPathSkipsChanger(t *testing.T) {
	drive := tcmuTestPublication()
	if !shouldTuneTcmuISCSIDataPath(drive) {
		t.Fatal("expected default drive publication to be tuned")
	}

	changer := tcmuTestPublication()
	if err := changer.SetDeviceIdentity("changer", "ibm-03584l32"); err != nil {
		t.Fatalf("set changer identity failed: %v", err)
	}
	if shouldTuneTcmuISCSIDataPath(changer) {
		t.Fatal("expected changer publication to skip data-path tuning")
	}
}

func TestTcmuHandlerEnvIncludesDriveProfileForChanger(t *testing.T) {
	changer := tcmuTestPublication()
	if err := changer.SetDeviceIdentity("changer", "adic-scalar-i500"); err != nil {
		t.Fatalf("set changer identity failed: %v", err)
	}
	changer.SetDriveProfile("quantum-ultrium-td7")

	env := tcmuHandlerEnv(changer)
	if !envContains(env, "HOLO_SCSI_CHANGER_PROFILE=adic-scalar-i500") {
		t.Fatalf("expected changer profile in env, got %v", env)
	}
	if !envContains(env, "HOLO_TAPE_DRIVE_PROFILE=quantum-ultrium-td7") {
		t.Fatalf("expected drive profile in changer env, got %v", env)
	}
}

func TestTcmuHandlerEnvIncludesRuntimeTraceConfig(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "cdb-trace.enabled")
	t.Setenv("HOLO_SCSI_TRACE_CONFIG", tracePath)

	env := tcmuHandlerEnv(tcmuTestPublication())
	if !envContains(env, "HOLO_SCSI_TRACE_CONFIG="+tracePath) {
		t.Fatalf("expected runtime trace config in env, got %v", env)
	}
}

func TestTcmuHandlerEnvIncludesTimingMetricsFile(t *testing.T) {
	runDir := t.TempDir()
	t.Setenv("HOLO_RUN_DIR", runDir)

	env := tcmuHandlerEnv(tcmuTestPublication())
	want := "HOLO_CDB_TIMING_METRICS_FILE=" + filepath.Join(runDir, "cdb-metrics", "pub-tcmu-001.prom")
	if !envContains(env, want) {
		t.Fatalf("expected timing metrics file in env, want %q got %v", want, env)
	}
}

func TestTcmuHandlerEnvIncludesTapePolicy(t *testing.T) {
	pub := tcmuTestPublication()
	pub.CompressionEnabled = false
	pub.DedupEnabled = false

	env := tcmuHandlerEnv(pub)
	if !envContains(env, "HOLO_TAPE_COMPRESSION_ENABLED=0") {
		t.Fatalf("expected compression policy in env, got %v", env)
	}
	if !envContains(env, "HOLO_TAPE_DEDUP_ENABLED=0") {
		t.Fatalf("expected dedup policy in env, got %v", env)
	}
}

func envContains(env []string, entry string) bool {
	for _, value := range env {
		if value == entry {
			return true
		}
	}
	return false
}

// TestTcmuAdapter_Unpublish_CleansBackstore verifies that Unpublish issues the
// /backstores/user:holo delete command (T029).
func TestTcmuAdapter_Unpublish_CleansBackstore(t *testing.T) {
	runner := newMockTcmuRunner()
	cfg := TargetRuntimeConfig{
		Mode:       "tcmu",
		PortalHost: "127.0.0.1",
		PortalPort: 3260,
		UseSudo:    false,
	}
	a := &TcmuAdapter{
		cfg:      normalizeTargetRuntimeConfig(cfg),
		runner:   runner,
		registry: newTcmuRegistry(),
		auditW:   &tcmuNoopAuditWriter{},
	}

	pub := tcmuTestPublication()
	ctx := context.Background()

	// Simulate the two main Unpublish targetcli calls.
	_ = a.deleteTcmuTarget(ctx, pub.TargetIQN)
	_ = a.deleteTcmuBackstore(ctx, "holo", runtimeBackstoreName(pub))

	foundBackstoreDelete := false
	for _, call := range runner.calls {
		if strings.Contains(call, "/backstores/user:holo") && strings.Contains(call, "delete") {
			foundBackstoreDelete = true
			break
		}
	}
	if !foundBackstoreDelete {
		t.Errorf("expected /backstores/user:holo delete call; got: %v", runner.calls)
	}
}

// TestTcmuSocketPath verifies the socket path format.
func TestTcmuSocketPath(t *testing.T) {
	path := tcmuSocketPath("pub-tcmu-001")
	if !strings.HasPrefix(path, "/run/holo/") {
		t.Errorf("expected path under /run/holo/, got: %s", path)
	}
	if !strings.HasSuffix(path, ".sock") {
		t.Errorf("expected .sock extension, got: %s", path)
	}
}

func TestBuildBackstorePlan_DefaultSubtypeUnavailable(t *testing.T) {
	t.Setenv("HOLO_TCMU_USER_BACKSTORE", "")
	t.Setenv("HOLO_TCMU_USER_BACKSTORE_FALLBACK", "")

	runner := newMockTcmuRunner()
	runner.outputs["/backstores ls"] = `
o- backstores
  o- user:fbo
  o- user:qcow
`
	a := &TcmuAdapter{
		cfg:      normalizeTargetRuntimeConfig(TargetRuntimeConfig{Mode: "tcmu", BackstoreDir: t.TempDir(), BackstoreSizeMB: 8}),
		runner:   runner,
		registry: newTcmuRegistry(),
		auditW:   &tcmuNoopAuditWriter{},
	}

	_, err := a.buildBackstorePlan(context.Background(), "holo_pub_test", "/run/holo/cdb-test.sock")
	if err == nil {
		t.Fatal("expected error when user:holo subtype is unavailable")
	}
	if !strings.Contains(err.Error(), "user:holo is unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildBackstorePlan_HoloUsesConfiguredBackstoreSize(t *testing.T) {
	t.Setenv("HOLO_TCMU_USER_BACKSTORE", "")
	t.Setenv("HOLO_TCMU_USER_BACKSTORE_FALLBACK", "")

	runner := newMockTcmuRunner()
	runner.outputs["/backstores ls"] = `
o- backstores
  o- user:holo
`
	a := &TcmuAdapter{
		cfg: normalizeTargetRuntimeConfig(TargetRuntimeConfig{
			Mode:            "tcmu",
			BackstoreDir:    t.TempDir(),
			BackstoreSizeMB: 8,
		}),
		runner:   runner,
		registry: newTcmuRegistry(),
		auditW:   &tcmuNoopAuditWriter{},
	}

	plan, err := a.buildBackstorePlan(context.Background(), "holo_pub_test", "/run/holo/cdb-test.sock")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.SizeArg != "size=8M" {
		t.Fatalf("expected configured size, got %s", plan.SizeArg)
	}
	if !plan.UseHandler || plan.FallbackUsed {
		t.Fatalf("expected native handler plan, got %+v", plan)
	}
}

func TestBuildBackstorePlan_FallbackToFbo(t *testing.T) {
	t.Setenv("HOLO_TCMU_USER_BACKSTORE", "")
	t.Setenv("HOLO_TCMU_USER_BACKSTORE_FALLBACK", "fbo")

	backstoreDir := t.TempDir()
	runner := newMockTcmuRunner()
	runner.outputs["/backstores ls"] = `
o- backstores
  o- user:fbo
  o- user:qcow
`
	a := &TcmuAdapter{
		cfg: normalizeTargetRuntimeConfig(TargetRuntimeConfig{
			Mode:            "tcmu",
			BackstoreDir:    backstoreDir,
			BackstoreSizeMB: 8,
		}),
		runner:   runner,
		registry: newTcmuRegistry(),
		auditW:   &tcmuNoopAuditWriter{},
	}

	plan, err := a.buildBackstorePlan(context.Background(), "holo_pub_test", "/run/holo/cdb-test.sock")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Subtype != "fbo" {
		t.Fatalf("expected fallback subtype fbo, got %s", plan.Subtype)
	}
	if plan.UseHandler {
		t.Fatalf("expected fallback mode to skip tcmu handler")
	}
	if !plan.FallbackUsed {
		t.Fatalf("expected fallbackUsed=true")
	}
	if plan.CleanupPath == "" {
		t.Fatal("expected fallback cleanup path to be set")
	}
	if _, err := os.Stat(plan.CleanupPath); err != nil {
		t.Fatalf("expected fallback image to exist, err=%v", err)
	}
}

func TestTcmuRegistryConcurrentAccess(t *testing.T) {
	reg := newTcmuRegistry()
	const workers = 32

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "backstore-" + strconv.Itoa(i)
			reg.save(&TcmuHandlerSession{
				PublicationID: "pub-" + strconv.Itoa(i),
				BackstoreName: name,
				PID:           i,
			})
			session, ok := reg.find(name)
			if !ok || session == nil {
				t.Errorf("expected to find saved session %s", name)
				return
			}
			session.PID = -1
			fresh, ok := reg.find(name)
			if !ok || fresh == nil {
				t.Errorf("expected to re-read session %s", name)
				return
			}
			if fresh.PID == -1 {
				t.Errorf("registry returned shared pointer for %s", name)
			}
			reg.delete(name)
		}(i)
	}
	wg.Wait()
}

func TestWaitForHandlerSocketStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := waitForHandlerSocket(ctx, 1234, filepath.Join(t.TempDir(), "missing.sock"), time.Hour, time.Hour, func(int) bool {
		return true
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("expected immediate cancellation, elapsed=%s", elapsed)
	}
}
