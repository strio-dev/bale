package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/strio-dev/bale"
)

func TestPushDryRunFormats(t *testing.T) {
	dir, root, _ := cliBundle(t)
	destination := "mirror.example.com/releases/v1:bundle"
	client, err := bale.NewClient(bale.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := client.PlanPush(t.Context(), dir, destination, bale.PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	expectedJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var readable bytes.Buffer
	if err := writePushPreview(&readable, plan); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, want string
		flags      []string
		json       bool
	}{
		{name: "default", want: readable.String()},
		{name: "quiet", flags: []string{"-q"}, want: readable.String()},
		{name: "boolean", flags: []string{"--dry-run=true"}, want: readable.String()},
		{name: "empty format", flags: []string{"--format="}, want: readable.String()},
		{name: "JSON", flags: []string{"--format", "json"}, json: true},
		{name: "JSON shortcut", flags: []string{"--format", "{{json}}"}, json: true},
		{name: "JSON helper", flags: []string{"--format", "{{json .}}"}, json: true},
		{name: "template", flags: []string{"--format", "{{.Descriptor.Digest}} {{.Destination}} {{len .Relocations}}"}, want: fmt.Sprintf("%s %s %d\n", root.Digest, destination, len(plan.Relocations))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"push", "--layout", dir, destination}
			if tc.name != "boolean" {
				args = append(args, "--dry-run")
			}
			args = append(args, tc.flags...)
			var out, errs bytes.Buffer
			if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 {
				t.Fatalf("exit %d stdout %s stderr %s", code, &out, &errs)
			}
			if !tc.json {
				if out.String() != tc.want {
					t.Fatalf("output %q want %q", out.String(), tc.want)
				}
				return
			}
			var got, want any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(expectedJSON, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("JSON differs from existing delivery plan:\n%s\nwant:\n%s", &out, expectedJSON)
			}
		})
	}
}

func TestPushDryRunOptionsAndHelp(t *testing.T) {
	opts, err := parseCommand("push", []string{"layout", "mirror.example.com/bundle:v1", "--dry-run", "--delivery", "delivery.yaml", "--map", "source.example.com/app=apps/web", "--format=json", "-q"})
	if err != nil || !opts.dryRun || !opts.quiet || opts.delivery != "delivery.yaml" || opts.format != "json" || len(opts.maps) != 1 || len(opts.args) != 2 {
		t.Fatalf("unexpected options: %+v, %v", opts, err)
	}
	for _, args := range [][]string{{"push", "--layout", "--help"}, {"help", "push"}} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 0 || errs.Len() != 0 {
			t.Fatalf("exit %d stderr %s", code, &errs)
		}
		for _, want := range []string{"bale push [options] BUNDLE DESTREF", "--dry-run", "without registry access", "existing remote tags are not checked", "dry-run: delivery preview"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("missing %q: %s", want, &out)
			}
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"push", "--layout", "--dry-run"}, "invalid arguments"},
		{[]string{"push", "--layout", "layout", "--dry-run"}, "invalid arguments"},
		{[]string{"push", "--layout", "layout", "mirror.example.com/b:tag", "extra", "--dry-run"}, "invalid arguments"},
		{[]string{"push", "--layout", "layout", "mirror.example.com/b:tag", "--dry-run", "--dry-run"}, "duplicate option"},
		{[]string{"push", "--layout", "layout", "mirror.example.com/b:tag", "--dry-run=maybe"}, "invalid boolean"},
		{[]string{"push", "--layout", "layout", "team/b:tag", "--dry-run"}, "explicit registry"},
		{[]string{"push", "--layout", "layout", "mirror.example.com/b", "--dry-run"}, "explicit tag"},
		{[]string{"push", "--layout", "layout", "mirror.example.com/b:tag", "--dry-run", "--map", "invalid"}, "--map requires"},
		{[]string{"pull", "--layout", "source.example.com/b:tag", "layout", "--dry-run"}, "unknown option"},
	} {
		var out, errs bytes.Buffer
		if code := run(tc.args, &out, &errs); code != 125 || out.Len() != 0 || !strings.Contains(errs.String(), tc.want) {
			t.Fatalf("%v: exit %d stdout %s stderr %s", tc.args, code, &out, &errs)
		}
	}
}

func TestPushDryRunOutputErrorsAndCancellation(t *testing.T) {
	dir, _, _ := cliBundle(t)
	destination := "mirror.example.com/releases/v1:bundle"
	docker := t.TempDir()
	if err := os.WriteFile(filepath.Join(docker, "config.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", docker)
	for _, format := range []string{"{{", "prefix{{.MissingField}}"} {
		var out, errs bytes.Buffer
		if code := run([]string{"push", "--layout", dir, destination, "--dry-run", "--format", format}, &out, &errs); code != 125 || out.Len() != 0 || !strings.HasPrefix(errs.String(), "Error: ") {
			t.Fatalf("%s: exit %d stdout %s stderr %s", format, code, &out, &errs)
		}
	}
	for _, flags := range [][]string{nil, {"--format", "json"}, {"--format", "{{.Destination}}"}} {
		args := append([]string{"push", "--layout", dir, destination, "--dry-run"}, flags...)
		var errs bytes.Buffer
		if code := run(args, brokenWriter{}, &errs); code != 125 || !strings.Contains(errs.String(), "write failed") {
			t.Fatalf("%v: exit %d stderr %s", flags, code, &errs)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out, errs bytes.Buffer
	if code := runContext(ctx, []string{"push", "--layout", dir, destination, "--dry-run"}, &out, &errs); code != 125 || out.Len() != 0 || !strings.Contains(errs.String(), "context canceled") {
		t.Fatalf("cancelled preview: exit %d stdout %s stderr %s", code, &out, &errs)
	}
}
