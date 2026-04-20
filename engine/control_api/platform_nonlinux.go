//go:build !linux

package main

import "errors"

func processExists(pid int) bool {
	// Engine containers run on Linux. On non-Linux hosts this fallback keeps
	// local diagnostics and tooling builds working.
	return false
}

func terminateProcess(pid int) error {
	return nil
}

func forceKillProcess(pid int) error {
	return nil
}

func diskUsageBytes(path string) (totalBytes uint64, usedBytes uint64, err error) {
	return 0, 0, errors.New("disk usage unsupported on non-linux")
}
