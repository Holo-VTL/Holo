package orchestration

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/storageutil"
)

// TcmuHandlerSession holds runtime state for a single TCMU-backed publication.
type TcmuHandlerSession struct {
	// PublicationID is the holo publication this session belongs to.
	PublicationID string
	// SocketPath is the UNIX domain socket where the data-plane CDB server listens.
	SocketPath string
	// PID of the spawned tcmu_handler binary. 0 if not yet spawned.
	PID int
	// ProcessStartToken identifies the spawned process on Linux to avoid PID reuse.
	ProcessStartToken string
	// BackstoreName is the targetcli user:holo backstore object name.
	BackstoreName string
	// BackstoreSubtype is the targetcli user handler type (for example holo, fbo).
	BackstoreSubtype string
	// BackstoreConfigPath stores cfgstring path for file-based fallback handlers.
	BackstoreConfigPath string
}

var tcmuTargetcliTPGAttributes = []string{
	"authentication=0",
	"generate_node_acls=1",
	"demo_mode_write_protect=0",
	"cache_dynamic_acls=1",
}

var tcmuISCSIDataPathParameters = []string{
	"InitialR2T=No",
	"ImmediateData=Yes",
	"FirstBurstLength=8388608",
	"MaxBurstLength=8388608",
	"MaxRecvDataSegmentLength=8388608",
	"MaxXmitDataSegmentLength=8388608",
}

// tcmuRegistry stores active sessions keyed by BackstoreName.
type tcmuRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*TcmuHandlerSession
}

func newTcmuRegistry() *tcmuRegistry {
	return &tcmuRegistry{sessions: make(map[string]*TcmuHandlerSession)}
}

func (r *tcmuRegistry) save(s *TcmuHandlerSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[s.BackstoreName] = cloneTcmuSession(s)
}

func (r *tcmuRegistry) delete(backstoreName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, backstoreName)
}

func (r *tcmuRegistry) find(backstoreName string) (*TcmuHandlerSession, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[backstoreName]
	if !ok {
		return nil, false
	}
	return cloneTcmuSession(s), ok
}

func cloneTcmuSession(in *TcmuHandlerSession) *TcmuHandlerSession {
	if in == nil {
		return nil
	}
	cp := *in
	return &cp
}

// TcmuAdapter implements TargetRuntimeAdapter using TCMU user:holo backstores.
// It replaces the fileio image approach with direct CDB dispatch to the data-plane.
type TcmuAdapter struct {
	cfg      TargetRuntimeConfig
	runner   commandRunner
	registry *tcmuRegistry
	auditW   audit.Writer
}

type tcmuBackstorePlan struct {
	Subtype       string
	CfgString     string
	SizeArg       string
	UseHandler    bool
	FallbackUsed  bool
	CleanupPath   string
	AvailableList []string
}

func newTcmuAdapter(cfg TargetRuntimeConfig, runner commandRunner, auditW audit.Writer) *TcmuAdapter {
	if runner == nil {
		runner = &osCommandRunner{}
	}
	return &TcmuAdapter{
		cfg:      normalizeTargetRuntimeConfig(cfg),
		runner:   runner,
		registry: newTcmuRegistry(),
		auditW:   auditW,
	}
}

