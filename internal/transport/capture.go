package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
)

type graph struct {
	service      *Service
	data         *metadataStore
	local        *metadataStore
	nodes        map[digest.Digest]v1.Descriptor
	source       map[digest.Digest]string
	meta         bundle.Metadata
	stats        Stats
	visited      map[string]bool
	discovered   map[string]bool
	origins      map[string]bool
	stores       map[string]content.ReadOnlyStorage
	referrers    map[string]registry.ReferrerLister
	payloads     map[digest.Digest]content.ReadOnlyStorage
	dependencies *dependencyCapture
}

func (s *Service) capture(ctx context.Context, recipe config.Recipe) (*graph, bundle.Snapshot, error) {
	if err := config.Validate(recipe); err != nil {
		return nil, bundle.Snapshot{}, err
	}
	if s.pluginRunner == nil {
		for i, input := range recipe.Include {
			if input.Plugin != nil {
				return nil, bundle.Snapshot{}, selectionError(i, selectionReference(input), v1.Descriptor{}, input.Plugin, fmt.Errorf("%w: configured plugins require a plugin runner", ErrInvalid))
			}
		}
	}
	g := newGraph(s)
	g.meta.Recipe = recipe
	resolved := map[string]v1.Descriptor{}
	g.dependencies = newDependencyCapture(g, resolved)
	tagLists := map[string][]string{}
	// Resolve every mutable root once before traversing any graph.
	for i, input := range recipe.Include {
		fail := func(err error) (*graph, bundle.Snapshot, error) {
			return nil, bundle.Snapshot{}, selectionError(i, selectionReference(input), v1.Descriptor{}, input.Plugin, err)
		}
		var repo string
		var refs []string
		if input.Ref != "" {
			r, err := parseRef(input.Ref)
			if err != nil {
				return fail(err)
			}
			repo = repositoryName(r)
			refs = []string{r.Reference}
		} else {
			repo = input.Repository
			refs = append([]string{}, (*input.Tags)...)
			if len(refs) == 0 {
				r, err := s.repository(repo)
				if err != nil {
					return fail(err)
				}
				if cached, ok := tagLists[repo]; ok {
					refs = append(refs, cached...)
				} else {
					s.recipeProgress("resolving", i, repo, repo, v1.Descriptor{}, input.Plugin)
					if err := r.Tags(ctx, "", func(tags []string) error { refs = append(refs, tags...); return nil }); err != nil {
						return fail(err)
					}
				}
				sort.Strings(refs)
				tagLists[repo] = append([]string{}, refs...)
				if len(refs) == 0 {
					return fail(fmt.Errorf("repository %s has no tags", repo))
				}
			}
		}
		r, err := s.repository(repo)
		if err != nil {
			return fail(err)
		}
		for _, ref := range refs {
			d, ok := resolved[repo+"@"+ref]
			if !ok {
				s.recipeProgress("resolving", i, fullReference(repo, ref), repo, v1.Descriptor{}, input.Plugin)
				d, err = r.Resolve(ctx, ref)
				resolved[repo+"@"+ref] = d
			}
			if err != nil {
				return nil, bundle.Snapshot{}, selectionError(i, fullReference(repo, ref), v1.Descriptor{}, input.Plugin, fmt.Errorf("resolve: %w", err))
			}
			if !bundle.IsEnvelope(d.MediaType) {
				return nil, bundle.Snapshot{}, selectionError(i, fullReference(repo, ref), d, input.Plugin, fmt.Errorf("%w: unsupported root envelope %s", ErrUnsupported, d.MediaType))
			}
			g.meta.Roots = append(g.meta.Roots, bundle.Root{Input: i, Repository: repo, Reference: ref, Descriptor: d})
		}
	}
	// Pin plugin selectors in the same observation table as artifact selectors.
	for i, input := range recipe.Include {
		if input.Plugin == nil {
			continue
		}
		root := bundle.Root{Input: i}
		for _, candidate := range g.meta.Roots {
			if candidate.Input == i {
				root = candidate
				break
			}
		}
		ref, _ := parseRef(input.Plugin.Ref)
		if _, pinned := resolved[repositoryName(ref)+"@"+ref.Reference]; !pinned {
			s.recipeProgress("resolving-plugin", i, fullReference(root.Repository, root.Reference), root.Repository, root.Descriptor, input.Plugin)
		}
		if _, _, err := g.dependencies.resolve(ctx, input.Plugin.Ref); err != nil {
			return nil, bundle.Snapshot{}, selectionError(i, fullReference(root.Repository, root.Reference), root.Descriptor, input.Plugin, fmt.Errorf("resolve plugin: %w", err))
		}
	}
	if err := g.capturePlugins(ctx, resolved); err != nil {
		return nil, bundle.Snapshot{}, err
	}
	for _, root := range g.meta.Roots {
		input := recipe.Include[root.Input]
		s.recipeProgress("capturing", root.Input, fullReference(root.Repository, root.Reference), root.Repository, root.Descriptor, input.Plugin)
		if err := g.captureRoot(ctx, root); err != nil {
			return nil, bundle.Snapshot{}, selectionError(root.Input, fullReference(root.Repository, root.Reference), root.Descriptor, input.Plugin, err)
		}
	}

	snap, err := bundle.BuildSnapshotFromGraph(ctx, g.data, g.meta, g.nodes)
	if err != nil {
		return nil, bundle.Snapshot{}, err
	}
	s.captureProgress(Progress{Operation: "captured", Descriptor: snap.Descriptor, Stats: g.stats})
	return g, snap, nil
}

