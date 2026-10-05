package api

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSupportBundleExcludesConfiguredKeyAndHardlinkAliases(t *testing.T) {
	root := t.TempDir()
	keyPath := filepath.Join(root, "vault.key")
	secret := []byte("synthetic-iscsi-vault-key-055-never-export")
	if err := os.WriteFile(keyPath, secret, 0o600); err != nil {
		t.Fatalf("write fake vault key: %v", err)
	}
	t.Setenv("HOLO_ISCSI_SECRET_KEY", keyPath)

	cfg := testSupportConfig(t)
	cfg.ISCSISecretKeyPath = DefaultSupportBundleConfig().ISCSISecretKeyPath
	if cfg.ISCSISecretKeyPath != keyPath {
		t.Fatalf("support config ignored configured key path: got %q want %q", cfg.ISCSISecretKeyPath, keyPath)
	}
	if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "holo.env"), []byte("HOLO_HTTP_ADDR=127.0.0.1:80\n"), 0o600); err != nil {
		t.Fatalf("write allowed config: %v", err)
	}
	hardlink := filepath.Join(cfg.LogDir, "key-copy.log")
	if err := os.Link(keyPath, hardlink); err != nil {
		t.Fatalf("create key hardlink: %v", err)
	}
	if err := os.Symlink(keyPath, filepath.Join(cfg.LogDir, "key-link.log")); err != nil {
		t.Fatalf("create key symlink: %v", err)
	}
	replacement := filepath.Join(cfg.LogDir, "replaced.log")
	if err := os.WriteFile(replacement, []byte("ordinary log\n"), 0o600); err != nil {
		t.Fatalf("write log candidate: %v", err)
	}
	if err := os.Remove(replacement); err != nil {
		t.Fatalf("remove log candidate: %v", err)
	}
	if err := os.Link(keyPath, replacement); err != nil {
		t.Fatalf("replace log candidate with key hardlink: %v", err)
	}

	h := NewOpsHandler(nil, nil, 3260)
	h.support = cfg
	bundle, _, err := h.buildSupportBundle(t.Context())
	if err != nil {
		t.Fatalf("build support bundle: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatalf("open support bundle: %v", err)
	}
	entries := zipEntries(reader)
	if !entries["config/holo.env"] {
		t.Fatal("expected the allowlisted non-secret holo.env file")
	}
	for _, name := range []string{"logs/key-copy.log", "logs/key-link.log", "logs/replaced.log"} {
		if entries[name] {
			t.Errorf("secret key alias was exported as %s", name)
		}
	}
	for _, file := range reader.File {
		opened, err := file.Open()
		if err != nil {
			t.Fatalf("open zip entry %s: %v", file.Name, err)
		}
		content, readErr := io.ReadAll(opened)
		_ = opened.Close()
		if readErr != nil {
			t.Fatalf("read zip entry %s: %v", file.Name, readErr)
		}
		if bytes.Contains(content, secret) {
			t.Fatalf("configured key bytes leaked through %s", file.Name)
		}
	}
}

func TestSupportBundleDoesNotExportTheKeyNamedHoloEnv(t *testing.T) {
	cfg := testSupportConfig(t)
	keyPath := filepath.Join(cfg.ConfigDir, "holo.env")
	secret := []byte("synthetic-key-disguised-as-holo-env")
	if err := os.WriteFile(keyPath, secret, 0o600); err != nil {
		t.Fatalf("write fake key: %v", err)
	}
	cfg.ISCSISecretKeyPath = keyPath
	h := NewOpsHandler(nil, nil, 3260)
	h.support = cfg
	bundle, _, err := h.buildSupportBundle(t.Context())
	if err != nil {
		t.Fatalf("build support bundle: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatalf("open support bundle: %v", err)
	}
	if zipEntries(reader)["config/holo.env"] {
		t.Fatal("configured key file must take precedence over the holo.env allowlist")
	}
}

func TestSupportTailBufferKeepsOnlyTheConfiguredCombinedTail(t *testing.T) {
	tail := newSupportTailBuffer(5)
	if _, err := tail.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := tail.Write([]byte("defghij")); err != nil {
		t.Fatal(err)
	}
	if got := string(tail.Bytes()); got != "fghij" {
		t.Fatalf("unexpected bounded tail %q", got)
	}
	if !tail.Truncated() || tail.Len() > 5 {
		t.Fatalf("expected bounded/truncated state, len=%d truncated=%v", tail.Len(), tail.Truncated())
	}
}

func TestSupportBundleTruncationDropsPartialFirstLine(t *testing.T) {
	cfg := testSupportConfig(t)
	path := filepath.Join(cfg.LogDir, "large.log")
	contents := append(bytes.Repeat([]byte("x"), supportMaxFile+10), []byte("secret-fragment=synthetic-key\ncomplete-tail\n")...)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write large log fixture: %v", err)
	}
	var output bytes.Buffer
	zw := zip.NewWriter(&output)
	addPlainFile(zw, "logs/large.log", path)
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	entry := zipEntryText(t, reader, "logs/large.log")
	if strings.Contains(entry, "secret-fragment") || !strings.Contains(entry, "complete-tail") {
		t.Fatalf("truncated entry must drop the partial first line and retain complete tail: %q", entry[:min(len(entry), 100)])
	}
}

