package orchestration

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/storageutil"
)

type inMemoryTargetRuntimeAdapter struct {
	portal string
}

func newInMemoryTargetRuntimeAdapter(cfg TargetRuntimeConfig) *inMemoryTargetRuntimeAdapter {
	return &inMemoryTargetRuntimeAdapter{
		portal: fmt.Sprintf("%s:%d", cfg.PortalHost, cfg.PortalPort),
	}
}

func (a *inMemoryTargetRuntimeAdapter) Publish(_ context.Context, publication *domain.TargetPublication) (string, error) {
	return a.portal, nil
}

func (a *inMemoryTargetRuntimeAdapter) Unpublish(_ context.Context, _ *domain.TargetPublication) error {
	return nil
}

func (a *inMemoryTargetRuntimeAdapter) ListSessions(_ context.Context) ([]TargetSession, error) {
	return nil, nil
}

type TargetSession struct {
	TargetIQN     string
	InitiatorIQN  string
	SourceAddress string
	SessionID     string
}

type commandRunner interface {
	Run(ctx context.Context, command string, args ...string) (string, error)
}

type targetcliRunFunc func(ctx context.Context, args ...string) error
type targetcliDeleteTargetFunc func(ctx context.Context, targetIQN string) error

type osCommandRunner struct{}

func (r *osCommandRunner) Run(ctx context.Context, command string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		if trimmed == "" {
			return "", err
		}
		return "", fmt.Errorf("%w: %s", err, trimmed)
	}
	return trimmed, nil
}

type lioShellTargetRuntimeAdapter struct {
	cfg            TargetRuntimeConfig
	runner         commandRunner
	securityHelper *ISCSISecurityHelper
}

func newLIOShellTargetRuntimeAdapter(cfg TargetRuntimeConfig, runner commandRunner) *lioShellTargetRuntimeAdapter {
	if runner == nil {
		runner = &osCommandRunner{}
	}
	return &lioShellTargetRuntimeAdapter{
		cfg:            normalizeTargetRuntimeConfig(cfg),
		runner:         runner,
		securityHelper: NewDefaultISCSISecurityHelper(cfg.UseSudo),
	}
}

func (a *lioShellTargetRuntimeAdapter) Publish(ctx context.Context, publication *domain.TargetPublication) (string, error) {
	if err := validateTargetPublicationForRuntime(publication); err != nil {
		return "", err
	}
	backstoreDir, err := lioBackstoreDir(a.cfg, publication)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(backstoreDir, 0o755); err != nil {
		return "", fmt.Errorf("create backstore directory: %w", err)
	}

	backstoreName := runtimeBackstoreName(publication)
	backstorePath, err := runtimeBackstorePath(backstoreDir, backstoreName)
	if err != nil {
		return "", err
	}
	if err := ensureBackstoreImage(backstorePath, a.cfg.BackstoreSizeMB); err != nil {
		return "", err
	}

	if err := a.runTargetcli(ctx, "/backstores/fileio", "create",
		"name="+backstoreName,
		"file_or_dev="+backstorePath,
		fmt.Sprintf("size=%dM", a.cfg.BackstoreSizeMB),
	); err != nil {
		return "", fmt.Errorf("create fileio backstore: %w", err)
	}

	if err := createISCSITargetReplacingExisting(ctx, a.runTargetcli, a.deleteTarget, publication.TargetIQN); err != nil {
		_ = a.deleteBackstore(ctx, backstoreName)
		return "", fmt.Errorf("create iscsi target: %w", err)
	}

	tpgAttributeArgs := append([]string{
		"/iscsi/" + publication.TargetIQN + "/tpg1",
		"set",
		"attribute",
	}, tcmuTargetcliTPGAttributes...)
	if err := a.runTargetcli(ctx, tpgAttributeArgs...); err != nil {
		_ = a.deleteTarget(ctx, publication.TargetIQN)
		_ = a.deleteBackstore(ctx, backstoreName)
		return "", fmt.Errorf("configure iscsi tpg attributes: %w", err)
	}

	lunPath := "/backstores/fileio/" + backstoreName
	if err := a.runTargetcli(ctx, "/iscsi/"+publication.TargetIQN+"/tpg1/luns", "create", lunPath); err != nil {
		_ = a.deleteTarget(ctx, publication.TargetIQN)
		_ = a.deleteBackstore(ctx, backstoreName)
		return "", fmt.Errorf("create iscsi lun: %w", err)
	}

	portal := fmt.Sprintf("%s:%d", a.cfg.PortalHost, a.cfg.PortalPort)
	return portal, nil
}