func (g *graph) captureRoot(ctx context.Context, root bundle.Root) error {
	policy := g.meta.Recipe.Include[root.Input].Referrers
	if err := g.visit(ctx, root.Repository, root.Descriptor, policy.Include == "all", true); err != nil {
		return err
	}
	effectivePolicy, err := bundle.RootDependencyPolicy(g.meta, root)
	if err != nil {
		return err
	}
	if err := g.dependencies.expand(ctx, root.Repository, root.Descriptor, effectivePolicy, policy.Include == "all"); err != nil {
		return err
	}
	for _, req := range policy.Require {
		r, err := g.service.repository(root.Repository)
		if err != nil {
			return err
		}
		d, err := r.Resolve(ctx, req.Digest.String())
		if err != nil {
			return fmt.Errorf("required referrer %s: %w", req.Digest, err)
		}
		if err := g.referrer(ctx, root.Repository, root.Descriptor, d, policy.Include == "all"); err != nil {
			return err
		}
		if err := g.dependencies.expand(ctx, root.Repository, d, dependency.Policy{Follow: g.meta.Recipe.Include[root.Input].Dependencies.Follow}, policy.Include == "all"); err != nil {
			return err
		}
	}
	return nil
}
func (g *graph) visit(ctx context.Context, repo string, d v1.Descriptor, discover, isEnvelope bool) error {
	if isEnvelope && !bundle.IsEnvelope(d.MediaType) {
		return fmt.Errorf("%w: unsupported envelope media type %q", ErrUnsupported, d.MediaType)
	}
	key := repo + "@" + visitKey(d, isEnvelope)
	if !g.visited[key] {
		g.visited[key] = true
		if prior, ok := g.nodes[d.Digest]; ok && prior.Size != d.Size {
			return fmt.Errorf("%w: conflicting size for %s", ErrIntegrity, d.Digest)
		}
		if prior, ok := g.nodes[d.Digest]; !ok || isEnvelope || !bundle.IsEnvelope(prior.MediaType) {
			g.nodes[d.Digest] = d
		}
		g.source[d.Digest] = repo
		r, err := g.store(repo)
		if err != nil {
			return err
		}
		g.payloads[d.Digest] = r
		if !isEnvelope {
			if d.Data != nil {
				if err := g.data.Push(ctx, d, bytes.NewReader(d.Data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
					return err
				}
				return nil
			}
			ok, downloaded, err := captureBlobExists(ctx, r, d)
			g.stats.BlobBytesDownloaded += downloaded

			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: missing blob %s in %s", ErrNotFound, d.Digest, repo)
			}
			return nil
		}
		raw, err := fetchEnvelopeBytes(ctx, r, d)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", key, err)
		}
		generated, err := g.generated(ctx, d)
		if err != nil {
			return err
		}
		if d.Data == nil && !generated {
			g.stats.ManifestBytesDownloaded += int64(len(raw))
		}
		e, err := bundle.ParseEnvelope(d, raw)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrIntegrity, err)
		}
		if err := g.data.Push(ctx, d, bytes.NewReader(raw)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			return err
		}
		origin := repo + "@" + d.Digest.String()
		if !g.origins[origin] {
			g.origins[origin] = true
			g.meta.Origins = append(g.meta.Origins, bundle.Origin{Repository: repo, Descriptor: d})
		}
		childDiscover := discover && (g.dependencies == nil || e.ArtifactType != bundle.BundleArtifactType)
		for i, child := range e.Children {
			if err := g.visit(ctx, repo, child, childDiscover, bundle.IsEnvelopeChild(d, i, child)); err != nil {
				return err
			}
		}
	}
	if discover && isEnvelope && !g.discovered[key] {
		g.discovered[key] = true
		// Upgrade previously visited descendants when another path requests all.
		raw, err := content.FetchAll(ctx, g.data, d)
		if err != nil {
			return err
		}
		e, err := bundle.ParseEnvelope(d, raw)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrIntegrity, err)
		}
		if g.dependencies == nil || e.ArtifactType != bundle.BundleArtifactType {
			for i, child := range e.Children {
				if err := g.visit(ctx, repo, child, true, bundle.IsEnvelopeChild(d, i, child)); err != nil {
					return err
				}
			}
		}
		if g.dependencies != nil {
			refs, err := g.dependencies.inventory(ctx, repo, d)
			if err != nil {
				return err
			}
			for _, ref := range refs {
				if err := g.referrer(ctx, repo, d, ref, true); err != nil {
					return err
				}
			}
			return nil
		}
		provider, err := g.provider(repo)
		if err != nil {
			return err
		}
		return provider.Referrers(ctx, d, "", func(refs []v1.Descriptor) error {
			for _, ref := range refs {
				if err := g.referrer(ctx, repo, d, ref, true); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return nil
}
func (g *graph) referrer(ctx context.Context, repo string, subject, d v1.Descriptor, discover bool) error {
	if !bundle.IsEnvelope(d.MediaType) {
		return fmt.Errorf("%w: referrer %s has unsupported envelope %s", ErrUnsupported, d.Digest, d.MediaType)
	}
	r, err := g.store(repo)
	if err != nil {
		return err
	}
	raw, err := fetchEnvelopeBytes(ctx, r, d)
	if err != nil {
		return err
	}
	generated, err := g.generated(ctx, d)
	if err != nil {
		return err
	}
	if d.Data == nil && !generated {
		g.stats.ManifestBytesDownloaded += int64(len(raw))
	}
	e, err := bundle.ParseEnvelope(d, raw)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrIntegrity, err)
	}
	if e.Subject == nil || e.Subject.Digest != subject.Digest || e.Subject.Size != subject.Size || e.Subject.MediaType != subject.MediaType {
		return fmt.Errorf("%w: referrer %s does not reference subject %s in %s", ErrIntegrity, d.Digest, subject.Digest, repo)
	}
	found := false
	for _, a := range g.meta.Referrers {
		if a.Repository == repo && a.Subject.Digest == subject.Digest && a.Descriptor.Digest == d.Digest {
			found = true
		}
	}
	if !found {
		g.meta.Referrers = append(g.meta.Referrers, bundle.Association{Repository: repo, Subject: subject, Descriptor: d})
	}
	return g.visit(ctx, repo, d, discover, true)
}
func (g *graph) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	if ok, err := g.data.Exists(ctx, d); err != nil {
		return nil, err
	} else if ok {
		return g.data.Fetch(ctx, d)
	}
	repo := g.source[d.Digest]
	if repo == "" {
		return nil, fmt.Errorf("no source for %s", d.Digest)
	}
	r := g.payloads[d.Digest]
	if r == nil {
		return nil, fmt.Errorf("no pinned source for %s", d.Digest)
	}
	var err error
	var rc io.ReadCloser
	if remoteStore, remote := r.(*remote.Repository); remote {
		rc, err = remoteStore.Blobs().Fetch(ctx, d)
	} else {
		rc, err = r.Fetch(ctx, d)
	}
	if err == nil {
		g.stats.BlobBytesDownloaded += d.Size
	}
	return rc, err
}

