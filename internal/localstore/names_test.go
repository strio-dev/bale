package localstore

import (
	"errors"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
)

func TestShortIDsResolveTagAndRemoveRetainedRoots(t *testing.T) {
	s := newStore(t)
	root := fixture(t, s, "source.example/short", "release:v1", "release:approved")
	prefix := root.Descriptor.Digest.Encoded()[:12]
	if err := s.With(t.Context(), func(session *Session) error {
		for _, reference := range []string{prefix, "sha256:" + prefix, root.Descriptor.Digest.Encoded(), "release@" + root.Descriptor.Digest.String()} {
			record, err := session.Resolve(t.Context(), reference)
			if err != nil || record.Descriptor.Digest != root.Descriptor.Digest {
				t.Fatalf("resolve %q: %s %v", reference, record.Descriptor.Digest, err)
			}
		}
		if err := session.Tag(t.Context(), prefix, []string{"release:short"}); err != nil {
			return err
		}
		if record, err := session.Resolve(t.Context(), "release:short"); err != nil || record.Descriptor.Digest != root.Descriptor.Digest {
			t.Fatalf("short-ID tag selected the wrong root: %s %v", record.Descriptor.Digest, err)
		}
		if err := session.Remove(t.Context(), []string{"sha256:" + prefix}); err != nil {
			return err
		}
		for _, reference := range []string{prefix, "release:v1", "release:approved", "release:short"} {
			if _, err := session.Resolve(t.Context(), reference); !errors.Is(err, errdef.ErrNotFound) {
				t.Fatalf("removed root still resolves as %q: %v", reference, err)
			}
		}
		exists, err := session.Storage().Exists(t.Context(), root.Descriptor)
		if err != nil || !exists {
			t.Fatalf("reference removal deleted CAS bytes: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExactHexNamesTakePrecedenceOverShortIDs(t *testing.T) {
	s := newStore(t)
	byID := fixture(t, s, "source.example/by-id")
	prefix := byID.Descriptor.Digest.Encoded()[:12]
	byName := fixture(t, s, "source.example/by-name", prefix, "sha256:"+prefix, byID.Descriptor.Digest.Encoded(), "keep")
	if err := s.With(t.Context(), func(session *Session) error {
		for _, reference := range []string{prefix, "sha256:" + prefix, byID.Descriptor.Digest.Encoded()} {
			record, err := session.Resolve(t.Context(), reference)
			if err != nil || record.Descriptor.Digest != byName.Descriptor.Digest {
				t.Fatalf("exact name %q did not win: %s %v", reference, record.Descriptor.Digest, err)
			}
			if err := session.Remove(t.Context(), []string{reference}); err != nil {
				return err
			}
		}
		for _, reference := range []string{"keep", byID.Descriptor.Digest.String(), prefix} {
			if _, err := session.Resolve(t.Context(), reference); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestShortIDsDoNotSelectUnretainedCASContent(t *testing.T) {
	s := newStore(t)
	if err := s.With(t.Context(), func(session *Session) error {
		unretained := snapshot(t, session.Storage(), member(t, session.Storage(), "unretained"), "source.example/unretained")
		for _, reference := range []string{unretained.Descriptor.Digest.Encoded()[:12], unretained.Descriptor.Digest.String()} {
			if _, err := session.Resolve(t.Context(), reference); !errors.Is(err, errdef.ErrNotFound) {
				t.Fatalf("unretained CAS content resolved by %q: %v", reference, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAmbiguousShortIDsLeaveRemovalBatchUnchanged(t *testing.T) {
	s := newStore(t)
	retained := fixture(t, s, "source.example/retained", "keep")
	// Colliding IDs exercise index selection only; these descriptors are never
	// passed to content inspection, which separately verifies bundle integrity.
	prefix := "0123456789ab"
	first := digest.Digest("sha256:" + prefix + strings.Repeat("0", 52))
	second := digest.Digest("sha256:" + prefix + strings.Repeat("1", 52))
	if err := s.With(t.Context(), func(session *Session) error {
		if err := session.metadata(t.Context(), func(current *index) error {
			for _, id := range []digest.Digest{first, second} {
				current.roots[id] = v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: id, Size: 1}
			}
			return session.writeIndex(t.Context(), current)
		}); err != nil {
			return err
		}
		for _, reference := range []string{prefix, "sha256:" + prefix} {
			if _, err := session.Resolve(t.Context(), reference); err == nil || !strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("ambiguous selector %q: %v", reference, err)
			}
			if err := session.Tag(t.Context(), reference, []string{"ambiguous"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("ambiguous tag %q: %v", reference, err)
			}
			if err := session.Remove(t.Context(), []string{"keep", reference}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("ambiguous removal %q: %v", reference, err)
			}
			current, err := session.readIndex()
			if err != nil {
				return err
			}
			if current.names["localhost/keep:latest"] != retained.Descriptor.Digest || len(current.names) != 1 || len(current.roots) != 3 {
				t.Fatal("failed removal batch changed retained roots or aliases")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMalformedShortIDs(t *testing.T) {
	s := newStore(t)
	if err := s.With(t.Context(), func(session *Session) error {
		for _, reference := range []string{"sha256:0123456789a", "sha256:0123456789ag", "sha256:" + strings.Repeat("a", 65)} {
			if _, err := session.Resolve(t.Context(), reference); err == nil || !strings.Contains(err.Error(), "bundle ID") {
				t.Fatalf("malformed ID %q: %v", reference, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
