package transport

import (
	"errors"
	"fmt"
	"io"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

var (
	ErrInvalid     = errors.New("invalid request")
	ErrUnsupported = errors.New("unsupported operation")
	ErrNotFound    = errors.New("artifact not found")
	ErrIntegrity   = errors.New("artifact integrity failure")
	ErrConflict    = errors.New("publication conflict")
)

// Error identifies the operation and artifact while preserving its original cause.
type Error struct {
	Op         string
	Repository string
	Descriptor v1.Descriptor
	Kind       error
	Cause      error
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s %s %s: %v", e.Op, e.Repository, e.Descriptor.Digest, e.Cause)
}
func (e *Error) Unwrap() error        { return e.Cause }
func (e *Error) Is(target error) bool { return target == e.Kind }
func operationError(op, repo string, d v1.Descriptor, kind, cause error) error {
	if cause == nil {
		return nil
	}

	for _, candidate := range []error{ErrInvalid, ErrUnsupported, ErrNotFound, ErrIntegrity, ErrConflict} {
		if errors.Is(cause, candidate) {
			kind = candidate
			break
		}
	}
	if errors.Is(cause, errdef.ErrNotFound) {
		kind = ErrNotFound
	}
	if errors.Is(cause, content.ErrMismatchedDigest) || errors.Is(cause, content.ErrTrailingData) || errors.Is(cause, content.ErrInvalidDescriptorSize) || errors.Is(cause, io.ErrUnexpectedEOF) {
		kind = ErrIntegrity
	}

	return &Error{Op: op, Repository: repo, Descriptor: cloneDescriptor(d), Kind: kind, Cause: cause}
}
