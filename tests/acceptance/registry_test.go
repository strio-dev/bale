package acceptance_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
)

// TestRegistryAcceptance deliberately uses the packaged executable for every
// user-facing operation. SDK calls only seed fixtures and verify delivered bytes.
func TestRegistryAcceptance(t *testing.T) {
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
		t.Fatal("all BALE_ACCEPTANCE_BINARY, BALE_ACCEPTANCE_DISTRIBUTION and BALE_ACCEPTANCE_ZOT must be set")
	}
	for i, path := range paths {
		info, err := os.Stat(path)
		if err != nil || !filepath.IsAbs(path) || info.IsDir() || info.Mode()&0111 == 0 {
			t.Fatalf("%s must name an absolute executable: %q (%v)", names[i], path, err)
		}
	}
	for _, backend := range []struct{ name, binary, version string }{{"distribution", paths[1], "3.1.2"}, {"zot", paths[2], "2.1.22"}} {
		t.Run(backend.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			version := exec.CommandContext(ctx, backend.binary, "--version")
			raw, err := version.CombinedOutput()
			if err != nil || !strings.Contains(string(raw), backend.version) {
				t.Fatalf("backend version: %v\n%s", err, raw)
			}
			t.Logf("backend: %s", bytes.TrimSpace(raw))
			runRegistryCycle(t, ctx, paths[0], backend.name, backend.binary)
		})
	}
}

type registryProcess struct {
	host, storage, logPath string
	command                *exec.Cmd
	done                   chan error
	stopped                bool
}

