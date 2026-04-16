package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ── Types ──────────────────────────────────────────────────────────────────────

type WorkerStatus string

const (
	StatusStopped  WorkerStatus = "stopped"
	StatusStarting WorkerStatus = "starting"
	StatusRunning  WorkerStatus = "running"
	StatusStopping WorkerStatus = "stopping"
	StatusError    WorkerStatus = "error"
)

type MT5Config struct {
	Login    int    `json:"login"`
	Password string `json:"password"`
	Server   string `json:"server"`
}

type Worker struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Status      WorkerStatus `json:"status"`
	Port        int          `json:"port"`
	Token       string       `json:"token"`
	Config      *MT5Config   `json:"config,omitempty"`
	PIDTerminal int          `json:"pid_terminal,omitempty"`
	PIDRPyC     int          `json:"pid_rpyc,omitempty"`
	VNCWSPort   int          `json:"vnc_ws_port,omitempty"`
	PIDXvfb     int          `json:"pid_xvfb,omitempty"`
	PIDx11vnc   int          `json:"pid_x11vnc,omitempty"`
	PIDWsockify int          `json:"pid_wsockify,omitempty"`
}

// ── Environment / paths ────────────────────────────────────────────────────────

var (
	fleetDir     string
	winePrefix   string
	display      string
	workersDir   string
	referenceDir string
	workersJSON  string
	winPython    string

	// Windows Z:-path to the RPyC server script (wine maps / → Z:\)
	rpycScript = `Z:\opt\mt5\scripts\worker_rpyc.py`

	basePort       = 18812
	vncWSHostBase  = 19000 // host-published websockify port base
	vncWSLocalBase = 6800  // container-local websockify port base
	vncRFBBase     = 5900  // container-local x11vnc RFB port base
	vncDisplayBase = 100   // Xvfb display number base

	writableDirs = []string{"MQL5", "logs", "config", "tester", "bases", "profiles"}

	mu sync.Mutex // guards all workers.json reads/writes
)

const workerSupervisorInterval = 10 * time.Second

func initPaths() {
	fleetDir = envOr("FLEET_DIR", "/mt5-fleet")
	display = envOr("DISPLAY", ":99")
	winePrefix = envOr("WINEPREFIX", filepath.Join(fleetDir, "wineprefix"))
	workersDir = filepath.Join(fleetDir, "workers")
	referenceDir = filepath.Join(fleetDir, "reference", "install")
	workersJSON = filepath.Join(fleetDir, "config", "workers.json")
	winPython = filepath.Join(winePrefix, "drive_c", "Python311", "pythonw.exe")
	basePort = envOrInt("WORKER_PORT_RANGE_START", basePort)
	vncWSHostBase = envOrInt("VNC_WS_PORT_RANGE_START", vncWSHostBase)
}

// ── Persistence ────────────────────────────────────────────────────────────────

func load() (map[string]*Worker, error) {
	data, err := os.ReadFile(workersJSON)
	if os.IsNotExist(err) {
		return make(map[string]*Worker), nil
	}
	if err != nil {
		return nil, err
	}
	workers := make(map[string]*Worker)
	if err := json.Unmarshal(data, &workers); err != nil {
		return nil, err
	}
	return workers, nil
}

func save(workers map[string]*Worker) error {
	if err := os.MkdirAll(filepath.Dir(workersJSON), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(workers, "", "  ")
	if err != nil {
		return err
	}
	tmp := workersJSON + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, workersJSON) // atomic on Linux
}

// ── Allocation helpers ─────────────────────────────────────────────────────────

func nextID(workers map[string]*Worker) string {
	for i := 1; ; i++ {
		id := fmt.Sprintf("terminal_%d", i)
		if _, exists := workers[id]; !exists {
			return id
		}
	}
}

func nextPort(workers map[string]*Worker) int {
	used := make(map[int]struct{})
	for _, w := range workers {
		used[w.Port] = struct{}{}
	}
	for p := basePort; ; p++ {
		if _, taken := used[p]; !taken {
			return p
		}
	}
}

