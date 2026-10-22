package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	bale "github.com/strio-dev/bale"
	relocate "github.com/strio-dev/bale/plugins/helm-relocate"
)

const pod = "apiVersion: v1\nkind: Pod\nspec:\n  containers: [{name: app, image: nginx}]\n"

func arguments(t *testing.T) ([]string, string) {
	t.Helper()
	chartDigest := digest.FromString("chart")
	chart := "target.example/charts/demo@" + chartDigest.String()
	imageDigest := digest.FromString("image")
	image := "target.example/images/nginx@" + imageDigest.String()
	record := bale.DestinationRecord{
		SchemaVersion: 1,
		Bundle:        "target.example/bundles/demo@" + digest.FromString("bundle").String(),
		Source:        "source.example/charts/demo@" + chartDigest.String(),
		Destination:   chart,
		Resolutions: []bale.DestinationResolution{{
			Capture: digest.FromString("capture"), Outcome: "selected",
			Declaration:  &bale.DestinationDeclaration{Digest: digest.FromString("declaration"), Context: "production"},
			Dependencies: []bale.DestinationDependency{{Ref: "docker.io/library/nginx:latest", Source: "docker.io/library/nginx@" + imageDigest.String(), Destination: image}},
		}},
	}
	recordJSON, err := bale.EncodeDestinationRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal([]any{map[string]any{
		"repository": "target.example/charts/demo",
		"subject":    v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: chartDigest, Size: 100},
		"descriptor": v1.Descriptor{MediaType: v1.MediaTypeImageManifest, ArtifactType: "application/vnd.bale.destination.v1+json", Digest: digest.FromString("record"), Size: 100},
		"record":     json.RawMessage(recordJSON), "local": true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return []string{"--chart", chart, "--records-json", string(raw)}, image
}

func TestRunRelocates(t *testing.T) {
	args, image := arguments(t)
	var out bytes.Buffer
	if err := run(args, strings.NewReader(pod), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), image) {
		t.Fatalf("missing destination image: %s", &out)
	}
}

func TestRunErrorsHaveNoOutput(t *testing.T) {
	args, _ := arguments(t)
	for _, tc := range []struct {
		name  string
		args  []string
		input string
	}{
		{"arguments", []string{"--unknown"}, pod},
		{"records", []string{"--chart", args[1], "--records-json", "[]"}, pod},
		{"later document", args, pod + "---\napiVersion: v1\nkind: Pod\nspec:\n  containers: [{name: bad, image: '@bad'}]\n"},
		{"oversize", args, strings.Repeat("x", relocate.MaxManifestBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := run(tc.args, strings.NewReader(tc.input), &out); err == nil {
				t.Fatal("expected error")
			}
			if out.Len() != 0 {
				t.Fatalf("partial output: %s", &out)
			}
		})
	}
}

type failedReader struct{ err error }

func (r failedReader) Read([]byte) (int, error) { return 0, r.err }

type failedWriter struct{ err error }

func (w failedWriter) Write([]byte) (int, error) { return 0, w.err }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestRunInputFailure(t *testing.T) {
	args, _ := arguments(t)
	sentinel := errors.New("input unavailable")
	var out bytes.Buffer
	if err := run(args, failedReader{sentinel}, &out); !errors.Is(err, sentinel) {
		t.Fatalf("error: %v", err)
	}
	if out.Len() != 0 {
		t.Fatal("output written after read failure")
	}
}

func TestRunOutputFailure(t *testing.T) {
	args, _ := arguments(t)
	sentinel := errors.New("output unavailable")
	if err := run(args, strings.NewReader(pod), failedWriter{sentinel}); !errors.Is(err, sentinel) {
		t.Fatalf("error: %v", err)
	}
	if err := run(args, strings.NewReader(pod), shortWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("error: %v", err)
	}
}

func TestHelpDoesNotReadInput(t *testing.T) {
	for _, arg := range []string{"--help", "-h"} {
		var out bytes.Buffer
		if err := run([]string{arg}, failedReader{errors.New("must not read stdin")}, &out); err != nil {
			t.Fatal(err)
		}
		if out.String() != help {
			t.Fatalf("unexpected help: %s", &out)
		}
	}
	sentinel := errors.New("help output unavailable")
	if err := run([]string{"--help"}, nil, failedWriter{sentinel}); !errors.Is(err, sentinel) {
		t.Fatalf("error: %v", err)
	}
}