func startRegistry(t *testing.T, ctx context.Context, kind, binary, role string) *registryProcess {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	storage := filepath.Join(dir, "storage")
	if err := os.Mkdir(storage, 0700); err != nil {
		t.Fatal(err)
	}
	var config []byte
	if kind == "distribution" {
		config = []byte(fmt.Sprintf("version: 0.1\nlog:\n  level: error\n  accesslog:\n    disabled: true\nstorage:\n  filesystem:\n    rootdirectory: %q\n  maintenance:\n    uploadpurging:\n      enabled: false\nhttp:\n  addr: %q\n  secret: bale-acceptance-only\n  draintimeout: 2s\nhealth:\n  storagedriver:\n    enabled: false\n", storage, host))
	} else {
		_, port, err := net.SplitHostPort(host)
		if err != nil {
			t.Fatal(err)
		}
		config = []byte(fmt.Sprintf(`{"distSpecVersion":"1.1.1","storage":{"rootDirectory":%q,"gc":false,"dedupe":false,"hydrateBlobOnRead":false},"http":{"address":"127.0.0.1","port":%q},"log":{"level":"error","output":"/dev/stderr"}}`, storage, port))
	}
	configPath := filepath.Join(dir, "config")
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "registry.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, binary, "serve", configPath)
	// Registry environment overrides must not escape the generated isolated config.
	command.Env = []string{"OTEL_TRACES_EXPORTER=none"}
	command.Stdout = log
	command.Stderr = log
	if err := command.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	p := &registryProcess{host: host, storage: storage, logPath: logPath, command: command, done: make(chan error, 1)}
	go func() { err := command.Wait(); _ = log.Close(); p.done <- err }()
	t.Cleanup(func() {
		p.stop(t)
		if t.Failed() {
			raw, _ := os.ReadFile(logPath)
			t.Logf("%s registry logs:\n%s", role, tail(raw, 16000))
		}
	})
	client := &http.Client{Timeout: 300 * time.Millisecond}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		response, err := client.Get("http://" + host + "/v2/")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return p
			}
		}
		select {
		case err := <-p.done:
			p.stopped = true
			t.Fatalf("%s exited during readiness: %v", role, err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-deadline.C:
			t.Fatalf("%s registry did not become ready", role)
		case <-time.After(25 * time.Millisecond):
		}
	}
}
func (p *registryProcess) stop(t *testing.T) {
	t.Helper()
	if p.stopped {
		return
	}
	p.stopped = true
	if err := p.command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("stop registry: %v", err)
	}
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		_ = p.command.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(time.Second):
			t.Error("registry failed to exit after kill")
		}
	}
}
func (p *registryProcess) restart(t *testing.T, ctx context.Context) {
	t.Helper()
	log, err := os.OpenFile(p.logPath, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, p.command.Path, p.command.Args[1:]...)
	command.Env = p.command.Env
	command.Stdout = log
	command.Stderr = log
	if err := command.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	p.command = command
	p.done = make(chan error, 1)
	p.stopped = false
	go func() { err := command.Wait(); _ = log.Close(); p.done <- err }()
	client := &http.Client{Timeout: 300 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get("http://" + p.host + "/v2/")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == 200 {
				return
			}
		}
		select {
		case err := <-p.done:
			p.stopped = true
			t.Fatalf("destination restart: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	t.Fatal("destination restart readiness timed out")
}

func assertStopped(t *testing.T, ctx context.Context, p *registryProcess) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+p.host+"/v2/", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err == nil {
		_ = response.Body.Close()
		t.Fatalf("stopped source still serves HTTP: %s", p.host)
	}
}

type cli struct {
	t      *testing.T
	ctx    context.Context
	binary string
	env    []string
	hosts  []string
}

func (c cli) run(phase string, args ...string) []byte {
	c.t.Helper()
	stdout, stderr, code := c.execute(phase, args...)
	if code != 0 {
		c.t.Fatalf("%s: exit %d\nstdout:\n%s\nstderr:\n%s", phase, code, tail(stdout, 16000), tail(stderr, 16000))
	}
	return stdout
}

func (c cli) execute(phase string, args ...string) ([]byte, []byte, int) {
	c.t.Helper()
	for _, host := range c.hosts {
		args = append(args, "--plain-http", host)
	}
	commandCtx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, c.binary, args...)
	command.Env = c.env
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	code := 0
	if err := command.Run(); err != nil {
		var status *exec.ExitError
		if !errors.As(err, &status) || commandCtx.Err() != nil {
			c.t.Fatalf("%s: bale %s: %v\nstdout:\n%s\nstderr:\n%s", phase, strings.Join(args, " "), err, tail(stdout.Bytes(), 16000), tail(stderr.Bytes(), 16000))
		}
		code = status.ExitCode()
	}
	c.t.Logf("%s: bale %s", phase, strings.Join(args, " "))
	return stdout.Bytes(), stderr.Bytes(), code
}
func tail(raw []byte, n int) []byte {
	if len(raw) > n {
		return raw[len(raw)-n:]
	}
	return raw
}
func decode[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var result T
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode CLI report: %v\n%s", err, tail(raw, 16000))
	}
	return result
}
func writeFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func runRegistryCycle(t *testing.T, ctx context.Context, binary, kind, backend string) {
	source := startRegistry(t, ctx, kind, backend, "source")
	holding := startRegistry(t, ctx, kind, backend, "holding")
	destination := startRegistry(t, ctx, kind, backend, "destination")
	upstream, err := url.Parse("http://" + destination.host)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	publication := &publicationProbe{}
	var requests atomic.Int64
	var phase atomic.Value
	phase.Store("initial setup")
	var requestMu sync.Mutex
	var requestLog []string
	// Keep a dedicated loopback address for this fixture. Every OCI API request
	// to the advertised destination is counted, including requests that fail.
	listener, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(registryAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		number := requests.Add(1)
		entry := fmt.Sprintf("request=%d time=%s phase=%q method=%q path=%q host=%q remote=%q userAgent=%q", number, time.Now().UTC().Format(time.RFC3339Nano), phase.Load(), r.Method, r.URL.RequestURI(), r.Host, r.RemoteAddr, r.UserAgent())
		requestMu.Lock()
		if len(requestLog) == 32 {
			copy(requestLog, requestLog[1:])
			requestLog = requestLog[:31]
		}
		requestLog = append(requestLog, entry)
		requestMu.Unlock()
		if !publication.reject(w, r) {
			proxy.ServeHTTP(w, r)
		}
	})))
	if err := server.Listener.Close(); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	defer server.Close()
	destinationHost := strings.TrimPrefix(server.URL, "http://")
	dir := t.TempDir()
	dockerConfig := filepath.Join(dir, "credentials")
	if err := os.Mkdir(dockerConfig, 0700); err != nil {
		t.Fatal(err)
	}
	env := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "DOCKER_CONFIG=") {
			env = append(env, entry)
		}
	}
	env = append(env, "DOCKER_CONFIG="+dockerConfig)
	command := cli{t: t, ctx: ctx, binary: binary, env: env, hosts: []string{source.host, holding.host, destinationHost}}
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: command.hosts, HTTPClient: &http.Client{Timeout: 10 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	subject := seed(t, ctx, client, source.host, "application", "component-v1", false)
	previous := seed(t, ctx, client, source.host, "runtime", "runtime-v1", true)
	attachment, err := bale.PrepareDependencyDeclaration(ctx, subject, bale.DependencyDeclaration{Context: "acceptance", Dependencies: []bale.DependencyReference{{Ref: previous.Repository + ":stable"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Attach(ctx, attachment); err != nil {
		t.Fatal(err)
	}
	recipe := filepath.Join(dir, "bale.yaml")
	writeFile(t, recipe, []byte("include:\n  - ref: "+subject.Repository+":stable\n"))
	oldFile := filepath.Join(dir, "old.delivery.yaml")
	newFile := filepath.Join(dir, "updated.delivery.yaml")
	bundleRef := holding.host + "/bundles:before"
	created := decode[bale.CreateResult](t, command.run("capture", "create", bundleRef, "--file", recipe, "--output-delivery", oldFile, "--format", "json"))
	locked, latest := filepath.Join(dir, "locked"), filepath.Join(dir, "latest")
	command.run("locked pull", "pull", bundleRef, locked)
	data, err := os.ReadFile(oldFile)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := bale.DecodeDeliveryFile(data)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	oldID := previous.Repository + "@" + previous.Descriptor.Digest.String()
	for i := range delivery.Artifacts {
		if delivery.Artifacts[i].Ref == oldID {
			delivery.Artifacts[i].Path = "curated/runtime"
			found = true
		}
	}
	if !found {
		t.Fatal("delivery export omitted runtime root")
	}
	data, err = bale.EncodeDeliveryFile(delivery)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, oldFile, data)
	refreshed := seed(t, ctx, client, source.host, "runtime", "runtime-v2", true)
	pulled := decode[bale.PullResult](t, command.run("latest refresh", "pull", bundleRef, latest, "--mode", "latest", "--format", "json"))
	if pulled.Descriptor.Digest == created.Descriptor.Digest {
		t.Fatal("latest pull did not refresh moved dependency")
	}
	source.stop(t)
	assertStopped(t, ctx, source)
	comparison := decode[bale.DiffResult](t, command.run("offline diff", "diff", "--layout", locked, latest, "--format", "json"))
	if !comparison.Changed {
		t.Fatal("diff did not detect changed snapshot")
	}
	moved := false
	for _, resolution := range comparison.Resolutions {
		if resolution.Kind == "dependency" && resolution.Ref == refreshed.Repository+":stable" && len(resolution.Before) == 1 && len(resolution.After) == 1 && resolution.Before[0].Digest == previous.Descriptor.Digest && resolution.After[0].Digest == refreshed.Descriptor.Digest {
			moved = true
		}
	}
	if !moved {
		t.Fatalf("diff omitted exact dependency movement: %+v", comparison.Resolutions)
	}
	update := decode[bale.DeliveryUpdateResult](t, command.run("offline delivery update", "delivery", "update", "--layout", locked, latest, "--file", oldFile, "--output", newFile, "--format", "json"))
	if update.Delivery == nil || len(update.Conflicts) != 0 || update.Delivery.Bundle != pulled.Descriptor.Digest {
		t.Fatal("delivery update did not bind refreshed snapshot")
	}
	newID := refreshed.Repository + "@" + refreshed.Descriptor.Digest.String()
	found = false
	for _, row := range update.Delivery.Artifacts {
		if row.Ref == newID && row.Path == "curated/runtime" {
			found = true
		}
		if row.Ref == oldID {
			t.Fatal("updated delivery retained obsolete runtime")
		}
	}
	if !found {
		t.Fatal("updated delivery lost curated runtime path")
	}
	completion := destinationHost + "/release:bundle"
	phase.Store("destination shutdown before preview")
	destination.stop(t)
	beforeStorage := storageSnapshot(t, destination.storage)
	phase.Store("offline preview")
	beforeRequests := requests.Load()
	previewStarted := time.Now().UTC()
	plan := decode[bale.DeliveryPlan](t, command.run("offline preview", "push", latest, completion, "--delivery", newFile, "--dry-run", "--format", "json"))
	if requests.Load() != beforeRequests {
		requestMu.Lock()
		entries := append([]string(nil), requestLog...)
		requestMu.Unlock()
		t.Fatalf("preview contacted destination: before=%d after=%d previewStarted=%s\n%s", beforeRequests, requests.Load(), previewStarted.Format(time.RFC3339Nano), strings.Join(entries, "\n"))
	}
	if !reflect.DeepEqual(beforeStorage, storageSnapshot(t, destination.storage)) {
		t.Fatal("preview changed destination storage")
	}
	placement, ok := plan.Relocations.Lookup(bale.ArtifactID{Repository: refreshed.Repository, Digest: refreshed.Descriptor.Digest})
	if !ok || placement.Destination.Repository != destinationHost+"/release/curated/runtime" {
		t.Fatal("preview ignored updated runtime path")
	}
	if plan.Descriptor.Digest != pulled.Descriptor.Digest || len(plan.Destinations) == 0 {
		t.Fatal("preview omitted snapshot or destination attachments")
	}
	phase.Store("destination restart and publication")
	destination.restart(t, ctx)
	result := decode[bale.PushResult](t, command.run("offline delivery", "push", latest, completion, "--delivery", newFile, "--format", "json"))
	if result.Descriptor.Digest != plan.Descriptor.Digest {
		t.Fatalf("actual bundle digest %s disagrees with preview %s", result.Descriptor.Digest, plan.Descriptor.Digest)
	}
	if len(result.Relocations) != len(plan.Relocations) {
		t.Fatalf("actual relocation count %d disagrees with preview %d", len(result.Relocations), len(plan.Relocations))
	}
	// Confirmation follows physical publication order; compare the complete
	// source-keyed mappings rather than imposing planning's sort order.
	for _, planned := range plan.Relocations {
		confirmed, ok := result.Relocations.Lookup(planned.Source)
		if !ok || !reflect.DeepEqual(confirmed, planned) {
			t.Fatalf("actual relocation disagrees with preview: planned=%+v confirmed=%+v found=%t", planned, confirmed, ok)
		}
	}
	if len(result.Destinations) != len(plan.Destinations) {
		t.Fatalf("actual destination attachment count %d disagrees with preview %d", len(result.Destinations), len(plan.Destinations))
	}
	for _, planned := range plan.Destinations {
		found := false
		for _, confirmed := range result.Destinations {
			if confirmed.Descriptor.Digest == planned.Descriptor.Digest && confirmed.Subject.Digest == planned.Subject.Digest && confirmed.Repository == planned.Repository {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("actual delivery omitted planned destination attachment: %+v", planned)
		}
	}
	for _, attachment := range result.Destinations {
		if !attachment.Published || !attachment.Discoverable {
			t.Fatal("destination attachment not confirmed discoverable")
		}
	}
	for _, root := range []bale.Root{subject, refreshed} {
		location, ok := plan.Relocations.Lookup(bale.ArtifactID{Repository: root.Repository, Digest: root.Descriptor.Digest})
		if !ok {
			t.Fatal("missing tagged root placement")
		}
		resolved, err := client.Resolve(ctx, location.Destination.Repository+":stable")
		if err != nil || resolved.Descriptor.Digest != root.Descriptor.Digest {
			t.Fatalf("stable tag was not restored for %s: %v", location.Destination.Repository, err)
		}
	}
	for _, tag := range plan.Tags {
		resolved, err := client.Resolve(ctx, tag.Repository+":"+tag.Reference)
		if err != nil || resolved.Descriptor.Digest != tag.Descriptor.Digest {
			t.Fatalf("restored tag %s:%s: %+v %v", tag.Repository, tag.Reference, resolved.Descriptor, err)
		}
	}
	// Verify complete subject and multi-platform graph bytes in canonical paths,
	// independently of the bundle's holding-repository copies.
	for _, root := range []bale.Root{subject, refreshed} {
		location, ok := plan.Relocations.Lookup(bale.ArtifactID{Repository: root.Repository, Digest: root.Descriptor.Digest})
		if !ok {
			t.Fatal("missing canonical root")
		}
		resolved, err := client.Resolve(ctx, location.Destination.Repository+"@"+root.Descriptor.Digest.String())
		if err != nil {
			t.Fatal(err)
		}
		verifyGraph(t, ctx, root.Store, resolved.Store, root.Descriptor)
	}
	subjectPlacement, ok := plan.Relocations.Lookup(bale.ArtifactID{Repository: subject.Repository, Digest: subject.Descriptor.Digest})
	if !ok {
		t.Fatal("missing subject placement")
	}
	records := decode[[]bale.InspectedDestination](t, command.run("discover destination records", "destinations", "inspect", subjectPlacement.Destination.Repository+"@"+subject.Descriptor.Digest.String()))
	if len(records) != 1 || !records[0].Local || records[0].Record.Bundle != destinationHost+"/release@"+pulled.Descriptor.Digest.String() {
		t.Fatalf("incorrect destination records: %+v", records)
	}
	if len(records[0].Record.Resolutions) != 1 || len(records[0].Record.Resolutions[0].Dependencies) != 1 || records[0].Record.Resolutions[0].Dependencies[0].Destination != placement.Destination.Repository+"@"+refreshed.Descriptor.Digest.String() {
		t.Fatalf("destination record lost relocated dependency: %+v", records)
	}
	holding.stop(t)
	assertStopped(t, ctx, holding)
	phase.Store("publication failure, retry and conflict checks")
	runPublicationChecks(t, ctx, client, command, publication, latest, newFile, destinationHost, subject, refreshed)
	finalLayout := filepath.Join(dir, "destination-only")
	command.run("destination-only locked pull", "pull", completion, finalLayout)
	final := decode[[]bale.InspectResult](t, command.run("destination-only inspect", "inspect", "--layout", finalLayout))
	if len(final) != 1 || final[0].Descriptor.Digest != pulled.Descriptor.Digest {
		t.Fatal("final pull changed bundle digest")
	}
	if len(final[0].Metadata.Dependencies) == 0 {
		t.Fatal("final inventory omitted dependency observations")
	}
	selected := false
	for _, observation := range final[0].Metadata.Dependencies {
		if observation.Subject.Digest == subject.Descriptor.Digest && observation.Outcome == "selected" && observation.Declaration != nil && len(observation.Dependencies) == 1 && observation.Dependencies[0].Ref == refreshed.Repository+":stable" && observation.Dependencies[0].Descriptor.Digest == refreshed.Descriptor.Digest {
			selected = true
		}
	}
	if !selected {
		t.Fatal("final inventory lost original selector, selected declaration or refreshed pin")
	}
	oldFound, newFound, subjectFound := false, false, false
	for _, origin := range final[0].Metadata.Origins {
		oldFound = oldFound || origin.Descriptor.Digest == previous.Descriptor.Digest
		newFound = newFound || origin.Descriptor.Digest == refreshed.Descriptor.Digest
		subjectFound = subjectFound || origin.Descriptor.Digest == subject.Descriptor.Digest
	}
	if oldFound || !newFound || !subjectFound {
		t.Fatal("final captured inventory does not match refreshed inputs")
	}
	assertStopped(t, ctx, source)
	t.Logf("completed snapshot %s -> %s; %d relocations, %d destination records", created.Descriptor.Digest, pulled.Descriptor.Digest, len(plan.Relocations), len(records))
}

func storageSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(raw)
		result[rel] = hex.EncodeToString(hash[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func seed(t *testing.T, ctx context.Context, client *bale.Client, host, name, value string, multi bool) bale.Root {
	t.Helper()
	store := memory.New()
	put := func(media string, raw []byte) v1.Descriptor {
		d := content.NewDescriptorFromBytes(media, raw)
		if err := store.Push(ctx, d, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		return d
	}
	layer := put(v1.MediaTypeImageLayer, make([]byte, 1024)) // deterministic empty tar archive
	manifest := func(arch string) v1.Descriptor {
		config := put(v1.MediaTypeImageConfig, []byte(fmt.Sprintf(`{"architecture":%q,"os":"linux","rootfs":{"type":"layers","diff_ids":[%q]},"config":{"Labels":{"acceptance":%q}}}`, arch, layer.Digest, value)))
		raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{layer}})
		if err != nil {
			t.Fatal(err)
		}
		d := put(v1.MediaTypeImageManifest, raw)
		d.Platform = &v1.Platform{OS: "linux", Architecture: arch}
		return d
	}
	descriptor := manifest("amd64")
	if multi {
		raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{descriptor, manifest("arm64")}})
		if err != nil {
			t.Fatal(err)
		}
		descriptor = put(v1.MediaTypeImageIndex, raw)
	} else {
		payload := put("application/vnd.example.component.v1+text", []byte(value))
		raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: "application/vnd.example.component.v1", Config: v1.Descriptor{MediaType: v1.MediaTypeEmptyJSON, Digest: digest.FromString("{}"), Size: 2, Data: []byte("{}")}, Layers: []v1.Descriptor{payload}})
		if err != nil {
			t.Fatal(err)
		}
		put(v1.MediaTypeEmptyJSON, []byte("{}"))
		descriptor = put(v1.MediaTypeImageManifest, raw)
	}
	root := bale.Root{Repository: "fixture.example/" + name, Descriptor: descriptor, Store: store, Referrers: bale.ReferrerPolicy{Include: "none"}}
	plan, err := client.Plan(ctx, bale.TransferRequest{Roots: []bale.Root{root}, Destination: host + "/source"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Execute(ctx, plan); err != nil {
		t.Fatal(err)
	}
	root.Repository = host + "/source/" + name
	// An external producer owns and advances its mutable source tag. Bale's
	// publication API intentionally refuses to overwrite existing tag identities.
	repository, err := remote.NewRepository(root.Repository)
	if err != nil {
		t.Fatal(err)
	}
	repository.PlainHTTP = true
	repository.Client = &http.Client{Timeout: 10 * time.Second}
	if err := repository.Tag(ctx, root.Descriptor, "stable"); err != nil {
		t.Fatal(err)
	}
	return root
}
func verifyGraph(t *testing.T, ctx context.Context, expected, actual content.ReadOnlyStorage, root v1.Descriptor) {
	t.Helper()
	raw, err := content.FetchAll(ctx, expected, root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := content.FetchAll(ctx, actual, root)
	if err != nil || !bytes.Equal(raw, got) {
		t.Fatalf("destination graph bytes %s: %v", root.Digest, err)
	}
	switch root.MediaType {
	case v1.MediaTypeImageIndex:
		var index v1.Index
		if err := json.Unmarshal(raw, &index); err != nil {
			t.Fatal(err)
		}
		for _, child := range index.Manifests {
			verifyGraph(t, ctx, expected, actual, child)
		}
	case v1.MediaTypeImageManifest:
		var manifest v1.Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatal(err)
		}
		for _, blob := range append([]v1.Descriptor{manifest.Config}, manifest.Layers...) {
			a, err := content.FetchAll(ctx, expected, blob)
			if err != nil {
				t.Fatal(err)
			}
			b, err := content.FetchAll(ctx, actual, blob)
			if err != nil || !bytes.Equal(a, b) {
				t.Fatalf("destination blob %s: %v", blob.Digest, err)
			}
		}
	}
}