// Publish creates a user:holo TCMU backstore for the publication and brings
// up the iSCSI target, exposing a Type-1 SCSI tape device to initiators.
func (a *TcmuAdapter) Publish(ctx context.Context, publication *domain.TargetPublication) (string, error) {
	backstoreName := runtimeBackstoreName(publication)
	socketPath := tcmuSocketPath(publication.PublicationID)

	plan, err := a.buildBackstorePlan(ctx, backstoreName, socketPath)
	if err != nil {
		audit.EmitTargetRuntimeEvent(ctx, a.auditW, "system", "tcmu_publish", publication.PublicationID, "failure",
			map[string]any{
				"step":          "resolve_backstore",
				"error":         err.Error(),
				"runtimeMode":   "tcmu",
				"availableUser": strings.Join(plan.AvailableList, ","),
			})
		return "", err
	}

	pid := 0
	if plan.UseHandler {
		// 1. Spawn the data-plane CDB handler (tcmu_handler binary).
		pid, err = a.spawnHandler(ctx, publication, socketPath)
		if err != nil {
			audit.EmitTargetRuntimeEvent(ctx, a.auditW, "system", "tcmu_publish", publication.PublicationID, "failure",
				map[string]any{"step": "spawn_handler", "error": err.Error(), "runtimeMode": "tcmu"})
			return "", fmt.Errorf("spawn tcmu handler: %w", err)
		}
	}

	// 2. Register user:holo backstore in targetcli.
	if err := a.runTargetcli(ctx, "/backstores/user:"+plan.Subtype, "create",
		"name="+backstoreName,
		plan.SizeArg,
		"cfgstring="+plan.CfgString,
	); err != nil {
		a.killHandler(pid)
		if plan.CleanupPath != "" {
			_ = os.Remove(plan.CleanupPath)
		}
		audit.EmitTargetRuntimeEvent(ctx, a.auditW, "system", "tcmu_publish", publication.PublicationID, "failure",
			map[string]any{"step": "create_backstore", "error": err.Error(), "runtimeMode": "tcmu"})
		return "", fmt.Errorf("create user:%s backstore: %w", plan.Subtype, err)
	}

	// 3. Create iSCSI target.
	if err := createISCSITargetReplacingExisting(ctx, a.runTargetcli, a.deleteTcmuTarget, publication.TargetIQN); err != nil {
		_ = a.deleteTcmuBackstore(ctx, plan.Subtype, backstoreName)
		a.killHandler(pid)
		if plan.CleanupPath != "" {
			_ = os.Remove(plan.CleanupPath)
		}
		audit.EmitTargetRuntimeEvent(ctx, a.auditW, "system", "tcmu_publish", publication.PublicationID, "failure",
			map[string]any{"step": "create_iscsi_target", "error": err.Error(), "runtimeMode": "tcmu"})
		return "", fmt.Errorf("create iscsi target: %w", err)
	}

	// 4. Configure TPG attributes.
	tpgAttributeArgs := append([]string{
		"/iscsi/" + publication.TargetIQN + "/tpg1",
		"set", "attribute",
	}, tcmuTargetcliTPGAttributes...)
	if err := a.runTargetcli(ctx, tpgAttributeArgs...); err != nil {
		_ = a.deleteTcmuTarget(ctx, publication.TargetIQN)
		_ = a.deleteTcmuBackstore(ctx, plan.Subtype, backstoreName)
		a.killHandler(pid)
		if plan.CleanupPath != "" {
			_ = os.Remove(plan.CleanupPath)
		}
		return "", fmt.Errorf("configure iscsi tpg attributes: %w", err)
	}

	// 5. Tune the drive data path before initiators log in.
	if shouldTuneTcmuISCSIDataPath(publication) {
		if err := a.configureTcmuISCSIDataPath(ctx, publication.TargetIQN); err != nil {
			_ = a.deleteTcmuTarget(ctx, publication.TargetIQN)
			_ = a.deleteTcmuBackstore(ctx, plan.Subtype, backstoreName)
			a.killHandler(pid)
			if plan.CleanupPath != "" {
				_ = os.Remove(plan.CleanupPath)
			}
			return "", err
		}
	}

	// 6. Attach LUN from user:holo backstore.
	lunPath := "/backstores/user:" + plan.Subtype + "/" + backstoreName
	if err := a.runTargetcli(ctx, "/iscsi/"+publication.TargetIQN+"/tpg1/luns", "create", lunPath); err != nil {
		_ = a.deleteTcmuTarget(ctx, publication.TargetIQN)
		_ = a.deleteTcmuBackstore(ctx, plan.Subtype, backstoreName)
		a.killHandler(pid)
		if plan.CleanupPath != "" {
			_ = os.Remove(plan.CleanupPath)
		}
		return "", fmt.Errorf("create iscsi lun: %w", err)
	}

	// 7. Record session.
	session := &TcmuHandlerSession{
		PublicationID:       publication.PublicationID,
		SocketPath:          socketPath,
		PID:                 pid,
		ProcessStartToken:   processStartToken(pid),
		BackstoreName:       backstoreName,
		BackstoreSubtype:    plan.Subtype,
		BackstoreConfigPath: plan.CleanupPath,
	}
	a.registry.save(session)

	portal := fmt.Sprintf("%s:%d", a.cfg.PortalHost, a.cfg.PortalPort)
	audit.EmitTargetRuntimeEvent(ctx, a.auditW, "system", "tcmu_publish", publication.PublicationID, "success",
		map[string]any{
			"runtimeMode":   "tcmu",
			"backstoreName": backstoreName,
			"socketPath":    socketPath,
			"pid":           pid,
			"portal":        portal,
			"backstoreType": plan.Subtype,
			"fallbackUsed":  plan.FallbackUsed,
		})
	return portal, nil
}

