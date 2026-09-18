//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
)

func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcessGroup}
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	output, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), strconv.Itoa(pid))
}

// processImageName returns the executable name of a process, or "" when it
// cannot be determined. Used to avoid killing an unrelated process when a
// stale pid is recorded in sanup.json.
func processImageName(pid int) string {
	if pid <= 0 {
		return ""
	}
	output, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.TrimPrefix(string(output), "\uFEFF"))
	if line == "" || strings.HasPrefix(line, "INFO:") {
		return ""
	}
	fields := strings.Split(line, ",")
	if len(fields) == 0 {
		return ""
	}
	return strings.Trim(fields[0], "\"")
}

func killProcess(pid int) {
	if pid <= 0 {
		return
	}
	_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}
