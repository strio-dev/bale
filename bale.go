package bale

import (
	"context"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/deliveryfile"
	"github.com/strio-dev/bale/internal/dependency"
	"github.com/strio-dev/bale/internal/destination"
	"github.com/strio-dev/bale/internal/transport"
)

// Client transports immutable OCI graphs and Bale bundles. Authentication,
// network policy, and progress reporting are supplied by its caller. Registry
// publication through Attach, Create, Execute, and Push is serialized per client;
// callers coordinate overlapping publication by other clients or processes.
type Client struct{ service *transport.Service }

type (
	DependencyDeclaration        = dependency.Declaration
	DependencyReference          = dependency.Reference
	DependencyPolicy             = dependency.Policy
	DependencySelector           = dependency.Selector
	DependencyResolution         = bundle.DependencyResolution
	ResolvedDependency           = bundle.ResolvedDependency
	ClientOptions                = transport.Options
	PluginRequest                = transport.PluginRequest
	PluginRunner                 = transport.PluginRunner
	PullOptions                  = transport.PullOptions
	Plugin                       = config.Plugin
	PluginBlob                   = config.PluginBlob
	PluginResolution             = bundle.PluginResolution
	ArtifactID                   = transport.ArtifactID
	Root                         = transport.Root
	TransferRequest              = transport.TransferRequest
	Plan                         = transport.TransferPlan
	PlanView                     = transport.PlanView
	Relocation                   = transport.Relocation
	Relocations                  = transport.Relocations
	Result                       = transport.Result
	Progress                     = transport.Progress
	Error                        = transport.Error
	Recipe                       = config.Recipe
	Selection                    = config.Selection
	ReferrerPolicy               = config.Policy
	RequiredReferrer             = config.RequiredReferrer
	Metadata                     = bundle.Metadata
	BundleRoot                   = bundle.Root
	Origin                       = bundle.Origin
	Association                  = bundle.Association
	Snapshot                     = bundle.Snapshot
	Stats                        = transport.Stats
	CreateOptions                = transport.CreateOptions
	CreateResult                 = transport.CreateResult
	ArchiveResult                = transport.ArchiveResult
	PullMode                     = transport.PullMode
	PullResult                   = transport.PullResult
	PushOptions                  = transport.PushOptions
	Tag                          = transport.Tag
	DeliveryRepository           = transport.DeliveryRepository
	DeliveryPlan                 = transport.DeliveryPlan
	PushResult                   = transport.PushResult
	AttachRequest                = transport.AttachRequest
	AttachResult                 = transport.AttachResult
	ListReferrersOptions         = transport.ListReferrersOptions
	InspectResult                = transport.InspectResult
	DiffResult                   = transport.DiffResult
	ArtifactChange               = transport.ArtifactChange
	ResolutionChange             = transport.ResolutionChange
	DeclarationChange            = transport.DeclarationChange
	DeclarationState             = transport.DeclarationState
	InspectedDeclaration         = transport.InspectedDeclaration
	DestinationRecord            = destination.Record
	DestinationResolution        = destination.Resolution
	DestinationDeclaration       = destination.Declaration
	DestinationDependency        = destination.Dependency
	DestinationAttachment        = transport.DestinationAttachment
	InspectedDestination         = transport.InspectedDestination
	DestinationInspectionOptions = transport.DestinationInspectionOptions
	DeliveryFile                 = deliveryfile.File
	DeliveryArtifact             = deliveryfile.Artifact
	DeliveryUpdateResult         = transport.DeliveryUpdateResult
	DeliveryPathChange           = transport.DeliveryPathChange
	DeliveryPathConflict         = transport.DeliveryPathConflict
)

const (
	DependencyArtifactType  = dependency.ArtifactType
	DependencyVersion       = dependency.Version
	DependencyMaxBytes      = dependency.MaxBytes
	DestinationArtifactType = destination.ArtifactType
	DestinationVersion      = destination.Version
	DestinationMaxBytes     = destination.MaxBytes
	PullLocked              = transport.PullLocked
	PullLatest              = transport.PullLatest
	BundleArtifactType      = bundle.BundleArtifactType
	MetadataArtifactType    = bundle.MetadataArtifactType
	MetadataMediaType       = bundle.MetadataMediaType
	MetadataDigestKey       = bundle.MetadataDigestKey
	MetadataVersion         = bundle.MetadataVersion
)

