package api

import (
	"bufio"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/config"
	"github.com/Holo-VTL/Holo/control-plane/internal/metrics"
	"github.com/Holo-VTL/Holo/control-plane/internal/orchestration"
)

type OpsHandler struct {
	health     *orchestration.HealthService
	query      *audit.QueryService
	portalPort int
	metrics    *metrics.MetricsRegistry
	support    SupportBundleConfig
	cdbTrace   CDBTraceConfig
}

type CDBTraceConfig struct {
	StateFile string `json:"stateFile"`
}

type cdbTraceStatus struct {
	Enabled   bool   `json:"enabled"`
	StateFile string `json:"stateFile"`
}

type cdbTraceUpdateRequest struct {
	Enabled bool `json:"enabled"`
}

type systemOverview struct {
	Version             config.VersionInfo `json:"version"`
	Hostname            string             `json:"hostname"`
	UptimeSeconds       int64              `json:"uptimeSeconds"`
	CPULoad1m           float64            `json:"cpuLoad1m"`
	CPULoad5m           float64            `json:"cpuLoad5m"`
	CPULoad15m          float64            `json:"cpuLoad15m"`
	MemoryTotalBytes    uint64             `json:"memoryTotalBytes"`
	MemoryAvailableBytes uint64             `json:"memoryAvailableBytes"`
	NetworkRxBytes      uint64             `json:"networkRxBytes"`
	NetworkTxBytes      uint64             `json:"networkTxBytes"`
	ISCSISessionCount   int                `json:"iscsiSessionCount"`
	CollectedAt         time.Time          `json:"collectedAt"`
}

func NewOpsHandler(health *orchestration.HealthService, query *audit.QueryService, portalPort int, registry ...*metrics.MetricsRegistry) *OpsHandler {
	if portalPort <= 0 {
		portalPort = 3260
	}
	var m *metrics.MetricsRegistry
	if len(registry) > 0 {
		m = registry[0]
	}
	return &OpsHandler{
		health:     health,
		query:      query,
		portalPort: portalPort,
		metrics:    m,
		support:    DefaultSupportBundleConfig(),
		cdbTrace:   DefaultCDBTraceConfig(),
	}
}

func DefaultCDBTraceConfig() CDBTraceConfig {
	if path := strings.TrimSpace(os.Getenv("HOLO_SCSI_TRACE_CONFIG")); path != "" {
		return CDBTraceConfig{StateFile: path}
	}
	runDir := strings.TrimSpace(os.Getenv("HOLO_RUN_DIR"))
	if runDir == "" {
		runDir = "/run/holo"
	}
	return CDBTraceConfig{StateFile: filepath.Join(runDir, "cdb-trace.enabled")}
}

func (h *OpsHandler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	summary := h.health.Summary()
	status := http.StatusOK
	if summary.Status == "degraded" {
		status = http.StatusServiceUnavailable
	}
	respondJSON(w, status, summary)
}

func (h *OpsHandler) handleAuditEvents(w http.ResponseWriter, r *http.Request) {
	if h.query == nil {
		respondError(w, http.StatusServiceUnavailable, "audit query unavailable", nil)
		return
	}
	res, err := h.query.Query(r.Context(), audit.QueryParams{})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "audit query failed", err)
		return
	}
	respondJSON(w, http.StatusOK, res.Records)
}

func (h *OpsHandler) handleSystemOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	respondJSON(w, http.StatusOK, h.systemOverviewSnapshot())
}

func (h *OpsHandler) handleCDBTrace(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		respondJSON(w, http.StatusOK, h.cdbTraceSnapshot())
	case http.MethodPost:
		var req cdbTraceUpdateRequest
		if err := decodeRequiredJSONBody(r, &req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid cdb trace request", err)
			return
		}
		if err := h.setCDBTraceEnabled(req.Enabled); err != nil {
			respondError(w, http.StatusInternalServerError, "cdb trace update failed", err)
			return
		}
		respondJSON(w, http.StatusOK, h.cdbTraceSnapshot())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *OpsHandler) cdbTraceSnapshot() cdbTraceStatus {
	enabled, ok := cdbTraceEnabledFromFile(h.cdbTrace.StateFile)
	if !ok {
		enabled = cdbTraceEnabledFromEnv()
	}
	return cdbTraceStatus{
		Enabled:   enabled,
		StateFile: h.cdbTrace.StateFile,
	}
}

func (h *OpsHandler) setCDBTraceEnabled(enabled bool) error {
	if strings.TrimSpace(h.cdbTrace.StateFile) == "" {
		return os.ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(h.cdbTrace.StateFile), 0o755); err != nil {
		return err
	}
	value := "0\n"
	if enabled {
		value = "1\n"
	}
	return os.WriteFile(h.cdbTrace.StateFile, []byte(value), 0o644)
}

func cdbTraceEnabledFromEnv() bool {
	raw := strings.TrimSpace(os.Getenv("HOLO_SCSI_TRACE"))
	return isTruthyFlag(raw)
}

func cdbTraceEnabledFromFile(path string) (bool, bool) {
	if strings.TrimSpace(path) == "" {
		return false, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	return isTruthyFlag(string(raw)), true
}

func isTruthyFlag(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func readLoadAverage() (float64, float64, float64) {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return 0, 0, 0
	}
	load1, _ := strconv.ParseFloat(fields[0], 64)
	load5, _ := strconv.ParseFloat(fields[1], 64)
	load15, _ := strconv.ParseFloat(fields[2], 64)
	return load1, load5, load15
}

func readUptimeSeconds() int64 {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 1 {
		return 0
	}
	sec, _ := strconv.ParseFloat(fields[0], 64)
	return int64(sec)
}

func readMemoryInfo() (uint64, uint64) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	var totalKB uint64
	var availableKB uint64
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			totalKB = parseMemInfoValueKB(line)
		}
		if strings.HasPrefix(line, "MemAvailable:") {
			availableKB = parseMemInfoValueKB(line)
		}
	}
	return totalKB * 1024, availableKB * 1024
}

func parseMemInfoValueKB(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	value, err := strconv.ParseUint(strings.TrimSpace(fields[1]), 10, 64)
	if err != nil {
		return 0
	}
	return value
}

func readNetworkTotals() (uint64, uint64) {
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	defer file.Close()

	var rxTotal uint64
	var txTotal uint64
	scanner := bufio.NewScanner(file)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if lineNo <= 2 {
			continue
		}
		parts := strings.Split(scanner.Text(), ":")
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		if iface == "lo" || iface == "" {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 16 {
			continue
		}
		rx, rxErr := strconv.ParseUint(fields[0], 10, 64)
		tx, txErr := strconv.ParseUint(fields[8], 10, 64)
		if rxErr != nil || txErr != nil {
			continue
		}
		rxTotal += rx
		txTotal += tx
	}
	return rxTotal, txTotal
}

func readISCSISessionCount(port int) int {
	hexPort := strings.ToUpper(strconv.FormatInt(int64(port), 16))
	for len(hexPort) < 4 {
		hexPort = "0" + hexPort
	}
	target := ":" + hexPort
	return countEstablishedSessions("/proc/net/tcp", target) + countEstablishedSessions("/proc/net/tcp6", target)
}

func countEstablishedSessions(path, localPortToken string) int {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()
	count := 0
	scanner := bufio.NewScanner(file)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if lineNo == 1 {
			continue
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		localAddr := strings.TrimSpace(fields[1])
		state := strings.TrimSpace(fields[3])
		if !strings.HasSuffix(localAddr, localPortToken) {
			continue
		}
		if state != "01" {
			continue
		}
		count++
	}
	return count
}
