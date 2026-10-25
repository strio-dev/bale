package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveCLIStreams(t *testing.T) {
	layout, root, _ := cliBundle(t)
	for _, output := range [][]string{nil, {"-o", "-"}} {
		var archive, stderr bytes.Buffer
		args := append([]string{"save", "--layout", layout}, output...)
		if code := run(args, &archive, &stderr); code != 0 || stderr.Len() != 0 {
			t.Fatalf("save stream: exit %d stderr %s", code, &stderr)
		}
		for _, input := range [][]string{nil, {"-i", "-"}} {
			withInput(t, archive.String())
			var out, errs bytes.Buffer
			args := append([]string{"load", "--store", t.TempDir(), "-t", "restored:v1"}, input...)
			if code := run(args, &out, &errs); code != 0 || out.String() != root.Digest.String()+"\n" || errs.Len() != 0 {
				t.Fatalf("load stream: exit %d stdout %s stderr %s", code, &out, &errs)
			}
		}
		withInput(t, archive.String())
		var out, errs bytes.Buffer
		if code := run([]string{"load", "--layout", filepath.Join(t.TempDir(), "restored")}, &out, &errs); code != 0 || out.String() != root.Digest.String()+"\n" {
			t.Fatalf("layout load stream: exit %d stdout %s stderr %s", code, &out, &errs)
		}
	}
}

func TestManagedSaveStreamAndShortID(t *testing.T) {
	layout, root, _ := cliBundle(t)
	var archive, errs bytes.Buffer
	if code := run([]string{"save", "--layout", layout}, &archive, &errs); code != 0 {
		t.Fatalf("save: %d %s", code, &errs)
	}
	withInput(t, archive.String())
	path := t.TempDir()
	var out bytes.Buffer
	if code := run([]string{"load", "--store", path, "-t", "release:v1"}, &out, &errs); code != 0 {
		t.Fatalf("load: %d %s", code, &errs)
	}
	var exported bytes.Buffer
	if code := run([]string{"save", "--store", path, root.Digest.Encoded()[:12]}, &exported, &errs); code != 0 || !bytes.Equal(archive.Bytes(), exported.Bytes()) {
		t.Fatalf("managed archive stream differs: %d %s", code, &errs)
	}
	out.Reset()
	if code := run([]string{"ls", "--store", path, "-q"}, &out, &errs); code != 0 || strings.TrimSpace(out.String()) != root.Digest.Encoded()[:12] {
		t.Fatalf("quiet IDs: %d %q %s", code, &out, &errs)
	}
	out.Reset()
	if code := run([]string{"inspect", "--store", path, root.Digest.Encoded()[:12]}, &out, &errs); code != 0 || !bytes.Contains(out.Bytes(), []byte(root.Digest)) {
		t.Fatalf("short ID inspect: %d %s", code, &errs)
	}
}
