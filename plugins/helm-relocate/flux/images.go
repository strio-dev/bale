package flux

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/distribution/reference"
)

type Image struct {
	Name    string `json:"name"`
	NewName string `json:"newName"`
	Digest  string `json:"digest"`
}
type Kustomize struct {
	Images []Image `json:"images"`
}
type PostRenderer struct {
	Kustomize Kustomize `json:"kustomize"`
}

// Images emits exact reference patterns for Flux helm-controller 1.6.5's
// Kustomize 0.21.1 image transformer. That transformer appends an optional
// tag/digest suffix to Name: anchoring our full reference prevents a different
// source tag from matching. Unmapped images are intentionally left unchanged;
// this metadata service cannot validate rendered manifests.
func Images(mapping map[string]string) ([]Image, error) {
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Image, 0, len(keys))
	seen := map[string]string{}
	for _, key := range keys {
		n, err := reference.ParseNormalizedNamed(key)
		if err != nil {
			return nil, err
		}
		dest, err := reference.ParseNormalizedNamed(mapping[key])
		if err != nil {
			return nil, err
		}
		d, ok := dest.(reference.Digested)
		if !ok {
			return nil, fmt.Errorf("destination must be digest pinned")
		}
		repos := repositoryAliases(reference.TrimNamed(n))
		parts := make([]string, 0, len(repos))
		for _, repo := range repos {
			parts = append(parts, regexp.QuoteMeta(repo))
		}
		pattern := "^(" + strings.Join(parts, "|") + ")"
		if digest, ok := n.(reference.Digested); ok {
			// Distribution normalization ignores a tag when a digest is present. Permit
			// any valid Docker tag before this exact digest, never a different digest.
			pattern += "(:" + reference.TagRegexp.String() + ")?@" + regexp.QuoteMeta(digest.Digest().String())
		} else {
			tagged := reference.TagNameOnly(n).(reference.Tagged)
			if tagged.Tag() == "latest" {
				pattern += "(:latest)?"
			} else {
				pattern += ":" + regexp.QuoteMeta(tagged.Tag())
			}
		}
		pattern += "$"
		target := reference.TrimNamed(dest).String() + "@" + d.Digest().String()
		if prior, ok := seen[pattern]; ok {
			if prior != target {
				return nil, fmt.Errorf("conflicting image patterns")
			}
			continue
		}
		seen[pattern] = target
		out = append(out, Image{Name: pattern, NewName: reference.TrimNamed(dest).String(), Digest: d.Digest().String()})
	}
	return out, nil
}
func repositoryAliases(n reference.Named) []string {
	repo := n.String()
	out := []string{repo}
	if reference.Domain(n) == "docker.io" {
		path := reference.Path(n)
		out = append(out, "index.docker.io/"+path, path)
		if strings.HasPrefix(path, "library/") {
			short := strings.TrimPrefix(path, "library/")
			out = append(out, short, "docker.io/"+short, "index.docker.io/"+short)
		}
	}
	// An unqualified path whose first component is localhost or contains a dot
	// names a registry. Keep only aliases that normalize back to this repository.
	aliases := out[:0]
	for _, alias := range out {
		parsed, err := reference.ParseNormalizedNamed(alias)
		if err == nil && reference.TrimNamed(parsed).String() == repo {
			aliases = append(aliases, alias)
		}
	}
	out = aliases
	sort.Strings(out)
	return out
}
