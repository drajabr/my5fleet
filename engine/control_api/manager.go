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
	"time"
)

// ── Types ──────────────────────────────────────────────────────────────────────

type TerminalStatus string

const (
	StatusStopped  TerminalStatus = "stopped"
	StatusStarting TerminalStatus = "starting"
	StatusRunning  TerminalStatus = "running"
	StatusStopping TerminalStatus = "stopping"
	StatusError    TerminalStatus = "error"
)

type MT5Config struct {
	Login    int    `json:"login"`
	Password string `json:"password"`
	Server   string `json:"server"`
}

type Terminal struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Status      TerminalStatus `json:"status"`
	LastError   string         `json:"last_error,omitempty"`
	KeepAlive   *bool          `json:"keep_alive,omitempty"`
	Port        int            `json:"port"`
	Token       string         `json:"token"`
	Config      *MT5Config     `json:"config,omitempty"`
	PIDTerminal int            `json:"pid_terminal,omitempty"`
	PIDRPyC     int            `json:"pid_rpyc,omitempty"`
	PIDWM       int            `json:"pid_wm,omitempty"`
	VNCWSPort   int            `json:"vnc_ws_port,omitempty"`
	PIDXvnc     int            `json:"pid_xvnc,omitempty"`
	PIDXvfb     int            `json:"pid_xvfb,omitempty"`
	PIDx11vnc   int            `json:"pid_x11vnc,omitempty"`
	PIDWsockify int            `json:"pid_wsockify,omitempty"`
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
	vncRFBBase     = 5900  // container-local Xvnc RFB port base
	vncDisplayBase = 100   // Xvnc display number base

	writableDirs = []string{"MQL5", "logs", "config", "tester", "bases", "profiles"}

	mu              sync.Mutex // guards all workers.json reads/writes
	cachedTerminals map[string]*Terminal
	cachedPath      string
	startMu         sync.Mutex
	startInFlight   = make(map[string]struct{})
)

func tryStartTerminal(id string) bool {
	startMu.Lock()
	defer startMu.Unlock()
	if _, exists := startInFlight[id]; exists {
		return false
	}
	startInFlight[id] = struct{}{}
	return true
}

func doneStartTerminal(id string) {
	startMu.Lock()
	delete(startInFlight, id)
	startMu.Unlock()
}

const workerSupervisorInterval = 10 * time.Second

