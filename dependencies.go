package bale

import (
	"context"
	"github.com/strio-dev/bale/internal/dependency"
	"github.com/strio-dev/bale/internal/transport"
)

// EncodeDependencyDeclaration validates and deterministically encodes inclusion
// metadata. It defaults an omitted version and encodes nil dependencies as [].
func EncodeDependencyDeclaration(declaration DependencyDeclaration) ([]byte, error) {
	raw, err := dependency.Encode(declaration)
	if err != nil {
		return nil, &Error{Kind: ErrInvalid, Op: "encode dependency declaration", Cause: err}
	}
	return raw, nil
}

// DecodeDependencyDeclaration strictly validates declaration JSON and returns
// canonical, deduplicated entries. It does not resolve any references.
func DecodeDependencyDeclaration(raw []byte) (DependencyDeclaration, error) {
	declaration, err := dependency.Decode(raw)
	if err != nil {
		return DependencyDeclaration{}, &Error{Kind: ErrInvalid, Op: "decode dependency declaration", Cause: err}
	}
	return declaration, nil
}

// ParseDependencyDeclaration parses strict user-authored YAML with an implicit
// schema version, using the same selector semantics as registry JSON. References
// are normalized and deduplicated without network access.
func ParseDependencyDeclaration(data []byte) (DependencyDeclaration, error) {
	declaration, err := dependency.ParseYAML(data)
	if err != nil {
		return DependencyDeclaration{}, &Error{Kind: ErrInvalid, Op: "parse dependency declaration", Cause: err}
	}
	return declaration, nil
}

// PrepareDependencyDeclaration creates a deterministic, metadata-only OCI
// attachment for an already pinned subject. It neither reads subject.Store nor
// resolves the subject or declared dependencies. Pass its result to Client.Attach
// to publish it to the subject's repository.
func PrepareDependencyDeclaration(ctx context.Context, subject Root, declaration DependencyDeclaration) (AttachRequest, error) {
	return transport.PrepareDependencyDeclaration(ctx, subject, declaration)
}
