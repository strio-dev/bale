// Package bundle implements Bale's registry-native OCI snapshot format.
package bundle

import (
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	BundleArtifactType   = "application/vnd.bale.bundle.v1+json"
	MetadataArtifactType = "application/vnd.bale.bundle.metadata.v1+json"
	MetadataMediaType    = MetadataArtifactType
	MetadataDigestKey    = "io.bale.metadata.digest"
	MetadataVersion      = 1
	DockerManifest       = "application/vnd.docker.distribution.manifest.v2+json"
	DockerIndex          = "application/vnd.docker.distribution.manifest.list.v2+json"
)

type Metadata struct {
	SchemaVersion int                    `json:"schemaVersion"`
	Recipe        config.Recipe          `json:"recipe"`
	Roots         []Root                 `json:"roots"`
	Origins       []Origin               `json:"origins"`
	Referrers     []Association          `json:"referrers"`
	Dependencies  []DependencyResolution `json:"dependencies,omitempty"`
	Plugins       []PluginResolution     `json:"plugins,omitempty"`
}

type Root struct {
	Input      int                `json:"input"`
	Repository string             `json:"repository"`
	Reference  string             `json:"reference"`
	Descriptor ocispec.Descriptor `json:"descriptor"`
}

type Origin struct {
	Repository string             `json:"repository"`
	Descriptor ocispec.Descriptor `json:"descriptor"`
}

type Association struct {
	Repository string             `json:"repository"`
	Subject    ocispec.Descriptor `json:"subject"`
	Descriptor ocispec.Descriptor `json:"descriptor"`
}

type Snapshot struct {
	Descriptor ocispec.Descriptor
	Metadata   Metadata
}

// Envelope contains strong structural edges. Subject is a weak backlink and
// must never cause traversal outside the captured graph.
type Envelope struct {
	Children     []ocispec.Descriptor
	Subject      *ocispec.Descriptor
	ArtifactType string
}

func IsEnvelope(mediaType string) bool {
	return mediaType == ocispec.MediaTypeImageManifest || mediaType == ocispec.MediaTypeImageIndex || mediaType == DockerManifest || mediaType == DockerIndex
}

// IsEnvelopeChild reports whether a structural child must be interpreted as an
// envelope. Index members always require supported envelopes; manifest configs
// remain opaque, while layers recurse only for recognized envelope media types.
// The ordinal is the position in Envelope.Children (config precedes layers).
func IsEnvelopeChild(parent ocispec.Descriptor, ordinal int, child ocispec.Descriptor) bool {
	return isIndex(parent.MediaType) || ordinal > 0 && IsEnvelope(child.MediaType)
}

func isIndex(mediaType string) bool {
	return mediaType == ocispec.MediaTypeImageIndex || mediaType == DockerIndex
}

// DependencyResolution captures declaration discovery and resolution without
// executing selectors when this immutable bundle is later loaded.
type DependencyResolution struct {
	Repository   string               `json:"repository"`
	Subject      ocispec.Descriptor   `json:"subject"`
	Policy       dependency.Policy    `json:"policy"`
	Outcome      string               `json:"outcome"`
	Declaration  *ocispec.Descriptor  `json:"declaration,omitempty"`
	Dependencies []ResolvedDependency `json:"dependencies"`
}

type ResolvedDependency struct {
	Ref        string             `json:"ref"`
	Descriptor ocispec.Descriptor `json:"descriptor"`
	Policy     dependency.Policy  `json:"policy"`
}

// PluginResolution pins the plugin and generated declaration for one selected
// root and recipe input. Literal plugin input remains in Recipe.
type PluginResolution struct {
	Input       int                `json:"input"`
	Repository  string             `json:"repository"`
	Subject     ocispec.Descriptor `json:"subject"`
	Ref         string             `json:"ref"`
	Descriptor  ocispec.Descriptor `json:"descriptor"`
	Declaration ocispec.Descriptor `json:"declaration"`
}
