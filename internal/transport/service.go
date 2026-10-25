// Package transport captures and delivers complete, immutable OCI graphs.
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/deliveryfile"
	"github.com/strio-dev/bale/internal/dependency"
	"github.com/strio-dev/bale/internal/destination"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// PluginRequest supplies a verified portable WASI module and its JSON input.
type PluginRequest struct {
	ModulePath string
	Input      json.RawMessage
}

// PluginRunner is a caller-owned dependency producer; transport is engine agnostic.
type PluginRunner func(context.Context, PluginRequest) (dependency.Declaration, error)

type Options struct {
	PluginRunner        PluginRunner
	PlainHTTPRegistries []string
	HTTPClient          *http.Client
	Credentials         auth.CredentialFunc
	OnProgress          func(Progress)
}
type Service struct {
	pluginRunner PluginRunner
	httpHosts    map[string]bool
	client       *auth.Client
	onProgress   func(Progress)
	progressMu   sync.Mutex
	publishGate  chan struct{}
}

// Stats accounts for graph capture and copy operations, not total HTTP traffic.
// Download counts exclude verification and latest-mode inspection rereads.
// Upload counts include generated Bale JSON; registry API overhead is excluded.
type Stats struct {
	Accounting              string `json:"accounting"`
	BlobBytesDownloaded     int64  `json:"blobBytesDownloaded"`
	BlobBytesUploaded       int64  `json:"blobBytesUploaded"`
	ManifestBytesDownloaded int64  `json:"manifestBytesDownloaded"`
	ManifestBytesUploaded   int64  `json:"manifestBytesUploaded"`
	BlobsMounted            int    `json:"blobsMounted"`
	ObjectsSkipped          int    `json:"objectsSkipped"`
}

const statsAccounting = "Graph content accounting, not total network traffic: downloads exclude verification and latest-mode inspection rereads; uploads include generated Bale JSON; HTTP/API overhead is excluded."

type CreateOptions struct {
	DryRun         bool
	ExportDelivery bool
}
type CreateResult struct {
	MetadataOnly bool               `json:"metadataOnly,omitempty"`
	LayoutPath   string             `json:"layoutPath,omitempty"`
	Published    bool               `json:"published"`
	Descriptor   v1.Descriptor      `json:"descriptor"`
	Metadata     bundle.Metadata    `json:"metadata"`
	Stats        Stats              `json:"stats"`
	DryRun       bool               `json:"dryRun"`
	Delivery     *deliveryfile.File `json:"delivery,omitempty"`
}
type PullMode string

const (
	PullLocked PullMode = "locked"
	PullLatest PullMode = "latest"
)

// PullOptions grants execution only for this explicit latest refresh.
type PullOptions struct {
	AllowPlugins bool
}

type PullResult struct {
	Descriptor v1.Descriptor `json:"descriptor"`
	LayoutPath string        `json:"layoutPath"`
	Mode       PullMode      `json:"mode"`
	Stats      Stats         `json:"stats"`
}
type PushOptions struct {
	RepositoryMap map[string]string
	Delivery      *deliveryfile.File
}
type Tag struct {
	Repository string        `json:"repository"`
	Reference  string        `json:"reference"`
	Descriptor v1.Descriptor `json:"descriptor"`
}
type DeliveryRepository struct {
	Repository string          `json:"repository"`
	Roots      []v1.Descriptor `json:"roots"`
}
type DestinationAttachment struct {
	Repository string             `json:"repository"`
	Subject    v1.Descriptor      `json:"subject"`
	Descriptor v1.Descriptor      `json:"descriptor"`
	Record     destination.Record `json:"record"`
}
type DeliveryPlan struct {
	MetadataOnly  bool `json:"metadataOnly,omitempty"`
	captureDigest digest.Digest
	Destinations  []DestinationAttachment `json:"destinations"`
	// Relocations lists planned canonical placements; Push confirms them after verification.
	Relocations   Relocations          `json:"relocations"`
	LayoutPath    string               `json:"layoutPath"`
	Destination   string               `json:"destination"`
	Descriptor    v1.Descriptor        `json:"descriptor"`
	Repositories  []DeliveryRepository `json:"repositories"`
	Tags          []Tag                `json:"tags"`
	RepositoryMap map[string]string    `json:"repositoryMap"`
	Referrers     []bundle.Association `json:"referrers"`
	seal          string
}
type PushResult struct {
	Destinations  []AttachResult       `json:"destinations"`
	Relocations   Relocations          `json:"relocations"`
	Descriptor    v1.Descriptor        `json:"descriptor"`
	Stats         Stats                `json:"stats"`
	Warnings      []string             `json:"warnings"`
	Destination   string               `json:"destination"`
	Repositories  []DeliveryRepository `json:"repositories"`
	RepositoryMap map[string]string    `json:"repositoryMap"`
	Tags          []Tag                `json:"tags"`
	Referrers     []bundle.Association `json:"referrers"`
}