func (a *lioShellTargetRuntimeAdapter) PublishProtected(ctx context.Context, publication *domain.TargetPublication, security ISCSIResolvedPublicationSecurity) (string, error) {
	if err := validateTargetPublicationForRuntime(publication); err != nil {
		return "", err
	}
	backstoreDir, err := lioBackstoreDir(a.cfg, publication)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(backstoreDir, 0o755); err != nil {
		return "", fmt.Errorf("create backstore directory: %w", err)
	}
	backstoreName := runtimeBackstoreName(publication)
	backstorePath, err := runtimeBackstorePath(backstoreDir, backstoreName)
	if err != nil {
		return "", err
	}
	if err := ensureBackstoreImage(backstorePath, a.cfg.BackstoreSizeMB); err != nil {
		return "", err
	}
	if err := a.runTargetcli(ctx, "/backstores/fileio", "create",
		"name="+backstoreName, "file_or_dev="+backstorePath,
		fmt.Sprintf("size=%dM", a.cfg.BackstoreSizeMB),
	); err != nil {
		_ = os.Remove(backstorePath)
		return "", fmt.Errorf("create protected fileio backstore: %w", err)
	}
	if err := runProtectedTargetHelper(ctx, a.securityHelper, publication, security, backstoreName, "fileio", a.cfg.PortalHost, a.cfg.PortalPort); err != nil {
		cleanupErr := a.cleanupProtectedTarget(ctx, publication.TargetIQN, backstoreName, backstorePath)
		return "", errors.Join(err, cleanupErr)
	}
	return fmt.Sprintf("%s:%d", a.cfg.PortalHost, a.cfg.PortalPort), nil
}

func (a *lioShellTargetRuntimeAdapter) cleanupProtectedTarget(ctx context.Context, targetIQN, backstoreName, backstorePath string) error {
	_, targetErr := a.securityHelper.Call(ctx, map[string]any{"version": 1, "operation": "delete-owned-target", "targetIQN": targetIQN})
	backstoreErr := a.deleteBackstore(ctx, backstoreName)
	fileErr := os.Remove(backstorePath)
	if errors.Is(fileErr, os.ErrNotExist) {
		fileErr = nil
	}
	return errors.Join(targetErr, backstoreErr, fileErr)
}

func (a *lioShellTargetRuntimeAdapter) Unpublish(ctx context.Context, publication *domain.TargetPublication) error {
	if err := validateTargetPublicationForRuntime(publication); err != nil {
		return err
	}
	backstoreName := runtimeBackstoreName(publication)

	if err := a.deleteTarget(ctx, publication.TargetIQN); err != nil {
		return err
	}
	if err := a.deleteBackstore(ctx, backstoreName); err != nil {
		return err
	}

	backstoreDir, err := lioBackstoreDir(a.cfg, publication)
	if err != nil {
		return err
	}
	backstorePath, err := runtimeBackstorePath(backstoreDir, backstoreName)
	if err != nil {
		return err
	}
	if err := os.Remove(backstorePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove backstore image: %w", err)
	}
	return nil
}

func (a *lioShellTargetRuntimeAdapter) deleteTarget(ctx context.Context, targetIQN string) error {
	err := a.runTargetcli(ctx, "/iscsi", "delete", targetIQN)
	if err != nil && !isIgnorableTargetcliError(err) {
		return fmt.Errorf("delete iscsi target: %w", err)
	}
	return nil
}

func (a *lioShellTargetRuntimeAdapter) deleteBackstore(ctx context.Context, backstoreName string) error {
	err := a.runTargetcli(ctx, "/backstores/fileio", "delete", backstoreName)
	if err != nil && !isIgnorableTargetcliError(err) {
		return fmt.Errorf("delete fileio backstore: %w", err)
	}
	return nil
}

func (a *lioShellTargetRuntimeAdapter) runTargetcli(ctx context.Context, args ...string) error {
	_, err := a.runTargetcliOutput(ctx, args...)
	return err
}