// ── Process helpers ────────────────────────────────────────────────────────────

// pidAlive checks process liveness by sending signal 0 (no-op probe).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// pidMatchesWorker checks /proc/<pid>/cmdline to verify the process belongs to
// the expected worker. This prevents false positives from PID reuse after a
// container restart.
func pidMatchesWorker(pid int, workerID string) bool {
	if pid <= 0 {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	return strings.Contains(string(data), workerID)
}

func deriveStatus(w *Worker) WorkerStatus {
	aliveT := pidAlive(w.PIDTerminal) && pidMatchesWorker(w.PIDTerminal, w.ID)
	aliveR := pidAlive(w.PIDRPyC) && pidMatchesWorker(w.PIDRPyC, w.ID)
	switch {
	case aliveT && aliveR:
		return StatusRunning
	case aliveT || aliveR:
		return StatusError // one half crashed
	default:
		return StatusStopped
	}
}

// cleanStaleState resets all workers to stopped with zeroed PIDs on startup.
// After a container restart, all old PIDs are invalid. Without this cleanup,
// PID reuse can cause deriveStatus to falsely report a worker as running.
func cleanStaleState() {
	mu.Lock()
	defer mu.Unlock()

	workers, err := load()
	if err != nil || len(workers) == 0 {
		return
	}

	changed := false
	for _, w := range workers {
		if deriveStatus(w) == StatusStopped && (w.PIDTerminal != 0 || w.PIDRPyC != 0) {
			w.PIDTerminal = 0
			w.PIDRPyC = 0
			w.PIDXvfb = 0
			w.PIDx11vnc = 0
			w.PIDWsockify = 0
			w.Status = StatusStopped
			changed = true
			log.Printf("[cleanup] Reset stale state for worker %s", w.ID)
		}
	}
	if changed {
		_ = save(workers)
	}
}

func wineEnv() []string {
	return append(os.Environ(),
		"WINEPREFIX="+winePrefix,
		"DISPLAY="+display,
		"WINEDEBUG=-all",
	)
}

// killPID sends SIGTERM, waits up to 8 s, then sends SIGKILL.
func killPID(pid int, label string) {
	if !pidAlive(pid) {
		return
	}
	log.Printf("[%s] SIGTERM → PID %d", label, pid)
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 8; i++ {
		time.Sleep(time.Second)
		if !pidAlive(pid) {
			return
		}
	}
	log.Printf("[%s] SIGKILL → PID %d", label, pid)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// ── Filesystem helpers ─────────────────────────────────────────────────────────

func createFS(workerID string) error {
	workerDir := filepath.Join(workersDir, workerID)
	if _, err := os.Stat(workerDir); err == nil {
		return fmt.Errorf("worker directory already exists: %s", workerDir)
	}
	if _, err := os.Stat(referenceDir); err != nil {
		return fmt.Errorf("reference install not found at %s — has first-boot finished?", referenceDir)
	}

	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		return err
	}

	// Build a set of writable dir names for quick lookup
	writable := make(map[string]struct{}, len(writableDirs))
	for _, d := range writableDirs {
		writable[d] = struct{}{}
	}

	// Symlink every reference item that is not worker-writable
	entries, err := os.ReadDir(referenceDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if _, skip := writable[entry.Name()]; skip {
			continue
		}
		link := filepath.Join(workerDir, entry.Name())
		target := filepath.Join(referenceDir, entry.Name())
		if err := os.Symlink(target, link); err != nil && !os.IsExist(err) {
			return err
		}
	}

	// Create fresh writable directories
	for _, d := range writableDirs {
		if err := os.MkdirAll(filepath.Join(workerDir, d), 0o755); err != nil {
			return err
		}
	}

	log.Printf("[createFS] Worker filesystem ready: %s", workerDir)
	return nil
}

func removeFS(workerID string) error {
	workerDir := filepath.Join(workersDir, workerID)
	if err := os.RemoveAll(workerDir); err != nil {
		return err
	}
	log.Printf("[removeFS] Removed: %s", workerDir)
	return nil
}

// ── Token ─────────────────────────────────────────────────────────────────────

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// ── Public manager operations ──────────────────────────────────────────────────

func ListWorkers() ([]*Worker, error) {
	mu.Lock()
	defer mu.Unlock()

	workers, err := load()
	if err != nil {
		return nil, err
	}

	changed := false
	list := make([]*Worker, 0, len(workers))
	for _, w := range workers {
		live := deriveStatus(w)
		// Sync transient statuses once the processes settle
		if w.Status != live &&
			(w.Status == StatusStarting || w.Status == StatusStopping ||
				w.Status == StatusRunning || w.Status == StatusError) {
			w.Status = live
			changed = true
		}
		// Backfill VNCWSPort for workers created before VNC support
		if w.VNCWSPort == 0 && w.Port > 0 {
			w.VNCWSPort = (w.Port - basePort) + vncWSHostBase
			changed = true
		}
		list = append(list, w)
	}
	if changed {
		_ = save(workers)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Port < list[j].Port })
	return list, nil
}

