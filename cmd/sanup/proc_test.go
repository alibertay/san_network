package main

import (
	"os"
	"testing"
)

func TestProcessImageNameForOwnProcess(t *testing.T) {
	name := processImageName(os.Getpid())
	if name == "" {
		t.Fatalf("processImageName returned no name for the test process")
	}
	if !isNodeProcess(os.Getpid()) {
		t.Fatalf("the test process name %q should match this executable", name)
	}
}

func TestIsNodeProcessRejectsUnknownPID(t *testing.T) {
	const bogus = 1 << 30
	if processImageName(bogus) != "" {
		t.Fatalf("processImageName(%d) = %q, want empty", bogus, processImageName(bogus))
	}
	if isNodeProcess(bogus) {
		t.Fatalf("isNodeProcess(%d) = true, want false", bogus)
	}
}
