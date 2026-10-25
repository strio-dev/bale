package main

import (
	"strings"
	"testing"
)

func TestCommandFlagsApplyToTheirOperation(t *testing.T) {
	for _, command := range []string{"ls", "info", "tag", "rm", "prune", "version"} {
		for _, option := range []string{"--plain-http=localhost:5000", "--authfile=auth.json"} {
			if _, err := parseCommand(command, []string{option}); err == nil || !strings.Contains(err.Error(), "unknown option") {
				t.Errorf("%s accepted irrelevant %s: %v", command, option, err)
			}
		}
	}
	for _, command := range []string{"tag", "rm"} {
		if _, err := parseCommand(command, []string{"--format=json"}); err == nil {
			t.Errorf("%s accepted an unused output format", command)
		}
	}
	if _, err := parseCommand("inspect", []string{"--output-delivery=delivery.yaml"}); err == nil {
		t.Fatal("inspect accepted the moved delivery-export option")
	}
}

func TestArchiveStreamFormatValidation(t *testing.T) {
	for _, args := range [][]string{{"bundle", "--format=json"}, {"bundle", "-o", "-", "--format=json"}} {
		if _, err := parseCommand("save", args); err == nil || !strings.Contains(err.Error(), "--format") {
			t.Errorf("save %v accepted archive and report on stdout: %v", args, err)
		}
	}
	if _, err := parseCommand("save", []string{"bundle", "-o", "bundle.tar", "--format=json"}); err != nil {
		t.Fatal(err)
	}
}
