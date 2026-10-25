package localstore

import (
	"fmt"
	"strings"

	digest "github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/registry"
)

// NormalizeTag uses explicit local names; it never searches registries.
func NormalizeTag(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, " \t\r\n@") || strings.Contains(value, "://") || strings.HasPrefix(value, "-") {
		return "", fmt.Errorf("invalid local name %q", value)
	}
	if _, err := digest.Parse(value); err == nil {
		return "", fmt.Errorf("a local name must not be a digest")
	}
	first, _, hasSlash := strings.Cut(value, "/")
	if !hasSlash || first != "localhost" && !strings.ContainsAny(first, ".:") {
		value = "localhost/" + value
	}
	ref, err := registry.ParseReference(value)
	if err != nil {
		return "", fmt.Errorf("invalid local name: %w", err)
	}
	if ref.Reference == "" {
		ref.Reference = "latest"
	}
	if err := ref.ValidateReferenceAsTag(); err != nil {
		return "", fmt.Errorf("invalid local tag: %w", err)
	}
	return ref.String(), nil
}

func selector(value string) (digest.Digest, string, error) {
	if strings.TrimSpace(value) != value || value == "" {
		return "", "", fmt.Errorf("invalid local reference %q", value)
	}
	if at := strings.LastIndexByte(value, '@'); at >= 0 {
		if _, err := NormalizeTag(value[:at]); err != nil {
			return "", "", err
		}
		d, err := digest.Parse(value[at+1:])
		return d, "", err
	}
	if d, err := digest.Parse(value); err == nil {
		return d, "", nil
	}
	if len(value) == 64 && !strings.ContainsAny(value, "/:") {
		if d, err := digest.Parse("sha256:" + value); err == nil {
			return d, "", nil
		}
	}
	name, err := NormalizeTag(value)
	return "", name, err
}

// shortID recognizes digest prefixes, leaving ordinary local names unchanged.
// Bare prefixes select SHA-256 roots; other algorithms must be explicit.
func shortID(value string) (string, error) {
	if strings.ContainsAny(value, "/@") {
		return "", nil
	}
	algorithm, encoded := digest.SHA256, value
	qualified := false
	if name, rest, ok := strings.Cut(value, ":"); ok {
		algorithm, encoded, qualified = digest.Algorithm(name), rest, true
		if !algorithm.Available() {
			return "", nil
		}
	}
	hex := encoded != ""
	for _, c := range encoded {
		hex = hex && (c >= '0' && c <= '9' || c >= 'a' && c <= 'f')
	}
	if !qualified && (!hex || len(encoded) < 12) {
		return "", nil
	}
	if !hex || len(encoded) < 12 || len(encoded) > algorithm.Size()*2 {
		return "", fmt.Errorf("invalid bundle ID %q: expected 12 to %d lowercase hexadecimal characters", value, algorithm.Size()*2)
	}
	return algorithm.String() + ":" + encoded, nil
}