var (
	ErrInvalid     = transport.ErrInvalid
	ErrUnsupported = transport.ErrUnsupported
	ErrNotFound    = transport.ErrNotFound
	ErrIntegrity   = transport.ErrIntegrity
	ErrConflict    = transport.ErrConflict
)

// NewClient constructs a client without reading ambient credential files.
func NewClient(options ClientOptions) (*Client, error) {
	service, err := transport.NewService(options)
	if err != nil {
		return nil, err
	}
	return &Client{service: service}, nil
}

// ParseRecipe parses strict YAML and resolves authoring defaults.
func ParseRecipe(data []byte) (Recipe, error) {
	recipe, err := config.Parse(data)
	if err != nil {
		return Recipe{}, &Error{Kind: ErrInvalid, Op: "parse recipe", Cause: err}
	}
	return recipe, nil
}

// NormalizeRecipe applies defaults and validates a deep copy of a Go recipe.
func NormalizeRecipe(recipe Recipe) (Recipe, error) {
	normalized, err := config.Normalize(recipe)
	if err != nil {
		return Recipe{}, &Error{Kind: ErrInvalid, Op: "normalize recipe", Cause: err}
	}
	return normalized, nil
}

// Resolve pins a registry reference and returns its content source.
func (c *Client) Resolve(ctx context.Context, reference string) (Root, error) {
	return c.service.Resolve(ctx, reference)
}

// Plan inspects the graph and validates relocation without publishing content.
func (c *Client) Plan(ctx context.Context, request TransferRequest) (*Plan, error) {
	return c.service.Plan(ctx, request)
}

// Execute publishes an in-memory plan and reports confirmed relocations.
func (c *Client) Execute(ctx context.Context, plan *Plan) (Result, error) {
	return c.service.Execute(ctx, plan)
}

// Attach publishes a prepared immutable OCI attachment to its existing subject's
// repository and verifies discoverability. The result retains completed work on
// failure. It shares this client's cancellable publication permit with Create,
// Execute, and Push.
func (c *Client) Attach(ctx context.Context, request AttachRequest) (AttachResult, error) {
	return c.service.Attach(ctx, request)
}

// ListReferrers returns validated, sorted referrers of a pinned subject. It makes
// no registry writes. On error, the result may contain validated earlier entries.
func (c *Client) ListReferrers(ctx context.Context, subjectReference string, options ListReferrersOptions) ([]v1.Descriptor, error) {
	return c.service.ListReferrers(ctx, subjectReference, options)
}

// InspectDependencies pins a registry subject once and reads all its verified
// dependency declarations. It neither resolves declared dependencies nor reads
// application payloads. An error returns no declarations; an empty inventory
// returns a nonnil empty slice.
func (c *Client) InspectDependencies(ctx context.Context, subjectReference string) ([]InspectedDeclaration, error) {
	return c.service.InspectDependencies(ctx, subjectReference)
}

// InspectDestinations pins a subject once and reads its verified destination
// records without resolving their locations or reading application payloads.
// Local identifies records for this repository and digest, not a newest record.
// Callers select the intended source, bundle, capture and declaration explicitly.
func (c *Client) InspectDestinations(ctx context.Context, subjectReference string) ([]InspectedDestination, error) {
	return c.service.InspectDestinations(ctx, subjectReference)
}

// InspectDestinationsWithOptions bounds aggregate metadata reads and referrer
// inventory counts. An error returns no partial records; zero limits preserve
// the existing InspectDestinations behavior.
func (c *Client) InspectDestinationsWithOptions(ctx context.Context, reference string, options DestinationInspectionOptions) ([]InspectedDestination, error) {
	return c.service.InspectDestinationsWithOptions(ctx, reference, options)
}

// EncodeDestinationRecord validates and deterministically encodes destination
// metadata. It does not verify availability or publish the referenced content.
func EncodeDestinationRecord(record DestinationRecord) ([]byte, error) {
	raw, err := destination.Encode(record)
	if err != nil {
		return nil, &Error{Kind: ErrInvalid, Op: "encode destination record", Cause: err}
	}
	return raw, nil
}

// DecodeDestinationRecord strictly validates registry JSON and returns its
// canonical records without making network requests.
func DecodeDestinationRecord(raw []byte) (DestinationRecord, error) {
	record, err := destination.Decode(raw)
	if err != nil {
		return DestinationRecord{}, &Error{Kind: ErrInvalid, Op: "decode destination record", Cause: err}
	}
	return record, nil
}