func (a *lioShellTargetRuntimeAdapter) runTargetcliOutput(ctx context.Context, args ...string) (string, error) {
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

func (a *lioShellTargetRuntimeAdapter) ListSessions(ctx context.Context) ([]TargetSession, error) {
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

func targetcliCommand(useSudo bool, args ...string) (string, []string) {
	cmdArgs := append([]string(nil), args...)
	if !useSudo {
		return "targetcli", cmdArgs
	}
	target := strings.TrimSpace(os.Getenv("HOLO_TARGETCLI_PRIVILEGED_HELPER"))
	if !isSafeTargetcliHelperPath(target) {
		if target != "" {
			warnInvalidTargetcliHelper(target)
		}
		target = "targetcli"
	}
	return "sudo", append([]string{"-n", target}, cmdArgs...)
}

func iscsiadmCommand(useSudo bool, args ...string) (string, []string) {
	cmdArgs := append([]string(nil), args...)
	if !useSudo {
		return "iscsiadm", cmdArgs
	}
	return "sudo", append([]string{"-n", "iscsiadm"}, cmdArgs...)
}

var invalidTargetcliHelperWarnings sync.Map

func warnInvalidTargetcliHelper(path string) {
	if _, loaded := invalidTargetcliHelperWarnings.LoadOrStore(path, struct{}{}); loaded {
		return
	}
	log.Printf("WARNING: ignoring unsafe HOLO_TARGETCLI_PRIVILEGED_HELPER=%q; using targetcli fallback", path)
}

func isSafeTargetcliHelperPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	cleaned := filepath.Clean(path)
	if filepath.Base(cleaned) != "holo-targetcli-helper" || filepath.Base(filepath.Dir(cleaned)) != "bin" {
		return false
	}
	for _, unsafeRoot := range []string{"/tmp", "/var/tmp", "/dev/shm"} {
		if cleaned == unsafeRoot || strings.HasPrefix(cleaned, unsafeRoot+string(os.PathSeparator)) {
			return false
		}
	}
	exe, err := os.Executable()
	if err == nil && cleaned == filepath.Join(filepath.Dir(exe), "holo-targetcli-helper") {
		return true
	}
	return strings.HasPrefix(cleaned, string(os.PathSeparator)+"opt"+string(os.PathSeparator)) ||
		strings.HasPrefix(cleaned, string(os.PathSeparator)+"usr"+string(os.PathSeparator))
}

func runtimeBackstoreName(publication *domain.TargetPublication) string {
	base := publication.PublicationID
	if strings.TrimSpace(base) == "" {
		base = publication.TargetIQN
	}
	base = strings.ToLower(base)
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	name := strings.Trim(b.String(), "_")
	if name == "" {
		name = "holo_pub"
	}
	if len(name) > 48 {
		name = name[:48]
	}
	return "holo_" + name
}

func runtimeBackstorePath(backstoreDir, backstoreName string) (string, error) {
	if !isSafeTargetcliToken(backstoreName) {
		return "", domain.ErrInvalidInput
	}
	return storageutil.SafeJoin(backstoreDir, backstoreName+".img")
}

func lioBackstoreDir(cfg TargetRuntimeConfig, publication *domain.TargetPublication) (string, error) {
	if publication != nil && strings.TrimSpace(publication.PoolID) != "" {
		poolBase := storageutil.ResolvePoolStorageBaseDir()
		if err := storageutil.ValidateRoot(storageutil.RootKindPool, poolBase); err != nil {
			return "", err
		}
		poolRoot, err := storageutil.SafeJoin(poolBase, storageutil.SanitizeLayoutID(publication.PoolID))
		if err != nil {
			return "", err
		}
		return storageutil.SafeJoin(poolRoot, "targets")
	}
	if err := storageutil.ValidateRoot(storageutil.RootKindBackstore, cfg.BackstoreDir); err != nil {
		return "", err
	}
	return filepath.Clean(cfg.BackstoreDir), nil
}

func ensureBackstoreImage(path string, sizeMB int) error {
	if sizeMB <= 0 {
		sizeMB = 64
	}
	sizeBytes := int64(sizeMB) * 1024 * 1024
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return fmt.Errorf("open backstore image: %w", err)
	}
	defer f.Close()
	if err := f.Truncate(sizeBytes); err != nil {
		return fmt.Errorf("resize backstore image: %w", err)
	}
	return nil
}

func createISCSITargetReplacingExisting(ctx context.Context, run targetcliRunFunc, deleteTarget targetcliDeleteTargetFunc, targetIQN string) error {
	err := run(ctx, "/iscsi", "create", targetIQN)
	if err == nil {
		return nil
	}
	if !isAlreadyExistsTargetcliError(err) {
		return err
	}
	if deleteErr := deleteTarget(ctx, targetIQN); deleteErr != nil {
		return fmt.Errorf("replace existing iscsi target: %w", deleteErr)
	}
	if retryErr := run(ctx, "/iscsi", "create", targetIQN); retryErr != nil {
		return fmt.Errorf("create iscsi target after replacing existing target: %w", retryErr)
	}
	return nil
}

func isIgnorableTargetcliError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "no such target in configfs") ||
		strings.Contains(msg, "no such object in configfs") ||
		strings.Contains(msg, "no storage object named") ||
		strings.Contains(msg, "already exists")
}

func isAlreadyExistsTargetcliError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists")
}
