package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type failingResponseWriter struct {
	header http.Header
}

func (w *failingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingResponseWriter) WriteHeader(int) {}

func (w *failingResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("client disconnected")
}

func TestOpsHandler_SystemOverviewEndpoint(t *testing.T) {
	h := NewOpsHandler(nil, nil, 3260)
	req := httptest.NewRequest(http.MethodGet, "/v1/system/overview", nil)
	resp := httptest.NewRecorder()

	h.handleSystemOverview(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestOpsHandler_SystemOverviewMethodNotAllowed(t *testing.T) {
	h := NewOpsHandler(nil, nil, 3260)
	req := httptest.NewRequest(http.MethodPost, "/v1/system/overview", nil)
	resp := httptest.NewRecorder()

	h.handleSystemOverview(resp, req)
	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", resp.Code)
	}
}

func TestOpsHandler_CDBTraceEndpointTogglesRuntimeFlag(t *testing.T) {
	h := NewOpsHandler(nil, nil, 3260)
	h.cdbTrace = CDBTraceConfig{StateFile: filepath.Join(t.TempDir(), "cdb-trace.enabled")}

	getReq := httptest.NewRequest(http.MethodGet, "/v1/ops/cdb-trace", nil)
	getResp := httptest.NewRecorder()
	h.handleCDBTrace(getResp, getReq)
	if getResp.Code != http.StatusOK {
		t.Fatalf("expected initial 200, got %d body=%s", getResp.Code, getResp.Body.String())
	}
	var initial cdbTraceStatus
	if err := json.Unmarshal(getResp.Body.Bytes(), &initial); err != nil {
		t.Fatalf("decode initial trace status: %v", err)
	}
	if initial.Enabled {
		t.Fatalf("expected cdb trace disabled by default")
	}

	postReq := httptest.NewRequest(http.MethodPost, "/v1/ops/cdb-trace", strings.NewReader(`{"enabled":true}`))
	postReq.Header.Set("Content-Type", "application/json")
	postResp := httptest.NewRecorder()
	h.handleCDBTrace(postResp, postReq)
	if postResp.Code != http.StatusOK {
		t.Fatalf("expected post 200, got %d body=%s", postResp.Code, postResp.Body.String())
	}
	var enabled cdbTraceStatus
	if err := json.Unmarshal(postResp.Body.Bytes(), &enabled); err != nil {
		t.Fatalf("decode enabled trace status: %v", err)
	}
	if !enabled.Enabled {
		t.Fatalf("expected cdb trace enabled")
	}
	raw, err := os.ReadFile(h.cdbTrace.StateFile)
	if err != nil {
		t.Fatalf("read trace state file: %v", err)
	}
	if string(raw) != "1\n" {
		t.Fatalf("unexpected trace state file contents %q", raw)
	}
}

func TestOpsHandler_SupportBundleEndpoint(t *testing.T) {
	h := NewOpsHandler(nil, nil, 3260)
	h.support = testSupportConfig(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/support/bundle", nil)
	resp := httptest.NewRecorder()

	h.handleSupportBundle(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Type"); got != "application/zip" {
		t.Fatalf("expected zip content type, got %q", got)
	}

	reader, err := zip.NewReader(bytes.NewReader(resp.Body.Bytes()), int64(resp.Body.Len()))
	if err != nil {
		t.Fatalf("open support bundle zip: %v", err)
	}
	entries := zipEntries(reader)
	if !entries["manifest.json"] || !entries["api/system-overview.json"] || !entries["env/holo-env.txt"] {
		t.Fatalf("support bundle missing required entries: %+v", entries)
	}
}

func TestOpsHandler_SupportBundleLogsWriteFailure(t *testing.T) {
	h := NewOpsHandler(nil, nil, 3260)
	h.support = testSupportConfig(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/support/bundle", nil)

	var logs bytes.Buffer
	original := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(original) })

	h.handleSupportBundle(&failingResponseWriter{}, req)

	if got := logs.String(); !strings.Contains(got, "support bundle response write failed") {
		t.Fatalf("expected support bundle write failure log, got %q", got)
	}
}

func TestOpsHandler_SupportBundleRedactsConfigSecrets(t *testing.T) {
	t.Setenv("HOLO_API_KEY", "secret-value")
	h := NewOpsHandler(nil, nil, 3260)
	h.support = testSupportConfig(t)
	if err := os.WriteFile(filepath.Join(h.support.ConfigDir, "holo.env"), []byte("HOLO_API_KEY=secret-value\nHOLO_HTTP_ADDR=0.0.0.0:80\n"), 0o600); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}

	bundle, _, err := h.buildSupportBundle(t.Context())
	if err != nil {
		t.Fatalf("build support bundle: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatalf("open support bundle zip: %v", err)
	}
	config := zipEntryText(t, reader, "config/holo.env")
	if config != "HOLO_API_KEY=[REDACTED]\nHOLO_HTTP_ADDR=0.0.0.0:80\n" {
		t.Fatalf("unexpected redacted config: %q", config)
	}
}

func TestOpsHandler_SupportBundleSkipsSymlinkedFiles(t *testing.T) {
	h := NewOpsHandler(nil, nil, 3260)
	h.support = testSupportConfig(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("outside-secret\n"), 0o600); err != nil {
		t.Fatalf("write outside fixture: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(h.support.ConfigDir, "linked.env")); err != nil {
		t.Fatalf("create config symlink: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(h.support.LogDir, "linked.log")); err != nil {
		t.Fatalf("create log symlink: %v", err)
	}

	bundle, _, err := h.buildSupportBundle(t.Context())
	if err != nil {
		t.Fatalf("build support bundle: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatalf("open support bundle zip: %v", err)
	}
	entries := zipEntries(reader)
	if entries["config/linked.env"] || entries["logs/linked.log"] {
		t.Fatalf("support bundle should skip symlinked files, got entries=%+v", entries)
	}
}

func TestOpsHandler_SupportBundleSkipsSymlinkedRoots(t *testing.T) {
	h := NewOpsHandler(nil, nil, 3260)
	h.support = testSupportConfig(t)
	outsideConfig := filepath.Join(t.TempDir(), "outside-config")
	outsideLog := filepath.Join(t.TempDir(), "outside-log")
	for _, dir := range []string{outsideConfig, outsideLog} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create outside dir: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(outsideConfig, "holo.env"), []byte("HOLO_API_KEY=outside-secret\n"), 0o600); err != nil {
		t.Fatalf("write outside config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outsideLog, "holo.log"), []byte("outside-log\n"), 0o600); err != nil {
		t.Fatalf("write outside log: %v", err)
	}
	if err := os.Remove(h.support.ConfigDir); err != nil {
		t.Fatalf("remove config dir: %v", err)
	}
	if err := os.Remove(h.support.LogDir); err != nil {
		t.Fatalf("remove log dir: %v", err)
	}
	if err := os.Symlink(outsideConfig, h.support.ConfigDir); err != nil {
		t.Fatalf("symlink config root: %v", err)
	}
	if err := os.Symlink(outsideLog, h.support.LogDir); err != nil {
		t.Fatalf("symlink log root: %v", err)
	}

	bundle, _, err := h.buildSupportBundle(t.Context())
	if err != nil {
		t.Fatalf("build support bundle: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatalf("open support bundle zip: %v", err)
	}
	entries := zipEntries(reader)
	if entries["config/holo.env"] || entries["logs/holo.log"] {
		t.Fatalf("support bundle should skip symlinked roots, got entries=%+v", entries)
	}
	if got := zipEntryText(t, reader, "config/README.txt"); !strings.Contains(got, "unavailable") {
		t.Fatalf("expected config unavailable note, got %q", got)
	}
	if got := zipEntryText(t, reader, "logs/README.txt"); !strings.Contains(got, "unavailable") {
		t.Fatalf("expected logs unavailable note, got %q", got)
	}
}

func TestAddPlainFileSkipsReadTimeSymlink(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("outside-secret\n"), 0o600); err != nil {
		t.Fatalf("write outside fixture: %v", err)
	}
	link := filepath.Join(t.TempDir(), "candidate.log")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	addPlainFile(zw, "logs/candidate.log", link)
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	if entries := zipEntries(reader); entries["logs/candidate.log"] {
		t.Fatalf("support bundle should skip read-time symlink, got entries=%+v", entries)
	}
}

func TestSupportBundleRedactsEnvironmentLines(t *testing.T) {
	input := "Environment=\"HOLO_API_KEY=secret-value\" HOLO_HTTP_ADDR=0.0.0.0:80\nHOLO_API_KEY=secret-value\nclient_secret=client-value\nprivate_key=pem-value\nAuthorization: Bearer bearer-value\njwt=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.signature\n"
	got := redactText(input)
	for _, leaked := range []string{"secret-value", "client-value", "pem-value", "bearer-value", "eyJhbGci"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("redacted text leaked secret %q: %q", leaked, got)
		}
	}
	if !strings.Contains(got, "Environment=[REDACTED]") {
		t.Fatalf("expected systemd Environment line to be redacted, got %q", got)
	}
	if !strings.Contains(got, "HOLO_API_KEY=[REDACTED]") {
		t.Fatalf("expected explicit HOLO_API_KEY assignment to be redacted, got %q", got)
	}
}

func TestLimitedBufferRejectsOversizedSupportBundle(t *testing.T) {
	buf := newLimitedBuffer(4)
	if _, err := buf.Write([]byte("1234")); err != nil {
		t.Fatalf("initial write should fit: %v", err)
	}
	if _, err := buf.Write([]byte("5")); !errors.Is(err, errSupportBundleTooLarge) {
		t.Fatalf("expected size limit error, got %v", err)
	}
}

func testSupportConfig(t *testing.T) SupportBundleConfig {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, "etc")
	dataDir := filepath.Join(root, "data")
	logDir := filepath.Join(root, "log")
	runDir := filepath.Join(root, "run")
	for _, dir := range []string{configDir, dataDir, logDir, runDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create support test dir: %v", err)
		}
	}
	return SupportBundleConfig{
		ConfigDir:       configDir,
		DataDir:         dataDir,
		LogDir:          logDir,
		RunDir:          runDir,
		IncludeCommands: false,
	}
}

func zipEntries(reader *zip.Reader) map[string]bool {
	entries := make(map[string]bool, len(reader.File))
	for _, file := range reader.File {
		entries[file.Name] = true
	}
	return entries
}

func zipEntryText(t *testing.T, reader *zip.Reader, name string) string {
	t.Helper()
	for _, file := range reader.File {
		if file.Name != name {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("open zip entry %s: %v", name, err)
		}
		defer func() { _ = rc.Close() }()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read zip entry %s: %v", name, err)
		}
		return string(b)
	}
	t.Fatalf("zip entry %s not found", name)
	return ""
}
