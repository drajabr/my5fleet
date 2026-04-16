package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── JSON helpers ───────────────────────────────────────────────────────────────
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
	installed := false
	flag := filepath.Join(fleetDir, ".installed")
	referenceBinary := filepath.Join(referenceDir, "terminal64.exe")
	installerVNCWSPort := envOrInt("INSTALLER_VNC_WS_PORT", 0)
	if _, err := os.Stat(flag); err == nil {
		if _, err := os.Stat(referenceBinary); err == nil {
			if _, err := os.Stat(winPython); err == nil {
				installed = true
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installed":             installed,
		"status":                map[bool]string{true: "ready", false: "installing"}[installed],
		"installer_vnc_ws_port": installerVNCWSPort,
	})
}

func readInstallLogLines(maxLines int) []string {
	data, err := os.ReadFile(filepath.Join(fleetDir, "config", "install.log"))
	if err != nil || len(data) == 0 {
		return nil
	}
	text := strings.ReplaceAll(string(data), "\x00", "")
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines
}

func handleListWorkers(w http.ResponseWriter, _ *http.Request) {
	workers, err := ListWorkers()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, workers)
}

func handleCreateWorker(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string     `json:"name"`
		Token  string     `json:"token"`
		Config *MT5Config `json:"config"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	worker, err := CreateWorker(body.Name, body.Token, body.Config)
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
		if _, err := StartWorker(id); err != nil {
			log.Printf("[create] auto-start worker %s failed: %v", id, err)
			return
		}
		if err := WaitForRPyCReady(id, 90*time.Second); err != nil {
			log.Printf("[create] worker %s started but RPyC not ready yet: %v", id, err)
		}
	}(worker.ID)

	writeJSON(w, http.StatusCreated, worker)
}

func handleGetWorker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	worker, err := GetWorker(id)
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

func handleDeleteWorker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := DeleteWorker(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleStartWorker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	worker, err := StartWorker(id)
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

func handleStopWorker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	worker, err := StopWorker(id)
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

func handleRestartWorker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := StopWorker(id); err != nil {
		// Allow restart even if stop fails; worker might already be stopped
		log.Printf("[restart] stop worker %s: %v", id, err)
	}
	worker, err := StartWorker(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"restart failed: "+err.Error())
		return
	}
	if worker == nil {
		writeError(w, http.StatusNotFound, "worker '"+id+"' not found")
		return
	}

	// Fire-and-forget RPyC readiness wait — the UI will poll status.
	go func(wid string) {
		if err := WaitForRPyCReady(wid, 90*time.Second); err != nil {
			log.Printf("[restart] worker %s started but RPyC not ready yet: %v", wid, err)
		}
	}(worker.ID)

	worker, _ = GetWorker(worker.ID)
	writeJSON(w, http.StatusOK, worker)
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

func handleGetWorkerLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	writeJSON(w, http.StatusOK, GetWorkerLogs(id))
}

func handleGetEngineLogs(w http.ResponseWriter, _ *http.Request) {
	flag := filepath.Join(fleetDir, ".installed")
	if _, err := os.Stat(flag); err != nil {
		if lines := readInstallLogLines(400); len(lines) > 0 {
			writeJSON(w, http.StatusOK, lines)
			return
		}
	}
	engineLogMu.Lock()
	lines := make([]string, len(engineLogRing))
	copy(lines, engineLogRing)
	engineLogMu.Unlock()
	writeJSON(w, http.StatusOK, lines)
}

func handleRenameWorker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	worker, err := RenameWorker(id, body.Name)
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

// ── Main ───────────────────────────────────────────────────────────────────────

func main() {
	initPaths()

	// Tee all log.Printf output into the in-memory ring so the UI can stream it.
	log.SetOutput(&engineLogWriter{inner: os.Stderr})

	// Reset stale worker state from a previous container lifecycle.
	cleanStaleState()
	startWorkerSupervisor()

	port := envOr("PORT", "18810")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /status", handleStatus)
	mux.HandleFunc("GET /workers", handleListWorkers)
	mux.HandleFunc("POST /workers", handleCreateWorker)
	mux.HandleFunc("GET /workers/{id}", handleGetWorker)
	mux.HandleFunc("DELETE /workers/{id}", handleDeleteWorker)
	mux.HandleFunc("POST /workers/{id}/start", handleStartWorker)
	mux.HandleFunc("POST /workers/{id}/stop", handleStopWorker)
	mux.HandleFunc("POST /workers/{id}/restart", handleRestartWorker)
	mux.HandleFunc("PATCH /workers/{id}/config", handleUpdateConfig)
	mux.HandleFunc("PATCH /workers/{id}/name", handleRenameWorker)
	mux.HandleFunc("GET /workers/{id}/logs", handleGetWorkerLogs)
	mux.HandleFunc("POST /workers/{id}/rotate-token", handleRotateToken)
	mux.HandleFunc("GET /logs", handleGetEngineLogs)

	log.Printf("mt5-fleet engine control API listening on :%s", port)
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