func TestSupportCommandTailIsBoundedAndCancellationIsReported(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "emit")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf '%s\\n' first-line\n/usr/bin/yes 'tail-record' | /usr/bin/head -c 600000\n"), 0o700); err != nil {
		t.Fatalf("write output command: %v", err)
	}
	var output bytes.Buffer
	zw := zip.NewWriter(&output)
	addCommandOutput(t.Context(), zw, supportCommand{entry: "commands/test.txt", name: command}, nil)
	if err := zw.Close(); err != nil {
		t.Fatalf("close command zip: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatalf("open command zip: %v", err)
	}
	entry := zipEntryText(t, reader, "commands/test.txt")
	if !strings.Contains(entry, "truncated") || !strings.Contains(entry, "tail-record") || strings.Contains(entry, "first-line") {
		t.Fatalf("expected truncated command tail with incomplete leading output dropped, got %q", entry[:min(len(entry), 160)])
	}

	traceCommand := filepath.Join(root, "trace")
	if err := os.WriteFile(traceCommand, []byte("#!/bin/sh\n/usr/bin/head -c 16777280 /dev/zero\n"), 0o700); err != nil {
		t.Fatalf("write trace command: %v", err)
	}
	output.Reset()
	zw = zip.NewWriter(&output)
	for _, name := range []string{"commands/journalctl-holo-cdb-trace.txt", "commands/journalctl-holo-cdb-trace-focus.txt"} {
		addCommandOutput(t.Context(), zw, supportCommand{entry: name, name: traceCommand}, nil)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close trace zip: %v", err)
	}
	reader, err = zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatalf("open trace zip: %v", err)
	}
	for _, name := range []string{"commands/journalctl-holo-cdb-trace.txt", "commands/journalctl-holo-cdb-trace-focus.txt"} {
		entry = zipEntryText(t, reader, name)
		if !strings.Contains(entry, "truncated to last 16777216 bytes") || len(entry) > supportMaxTraceCommand+512 {
			t.Fatalf("expected independently bounded trace tail for %s: size=%d", name, len(entry))
		}
	}

	slowCommand := filepath.Join(root, "slow")
	if err := os.WriteFile(slowCommand, []byte("#!/bin/sh\nexec /usr/bin/yes 'continuous-output'\n"), 0o700); err != nil {
		t.Fatalf("write slow command: %v", err)
	}
	cancelCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	output.Reset()
	zw = zip.NewWriter(&output)
	started := time.Now()
	addCommandOutput(cancelCtx, zw, supportCommand{entry: "commands/cancelled.txt", name: slowCommand}, nil)
	if err := zw.Close(); err != nil {
		t.Fatalf("close cancelled command zip: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled command took too long: %s", elapsed)
	}
	reader, err = zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatalf("open cancelled command zip: %v", err)
	}
	entry = zipEntryText(t, reader, "commands/cancelled.txt")
	if !strings.Contains(entry, "cancelled") {
		t.Fatalf("expected a fixed cancellation notice, got %q", entry[:min(len(entry), 160)])
	}
}

func TestSupportFileTailLimitsUnknownReportedSize(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "size-zero-*")
	if err != nil {
		t.Fatalf("create virtual-file fixture: %v", err)
	}
	if _, err := file.Write(bytes.Repeat([]byte("x"), 100)); err != nil {
		t.Fatalf("write virtual-file fixture: %v", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("rewind virtual-file fixture: %v", err)
	}
	data, truncated, partial, err := readSupportFileTail(file, 0, 16)
	if err != nil {
		t.Fatalf("read size-zero virtual file: %v", err)
	}
	if !truncated || partial || len(data) != 16 {
		t.Fatalf("expected a bounded size-zero read, got bytes=%d truncated=%v partial=%v", len(data), truncated, partial)
	}
}

func TestSupportCommandTimeoutIsReportedWithFixedReason(t *testing.T) {
	previousTimeout := supportCommandTimeout
	supportCommandTimeout = 50 * time.Millisecond
	defer func() { supportCommandTimeout = previousTimeout }()

	command := filepath.Join(t.TempDir(), "slow-command")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nexec /usr/bin/sleep 5\n"), 0o700); err != nil {
		t.Fatalf("write slow command: %v", err)
	}
	var output bytes.Buffer
	zw := zip.NewWriter(&output)
	started := time.Now()
	addCommandOutput(t.Context(), zw, supportCommand{entry: "commands/timeout.txt", name: command}, nil)
	if err := zw.Close(); err != nil {
		t.Fatalf("close timeout zip: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timed-out command was not stopped promptly: %s", elapsed)
	}
	reader, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatalf("open timeout zip: %v", err)
	}
	entry := zipEntryText(t, reader, "commands/timeout.txt")
	if !strings.Contains(entry, "cancelled reason=command_timeout") {
		t.Fatalf("expected fixed timeout status, got %q", entry)
	}
}
