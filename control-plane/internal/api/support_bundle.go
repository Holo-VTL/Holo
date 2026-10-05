package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
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
	"unicode/utf8"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/config"
)

const (
	supportMaxCommand      = 512 * 1024
	supportMaxTraceCommand = 16 * 1024 * 1024
	supportMaxFile         = 2 * 1024 * 1024
	supportMaxBundle       = 64 * 1024 * 1024
	supportMaxWalkDepth    = 8
)

var supportCommandTimeout = 8 * time.Second

var errSupportBundleTooLarge = errors.New("support bundle size limit exceeded")

type SupportBundleConfig struct {
	ConfigDir          string `json:"configDir"`
	DataDir            string `json:"dataDir"`
	LogDir             string `json:"logDir"`
	RunDir             string `json:"runDir"`
	IncludeCommands    bool   `json:"includeCommands"`
	ISCSISecretKeyPath string `json:"-"`
}

type supportCommand struct {
	entry string
	name  string
	args  []string
}

type supportManifest struct {
	GeneratedAt time.Time           `json:"generatedAt"`
	Hostname    string              `json:"hostname"`
	Paths       SupportBundleConfig `json:"paths"`
	Notes       []string            `json:"notes"`
	Notices     []supportNotice     `json:"notices,omitempty"`
}

