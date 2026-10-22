package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

func dependencyInspectionFixture(t *testing.T) (*sdkRegistry, *bale.Client, bale.Root) {
	t.Helper()
	registry := newSDKRegistry()
	server := httptest.NewServer(registry)
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	subject := publishDependencyArtifact(t, client, host, dependencyArtifact(t, "inspection-subject", false), "inspect")
	return registry, client, subject
}

// Install exact registry metadata, including intentionally invalid declarations
// which the publishing API correctly refuses to create.
func installInspectionManifest(t *testing.T, registry *sdkRegistry, subject bale.Root, manifest v1.Manifest, payload []byte) v1.Descriptor {
	t.Helper()
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, raw)
	descriptor.ArtifactType = manifest.ArtifactType
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.objects[descriptor.Digest.String()] = raw
	registry.types[descriptor.Digest.String()] = descriptor.MediaType
	if payload != nil {
		registry.objects[manifest.Layers[0].Digest.String()] = payload
	}
	tag := strings.Replace(subject.Descriptor.Digest.String(), ":", "-", 1)
	index := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{}}
	if previous := registry.tags[tag]; previous != "" {
		if err := json.Unmarshal(registry.objects[previous], &index); err != nil {
			t.Fatal(err)
		}
	}
	index.Manifests = append(index.Manifests, descriptor)
	raw, err = json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	key := digest.FromBytes(raw).String()
	registry.objects[key], registry.types[key], registry.tags[tag] = raw, v1.MediaTypeImageIndex, key
	return descriptor
}

func inspectionPreparedManifest(t *testing.T, subject bale.Root, declaration bale.DependencyDeclaration) (bale.AttachRequest, v1.Manifest, []byte) {
	t.Helper()
	request, err := bale.PrepareDependencyDeclaration(context.Background(), subject, declaration)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := content.FetchAll(context.Background(), request.Store, request.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	payload, err := content.FetchAll(context.Background(), request.Store, manifest.Layers[0])
	if err != nil {
		t.Fatal(err)
	}
	return request, manifest, payload
}

func TestPublicInspectDependenciesPinsSubjectAndReadsOnlyDeclarations(t *testing.T) {
	registry, publisher, subject := dependencyInspectionFixture(t)
	ctx := context.Background()
	declarations := []bale.DependencyDeclaration{
		{Context: "production", Dependencies: []bale.DependencyReference{{Ref: "unreachable.example/images/web:stable"}, {Ref: "unreachable.example/images/web:stable", Follow: "auto"}}},
		{Context: "development", Dependencies: []bale.DependencyReference{{Ref: "unreachable.example/charts/db:latest", Select: &bale.DependencySelector{Context: "testing"}}, {Ref: "unreachable.example/images/tools:latest", Follow: "none"}, {Ref: "unreachable.example/charts/worker:stable", Select: &bale.DependencySelector{Digest: digest.FromString("selected-declaration")}}}},
	}
	expected := map[digest.Digest]bale.DependencyDeclaration{}
	allowedBlobs := map[string]bool{}
	for _, declaration := range declarations {
		request, manifest, _ := inspectionPreparedManifest(t, subject, declaration)
		allowedBlobs[manifest.Config.Digest.String()] = true
		for _, layer := range manifest.Layers {
			allowedBlobs[layer.Digest.String()] = true
		}
		if _, err := publisher.Attach(ctx, request); err != nil {
			t.Fatal(err)
		}
		encoded, err := bale.EncodeDependencyDeclaration(declaration)
		if err != nil {
			t.Fatal(err)
		}
		normalized, err := bale.DecodeDependencyDeclaration(encoded)
		if err != nil {
			t.Fatal(err)
		}
		expected[request.Descriptor.Digest] = normalized
	}
	unrelated := attachmentFixture(t, &subject.Descriptor, false, "unrelated")
	unrelated.Subject = subject.Repository + "@" + subject.Descriptor.Digest.String()
	if _, err := publisher.Attach(ctx, unrelated); err != nil {
		t.Fatal(err)
	}
	replacement := publishDependencyArtifact(t, publisher, strings.Split(subject.Repository, "/")[0], dependencyArtifact(t, "replacement", false), "replacement")
	host := strings.Split(subject.Repository, "/")[0]
	var resolutions, forbidden atomic.Int32
	reader, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}, HTTPClient: &http.Client{Transport: inspectRoundTripper(func(r *http.Request) (*http.Response, error) {
		_, blob, isBlob := strings.Cut(r.URL.Path, "/blobs/")
		if r.URL.Host != host || (r.Method != http.MethodGet && r.Method != http.MethodHead) || isBlob && !allowedBlobs[blob] {
			forbidden.Add(1)
			return nil, errors.New("inspection accessed application content, a dependency, or wrote data")
		}
		response, err := http.DefaultTransport.RoundTrip(r)
		if strings.HasSuffix(r.URL.Path, "/manifests/inspect") {
			resolutions.Add(1)
			registry.mu.Lock()
			registry.tags["inspect"] = replacement.Descriptor.Digest.String()
			registry.mu.Unlock()
		}
		return response, err
	})}})
	if err != nil {
		t.Fatal(err)
	}
	registry.mu.Lock()
	before := registry.writes
	registry.mu.Unlock()
	got, err := reader.InspectDependencies(ctx, subject.Repository+":inspect")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("declarations=%+v; want both contexts without selection", got)
	}
	if resolutions.Load() != 1 || forbidden.Load() != 0 {
		t.Fatalf("tag resolutions=%d forbidden requests=%d", resolutions.Load(), forbidden.Load())
	}
	keys := make([]string, 0, len(got))
	for _, record := range got {
		keys = append(keys, record.Descriptor.Digest.String())
		if record.Repository != subject.Repository || record.Subject.Digest != subject.Descriptor.Digest || !reflect.DeepEqual(record.Declaration, expected[record.Descriptor.Digest]) {
			t.Fatalf("unexpected declaration: %+v", record)
		}
	}
	if !sort.StringsAreSorted(keys) {
		t.Fatalf("declarations not in digest order: %v", keys)
	}
	registry.mu.Lock()
	after := registry.writes
	registry.mu.Unlock()
	if after != before {
		t.Fatalf("inspection wrote %d registry requests", after-before)
	}
}

