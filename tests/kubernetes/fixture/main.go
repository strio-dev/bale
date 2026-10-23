// Command fixture prepares runnable OCI artifacts for the Kubernetes acceptance test.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"go.yaml.in/yaml/v3"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

type version struct {
	OriginalImage string `json:"original_image"`
	ImageDigest   string `json:"image_digest"`
}
type manualImage struct {
	Ref     string `json:"ref"`
	Digest  string `json:"digest"`
	Version string `json:"version"`
}
type fixture struct {
	Override manualImage        `json:"override"`
	Source   string             `json:"source"`
	Dest     string             `json:"dest"`
	Chart    string             `json:"chart"`
	Versions map[string]version `json:"versions"`
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixture:", err)
		os.Exit(1)
	}
}
func write(path string, v any) {
	raw, e := json.MarshalIndent(v, "", "  ")
	must(e)
	must(os.WriteFile(path, append(raw, '\n'), 0644))
}
func main() {
	if len(os.Args) < 2 {
		must(errors.New("expected seed, retag, service-seed, or plugin-seed"))
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	source := fs.String("source", "", "source registry host:port")
	dest := fs.String("dest", "", "destination registry host:port")
	dir := fs.String("dir", "", "state directory (required)")
	pluginArchive := fs.String("plugin-archive", "", "Helm-packaged native plugin archive")
	serviceChart := fs.String("service-chart", "", "chart archive")
	selectedVersion := fs.String("version", "", "image version A or B for retag")
	workload := fs.String("workload", "", "static workload executable")
	registryCA := fs.String("registry-ca", "", "destination registry CA certificate")
	registryConfig := fs.String("registry-config", "", "Docker registry credentials file")
	must(fs.Parse(os.Args[2:]))
	must(validate(os.Args[1], *source, *dest, *dir, *workload, *serviceChart, *selectedVersion, *pluginArchive, fs.Args()))
	must(os.MkdirAll(*dir, 0755))
	ctx := context.Background()
	options, e := registryOptions(*source, *dest, *registryCA, *registryConfig)
	must(e)
	c, e := bale.NewClient(options)
	must(e)
	switch os.Args[1] {
	case "plugin-seed":
		raw, err := os.ReadFile(*pluginArchive)
		must(err)
		store := memory.New()
		layer := content.NewDescriptorFromBytes(v1.MediaTypeImageLayerGzip, raw)
		layer.Annotations = map[string]string{v1.AnnotationTitle: filepath.Base(*pluginArchive)}
		must(store.Push(ctx, layer, bytes.NewReader(raw)))
		descriptor, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, "application/vnd.helm.plugin.v1+json", oras.PackManifestOptions{Layers: []v1.Descriptor{layer}})
		must(err)
		root := bale.Root{Repository: "fixture.example/plugins/bale", Descriptor: descriptor, Store: store, Aliases: []string{"0.0.0"}, Referrers: bale.ReferrerPolicy{Include: "none"}}
		plan, err := c.Plan(ctx, bale.TransferRequest{Roots: []bale.Root{root}, Destination: *source, RepositoryMap: map[string]string{root.Repository: "plugins/bale"}})
		must(err)
		_, err = c.Execute(ctx, plan)
		must(err)
		write(filepath.Join(*dir, "plugin-seed.json"), map[string]string{"plugin": *source + "/plugins/bale@" + descriptor.Digest.String(), "tagged": *source + "/plugins/bale:0.0.0"})
	case "service-seed":
		chartRaw, err := os.ReadFile(*serviceChart)
		must(err)
		configRaw, chartVersion, err := helmChartConfig(chartRaw)
		must(err)
		store := memory.New()
		cfg := put(ctx, store, "application/vnd.cncf.helm.config.v1+json", configRaw)
		layer := put(ctx, store, "application/vnd.cncf.helm.chart.content.v1.tar+gzip", chartRaw)
		d := manifest(ctx, store, cfg, []v1.Descriptor{layer})
		root := bale.Root{Repository: "fixture.example/charts/bale-flux", Descriptor: d, Store: store, Aliases: []string{strings.ReplaceAll(chartVersion, "+", "_")}, Referrers: bale.ReferrerPolicy{Include: "none"}}
		plan, err := c.Plan(ctx, bale.TransferRequest{Roots: []bale.Root{root}, Destination: *source, RepositoryMap: map[string]string{root.Repository: "charts/bale-flux"}})
		must(err)
		_, err = c.Execute(ctx, plan)
		must(err)
		write(filepath.Join(*dir, "service-seed.json"), map[string]string{"chart": *source + "/charts/bale-flux@" + d.Digest.String(), "version": chartVersion})
	case "retag":
		repo, err := remote.NewRepository(*source + "/images/app")
		must(err)
		repo.PlainHTTP = true
		descriptor, err := repo.Resolve(ctx, strings.ToLower(*selectedVersion))
		must(err)
		must(repo.Tag(ctx, descriptor, "stable"))
		must(repo.Tag(ctx, descriptor, "hook"))
	case "seed":
		app, e := os.ReadFile(*workload)
		must(e)
		store := memory.New()
		layer := put(ctx, store, v1.MediaTypeImageLayer, tarBytes(map[string][]byte{"app": app}, 0755))
		roots := map[string]bale.Root{}
		for _, v := range []string{"A", "B", "CUSTOM"} {
			conf := v1.Image{Platform: v1.Platform{Architecture: runtime.GOARCH, OS: "linux"}, Config: v1.ImageConfig{Env: []string{"FIXTURE_VERSION=" + v}, Entrypoint: []string{"/app"}}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{layer.Digest}}}
			raw, e := json.Marshal(conf)
			must(e)
			cfg := put(ctx, store, v1.MediaTypeImageConfig, raw)
			d := manifest(ctx, store, cfg, []v1.Descriptor{layer})
			repository, alias := "images/app", strings.ToLower(v)
			if v == "CUSTOM" {
				repository, alias = "images/override", "custom"
			}
			root := bale.Root{Repository: "fixture.example/" + repository, Descriptor: d, Store: store, Aliases: []string{alias}, Referrers: bale.ReferrerPolicy{Include: "none"}}
			plan, e := c.Plan(ctx, bale.TransferRequest{Roots: []bale.Root{root}, Destination: *source, RepositoryMap: map[string]string{root.Repository: repository}})
			must(e)
			_, e = c.Execute(ctx, plan)
			must(e)
			root.Repository = *source + "/" + repository
			roots[v] = root
		}
		repo, e := remote.NewRepository(*source + "/images/app")
		must(e)
		repo.PlainHTTP = true
		must(repo.Tag(ctx, roots["A"].Descriptor, "stable"))
		must(repo.Tag(ctx, roots["A"].Descriptor, "hook"))
		chartRaw := chart(*source + "/images/app:stable")
		must(os.WriteFile(filepath.Join(*dir, "original-chart.tgz"), chartRaw, 0644))
		cfg := put(ctx, store, "application/vnd.cncf.helm.config.v1+json", []byte(`{"apiVersion":"v2","name":"app","version":"1.0.0"}`))
		layer = put(ctx, store, "application/vnd.cncf.helm.chart.content.v1.tar+gzip", chartRaw)
		d := manifest(ctx, store, cfg, []v1.Descriptor{layer})
		root := bale.Root{Repository: "fixture.example/charts/app", Descriptor: d, Store: store, Aliases: []string{"1.0.0"}, Referrers: bale.ReferrerPolicy{Include: "none"}}
		plan, e := c.Plan(ctx, bale.TransferRequest{Roots: []bale.Root{root}, Destination: *source, RepositoryMap: map[string]string{root.Repository: "charts/app"}})
		must(e)
		_, e = c.Execute(ctx, plan)
		must(e)
		f := fixture{Override: manualImage{Ref: *source + "/images/override:custom", Digest: roots["CUSTOM"].Descriptor.Digest.String(), Version: "CUSTOM"}, Source: *source, Dest: *dest, Chart: *source + "/charts/app@" + d.Digest.String(), Versions: map[string]version{}}
		for _, v := range []string{"A", "B"} {
			f.Versions[v] = version{OriginalImage: *source + "/images/app:stable", ImageDigest: roots[v].Descriptor.Digest.String()}
		}
		write(filepath.Join(*dir, "seed.json"), f)
	default:
		must(fmt.Errorf("unknown subcommand %q", os.Args[1]))
	}
}

