package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGetTerminalLogs_ReadsAllLogFilesAndStripsNul(t *testing.T) {
	oldTerminalsDir := workersDir
	t.Cleanup(func() { workersDir = oldTerminalsDir })

	tmp := t.TempDir()
	workersDir = tmp
	logDir := filepath.Join(tmp, "terminal_9", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(logDir, "20260415.log"), []byte("A\x00B\nC\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "terminal.stderr.log"), []byte("err line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "notes.txt"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := GetTerminalLogs("terminal_9")
	joined := strings.Join(out, "\n")

	if len(out) == 0 {
		t.Fatalf("expected log lines, got empty")
	}
	if strings.Contains(joined, "\x00") {
		t.Fatalf("expected NUL bytes stripped, got %q", joined)
	}
	if !strings.Contains(joined, "[20260415.log] AB") {
		t.Fatalf("expected dated log file to be included, got %q", joined)
	}
	if !strings.Contains(joined, "[terminal.stderr.log] err line") {
		t.Fatalf("expected terminal stderr line, got %q", joined)
	}
	if strings.Contains(joined, "notes.txt") {
		t.Fatalf("expected non-.log files to be ignored, got %q", joined)
	}
}

func TestGetTerminalLogs_NoLogDirReturnsWaitingMessage(t *testing.T) {
	oldTerminalsDir := workersDir
	t.Cleanup(func() { workersDir = oldTerminalsDir })

	workersDir = t.TempDir()
	out := GetTerminalLogs("missing-worker")

	if len(out) != 1 {
		t.Fatalf("expected one informational line, got %d", len(out))
	}
	if out[0] != "[logs] waiting for output..." {
		t.Fatalf("unexpected message: %q", out[0])
	}
}

// TestStopTerminalZerosAllVNCPIDs verifies that after StopTerminal all five PIDs
// are zeroed in workers.json, not just PIDTerminal and PIDRPyC.
func TestStopTerminalZerosAllVNCPIDs(t *testing.T) {
	// Set up a temporary fleet directory with a workers.json containing
	// a stopped worker that has non-zero VNC PIDs.
	tmp := t.TempDir()
	oldTerminalsDir := workersDir
	oldTerminalsJSON := workersJSON
	oldFleetDir := fleetDir
	t.Cleanup(func() {
		workersDir = oldTerminalsDir
		workersJSON = oldTerminalsJSON
		fleetDir = oldFleetDir
	})

	fleetDir = tmp
	workersDir = filepath.Join(tmp, "workers")
	workersJSON = filepath.Join(tmp, "config", "workers.json")

	if err := os.MkdirAll(filepath.Join(tmp, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Create worker directory so StopTerminal can find the worker
	workerDir := filepath.Join(workersDir, "terminal_1")
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a workers.json with all PIDs set to 99999 (non-existent, so
	// pidAlive returns false and stopProcs is a no-op, but PIDs should be zeroed).
	initial := map[string]*Terminal{
		"terminal_1": {
			ID:          "terminal_1",
			Name:        "test",
			Status:      StatusStopped,
			Port:        18812,
			Token:       "tok",
			PIDTerminal: 99999,
			PIDRPyC:     99998,
			PIDXvnc:     99994,
			PIDWsockify: 99995,
			VNCWSPort:   19000,
		},
	}
	data, _ := json.MarshalIndent(initial, "", "  ")
	if err := os.WriteFile(workersJSON, data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := StopTerminal("terminal_1")
	if err != nil {
		t.Fatalf("StopTerminal returned error: %v", err)
	}

	// Read back workers.json and verify all PIDs are 0.
	raw, err := os.ReadFile(workersJSON)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]*Terminal
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	w := saved["terminal_1"]
	if w == nil {
		t.Fatal("worker not found in saved JSON")
	}
	for field, val := range map[string]int{
		"PIDTerminal": w.PIDTerminal,
		"PIDRPyC":     w.PIDRPyC,
		"PIDXvnc":     w.PIDXvnc,
		"PIDWsockify": w.PIDWsockify,
	} {
		if val != 0 {
			t.Errorf("expected %s == 0 after StopTerminal, got %d", field, val)
		}
	}
	if w.Status != StatusStopped {
		t.Errorf("expected status %q, got %q", StatusStopped, w.Status)
	}
	if keepAliveEnabled(w) {
		t.Errorf("expected keepalive disabled after StopTerminal")
	}
}

func TestShouldSupervisorStart(t *testing.T) {
	truePtr := boolPtr(true)
	falsePtr := boolPtr(false)

	tests := []struct {
		name string
		w    *Terminal
		want bool
	}{
		{name: "nil worker", w: nil, want: false},
		{name: "running keepalive", w: &Terminal{Status: StatusRunning, KeepAlive: truePtr}, want: false},
		{name: "starting keepalive", w: &Terminal{Status: StatusStarting, KeepAlive: truePtr}, want: false},
		{name: "stopping keepalive", w: &Terminal{Status: StatusStopping, KeepAlive: truePtr}, want: false},
		{name: "stopped keepalive", w: &Terminal{Status: StatusStopped, KeepAlive: truePtr}, want: true},
		{name: "error keepalive", w: &Terminal{Status: StatusError, KeepAlive: truePtr}, want: true},
		{name: "unknown keepalive", w: &Terminal{Status: TerminalStatus(""), KeepAlive: truePtr}, want: true},
		{name: "stopped keepalive false", w: &Terminal{Status: StatusStopped, KeepAlive: falsePtr}, want: false},
		{name: "stopped keepalive missing defaults true", w: &Terminal{Status: StatusStopped, KeepAlive: nil}, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSupervisorStart(tc.w); got != tc.want {
				t.Fatalf("shouldSupervisorStart() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRenameTerminalRejectsDuplicateNameCaseInsensitive(t *testing.T) {
	tmp := t.TempDir()
	oldWorkersJSON := workersJSON
	oldCache := cachedTerminals
	oldCachePath := cachedPath
	t.Cleanup(func() {
		workersJSON = oldWorkersJSON
		cachedTerminals = oldCache
		cachedPath = oldCachePath
	})

	workersJSON = filepath.Join(tmp, "workers.json")
	cachedTerminals = nil
	cachedPath = ""

	seed := map[string]*Terminal{
		"terminal_1": {ID: "terminal_1", Name: "Alpha", Port: 18812, Token: "a"},
		"terminal_2": {ID: "terminal_2", Name: "Beta", Port: 18813, Token: "b"},
	}
	raw, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(workersJSON, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := RenameTerminal("terminal_2", "  alpha  ")
	if err == nil {
		t.Fatalf("expected duplicate-name error, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		t.Fatalf("expected conflict-like error, got: %v", err)
	}

	outRaw, err := os.ReadFile(workersJSON)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]*Terminal
	if err := json.Unmarshal(outRaw, &out); err != nil {
		t.Fatal(err)
	}
	if out["terminal_2"].Name != "Beta" {
		t.Fatalf("expected original name to be preserved, got %q", out["terminal_2"].Name)
	}
}

func TestRenameTerminalTrimsWhitespace(t *testing.T) {
	tmp := t.TempDir()
	oldWorkersJSON := workersJSON
	oldCache := cachedTerminals
	oldCachePath := cachedPath
	t.Cleanup(func() {
		workersJSON = oldWorkersJSON
		cachedTerminals = oldCache
		cachedPath = oldCachePath
	})

	workersJSON = filepath.Join(tmp, "workers.json")
	cachedTerminals = nil
	cachedPath = ""

	seed := map[string]*Terminal{
		"terminal_1": {ID: "terminal_1", Name: "Alpha", Port: 18812, Token: "a"},
	}
	raw, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(workersJSON, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := RenameTerminal("terminal_1", "  New Name  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w == nil || w.Name != "New Name" {
		t.Fatalf("expected trimmed name New Name, got %#v", w)
	}
}

func TestWaitForRPyCReady_FailsFastWhenRPyCExited(t *testing.T) {
	tmp := t.TempDir()
	oldWorkersJSON := workersJSON
	oldCache := cachedTerminals
	oldCachePath := cachedPath
	t.Cleanup(func() {
		workersJSON = oldWorkersJSON
		cachedTerminals = oldCache
		cachedPath = oldCachePath
	})

	workersJSON = filepath.Join(tmp, "workers.json")
	cachedTerminals = nil
	cachedPath = ""

	seed := map[string]*Terminal{
		"terminal_1": {
			ID:          "terminal_1",
			Name:        "Alpha",
			Status:      StatusStarting,
			Port:        65530,
			Token:       "tok",
			PIDTerminal: os.Getpid(),
			PIDRPyC:     0,
		},
	}
	raw, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(workersJSON, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err := WaitForRPyCReady("terminal_1", 5*time.Second)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if processExists(os.Getpid()) {
		if !strings.Contains(err.Error(), "RPyC process") {
			t.Fatalf("expected RPyC-process failure, got: %v", err)
		}
	} else {
		if !strings.Contains(err.Error(), "processes exited") {
			t.Fatalf("expected generic process-exit failure on non-linux, got: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expected fast failure, took %v", elapsed)
	}
}
