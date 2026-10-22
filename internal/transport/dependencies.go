package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
)

// Declaration expansion is capture policy, separate from immutable graph copy.
// Its repository/policy state must not be conflated with payload deduplication.
type dependencyCapture struct {
	graph       *graph
	resolved    map[string]v1.Descriptor
	inventories map[string][]v1.Descriptor
	candidates  map[string][]declarationCandidate
	visited     map[string]bool
	records     map[string]int
}

type declarationCandidate struct {
	descriptor  v1.Descriptor
	declaration dependency.Declaration
}

type dependencyMetadataLimitKey struct{}

const maxDependencyStates = 10000

func newDependencyCapture(g *graph, resolved map[string]v1.Descriptor) *dependencyCapture {
	return &dependencyCapture{graph: g, resolved: resolved, inventories: map[string][]v1.Descriptor{}, candidates: map[string][]declarationCandidate{}, visited: map[string]bool{}, records: map[string]int{}}
}

// inventory freezes all pages once, shared by ordinary referrer capture and
// dependency selection. A failed listing never becomes an empty observation.
func (c *dependencyCapture) inventory(ctx context.Context, repo string, subject v1.Descriptor) ([]v1.Descriptor, error) {
	key := repo + "@" + subject.Digest.String()
	if refs, ok := c.inventories[key]; ok {
		return refs, nil
	}
	provider, err := c.graph.provider(repo)
	if err != nil {
		return nil, err
	}
	refs := []v1.Descriptor{}
	seen := map[string]v1.Descriptor{}
	inventoryContext := context.WithValue(ctx, dependencyMetadataLimitKey{}, dependency.MaxBytes)
	err = provider.Referrers(inventoryContext, subject, "", func(page []v1.Descriptor) error {
		for _, d := range page {
			if d.Size > dependency.MaxBytes {
				return fmt.Errorf("%w: referrer metadata exceeds dependency discovery limit of %d bytes", ErrUnsupported, dependency.MaxBytes)
			}
			if prior, ok := seen[d.Digest.String()]; ok {
				if prior.MediaType != d.MediaType || prior.Size != d.Size || prior.ArtifactType != d.ArtifactType {
					return fmt.Errorf("%w: conflicting referrer inventory for %s", ErrIntegrity, d.Digest)
				}
				continue
			}
			seen[d.Digest.String()] = d
			refs = append(refs, cloneDescriptor(d))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Digest < refs[j].Digest })
	c.inventories[key] = refs
	return refs, nil
}

func (c *dependencyCapture) declarations(ctx context.Context, repo string, subject v1.Descriptor) ([]declarationCandidate, error) {
	key := repo + "@" + subject.Digest.String()
	if candidates, ok := c.candidates[key]; ok {
		return candidates, nil
	}
	refs, err := c.inventory(ctx, repo, subject)
	if err != nil {
		return nil, err
	}
	store, err := c.graph.store(repo)
	if err != nil {
		return nil, err
	}
	candidates := []declarationCandidate{}
	for _, d := range refs {
		if d.Size > dependency.MaxBytes {
			return nil, fmt.Errorf("%w: dependency candidate manifest exceeds %d bytes", ErrUnsupported, dependency.MaxBytes)
		}
		raw, err := fetchEnvelopeBytes(ctx, store, d)
		if err != nil {
			return nil, err
		}
		env, err := bundle.ParseEnvelope(d, raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrIntegrity, err)
		}
		if !strings.HasPrefix(env.ArtifactType, "application/vnd.bale.dependencies.") {
			continue
		}
		if env.ArtifactType != dependency.ArtifactType {
			return nil, fmt.Errorf("%w: unsupported dependency declaration type %s", ErrUnsupported, env.ArtifactType)
		}
		decl, err := bundle.LoadDependencyDeclaration(ctx, attachmentSource{store}, d, subject)
		if err != nil {
			kind := ErrIntegrity
			var sourceErr *attachmentSourceError
			if errors.As(err, &sourceErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				kind = nil
			}
			return nil, operationError("dependency declaration", repo, d, kind, err)
		}
		candidates = append(candidates, declarationCandidate{d, decl})
	}
	c.candidates[key] = candidates
	return candidates, nil
}