// registryOptions keeps only the local source on HTTP when a destination CA is supplied.
// ORAS looks credentials up by the exact challenged registry host.
func registryOptions(source, destination, caFile, configFile string) (bale.ClientOptions, error) {
	options := bale.ClientOptions{PlainHTTPRegistries: nonempty(source, destination)}
	if configFile != "" {
		store, err := credentials.NewStore(configFile, credentials.StoreOptions{})
		if err != nil {
			return options, fmt.Errorf("open registry credentials: %w", err)
		}
		options.Credentials = credentials.Credential(store)
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return options, fmt.Errorf("read registry CA: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			return options, fmt.Errorf("load system roots: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return options, errors.New("registry CA contains no certificates")
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
		transport.ResponseHeaderTimeout = 30 * time.Second
		options.HTTPClient = &http.Client{Transport: transport, Timeout: 2 * time.Minute}
		options.PlainHTTPRegistries = nonempty(source)
	}
	return options, nil
}

// Validate before touching the filesystem or registry so invocation errors are actionable.
func validate(command, source, dest, dir, workload, serviceChart, selectedVersion, pluginArchive string, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(args, " "))
	}
	if dir == "" {
		return errors.New("--dir is required")
	}
	switch command {
	case "plugin-seed":
		if source == "" || pluginArchive == "" {
			return errors.New("plugin-seed requires --source and --plugin-archive")
		}
	case "seed":
		if source == "" || dest == "" || workload == "" {
			return errors.New("seed requires --source, --dest, and --workload")
		}
	case "retag":
		if source == "" || (selectedVersion != "A" && selectedVersion != "B") {
			return errors.New("retag requires --source and --version A or B")
		}
	case "service-seed":
		if source == "" || serviceChart == "" {
			return errors.New("service-seed requires --source and --service-chart")
		}
	default:
		return fmt.Errorf("unknown subcommand %q", command)
	}
	for _, registry := range []string{source, dest} {
		if registry != "" && (strings.Contains(registry, "/") || strings.ContainsAny(registry, " \t\r\n")) {
			return fmt.Errorf("registry must be a host[:port], got %q", registry)
		}
	}
	return nil
}