func GetWorker(id string) (*Worker, error) {
	mu.Lock()
	defer mu.Unlock()

	workers, err := load()
	if err != nil {
		return nil, err
	}
	w, ok := workers[id]
	if !ok {
		return nil, nil
	}
	w.Status = deriveStatus(w)
	return w, nil
}

func CreateWorker(name string, token string, config *MT5Config) (*Worker, error) {
	mu.Lock()
	defer mu.Unlock()

	workers, err := load()
	if err != nil {
		return nil, err
	}

	id := nextID(workers)
	port := nextPort(workers)

	if err := createFS(id); err != nil {
		return nil, err
	}

	if name == "" {
		name = id
	}
	if token == "" {
		token = newToken()
	}

	w := &Worker{
		ID:        id,
		Name:      name,
		Status:    StatusStopped,
		Port:      port,
		Token:     token,
		Config:    config,
		VNCWSPort: (port - basePort) + vncWSHostBase,
	}
	workers[id] = w
	if err := save(workers); err != nil {
		return nil, err
	}
	log.Printf("[create] Worker %s created on port %d", id, port)
	return w, nil
}

func DeleteWorker(id string) error {
	mu.Lock()
	defer mu.Unlock()

	workers, err := load()
	if err != nil {
		return err
	}
	w, ok := workers[id]
	if !ok {
		return nil // idempotent
	}

	// Stop if running
	if deriveStatus(w) != StatusStopped {
		stopProcs(w)
	}

	if err := removeFS(id); err != nil {
		return err
	}
	delete(workers, id)
	return save(workers)
}

