package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"github.com/strio-dev/bale/internal/config"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
)

// ArtifactID identifies an immutable artifact in a particular repository.
type ArtifactID struct {
	Repository string        `json:"repository"`
	Digest     digest.Digest `json:"digest"`
}
type Root struct {
	Repository     string
	Descriptor     v1.Descriptor
	Store          content.ReadOnlyStorage
	Aliases        []string
	Referrers      config.Policy
	ReferrerSource registry.ReferrerLister
}
type TransferRequest struct {
	Roots         []Root
	Destination   string
	RepositoryMap map[string]string
}
type Relocation struct {
	Source      ArtifactID    `json:"source"`
	Destination ArtifactID    `json:"destination"`
	Descriptor  v1.Descriptor `json:"descriptor"`
}
type Relocations []Relocation

func (r Relocations) Lookup(id ArtifactID) (Relocation, bool) {
	for _, entry := range r {
		if entry.Source == id {
			return cloneJSON(entry), true
		}
	}
	return Relocation{}, false
}

type PlanView struct {
	Destination   string               `json:"destination"`
	Repositories  []DeliveryRepository `json:"repositories"`
	RepositoryMap map[string]string    `json:"repositoryMap"`
	Tags          []Tag                `json:"tags"`
	Referrers     []bundle.Association `json:"referrers"`
	Relocations   Relocations          `json:"relocations"`
}

// TransferPlan holds pinned graph metadata. It is executable only in this process.
// Caller-owned stores must remain available until Execute completes.
type TransferPlan struct {
	view  PlanView
	graph *graph
}

func (p *TransferPlan) View() PlanView {
	if p == nil {
		return PlanView{}
	}
	return cloneJSON(p.view)
}

type Result struct {
	Relocations Relocations `json:"relocations"`
	Tags        []Tag       `json:"tags"`
	Stats       Stats       `json:"stats"`
	Warnings    []string    `json:"warnings"`
}
type Progress struct {
	// Input identifies the zero-based recipe selection; nil marks non-recipe work.
	Input      *int
	Reference  string
	Plugin     string
	Operation  string
	Repository string
	Descriptor v1.Descriptor
	Stats      Stats
}

func cloneJSON[T any](value T) T {
	raw, _ := json.Marshal(value)
	var result T
	_ = json.Unmarshal(raw, &result)
	return result
}

func (s *Service) Resolve(ctx context.Context, reference string) (Root, error) {
	ref, err := parseRef(reference)
	if err != nil {
		return Root{}, operationError("resolve", reference, v1.Descriptor{}, ErrInvalid, err)
	}
	name := repositoryName(ref)
	repo, err := s.repository(ctx, name)
	if err != nil {
		return Root{}, operationError("resolve", name, v1.Descriptor{}, nil, err)
	}
	d, err := repo.Resolve(ctx, ref.Reference)
	if err != nil {
		return Root{}, operationError("resolve", name, d, nil, err)
	}
	if !bundle.IsEnvelope(d.MediaType) {
		return Root{}, operationError("resolve", name, d, ErrUnsupported, fmt.Errorf("unsupported root envelope %s", d.MediaType))
	}
	root := Root{Repository: name, Descriptor: cloneDescriptor(d), Store: repo, ReferrerSource: validatedRegistryReferrers{repo}, Referrers: config.Policy{Include: "all", Require: []config.RequiredReferrer{}}}
	if ref.Reference != d.Digest.String() {
		root.Aliases = []string{ref.Reference}
	}
	return root, nil
}