// helmChartConfig uses the packaged chart's metadata without changing its archive.
func helmChartConfig(archive []byte) ([]byte, string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, "", fmt.Errorf("open chart archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	var metadata map[string]any
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("read chart archive: %w", err)
		}
		name := path.Clean(header.Name)
		if path.Base(name) != "Chart.yaml" || strings.Count(name, "/") != 1 || header.Typeflag != tar.TypeReg {
			continue
		}
		if metadata != nil {
			return nil, "", errors.New("chart archive contains multiple top-level Chart.yaml entries")
		}
		if header.Size > 1<<20 {
			return nil, "", errors.New("chart metadata exceeds 1 MiB")
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			return nil, "", fmt.Errorf("read Chart.yaml: %w", err)
		}
		if err := yaml.Unmarshal(raw, &metadata); err != nil {
			return nil, "", fmt.Errorf("decode Chart.yaml: %w", err)
		}
	}
	for _, field := range []string{"apiVersion", "name", "version"} {
		value, ok := metadata[field].(string)
		if !ok || value == "" {
			return nil, "", fmt.Errorf("chart metadata requires a nonempty string %s", field)
		}
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, "", fmt.Errorf("encode chart config: %w", err)
	}
	return raw, metadata["version"].(string), nil
}

func nonempty(items ...string) []string {
	out := []string{}
	for _, s := range items {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
func put(ctx context.Context, s *memory.Store, media string, b []byte) v1.Descriptor {
	d := content.NewDescriptorFromBytes(media, b)
	must(s.Push(ctx, d, bytes.NewReader(b)))
	return d
}
func manifest(ctx context.Context, s *memory.Store, c v1.Descriptor, l []v1.Descriptor) v1.Descriptor {
	raw, e := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: c, Layers: l})
	must(e)
	return put(ctx, s, v1.MediaTypeImageManifest, raw)
}
func tarBytes(files map[string][]byte, mode int64) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	names := []string{}
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		raw := files[n]
		must(tw.WriteHeader(&tar.Header{Name: n, Mode: mode, Size: int64(len(raw))}))
		_, e := tw.Write(raw)
		must(e)
	}
	must(tw.Close())
	return b.Bytes()
}
func chart(image string) []byte {
	files := map[string][]byte{
		"app/Chart.yaml":               []byte("apiVersion: v2\nname: app\nversion: 1.0.0\ndependencies:\n  - name: child\n    version: 1.0.0\n    repository: file://charts/child\n"),
		"app/charts/child/Chart.yaml":  []byte("apiVersion: v2\nname: child\nversion: 1.0.0\n"),
		"app/charts/child/values.yaml": []byte("image: " + strings.TrimSuffix(image, ":stable") + ":hook" + "\npodAnnotations: {}\nimagePullSecrets: []\nimagePullPolicy: IfNotPresent\n"),
		"app/values.yaml":              []byte("image: " + image + "\nhelper: " + strings.TrimSuffix(image, ":stable") + ":b\npodAnnotations: {}\nimagePullSecrets: []\nimagePullPolicy: IfNotPresent\n"),
		"app/templates/deployment.yaml": []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}
  namespace: {{ .Release.Namespace }}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: {{ .Release.Name }}
  template:
    metadata:
      labels:
        app: {{ .Release.Name }}
      annotations:
{{ toYaml .Values.podAnnotations | indent 8 }}
    spec:
      imagePullSecrets:
{{ toYaml .Values.imagePullSecrets | indent 8 }}
      initContainers:
        - name: init
          image: {{ .Values.helper | quote }}
          imagePullPolicy: {{ .Values.imagePullPolicy }}
          args: ["once"]
      containers:
        - name: app
          image: {{ .Values.image | quote }}
          imagePullPolicy: {{ .Values.imagePullPolicy }}
          ports:
            - containerPort: 8080
          readinessProbe:
            httpGet:
              path: /version
              port: 8080
`),
		"app/charts/child/templates/hook.yaml": []byte(`apiVersion: batch/v1
kind: Job
metadata:
  name: {{ .Release.Name }}-hook
  namespace: {{ .Release.Namespace }}
  annotations:
    helm.sh/hook: post-install,post-upgrade,post-rollback
    helm.sh/hook-delete-policy: before-hook-creation
spec:
  template:
    metadata:
      annotations:
{{ toYaml .Values.podAnnotations | indent 8 }}
    spec:
      imagePullSecrets:
{{ toYaml .Values.imagePullSecrets | indent 8 }}
      restartPolicy: Never
      containers:
        - name: hook
          image: {{ .Values.image | quote }}
          imagePullPolicy: {{ .Values.imagePullPolicy }}
          args: ["once"]
`),
	}
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	_, e := gz.Write(tarBytes(files, 0644))
	must(e)
	must(gz.Close())
	return b.Bytes()
}