func NewService(opts Options) (*Service, error) {
	client := opts.HTTPClient
	if client == nil {
		client = auth.DefaultClient.Client
	}
	s := &Service{pluginRunner: opts.PluginRunner, httpHosts: map[string]bool{}, client: &auth.Client{Client: client, Cache: auth.NewCache(), Credential: opts.Credentials}, onProgress: opts.OnProgress, publishGate: make(chan struct{}, 1)}
	for _, host := range opts.PlainHTTPRegistries {
		ref, err := registry.ParseReference(host + "/bale")
		if err != nil || ref.Registry != host {
			return nil, operationError("configure", host, v1.Descriptor{}, ErrInvalid, fmt.Errorf("invalid plain HTTP registry %q", host))
		}
		s.httpHosts[host] = true
	}
	return s, nil
}

// acquirePublication coordinates same-client writes while ORAS handles registry
// updates. Its merge pool cannot independently cancel waiting participants.
func (s *Service) acquirePublication(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.publishGate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			s.releasePublication()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) releasePublication() { <-s.publishGate }

func (s *Service) repository(name string) (*remote.Repository, error) {
	r, err := remote.NewRepository(name)
	if err != nil {
		return nil, err
	}
	r.PlainHTTP = s.httpHosts[r.Reference.Registry]
	r.Client = inventoryValidationClient{Client: s.client}
	// Updating standard referrer inventories must not delete existing manifests.
	r.SkipReferrersGC = true
	return r, nil
}
func parseRef(value string) (registry.Reference, error) {
	r, err := registry.ParseReference(value)
	if err != nil {
		return r, err
	}
	if r.Reference == "" {
		return r, fmt.Errorf("reference needs explicit tag or digest: %s", value)
	}
	return r, nil
}
func repositoryName(r registry.Reference) string { return r.Registry + "/" + r.Repository }
func completionRef(value string) (registry.Reference, error) {
	r, err := parseRef(value)
	if err != nil {
		return r, err
	}
	if strings.Contains(r.Reference, ":") {
		return r, fmt.Errorf("completion reference must use a tag")
	}
	return r, nil
}
func (s *Service) checkTag(ctx context.Context, t Tag) error {
	r, err := s.repository(t.Repository)
	if err != nil {
		return err
	}
	d, err := r.Resolve(ctx, t.Reference)
	if errors.Is(err, errdef.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if d.Digest != t.Descriptor.Digest {
		return fmt.Errorf("%w: refusing conflicting tag %s:%s: %s already exists", ErrConflict, t.Repository, t.Reference, d.Digest)
	}
	return nil
}

func (s *Service) progress(event Progress) {
	if s.onProgress == nil {
		return
	}
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	s.onProgress(cloneJSON(event))
}

// captureProgress is advisory: another operation's callback must not block
// read-only capture or prevent a publication waiter from reaching cancellation.
// Available callbacks still run synchronously under the shared serialization lock.
func (s *Service) captureProgress(event Progress) {
	if s.onProgress == nil || !s.progressMu.TryLock() {
		return
	}
	defer s.progressMu.Unlock()
	s.onProgress(cloneJSON(event))
}
