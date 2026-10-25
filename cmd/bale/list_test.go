package main

import (
	"bytes"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/store"
)

func TestBundleListIDs(t *testing.T) {
	id := digest.FromString("bundle")
	entries := []store.Entry{{Descriptor: v1.Descriptor{Digest: id}, Names: []string{"localhost/release:v1"}, MetadataOnly: true}}
	for _, tc := range []struct {
		name           string
		quiet, noTrunc bool
		want           string
	}{
		{"table", false, false, id.Encoded()[:12]},
		{"full table", false, true, id.String()},
		{"quiet", true, false, id.Encoded()[:12]},
		{"full quiet", true, true, id.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeBundleList(&out, entries, tc.quiet, tc.noTrunc); err != nil {
				t.Fatal(err)
			}
			if tc.quiet {
				if out.String() != tc.want+"\n" {
					t.Fatalf("unexpected IDs %q", out.String())
				}
			} else if !strings.Contains(out.String(), tc.want) || !strings.Contains(out.String(), "metadata") || !strings.Contains(out.String(), "localhost/release:v1") {
				t.Fatalf("missing bundle details: %s", &out)
			}
			if !tc.noTrunc && strings.Contains(out.String(), id.String()) {
				t.Fatal("default listing printed a full digest")
			}
		})
	}
}