// Plan captures immutable graph metadata and checks destinations without writing.
func (s *Service) Plan(ctx context.Context, request TransferRequest) (*TransferPlan, error) {
	fail := func(kind, err error) (*TransferPlan, error) {
		return nil, operationError("plan", request.Destination, v1.Descriptor{}, kind, err)
	}
	if err := ctx.Err(); err != nil {
		return fail(nil, err)
	}
	// An explicit registry host is a useful empty repository prefix for publishing
	// an exact target path. Bare names remain ambiguous and are rejected.
	dest, err := registry.ParseReference(request.Destination)
	if !strings.Contains(request.Destination, "/") {
		dest = registry.Reference{Registry: request.Destination}
		err = dest.ValidateRegistry()
		if dest.Registry != "localhost" && !strings.ContainsAny(dest.Registry, ".:") {
			err = fmt.Errorf("explicit registry required")
		}
	}
	if err != nil || dest.Reference != "" {
		return fail(ErrInvalid, fmt.Errorf("destination must be a registry/repository prefix"))
	}
	if len(request.Roots) == 0 {
		return fail(ErrInvalid, fmt.Errorf("at least one root is required"))
	}
	g := newGraph(s)
	view := PlanView{Destination: request.Destination, RepositoryMap: map[string]string{}, Repositories: []DeliveryRepository{}, Tags: []Tag{}, Referrers: []bundle.Association{}, Relocations: Relocations{}}
	owners := map[string]string{}
	roots := make([]Root, len(request.Roots))
	for i, root := range request.Roots {
		root.Descriptor = cloneDescriptor(root.Descriptor)
		root.Aliases = append([]string{}, root.Aliases...)
		normalized, err := config.Normalize(config.Recipe{Include: []config.Selection{{Ref: root.Repository + "@" + root.Descriptor.Digest.String(), Referrers: root.Referrers}}})
		if err != nil {
			return fail(ErrInvalid, err)
		}
		root.Referrers = normalized.Include[0].Referrers
		src, err := registry.ParseReference(root.Repository)
		if err != nil || src.Reference != "" || root.Store == nil {
			return fail(ErrInvalid, fmt.Errorf("root needs a repository and source store"))
		}
		if !bundle.IsEnvelope(root.Descriptor.MediaType) {
			return fail(ErrUnsupported, fmt.Errorf("unsupported root envelope %s", root.Descriptor.MediaType))
		}
		if root.ReferrerSource == nil {
			root.ReferrerSource, _ = root.Store.(registry.ReferrerLister)
		}
		if root.Referrers.Include == "all" && root.ReferrerSource == nil {
			return fail(ErrUnsupported, fmt.Errorf("source %s cannot discover referrers", root.Repository))
		}

		relative := src.Repository
		if mapped, ok := request.RepositoryMap[root.Repository]; ok {
			relative = mapped
		}
		target, err := registry.ParseReference(request.Destination + "/" + relative)
		if err != nil || relative == "" || strings.HasPrefix(relative, "/") || target.Reference != "" || target.Registry != dest.Registry {
			return fail(ErrInvalid, fmt.Errorf("invalid repository mapping for %s", root.Repository))
		}
		name := repositoryName(target)
		if prior, ok := owners[name]; ok && prior != root.Repository {
			return fail(ErrConflict, fmt.Errorf("repository collision for %s", name))
		}
		owners[name] = root.Repository
		view.RepositoryMap[root.Repository] = name
		roots[i] = root
	}
	for source := range request.RepositoryMap {
		if _, ok := view.RepositoryMap[source]; !ok {
			return fail(ErrInvalid, fmt.Errorf("unknown source repository %s", source))
		}
	}
	for _, root := range roots {
		sourceFail := func(kind, cause error) (*TransferPlan, error) {
			return nil, operationError("plan", root.Repository, root.Descriptor, kind, cause)
		}
		g.stores[root.Repository] = root.Store
		g.referrers[root.Repository] = root.ReferrerSource
		// A caller may provide distinct inventories for partial stores sharing
		// repository provenance. Deduplicate discovery within this source walk,
		// while retaining the union of verified bytes and selected associations.
		g.discovered = map[string]bool{}
		if err := g.visit(ctx, root.Repository, root.Descriptor, root.Referrers.Include == "all", true); err != nil {
			return sourceFail(nil, err)
		}
		for _, required := range root.Referrers.Require {
			resolver, ok := root.Store.(content.Resolver)
			var d v1.Descriptor
			if ok {
				d, err = resolver.Resolve(ctx, required.Digest.String())
			} else if root.ReferrerSource != nil {
				err = root.ReferrerSource.Referrers(ctx, root.Descriptor, "", func(refs []v1.Descriptor) error {
					for _, candidate := range refs {
						if candidate.Digest == required.Digest {
							d = candidate
						}
					}
					return nil
				})
				if err == nil && d.Digest == "" {
					err = fmt.Errorf("%w: required referrer %s not found", ErrNotFound, required.Digest)
				}
			} else {
				return sourceFail(ErrUnsupported, fmt.Errorf("source cannot resolve required referrer %s", required.Digest))
			}
			if err != nil {
				return sourceFail(nil, err)
			}
			if d.Digest != required.Digest {
				return sourceFail(ErrIntegrity, fmt.Errorf("required referrer resolved to another digest"))
			}
			if err := g.referrer(ctx, root.Repository, root.Descriptor, d, root.Referrers.Include == "all"); err != nil {
				return sourceFail(nil, err)
			}
		}
	}
	byRepo := map[string][]v1.Descriptor{}
	for _, origin := range g.meta.Origins {
		target := view.RepositoryMap[origin.Repository]
		byRepo[target] = append(byRepo[target], origin.Descriptor)
		view.Relocations = append(view.Relocations, Relocation{Source: ArtifactID{origin.Repository, origin.Descriptor.Digest}, Destination: ArtifactID{target, origin.Descriptor.Digest}, Descriptor: origin.Descriptor})
	}
	names := make([]string, 0, len(byRepo))
	for name := range byRepo {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sort.Slice(byRepo[name], func(i, j int) bool { return byRepo[name][i].Digest < byRepo[name][j].Digest })
		view.Repositories = append(view.Repositories, DeliveryRepository{Repository: name, Roots: byRepo[name]})
	}
	tags := map[string]Tag{}
	for _, root := range roots {
		for _, alias := range root.Aliases {
			ref, err := completionRef(view.RepositoryMap[root.Repository] + ":" + alias)
			if err != nil || ref.Reference != alias {
				return fail(ErrInvalid, fmt.Errorf("invalid alias %q", alias))
			}
			tag := Tag{Repository: view.RepositoryMap[root.Repository], Reference: alias, Descriptor: root.Descriptor}
			key := tag.Repository + ":" + alias
			if prior, ok := tags[key]; ok && prior.Descriptor.Digest != tag.Descriptor.Digest {
				return fail(ErrConflict, fmt.Errorf("alias collision %s", key))
			}
			tags[key] = tag
		}
	}
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		view.Tags = append(view.Tags, tags[key])
	}
	for _, a := range g.meta.Referrers {
		a.Repository = view.RepositoryMap[a.Repository]
		view.Referrers = append(view.Referrers, a)
	}
	if err := s.preflightManagedTags(ctx, g, view.Repositories, view.Tags); err != nil {
		return fail(nil, err)
	}
	for _, tag := range view.Tags {
		if err := s.checkTag(ctx, tag); err != nil {
			return fail(nil, err)
		}
	}
	return &TransferPlan{view: cloneJSON(view), graph: g}, nil
}

