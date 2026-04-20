package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── Engine log ring buffer ─────────────────────────────────────────────────────
// All log.Printf calls are tee-d into this ring so the frontend can stream them.

const engineLogMax = 500

var (
	engineLogMu   sync.Mutex
	engineLogRing []string
)

type engineLogWriter struct{ inner io.Writer }

func (w *engineLogWriter) Write(p []byte) (int, error) {
	n, err := w.inner.Write(p)
	line := strings.TrimRight(string(p), "\r\n")
	if line != "" {
		engineLogMu.Lock()
		engineLogRing = append(engineLogRing, line)
		if len(engineLogRing) > engineLogMax {
			engineLogRing = engineLogRing[len(engineLogRing)-engineLogMax:]
		}
		engineLogMu.Unlock()
	}
	return n, err
}

// ── JSON helpers ───────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON encode error: %v", err)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"detail": msg})
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer io.Copy(io.Discard, r.Body) //nolint:errcheck
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// ── Handlers ───────────────────────────────────────────────────────────────────

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleStatus(w http.ResponseWriter, _ *http.Request) {
	ready := false
	referenceBinary := filepath.Join(referenceDir, "terminal64.exe")
	if _, err := os.Stat(referenceBinary); err == nil {
		if _, err := os.Stat(winPython); err == nil {
			ready = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installed": ready,
		"status":    map[bool]string{true: "ready", false: "not_installed"}[ready],
	})
}

func handleListTerminals(w http.ResponseWriter, _ *http.Request) {
	workers, err := ListTerminals()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, workers)
}

