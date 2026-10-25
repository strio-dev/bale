package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestArchiveTerminalProtection(t *testing.T) {
	terminal, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("PTY unavailable: %v", err)
	}
	defer func() { _ = terminal.Close() }()
	if err := validateArchiveStream("save", commandOptions{}, terminal); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("save did not refuse terminal: %v", err)
	}
	previous := os.Stdin
	os.Stdin = terminal
	defer func() { os.Stdin = previous }()
	if err := validateArchiveStream("load", commandOptions{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("load did not refuse terminal: %v", err)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devNull.Close() }()
	if err := validateArchiveStream("save", commandOptions{}, devNull); err != nil {
		t.Fatal(err)
	}
}