func TestPublicInspectDependenciesEmptyAndCancellation(t *testing.T) {
	_, client, subject := dependencyInspectionFixture(t)
	got, err := client.InspectDependencies(context.Background(), subject.Repository+":inspect")
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty declarations=%+v err=%v; want nonnil []", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err = client.InspectDependencies(ctx, subject.Repository+":inspect")
	if !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("canceled declarations=%+v err=%v", got, err)
	}
}

func TestPublicInspectDependenciesRejectsInvalidMetadataWithoutPartialResults(t *testing.T) {
	for _, name := range []string{"missing payload", "corrupt payload", "malformed JSON", "oversized payload", "missing manifest", "corrupt manifest", "oversized manifest", "wrong subject", "future version", "invalid unrelated envelope"} {
		t.Run(name, func(t *testing.T) {
			registry, client, subject := dependencyInspectionFixture(t)
			valid, _, _ := inspectionPreparedManifest(t, subject, bale.DependencyDeclaration{Context: "valid", Dependencies: []bale.DependencyReference{}})
			if _, err := client.Attach(context.Background(), valid); err != nil {
				t.Fatal(err)
			}
			_, manifest, payload := inspectionPreparedManifest(t, subject, bale.DependencyDeclaration{Context: "invalid", Dependencies: []bale.DependencyReference{{Ref: "unreachable.example/image:latest"}}})
			switch name {
			case "malformed JSON":
				payload = []byte(`{"schemaVersion":1,"dependencies":{},"context":"invalid"}`)
				manifest.Layers[0] = content.NewDescriptorFromBytes(bale.DependencyArtifactType, payload)
			case "oversized payload":
				manifest.Layers[0].Size = bale.DependencyMaxBytes + 1
			case "oversized manifest":
				manifest.Annotations = map[string]string{"oversized": strings.Repeat("x", int(bale.DependencyMaxBytes))}
			case "wrong subject":
				wrong := subject.Descriptor
				wrong.Size++
				manifest.Subject = &wrong
			case "future version":
				manifest.ArtifactType = "application/vnd.bale.dependencies.v2+json"
			case "invalid unrelated envelope":
				manifest.ArtifactType = "application/example.unrelated"
				manifest.SchemaVersion = 1
			}
			descriptor := installInspectionManifest(t, registry, subject, manifest, payload)
			registry.mu.Lock()
			switch name {
			case "missing payload":
				delete(registry.objects, manifest.Layers[0].Digest.String())
			case "corrupt payload":
				registry.corruptRead = manifest.Layers[0].Digest.String()
			case "missing manifest":
				delete(registry.objects, descriptor.Digest.String())
			case "corrupt manifest":
				registry.corruptRead = descriptor.Digest.String()
			}
			before := registry.writes
			registry.mu.Unlock()
			got, err := client.InspectDependencies(context.Background(), subject.Repository+":inspect")
			if err == nil || got != nil {
				t.Fatalf("invalid metadata returned declarations=%+v err=%v", got, err)
			}
			if name == "future version" && !errors.Is(err, bale.ErrUnsupported) {
				t.Fatalf("future version should be unsupported: %v", err)
			}
			registry.mu.Lock()
			after := registry.writes
			registry.mu.Unlock()
			if after != before {
				t.Fatalf("invalid metadata inspection wrote %d requests", after-before)
			}
		})
	}
}