func shouldTuneTcmuISCSIDataPath(publication *domain.TargetPublication) bool {
	if publication == nil {
		return false
	}
	role := strings.TrimSpace(publication.DeviceRole)
	return role == "" || role == "drive"
}

func (a *TcmuAdapter) configureTcmuISCSIDataPath(ctx context.Context, targetIQN string) error {
	tpgPath := "/iscsi/" + targetIQN + "/tpg1"
	for _, parameter := range tcmuISCSIDataPathParameters {
		if err := a.runTargetcli(ctx, tpgPath, "set", "parameter", parameter); err != nil {
			return fmt.Errorf("configure iscsi tpg parameter %s: %w", parameter, err)
		}
	}
	return nil
}

// Unpublish tears down the iSCSI target, removes the user:holo backstore,
// terminates the CDB handler process, and removes the socket file.
func (a *TcmuAdapter) Unpublish(ctx context.Context, publication *domain.TargetPublication) error {
	backstoreName := runtimeBackstoreName(publication)
	socketPath := tcmuSocketPath(publication.PublicationID)
	backstoreSubtype := desiredTcmuSubtype()
	var backstoreConfigPath string
	var session *TcmuHandlerSession

	if s, ok := a.registry.find(backstoreName); ok {
		session = s
		if s.BackstoreSubtype != "" {
			backstoreSubtype = s.BackstoreSubtype
		}
		backstoreConfigPath = s.BackstoreConfigPath
	}

	// 1. Stop userspace socket worker first. Some targetcli/tcmu-runner
	// combinations block target deletion while the user handler is still
	// connected to the TCMU socket.
	if session != nil {
		a.killSession(session)
	}

	// 2. Remove iSCSI target.
	if err := a.deleteTcmuTarget(ctx, publication.TargetIQN); err != nil {
		audit.EmitTargetRuntimeEvent(ctx, a.auditW, "system", "tcmu_unpublish", publication.PublicationID, "failure",
			map[string]any{"step": "delete_target", "error": err.Error(), "runtimeMode": "tcmu"})
		return err
	}

	// 3. Remove user:holo backstore.
	if err := a.deleteTcmuBackstore(ctx, backstoreSubtype, backstoreName); err != nil {
		audit.EmitTargetRuntimeEvent(ctx, a.auditW, "system", "tcmu_unpublish", publication.PublicationID, "failure",
			map[string]any{"step": "delete_backstore", "error": err.Error(), "runtimeMode": "tcmu"})
		return err
	}

	// 4. Clean in-memory/runtime artifacts.
	if session != nil {
		a.registry.delete(backstoreName)
	}
	if backstoreConfigPath != "" {
		_ = os.Remove(backstoreConfigPath)
	}

	// 5. Remove socket file.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove socket file: %w", err)
	}

	audit.EmitTargetRuntimeEvent(ctx, a.auditW, "system", "tcmu_unpublish", publication.PublicationID, "success",
		map[string]any{"runtimeMode": "tcmu", "backstoreName": backstoreName})
	return nil
}

// spawnHandler starts the tcmu_handler binary that listens on socketPath and
// dispatches CDBs to the data-plane's tape state machine.
// Returns the PID of the spawned process, or an error.
func (a *TcmuAdapter) spawnHandler(ctx context.Context, publication *domain.TargetPublication, socketPath string) (int, error) {
	publicationID := publication.PublicationID
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return 0, fmt.Errorf("create socket dir: %w", err)
	}

	handlerBin := tcmuHandlerBinary()
	// Do not bind worker lifetime to request context. Publish returns quickly,
	// but handler must stay alive until Unpublish.
	cmd := exec.Command(handlerBin,
		"--socket-path", socketPath,
		"--publication-id", publicationID,
	)
	cmd.Env = tcmuHandlerEnv(publication)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start tcmu_handler: %w", err)
	}
	pid := cmd.Process.Pid
	// Reap child exit status to avoid zombie processes.
	go func() {
		_ = cmd.Wait()
	}()

	if err := waitForHandlerSocket(ctx, pid, socketPath, 5*time.Second, 100*time.Millisecond, processAlive); err != nil {
		a.killHandler(pid)
		return 0, err
	}
	return pid, nil
}

