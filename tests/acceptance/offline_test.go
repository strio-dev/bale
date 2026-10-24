package acceptance_test

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/internal/wasmtest"
	"github.com/strio-dev/bale/plugin"
	"oras.land/oras-go/v2/content/memory"
)

// TestOfflineAcceptance exercises the removable-media path with the packaged
// CLI. Fixture producers publish before the source is made strictly read-only.
func TestOfflineAcceptance(t *testing.T) {
	names := []string{"BALE_ACCEPTANCE_BINARY", "BALE_ACCEPTANCE_DISTRIBUTION", "BALE_ACCEPTANCE_ZOT"}
	paths := make([]string, len(names))
	present := 0
	for i, name := range names {
		paths[i] = os.Getenv(name)
		if paths[i] != "" {
			present++
		}
	}
	if present == 0 {
		t.Skip("run the registry acceptance app to supply packaged Bale, Distribution and Zot")
	}
	if present != len(names) {
		t.Fatal("all acceptance executable paths must be supplied")
	}
	for i, path := range paths {
		info, err := os.Stat(path)
		if err != nil || !filepath.IsAbs(path) || info.IsDir() || info.Mode()&0111 == 0 {
			t.Fatalf("%s must name an absolute executable: %q (%v)", names[i], path, err)
		}
	}
	for _, backend := range []struct{ name, binary string }{{"distribution", paths[1]}, {"zot", paths[2]}} {
		t.Run(backend.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			runOfflineCycle(t, ctx, paths[0], backend.name, backend.binary)
		})
	}
}