// Create captures a normalized recipe as a registry-native bundle. On failure,
// its result retains completed work; Published confirms completion-tag readback.
func (c *Client) Create(ctx context.Context, recipe Recipe, storageReference string, options CreateOptions) (CreateResult, error) {
	normalized, err := NormalizeRecipe(recipe)
	if err != nil {
		return CreateResult{}, err
	}
	return c.service.Create(ctx, normalized, storageReference, options)
}

// CreateLayout captures a recipe directly into a new local OCI directory.
// Source registries are read only. LayoutPath confirms completed local output;
// Published remains reserved for registry publication. DryRun writes no layout.
func (c *Client) CreateLayout(ctx context.Context, recipe Recipe, layoutPath string, options CreateOptions) (CreateResult, error) {
	normalized, err := NormalizeRecipe(recipe)
	if err != nil {
		return CreateResult{}, err
	}
	return c.service.CreateLayout(ctx, normalized, layoutPath, options)
}

// CreateLocal locks a recipe into a source-dependent local metadata capture.
// It retains manifests and interpreted metadata without downloading ordinary
// image payloads. Discovery may read chart or plugin bytes. Save or Push reads
// missing payloads by pinned digest; neither refreshes tags nor runs plugins.
func (c *Client) CreateLocal(ctx context.Context, recipe Recipe, path string, options CreateOptions) (CreateResult, error) {
	normalized, err := NormalizeRecipe(recipe)
	if err != nil {
		return CreateResult{}, err
	}
	return c.service.CreateLocal(ctx, normalized, path, options)
}

// Save writes a verified bundle to a new, deterministic OCI tar archive.
// Metadata-only captures fetch missing payloads from their pinned sources;
// complete layouts are saved offline. The source is unchanged, the bundle
// digest is preserved, and plugins are never executed.
func (c *Client) Save(ctx context.Context, layoutPath, archivePath string) (ArchiveResult, error) {
	return c.service.Save(ctx, layoutPath, archivePath)
}

// Load restores a verified OCI tar archive to a new bundle layout directory.
// It leaves the archive unchanged and performs no network or plugin operations.
func (c *Client) Load(ctx context.Context, archivePath, layoutPath string) (ArchiveResult, error) {
	return c.service.Load(ctx, archivePath, layoutPath)
}

// EncodeDeliveryFile produces deterministic YAML without mutating the caller.
func EncodeDeliveryFile(file DeliveryFile) ([]byte, error) {
	raw, err := deliveryfile.Encode(file)
	if err != nil {
		return nil, &Error{Kind: ErrInvalid, Op: "encode delivery file", Cause: err}
	}
	return raw, nil
}

// DecodeDeliveryFile parses strict YAML path overrides bound to a bundle digest.
func DecodeDeliveryFile(data []byte) (DeliveryFile, error) {
	file, err := deliveryfile.Decode(data)
	if err != nil {
		return DeliveryFile{}, &Error{Kind: ErrInvalid, Op: "decode delivery file", Cause: err}
	}
	return file, nil
}

// ExportDelivery resolves a registry bundle once and exports its default paths
// after validating captured manifests, indexes, and Bale metadata. It does not
// fetch or check opaque application configs or payloads, discover referrers, or
// resolve original source selectors. It does not prove payload availability or
// integrity; use Inspect for complete captured-graph verification.
func (c *Client) ExportDelivery(ctx context.Context, reference string) (DeliveryFile, error) {
	return c.service.ExportDelivery(ctx, reference)
}

// ExportDeliveryLayout exports default paths from a verified local bundle,
// without network access or changes to the layout.
func (c *Client) ExportDeliveryLayout(ctx context.Context, path string) (DeliveryFile, error) {
	return c.service.ExportDeliveryLayout(ctx, path)
}

// ExportDeliveryRoot exports a pinned bundle in caller-owned read-only storage.
func (c *Client) ExportDeliveryRoot(ctx context.Context, root Root) (DeliveryFile, error) {
	return c.service.ExportDeliveryRoot(ctx, root)
}

// UpdateDelivery carries explicit delivery paths between captured registry
// snapshots. Exact identities and unambiguous selector changes preserve paths.
// It validates interpreted metadata without reading opaque payloads or refreshing
// sources. Identical references share one pinned read. Conflicts return diagnostics
// with ErrConflict and no candidate Delivery; other failures return an empty result.
func (c *Client) UpdateDelivery(ctx context.Context, beforeRef, afterRef string, file DeliveryFile) (DeliveryUpdateResult, error) {
	return c.service.UpdateDelivery(ctx, beforeRef, afterRef, file)
}

