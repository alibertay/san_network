//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// processAlive reports whether pid is a live process. Signal 0 performs the
// permission and existence checks without delivering a signal; zombies are
// reaped by PID 1 because the detached child reparents to init.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

// killProcess asks the target to shut down gracefully (SIGTERM).
func killProcess(pid int) {
	if pid <= 0 {
		return
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = process.Signal(syscall.SIGTERM)
}

// forceKillProcess is the last resort after the graceful timeout (SIGKILL).
func forceKillProcess(pid int) {
	if pid <= 0 {
		return
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = process.Signal(syscall.SIGKILL)
}

// processImageName returns the executable name of a process, or "" when it
// cannot be determined. Used to avoid killing an unrelated process when a
// stale pid is recorded in sanup.json.
func processImageName(pid int) string {
	if pid <= 0 {
		return ""
	}
	if target, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		// A binary replaced while running shows up as "/path/to/bin (deleted)".
		name := strings.TrimSuffix(filepath.Base(target), " (deleted)")
		if name != "" {
			return name
		}
	}
	if cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		fields := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		if len(fields) > 0 && fields[0] != "" {
			return filepath.Base(fields[0])
		}
	}
	if name, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
		return strings.TrimSpace(string(name))
	}
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}