func runOfflineCycle(t *testing.T, ctx context.Context, binary, kind, backend string) {
	source := startRegistry(t, ctx, kind, backend, "offline source")
	destination := startRegistry(t, ctx, kind, backend, "offline destination")
	upstream, err := url.Parse("http://" + source.host)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var trace struct {
		sync.Mutex
		phase    string
		offline  bool
		requests []string
	}
	setPhase := func(phase string) {
		trace.Lock()
		defer trace.Unlock()
		trace.phase = phase
	}
	setPhase("fixture publication")
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		trace.Lock()
		defer trace.Unlock()
		for _, request := range trace.requests {
			t.Logf("source request after shutdown: %s", request)
		}
	})
	run := func(command cli, phase string, args ...string) []byte {
		setPhase(phase)
		defer setPhase("after " + phase)
		return command.run(phase, args...)
	}
	var readOnly atomic.Bool
	var sourceReads, sourceWrites atomic.Int64
	listener, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(registryAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace.Lock()
		if trace.offline {
			trace.requests = append(trace.requests, fmt.Sprintf("phase=%q method=%s path=%q host=%q remote=%q user-agent=%q", trace.phase, r.Method, r.URL.RequestURI(), r.Host, r.RemoteAddr, r.UserAgent()))
		}
		trace.Unlock()
		if readOnly.Load() {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				sourceWrites.Add(1)
				http.Error(w, "read-only source", http.StatusForbidden)
				return
			}
			sourceReads.Add(1)
		}
		proxy.ServeHTTP(w, r)
	})))
	if err := server.Listener.Close(); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	defer server.Close()
	sourceHost := strings.TrimPrefix(server.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{sourceHost, destination.host}, HTTPClient: &http.Client{Timeout: 10 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	subject := seed(t, ctx, client, sourceHost, "offline-component", "component-v1", false)
	runtime := seed(t, ctx, client, sourceHost, "offline-runtime", "runtime-v1", true)
	declaration, err := bale.PrepareDependencyDeclaration(ctx, subject, bale.DependencyDeclaration{Context: "offline acceptance", Dependencies: []bale.DependencyReference{{Ref: runtime.Repository + ":stable"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Attach(ctx, declaration); err != nil {
		t.Fatal(err)
	}
	nestedRecipe, err := bale.ParseRecipe([]byte("include:\n  - " + subject.Repository + ":stable\n"))
	if err != nil {
		t.Fatal(err)
	}
	nestedRef := sourceHost + "/bundles:nested"
	nested, err := client.Create(ctx, nestedRecipe, nestedRef, bale.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pluginSubject := seed(t, ctx, client, sourceHost, "offline-plugin-subject", "plugin-subject-v1", false)
	dir := t.TempDir()
	packagePath := filepath.Join(dir, "discovery-0.0.0.tgz")
	output := fmt.Sprintf(`{"schemaVersion":1,"dependencies":[{"ref":%q}]}`, runtime.Repository+":stable")
	writeFile(t, packagePath, wasmtest.Package(wasmtest.Command(output, 0)))
	pluginStore := memory.New()
	pluginDescriptor, err := plugin.Pack(ctx, pluginStore, packagePath)
	if err != nil {
		t.Fatal(err)
	}
	pluginRoot := bale.Root{Repository: "fixture.example/offline-plugin", Descriptor: pluginDescriptor, Store: pluginStore, Referrers: bale.ReferrerPolicy{Include: "none"}, Aliases: []string{"v1"}}
	pluginPlan, err := client.Plan(ctx, bale.TransferRequest{Roots: []bale.Root{pluginRoot}, Destination: sourceHost + "/plugins"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Execute(ctx, pluginPlan); err != nil {
		t.Fatal(err)
	}
	pluginRef := sourceHost + "/plugins/offline-plugin:v1"
	recipe := filepath.Join(dir, "bale.yaml")
	writeFile(t, recipe, []byte("include:\n  - "+nestedRef+"\n  - ref: "+pluginSubject.Repository+":stable\n    plugin:\n      ref: "+pluginRef+"\n"))
	dockerConfig := filepath.Join(dir, "docker")
	if err := os.Mkdir(dockerConfig, 0700); err != nil {
		t.Fatal(err)
	}
	command := cli{t: t, ctx: ctx, binary: binary, env: []string{"DOCKER_CONFIG=" + dockerConfig, "XDG_CACHE_HOME=" + filepath.Join(dir, "cache")}, hosts: []string{sourceHost, destination.host}}
	readOnly.Store(true)
	original := filepath.Join(dir, "release")
	deliveryPath := filepath.Join(dir, "bale.delivery.yaml")
	dry := decode[bale.CreateResult](t, run(command, "local capture preview", "create", "--layout", "-f", recipe, original, "--dry-run", "--format", "json"))
	if dry.Published || !dry.DryRun || dry.LayoutPath != "" {
		t.Fatal("local preview reported publication")
	}
	if _, err := os.Stat(original); !os.IsNotExist(err) {
		t.Fatal("dry run created a layout")
	}
	captured := decode[bale.CreateResult](t, run(command, "read-only local capture", "create", "--layout", "-f", recipe, original, "--output-delivery", deliveryPath, "--format", "json"))
	if captured.Published || captured.LayoutPath != original || captured.Descriptor.Digest != dry.Descriptor.Digest {
		t.Fatal("local capture changed digest or registry publication semantics")
	}
	if sourceWrites.Load() != 0 || sourceReads.Load() == 0 {
		t.Fatalf("source access: %d writes, %d reads", sourceWrites.Load(), sourceReads.Load())
	}
	before := decode[[]bale.InspectResult](t, run(command, "inspect captured local bundle", "inspect", "--layout", original))
	if len(before) != 1 || len(before[0].Metadata.Plugins) != 1 {
		t.Fatal("local capture omitted plugin provenance")
	}
	archive := filepath.Join(dir, "release.tar")
	setPhase("stop original source")
	source.stop(t)
	trace.Lock()
	trace.offline = true
	trace.Unlock()
	assertStopped(t, ctx, source)
	// A configured helper cannot be used by these offline commands: its sentinel
	// makes any invocation observable, and source registry storage is unavailable.
	helperDir := filepath.Join(dir, "helpers")
	if err := os.Mkdir(helperDir, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dir, "helper-called")
	helper := "#!/bin/sh\nprintf called > '" + sentinel + "'\nexit 1\n"
	helperPath := filepath.Join(helperDir, "docker-credential-bale-offline-proof")
	if err := os.WriteFile(helperPath, []byte(helper), 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dockerConfig, "config.json"), []byte(`{"credsStore":"bale-offline-proof"}`))
	offline := command
	offline.hosts = nil
	offline.env = append(append([]string{}, command.env...), "PATH="+helperDir)
	readsBefore := sourceReads.Load()
	saved := decode[bale.ArchiveResult](t, run(offline, "save offline archive", "save", original, "-o", archive, "--format", "json"))
	if saved.Descriptor.Digest != captured.Descriptor.Digest {
		t.Fatal("save changed bundle digest")
	}
	assertArchiveDeduplicated(t, archive)
	if err := os.RemoveAll(original); err != nil {
		t.Fatal(err)
	}
	restoredPath := filepath.Join(dir, "restored")
	loaded := decode[bale.ArchiveResult](t, run(offline, "load removable-media archive", "load", "-i", archive, restoredPath, "--format", "json"))
	if loaded.Descriptor.Digest != captured.Descriptor.Digest || loaded.LayoutPath != restoredPath || loaded.ArchivePath != archive {
		t.Fatal("load did not retain archive identity")
	}
	after := decode[[]bale.InspectResult](t, run(offline, "inspect restored bundle", "inspect", "--layout", restoredPath))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("archive roundtrip changed verified inventory")
	}
	raw, err := os.ReadFile(deliveryPath)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := bale.DecodeDeliveryFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	foundRuntime, foundNested, foundPlugin := false, false, false
	for i := range delivery.Artifacts {
		row := &delivery.Artifacts[i]
		if row.Ref == runtime.Repository+"@"+runtime.Descriptor.Digest.String() {
			row.Path = "curated/runtime"
			foundRuntime = true
		}
		foundNested = foundNested || strings.HasSuffix(row.Ref, "@"+nested.Descriptor.Digest.String())
		foundPlugin = foundPlugin || strings.HasSuffix(row.Ref, "@"+pluginDescriptor.Digest.String())
	}
	if !foundRuntime || !foundNested || !foundPlugin {
		t.Fatal("delivery export omitted nested, runtime or plugin artifact")
	}
	raw, err = bale.EncodeDeliveryFile(delivery)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, deliveryPath, raw)
	completion := destination.host + "/offline-release:bundle"
	plan := decode[bale.DeliveryPlan](t, run(offline, "preview restored delivery", "push", restoredPath, completion, "--delivery", deliveryPath, "--dry-run", "--format", "json"))
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("offline operation invoked credential helper")
	}
	if sourceReads.Load() != readsBefore || sourceWrites.Load() != 0 {
		t.Fatal("offline archive operations contacted source")
	}
	// Actual publication can use the normal empty credential configuration.
	if err := os.Remove(filepath.Join(dockerConfig, "config.json")); err != nil {
		t.Fatal(err)
	}
	result := decode[bale.PushResult](t, run(command, "deliver restored bundle", "push", restoredPath, completion, "--delivery", deliveryPath, "--format", "json"))
	if result.Descriptor.Digest != captured.Descriptor.Digest || len(result.Destinations) == 0 {
		t.Fatal("restored delivery changed bundle or omitted records")
	}
	for _, record := range result.Destinations {
		if !record.Published || !record.Discoverable {
			t.Fatal("destination record not discoverable")
		}
	}
	setPhase("verify delivered artifact graphs")
	for _, root := range []bale.Root{subject, runtime, pluginSubject} {
		location, ok := plan.Relocations.Lookup(bale.ArtifactID{Repository: root.Repository, Digest: root.Descriptor.Digest})
		if !ok {
			t.Fatalf("missing relocation: %s", root.Repository)
		}
		resolved, err := client.Resolve(ctx, location.Destination.Repository+":stable")
		if err != nil || resolved.Descriptor.Digest != root.Descriptor.Digest {
			t.Fatalf("restored stable alias: %v", err)
		}
		verifyGraph(t, ctx, root.Store, resolved.Store, root.Descriptor)
		if root.Repository == runtime.Repository && location.Destination.Repository != destination.host+"/offline-release/curated/runtime" {
			t.Fatal("delivery ignored authored runtime path")
		}
	}
	setPhase("verify delivered plugin graph")
	pluginLocation, ok := plan.Relocations.Lookup(bale.ArtifactID{Repository: sourceHost + "/plugins/offline-plugin", Digest: pluginDescriptor.Digest})
	if !ok {
		t.Fatal("missing portable plugin relocation")
	}
	deliveredPlugin, err := client.Resolve(ctx, pluginLocation.Destination.Repository+":v1")
	if err != nil || deliveredPlugin.Descriptor.Digest != pluginDescriptor.Digest {
		t.Fatalf("portable plugin alias: %v", err)
	}
	verifyGraph(t, ctx, pluginStore, deliveredPlugin.Store, pluginDescriptor)
	pluginSubjectLocation, ok := plan.Relocations.Lookup(bale.ArtifactID{Repository: pluginSubject.Repository, Digest: pluginSubject.Descriptor.Digest})
	if !ok {
		t.Fatal("missing plugin subject relocation")
	}
	records := decode[[]bale.InspectedDestination](t, run(command, "discover offline destination records", "destinations", "inspect", pluginSubjectLocation.Destination.Repository+"@"+pluginSubject.Descriptor.Digest.String()))
	if len(records) == 0 {
		t.Fatal("destination-only discovery omitted dependency record")
	}
	finalPath := filepath.Join(dir, "destination-only")
	run(command, "fresh destination-only pull", "pull", completion, finalPath)
	final := decode[[]bale.InspectResult](t, run(command, "verify destination-only bundle", "inspect", "--layout", finalPath))
	if len(final) != 1 {
		t.Fatalf("destination-only inspection returned %d bundles, want 1", len(final))
	}
	// Registry Resolve supplies root descriptor hints from HTTP metadata, while
	// local capture supplies ArtifactType explicitly. Compare content identity
	// and the complete verified inventory, without requiring transport hints.
	originalRoot, deliveredRoot := before[0].Descriptor, final[0].Descriptor
	identityEqual := originalRoot.Digest == deliveredRoot.Digest && originalRoot.Size == deliveredRoot.Size && originalRoot.MediaType == deliveredRoot.MediaType
	metadataEqual := reflect.DeepEqual(before[0].Metadata, final[0].Metadata)
	declarationsEqual := reflect.DeepEqual(before[0].Declarations, final[0].Declarations)
	if !identityEqual || !metadataEqual || !declarationsEqual {
		t.Fatalf("destination-only inspection mismatch: identityEqual=%t metadataEqual=%t declarationsEqual=%t\noriginal descriptor=%+v\ndelivered descriptor=%+v", identityEqual, metadataEqual, declarationsEqual, originalRoot, deliveredRoot)
	}
	diff := decode[bale.DiffResult](t, run(offline, "compare independent restored and delivered copies", "diff", "--layout", restoredPath, finalPath, "--format", "json"))
	if diff.Changed || diff.Before.Digest != captured.Descriptor.Digest || diff.After.Digest != captured.Descriptor.Digest {
		t.Fatal("delivered snapshot differs from archived snapshot")
	}
	if sourceReads.Load() != readsBefore || sourceWrites.Load() != 0 {
		t.Fatal("delivery contacted original source")
	}
	t.Logf("offline roundtrip complete: %s; nested bundle, multiarch dependency, portable plugin, %d relocations", captured.Descriptor.Digest, len(plan.Relocations))
}

func assertArchiveDeduplicated(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()
	reader := tar.NewReader(f)
	seen := map[string]bool{}
	blobs := 0
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if seen[header.Name] {
			t.Fatalf("duplicate archive entry: %s", header.Name)
		}
		seen[header.Name] = true
		if strings.HasPrefix(header.Name, "blobs/") && header.Typeflag == tar.TypeReg {
			blobs++
		}
	}
	if blobs == 0 {
		t.Fatal("archive contains no OCI blobs")
	}
}
