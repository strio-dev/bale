package flux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/distribution/reference"
	relocate "github.com/strio-dev/bale/plugins/helm-relocate"
)

const MaxResponseBytes = 900 << 10

type Input struct {
	ID              string         `json:"id"`
	ChartRepository string         `json:"chartRepository"`
	ChartDigest     string         `json:"chartDigest"`
	SourceName      string         `json:"sourceName"`
	Bundle          string         `json:"bundle"`
	Capture         string         `json:"capture"`
	Attachment      string         `json:"attachment"`
	PostRenderers   []PostRenderer `json:"postRenderers"`
}
type Response struct {
	Inputs []Input `json:"inputs"`
}
type HandlerOptions struct {
	BearerToken string
	Timeout     time.Duration
	// OnError reports resolution diagnostics to the operator, never the HTTP client.
	// It may be called concurrently and must be safe for concurrent use.
	OnError func(release string, err error)
}
type handler struct {
	config    Config
	inspector relocate.DestinationInspector
	options   HandlerOptions
	slots     chan struct{}
}

func NewHandler(config Config, inspector relocate.DestinationInspector, options HandlerOptions) (http.Handler, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if inspector == nil {
		return nil, fmt.Errorf("inspector is required")
	}
	if options.Timeout <= 0 {
		options.Timeout = 30 * time.Second
	}
	// Retain a private copy so callers cannot change the request allowlist.
	config.Releases = append([]Release(nil), config.Releases...)
	return &handler{config: config, inspector: inspector, options: options, slots: make(chan struct{}, 16)}, nil
}
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// Kubernetes probes check only the serving process, including with TLS and
	// bearer authentication enabled. Registry failures must not restart it.
	if r.URL.Path == "/healthz" && r.URL.RawQuery == "" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	if h.options.BearerToken != "" {
		got := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		want := sha256.Sum256([]byte("Bearer " + h.options.BearerToken))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.RawQuery != "" {
		http.Error(w, "query parameters are not supported", http.StatusBadRequest)
		return
	}
	releases := h.config.Releases
	if r.URL.Path != "/inputs" {
		name, ok := strings.CutPrefix(r.URL.Path, "/inputs/")
		if !ok || name == "" || strings.Contains(name, "/") {
			http.NotFound(w, r)
			return
		}
		releases = nil
		for _, release := range h.config.Releases {
			if release.Name == name {
				releases = []Release{release}
				break
			}
		}
		if releases == nil {
			http.NotFound(w, r)
			return
		}
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		http.Error(w, "resolver is busy", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.options.Timeout)
	defer cancel()
	var result bytes.Buffer
	result.WriteString(`{"inputs":[`)
	for _, release := range releases {
		input, err := resolve(ctx, h.inspector, release)
		if err != nil { // Never return successful empty inputs: Flux Operator may prune resources.
			if h.options.OnError != nil {
				h.options.OnError(release.Name, err)
			}
			http.Error(w, "destination resolution failed for release "+release.Name, http.StatusBadGateway)
			return
		}
		raw, err := json.Marshal(input)
		if err != nil || result.Len()+len(raw)+3 > MaxResponseBytes {
			http.Error(w, "input response exceeds limit", http.StatusBadGateway)
			return
		}
		if result.Len() > len(`{"inputs":[`) {
			result.WriteByte(',')
		}
		result.Write(raw)
	}
	result.WriteString(`]}`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// json.Marshal escapes HTML characters; this endpoint serves application/json.
	// nosemgrep: cache.semgrep-rules.go.lang.security.audit.xss.no-direct-write-to-responsewriter
	_, _ = w.Write(result.Bytes())
}
func resolve(ctx context.Context, inspector relocate.DestinationInspector, release Release) (Input, error) {
	selection, err := relocate.Resolve(ctx, inspector, release.Chart, release.Selection.options())
	if err != nil {
		return Input{}, err
	}
	images, err := Images(selection.Mappings())
	if err != nil {
		return Input{}, err
	}
	n, err := reference.ParseNormalizedNamed(selection.Chart)
	if err != nil {
		return Input{}, err
	}
	d, ok := n.(reference.Digested)
	if !ok {
		return Input{}, fmt.Errorf("chart is not pinned")
	}
	return Input{ID: release.Name, ChartRepository: "oci://" + reference.TrimNamed(n).String(), ChartDigest: d.Digest().String(), SourceName: SourceName(release.Name, selection.Chart), Bundle: selection.Record.Bundle, Capture: selection.Resolution.Capture.String(), Attachment: selection.Attachment.Digest.String(), PostRenderers: []PostRenderer{{Kustomize: Kustomize{Images: images}}}}, nil
}

// SourceName includes release ownership and repository/digest identity.
// Distinct releases must not share sources pruned by their owning ResourceSets.
func SourceName(release, chart string) string {
	sum := sha256.Sum256([]byte(release + "\x00" + chart))
	return "bale-" + hex.EncodeToString(sum[:20])
}