func StartWorker(id string) (*Worker, error) {
	mu.Lock()
	defer mu.Unlock()

	workers, err := load()
	if err != nil {
		return nil, err
	}
	w, ok := workers[id]
	if !ok {
		return nil, nil
	}

	current := deriveStatus(w)
	if current == StatusRunning {
		return w, nil
	}

	if w.Status == StatusStopping {
		return w, nil
	}

	if current == StatusError {
		log.Printf("[start] Worker %s unhealthy; cleaning up stale processes before restart", id)
		stopProcs(w)
	}

	w.PIDTerminal = 0
	w.PIDRPyC = 0
	w.PIDXvfb = 0
	w.PIDx11vnc = 0
	w.PIDWsockify = 0

	workerDir := filepath.Join(workersDir, id)
	logsDir := filepath.Join(workerDir, "logs")
	winMT5Path := fmt.Sprintf(`Z:\mt5-fleet\workers\%s\terminal64.exe`, id)

	// ── Per-worker virtual display ─────────────────────────────────────────────
	portOffset := w.Port - basePort
	displayNum := vncDisplayBase + portOffset
	workerDisplay := fmt.Sprintf(":%d", displayNum)
	vncInternalPort := vncRFBBase + portOffset
	wsContainerPort := vncWSLocalBase + portOffset

	// Clean up any stale Xvfb lock/socket files left over from a previous
	// SIGKILL. Xvfb refuses to start if these files exist, silently breaking
	// the worker's display and causing MT5 IPC to fail.
	_ = os.Remove(fmt.Sprintf("/tmp/.X%d-lock", displayNum))
	_ = os.Remove(fmt.Sprintf("/tmp/.X11-unix/X%d", displayNum))

	xvfbCmd := exec.Command("Xvfb",
		workerDisplay,
		"-screen", "0", "1280x800x24",
		"-nolisten", "tcp", "-nolisten", "inet6",
	)
	xvfbCmd.Stdout = os.Stdout
	xvfbCmd.Stderr = os.Stderr
	if err := xvfbCmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start Xvfb for %s: %w", id, err)
	}
	log.Printf("[start] Xvfb PID %d for %s on display %s", xvfbCmd.Process.Pid, id, workerDisplay)
	time.Sleep(time.Second) // give display a moment to initialise

	// ── Tiling window manager (ratpoison) ─────────────────────────────────────
	wmCmd := exec.Command("ratpoison")
	wmCmd.Env = append(os.Environ(), "DISPLAY="+workerDisplay)
	wmCmd.Stdout = os.Stdout
	wmCmd.Stderr = os.Stderr
	if err := wmCmd.Start(); err != nil {
		log.Printf("[start] Warning: ratpoison failed for %s: %v", id, err)
	} else {
		log.Printf("[start] ratpoison PID %d for %s on display %s", wmCmd.Process.Pid, id, workerDisplay)
	}

	// ── Per-worker wine environment ────────────────────────────────────────────
	env := append(os.Environ(),
		"WINEPREFIX="+winePrefix,
		"DISPLAY="+workerDisplay,
		"WINEDEBUG=-all",
	)

	// ── MT5 terminal ───────────────────────────────────────────────────────────
	terminalExe := filepath.Join(workerDir, "terminal64.exe")
	tOut, _ := os.OpenFile(filepath.Join(logsDir, "terminal.stdout.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	tErr, _ := os.OpenFile(filepath.Join(logsDir, "terminal.stderr.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)

	termCmd := exec.Command("wine", terminalExe, "/portable")
	termCmd.Dir = workerDir
	termCmd.Env = env
	termCmd.Stdout = tOut
	termCmd.Stderr = tErr
	if err := termCmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start terminal: %w", err)
	}
	_ = tOut.Close()
	_ = tErr.Close()
	log.Printf("[start] MT5 terminal PID %d for %s", termCmd.Process.Pid, id)

	// ── RPyC server ───────────────────────────────────────────────────────────
	// Use pythonw.exe (WINDOWS subsystem): Wine will NOT allocate a console for
	// it, so inherited pipe handles are passed through intact and Python's
	// init_sys_streams can wrap them without hitting WinError 6.
	// python.exe (CONSOLE subsystem) triggers Wine console allocation which
	// produces INVALID_HANDLE_VALUE handles → WinError 6 before Python starts.
	rPipeR, rPipeW, _ := os.Pipe()
	ePipeR, ePipeW, _ := os.Pipe()

	// Goroutines: drain pipes into log files so we don't block the child.
	go func() {
		defer rPipeR.Close()
		if dst, err := os.OpenFile(filepath.Join(logsDir, "rpyc.stdout.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			defer dst.Close()
			io.Copy(dst, rPipeR) //nolint:errcheck
		}
	}()
	go func() {
		defer ePipeR.Close()
		if dst, err := os.OpenFile(filepath.Join(logsDir, "rpyc.stderr.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			defer dst.Close()
			io.Copy(dst, ePipeR) //nolint:errcheck
		}
	}()

	// Wine path for the RPyC log file (Z: maps Linux root → used by Wine Python)
	winLogFile := fmt.Sprintf(`Z:\mt5-fleet\workers\%s\logs\rpyc.log`, id)

	rpycCmd := exec.Command("wine", winPython, rpycScript,
		"--port", fmt.Sprintf("%d", w.Port),
		fmt.Sprintf("--token=%s", w.Token),
		"--mt5-path", winMT5Path,
		"--log-file", winLogFile,
	)
	rpycCmd.Env = append(env, "PYTHONUNBUFFERED=1")
	rpycCmd.Stdout = rPipeW
	rpycCmd.Stderr = ePipeW
	if err := rpycCmd.Start(); err != nil {
		// Terminal is running but RPyC failed — kill the terminal too
		_ = termCmd.Process.Kill()
		_ = rPipeW.Close()
		_ = ePipeW.Close()
		return nil, fmt.Errorf("failed to start RPyC server: %w", err)
	}
	// Close write ends in the parent — goroutines read until EOF when child exits.
	_ = rPipeW.Close()
	_ = ePipeW.Close()
	log.Printf("[start] RPyC server PID %d for %s on port %d", rpycCmd.Process.Pid, id, w.Port)

	// ── x11vnc (VNC server for this worker's display) ─────────────────────────
	vncLogFile := filepath.Join(logsDir, "x11vnc.log")
	x11vncCmd := exec.Command("x11vnc",
		"-display", workerDisplay,
		"-rfbport", fmt.Sprintf("%d", vncInternalPort),
		"-nopw", "-forever", "-shared",
		"-noxdamage", "-q",
		"-o", vncLogFile,
	)
	x11vncCmd.Stdout = os.Stdout
	x11vncCmd.Stderr = os.Stderr
	if err := x11vncCmd.Start(); err != nil {
		log.Printf("[start] Warning: x11vnc failed for %s: %v", id, err)
	} else {
		log.Printf("[start] x11vnc PID %d for %s on rfbport %d", x11vncCmd.Process.Pid, id, vncInternalPort)
	}

	// ── websockify (WebSocket → VNC proxy) ────────────────────────────────────
	wsCmd := exec.Command("websockify",
		fmt.Sprintf("0.0.0.0:%d", wsContainerPort),
		fmt.Sprintf("localhost:%d", vncInternalPort),
	)
	wsCmd.Stdout = os.Stdout
	wsCmd.Stderr = os.Stderr
	if err := wsCmd.Start(); err != nil {
		log.Printf("[start] Warning: websockify failed for %s: %v", id, err)
	} else {
		log.Printf("[start] websockify PID %d for %s on port %d", wsCmd.Process.Pid, id, wsContainerPort)
	}

	w.PIDTerminal = termCmd.Process.Pid
	w.PIDRPyC = rpycCmd.Process.Pid
	if xvfbCmd.Process != nil {
		w.PIDXvfb = xvfbCmd.Process.Pid
	}
	if x11vncCmd.Process != nil {
		w.PIDx11vnc = x11vncCmd.Process.Pid
	}
	if wsCmd.Process != nil {
		w.PIDWsockify = wsCmd.Process.Pid
	}
	w.VNCWSPort = (w.Port - basePort) + vncWSHostBase
	w.Status = StatusStarting

	if err := save(workers); err != nil {
		return nil, err
	}
	return w, nil
}

func reconcileWorkers() {
	workers, err := ListWorkers()
	if err != nil {
		log.Printf("[supervisor] list workers failed: %v", err)
		return
	}

	for _, w := range workers {
		switch w.Status {
		case StatusRunning, StatusStarting, StatusStopping:
			continue
		}

		log.Printf("[supervisor] Ensuring worker %s is running (status=%s)", w.ID, w.Status)
		if _, err := StartWorker(w.ID); err != nil {
			log.Printf("[supervisor] StartWorker(%s) failed: %v", w.ID, err)
		}
	}
}

func startWorkerSupervisor() {
	go func() {
		reconcileWorkers()

		ticker := time.NewTicker(workerSupervisorInterval)
		defer ticker.Stop()

		for range ticker.C {
			reconcileWorkers()
		}
	}()
}

// WaitForRPyCReady waits until the worker's RPyC TCP port accepts connections.
// This is a pragmatic readiness signal that avoids false negatives from
// protocol-level timing during early process startup.
func WaitForRPyCReady(id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		mu.Lock()
		workers, err := load()
		if err != nil {
			mu.Unlock()
			return err
		}
		w, ok := workers[id]
		if !ok {
			mu.Unlock()
			return fmt.Errorf("worker %s not found", id)
		}
		port := w.Port
		status := deriveStatus(w)
		mu.Unlock()

		if status != StatusRunning {
			time.Sleep(2 * time.Second)
			continue
		}

		addr := fmt.Sprintf("127.0.0.1:%d", port)
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		_ = conn.Close()
		return nil
	}

	return fmt.Errorf("timeout waiting for RPyC readiness for %s", id)
}

