package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

type destinationFixture struct {
	layout                      string
	created                     bale.CreateResult
	inner                       bale.CreateResult
	chart, imageA, imageB, tool bale.Root
}

func publishDestinationComponent(t *testing.T, client *bale.Client, host, name, value string) bale.Root {
	t.Helper()
	root := dependencyArtifact(t, value, name == "images/a" || name == "images/b")
	root.Repository = "fixture.example/" + name
	root.Aliases = []string{"stable"}
	plan, err := client.Plan(t.Context(), bale.TransferRequest{Roots: []bale.Root{root}, Destination: host + "/source"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Execute(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	result, err := client.Resolve(t.Context(), host+"/source/"+name+":stable")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func newDestinationFixture(t *testing.T, nested bool) destinationFixture {
	t.Helper()
	source := httptest.NewServer(&deliveryRegistry{})
	holding := httptest.NewServer(&deliveryRegistry{})
	defer source.Close()
	defer holding.Close()
	sourceHost, holdingHost := strings.TrimPrefix(source.URL, "http://"), strings.TrimPrefix(holding.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{sourceHost, holdingHost}})
	if err != nil {
		t.Fatal(err)
	}
	f := destinationFixture{}
	f.chart = publishDestinationComponent(t, client, sourceHost, "charts/app", "chart")
	f.imageA = publishDestinationComponent(t, client, sourceHost, "images/a", "same-image")
	f.imageB = publishDestinationComponent(t, client, sourceHost, "images/b", "same-image")
	f.tool = publishDestinationComponent(t, client, sourceHost, "images/tool", "transitive-tool")
	attach := func(subject bale.Root, context string, dependencies []bale.DependencyReference) {
		request, err := bale.PrepareDependencyDeclaration(t.Context(), subject, bale.DependencyDeclaration{Context: context, Dependencies: dependencies})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Attach(t.Context(), request); err != nil {
			t.Fatal(err)
		}
	}
	attach(f.chart, "production", []bale.DependencyReference{{Ref: f.imageA.Repository + ":stable"}})
	attach(f.imageA, "", []bale.DependencyReference{{Ref: f.tool.Repository + ":stable", Follow: "none"}})
	selection := bale.Selection{Ref: f.chart.Repository + ":stable", Dependencies: bale.DependencyPolicy{Select: &bale.DependencySelector{Context: "production"}}}
	recipe := bale.Recipe{Include: []bale.Selection{selection}}
	if nested {
		f.inner, err = client.Create(t.Context(), recipe, holdingHost+"/bundles:inner", bale.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		attach(f.chart, "staging", []bale.DependencyReference{{Ref: f.imageB.Repository + ":stable"}})
		selection.Dependencies.Select = &bale.DependencySelector{Context: "staging"}
		recipe.Include = []bale.Selection{{Ref: holdingHost + "/bundles:inner"}, selection}
	}
	reference := holdingHost + "/bundles:outer"
	f.created, err = client.Create(t.Context(), recipe, reference, bale.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.layout = filepath.Join(t.TempDir(), "layout")
	if _, err := client.Pull(t.Context(), reference, f.layout, bale.PullLocked); err != nil {
		t.Fatal(err)
	}
	return f
}

func destinationTestClient(t *testing.T, host string, transport http.RoundTripper) *bale.Client {
	t.Helper()
	if transport == nil {
		transport = http.DefaultTransport
	}
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}, HTTPClient: &http.Client{Transport: inspectRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != host {
			t.Errorf("destination operation contacted historical registry: %s", r.URL)
			return nil, errors.New("unexpected historical registry request")
		}
		return transport.RoundTrip(r)
	})}})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func destinationRef(root bale.Root) string {
	return root.Repository + "@" + root.Descriptor.Digest.String()
}

func TestPublicDestinationRecordsPreserveNestedContextsAndRepositoryIdentity(t *testing.T) {
	f := newDestinationFixture(t, true)
	registry := &deliveryRegistry{}
	server := httptest.NewServer(registry)
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	var inspecting atomic.Bool
	opaque := map[string]bool{}
	for _, value := range []string{"opaque component config", "chart", "same-image", "transitive-tool"} {
		opaque[digest.FromString(value).String()] = true
	}
	client := destinationTestClient(t, host, inspectRoundTripper(func(r *http.Request) (*http.Response, error) {
		_, blob, isBlob := strings.Cut(r.URL.Path, "/blobs/")
		if inspecting.Load() && (isBlob && opaque[blob] || r.Method != http.MethodGet && r.Method != http.MethodHead) {
			t.Errorf("inspection accessed application payload or wrote registry: %s %s", r.Method, r.URL)
			return nil, errors.New("unexpected inspection operation")
		}
		return http.DefaultTransport.RoundTrip(r)
	}))
	overrides := bale.DeliveryFile{Bundle: f.created.Descriptor.Digest, Artifacts: []bale.DeliveryArtifact{
		{Ref: destinationRef(f.chart), Path: "application"},
		{Ref: destinationRef(f.imageA), Path: "runtime/production"},
		{Ref: destinationRef(f.imageB), Path: "runtime/staging"},
		{Ref: destinationRef(f.tool), Path: "tools"},
	}}
	plan, err := client.PlanPush(t.Context(), f.layout, host+"/release:complete", bale.PushOptions{Delivery: &overrides})
	if err != nil {
		t.Fatal(err)
	}
	if f.imageA.Descriptor.Digest != f.imageB.Descriptor.Digest {
		t.Fatal("fixture did not retain identical image bytes")
	}
	result, err := client.Push(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Destinations) != len(plan.Destinations) || len(plan.Destinations) < 9 {
		t.Fatalf("missing application/platform/nested records: planned=%d published=%d", len(plan.Destinations), len(result.Destinations))
	}

	for _, attachment := range result.Destinations {
		if !attachment.Published || !attachment.Discoverable {
			t.Fatalf("unconfirmed attachment: %+v", attachment)
		}
	}
	inspecting.Store(true)
	records, err := client.InspectDestinations(t.Context(), host+"/release/application@"+f.chart.Descriptor.Digest.String())
	if err != nil || len(records) != 1 {
		t.Fatalf("chart records=%+v err=%v", records, err)
	}
	record := records[0]
	if !record.Local || record.Record.Source != destinationRef(f.chart) || record.Record.Destination != host+"/release/application@"+f.chart.Descriptor.Digest.String() || record.Record.Bundle != host+"/release@"+f.created.Descriptor.Digest.String() {
		t.Fatalf("incorrect canonical chart record: %+v", record)
	}
	contexts := map[string]bale.DestinationResolution{}
	for _, resolution := range record.Record.Resolutions {
		if resolution.Declaration == nil {
			t.Fatalf("chart resolution has no declaration: %+v", resolution)
		}
		contexts[resolution.Declaration.Context] = resolution
	}
	for _, tc := range []struct {
		context string
		capture digest.Digest
		image   bale.Root
		path    string
	}{
		{"production", f.inner.Descriptor.Digest, f.imageA, "runtime/production"},
		{"staging", f.created.Descriptor.Digest, f.imageB, "runtime/staging"},
	} {
		resolution, ok := contexts[tc.context]
		if !ok || resolution.Capture != tc.capture || resolution.Outcome != "selected" || len(resolution.Dependencies) != 1 {
			t.Fatalf("lost owning capture/context or added transitive deps: %+v", resolution)
		}
		dependency := resolution.Dependencies[0]
		if dependency.Ref != tc.image.Repository+":stable" || dependency.Source != destinationRef(tc.image) || dependency.Destination != host+"/release/"+tc.path+"@"+tc.image.Descriptor.Digest.String() {
			t.Fatalf("wrong direct dependency mapping: %+v", dependency)
		}
	}
	for _, tc := range []struct {
		path    string
		root    bale.Root
		outcome string
	}{
		{"runtime/production", f.imageA, "selected"}, {"runtime/staging", f.imageB, "absent"}, {"tools", f.tool, "disabled"},
	} {
		got, err := client.InspectDestinations(t.Context(), host+"/release/"+tc.path+"@"+tc.root.Descriptor.Digest.String())
		if err != nil || len(got) != 1 || got[0].Record.Source != destinationRef(tc.root) || len(got[0].Record.Resolutions) != 1 || got[0].Record.Resolutions[0].Outcome != tc.outcome {
			t.Fatalf("lost %s observation: %+v %v", tc.outcome, got, err)
		}
	}
	// Root placement overrides do not implicitly override each platform's
	// canonical placement. Incidental structural copies receive no new record.
	store := oci.NewStorageFromFS(os.DirFS(f.layout))
	raw, err := content.FetchAll(t.Context(), store, f.imageA.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var imageIndex v1.Index
	if err := json.Unmarshal(raw, &imageIndex); err != nil {
		t.Fatal(err)
	}
	for _, image := range []bale.Root{f.imageA, f.imageB} {
		for _, platform := range imageIndex.Manifests {
			placement, ok := plan.Relocations.Lookup(bale.ArtifactID{Repository: image.Repository, Digest: platform.Digest})
			if !ok {
				t.Fatalf("platform missing canonical relocation: %s %s", image.Repository, platform.Digest)
			}
			got, err := client.InspectDestinations(t.Context(), placement.Destination.Repository+"@"+platform.Digest.String())
			if err != nil || len(got) != 1 || !got[0].Local || len(got[0].Record.Resolutions) != 1 || got[0].Record.Resolutions[0].Outcome != "absent" {
				t.Fatalf("platform record in %s: %+v %v", placement.Destination.Repository, got, err)
			}
		}
	}
	for _, path := range []string{"runtime/production", "runtime/staging"} {
		for _, platform := range imageIndex.Manifests {
			got, err := client.InspectDestinations(t.Context(), host+"/release/"+path+"@"+platform.Digest.String())
			if err != nil || got == nil || len(got) != 0 {
				t.Fatalf("incidental platform copy received new record: %+v %v", got, err)
			}
		}
	}

	// New records occupy their own family and do not become producer choices.
	declarations, err := client.InspectDependencies(t.Context(), host+"/release/application@"+f.chart.Descriptor.Digest.String())
	if err != nil || len(declarations) != 2 {
		t.Fatalf("destination records interfered with producer declarations: %+v %v", declarations, err)
	}
	for _, planned := range plan.Destinations {
		placement, ok := result.Relocations.Lookup(bale.ArtifactID{Repository: strings.Split(planned.Record.Source, "@")[0], Digest: planned.Subject.Digest})
		if !ok || planned.Record.Destination != placement.Destination.Repository+"@"+placement.Destination.Digest.String() {
			t.Fatalf("record lacks confirmed canonical mapping: %+v", planned)
		}
		raw, err := content.FetchAll(t.Context(), store, planned.Subject)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			ArtifactType string `json:"artifactType"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.ArtifactType == bale.MetadataArtifactType || envelope.ArtifactType == bale.DependencyArtifactType || strings.HasPrefix(envelope.ArtifactType, "application/vnd.bale.destination.") {
			t.Fatalf("generated record about excluded metadata: %+v", planned)
		}
		if planned.Subject.Digest == f.created.Descriptor.Digest {
			t.Fatal("generated outer completion received application destination metadata")
		}
		if planned.Record.Source == destinationRef(f.chart) && planned.Repository != host+"/release/application" {
			t.Fatal("generated record for incidental chart placement")
		}
	}
}

func TestPublicDestinationRecordsWaitForClosureAndRepairPublication(t *testing.T) {
	f := newDestinationFixture(t, false)
	for _, failure := range []string{"dependency upload", "record manifest", "record inventory"} {
		t.Run(failure, func(t *testing.T) {
			registry := &deliveryRegistry{}
			var fail atomic.Bool
			var recordWrites atomic.Int32
			fail.Store(true)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var isRecord bool
				if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/") {
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						http.Error(w, "body", 500)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(raw))
					var manifest v1.Manifest
					if json.Unmarshal(raw, &manifest) == nil && manifest.ArtifactType == bale.DestinationArtifactType {
						isRecord = true
						recordWrites.Add(1)
					}
				}
				reject := failure == "dependency upload" && r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/manifests/"+f.imageA.Descriptor.Digest.String())
				reject = reject || failure == "record manifest" && isRecord
				reject = reject || failure == "record inventory" && r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/manifests/"+strings.Replace(f.chart.Descriptor.Digest.String(), ":", "-", 1)) && recordWrites.Load() > 0
				if fail.Load() && reject {
					http.Error(w, "injected publication failure", http.StatusForbidden)
					return
				}
				registry.ServeHTTP(w, r)
			}))
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "http://")
			client := destinationTestClient(t, host, nil)
			plan, err := client.PlanPush(t.Context(), f.layout, host+"/release:complete", bale.PushOptions{})
			if err != nil {
				t.Fatal(err)
			}
			partial, err := client.Push(t.Context(), plan)
			if err == nil {
				t.Fatal("injected failure did not stop delivery")
			}
			if failure == "dependency upload" && (recordWrites.Load() != 0 || len(partial.Destinations) != 0) {
				t.Fatalf("records preceded verified dependency closure: writes=%d result=%+v", recordWrites.Load(), partial.Destinations)
			}
			completion := registry.repository("release")
			completion.mu.Lock()
			_, published := completion.tags["complete"]
			completion.mu.Unlock()
			if published {
				t.Fatal("completion published despite destination record failure")
			}
			fail.Store(false)
			result, err := client.Push(t.Context(), plan)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Destinations) != len(plan.Destinations) {
				t.Fatalf("retry omitted planned records: %+v", result.Destinations)
			}
			for i, attachment := range result.Destinations {
				if !attachment.Published || !attachment.Discoverable || attachment.Descriptor.Digest != plan.Destinations[i].Descriptor.Digest {
					t.Fatalf("retry changed record or failed discovery: %+v", attachment)
				}
			}
			repeated, err := client.PlanPush(t.Context(), f.layout, host+"/release:complete", bale.PushOptions{})
			if err != nil || !reflect.DeepEqual(plan.Destinations, repeated.Destinations) {
				t.Fatalf("destination planning is not deterministic: %v", err)
			}
		})
	}
}

func TestPublicDestinationGeneratedManagedTagCollisionFailsBeforeWrites(t *testing.T) {
	f := newDestinationFixture(t, false)
	registry := &deliveryRegistry{}
	server := httptest.NewServer(registry)
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	client := destinationTestClient(t, host, nil)
	plan, err := client.PlanPush(t.Context(), f.layout, host+"/release:complete", bale.PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Destinations) == 0 {
		t.Fatal("no generated record subjects")
	}
	first := plan.Destinations[0]
	for _, candidate := range plan.Destinations {
		if candidate.Record.Source == destinationRef(f.tool) {
			first = candidate
			break
		}
	}
	if first.Record.Source != destinationRef(f.tool) {
		t.Fatal("missing producer-free tool record for generated-only collision")
	}
	repo := registry.repository(strings.TrimPrefix(first.Repository, host+"/"))
	// A valid unrelated artifact occupies the tag which generated destination
	// discovery needs. Existing captured producer inventories alone are insufficient
	// to reveal this collision for an artifact with no producer declaration.
	fixture := sdkFixture(t)
	raw, err := content.FetchAll(t.Context(), fixture.Store, fixture.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	tag := strings.Replace(first.Subject.Digest.String(), ":", "-", 1)
	repo.mu.Lock()
	repo.objects[fixture.Descriptor.Digest.String()] = raw
	repo.types[fixture.Descriptor.Digest.String()] = fixture.Descriptor.MediaType
	repo.tags[tag] = fixture.Descriptor.Digest.String()
	repo.mu.Unlock()
	_, err = client.Push(t.Context(), plan)
	if !errors.Is(err, bale.ErrConflict) {
		t.Fatalf("managed tag collision was not conflict: %v", err)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for name, repository := range registry.repositories {
		repository.mu.Lock()
		writes := repository.writes
		repository.mu.Unlock()
		if writes != 0 {
			t.Fatalf("preflight wrote %d requests to %s", writes, name)
		}
	}
}

func TestPublicDestinationInspectionDistinguishesHistoricalAndLocalRecords(t *testing.T) {
	f := newDestinationFixture(t, false)
	registry := &deliveryRegistry{}
	server := httptest.NewServer(registry)
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	client := destinationTestClient(t, host, nil)
	plan, err := client.PlanPush(t.Context(), f.layout, host+"/first:complete", bale.PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Push(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	chartLocation, ok := plan.Relocations.Lookup(bale.ArtifactID{Repository: f.chart.Repository, Digest: f.chart.Descriptor.Digest})
	if !ok {
		t.Fatal("missing canonical chart")
	}
	firstReference := chartLocation.Destination.Repository + "@" + f.chart.Descriptor.Digest.String()
	var resolutions atomic.Int32
	pinReader := destinationTestClient(t, host, inspectRoundTripper(func(r *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(r)
		if strings.HasSuffix(r.URL.Path, "/manifests/stable") {
			resolutions.Add(1)
			repository := registry.repository(strings.TrimPrefix(chartLocation.Destination.Repository, host+"/"))
			repository.mu.Lock()
			repository.tags["stable"] = f.tool.Descriptor.Digest.String()
			repository.mu.Unlock()
		}
		return response, err
	}))
	historical, err := pinReader.InspectDestinations(t.Context(), chartLocation.Destination.Repository+":stable")
	if resolutions.Load() != 1 {
		t.Fatalf("subject tag resolved %d times", resolutions.Load())
	}
	if err != nil || len(historical) != 1 || !historical[0].Local {
		t.Fatalf("first record: %+v %v", historical, err)
	}
	// Producer selectors still describe the stopped original registry. Capturing
	// opaque historical records must not resolve their recorded locations.
	captureRef := host + "/captures:second"
	created, err := client.Create(t.Context(), bale.Recipe{Include: []bale.Selection{{Ref: firstReference, Dependencies: bale.DependencyPolicy{Follow: "none"}}}}, captureRef, bale.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(t.TempDir(), "second")
	if _, err := client.Pull(t.Context(), captureRef, layout, bale.PullLocked); err != nil {
		t.Fatal(err)
	}
	next, err := client.PlanPush(t.Context(), layout, host+"/second:complete", bale.PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Destinations) != 1 {
		t.Fatalf("historical record/declaration received a generated record: %+v", next.Destinations)
	}
	if _, err := client.Push(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	nextLocation, ok := next.Relocations.Lookup(bale.ArtifactID{Repository: chartLocation.Destination.Repository, Digest: f.chart.Descriptor.Digest})
	if !ok {
		t.Fatal("missing second chart placement")
	}
	records, err := client.InspectDestinations(t.Context(), nextLocation.Destination.Repository+"@"+f.chart.Descriptor.Digest.String())
	if err != nil || len(records) != 2 {
		t.Fatalf("historical/current records: %+v %v", records, err)
	}
	local, old := 0, 0
	for _, record := range records {
		if record.Local {
			local++
			if record.Record.Bundle != host+"/second@"+created.Descriptor.Digest.String() || record.Record.Source != firstReference {
				t.Fatalf("second record has incorrect current capture: %+v", record)
			}
		} else {
			old++
			if record.Descriptor.Digest != historical[0].Descriptor.Digest || !reflect.DeepEqual(record.Record, historical[0].Record) {
				t.Fatalf("historical record rewritten: %+v", record)
			}
		}
	}
	if local != 1 || old != 1 {
		t.Fatalf("local=%d historical=%d", local, old)
	}
}

func TestPublicDestinationInspectionRejectsInvalidMetadataWithoutPartialResults(t *testing.T) {
	f := newDestinationFixture(t, false)
	for _, failure := range []string{"malformed JSON", "missing payload", "corrupt payload", "oversized payload", "corrupt manifest", "oversized manifest", "wrong subject", "future type", "authorization", "transport", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			registry := &deliveryRegistry{}
			server := httptest.NewServer(registry)
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "http://")
			publisher := destinationTestClient(t, host, nil)
			plan, err := publisher.PlanPush(t.Context(), f.layout, host+"/release:complete", bale.PushOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := publisher.Push(t.Context(), plan); err != nil {
				t.Fatal(err)
			}
			var repository string
			var descriptor v1.Descriptor
			for _, attachment := range plan.Destinations {
				if attachment.Record.Source == destinationRef(f.chart) {
					repository, descriptor = attachment.Repository, attachment.Descriptor
				}
			}
			if repository == "" {
				t.Fatal("missing chart record")
			}
			repo := registry.repository(strings.TrimPrefix(repository, host+"/"))
			repo.mu.Lock()
			raw := append([]byte(nil), repo.objects[descriptor.Digest.String()]...)
			repo.mu.Unlock()
			var manifest v1.Manifest
			if err := json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			repo.mu.Lock()
			payload := append([]byte(nil), repo.objects[manifest.Layers[0].Digest.String()]...)
			repo.mu.Unlock()
			switch failure {
			case "malformed JSON":
				payload = []byte(`{"schemaVersion":1,"resolutions":null}`)
				manifest.Layers[0] = content.NewDescriptorFromBytes(bale.DestinationArtifactType, payload)
			case "oversized payload":
				manifest.Layers[0].Size = bale.DestinationMaxBytes + 1
			case "oversized manifest":
				manifest.Annotations = map[string]string{"large": strings.Repeat("x", int(bale.DestinationMaxBytes))}
			case "wrong subject":
				wrong := *manifest.Subject
				wrong.Size++
				manifest.Subject = &wrong
			case "future type":
				manifest.ArtifactType = "application/vnd.bale.destination.v2+json"
			}
			mutated := installInspectionManifest(t, repo, bale.Root{Repository: repository, Descriptor: f.chart.Descriptor}, manifest, payload)
			repo.mu.Lock()
			switch failure {
			case "missing payload":
				delete(repo.objects, manifest.Layers[0].Digest.String())
			case "corrupt payload":
				repo.corruptRead = manifest.Layers[0].Digest.String()
			case "corrupt manifest":
				repo.corruptRead = mutated.Digest.String()
			case "authorization":
				repo.failRead = manifest.Layers[0].Digest.String()
			}
			writes := repo.writes
			repo.mu.Unlock()
			cause := errors.New("destination transport failure")
			if failure == "cancellation" {
				cause = context.Canceled
			}
			reader := destinationTestClient(t, host, inspectRoundTripper(func(r *http.Request) (*http.Response, error) {
				if (failure == "transport" || failure == "cancellation") && strings.HasSuffix(r.URL.Path, "/blobs/"+manifest.Layers[0].Digest.String()) {
					return nil, cause
				}
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					return nil, errors.New("inspection wrote registry")
				}
				return http.DefaultTransport.RoundTrip(r)
			}))
			got, err := reader.InspectDestinations(t.Context(), repository+"@"+f.chart.Descriptor.Digest.String())
			if err == nil || got != nil {
				t.Fatalf("invalid record returned partial results=%+v err=%v", got, err)
			}
			if failure == "future type" && !errors.Is(err, bale.ErrUnsupported) {
				t.Fatalf("future type error=%v", err)
			}
			if failure == "authorization" {
				var response *errcode.ErrorResponse
				if errors.Is(err, bale.ErrIntegrity) || !errors.As(err, &response) || response.StatusCode != http.StatusForbidden {
					t.Fatalf("lost authorization status: %v", err)
				}
			}
			if (failure == "transport" || failure == "cancellation") && !errors.Is(err, cause) {
				t.Fatalf("lost read cause %v: %v", cause, err)
			}
			repo.mu.Lock()
			after := repo.writes
			repo.mu.Unlock()
			if after != writes {
				t.Fatal("inspection wrote registry")
			}
		})
	}
}

func TestPublicDestinationInspectionEmptyAndCancelled(t *testing.T) {
	_, client, subject := dependencyInspectionFixture(t)
	records, err := client.InspectDestinations(t.Context(), subject.Repository+":inspect")
	if err != nil || records == nil || len(records) != 0 {
		t.Fatalf("empty records=%+v err=%v", records, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	records, err = client.InspectDestinations(ctx, subject.Repository+":inspect")
	if !errors.Is(err, context.Canceled) || records != nil {
		t.Fatalf("cancelled records=%+v err=%v", records, err)
	}
}

func TestPublicDestinationSourceCorruptionPreventsPublication(t *testing.T) {
	f := newDestinationFixture(t, false)
	registry := &deliveryRegistry{}
	server := httptest.NewServer(registry)
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	client := destinationTestClient(t, host, nil)
	plan, err := client.PlanPush(t.Context(), f.layout, host+"/release:complete", bale.PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	payload := digest.FromString("transitive-tool")
	path := filepath.Join(f.layout, "blobs", payload.Algorithm().String(), payload.Encoded())
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupted-bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	partial, err := client.Push(t.Context(), plan)
	if err == nil || len(partial.Destinations) != 0 {
		t.Fatalf("corrupt source produced records=%+v err=%v", partial.Destinations, err)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for name, repo := range registry.repositories {
		repo.mu.Lock()
		writes := repo.writes
		repo.mu.Unlock()
		if writes != 0 {
			t.Fatalf("source verification wrote %d requests to %s", writes, name)
		}
	}
}