func TestPublicInspectDependenciesPreservesReadFailureCauses(t *testing.T) {
	for _, failure := range []string{"authorization", "transport", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			registry, publisher, subject := dependencyInspectionFixture(t)
			request, manifest, _ := inspectionPreparedManifest(t, subject, bale.DependencyDeclaration{Dependencies: []bale.DependencyReference{}})
			if _, err := publisher.Attach(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			cause := errors.New("injected dependency metadata transport failure")
			if failure == "cancellation" {
				cause = context.Canceled
			}
			host := strings.Split(subject.Repository, "/")[0]
			reader, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}, HTTPClient: &http.Client{Transport: inspectRoundTripper(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/blobs/"+manifest.Layers[0].Digest.String()) {
					if failure != "authorization" {
						return nil, cause
					}
					registry.mu.Lock()
					registry.failRead = manifest.Layers[0].Digest.String()
					registry.mu.Unlock()
				}
				return http.DefaultTransport.RoundTrip(r)
			})}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := reader.InspectDependencies(context.Background(), subject.Repository+":inspect")
			if got != nil || err == nil || errors.Is(err, bale.ErrIntegrity) {
				t.Fatalf("read failure declarations=%+v err=%v", got, err)
			}
			if failure == "authorization" {
				var response *errcode.ErrorResponse
				if !errors.As(err, &response) || response.StatusCode != http.StatusForbidden {
					t.Fatalf("lost authorization status: %v", err)
				}
			}
			if failure != "authorization" && !errors.Is(err, cause) {
				t.Fatalf("lost cause %v: %v", cause, err)
			}
		})
	}
}

func TestPublicInspectDependenciesAcceptsPayloadAtMetadataLimit(t *testing.T) {
	registry, client, subject := dependencyInspectionFixture(t)
	declaration := bale.DependencyDeclaration{Context: "boundary", Dependencies: []bale.DependencyReference{}}
	request, manifest, payload := inspectionPreparedManifest(t, subject, declaration)
	if _, err := client.Attach(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	payload = append(payload, []byte(strings.Repeat(" ", int(bale.DependencyMaxBytes)-len(payload)))...)
	manifest.Layers[0] = content.NewDescriptorFromBytes(bale.DependencyArtifactType, payload)
	expected := installInspectionManifest(t, registry, subject, manifest, payload)
	got, err := client.InspectDependencies(context.Background(), subject.Repository+":inspect")
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range got {
		if record.Descriptor.Digest == expected.Digest && record.Declaration.Context == "boundary" && len(record.Declaration.Dependencies) == 0 {
			return
		}
	}
	t.Fatalf("valid declaration at metadata size limit was not inspected: %+v", got)
}