func StopWorker(id string) (*Worker, error) {
	mu.Lock()
	defer mu.Unlock()

	workers, err := load()
	if err != nil {
		return nil, err
	}
	w, ok := workers[id]
	if !ok {
		return nil, nil
	}

	stopProcs(w)

	w.PIDTerminal = 0
	w.PIDRPyC = 0
	w.PIDXvfb = 0
	w.PIDx11vnc = 0
	w.PIDWsockify = 0
	w.Status = StatusStopped
	if err := save(workers); err != nil {
		return nil, err
	}
	return w, nil
}

// stopProcs terminates both sub-processes for a worker (RPyC first, then terminal).
// Must be called with mu held.
func stopProcs(w *Worker) {
	killPID(w.PIDRPyC, "rpyc")
	killPID(w.PIDTerminal, "terminal")
	killPID(w.PIDWsockify, "websockify")
	killPID(w.PIDx11vnc, "x11vnc")
	killPID(w.PIDXvfb, "xvfb")
}

func UpdateConfig(id string, config MT5Config) (*Worker, error) {
	mu.Lock()
	defer mu.Unlock()

	workers, err := load()
	if err != nil {
		return nil, err
	}
	w, ok := workers[id]
	if !ok {
		return nil, nil
	}
	cfg := config
	w.Config = &cfg
	if err := save(workers); err != nil {
		return nil, err
	}
	return w, nil
}