var _ content.Fetcher = (*graph)(nil)

func newGraph(s *Service) *graph {
	return &graph{service: s, data: newMetadataStore(), nodes: map[digest.Digest]v1.Descriptor{}, source: map[digest.Digest]string{}, visited: map[string]bool{}, discovered: map[string]bool{}, origins: map[string]bool{}, stores: map[string]content.ReadOnlyStorage{}, referrers: map[string]registry.ReferrerLister{}, payloads: map[digest.Digest]content.ReadOnlyStorage{}, stats: Stats{Accounting: statsAccounting}, meta: bundle.Metadata{SchemaVersion: 1, Roots: []bundle.Root{}, Origins: []bundle.Origin{}, Referrers: []bundle.Association{}}}
}
func (g *graph) store(repo string) (content.ReadOnlyStorage, error) {
	source := g.stores[repo]
	if source == nil {
		var err error
		source, err = g.service.repository(repo)
		if err != nil {
			return nil, err
		}
	}
	if g.local != nil {
		return overlayStorage{local: g.local, source: source}, nil
	}
	return source, nil
}
func (g *graph) provider(repo string) (registry.ReferrerLister, error) {
	if _, ok := g.stores[repo]; ok {
		if provider := g.referrers[repo]; provider != nil {
			return provider, nil
		}
		return nil, fmt.Errorf("%w: source %s cannot discover referrers", ErrUnsupported, repo)
	}
	r, err := g.service.repository(repo)
	if err != nil {
		return nil, err
	}
	return validatedRegistryReferrers{r}, nil
}

func fullReference(repository, reference string) string {
	if reference == "" {
		return repository
	}
	if strings.Contains(reference, ":") {
		return repository + "@" + reference
	}
	return repository + ":" + reference
}

func selectionReference(input config.Selection) string {
	if input.Ref != "" {
		return input.Ref
	}
	return input.Repository
}

func selectionError(index int, reference string, subject v1.Descriptor, plugin *config.Plugin, err error) error {
	label := fmt.Sprintf("include[%d] artifact %s", index, reference)
	if subject.Digest != "" {
		label += " (subject " + subject.Digest.String() + ")"
	}
	if plugin != nil {
		label += " plugin " + plugin.Ref
	}
	return fmt.Errorf("%s: %w", label, err)
}

func (s *Service) recipeProgress(operation string, index int, reference, repository string, descriptor v1.Descriptor, plugin *config.Plugin) {
	event := Progress{Operation: operation, Input: &index, Reference: reference, Repository: repository, Descriptor: descriptor}
	if plugin != nil {
		event.Plugin = plugin.Ref
	}
	s.captureProgress(event)
}
