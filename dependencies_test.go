package bale_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
)

type forbiddenDependencySource struct{ t *testing.T }

func (s forbiddenDependencySource) Fetch(context.Context, v1.Descriptor) (io.ReadCloser, error) {
	s.t.Error("authoring read subject source")
	return nil, errors.New("unexpected Fetch")
}
func (s forbiddenDependencySource) Exists(context.Context, v1.Descriptor) (bool, error) {
	s.t.Error("authoring queried subject source")
	return false, errors.New("unexpected Exists")
}

func dependencySubject(t *testing.T) bale.Root {
	return bale.Root{Repository: "source.example/charts/app", Descriptor: v1.Descriptor{
		MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString("unfetched subject"), Size: 123,
		Annotations: map[string]string{"producer": "original"}, URLs: []string{"https://source.example/subject"},
	}, Store: forbiddenDependencySource{t}}
}

func TestPublicDependencyDeclarationAuthoring(t *testing.T) {
	subject := dependencySubject(t)
	declaration := bale.DependencyDeclaration{Dependencies: []bale.DependencyReference{
		{Ref: "other.example/images/web:stable"}, {Ref: "other.example/images/web:stable"},
		{Ref: "third.example/components/base:stable", Select: &bale.DependencySelector{Context: "production"}},
	}}
	ctx := context.Background()
	request, err := bale.PrepareDependencyDeclaration(ctx, subject, declaration)
	if err != nil {
		t.Fatal(err)
	}
	again, err := bale.PrepareDependencyDeclaration(ctx, subject, declaration)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(request.Descriptor, again.Descriptor) {
		t.Fatalf("not deterministic: %v %v", request.Descriptor, again.Descriptor)
	}
	if request.Subject != subject.Repository+"@"+subject.Descriptor.Digest.String() {
		t.Fatalf("subject not pinned: %s", request.Subject)
	}
	raw, err := content.FetchAll(ctx, request.Store, request.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	second, err := content.FetchAll(ctx, again.Store, again.Descriptor)
	if err != nil || !bytes.Equal(raw, second) {
		t.Fatal("changed manifest bytes", err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ArtifactType != bale.DependencyArtifactType || manifest.MediaType != v1.MediaTypeImageManifest || manifest.SchemaVersion != 2 || len(manifest.Layers) != 1 {
		t.Fatalf("wrong declaration envelope: %+v", manifest)
	}
	if !reflect.DeepEqual(manifest.Subject, &subject.Descriptor) {
		t.Fatal("subject descriptor changed")
	}
	if len(manifest.Annotations) != 0 {
		t.Fatal("unexpected generated annotations")
	}
	config, err := content.FetchAll(ctx, request.Store, manifest.Config)
	if err != nil || string(config) != "{}" || manifest.Config.MediaType != v1.MediaTypeEmptyJSON {
		t.Fatalf("wrong config: %q %v", config, err)
	}
	payload, err := content.FetchAll(ctx, request.Store, manifest.Layers[0])
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := bale.DecodeDependencyDeclaration(payload)
	if err != nil || decoded.SchemaVersion != bale.DependencyVersion || len(decoded.Dependencies) != 2 {
		t.Fatalf("wrong declaration payload: %s %v", payload, err)
	}
	if exists, err := request.Store.Exists(ctx, subject.Descriptor); err != nil || exists {
		t.Fatalf("subject copied: exists=%v err=%v", exists, err)
	}
	// Changing caller-owned input cannot change prepared content.
	subject.Descriptor.Annotations["producer"] = "changed"
	subject.Descriptor.URLs[0] = "https://changed.example"
	declaration.Dependencies[2].Select.Context = "changed"
	preserved, err := content.FetchAll(ctx, request.Store, request.Descriptor)
	if err != nil || !bytes.Equal(raw, preserved) {
		t.Fatal("prepared content shares mutable caller data")
	}
}

func TestPublicDependencyDeclarationTypedErrors(t *testing.T) {
	if _, err := bale.EncodeDependencyDeclaration(bale.DependencyDeclaration{SchemaVersion: 77}); !errors.Is(err, bale.ErrInvalid) {
		t.Fatalf("encode missing typed error: %v", err)
	}
	if _, err := bale.DecodeDependencyDeclaration([]byte(`{"schemaVersion":1,"dependencies":null}`)); !errors.Is(err, bale.ErrInvalid) {
		t.Fatalf("decode missing typed error: %v", err)
	}
	declaration := bale.DependencyDeclaration{}
	cases := []struct {
		name   string
		change func(*bale.Root)
		kind   error
	}{
		{"missing repository", func(s *bale.Root) { s.Repository = "" }, bale.ErrInvalid},
		{"implicit registry", func(s *bale.Root) { s.Repository = "team/chart" }, bale.ErrInvalid},
		{"tagged repository", func(s *bale.Root) { s.Repository = "source.example/chart:tag" }, bale.ErrInvalid},
		{"invalid digest", func(s *bale.Root) { s.Descriptor.Digest = "sha256:bad" }, bale.ErrInvalid},
		{"negative size", func(s *bale.Root) { s.Descriptor.Size = -1 }, bale.ErrInvalid},
		{"unsupported envelope", func(s *bale.Root) { s.Descriptor.MediaType = "application/example" }, bale.ErrUnsupported},
		{"mismatched inline bytes", func(s *bale.Root) { s.Descriptor.Data = []byte("invalid") }, bale.ErrInvalid},
		{"malformed inline envelope", func(s *bale.Root) {
			s.Descriptor = content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, []byte("{}"))
			s.Descriptor.Data = []byte("{}")
		}, bale.ErrInvalid},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			subject := dependencySubject(t)
			tt.change(&subject)
			if _, err := bale.PrepareDependencyDeclaration(context.Background(), subject, declaration); !errors.Is(err, tt.kind) {
				t.Fatalf("got %v; want %v", err, tt.kind)
			}
		})
	}
	if _, err := bale.PrepareDependencyDeclaration(context.Background(), dependencySubject(t), bale.DependencyDeclaration{Dependencies: []bale.DependencyReference{{Ref: "implicit/tag"}}}); !errors.Is(err, bale.ErrInvalid) {
		t.Fatalf("invalid dependency: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := bale.PrepareDependencyDeclaration(ctx, dependencySubject(t), declaration); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestPublicDependencyAuthoringSupportsPinnedIndexesAndInlineSubject(t *testing.T) {
	for _, mt := range []string{v1.MediaTypeImageIndex, "application/vnd.docker.distribution.manifest.v2+json", "application/vnd.docker.distribution.manifest.list.v2+json"} {
		subject := dependencySubject(t)
		subject.Descriptor.MediaType = mt
		if _, err := bale.PrepareDependencyDeclaration(context.Background(), subject, bale.DependencyDeclaration{}); err != nil {
			t.Errorf("%s: %v", mt, err)
		}
	}
	raw := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	subject := dependencySubject(t)
	subject.Descriptor = content.NewDescriptorFromBytes(v1.MediaTypeImageIndex, raw)
	subject.Descriptor.Data = raw
	if _, err := bale.PrepareDependencyDeclaration(context.Background(), subject, bale.DependencyDeclaration{}); err != nil {
		t.Fatal(err)
	}
}

func TestPublicDependencyAuthoringSizeLimits(t *testing.T) {
	subject := dependencySubject(t)
	subject.Descriptor.Annotations["oversized"] = strings.Repeat("x", int(bale.DependencyMaxBytes))
	if _, err := bale.PrepareDependencyDeclaration(context.Background(), subject, bale.DependencyDeclaration{}); !errors.Is(err, bale.ErrInvalid) {
		t.Fatalf("oversized manifest: %v", err)
	}
	if _, err := bale.EncodeDependencyDeclaration(bale.DependencyDeclaration{Context: strings.Repeat("x", int(bale.DependencyMaxBytes))}); !errors.Is(err, bale.ErrInvalid) {
		t.Fatalf("oversized payload: %v", err)
	}
}