func RotateToken(id string) (*Worker, error) {
	mu.Lock()
	defer mu.Unlock()

	workers, err := load()
	if err != nil {
		return nil, err
	}
	w, ok := workers[id]
	if !ok {
		return nil, nil
	}
	w.Token = newToken()
	if err := save(workers); err != nil {
		return nil, err
	}
	return w, nil
}

func RenameWorker(id, name string) (*Worker, error) {
	mu.Lock()
	defer mu.Unlock()

	workers, err := load()
	if err != nil {
		return nil, err
	}
	w, ok := workers[id]
	if !ok {
		return nil, nil
	}
	if name != "" {
		w.Name = name
	}
	if err := save(workers); err != nil {
		return nil, err
	}
	return w, nil
}

// GetWorkerLogs returns the last ~200 lines from the worker's log files.
func GetWorkerLogs(id string) []string {
	logDir := filepath.Join(workersDir, id, "logs")
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return []string{"[logs] waiting for output..."}
	}

	logFiles := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(strings.ToLower(name), ".log") {
			logFiles = append(logFiles, name)
		}
	}
	if len(logFiles) == 0 {
		return []string{"[logs] no .log files found"}
	}

	sort.Strings(logFiles)
	if len(logFiles) > 8 {
		logFiles = logFiles[len(logFiles)-8:]
	}

	out := make([]string, 0, 400)
	for _, fname := range logFiles {
		data, err := os.ReadFile(filepath.Join(logDir, fname))
		if err != nil {
			continue
		}
		text := strings.ReplaceAll(string(data), "\x00", "")
		trimmed := strings.TrimRight(text, "\n")
		if trimmed == "" {
			continue
		}
		lines := strings.Split(trimmed, "\n")
		if len(lines) > 80 {
			lines = lines[len(lines)-80:]
		}
		for _, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			out = append(out, "["+fname+"] "+line)
		}
	}
	if len(out) > 400 {
		out = out[len(out)-400:]
	}
	if len(out) == 0 {
		return []string{"[logs] waiting for output..."}
	}
	return out
}
