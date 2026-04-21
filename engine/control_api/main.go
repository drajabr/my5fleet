package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// â”€â”€ Engine log ring buffer â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
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

// â”€â”€ JSON helpers â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

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

// â”€â”€ Handlers â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleStatus(w http.ResponseWriter, _ *http.Request) {
	lockFile := filepath.Join(fleetDir, "reference", ".installing")
	installing := false
	if _, err := os.Stat(lockFile); err == nil {
		installing = true
	}

	ready := false
	if !installing {
		referenceBinary := filepath.Join(referenceDir, "terminal64.exe")
		if _, err := os.Stat(referenceBinary); err == nil {
			if _, err := os.Stat(winPython); err == nil {
				ready = true
			}
		}
	}

	status := "not_installed"
	if installing {
		status = "installing"
	} else if ready {
		status = "ready"
	}

	refProgramStatus := supervisorProgramStatus("reference-mt5")
	referenceRunning := refProgramStatus == "RUNNING"
	vncReady := isTCPPortReady("127.0.0.1", installerWSPort, 300*time.Millisecond)

	writeJSON(w, http.StatusOK, map[string]any{
		"installed":            ready,
		"status":               status,
		"reference_installed":  ready,
		"reference_installing": installing,
		"reference_running":    referenceRunning,
		"reference_status":     strings.ToLower(refProgramStatus),
		"installer_vnc_ready":  vncReady,
	})
}

func isTCPPortReady(host string, port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func supervisorProgramStatus(program string) string {
	cmd := exec.Command("supervisorctl", "status", program)
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return "UNKNOWN"
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 2 {
		return "UNKNOWN"
	}
	return strings.ToUpper(fields[1])
}

func supervisorAction(action, program string) (string, error) {
	cmd := exec.Command("supervisorctl", action, program)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("supervisorctl %s %s failed: %w (%s)", action, program, err, text)
	}
	return text, nil
}

func isReferenceActiveStatus(status string) bool {
	status = strings.ToUpper(strings.TrimSpace(status))
	// Treat unknown/transient states as active to fail closed.
	switch status {
	case "STOPPED", "EXITED", "FATAL":
		return false
	default:
		return true
	}
}

// stopAllWorkersForReference stops every running/starting worker and clears
// keep_alive so the supervisor does not restart them while reference is active.
// Workers regain keep_alive (and thus auto-restart) when reference stops.
func stopAllWorkersForReference(timeout time.Duration) error {
	// First disable keep_alive for all workers so no supervisor cycle can restart
	// them while reference mode is being activated.
	mu.Lock()
	workersMap, err := load()
	if err != nil {
		mu.Unlock()
		return fmt.Errorf("load workers failed: %w", err)
	}
	changed := false
	for _, ww := range workersMap {
		if ww.KeepAlive == nil || *ww.KeepAlive {
			ww.KeepAlive = boolPtr(false)
			changed = true
		}
	}
	if changed {
		if err := save(workersMap); err != nil {
			mu.Unlock()
			return fmt.Errorf("save workers failed: %w", err)
		}
	}
	mu.Unlock()

	workers, err := ListTerminals()
	if err != nil {
		return fmt.Errorf("list workers failed: %w", err)
	}

	var wg sync.WaitGroup
	for _, w := range workers {
		if w.Status == StatusRunning || w.Status == StatusStarting || w.Status == StatusError {
			wid := w.ID
			wg.Add(1)
			go func() {
				defer wg.Done()
				log.Printf("[reference] stopping worker %s before reference start", wid)
				if _, err := StopTerminal(wid); err != nil {
					log.Printf("[reference] stop worker %s failed: %v", wid, err)
				}
			}()
		}
	}
	wg.Wait()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		remaining := make([]string, 0)
		latest, err := ListTerminals()
		if err != nil {
			return fmt.Errorf("list workers during drain failed: %w", err)
		}
		for _, w := range latest {
			if w.Status == StatusRunning || w.Status == StatusStarting || w.Status == StatusStopping || w.Status == StatusError {
				remaining = append(remaining, w.ID+"("+string(w.Status)+")")
			}
		}
		if len(remaining) == 0 {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}

	latest, err := ListTerminals()
	if err != nil {
		return fmt.Errorf("list workers after drain timeout failed: %w", err)
	}
	remaining := make([]string, 0)
	for _, w := range latest {
		if w.Status == StatusRunning || w.Status == StatusStarting || w.Status == StatusStopping || w.Status == StatusError {
			remaining = append(remaining, w.ID+"("+string(w.Status)+")")
		}
	}
	if len(remaining) > 0 {
		return fmt.Errorf("workers not fully stopped before reference start: %s", strings.Join(remaining, ", "))
	}

	return nil
}