// Execute publishes pinned bytes, reporting only verified relocations and aliases.
func (s *Service) Execute(ctx context.Context, plan *TransferPlan) (result Result, retErr error) {
	result = Result{Relocations: Relocations{}, Tags: []Tag{}, Warnings: []string{"Tag publication is not atomic; concurrent writers can race between validation and publication."}, Stats: Stats{Accounting: statsAccounting}}
	fail := func(op, repo string, d v1.Descriptor, kind, err error) (Result, error) {
		return result, operationError(op, repo, d, kind, err)
	}
	if plan == nil || plan.graph == nil {
		return fail("execute", "", v1.Descriptor{}, ErrInvalid, fmt.Errorf("invalid transfer plan"))
	}
	// Copy per-execution counters; graph metadata and source handles are pinned
	// and read-only after planning, including when a plan is executed again.
	graphCopy := *plan.graph
	g, view := &graphCopy, plan.view
	g.stats = Stats{Accounting: statsAccounting}
	result.Stats.Accounting += " Direct relocation excludes planning reads."
	defer func() {
		result.Stats.BlobBytesDownloaded = g.stats.BlobBytesDownloaded
		result.Stats.ManifestBytesDownloaded = g.stats.ManifestBytesDownloaded
	}()
	if err := ctx.Err(); err != nil {
		return fail("execute", view.Destination, v1.Descriptor{}, nil, err)
	}
	if err := s.acquirePublication(ctx); err != nil {
		return fail("execute", view.Destination, v1.Descriptor{}, nil, err)
	}
	defer s.releasePublication()
	if err := s.preflightManagedTags(ctx, g, view.Repositories, view.Tags); err != nil {
		return fail("preflight", view.Destination, v1.Descriptor{}, nil, err)
	}
	for _, tag := range view.Tags {
		if err := s.checkTag(ctx, tag); err != nil {
			return fail("preflight", tag.Repository, tag.Descriptor, nil, err)
		}
	}
	for _, entry := range view.Repositories {
		repo, err := s.repository(ctx, entry.Repository)
		if err != nil {
			return fail("execute", entry.Repository, v1.Descriptor{}, nil, err)
		}
		for _, root := range entry.Roots {
			if err := copyGraph(ctx, g, repo, root, &result.Stats, g.source); err != nil {
				return fail("copy", entry.Repository, root, nil, err)
			}
			if err := verifyRemoteClosure(ctx, repo, root); err != nil {
				return fail("verify", entry.Repository, root, nil, err)
			}
			for _, relocation := range view.Relocations {
				if relocation.Destination.Repository == entry.Repository && relocation.Descriptor.Digest == root.Digest {
					result.Relocations = append(result.Relocations, cloneJSON(relocation))
				}
			}
			result.Stats.BlobBytesDownloaded = g.stats.BlobBytesDownloaded
			result.Stats.ManifestBytesDownloaded = g.stats.ManifestBytesDownloaded
			s.progress(Progress{Operation: "copied", Repository: entry.Repository, Descriptor: root, Stats: result.Stats})
		}
	}
	for _, a := range view.Referrers {
		repo, err := s.repository(ctx, a.Repository)
		if err != nil {
			return fail("referrers", a.Repository, a.Descriptor, nil, err)
		}
		raw, err := content.FetchAll(ctx, g, a.Descriptor)
		if err != nil {
			return fail("referrers", a.Repository, a.Descriptor, nil, err)
		}
		if err := repo.Push(ctx, a.Descriptor, bytes.NewReader(raw)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			return fail("referrers", a.Repository, a.Descriptor, nil, err)
		} else if err != nil {
			result.Stats.ObjectsSkipped++
		} else {
			result.Stats.ManifestBytesUploaded += int64(len(raw))
		}
		refs, err := listPinnedReferrers(ctx, repo, a.Subject, ListReferrersOptions{})
		if err != nil {
			return fail("referrers", a.Repository, a.Descriptor, nil, err)
		}
		found := hasAttachment(refs, a.Descriptor)
		if !found {
			return fail("referrers", a.Repository, a.Descriptor, ErrIntegrity, fmt.Errorf("published referrer is not discoverable"))
		}
	}
	for _, tag := range view.Tags {
		if err := s.checkTag(ctx, tag); err != nil {
			return fail("tag", tag.Repository, tag.Descriptor, nil, err)
		}
		repo, err := s.repository(ctx, tag.Repository)
		if err != nil {
			return fail("tag", tag.Repository, tag.Descriptor, nil, err)
		}
		if err := repo.Tag(ctx, tag.Descriptor, tag.Reference); err != nil {
			return fail("tag", tag.Repository, tag.Descriptor, nil, err)
		}
		d, err := repo.Resolve(ctx, tag.Reference)
		if err != nil {
			return fail("tag", tag.Repository, tag.Descriptor, nil, err)
		}
		if d.Digest != tag.Descriptor.Digest {
			return fail("tag", tag.Repository, tag.Descriptor, ErrConflict, fmt.Errorf("tag changed concurrently"))
		}
		result.Tags = append(result.Tags, cloneJSON(tag))
		s.progress(Progress{Operation: "tagged", Repository: tag.Repository, Descriptor: tag.Descriptor, Stats: result.Stats})
	}
	return result, nil
}
