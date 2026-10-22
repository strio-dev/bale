package bundle

import (
	"encoding/json"
	"testing"

	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestEnvelopeEffectiveArtifactType(t *testing.T) {
	for _, explicit := range []string{"", "application/example.explicit"} {
		t.Run("manifest/"+explicit, func(t *testing.T) {
			store := testStore{}
			config := add(t, store, "application/example.config", []byte("{}"))
			manifest := v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: explicit, Config: config, Layers: []v1.Descriptor{}}
			d := add(t, store, v1.MediaTypeImageManifest, manifest)
			want := explicit
			if want == "" {
				want = config.MediaType
			}
			d.ArtifactType = want
			env, err := ParseEnvelope(d, store[d.Digest])
			if err != nil || env.ArtifactType != want {
				t.Fatalf("effective type=%q err=%v", env.ArtifactType, err)
			}
			d.ArtifactType = "application/example.wrong"
			if _, err := ParseEnvelope(d, store[d.Digest]); err == nil {
				t.Fatal("accepted incorrect inventory artifactType")
			}
			if explicit == "" {
				var fields map[string]any
				if err := json.Unmarshal(store[d.Digest], &fields); err != nil {
					t.Fatal(err)
				}
				fields["artifactType"] = ""
				d = add(t, store, v1.MediaTypeImageManifest, fields)
				d.ArtifactType = want
				if env, err := ParseEnvelope(d, store[d.Digest]); err != nil || env.ArtifactType != want {
					t.Fatalf("empty explicit fallback: %+v err=%v", env, err)
				}
			}
		})
	}
	store := testStore{}
	d := add(t, store, v1.MediaTypeImageIndex, v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{}})
	if env, err := ParseEnvelope(d, store[d.Digest]); err != nil || env.ArtifactType != "" {
		t.Fatalf("untyped index: %+v err=%v", env, err)
	}
	for _, bad := range []any{nil, 42, []string{"type"}} {
		d := add(t, store, v1.MediaTypeImageIndex, map[string]any{"schemaVersion": 2, "manifests": []any{}, "artifactType": bad})
		if _, err := ParseEnvelope(d, store[d.Digest]); err == nil {
			t.Fatalf("accepted nonstring artifactType: %#v", bad)
		}
	}
}