func handleReferenceStart(w http.ResponseWriter, _ *http.Request) {
	referenceBinary := filepath.Join(referenceDir, "terminal64.exe")
	if _, err := os.Stat(referenceBinary); err != nil {
		writeError(w, http.StatusConflict, "reference terminal is not installed yet")
		return
	}

	// Stop all workers first — they cannot run alongside the reference terminal.
	if err := stopAllWorkersForReference(8 * time.Second); err != nil {
		writeError(w, http.StatusConflict, "failed to stop workers before reference start: "+err.Error())
		return
	}

	out, err := supervisorAction("start", "reference-mt5")
	if err != nil && !strings.Contains(strings.ToLower(out), "already started") {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "starting"})
}

func handleReferenceStop(w http.ResponseWriter, _ *http.Request) {
	out, err := supervisorAction("stop", "reference-mt5")
	if err != nil {
		lower := strings.ToLower(out)
		if !strings.Contains(lower, "not running") && !strings.Contains(lower, "no such process") {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	// Re-enable keep_alive on all workers so the supervisor restarts them.
	resumeAllWorkersAfterReference()
	triggerBulkWorkerStartAfterReferenceStop()

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "stopped"})
}

// resumeAllWorkersAfterReference re-enables keep_alive on every worker that
// was stopped for reference mode. The supervisor loop will then restart them.
func resumeAllWorkersAfterReference() {
	mu.Lock()
	defer mu.Unlock()
	workers, err := load()
	if err != nil {
		log.Printf("[reference] resume workers load failed: %v", err)
		return
	}
	changed := false
	for _, w := range workers {
		if w.KeepAlive != nil && !*w.KeepAlive {
			w.KeepAlive = boolPtr(true)
			changed = true
			log.Printf("[reference] re-enabled keep_alive for worker %s", w.ID)
		}
	}
	if changed {
		_ = save(workers)
	}
}

// triggerBulkWorkerStartAfterReferenceStop kicks off worker starts immediately
// after reference mode is turned off, instead of waiting for the next supervisor
// ticker cycle. Starts are dispatched in parallel for faster recovery.
func triggerBulkWorkerStartAfterReferenceStop() {
	go func() {
		deadline := time.Now().Add(8 * time.Second)
		ready := false
		for time.Now().Before(deadline) {
			status := supervisorProgramStatus("reference-mt5")
			if !isReferenceActiveStatus(status) {
				ready = true
				break
			}
			time.Sleep(150 * time.Millisecond)
		}

		if !ready {
			log.Printf("[reference] bulk start skipped: reference-mt5 still active")
			return
		}

		workers, err := ListTerminals()
		if err != nil {
			log.Printf("[reference] bulk start list workers failed: %v", err)
			return
		}

		var wg sync.WaitGroup
		for _, w := range workers {
			if !shouldSupervisorStart(w) {
				continue
			}

			wid := w.ID
			wg.Add(1)
			go func() {
				defer wg.Done()
				log.Printf("[reference] bulk-start worker %s", wid)
				if _, err := StartTerminal(wid); err != nil {
					log.Printf("[reference] bulk-start worker %s failed: %v", wid, err)
				}
			}()
		}
		wg.Wait()
	}()
}