func waitForHandlerSocket(ctx context.Context, pid int, socketPath string, timeout, interval time.Duration, alive func(int) bool) error {
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if _, err := os.Stat(socketPath); err == nil {
			return nil
		}
		if !alive(pid) {
			return fmt.Errorf("tcmu_handler exited before creating socket %s", socketPath)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for tcmu_handler socket %s: %w", socketPath, ctx.Err())
		case <-timer.C:
			return fmt.Errorf("tcmu_handler did not create socket %s within %s", socketPath, timeout)
		case <-ticker.C:
		}
	}
}

func tcmuHandlerEnv(publication *domain.TargetPublication) []string {
	role := strings.TrimSpace(publication.DeviceRole)
	if role == "" {
		role = "drive"
	}
	serialSeed := strings.TrimSpace(publication.DriveID)
	if serialSeed == "" {
		serialSeed = publication.PublicationID
	}
	env := os.Environ()
	env = withEnvAssignment(env, "HOLO_SCSI_DEVICE_ROLE", role)
	env = withEnvAssignment(env, "HOLO_SCSI_SERIAL_SEED", serialSeed)
	env = withEnvAssignment(env, "HOLO_MEDIA_STATE_KEY", storageutil.MediaStateKey(publication.LibraryID, publication.DriveID))
	env = withEnvAssignment(env, "HOLO_STORAGE_ROOT", storageutil.PoolStorageRoot(publication.PoolID))
	env = withEnvAssignment(env, "HOLO_SCSI_TRACE_CONFIG", tcmuTraceConfigPath())
	env = withEnvAssignment(env, "HOLO_CDB_TIMING_METRICS_FILE", tcmuTimingMetricsPath(publication.PublicationID))
	env = withEnvAssignment(env, "HOLO_TAPE_COMPRESSION_ENABLED", runtimeBoolEnv(publication.CompressionEnabled))
	env = withEnvAssignment(env, "HOLO_TAPE_DEDUP_ENABLED", runtimeBoolEnv(publication.DedupEnabled))
	if traceRaw := strings.TrimSpace(os.Getenv("HOLO_SCSI_TRACE")); traceRaw != "" {
		env = withEnvAssignment(env, "HOLO_SCSI_TRACE", traceRaw)
	}
	if mediaStateDir := strings.TrimSpace(os.Getenv("HOLO_MEDIA_STATE_DIR")); mediaStateDir != "" {
		env = withEnvAssignment(env, "HOLO_MEDIA_STATE_DIR", mediaStateDir)
	}
	profile := strings.TrimSpace(publication.DeviceProfile)
	if profile != "" {
		switch role {
		case "changer":
			env = withEnvAssignment(env, "HOLO_SCSI_CHANGER_PROFILE", profile)
		default:
			env = withEnvAssignment(env, "HOLO_TAPE_DRIVE_PROFILE", profile)
		}
	}
	if role == "changer" {
		driveProfile := strings.TrimSpace(publication.DriveProfile)
		if driveProfile != "" {
			env = withEnvAssignment(env, "HOLO_TAPE_DRIVE_PROFILE", driveProfile)
		}
	}
	return env
}

func runtimeBoolEnv(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func tcmuTraceConfigPath() string {
	if path := strings.TrimSpace(os.Getenv("HOLO_SCSI_TRACE_CONFIG")); path != "" {
		return path
	}
	runDir := strings.TrimSpace(os.Getenv("HOLO_RUN_DIR"))
	if runDir == "" {
		runDir = "/run/holo"
	}
	return filepath.Join(runDir, "cdb-trace.enabled")
}

func tcmuTimingMetricsPath(publicationID string) string {
	runDir := strings.TrimSpace(os.Getenv("HOLO_RUN_DIR"))
	if runDir == "" {
		runDir = "/run/holo"
	}
	return filepath.Join(runDir, "cdb-metrics", storageutil.SanitizeLayoutID(publicationID)+".prom")
}

func withEnvAssignment(env []string, key, value string) []string {
	prefix := key + "="
	next := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			next = append(next, entry)
		}
	}
	return append(next, prefix+value)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM means process exists but we don't have permission.
	return errors.Is(err, syscall.EPERM)
}