func handleCreateTerminal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string     `json:"name"`
		Token  string     `json:"token"`
		Config *MT5Config `json:"config"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if len(body.Name) > 100 {
		writeError(w, http.StatusBadRequest, "name must be 100 characters or fewer")
		return
	}
	worker, err := CreateTerminal(body.Name, body.Token, body.Config)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			writeError(w, http.StatusConflict, err.Error())
		} else if strings.Contains(err.Error(), "reference install") {
			writeError(w, http.StatusServiceUnavailable, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	// Auto-start in the background so the response returns immediately.
	// The UI polls status and will see the worker transition to "running".
	go func(id string) {
		if _, err := StartTerminal(id); err != nil {
			log.Printf("[create] auto-start worker %s failed: %v", id, err)
			SetTerminalError(id, err.Error())
			return
		}
		if err := WaitForRPyCReady(id, 90*time.Second); err != nil {
			log.Printf("[create] worker %s started but RPyC not ready yet: %v", id, err)
			SetTerminalError(id, err.Error())
		}
	}(worker.ID)

	writeJSON(w, http.StatusCreated, worker)
}

func handleGetTerminal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	worker, err := GetTerminal(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if worker == nil {
		writeError(w, http.StatusNotFound, "worker '"+id+"' not found")
		return
	}
	writeJSON(w, http.StatusOK, worker)
}

func handleDeleteTerminal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	worker, err := GetTerminal(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if worker == nil {
		writeError(w, http.StatusNotFound, "worker '"+id+"' not found")
		return
	}

	go func(wid string) {
		if err := DeleteTerminal(wid); err != nil {
			log.Printf("[delete] worker %s delete failed: %v", wid, err)
		}
	}(id)

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "deleting"})
}

func handleStartTerminal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	worker, err := GetTerminal(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if worker == nil {
		writeError(w, http.StatusNotFound, "worker '"+id+"' not found")
		return
	}

	if worker.Status != StatusRunning && worker.Status != StatusStarting {
		go func(wid string) {
			if _, err := StartTerminal(wid); err != nil {
				log.Printf("[start] worker %s start failed: %v", wid, err)
				SetTerminalError(wid, err.Error())
				return
			}
			if err := WaitForRPyCReady(wid, 90*time.Second); err != nil {
				log.Printf("[start] worker %s started but RPyC not ready yet: %v", wid, err)
				SetTerminalError(wid, err.Error())
			}
		}(id)
		worker.Status = StatusStarting
	}

	writeJSON(w, http.StatusAccepted, worker)
}

func handleStopTerminal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	worker, err := StopTerminal(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if worker == nil {
		writeError(w, http.StatusNotFound, "worker '"+id+"' not found")
		return
	}
	writeJSON(w, http.StatusOK, worker)
}

func handleRestartTerminal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	worker, err := GetTerminal(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if worker == nil {
		writeError(w, http.StatusNotFound, "worker '"+id+"' not found")
		return
	}

	go func(wid string) {
		if _, err := StopTerminal(wid); err != nil {
			// Allow restart even if stop fails; worker might already be stopped.
			log.Printf("[restart] stop worker %s: %v", wid, err)
		}
		if _, err := StartTerminal(wid); err != nil {
			log.Printf("[restart] worker %s start failed: %v", wid, err)
			SetTerminalError(wid, err.Error())
			return
		}
		if err := WaitForRPyCReady(wid, 90*time.Second); err != nil {
			log.Printf("[restart] worker %s started but RPyC not ready yet: %v", wid, err)
			SetTerminalError(wid, err.Error())
		}
	}(worker.ID)

	worker.Status = StatusStarting

	writeJSON(w, http.StatusAccepted, worker)
}

func handleRefreshDisplay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := RefreshDisplay(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var config MT5Config
	if !decodeBody(w, r, &config) {
		return
	}
	worker, err := UpdateConfig(id, config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if worker == nil {
		writeError(w, http.StatusNotFound, "worker '"+id+"' not found")
		return
	}
	writeJSON(w, http.StatusOK, worker)
}

func handleRotateToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	worker, err := RotateToken(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if worker == nil {
		writeError(w, http.StatusNotFound, "worker '"+id+"' not found")
		return
	}
	writeJSON(w, http.StatusOK, worker)
}

func handleGetTerminalLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	writeJSON(w, http.StatusOK, GetTerminalLogs(id))
}

// ── Metrics ────────────────────────────────────────────────────────────────────

type systemMetrics struct {
	CPUPercent  float64        `json:"cpu_percent"`
	MemTotalMB  int64          `json:"mem_total_mb"`
	MemUsedMB   int64          `json:"mem_used_mb"`
	MemPercent  float64        `json:"mem_percent"`
	DiskTotalMB int64          `json:"disk_total_mb"`
	DiskUsedMB  int64          `json:"disk_used_mb"`
	DiskPercent float64        `json:"disk_percent"`
	Terminals     []workerMetric `json:"workers"`
}

type workerMetric struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Status string  `json:"status"`
	MemMB  float64 `json:"mem_mb"`
	CPU    float64 `json:"cpu_percent"`
	DiskMB float64 `json:"disk_mb"`
}

// prevCPU stores the previous /proc/stat snapshot for delta CPU calculation.
var (
	prevCPUTotal uint64
	prevCPUIdle  uint64
	prevCPUMu    sync.Mutex

	// per-PID previous readings for worker CPU deltas
	prevPidCPU   = make(map[int]pidCPUSnap)
	prevPidCPUMu sync.Mutex
)

type pidCPUSnap struct {
	utime  uint64
	stime  uint64
	wallNs int64
}

func readSystemCPU() (total, idle uint64) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	// first line: cpu  user nice system idle iowait irq softirq steal ...
	line := strings.SplitN(string(data), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0
	}
	var sum uint64
	for _, f := range fields[1:] {
		v, _ := strconv.ParseUint(f, 10, 64)
		sum += v
	}
	idleV, _ := strconv.ParseUint(fields[4], 10, 64)
	return sum, idleV
}

func calcSystemCPUPercent() float64 {
	prevCPUMu.Lock()
	defer prevCPUMu.Unlock()

	total, idle := readSystemCPU()
	if prevCPUTotal == 0 {
		prevCPUTotal = total
		prevCPUIdle = idle
		return 0
	}
	dt := total - prevCPUTotal
	di := idle - prevCPUIdle
	prevCPUTotal = total
	prevCPUIdle = idle
	if dt == 0 {
		return 0
	}
	return float64(dt-di) / float64(dt) * 100
}

func readMemInfo() (totalMB, usedMB int64, pct float64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, 0
	}
	var memTotal, memAvail int64
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fmt.Sscanf(line, "MemTotal: %d kB", &memTotal)
		} else if strings.HasPrefix(line, "MemAvailable:") {
			fmt.Sscanf(line, "MemAvailable: %d kB", &memAvail)
		}
	}
	totalMB = memTotal / 1024
	usedMB = (memTotal - memAvail) / 1024
	if memTotal > 0 {
		pct = float64(memTotal-memAvail) / float64(memTotal) * 100
	}
	return
}

func readDiskUsage(path string) (totalMB, usedMB int64, pct float64) {
	totalB, usedB, err := diskUsageBytes(path)
	if err != nil {
		return 0, 0, 0
	}
	totalMB = int64(totalB / (1024 * 1024))
	usedMB = int64(usedB / (1024 * 1024))
	if totalB > 0 {
		pct = float64(usedB) / float64(totalB) * 100
	}
	return
}

// dirSizeMB returns the total size of a directory tree in MB.
func dirSizeMB(path string) float64 {
	var total int64
	filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip inaccessible entries
		}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return math.Round(float64(total)/(1024*1024)*10) / 10
}

// pidRSSMB reads RSS from /proc/<pid>/status (VmRSS line, in kB).
func pidRSSMB(pid int) float64 {
	if pid <= 0 {
		return 0
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			var kb int64
			fmt.Sscanf(line, "VmRSS: %d kB", &kb)
			return float64(kb) / 1024.0
		}
	}
	return 0
}

// pidCPUPercent returns approximate CPU% for a PID since the last call.
func pidCPUPercent(pid int) float64 {
	if pid <= 0 {
		return 0
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// Fields are: pid (comm) state ppid ... utime(14) stime(15) ...
	// Find closing paren to skip comm which may contain spaces
	closeP := strings.LastIndex(string(data), ")")
	if closeP < 0 {
		return 0
	}
	rest := strings.Fields(string(data)[closeP+2:])
	if len(rest) < 13 {
		return 0
	}
	utime, _ := strconv.ParseUint(rest[11], 10, 64) // field 14 (0-indexed 11 after state)
	stime, _ := strconv.ParseUint(rest[12], 10, 64) // field 15

	now := time.Now().UnixNano()

	prevPidCPUMu.Lock()
	defer prevPidCPUMu.Unlock()

	prev, ok := prevPidCPU[pid]
	prevPidCPU[pid] = pidCPUSnap{utime: utime, stime: stime, wallNs: now}
	if !ok || now == prev.wallNs {
		return 0
	}

	// clock ticks → seconds (typically 100 Hz)
	hz := uint64(100)
	dticks := (utime + stime) - (prev.utime + prev.stime)
	wallSec := float64(now-prev.wallNs) / 1e9
	if wallSec <= 0 {
		return 0
	}
	return (float64(dticks) / float64(hz)) / wallSec * 100
}

func handleMetrics(w http.ResponseWriter, _ *http.Request) {
	cpuPct := calcSystemCPUPercent()
	memTotal, memUsed, memPct := readMemInfo()
	diskTotal, diskUsed, diskPct := readDiskUsage(fleetDir)

	workers, _ := ListTerminals()
	wm := make([]workerMetric, 0, len(workers))
	for _, wk := range workers {
		var rss float64
		var cpu float64
		// Sum RSS and CPU for all PIDs belonging to this worker
		pids := []int{wk.PIDTerminal, wk.PIDRPyC, wk.PIDWM, wk.PIDXvnc, wk.PIDWsockify}
		for _, pid := range pids {
			rss += pidRSSMB(pid)
			cpu += pidCPUPercent(pid)
		}
		diskMB := dirSizeMB(filepath.Join(workersDir, wk.ID))
		wm = append(wm, workerMetric{
			ID:     wk.ID,
			Name:   wk.Name,
			Status: string(wk.Status),
			MemMB:  math.Round(rss*10) / 10,
			CPU:    math.Round(cpu*10) / 10,
			DiskMB: diskMB,
		})
	}

	// Evict stale entries from the per-PID CPU snapshot map.
	activePIDs := make(map[int]struct{})
	for _, wk := range workers {
		for _, pid := range []int{wk.PIDTerminal, wk.PIDRPyC, wk.PIDWM, wk.PIDXvnc, wk.PIDWsockify} {
			if pid > 0 {
				activePIDs[pid] = struct{}{}
			}
		}
	}
	prevPidCPUMu.Lock()
	for pid := range prevPidCPU {
		if _, active := activePIDs[pid]; !active {
			delete(prevPidCPU, pid)
		}
	}
	prevPidCPUMu.Unlock()

	writeJSON(w, http.StatusOK, systemMetrics{
		CPUPercent:  math.Round(cpuPct*10) / 10,
		MemTotalMB:  memTotal,
		MemUsedMB:   memUsed,
		MemPercent:  math.Round(memPct*10) / 10,
		DiskTotalMB: diskTotal,
		DiskUsedMB:  diskUsed,
		DiskPercent: math.Round(diskPct*10) / 10,
		Terminals:     wm,
	})
}

func handleGetEngineLogs(w http.ResponseWriter, _ *http.Request) {
	engineLogMu.Lock()
	lines := make([]string, len(engineLogRing))
	copy(lines, engineLogRing)
	engineLogMu.Unlock()
	writeJSON(w, http.StatusOK, lines)
}

func handleRenameTerminal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if len(body.Name) > 100 {
		writeError(w, http.StatusBadRequest, "name must be 100 characters or fewer")
		return
	}
	worker, err := RenameTerminal(id, body.Name)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			writeError(w, http.StatusConflict, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if worker == nil {
		writeError(w, http.StatusNotFound, "worker '"+id+"' not found")
		return
	}
	writeJSON(w, http.StatusOK, worker)
}

// ── VNC WebSocket proxy ────────────────────────────────────────────────────────
// Proxies WebSocket connections through the API so only one port is exposed.

const installerWSPort = 6799

func vncProxy(localPort int) http.Handler {
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("localhost:%d", localPort)}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1
	base := rp.Director
	rp.Director = func(req *http.Request) {
		base(req)
		req.URL.Path = "/"
		req.URL.RawPath = ""
		req.Host = target.Host
	}
	return rp
}

func handleVNCTerminal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	worker, err := GetTerminal(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if worker == nil {
		writeError(w, http.StatusNotFound, "worker '"+id+"' not found")
		return
	}
	if worker.Port == 0 {
		writeError(w, http.StatusServiceUnavailable, "worker has no port assigned")
		return
	}
	localPort := vncWSLocalBase + (worker.Port - basePort)
	vncProxy(localPort).ServeHTTP(w, r)
}

func handleVNCInstaller(w http.ResponseWriter, r *http.Request) {
	vncProxy(installerWSPort).ServeHTTP(w, r)
}

// ── Main ───────────────────────────────────────────────────────────────────────

func main() {
	initPaths()

	// Tee all log.Printf output into the in-memory ring so the UI can stream it.
	log.SetOutput(&engineLogWriter{inner: os.Stderr})

	// Reset stale worker state from a previous container lifecycle.
	cleanStaleState()
	startTerminalSupervisor()

	port := envOr("PORT", "18810")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /status", handleStatus)
	mux.HandleFunc("GET /workers", handleListTerminals)
	mux.HandleFunc("POST /workers", handleCreateTerminal)
	mux.HandleFunc("GET /workers/{id}", handleGetTerminal)
	mux.HandleFunc("DELETE /workers/{id}", handleDeleteTerminal)
	mux.HandleFunc("POST /workers/{id}/start", handleStartTerminal)
	mux.HandleFunc("POST /workers/{id}/stop", handleStopTerminal)
	mux.HandleFunc("POST /workers/{id}/restart", handleRestartTerminal)
	mux.HandleFunc("PATCH /workers/{id}/config", handleUpdateConfig)
	mux.HandleFunc("PATCH /workers/{id}/name", handleRenameTerminal)
	mux.HandleFunc("GET /workers/{id}/logs", handleGetTerminalLogs)
	mux.HandleFunc("POST /workers/{id}/rotate-token", handleRotateToken)
	mux.HandleFunc("POST /workers/{id}/refresh-display", handleRefreshDisplay)
	mux.HandleFunc("GET /logs", handleGetEngineLogs)
	mux.HandleFunc("GET /metrics", handleMetrics)
	mux.HandleFunc("/vnc/workers/{id}", handleVNCTerminal)
	mux.HandleFunc("/vnc/installer", handleVNCInstaller)

	log.Printf("my5fleet engine control API listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envOrInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			return parsed
		}
	}
	return fallback
}

// filteredEnv returns os.Environ() with overrides applied. Any existing keys
// that match an override are removed first to prevent duplicates (e.g. DISPLAY).
func filteredEnv(overrides ...string) []string {
	keys := make(map[string]struct{}, len(overrides))
	for _, o := range overrides {
		if k, _, ok := strings.Cut(o, "="); ok {
			keys[k] = struct{}{}
		}
	}
	env := os.Environ()
	filtered := make([]string, 0, len(env)+len(overrides))
	for _, e := range env {
		if k, _, ok := strings.Cut(e, "="); ok {
			if _, dup := keys[k]; dup {
				continue
			}
		}
		filtered = append(filtered, e)
	}
	return append(filtered, overrides...)
}