func (c *dependencyCapture) resolve(ctx context.Context, reference string) (string, v1.Descriptor, error) {
	ref, err := parseRef(reference)
	if err != nil {
		return "", v1.Descriptor{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	repo := repositoryName(ref)
	key := repo + "@" + ref.Reference
	if d, ok := c.resolved[key]; ok {
		return repo, d, nil
	}
	store, err := c.graph.service.repository(repo)
	if err != nil {
		return "", v1.Descriptor{}, err
	}
	d, err := store.Resolve(ctx, ref.Reference)
	if err != nil {
		return "", v1.Descriptor{}, fmt.Errorf("resolve dependency %s: %w", reference, err)
	}
	if !bundle.IsEnvelope(d.MediaType) {
		return "", v1.Descriptor{}, fmt.Errorf("%w: unsupported dependency envelope %s", ErrUnsupported, d.MediaType)
	}
	c.resolved[key] = d
	return repo, d, nil
}

// The graph normally caches only envelopes. The small selected declaration
// documents must also be available to the bundle's source-independent validator.
func (c *dependencyCapture) cacheDeclaration(ctx context.Context, repo string, descriptor v1.Descriptor) error {
	raw, err := content.FetchAll(ctx, c.graph.data, descriptor)
	if err != nil {
		return err
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return err
	}
	store, err := c.graph.store(repo)
	if err != nil {
		return err
	}
	blobs := []v1.Descriptor{manifest.Config}
	for _, layer := range manifest.Layers {
		if layer.MediaType == dependency.ArtifactType {
			blobs = append(blobs, layer)
		}
	}
	for _, blob := range blobs {
		if ok, err := c.graph.data.Exists(ctx, blob); err != nil {
			return err
		} else if ok {
			continue
		}
		data := blob.Data
		if data == nil {
			data, err = content.FetchAll(ctx, store, blob)
			if err != nil {
				return err
			}
			generated, err := c.graph.generated(ctx, blob)
			if err != nil {
				return err
			}
			if !generated {
				c.graph.stats.BlobBytesDownloaded += int64(len(data))
			}
		}
		if err := c.graph.data.Push(ctx, blob, bytes.NewReader(data)); err != nil {
			return err
		}
	}
	return nil
}

func (c *dependencyCapture) expand(ctx context.Context, repo string, subject v1.Descriptor, policy dependency.Policy, optional bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	policy, err := dependency.NormalizePolicy(policy)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	policyJSON, _ := json.Marshal(policy)
	state := repo + "@" + subject.Digest.String() + "/" + string(policyJSON)
	visit := fmt.Sprintf("%s/%t", state, optional)
	if c.visited[visit] {
		return nil
	}
	if len(c.visited) >= maxDependencyStates {
		return fmt.Errorf("%w: dependency expansion exceeds %d states", ErrUnsupported, maxDependencyStates)
	}
	c.visited[visit] = true
	raw, err := content.FetchAll(ctx, c.graph.data, subject)
	if err != nil {
		return err
	}
	env, err := bundle.ParseEnvelope(subject, raw)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrIntegrity, err)
	}
	if env.ArtifactType == bundle.BundleArtifactType {
		// Its existing capture owns the members and their locked observations.
		store, err := c.graph.store(repo)
		if err != nil {
			return err
		}
		if _, err = bundle.InspectSnapshot(ctx, store, subject); err != nil {
			return err
		}
		return c.expandReferrers(ctx, repo, subject, policy, optional)
	}
	if strings.HasPrefix(env.ArtifactType, "application/vnd.bale.dependencies.") {
		return nil
	}
	index, recorded := c.records[state]
	if !recorded {
		record := bundle.DependencyResolution{Repository: repo, Subject: subject, Policy: policy, Outcome: "absent", Dependencies: []bundle.ResolvedDependency{}}
		if policy.Follow == "none" {
			record.Outcome = "disabled"
		} else {
			candidates, err := c.declarations(ctx, repo, subject)
			if err != nil {
				return err
			}
			matches := []declarationCandidate{}
			for _, candidate := range candidates {
				if policy.Select != nil {
					if policy.Select.Digest != "" && policy.Select.Digest != candidate.descriptor.Digest {
						continue
					}
					if policy.Select.Context != "" && policy.Select.Context != candidate.declaration.Context {
						continue
					}
				}
				matches = append(matches, candidate)
			}
			if len(matches) > 1 {
				return fmt.Errorf("%w: %s@%s has multiple dependency declarations; select one explicitly", ErrConflict, repo, subject.Digest)
			}
			if len(matches) == 0 && policy.Select != nil {
				return fmt.Errorf("%w: dependency declaration selection does not match %s@%s", ErrNotFound, repo, subject.Digest)
			}
			if len(matches) == 1 {
				selected := matches[0]
				record.Outcome = "selected"
				record.Declaration = &selected.descriptor
				for _, entry := range selected.declaration.Dependencies {
					_, d, err := c.resolve(ctx, entry.Ref)
					if err != nil {
						return err
					}
					childPolicy := dependency.Policy{Follow: policy.Follow, Select: entry.Select}
					if entry.Follow == "none" {
						childPolicy.Follow = "none"
					}
					childPolicy, err = dependency.NormalizePolicy(childPolicy)
					if err != nil {
						return err
					}
					record.Dependencies = append(record.Dependencies, bundle.ResolvedDependency{Ref: entry.Ref, Descriptor: d, Policy: childPolicy})
				}
			}
		}
		index = len(c.graph.meta.Dependencies)
		c.graph.meta.Dependencies = append(c.graph.meta.Dependencies, record)
		c.records[state] = index
	}
	// Copy the record: recursion can grow/reallocate the metadata slice.
	record := c.graph.meta.Dependencies[index]
	if record.Outcome == "disabled" {
		return nil
	}
	if record.Declaration != nil {
		if err := c.graph.referrer(ctx, repo, subject, *record.Declaration, optional); err != nil {
			return err
		}
		if err := c.cacheDeclaration(ctx, repo, *record.Declaration); err != nil {
			return err
		}
	}
	for _, target := range record.Dependencies {
		ref, _ := registry.ParseReference(target.Ref)
		targetRepo := repositoryName(ref)
		if err := c.graph.visit(ctx, targetRepo, target.Descriptor, optional, true); err != nil {
			return fmt.Errorf("capture dependency %s: %w", target.Ref, err)
		}
		if err := c.expand(ctx, targetRepo, target.Descriptor, target.Policy, optional); err != nil {
			return fmt.Errorf("expand dependency %s: %w", target.Ref, err)
		}
	}
	childPolicy := dependency.Policy{Follow: policy.Follow}
	for i, child := range env.Children {
		if !bundle.IsEnvelopeChild(subject, i, child) {
			continue
		}
		if err := c.expand(ctx, repo, child, childPolicy, optional); err != nil {
			return err
		}
	}
	return c.expandReferrers(ctx, repo, subject, policy, optional)
}

// A native bundle freezes its own members, while new outer attachments retain
// the enclosing capture's policy. Declaration metadata itself stays opaque.
func (c *dependencyCapture) expandReferrers(ctx context.Context, repo string, subject v1.Descriptor, policy dependency.Policy, optional bool) error {
	if !optional || policy.Follow == "none" {
		return nil
	}
	childPolicy := dependency.Policy{Follow: policy.Follow}
	associations := append([]bundle.Association{}, c.graph.meta.Referrers...)
	for _, association := range associations {
		if association.Repository != repo || association.Subject.Digest != subject.Digest {
			continue
		}
		if err := c.expand(ctx, repo, association.Descriptor, childPolicy, optional); err != nil {
			return err
		}
	}
	return nil
}