func processStartToken(pid int) string {
	if pid <= 0 {
		return ""
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	text := string(raw)
	end := strings.LastIndex(text, ")")
	if end < 0 || end+2 >= len(text) {
		return ""
	}
	fields := strings.Fields(text[end+2:])
	if len(fields) <= 19 {
		return ""
	}
	return fields[19]
}

func processIdentityMatches(pid int, startToken string) bool {
	if pid <= 0 || strings.TrimSpace(startToken) == "" {
		return true
	}
	return processStartToken(pid) == startToken
}

func (a *TcmuAdapter) killSession(session *TcmuHandlerSession) {
	if session == nil {
		return
	}
	if !processIdentityMatches(session.PID, session.ProcessStartToken) {
		return
	}
	a.killHandler(session.PID)
}

func (a *TcmuAdapter) killHandler(pid int) {
	if pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(os.Interrupt)
	time.Sleep(200 * time.Millisecond)
	_ = proc.Kill()
}

func (a *TcmuAdapter) deleteTcmuTarget(ctx context.Context, targetIQN string) error {
	err := a.runTargetcli(ctx, "/iscsi", "delete", targetIQN)
	if err != nil && !isIgnorableTargetcliError(err) {
		return fmt.Errorf("delete iscsi target: %w", err)
	}
	return nil
}

func (a *TcmuAdapter) deleteTcmuBackstore(ctx context.Context, subtype, backstoreName string) error {
	if strings.TrimSpace(subtype) == "" {
		subtype = desiredTcmuSubtype()
	}
	err := a.runTargetcli(ctx, "/backstores/user:"+subtype, "delete", backstoreName)
	if err != nil && !isIgnorableTargetcliError(err) {
		return fmt.Errorf("delete user:%s backstore: %w", subtype, err)
	}
	return nil
}

func (a *TcmuAdapter) runTargetcli(ctx context.Context, args ...string) error {
	_, err := a.runTargetcliOutput(ctx, args...)
	return err
}

func (a *TcmuAdapter) runTargetcliOutput(ctx context.Context, args ...string) (string, error) {
	if err := validateTargetcliArgs(args...); err != nil {
		return "", err
	}
	timeout := targetcliTimeout()
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, cmdArgs := targetcliCommand(a.cfg.UseSudo, args...)
	out, err := a.runner.Run(timeoutCtx, cmd, cmdArgs...)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(timeoutCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("targetcli timed out after %s: %s", timeout, strings.Join(args, " "))
		}
		return "", err
	}
	return out, nil
}

func (a *TcmuAdapter) ListSessions(ctx context.Context) ([]TargetSession, error) {
	sessions, err := listConfigfsTargetSessions(a.cfg.IscsiConfigfsRoot)
	if err == nil && sessions != nil {
		return sessions, nil
	}
	if err != nil {
		log.Printf("configfs target session discovery unavailable err=%v", err)
	}

	out, err := a.runTargetcliOutput(ctx, "sessions", "detail")
	if err != nil {
		return fallbackIscsiadmSessions(ctx, a.runner, a.cfg.UseSudo, nil), nil
	}
	sessions = parseTargetcliSessions(out)
	if len(sessions) > 0 {
		return sessions, nil
	}
	return fallbackIscsiadmSessions(ctx, a.runner, a.cfg.UseSudo, sessions), nil
}

func targetcliTimeout() time.Duration {
	const defaultTimeout = 15 * time.Second
	raw := strings.TrimSpace(os.Getenv("HOLO_TCMU_TARGETCLI_TIMEOUT_SEC"))
	if raw == "" {
		return defaultTimeout
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		return defaultTimeout
	}
	return time.Duration(secs) * time.Second
}

func desiredTcmuSubtype() string {
	if v := strings.TrimSpace(os.Getenv("HOLO_TCMU_USER_BACKSTORE")); v != "" {
		return strings.ToLower(v)
	}
	return "holo"
}

