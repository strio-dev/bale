package transport

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/dependency"
	"github.com/strio-dev/bale/internal/destination"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
)

// destinationStores overlays generated metadata without mutating the layout.
type destinationStores struct {
	base      content.ReadOnlyStorage
	generated map[string]content.ReadOnlyStorage
}

func (s destinationStores) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	if store, ok := s.generated[d.Digest.String()]; ok {
		return store.Fetch(ctx, d)
	}
	return s.base.Fetch(ctx, d)
}
func (s destinationStores) Exists(ctx context.Context, d v1.Descriptor) (bool, error) {
	if store, ok := s.generated[d.Digest.String()]; ok {
		return store.Exists(ctx, d)
	}
	return s.base.Exists(ctx, d)
}

func prepareDestinations(ctx context.Context, base content.ReadOnlyStorage, planned []DestinationAttachment) (destinationStores, error) {
	stores := destinationStores{base: base, generated: map[string]content.ReadOnlyStorage{}}
	for _, attachment := range planned {
		d, store, err := destination.Prepare(ctx, attachment.Subject, attachment.Record)
		if err != nil {
			return stores, err
		}
		if !content.Equal(d, attachment.Descriptor) {
			return stores, fmt.Errorf("%w: destination attachment changed since planning", ErrIntegrity)
		}
		if attachment.Record.Destination != attachment.Repository+"@"+attachment.Subject.Digest.String() {
			return stores, fmt.Errorf("%w: destination attachment placement mismatch", ErrIntegrity)
		}
		if err := bundle.VerifyClosure(ctx, store, d); err != nil {
			return stores, err
		}
		raw, err := fetchEnvelopeBytes(ctx, store, d)
		if err != nil {
			return stores, err
		}
		envelope, err := bundle.ParseEnvelope(d, raw)
		if err != nil {
			return stores, err
		}
		stores.generated[d.Digest.String()] = store
		for _, child := range envelope.Children {
			stores.generated[child.Digest.String()] = store
		}
	}
	return stores, nil
}

func appendDestinationTargets(targets []DeliveryRepository, attachments []DestinationAttachment) []DeliveryRepository {
	for _, a := range attachments {
		targets = append(targets, DeliveryRepository{Repository: a.Repository, Roots: []v1.Descriptor{a.Descriptor}})
	}
	return targets
}

func planDestinationAttachments(ctx context.Context, store content.ReadOnlyStorage, snapshots []bundle.Snapshot, plan DeliveryPlan) ([]DestinationAttachment, error) {
	records := map[ArtifactID]destination.Record{}
	descriptors := map[ArtifactID]v1.Descriptor{}
	dest, err := completionRef(plan.Destination)
	if err != nil {
		return nil, err
	}
	for _, relocation := range plan.Relocations {
		raw, err := fetchEnvelopeBytes(ctx, store, relocation.Descriptor)
		if err != nil {
			return nil, err
		}
		env, err := bundle.ParseEnvelope(relocation.Descriptor, raw)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(env.ArtifactType, "application/vnd.bale.destination.") || strings.HasPrefix(env.ArtifactType, "application/vnd.bale.dependencies.") || env.ArtifactType == bundle.MetadataArtifactType {
			continue
		}
		id := relocation.Source
		records[id] = destination.Record{SchemaVersion: destination.Version, Bundle: repositoryName(dest) + "@" + plan.Descriptor.Digest.String(), Source: id.Repository + "@" + id.Digest.String(), Destination: relocation.Destination.Repository + "@" + id.Digest.String(), Resolutions: []destination.Resolution{}}
		descriptors[id] = cloneDescriptor(relocation.Descriptor)
	}
	for _, snapshot := range snapshots {
		for _, observed := range snapshot.Metadata.Dependencies {
			id := ArtifactID{Repository: observed.Repository, Digest: observed.Subject.Digest}
			record, ok := records[id]
			if !ok {
				continue
			}
			resolution := destination.Resolution{Capture: snapshot.Descriptor.Digest, Policy: observed.Policy, Outcome: observed.Outcome, Dependencies: []destination.Dependency{}}
			if observed.Declaration != nil {
				declaration, err := bundle.LoadDependencyDeclaration(ctx, store, *observed.Declaration, observed.Subject)
				if err != nil {
					return nil, err
				}
				resolution.Declaration = &destination.Declaration{Digest: observed.Declaration.Digest, Context: declaration.Context}
			}
			for _, locked := range observed.Dependencies {
				ref, err := registry.ParseReference(locked.Ref)
				if err != nil {
					return nil, err
				}
				source := ArtifactID{Repository: repositoryName(ref), Digest: locked.Descriptor.Digest}
				moved, ok := plan.Relocations.Lookup(source)
				if !ok {
					return nil, fmt.Errorf("%w: dependency has no canonical destination", ErrIntegrity)
				}
				policy, err := dependency.NormalizePolicy(locked.Policy)
				if err != nil {
					return nil, err
				}
				resolution.Dependencies = append(resolution.Dependencies, destination.Dependency{Ref: locked.Ref, Policy: policy, Source: source.Repository + "@" + source.Digest.String(), Destination: moved.Destination.Repository + "@" + moved.Destination.Digest.String()})
			}
			record.Resolutions = append(record.Resolutions, resolution)
			records[id] = record
		}
	}
	ids := make([]ArtifactID, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i].Repository+"@"+ids[i].Digest.String() < ids[j].Repository+"@"+ids[j].Digest.String()
	})
	result := make([]DestinationAttachment, 0, len(ids))
	for _, id := range ids {
		raw, err := destination.Encode(records[id])
		if err != nil {
			return nil, err
		}
		record, err := destination.Decode(raw)
		if err != nil {
			return nil, err
		}
		d, _, err := destination.Prepare(ctx, descriptors[id], record)
		if err != nil {
			return nil, err
		}
		moved, _ := plan.Relocations.Lookup(id)
		result = append(result, DestinationAttachment{Repository: moved.Destination.Repository, Subject: descriptors[id], Descriptor: d, Record: record})
	}
	return result, nil
}

func confirmDestinationMappings(record destination.Record, confirmed Relocations) error {
	mappings := map[string]string{}
	for _, r := range confirmed {
		mappings[r.Source.Repository+"@"+r.Source.Digest.String()] = r.Destination.Repository + "@" + r.Destination.Digest.String()
	}
	if mappings[record.Source] != record.Destination {
		return fmt.Errorf("%w: destination subject mapping is unconfirmed", ErrIntegrity)
	}
	for _, resolution := range record.Resolutions {
		for _, dependency := range resolution.Dependencies {
			if mappings[dependency.Source] != dependency.Destination {
				return fmt.Errorf("%w: destination dependency mapping is unconfirmed", ErrIntegrity)
			}
		}
	}
	return nil
}

func addAttachmentStats(total *Stats, attachment Stats) {
	total.BlobBytesDownloaded += attachment.BlobBytesDownloaded
	total.BlobBytesUploaded += attachment.BlobBytesUploaded
	total.ManifestBytesDownloaded += attachment.ManifestBytesDownloaded
	total.ManifestBytesUploaded += attachment.ManifestBytesUploaded
	total.BlobsMounted += attachment.BlobsMounted
	total.ObjectsSkipped += attachment.ObjectsSkipped
}