func handleReferenceRebuild(w http.ResponseWriter, _ *http.Request) {
	// Stop the reference runtime and installer before resetting artifacts.
	_, _ = supervisorAction("stop", "reference-mt5")
	_, _ = supervisorAction("stop", "reference-install")

	pyDir := filepath.Dir(winPython)
	lockFile := filepath.Join(fleetDir, "reference", ".installing")

	if err := os.RemoveAll(referenceDir); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to clear reference install: "+err.Error())
		return
	}
	if err := os.RemoveAll(pyDir); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to clear embedded Python: "+err.Error())
		return
	}
	_ = os.Remove(lockFile)

	if _, err := supervisorAction("start", "reference-install"); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := supervisorAction("start", "reference-mt5"); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "rebuilding"})
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

// â”€â”€ Metrics â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

type systemMetrics struct {
	CPUPercent  float64        `json:"cpu_percent"`
	MemTotalMB  int64          `json:"mem_total_mb"`
	MemUsedMB   int64          `json:"mem_used_mb"`
	MemPercent  float64        `json:"mem_percent"`
	DiskTotalMB int64          `json:"disk_total_mb"`
	DiskUsedMB  int64          `json:"disk_used_mb"`
	DiskPercent float64        `json:"disk_percent"`
	Terminals   []workerMetric `json:"workers"`
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

	// clock ticks â†’ seconds (typically 100 Hz)
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
		Terminals:   wm,
	})
}

func handleGetEngineLogs(w http.ResponseWriter, _ *http.Request) {
	files := []struct {
		path   string
		prefix string
	}{
		{path: "/var/log/supervisor/supervisord.log", prefix: "[supervisord]"},
		{path: "/var/log/supervisor/control-api.out.log", prefix: "[control-api]"},
		{path: "/var/log/supervisor/control-api.err.log", prefix: "[control-api][err]"},
		{path: "/var/log/supervisor/reference-install.out.log", prefix: "[reference-install]"},
		{path: "/var/log/supervisor/reference-install.err.log", prefix: "[reference-install][err]"},
		{path: "/var/log/supervisor/reference-mt5.out.log", prefix: "[reference-mt5]"},
		{path: "/var/log/supervisor/reference-mt5.err.log", prefix: "[reference-mt5][err]"},
		{path: "/var/log/supervisor/reference-wm.out.log", prefix: "[reference-wm]"},
		{path: "/var/log/supervisor/reference-wm.err.log", prefix: "[reference-wm][err]"},
		{path: "/var/log/supervisor/reference-websockify.out.log", prefix: "[reference-websockify]"},
		{path: "/var/log/supervisor/reference-websockify.err.log", prefix: "[reference-websockify][err]"},
		{path: "/var/log/supervisor/reference-xvnc.out.log", prefix: "[reference-xvnc]"},
		{path: "/var/log/supervisor/reference-xvnc.err.log", prefix: "[reference-xvnc][err]"},
	}

	lines := make([]string, 0, engineLogMax)
	seen := make(map[string]struct{}, engineLogMax*2)
	appendUnique := func(v string) {
		if v == "" {
			return
		}
		if _, ok := seen[v]; ok {
			return
		}
		seen[v] = struct{}{}
		lines = append(lines, v)
	}

	for _, f := range files {
		for _, line := range tailFileLines(f.path, 80) {
			appendUnique(fmt.Sprintf("%s %s", f.prefix, line))
		}
	}

	engineLogMu.Lock()
	ring := make([]string, len(engineLogRing))
	copy(ring, engineLogRing)
	engineLogMu.Unlock()

	for _, line := range ring {
		appendUnique(line)
	}

	if len(lines) > engineLogMax {
		lines = lines[len(lines)-engineLogMax:]
	}
	writeJSON(w, http.StatusOK, lines)
}

func tailFileLines(path string, maxLines int) []string {
	if maxLines <= 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	buf := make([]string, 0, maxLines)
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		buf = append(buf, line)
		if len(buf) > maxLines {
			buf = buf[1:]
		}
	}
	return buf
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

// â”€â”€ VNC WebSocket proxy â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
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

// â”€â”€ Main â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

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
	mux.HandleFunc("POST /reference/start", handleReferenceStart)
	mux.HandleFunc("POST /reference/stop", handleReferenceStop)
	mux.HandleFunc("POST /reference/rebuild", handleReferenceRebuild)
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