type supportNotice struct {
	Entry  string `json:"entry"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

func recordSupportNotice(notices *[]supportNotice, entry, status, reason string) {
	if notices == nil || len(*notices) >= 512 {
		return
	}
	*notices = append(*notices, supportNotice{Entry: entry, Status: status, Reason: reason})
}

func DefaultSupportBundleConfig() SupportBundleConfig {
	return SupportBundleConfig{
		ConfigDir:          envOr("HOLO_CONFIG_DIR", "/etc/holo"),
		DataDir:            envOr("HOLO_DATA_DIR", "/var/lib/holo"),
		LogDir:             envOr("HOLO_LOG_DIR", "/var/log/holo"),
		RunDir:             envOr("HOLO_RUN_DIR", "/run/holo"),
		IncludeCommands:    true,
		ISCSISecretKeyPath: envOr("HOLO_ISCSI_SECRET_KEY", "/etc/holo/iscsi-secrets.key"),
	}
}

func (h *OpsHandler) handleSupportBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}

	bundle, filename, err := h.buildSupportBundle(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "support bundle unavailable", err)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(bundle)))
	if _, err := w.Write(bundle); err != nil {
		log.Printf("support bundle response write failed: %v", err)
	}
}

func (h *OpsHandler) buildSupportBundle(ctx context.Context) ([]byte, string, error) {
	secretKey := identifySupportSecret(h.support.ISCSISecretKeyPath)
	buf := newLimitedBuffer(supportMaxBundle)
	zw := zip.NewWriter(buf)
	generatedAt := time.Now().UTC()
	hostname, _ := os.Hostname()

	manifest := supportManifest{
		GeneratedAt: generatedAt,
		Hostname:    strings.TrimSpace(hostname),
		Paths:       h.support,
		Notes: []string{
			"Sensitive environment values are redacted.",
			"Cartridge payload data and metadata database files are intentionally not included.",
			"Command failures are captured inside their corresponding files.",
			"The 64 MiB ZIP limit bounds archive size, not total process memory.",
			"See notices for included, skipped, truncated, failed, or cancelled sources.",
		},
	}
	h.addAPISnapshots(ctx, zw)
	addText(zw, "env/holo-env.txt", collectHoloEnv())
	addJSON(zw, "api/cdb-trace.json", h.cdbTraceSnapshot())
	var notices []supportNotice
	h.addFileSnapshots(zw, secretKey, &notices)
	if h.support.IncludeCommands {
		h.addCommandSnapshots(ctx, zw, &notices)
	}
	manifest.Notices = notices
	if err := addJSON(zw, "manifest.json", manifest); err != nil {
		return nil, "", err
	}

	if err := zw.Close(); err != nil {
		return nil, "", err
	}

	safeHost := sanitizeFilename(hostname)
	if safeHost == "" {
		safeHost = "host"
	}
	filename := fmt.Sprintf("holo-support-%s-%s.zip", safeHost, generatedAt.Format("20060102T150405Z"))
	return buf.Bytes(), filename, nil
}

func (h *OpsHandler) addAPISnapshots(ctx context.Context, zw *zip.Writer) {
	if h.health != nil {
		addJSON(zw, "api/health.json", h.health.Summary())
	} else {
		addJSON(zw, "api/health.json", map[string]string{"status": "unavailable"})
	}
	addJSON(zw, "api/system-overview.json", h.systemOverviewSnapshot())
	if h.query != nil {
		result, err := h.query.Query(ctx, audit.QueryParams{Limit: 500})
		if err != nil {
			addText(zw, "api/audit-events.error.txt", "audit query failed\n")
		} else {
			addJSON(zw, "api/audit-events.json", result)
		}
	}
	addText(zw, "api/metrics.prom", PrometheusText(h.metrics))
}

func (h *OpsHandler) systemOverviewSnapshot() systemOverview {
	host, _ := os.Hostname()
	load1, load5, load15 := readLoadAverage()
	memTotal, memAvailable := readMemoryInfo()
	rxBytes, txBytes := readNetworkTotals()
	return systemOverview{
		Version:              config.GetVersionInfo(),
		Hostname:             strings.TrimSpace(host),
		UptimeSeconds:        readUptimeSeconds(),
		CPULoad1m:            load1,
		CPULoad5m:            load5,
		CPULoad15m:           load15,
		MemoryTotalBytes:     memTotal,
		MemoryAvailableBytes: memAvailable,
		NetworkRxBytes:       rxBytes,
		NetworkTxBytes:       txBytes,
		ISCSISessionCount:    readISCSISessionCount(h.portalPort),
		CollectedAt:          time.Now().UTC(),
	}
}

func (h *OpsHandler) addFileSnapshots(zw *zip.Writer, secretKey supportFileIdentity, notices *[]supportNotice) {
	for _, file := range []struct{ entry, path string }{
		{"files/etc-os-release.txt", "/etc/os-release"},
		{"files/proc-cmdline.txt", "/proc/cmdline"},
		{"files/proc-loadavg.txt", "/proc/loadavg"},
		{"files/proc-meminfo.txt", "/proc/meminfo"},
		{"files/proc-modules.txt", "/proc/modules"},
		{"files/proc-mounts.txt", "/proc/mounts"},
		{"files/proc-net-dev.txt", "/proc/net/dev"},
		{"files/proc-scsi-scsi.txt", "/proc/scsi/scsi"},
	} {
		addPlainFileWithIdentity(zw, file.entry, file.path, secretKey, supportMaxFile, notices)
	}
	addRedactedConfigFiles(zw, h.support.ConfigDir, secretKey, notices)
	addLogFiles(zw, h.support.LogDir, secretKey, notices)
}

func (h *OpsHandler) addCommandSnapshots(ctx context.Context, zw *zip.Writer, notices *[]supportNotice) {
	priv := newSupportPrivilegeRouter()
	commands := []supportCommand{
		{"commands/uname.txt", "uname", []string{"-a"}},
		{"commands/hostnamectl.txt", "hostnamectl", nil},
		{"commands/timedatectl.txt", "timedatectl", nil},
		{"commands/systemctl-holo-control-plane-status.txt", "systemctl", []string{"status", "holo-control-plane", "--no-pager", "-l"}},
		{"commands/systemctl-holo-control-plane-cat.txt", "systemctl", []string{"cat", "holo-control-plane"}},
		{"commands/systemctl-tcmu-runner-status.txt", "systemctl", []string{"status", "tcmu-runner", "--no-pager", "-l"}},
		priv.command("commands/journalctl-holo-control-plane.txt", "journalctl-unit",
			[]string{"holo-control-plane", "800"},
			"journalctl", []string{"-u", "holo-control-plane", "-n", "800", "--no-pager", "-o", "short-iso"}),
		priv.command("commands/journalctl-holo-cdb-trace.txt", "journalctl-cdb-trace",
			[]string{"holo-control-plane", "20000"},
			"sh", []string{"-c", "journalctl -u holo-control-plane -n 20000 --no-pager -o short-iso | grep '\\[cdb_trace\\]' || true"}),
		priv.command("commands/journalctl-holo-cdb-trace-focus.txt", "journalctl-cdb-trace-focus",
			[]string{"holo-control-plane", "100000"},
			"sh", []string{"-c", "journalctl -u holo-control-plane -n 100000 --no-pager -o short-iso | grep '\\[cdb_trace\\]' | grep -v 'opcode=0x0A' || true"}),
		priv.command("commands/journalctl-tcmu-runner.txt", "journalctl-unit",
			[]string{"tcmu-runner", "800"},
			"journalctl", []string{"-u", "tcmu-runner", "-n", "800", "--no-pager", "-o", "short-iso"}),
		priv.command("commands/journalctl-kernel.txt", "journalctl-kernel",
			[]string{"500"},
			"journalctl", []string{"-k", "-n", "500", "--no-pager", "-o", "short-iso"}),
		priv.command("commands/dmesg-warnings.txt", "dmesg",
			nil,
			"dmesg", []string{"-T", "--level=err,warn"}),
		priv.command("commands/targetcli-ls.txt", "targetcli-ls",
			nil,
			"targetcli", []string{"ls"}),
		priv.command("commands/targetcli-backstores-ls.txt", "targetcli-backstores",
			nil,
			"targetcli", []string{"/backstores", "ls"}),
		priv.command("commands/targetcli-iscsi-ls.txt", "targetcli-iscsi",
			nil,
			"targetcli", []string{"/iscsi", "ls"}),
		priv.command("commands/iscsiadm-sessions.txt", "iscsiadm-sessions",
			nil,
			"iscsiadm", []string{"-m", "session"}),
		priv.command("commands/iscsiadm-nodes.txt", "iscsiadm-nodes",
			nil,
			"iscsiadm", []string{"-m", "node"}),
		{"commands/lsscsi-g.txt", "lsscsi", []string{"-g"}},
		priv.command("commands/sg-map-i.txt", "sg-map-i",
			nil,
			"sg_map", []string{"-i"}),
		{"commands/lsblk-json.txt", "lsblk", []string{"-O", "-J"}},
		{"commands/findmnt.txt", "findmnt", []string{"-R", "-o", "TARGET,SOURCE,FSTYPE,OPTIONS"}},
		{"commands/df-hT.txt", "df", []string{"-hT"}},
		{"commands/lsmod.txt", "lsmod", nil},
		{"commands/ip-addr.txt", "ip", []string{"addr"}},
		{"commands/ip-route.txt", "ip", []string{"route"}},
		{"commands/ss-listeners.txt", "ss", []string{"-lntup"}},
		{"commands/ps-holo.txt", "pgrep", []string{"-af", "holo|tcmu|targetcli|iscsi"}},
	}
	for _, command := range commands {
		addCommandOutput(ctx, zw, command, notices)
	}

	addCommandOutput(ctx, zw, supportCommand{
		entry: "commands/config-dir-listing.txt",
		name:  priv.commandName("find-config", "find"),
		args:  priv.commandArgs("find-config", []string{h.support.ConfigDir}, []string{h.support.ConfigDir, "-maxdepth", "3", "-ls"}),
	}, notices)
	addCommandOutput(ctx, zw, supportCommand{
		entry: "commands/log-dir-listing.txt",
		name:  priv.commandName("find-log", "find"),
		args:  priv.commandArgs("find-log", []string{h.support.LogDir}, []string{h.support.LogDir, "-maxdepth", "3", "-ls"}),
	}, notices)
	addCommandOutput(ctx, zw, supportCommand{
		entry: "commands/data-dir-shallow-listing.txt",
		name:  priv.commandName("find-data", "find"),
		args:  priv.commandArgs("find-data", []string{h.support.DataDir}, []string{h.support.DataDir, "-maxdepth", "4", "-type", "f", "-printf", "%p %s bytes %TY-%Tm-%Td %TH:%TM:%TS\n"}),
	}, notices)
	addCommandOutput(ctx, zw, supportCommand{
		entry: "commands/run-dir-listing.txt",
		name:  priv.commandName("find-run", "find"),
		args:  priv.commandArgs("find-run", []string{h.support.RunDir}, []string{h.support.RunDir, "-maxdepth", "3", "-ls"}),
	}, notices)
}

// supportPrivilegeRouter routes commands that need root through the
// holo-support-helper sudo wrapper when configured. Falls back to a direct
// invocation otherwise (matching legacy behaviour, which surfaces "permission
// denied" inside the bundle entry rather than failing the whole export).
type supportPrivilegeRouter struct {
	helper string
	enable bool
}

func newSupportPrivilegeRouter() supportPrivilegeRouter {
	helper := strings.TrimSpace(os.Getenv("HOLO_SUPPORT_PRIVILEGED_HELPER"))
	useSudo := envBoolDefault("HOLO_TARGET_RUNTIME_USE_SUDO", true)
	enable := helper != "" && useSudo
	return supportPrivilegeRouter{helper: helper, enable: enable}
}

func (r supportPrivilegeRouter) command(entry, sub string, helperArgs []string, fallbackName string, fallbackArgs []string) supportCommand {
	return supportCommand{entry: entry, name: r.commandName(sub, fallbackName), args: r.commandArgs(sub, helperArgs, fallbackArgs)}
}

func (r supportPrivilegeRouter) commandName(_ string, fallbackName string) string {
	if !r.enable {
		return fallbackName
	}
	return "sudo"
}

func (r supportPrivilegeRouter) commandArgs(sub string, helperArgs []string, fallbackArgs []string) []string {
	if !r.enable {
		return fallbackArgs
	}
	args := make([]string, 0, 3+len(helperArgs))
	args = append(args, "-n", r.helper, sub)
	args = append(args, helperArgs...)
	return args
}

func envBoolDefault(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func addJSON(zw *zip.Writer, name string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return addText(zw, name, string(b)+"\n")
}

func addText(zw *zip.Writer, name, text string) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, redactText(text))
	return err
}

func addPlainFile(zw *zip.Writer, entry, path string) {
	addPlainFileWithIdentity(zw, entry, path, supportFileIdentity{}, supportMaxFile, nil)
}

type supportFileIdentity struct {
	path  string
	dev   uint64
	ino   uint64
	valid bool
}

func identifySupportSecret(path string) supportFileIdentity {
	identity := supportFileIdentity{path: supportPathKey(path)}
	if identity.path == "" {
		return identity
	}
	file, err := os.OpenFile(identity.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return identity
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return identity
	}
	if dev, ino, ok := supportDeviceInode(info); ok {
		identity.dev, identity.ino, identity.valid = dev, ino, true
	}
	return identity
}

func supportPathKey(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	return filepath.Clean(abs)
}

func supportDeviceInode(info os.FileInfo) (uint64, uint64, bool) {
	statInfo, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(statInfo.Dev), uint64(statInfo.Ino), true
}

func supportFileIsSecret(path string, info os.FileInfo, secret supportFileIdentity) bool {
	if secret.path != "" && supportPathKey(path) == secret.path {
		return true
	}
	if info == nil || !secret.valid {
		return false
	}
	dev, ino, ok := supportDeviceInode(info)
	return ok && dev == secret.dev && ino == secret.ino
}

func addPlainFileWithIdentity(zw *zip.Writer, entry, path string, secret supportFileIdentity, limit int64, notices *[]supportNotice) {
	if supportFileIsSecret(path, nil, secret) {
		recordSupportNotice(notices, entry, "skipped", "secret_file")
		return
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		addText(zw, entry+".error.txt", "read failed\n")
		recordSupportNotice(notices, entry, "failed", "unavailable")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		addText(zw, entry+".error.txt", "non-regular file skipped\n")
		recordSupportNotice(notices, entry, "skipped", "non_regular_file")
		return
	}
	statInfo, ok := info.Sys().(*syscall.Stat_t)
	if !ok || statInfo.Nlink != 1 {
		addText(zw, entry+".notice.txt", "skipped: linked file\n")
		recordSupportNotice(notices, entry, "skipped", "linked_file")
		return
	}
	if supportFileIsSecret(path, info, secret) {
		recordSupportNotice(notices, entry, "skipped", "secret_file")
		return
	}
	if limit <= 0 {
		addText(zw, entry+".notice.txt", "skipped: invalid read budget\n")
		recordSupportNotice(notices, entry, "skipped", "invalid_read_budget")
		return
	}
	contents, truncated, partialLine, err := readSupportFileTail(file, info.Size(), limit)
	if err != nil {
		addText(zw, entry+".error.txt", "read failed\n")
		recordSupportNotice(notices, entry, "failed", "read_error")
		return
	}
	if !utf8.Valid(contents) {
		addText(zw, entry+".notice.txt", "skipped: binary file\n")
		recordSupportNotice(notices, entry, "skipped", "binary_file")
		return
	}
	if truncated {
		marker := fmt.Sprintf("[truncated to %d bytes; earlier content omitted]\n", limit)
		if partialLine {
			contents = dropPartialFirstLine(contents)
			marker = fmt.Sprintf("[truncated to %d bytes; partial first line omitted]\n", limit)
		}
		contents = append([]byte(marker), contents...)
		recordSupportNotice(notices, entry, "truncated", "read_budget")
	} else {
		recordSupportNotice(notices, entry, "included", "")
	}
	addText(zw, entry, string(contents))
}

func readSupportFileTail(file *os.File, size, limit int64) ([]byte, bool, bool, error) {
	if size > limit {
		offset := size - limit
		partialLine := false
		if offset > 0 {
			if _, err := file.Seek(offset-1, io.SeekStart); err != nil {
				return nil, false, false, err
			}
			var preceding [1]byte
			if _, err := io.ReadFull(file, preceding[:]); err != nil {
				return nil, false, false, err
			}
			partialLine = preceding[0] != '\n'
		}
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return nil, false, false, err
		}
		data, err := io.ReadAll(io.LimitReader(file, limit))
		return data, true, partialLine, err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, false, false, err
	}
	if int64(len(data)) > limit {
		return data[:limit], true, false, nil
	}
	return data, false, false, nil
}

func dropPartialFirstLine(data []byte) []byte {
	newline := bytes.IndexByte(data, '\n')
	if newline < 0 {
		return nil
	}
	return data[newline+1:]
}

func addRedactedConfigFiles(zw *zip.Writer, configDir string, secret supportFileIdentity, notices *[]supportNotice) {
	info, err := os.Lstat(configDir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		addText(zw, "config/README.txt", "config directory is unavailable\n")
		recordSupportNotice(notices, "config", "skipped", "directory_unavailable")
		return
	}
	addPlainFileWithIdentity(zw, "config/holo.env", filepath.Join(configDir, "holo.env"), secret, supportMaxFile, notices)
}

func addLogFiles(zw *zip.Writer, logDir string, secret supportFileIdentity, notices *[]supportNotice) {
	info, err := os.Lstat(logDir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		addText(zw, "logs/README.txt", "log directory is unavailable\n")
		recordSupportNotice(notices, "logs", "skipped", "directory_unavailable")
		return
	}
	count := 0
	_ = filepath.WalkDir(logDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if count >= 128 {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if walkDepth(logDir, path) > supportMaxWalkDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, relErr := filepath.Rel(logDir, path)
		if relErr != nil || strings.HasPrefix(rel, "..") {
			return nil
		}
		addPlainFileWithIdentity(zw, filepath.ToSlash(filepath.Join("logs", rel)), path, secret, supportMaxFile, notices)
		count++
		return nil
	})
}

type supportTailBuffer struct {
	mu        sync.Mutex
	limit     int
	data      []byte
	truncated bool
}

func newSupportTailBuffer(limit int) *supportTailBuffer {
	if limit < 0 {
		limit = 0
	}
	return &supportTailBuffer{limit: limit, data: make([]byte, 0, limit)}
}

func (b *supportTailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit == 0 {
		if len(p) > 0 {
			b.truncated = true
		}
		return len(p), nil
	}
	if len(p) >= b.limit {
		if len(p) > b.limit || len(b.data) > 0 {
			b.truncated = true
		}
		b.data = append(b.data[:0], p[len(p)-b.limit:]...)
		return len(p), nil
	}
	if overflow := len(b.data) + len(p) - b.limit; overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *supportTailBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.data...)
}

func (b *supportTailBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.data)
}

func (b *supportTailBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

func addCommandOutput(ctx context.Context, zw *zip.Writer, command supportCommand, notices *[]supportNotice) {
	path, err := exec.LookPath(command.name)
	if err != nil {
		addText(zw, command.entry, fmt.Sprintf("$ %s %s\n\ncommand not found\n", command.name, strings.Join(command.args, " ")))
		recordSupportNotice(notices, command.entry, "failed", "command_not_found")
		return
	}
	cmdCtx, cancel := context.WithTimeout(ctx, supportCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, path, command.args...)
	maxBytes := supportMaxCommand
	if strings.HasPrefix(command.entry, "commands/journalctl-holo-cdb-trace") {
		maxBytes = supportMaxTraceCommand
	}
	tail := newSupportTailBuffer(maxBytes)
	cmd.Stdout = tail
	cmd.Stderr = tail
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	runErr := cmd.Run()
	output := tail.Bytes()
	truncated := tail.Truncated()
	if truncated {
		output = dropPartialFirstLine(output)
	}
	var body strings.Builder
	body.WriteString("$ ")
	body.WriteString(command.name)
	if len(command.args) > 0 {
		body.WriteByte(' ')
		body.WriteString(strings.Join(command.args, " "))
	}
	body.WriteString("\n\n")
	if truncated {
		body.WriteString(fmt.Sprintf("[truncated to last %d bytes; partial first line omitted]\n", maxBytes))
	}
	body.Write(output)
	status := "included"
	reason := ""
	if runErr != nil {
		if cmdCtx.Err() != nil {
			status = "cancelled"
			switch {
			case errors.Is(ctx.Err(), context.Canceled):
				reason = "request_cancelled"
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				reason = "request_deadline"
			default:
				reason = "command_timeout"
			}
		} else {
			status = "failed"
			reason = "command_failed"
		}
		body.WriteString("\n[status] ")
		body.WriteString(status)
		body.WriteString(" reason=")
		body.WriteString(reason)
		body.WriteByte('\n')
	} else if truncated {
		status = "truncated"
		reason = "output_budget"
	}
	addText(zw, command.entry, body.String())
	recordSupportNotice(notices, command.entry, status, reason)
}

func collectHoloEnv() string {
	var lines []string
	for _, pair := range os.Environ() {
		if strings.HasPrefix(pair, "HOLO_") {
			lines = append(lines, pair)
		}
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return "no HOLO_* environment variables visible to control-plane\n"
	}
	return strings.Join(lines, "\n") + "\n"
}

func redactText(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		line = redactExplicitSecrets(line)
		if secretLikeLine(line) {
			lines[i] = redactSecretLine(line)
		} else {
			lines[i] = line
		}
	}
	return strings.Join(lines, "\n")
}

func secretLikeLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	lower := strings.ToLower(line)
	return strings.HasPrefix(trimmed, "Environment=") ||
		strings.Contains(lower, "api_key") ||
		strings.Contains(lower, "access_key") ||
		strings.Contains(lower, "authorization") ||
		strings.Contains(lower, "bearer ") ||
		strings.Contains(lower, "client_secret") ||
		strings.Contains(lower, "password") ||
		strings.Contains(lower, "private_key") ||
		strings.Contains(lower, "secret") ||
		strings.Contains(lower, "token") ||
		jwtLikeRE.MatchString(line)
}

var assignmentRE = regexp.MustCompile(`^([^:=]+[:=]).*$`)
var holoAPIKeyRE = regexp.MustCompile(`(?i)(HOLO_API_KEY\s*=\s*)("[^"]*"|'[^']*'|[^\s]+)`)
var jwtLikeRE = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

func redactExplicitSecrets(line string) string {
	return holoAPIKeyRE.ReplaceAllString(line, "${1}[REDACTED]")
}

func redactSecretLine(line string) string {
	if assignmentRE.MatchString(line) {
		return assignmentRE.ReplaceAllString(line, "$1[REDACTED]")
	}
	return "[REDACTED]"
}

func sanitizeFilename(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else if r == '.' {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-_")
}

func walkDepth(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return len(strings.Split(filepath.Clean(rel), string(os.PathSeparator)))
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func newLimitedBuffer(max int) *limitedBuffer {
	return &limitedBuffer{max: max}
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.max {
		return 0, errSupportBundleTooLarge
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Bytes() []byte {
	return b.buf.Bytes()
}
