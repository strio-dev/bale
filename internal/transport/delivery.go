package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/deliveryfile"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
)

func (s *Service) PlanPush(ctx context.Context, layoutPath, destination string, opts PushOptions) (result DeliveryPlan, retErr error) {
	defer func() {
		if retErr != nil {
			retErr = operationError("plan push", destination, result.Descriptor, nil, retErr)
		}
	}()

	dest, err := completionRef(destination)
	if err != nil {
		return DeliveryPlan{}, operationError("destination", destination, v1.Descriptor{}, ErrInvalid, err)
	}
	store, snapshot, err := openLayout(ctx, layoutPath)
	if err != nil {
		return DeliveryPlan{}, err
	}
	plan := DeliveryPlan{LayoutPath: layoutPath, Destination: destination, Descriptor: snapshot.Descriptor, Repositories: []DeliveryRepository{}, Tags: []Tag{}}
	if _, err := bundle.LoadSnapshotGraph(ctx, store, snapshot.Descriptor); err != nil {
		return DeliveryPlan{}, err
	}
	snapshots, err := collectBundleSnapshots(ctx, store, snapshot.Descriptor, bundle.InspectSnapshot)
	metadata := make([]bundle.Metadata, len(snapshots))
	for i, snap := range snapshots {
		metadata[i] = snap.Metadata
	}
	if err != nil {
		return DeliveryPlan{}, err
	}
	identities := map[ArtifactID]v1.Descriptor{}
	sourceRepositories := map[string]bool{}
	for _, m := range metadata {
		for _, origin := range m.Origins {
			identities[ArtifactID{Repository: origin.Repository, Digest: origin.Descriptor.Digest}] = origin.Descriptor
			sourceRepositories[origin.Repository] = true
		}
	}
	overrides := map[ArtifactID]string{}
	if opts.Delivery != nil {
		file, err := deliveryfile.Normalize(*opts.Delivery)
		if err != nil {
			return DeliveryPlan{}, fmt.Errorf("%w: delivery file: %v", ErrInvalid, err)
		}
		if file.Bundle != snapshot.Descriptor.Digest {
			return DeliveryPlan{}, fmt.Errorf("%w: delivery file bundle does not match layout root", ErrInvalid)
		}
		for _, artifact := range file.Artifacts {
			ref, err := registry.ParseReference(artifact.Ref)
			if err != nil {
				return DeliveryPlan{}, fmt.Errorf("%w: delivery reference: %v", ErrInvalid, err)
			}
			id := ArtifactID{Repository: repositoryName(ref), Digest: digest.Digest(ref.Reference)}
			if _, ok := identities[id]; !ok {
				return DeliveryPlan{}, fmt.Errorf("%w: unknown delivery artifact %s", ErrInvalid, artifact.Ref)
			}
			overrides[id] = artifact.Path
		}
	}
	mapping := map[string]string{}
	for source := range sourceRepositories {
		ref, err := registry.ParseReference(source)
		if err != nil || ref.Reference != "" {
			return DeliveryPlan{}, fmt.Errorf("%w: invalid source repository %q", ErrInvalid, source)
		}
		relative := ref.Repository
		if mapped, ok := opts.RepositoryMap[source]; ok {
			relative = mapped
		}
		target, err := registry.ParseReference(dest.Registry + "/" + dest.Repository + "/" + relative)
		if err != nil || target.Reference != "" || target.Registry != dest.Registry || relative == "" || strings.HasPrefix(relative, "/") {
			return DeliveryPlan{}, fmt.Errorf("%w: invalid repository mapping for %s", ErrInvalid, source)
		}
		mapping[source] = repositoryName(target)
	}
	for source := range opts.RepositoryMap {
		if !sourceRepositories[source] {
			return DeliveryPlan{}, fmt.Errorf("%w: mapping has unknown source repository %s", ErrInvalid, source)
		}
	}
	primary := map[ArtifactID]string{}
	owners := map[string]string{}
	for id, descriptor := range identities {
		target := mapping[id.Repository]
		if relative, ok := overrides[id]; ok {
			target = dest.Registry + "/" + dest.Repository + "/" + relative
		} else {
			if owner, ok := owners[target]; ok && owner != id.Repository {
				return DeliveryPlan{}, fmt.Errorf("%w: repository collision: %s and %s map to %s; provide distinct mappings", ErrConflict, owner, id.Repository, target)
			}
			owners[target] = id.Repository
		}
		if target == repositoryName(dest) {
			return DeliveryPlan{}, fmt.Errorf("%w: member repository collides with bundle storage repository %s", ErrConflict, target)
		}
		primary[id] = target
		plan.Relocations = append(plan.Relocations, Relocation{Source: id, Destination: ArtifactID{Repository: target, Digest: id.Digest}, Descriptor: cloneDescriptor(descriptor)})
	}
	sort.Slice(plan.Relocations, func(i, j int) bool {
		a, b := plan.Relocations[i].Source, plan.Relocations[j].Source
		return a.Repository+"@"+a.Digest.String() < b.Repository+"@"+b.Digest.String()
	})
	roots := map[string]map[digest.Digest]v1.Descriptor{}
	addRoot := func(repo string, d v1.Descriptor) error {
		if roots[repo] == nil {
			roots[repo] = map[digest.Digest]v1.Descriptor{}
		}
		if prior, ok := roots[repo][d.Digest]; ok && (prior.Size != d.Size || prior.MediaType != d.MediaType) {
			return fmt.Errorf("%w: conflicting descriptor %s", ErrIntegrity, d.Digest)
		}
		roots[repo][d.Digest] = d
		return nil
	}
	tags := map[string]Tag{}
	addTag := func(t Tag) error {
		key := t.Repository + ":" + t.Reference
		if prior, ok := tags[key]; ok && prior.Descriptor.Digest != t.Descriptor.Digest {
			return fmt.Errorf("%w: tag collision %s", ErrConflict, key)
		}
		tags[key] = t
		return nil
	}
	for _, m := range metadata {
		for _, origin := range m.Origins {
			if err := addRoot(primary[ArtifactID{Repository: origin.Repository, Digest: origin.Descriptor.Digest}], origin.Descriptor); err != nil {
				return DeliveryPlan{}, err
			}
		}
		for _, root := range m.Roots {
			if root.Reference == root.Descriptor.Digest.String() {
				continue
			}
			if err := addTag(Tag{Repository: primary[ArtifactID{Repository: root.Repository, Digest: root.Descriptor.Digest}], Reference: root.Reference, Descriptor: root.Descriptor}); err != nil {
				return DeliveryPlan{}, err
			}
		}
		for _, plugin := range m.Plugins {
			ref, err := registry.ParseReference(plugin.Ref)
			if err != nil {
				return DeliveryPlan{}, err
			}
			if ref.Reference == plugin.Descriptor.Digest.String() {
				continue
			}
			if err := addTag(Tag{Repository: primary[ArtifactID{Repository: repositoryName(ref), Digest: plugin.Descriptor.Digest}], Reference: ref.Reference, Descriptor: plugin.Descriptor}); err != nil {
				return DeliveryPlan{}, err
			}
		}
		for _, resolution := range m.Dependencies {
			for _, required := range resolution.Dependencies {
				ref, err := registry.ParseReference(required.Ref)
				if err != nil {
					return DeliveryPlan{}, err
				}
				if ref.Reference == required.Descriptor.Digest.String() {
					continue
				}
				if err := addTag(Tag{Repository: primary[ArtifactID{Repository: repositoryName(ref), Digest: required.Descriptor.Digest}], Reference: ref.Reference, Descriptor: required.Descriptor}); err != nil {
					return DeliveryPlan{}, err
				}
			}
		}
	}
	plan.RepositoryMap = mapping
	// Expand placements through unchanged graph edges and captured subject links.
	// An explicit primary path does not remove copies required beside an index,
	// containing artifact, or subject of an attachment.
	plan.Referrers = []bundle.Association{}
	bySubject := map[ArtifactID][]bundle.Association{}
	byReferrer := map[ArtifactID][]bundle.Association{}
	for _, m := range metadata {
		for _, a := range m.Referrers {
			subject := ArtifactID{Repository: a.Repository, Digest: a.Subject.Digest}
			bySubject[subject] = append(bySubject[subject], a)
			referrer := ArtifactID{Repository: a.Repository, Digest: a.Descriptor.Digest}
			byReferrer[referrer] = append(byReferrer[referrer], a)
		}
	}
	type placement struct {
		source     string
		repository string
		descriptor v1.Descriptor
	}
	queue := []placement{{"", repositoryName(dest), plan.Descriptor}}
	for id, descriptor := range identities {
		queue = append(queue, placement{id.Repository, primary[id], descriptor}, placement{id.Repository, repositoryName(dest), descriptor})
	}
	seen := map[string]bool{}
	associations := map[string]bool{}
	envelopes := map[string]bundle.Envelope{}
	nestedOrigins := map[string][]bundle.Origin{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		physicalKey := current.repository + "@" + current.descriptor.Digest.String()
		envelopeKey := visitKey(current.descriptor, true)
		key := current.source + "@" + current.repository + "@" + envelopeKey
		if seen[key] {
			continue
		}
		seen[key] = true
		envelope, ok := envelopes[envelopeKey]
		if !ok {
			raw, err := fetchEnvelopeBytes(ctx, store, current.descriptor)
			if err != nil {
				return DeliveryPlan{}, err
			}
			envelope, err = bundle.ParseEnvelope(current.descriptor, raw)
			if err != nil {
				return DeliveryPlan{}, err
			}
			envelopes[envelopeKey] = envelope
		}
		if envelope.ArtifactType == bundle.BundleArtifactType {
			origins, ok := nestedOrigins[envelopeKey]
			if !ok {
				// Snapshot collection already validated every native snapshot against its
				// own closure. Placement needs only these captured coordinates.
				nested, err := bundle.InspectSnapshot(ctx, store, current.descriptor)
				if err != nil {
					return DeliveryPlan{}, err
				}
				origins = nested.Metadata.Origins
				nestedOrigins[envelopeKey] = origins
			}
			for _, origin := range origins {
				queue = append(queue, placement{origin.Repository, current.repository, origin.Descriptor})
			}
		}
		for i, child := range envelope.Children {
			if bundle.IsEnvelopeChild(current.descriptor, i, child) {
				source := current.source
				if envelope.ArtifactType == bundle.BundleArtifactType {
					source = ""
				}
				queue = append(queue, placement{source, current.repository, child})
			}
		}
		// Placing an attachment independently also places its captured subject.
		for _, a := range byReferrer[ArtifactID{Repository: current.source, Digest: current.descriptor.Digest}] {
			if err := addRoot(current.repository, a.Subject); err != nil {
				return DeliveryPlan{}, err
			}
			queue = append(queue, placement{current.source, current.repository, a.Subject})
		}
		for _, a := range bySubject[ArtifactID{Repository: current.source, Digest: current.descriptor.Digest}] {
			a.Repository = current.repository
			associationKey := physicalKey + "@" + a.Descriptor.Digest.String()
			if !associations[associationKey] {
				associations[associationKey] = true
				plan.Referrers = append(plan.Referrers, a)
				if err := addRoot(current.repository, a.Descriptor); err != nil {
					return DeliveryPlan{}, err
				}
			}
			queue = append(queue, placement{current.source, current.repository, a.Descriptor})
		}
	}
	sort.Slice(plan.Referrers, func(i, j int) bool {
		a, b := plan.Referrers[i], plan.Referrers[j]
		return a.Repository+a.Subject.Digest.String()+a.Descriptor.Digest.String() < b.Repository+b.Subject.Digest.String()+b.Descriptor.Digest.String()
	})
	names := make([]string, 0, len(roots))
	for name := range roots {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := DeliveryRepository{Repository: name, Roots: []v1.Descriptor{}}
		for _, d := range roots[name] {
			entry.Roots = append(entry.Roots, d)
		}
		sort.Slice(entry.Roots, func(i, j int) bool { return entry.Roots[i].Digest < entry.Roots[j].Digest })
		plan.Repositories = append(plan.Repositories, entry)
	}
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		plan.Tags = append(plan.Tags, tags[key])
	}
	targets := append([]DeliveryRepository{}, plan.Repositories...)
	targets = append(targets, DeliveryRepository{Repository: repositoryName(dest), Roots: []v1.Descriptor{plan.Descriptor}})
	generated, err := planDestinationAttachments(ctx, store, snapshots, plan)
	if err != nil {
		return DeliveryPlan{}, err
	}
	plan.Destinations = generated
	prepared, err := prepareDestinations(ctx, store, plan.Destinations)
	if err != nil {
		return DeliveryPlan{}, err
	}
	targets = appendDestinationTargets(targets, plan.Destinations)
	managed, err := managedReferrerTags(ctx, prepared, targets)
	if err != nil {
		return DeliveryPlan{}, err
	}
	completion := Tag{Repository: repositoryName(dest), Reference: dest.Reference, Descriptor: plan.Descriptor}
	if err := checkManagedTagCollisions(managed, append(append([]Tag{}, plan.Tags...), completion)); err != nil {
		return DeliveryPlan{}, err
	}
	plan.seal = planSeal(plan)
	return plan, nil
}
func planSeal(p DeliveryPlan) string {
	raw, _ := json.Marshal(p)
	return digest.FromBytes(raw).String()
}

