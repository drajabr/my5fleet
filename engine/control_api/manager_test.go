package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetWorkerLogs_ReadsAllLogFilesAndStripsNul(t *testing.T) {
	oldWorkersDir := workersDir
	t.Cleanup(func() { workersDir = oldWorkersDir })

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

	out := GetWorkerLogs("terminal_9")
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

func TestGetWorkerLogs_NoLogDirReturnsWaitingMessage(t *testing.T) {
	oldWorkersDir := workersDir
	t.Cleanup(func() { workersDir = oldWorkersDir })

	workersDir = t.TempDir()
	out := GetWorkerLogs("missing-worker")

	if len(out) != 1 {
		t.Fatalf("expected one informational line, got %d", len(out))
	}
	if out[0] != "[logs] waiting for output..." {
		t.Fatalf("unexpected message: %q", out[0])
	}
}

// TestStopWorkerZerosAllVNCPIDs verifies that after StopWorker all five PIDs
// are zeroed in workers.json, not just PIDTerminal and PIDRPyC.
func TestStopWorkerZerosAllVNCPIDs(t *testing.T) {
	// Set up a temporary fleet directory with a workers.json containing
	// a stopped worker that has non-zero VNC PIDs.
	tmp := t.TempDir()
	oldWorkersDir := workersDir
	oldWorkersJSON := workersJSON
	oldFleetDir := fleetDir
	t.Cleanup(func() {
		workersDir = oldWorkersDir
		workersJSON = oldWorkersJSON
		fleetDir = oldFleetDir
	})

	fleetDir = tmp
	workersDir = filepath.Join(tmp, "workers")
	workersJSON = filepath.Join(tmp, "config", "workers.json")

	if err := os.MkdirAll(filepath.Join(tmp, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Create worker directory so StopWorker can find the worker
	workerDir := filepath.Join(workersDir, "terminal_1")
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a workers.json with all PIDs set to 99999 (non-existent, so
	// pidAlive returns false and stopProcs is a no-op, but PIDs should be zeroed).
	initial := map[string]*Worker{
		"terminal_1": {
			ID:          "terminal_1",
			Name:        "test",
			Status:      StatusStopped,
			Port:        18812,
			Token:       "tok",
			PIDTerminal: 99999,
			PIDRPyC:     99998,
			PIDXvfb:     99997,
			PIDx11vnc:   99996,
			PIDWsockify: 99995,
			VNCWSPort:   19000,
		},
	}
	data, _ := json.MarshalIndent(initial, "", "  ")
	if err := os.WriteFile(workersJSON, data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := StopWorker("terminal_1")
	if err != nil {
		t.Fatalf("StopWorker returned error: %v", err)
	}

	// Read back workers.json and verify all PIDs are 0.
	raw, err := os.ReadFile(workersJSON)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]*Worker
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
		"PIDXvfb":     w.PIDXvfb,
		"PIDx11vnc":   w.PIDx11vnc,
		"PIDWsockify": w.PIDWsockify,
	} {
		if val != 0 {
			t.Errorf("expected %s == 0 after StopWorker, got %d", field, val)
		}
	}
	if w.Status != StatusStopped {
		t.Errorf("expected status %q, got %q", StatusStopped, w.Status)
	}
}