func fallbackTcmuSubtype() string {
	return strings.ToLower(strings.TrimSpace(os.Getenv("HOLO_TCMU_USER_BACKSTORE_FALLBACK")))
}

var userBackstorePattern = regexp.MustCompile(`user:([a-zA-Z0-9_-]+)`)

func (a *TcmuAdapter) availableUserBackstores(ctx context.Context) ([]string, error) {
	out, err := a.runTargetcliOutput(ctx, "/backstores", "ls")
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	matches := userBackstorePattern.FindAllStringSubmatch(out, -1)
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(m[1]))
		if name != "" {
			seen[name] = struct{}{}
		}
	}
	available := make([]string, 0, len(seen))
	for k := range seen {
		available = append(available, k)
	}
	sort.Strings(available)
	return available, nil
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func (a *TcmuAdapter) buildBackstorePlan(ctx context.Context, backstoreName, socketPath string) (tcmuBackstorePlan, error) {
	available, err := a.availableUserBackstores(ctx)
	if err != nil {
		return tcmuBackstorePlan{}, fmt.Errorf("query targetcli user backstores: %w", err)
	}
	desired := desiredTcmuSubtype()
	plan := tcmuBackstorePlan{AvailableList: available}

	if containsString(available, desired) {
		plan.Subtype = desired
		plan.CfgString = socketPath
		// targetcli user backstores require integer size units.
		plan.SizeArg = fmt.Sprintf("size=%dM", a.cfg.BackstoreSizeMB)
		plan.UseHandler = true
		return plan, nil
	}

	fallback := fallbackTcmuSubtype()
	if fallback != "" && containsString(available, fallback) {
		backstorePath, err := runtimeBackstorePath(a.cfg.BackstoreDir, backstoreName)
		if err != nil {
			return tcmuBackstorePlan{}, err
		}
		if err := ensureBackstoreImage(backstorePath, a.cfg.BackstoreSizeMB); err != nil {
			return tcmuBackstorePlan{}, err
		}
		plan.Subtype = fallback
		plan.CfgString = backstorePath
		plan.SizeArg = fmt.Sprintf("size=%dM", a.cfg.BackstoreSizeMB)
		plan.UseHandler = false
		plan.FallbackUsed = true
		plan.CleanupPath = backstorePath
		return plan, nil
	}

	return plan, fmt.Errorf(
		"required targetcli backstore user:%s is unavailable (available: %s); set HOLO_TCMU_USER_BACKSTORE_FALLBACK=fbo for temporary fallback",
		desired,
		strings.Join(available, ","),
	)
}

// tcmuSocketPath returns the UNIX domain socket path for a given publication.
func tcmuSocketPath(publicationID string) string {
	safe := strings.ToLower(publicationID)
	var b strings.Builder
	for _, r := range safe {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	root := os.Getenv("HOLO_TCMU_SOCKET_DIR")
	if strings.TrimSpace(root) == "" {
		root = "/run/holo"
	}
	return filepath.Join(root, "cdb-"+b.String()+".sock")
}

// tcmuHandlerBinary returns the path to the tcmu_handler binary.
// Falls back to a relative path if the env var is not set.
func tcmuHandlerBinary() string {
	if v := os.Getenv("HOLO_TCMU_HANDLER_BIN"); v != "" {
		return v
	}
	const defaultBin = "/usr/local/bin/holo-tcmu-handler"
	if _, err := os.Stat(defaultBin); err == nil {
		return defaultBin
	}
	if v, err := exec.LookPath("holo-tcmu-handler"); err == nil {
		return v
	}
	if v, err := exec.LookPath("tcmu_handler"); err == nil {
		return v
	}
	return defaultBin
}

// tcmuSessionInfo is used in tests to introspect the adapter's session registry.
func (a *TcmuAdapter) sessionInfo(publicationID string) (*TcmuHandlerSession, bool) {
	backstoreName := "holo_" + strings.ReplaceAll(strings.ToLower(publicationID), "-", "_")
	return a.registry.find(backstoreName)
}

// Ensure TcmuAdapter implements TargetRuntimeAdapter.
var _ TargetRuntimeAdapter = (*TcmuAdapter)(nil)