func collectBundleMetadata(ctx context.Context, store content.ReadOnlyStorage, root v1.Descriptor, load func(context.Context, content.ReadOnlyStorage, v1.Descriptor) (bundle.Snapshot, error)) ([]bundle.Metadata, error) {
	snapshots, err := collectBundleSnapshots(ctx, store, root, load)
	if err != nil {
		return nil, err
	}
	result := make([]bundle.Metadata, len(snapshots))
	for i, snap := range snapshots {
		result[i] = snap.Metadata
	}
	return result, nil
}

func collectBundleSnapshots(ctx context.Context, store content.ReadOnlyStorage, root v1.Descriptor, load func(context.Context, content.ReadOnlyStorage, v1.Descriptor) (bundle.Snapshot, error)) ([]bundle.Snapshot, error) {
	result := []bundle.Snapshot{}
	seen := map[string]bool{}
	var walk func(v1.Descriptor, bool) error
	walk = func(d v1.Descriptor, isEnvelope bool) error {
		key := visitKey(d, isEnvelope)
		if seen[key] || !isEnvelope {
			return nil
		}
		seen[key] = true
		raw, err := fetchEnvelopeBytes(ctx, store, d)
		if err != nil {
			return err
		}
		var header struct {
			ArtifactType string `json:"artifactType"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return err
		}
		if header.ArtifactType == bundle.BundleArtifactType {
			snap, err := load(ctx, store, d)
			if err != nil {
				return err
			}
			result = append(result, snap)
		}
		e, err := bundle.ParseEnvelope(d, raw)
		if err != nil {
			return err
		}
		for i, child := range e.Children {
			if err := walk(child, bundle.IsEnvelopeChild(d, i, child)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, true); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Service) Push(ctx context.Context, plan DeliveryPlan) (result PushResult, retErr error) {
	result = PushResult{Descriptor: cloneDescriptor(plan.Descriptor), Destination: plan.Destination, Repositories: cloneJSON(plan.Repositories), RepositoryMap: cloneJSON(plan.RepositoryMap), Tags: []Tag{}, Referrers: cloneJSON(plan.Referrers), Relocations: Relocations{}, Destinations: []AttachResult{}, Warnings: []string{"Tag publication is not atomic; concurrent writers can race between validation and publication."}, Stats: Stats{Accounting: statsAccounting}}
	defer func() {
		if retErr != nil {
			retErr = operationError("push", plan.Destination, plan.Descriptor, nil, retErr)
		}
	}()

	if plan.seal == "" || plan.seal != planSeal(plan) {
		return result, operationError("plan", plan.Destination, plan.Descriptor, ErrInvalid, fmt.Errorf("delivery plan changed; generate a new plan"))
	}
	store, snapshot, err := openLayout(ctx, plan.LayoutPath)
	if err != nil {
		return result, err
	}
	if snapshot.Descriptor.Digest != plan.Descriptor.Digest {
		return result, operationError("plan", plan.Destination, plan.Descriptor, ErrIntegrity, fmt.Errorf("layout root changed since planning"))
	}
	dest, err := completionRef(plan.Destination)
	if err != nil {
		return result, operationError("destination", plan.Destination, plan.Descriptor, ErrInvalid, err)
	}
	prepared, err := prepareDestinations(ctx, store, plan.Destinations)
	if err != nil {
		return result, err
	}
	if err := s.acquirePublication(ctx); err != nil {
		return result, err
	}
	defer s.releasePublication()
	completion := Tag{Repository: repositoryName(dest), Reference: dest.Reference, Descriptor: plan.Descriptor}
	targets := append([]DeliveryRepository{}, plan.Repositories...)
	targets = append(targets, DeliveryRepository{Repository: completion.Repository, Roots: []v1.Descriptor{plan.Descriptor}})
	targets = appendDestinationTargets(targets, plan.Destinations)
	if err := s.preflightManagedTags(ctx, prepared, targets, append(append([]Tag{}, plan.Tags...), completion)); err != nil {
		return result, err
	}
	// Check every tag before any upload. Multi-tag updates are not atomic.
	for _, tag := range append(append([]Tag{}, plan.Tags...), completion) {
		if err := s.checkTag(ctx, tag); err != nil {
			return result, err
		}
	}
	for _, entry := range plan.Repositories {
		repo, err := s.repository(entry.Repository)
		if err != nil {
			return result, err
		}
		for _, root := range entry.Roots {
			if err := copyGraph(ctx, store, repo, root, &result.Stats, nil); err != nil {
				return result, err
			}
			if err := verifyRemoteClosure(ctx, repo, root); err != nil {
				return result, operationError("verify", entry.Repository, root, nil, err)
			}
			for _, relocation := range plan.Relocations {
				if relocation.Destination.Repository == entry.Repository && relocation.Destination.Digest == root.Digest {
					result.Relocations = append(result.Relocations, cloneJSON(relocation))
				}
			}
			s.progress(Progress{Operation: "copied", Repository: entry.Repository, Descriptor: root, Stats: result.Stats})
		}
	}
	// Re-pushing an unchanged selected referrer repairs a missing standard
	// fallback inventory on a retry, while ORAS handles native API discovery.
	for _, a := range plan.Referrers {
		target, err := s.repository(a.Repository)
		if err != nil {
			return result, err
		}
		raw, err := content.FetchAll(ctx, store, a.Descriptor)
		if err != nil {
			return result, err
		}
		if err := target.Push(ctx, a.Descriptor, bytes.NewReader(raw)); err != nil {
			return result, err
		}
		result.Stats.ManifestBytesUploaded += int64(len(raw))
		refs, err := listPinnedReferrers(ctx, target, a.Subject, ListReferrersOptions{})
		if err != nil {
			return result, err
		}
		found := hasAttachment(refs, a.Descriptor)
		if !found {
			return result, fmt.Errorf("%w: destination referrer %s is not discoverable beside %s", ErrIntegrity, a.Descriptor.Digest, a.Subject.Digest)
		}
	}
	repo, err := s.repository(completion.Repository)
	if err != nil {
		return result, err
	}
	if err := copyGraph(ctx, store, repo, plan.Descriptor, &result.Stats, nil); err != nil {
		return result, err
	}
	if err := verifyRemoteClosure(ctx, repo, plan.Descriptor); err != nil {
		return result, err
	}
	// Publish destination metadata only after every ordinary target and the
	// completion graph have been verified. Confirmed progress remains on errors.
	for _, attachment := range plan.Destinations {
		if err := confirmDestinationMappings(attachment.Record, result.Relocations); err != nil {
			return result, err
		}
	}
	for _, attachment := range plan.Destinations {
		attached, err := s.attachPinned(ctx, AttachRequest{Descriptor: attachment.Descriptor, Store: prepared}, attachment.Repository, attachment.Subject)
		result.Destinations = append(result.Destinations, attached)
		addAttachmentStats(&result.Stats, attached.Stats)
		if err != nil {
			return result, err
		}
	}
	// Recheck all conflicts before publishing the first alias.
	for _, tag := range append(append([]Tag{}, plan.Tags...), completion) {
		if err := s.checkTag(ctx, tag); err != nil {
			return result, err
		}
	}
	for _, tag := range plan.Tags {
		target, err := s.repository(tag.Repository)
		if err != nil {
			return result, err
		}
		if err := s.checkTag(ctx, tag); err != nil {
			return result, err
		}
		if err := target.Tag(ctx, tag.Descriptor, tag.Reference); err != nil {
			return result, err
		}
		observed, err := target.Resolve(ctx, tag.Reference)
		if err != nil {
			return result, err
		}
		if observed.Digest != tag.Descriptor.Digest {
			return result, operationError("tag", tag.Repository, tag.Descriptor, ErrConflict, fmt.Errorf("tag changed concurrently: %s:%s", tag.Repository, tag.Reference))
		}
		result.Tags = append(result.Tags, cloneJSON(tag))
		s.progress(Progress{Operation: "tagged", Repository: tag.Repository, Descriptor: tag.Descriptor, Stats: result.Stats})
	}
	if err := s.checkTag(ctx, completion); err != nil {
		return result, err
	}
	if err := repo.Tag(ctx, plan.Descriptor, dest.Reference); err != nil {
		return result, err
	}
	observed, err := repo.Resolve(ctx, dest.Reference)
	if err != nil {
		return result, err
	}
	if observed.Digest != plan.Descriptor.Digest {
		return result, operationError("tag", completion.Repository, completion.Descriptor, ErrConflict, fmt.Errorf("completion tag changed concurrently"))
	}
	s.progress(Progress{Operation: "tagged", Repository: completion.Repository, Descriptor: completion.Descriptor, Stats: result.Stats})
	return result, nil
}