func initPaths() {
	fleetDir = envOr("FLEET_DIR", "/my5fleet")
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

func load() (map[string]*Terminal, error) {
	if cachedTerminals != nil && cachedPath == workersJSON {
		return cachedTerminals, nil
	}
	data, err := os.ReadFile(workersJSON)
	if os.IsNotExist(err) {
		w := make(map[string]*Terminal)
		cachedTerminals = w
		cachedPath = workersJSON
		return w, nil
	}
	if err != nil {
		return nil, err
	}
	workers := make(map[string]*Terminal)
	if err := json.Unmarshal(data, &workers); err != nil {
		return nil, err
	}
	cachedTerminals = workers
	cachedPath = workersJSON
	return workers, nil
}

func save(workers map[string]*Terminal) error {
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
	if err := os.Rename(tmp, workersJSON); err != nil {
		return err
	}
	cachedTerminals = workers
	cachedPath = workersJSON
	return nil
}

// ── Allocation helpers ─────────────────────────────────────────────────────────

func nextID(workers map[string]*Terminal) string {
	for i := 1; ; i++ {
		id := fmt.Sprintf("terminal_%d", i)
		if _, exists := workers[id]; !exists {
			return id
		}
	}
}

func nextPort(workers map[string]*Terminal) int {
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
	return processExists(pid)
}

// pidMatchesTerminal checks /proc/<pid>/cmdline to verify the process belongs to
// the expected worker. This prevents false positives from PID reuse after a
// container restart.
func pidMatchesTerminal(pid int, workerID string) bool {
	if pid <= 0 {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	return strings.Contains(string(data), workerID)
}

func deriveStatus(w *Terminal) TerminalStatus {
	aliveT := pidAlive(w.PIDTerminal) && pidMatchesTerminal(w.PIDTerminal, w.ID)
	aliveR := pidAlive(w.PIDRPyC) && pidMatchesTerminal(w.PIDRPyC, w.ID)
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
			w.PIDWM = 0
			w.PIDXvnc = 0
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

// killPID sends SIGTERM, waits up to 8 s, then sends SIGKILL.
func killPID(pid int, label string) {
	if !pidAlive(pid) {
		return
	}
	log.Printf("[%s] SIGTERM → PID %d", label, pid)
	_ = terminateProcess(pid)
	for i := 0; i < 8; i++ {
		time.Sleep(time.Second)
		if !pidAlive(pid) {
			return
		}
	}
	log.Printf("[%s] SIGKILL → PID %d", label, pid)
	_ = forceKillProcess(pid)
}

// ── Filesystem helpers ─────────────────────────────────────────────────────────

// copyDir recursively copies src directory tree into dst.
func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func createFS(workerID string) error {
	workerDir := filepath.Join(workersDir, workerID)
	if _, err := os.Stat(workerDir); err == nil {
		log.Printf("[createFS] Removing stale worker directory: %s", workerDir)
		if err := os.RemoveAll(workerDir); err != nil {
			return fmt.Errorf("failed to remove stale worker directory %s: %w", workerDir, err)
		}
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

	// Copy writable directories from reference (or create empty if not present yet)
	// This propagates EAs, broker configs, profiles etc. to each new worker.
	for _, d := range writableDirs {
		srcDir := filepath.Join(referenceDir, d)
		dstDir := filepath.Join(workerDir, d)
		if _, err := os.Stat(srcDir); err == nil {
			if err := copyDir(srcDir, dstDir); err != nil {
				log.Printf("[createFS] Warning: failed to copy %s from reference: %v; using empty dir", d, err)
				_ = os.MkdirAll(dstDir, 0o755)
			}
		} else {
			_ = os.MkdirAll(dstDir, 0o755)
		}
	}

	log.Printf("[createFS] Terminal filesystem ready: %s", workerDir)
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

func boolPtr(v bool) *bool {
	b := v
	return &b
}

func keepAliveEnabled(w *Terminal) bool {
	if w == nil || w.KeepAlive == nil {
		// Backward-compatible default for old workers without this field.
		return true
	}
	return *w.KeepAlive
}

func normalizeTerminalName(name string) string {
	return strings.TrimSpace(name)
}

func terminalNameExists(workers map[string]*Terminal, exceptID string, name string) bool {
	want := strings.ToLower(strings.TrimSpace(name))
	if want == "" {
		return false
	}
	for id, w := range workers {
		if id == exceptID || w == nil {
			continue
		}
		if strings.ToLower(strings.TrimSpace(w.Name)) == want {
			return true
		}
	}
	return false
}

// ── Public manager operations ──────────────────────────────────────────────────

func ListTerminals() ([]*Terminal, error) {
	mu.Lock()
	defer mu.Unlock()

	workers, err := load()
	if err != nil {
		return nil, err
	}

	changed := false
	list := make([]*Terminal, 0, len(workers))
	for _, w := range workers {
		if w.KeepAlive == nil {
			w.KeepAlive = boolPtr(true)
			changed = true
		}

		live := deriveStatus(w)
		// Sync transient statuses once the processes settle
		if w.Status != live &&
			(w.Status == StatusStarting || w.Status == StatusStopping ||
				w.Status == StatusRunning || w.Status == StatusError) {
			w.Status = live
			changed = true
		}
		if live != StatusError && w.LastError != "" {
			w.LastError = ""
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

func GetTerminal(id string) (*Terminal, error) {
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
	live := deriveStatus(w)
	w.Status = live
	if live != StatusError && w.LastError != "" {
		w.LastError = ""
		_ = save(workers)
	}
	return w, nil
}

func SetTerminalError(id, reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "terminal start failed"
	}

	mu.Lock()
	defer mu.Unlock()

	workers, err := load()
	if err != nil {
		return
	}
	w, ok := workers[id]
	if !ok {
		return
	}
	w.Status = StatusError
	w.LastError = reason
	_ = save(workers)
}

func CreateTerminal(name string, token string, config *MT5Config) (*Terminal, error) {
	mu.Lock()
	workers, err := load()
	if err != nil {
		mu.Unlock()
		return nil, err
	}

	id := nextID(workers)
	port := nextPort(workers)

	name = normalizeTerminalName(name)
	if name == "" {
		name = id
	}
	if terminalNameExists(workers, "", name) {
		mu.Unlock()
		return nil, fmt.Errorf("terminal name %q already exists", name)
	}
	if token == "" {
		token = newToken()
	}

	w := &Terminal{
		ID:        id,
		Name:      name,
		Status:    StatusStopped,
		KeepAlive: boolPtr(true),
		Port:      port,
		Token:     token,
		Config:    config,
		VNCWSPort: (port - basePort) + vncWSHostBase,
	}
	workers[id] = w
	if err := save(workers); err != nil {
		mu.Unlock()
		return nil, err
	}
	mu.Unlock()

	if err := createFS(id); err != nil {
		mu.Lock()
		defer mu.Unlock()
		if ww, loadErr := load(); loadErr == nil {
			delete(ww, id)
			_ = save(ww)
		}
		return nil, err
	}

	log.Printf("[create] Terminal %s created on port %d", id, port)
	return w, nil
}

func DeleteTerminal(id string) error {
	mu.Lock()
	workers, err := load()
	if err != nil {
		mu.Unlock()
		return err
	}
	w, ok := workers[id]
	if !ok {
		mu.Unlock()
		return nil // idempotent
	}
	snap := *w
	mu.Unlock()

	// Stop if running
	if deriveStatus(&snap) != StatusStopped {
		stopProcs(&snap)
	}

	if err := removeFS(id); err != nil {
		return err
	}

	mu.Lock()
	defer mu.Unlock()
	workers, err = load()
	if err != nil {
		return err
	}
	if existing, exists := workers[id]; exists {
		snap.Name = existing.Name
		delete(workers, id)
		if err := save(workers); err != nil {
			return err
		}
	}
	log.Printf("[delete] Terminal %s (%s) deleted", id, snap.Name)
	return nil
}

func StartTerminal(id string) (*Terminal, error) {
	if !tryStartTerminal(id) {
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
		cp := *w
		return &cp, nil
	}
	defer doneStartTerminal(id)

	// ── Phase 1: read state + mark "starting" under lock ───────────────────────
	mu.Lock()
	workers, err := load()
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	w, ok := workers[id]
	if !ok {
		mu.Unlock()
		return nil, nil
	}

	current := deriveStatus(w)
	if current == StatusRunning {
		cp := *w
		mu.Unlock()
		return &cp, nil
	}
	if w.Status == StatusStopping || w.Status == StatusStarting {
		cp := *w
		mu.Unlock()
		return &cp, nil
	}

	if current == StatusError {
		log.Printf("[start] Terminal %s unhealthy; cleaning up stale processes before restart", id)
		stopProcs(w)
	}

	// Snapshot values needed for spawning
	port := w.Port
	token := w.Token

	w.PIDTerminal = 0
	w.PIDRPyC = 0
	w.PIDWM = 0
	w.PIDXvnc = 0
	w.PIDXvfb = 0
	w.PIDx11vnc = 0
	w.PIDWsockify = 0
	w.Status = StatusStarting
	w.LastError = ""
	w.KeepAlive = boolPtr(true)
	w.VNCWSPort = (port - basePort) + vncWSHostBase

	if err := save(workers); err != nil {
		mu.Unlock()
		return nil, err
	}
	mu.Unlock()

	// ── Phase 2: spawn all processes WITHOUT holding the lock ──────────────────
	// On failure, set status to error so the supervisor can retry.
	setError := func(reason string) {
		reason = strings.TrimSpace(reason)
		if reason == "" {
			reason = "terminal start failed"
		}
		mu.Lock()
		defer mu.Unlock()
		if ww, err := load(); err == nil {
			if wk, ok := ww[id]; ok {
				wk.Status = StatusError
				wk.LastError = reason
				_ = save(ww)
			}
		}
	}

	workerDir := filepath.Join(workersDir, id)
	logsDir := filepath.Join(workerDir, "logs")
	winMT5Path := fmt.Sprintf(`Z:\my5fleet\workers\%s\terminal64.exe`, id)

	// ── Per-worker virtual display (Xvnc) ─────────────────────────────────────
	portOffset := port - basePort
	displayNum := vncDisplayBase + portOffset
	workerDisplay := fmt.Sprintf(":%d", displayNum)
	vncInternalPort := vncRFBBase + portOffset
	wsContainerPort := vncWSLocalBase + portOffset

	// Clean up any stale X lock/socket files left over from a previous
	// SIGKILL. X servers refuse to start if these files exist, silently breaking
	// the worker's display and causing MT5 IPC to fail.
	_ = os.Remove(fmt.Sprintf("/tmp/.X%d-lock", displayNum))
	_ = os.Remove(fmt.Sprintf("/tmp/.X11-unix/X%d", displayNum))

	xvncCmd := exec.Command("Xvnc",
		workerDisplay,
		"-geometry", "1280x800",
		"-depth", "24",
		"-rfbport", fmt.Sprintf("%d", vncInternalPort),
		"-SecurityTypes", "None",
		"-AcceptSetDesktopSize",
		"-AlwaysShared",
		"-nolisten", "tcp", "-nolisten", "inet6",
	)
	xvncCmd.Stdout = os.Stdout
	xvncCmd.Stderr = os.Stderr
	if err := xvncCmd.Start(); err != nil {
		setError(fmt.Sprintf("failed to start Xvnc: %v", err))
		return nil, fmt.Errorf("failed to start Xvnc for %s: %w", id, err)
	}
	log.Printf("[start] Xvnc PID %d for %s on display %s rfbport %d", xvncCmd.Process.Pid, id, workerDisplay, vncInternalPort)
	if err := waitForXvncReady(displayNum, vncInternalPort, 25*time.Second); err != nil {
		killPID(xvncCmd.Process.Pid, "xvnc")
		setError(fmt.Sprintf("Xvnc readiness failed: %v", err))
		return nil, fmt.Errorf("xvnc did not become ready for %s: %w", id, err)
	}
	_ = exec.Command("xsetroot", "-display", workerDisplay, "-cursor_name", "left_ptr").Run()

	// ── Window manager (bspwm) ──────────────────────────────────────────────────
	wmCmd := exec.Command("bspwm", "-c", "/opt/mt5/scripts/bspwmrc")
	wmCmd.Env = filteredEnv(
		"DISPLAY="+workerDisplay,
		"XDG_RUNTIME_DIR=/tmp",
	)
	wmCmd.Stdout = os.Stdout
	wmCmd.Stderr = os.Stderr
	if err := wmCmd.Start(); err != nil {
		log.Printf("[start] Warning: bspwm failed for %s: %v", id, err)
	} else {
		log.Printf("[start] bspwm PID %d for %s on display %s", wmCmd.Process.Pid, id, workerDisplay)
		if err := waitForWMReady(wmCmd.Process.Pid, 3*time.Second); err != nil {
			log.Printf("[start] Warning: bspwm readiness check for %s failed: %v", id, err)
		}
	}

	// ── Per-worker wine environment ────────────────────────────────────────────
	env := filteredEnv(
		"WINEPREFIX="+winePrefix,
		"DISPLAY="+workerDisplay,
		"WINEDEBUG=-all",
	)

	// ── MT5 terminal ───────────────────────────────────────────────────────────
	// MT5 runs directly on the X display, managed by bspwm.
	tOut, _ := os.OpenFile(filepath.Join(logsDir, "terminal.stdout.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	tErr, _ := os.OpenFile(filepath.Join(logsDir, "terminal.stderr.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)

	if _, err := os.Stat(workerDir); err != nil {
		killPID(xvncCmd.Process.Pid, "xvnc")
		if wmCmd.Process != nil {
			killPID(wmCmd.Process.Pid, "wm")
		}
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("worker %s deleted during start", id)
		}
		setError(fmt.Sprintf("worker directory check failed: %v", err))
		return nil, fmt.Errorf("worker dir check failed for %s: %w", id, err)
	}

	termCmd := exec.Command("wine", winMT5Path, "/portable")
	termCmd.Dir = workerDir
	termCmd.Env = env
	termCmd.Stdout = tOut
	termCmd.Stderr = tErr
	if err := termCmd.Start(); err != nil {
		killPID(xvncCmd.Process.Pid, "xvnc")
		if wmCmd.Process != nil {
			killPID(wmCmd.Process.Pid, "wm")
		}
		setError(fmt.Sprintf("failed to start MT5 terminal: %v", err))
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
	winLogFile := fmt.Sprintf(`Z:\my5fleet\workers\%s\logs\rpyc.log`, id)

	rpycCmd := exec.Command("wine", winPython, rpycScript,
		"--port", fmt.Sprintf("%d", port),
		fmt.Sprintf("--token=%s", token),
		"--mt5-path", winMT5Path,
		"--portable",
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
		killPID(xvncCmd.Process.Pid, "xvnc")
		if wmCmd.Process != nil {
			killPID(wmCmd.Process.Pid, "wm")
		}
		setError(fmt.Sprintf("failed to start RPyC server: %v", err))
		return nil, fmt.Errorf("failed to start RPyC server: %w", err)
	}
	// Close write ends in the parent — goroutines read until EOF when child exits.
	_ = rPipeW.Close()
	_ = ePipeW.Close()
	log.Printf("[start] RPyC server PID %d for %s on port %d", rpycCmd.Process.Pid, id, port)

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

	// ── Phase 3: record PIDs under lock ────────────────────────────────────────
	mu.Lock()
	workers, err = load()
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	w, ok = workers[id]
	if !ok {
		mu.Unlock()
		killPID(rpycCmd.Process.Pid, "rpyc")
		killPID(termCmd.Process.Pid, "terminal")
		if wsCmd.Process != nil {
			killPID(wsCmd.Process.Pid, "websockify")
		}
		if wmCmd.Process != nil {
			killPID(wmCmd.Process.Pid, "wm")
		}
		killPID(xvncCmd.Process.Pid, "xvnc")
		return nil, fmt.Errorf("worker %s deleted during start", id)
	}

	w.PIDTerminal = termCmd.Process.Pid
	w.PIDRPyC = rpycCmd.Process.Pid
	if wmCmd.Process != nil {
		w.PIDWM = wmCmd.Process.Pid
	}
	if xvncCmd.Process != nil {
		w.PIDXvnc = xvncCmd.Process.Pid
	}
	if wsCmd.Process != nil {
		w.PIDWsockify = wsCmd.Process.Pid
	}
	w.VNCWSPort = (port - basePort) + vncWSHostBase
	w.KeepAlive = boolPtr(true)
	w.Status = StatusStarting

	if err := save(workers); err != nil {
		mu.Unlock()
		return nil, err
	}
	mu.Unlock()
	return w, nil
}

func reconcileTerminals() {
	workers, err := ListTerminals()
	if err != nil {
		log.Printf("[supervisor] list workers failed: %v", err)
		return
	}

	for _, w := range workers {
		if !shouldSupervisorStart(w) {
			continue
		}

		log.Printf("[supervisor] Ensuring worker %s is running (status=%s)", w.ID, w.Status)
		if _, err := StartTerminal(w.ID); err != nil {
			log.Printf("[supervisor] StartTerminal(%s) failed: %v", w.ID, err)
		}
	}
}

func shouldSupervisorStart(w *Terminal) bool {
	if w == nil || !keepAliveEnabled(w) {
		return false
	}
	switch w.Status {
	case StatusRunning, StatusStarting, StatusStopping:
		return false
	default:
		return true
	}
}

func startTerminalSupervisor() {
	go func() {
		reconcileTerminals()

		ticker := time.NewTicker(workerSupervisorInterval)
		defer ticker.Stop()

		for range ticker.C {
			reconcileTerminals()
		}
	}()
}

// WaitForRPyCReady waits until the worker's RPyC TCP port accepts connections.
// This is a pragmatic readiness signal that avoids false negatives from
// protocol-level timing during early process startup.
func WaitForRPyCReady(id string, timeout time.Duration) error {
	// Snapshot port and PIDs once — they don't change while the worker exists.
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
	token := w.Token
	pidT := w.PIDTerminal
	pidR := w.PIDRPyC
	mu.Unlock()

	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for time.Now().Before(deadline) {
		aliveT := pidAlive(pidT)
		aliveR := pidAlive(pidR)

		// Fail fast with a precise cause. Previously we waited until timeout when
		// only one side died (most commonly RPyC), which obscured root causes.
		if !aliveR {
			if !aliveT {
				return fmt.Errorf("worker %s processes exited before RPyC became ready", id)
			}
			return fmt.Errorf("RPyC process for %s exited before readiness", id)
		}
		if !aliveT {
			return fmt.Errorf("terminal process for %s exited before readiness", id)
		}

		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			_ = conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
			_, writeErr := conn.Write([]byte(token + "\n"))
			_ = conn.Close()
			if writeErr == nil {
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("timeout waiting for RPyC readiness for %s", id)
}

func RefreshDisplay(id string) error {
	mu.Lock()
	workers, err := load()
	if err != nil {
		mu.Unlock()
		return err
	}
	w, ok := workers[id]
	if !ok {
		mu.Unlock()
		return fmt.Errorf("worker %q not found", id)
	}
	if deriveStatus(w) != StatusRunning {
		mu.Unlock()
		return fmt.Errorf("worker %q is not running", id)
	}
	displayNum := vncDisplayBase + (w.Port - basePort)
	mu.Unlock()

	display := fmt.Sprintf(":%d", displayNum)
	cmd := exec.Command("xrefresh", "-display", display)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("xrefresh on %s failed: %w", display, err)
	}
	log.Printf("[refresh] xrefresh on display %s for worker %s", display, id)
	return nil
}

func StopTerminal(id string) (*Terminal, error) {
	mu.Lock()
	workers, err := load()
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	w, ok := workers[id]
	if !ok {
		mu.Unlock()
		return nil, nil
	}
	snap := *w
	mu.Unlock()

	stopProcs(&snap)

	mu.Lock()
	defer mu.Unlock()
	workers, err = load()
	if err != nil {
		return nil, err
	}
	w, ok = workers[id]
	if !ok {
		return nil, nil
	}

	w.PIDTerminal = 0
	w.PIDRPyC = 0
	w.PIDWM = 0
	w.PIDXvnc = 0
	w.PIDXvfb = 0
	w.PIDx11vnc = 0
	w.PIDWsockify = 0
	w.KeepAlive = boolPtr(false)
	w.Status = StatusStopped
	w.LastError = ""
	if err := save(workers); err != nil {
		return nil, err
	}
	log.Printf("[stop] Terminal %s (%s) stopped", id, w.Name)
	cp := *w
	return &cp, nil
}

// stopProcs terminates all sub-processes for a worker.
func stopProcs(w *Terminal) {
	killPID(w.PIDRPyC, "rpyc")
	killPID(w.PIDTerminal, "terminal")
	killPID(w.PIDWsockify, "websockify")
	killPID(w.PIDWM, "wm")
	killPID(w.PIDXvnc, "xvnc")
}

func waitForXvncReady(displayNum, rfbPort int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	lockFile := fmt.Sprintf("/tmp/.X%d-lock", displayNum)
	unixSock := fmt.Sprintf("/tmp/.X11-unix/X%d", displayNum)
	rfbAddr := fmt.Sprintf("127.0.0.1:%d", rfbPort)

	for time.Now().Before(deadline) {
		_, lockErr := os.Stat(lockFile)
		_, sockErr := os.Stat(unixSock)
		if lockErr == nil && sockErr == nil {
			conn, err := net.DialTimeout("tcp", rfbAddr, 600*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for X display :%d and RFB port %d", displayNum, rfbPort)
}

func waitForWMReady(pid int, timeout time.Duration) error {
	if pid <= 0 {
		return fmt.Errorf("invalid wm pid: %d", pid)
	}

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return fmt.Errorf("wm pid %d exited before becoming ready", pid)
		}

		// Lightweight WMs usually don't expose a readiness file; if still alive
		// after a short stabilization period, treat as ready.
		if time.Now().After(deadline.Add(-500 * time.Millisecond)) {
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for wm pid %d", pid)
}

func UpdateConfig(id string, config MT5Config) (*Terminal, error) {
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
	log.Printf("[config] Terminal %s (%s) configuration updated", id, w.Name)
	return w, nil
}

func RotateToken(id string) (*Terminal, error) {
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
	log.Printf("[token] Terminal %s (%s) token rotated", id, w.Name)
	return w, nil
}

func RenameTerminal(id, name string) (*Terminal, error) {
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
	name = normalizeTerminalName(name)
	if name == "" {
		name = w.Name
	}
	if terminalNameExists(workers, id, name) {
		return nil, fmt.Errorf("terminal name %q already exists", name)
	}
	old := w.Name
	w.Name = name
	if err := save(workers); err != nil {
		return nil, err
	}
	log.Printf("[rename] Terminal %s renamed: %s → %s", id, old, w.Name)
	return w, nil
}

// GetTerminalLogs returns the last ~200 lines from the worker's log files.
func GetTerminalLogs(id string) []string {
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
		lines := tailFile(filepath.Join(logDir, fname), 80)
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

// tailFile returns the last n non-empty lines from a file by seeking from the end.
func tailFile(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return nil
	}

	// Read at most 64KB from the end; enough for 80 lines in most cases.
	const maxTail = 64 * 1024
	size := fi.Size()
	readSize := size
	if readSize > maxTail {
		readSize = maxTail
	}
	buf := make([]byte, readSize)
	_, err = f.ReadAt(buf, size-readSize)
	if err != nil && err != io.EOF {
		return nil
	}

	text := strings.ReplaceAll(string(buf), "\x00", "")
	trimmed := strings.TrimRight(text, "\n")
	if trimmed == "" {
		return nil
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