// UpdateDeliveryLayouts updates paths using two local OCI bundle layouts without
// network access or writes. Only envelopes and interpreted metadata are read;
// this does not prove opaque payload availability or integrity.
func (c *Client) UpdateDeliveryLayouts(ctx context.Context, beforePath, afterPath string, file DeliveryFile) (DeliveryUpdateResult, error) {
	return c.service.UpdateDeliveryLayouts(ctx, beforePath, afterPath, file)
}

// UpdateDeliveryRoots compares pinned snapshots in caller-owned read-only stores
// without closing them. Inventories and selector sets include nested bundles.
func (c *Client) UpdateDeliveryRoots(ctx context.Context, beforeRoot, afterRoot Root, file DeliveryFile) (DeliveryUpdateResult, error) {
	return c.service.UpdateDeliveryRoots(ctx, beforeRoot, afterRoot, file)
}

// Pull materializes a bundle as a complete local OCI layout. Failed pulls retain
// completed download accounting; LayoutPath is set only after atomic publication.
func (c *Client) Pull(ctx context.Context, reference, layoutPath string, mode PullMode) (PullResult, error) {
	return c.service.Pull(ctx, reference, layoutPath, mode)
}

// PullWithOptions explicitly permits configured producers during latest refresh.
// Locked pulls never execute plugins.
func (c *Client) PullWithOptions(ctx context.Context, reference, layoutPath string, mode PullMode, options PullOptions) (PullResult, error) {
	return c.service.PullWithOptions(ctx, reference, layoutPath, mode, options)
}

// PlanPush validates a bundle layout and its destination repository mapping.
func (c *Client) PlanPush(ctx context.Context, layoutPath, destination string, options PushOptions) (DeliveryPlan, error) {
	return c.service.PlanPush(ctx, layoutPath, destination, options)
}

// Push publishes a validated bundle delivery plan. Tags and Relocations report
// confirmed publications, including on failure. Repositories, RepositoryMap, and
// Referrers describe the planned delivery and do not imply successful publication.
func (c *Client) Push(ctx context.Context, plan DeliveryPlan) (PushResult, error) {
	return c.service.Push(ctx, plan)
}

// Inspect resolves a remote bundle once and verifies its full captured graph.
// It does not refresh source selectors or discover additional referrers.
func (c *Client) Inspect(ctx context.Context, reference string) (InspectResult, error) {
	return c.service.Inspect(ctx, reference)
}

// InspectLayout verifies local bundle content without network access or writes.
// Marked metadata-only captures verify their interpreted graph and report
// MetadataOnly; complete layouts additionally verify every payload.
func (c *Client) InspectLayout(ctx context.Context, layoutPath string) (InspectResult, error) {
	return c.service.InspectLayout(ctx, layoutPath)
}

// InspectRoot verifies a pinned bundle in a caller-owned content store, without
// resolving references, discovering referrers, or taking ownership of the store.
func (c *Client) InspectRoot(ctx context.Context, root Root) (InspectResult, error) {
	return c.service.InspectRoot(ctx, root)
}

// Diff compares captured metadata from two registry bundles. Each reference is
// resolved once; identical reference strings share one inspection. It does not
// refresh source selectors, discover attachments, execute plugins, or publish.
// Opaque payloads are not read or checked for availability or integrity.
func (c *Client) Diff(ctx context.Context, beforeRef, afterRef string) (DiffResult, error) {
	return c.service.Diff(ctx, beforeRef, afterRef)
}

// DiffLayouts compares two single-root OCI bundle layouts without network
// access or writes. It validates envelopes and interpreted metadata, including
// nested bundle metadata, without checking opaque payloads.
func (c *Client) DiffLayouts(ctx context.Context, beforePath, afterPath string) (DiffResult, error) {
	return c.service.DiffLayouts(ctx, beforePath, afterPath)
}

// DiffRoots compares captured metadata in caller-owned stores without closing
// or modifying them. Detailed observations describe each root bundle; nested
// bundles can be compared separately by their pinned descriptors.
func (c *Client) DiffRoots(ctx context.Context, beforeRoot, afterRoot Root) (DiffResult, error) {
	return c.service.DiffRoots(ctx, beforeRoot, afterRoot)
}
