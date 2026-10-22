package main

import (
	"bytes"
	"strings"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
)

func TestCaptureProgressDisplay(t *testing.T) {
	input := 1
	descriptor := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, []byte("{}"))
	for _, tc := range []struct {
		operation string
		want      string
	}{
		{"resolving", "resolving include[1] example.com/chart:v1"},
		{"resolving-plugin", "resolving plugin example.com/plugin:v2 for include[1] example.com/chart:v1"},
		{"fetching-plugin", "fetching plugin example.com/plugin:v2 for include[1] example.com/chart:v1"},
		{"running-plugin", "discovering dependencies for include[1] example.com/chart:v1 using example.com/plugin:v2"},
		{"capturing", "capturing include[1] example.com/chart:v1"},
		{"captured", "captured bundle " + descriptor.Digest.String()},
		{"copied", "copied example.com/chart (" + descriptor.Digest.String() + ")"},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			event := bale.Progress{Operation: tc.operation, Input: &input, Reference: "example.com/chart:v1", Plugin: "example.com/plugin:v2", Repository: "example.com/chart", Descriptor: descriptor}
			var out bytes.Buffer
			if err := writeProgress(&out, event); err != nil || out.String() != tc.want+"\n" {
				t.Fatalf("progress %q: %v", &out, err)
			}
			if err := writeProgress(brokenWriter{}, event); err == nil {
				t.Fatal("ignored progress output failure")
			}
		})
	}
}

func TestProgressBeforeResolution(t *testing.T) {
	var out bytes.Buffer
	if err := writeProgress(&out, bale.Progress{Operation: "resolving", Reference: "example.com/app:latest"}); err != nil || out.String() != "resolving example.com/app:latest\n" || strings.Contains(out.String(), "()") {
		t.Fatalf("unresolved progress %q: %v", &out, err)
	}
}
