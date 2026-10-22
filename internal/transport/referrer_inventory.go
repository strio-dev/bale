package transport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale/internal/bundle"
	"oras.land/oras-go/v2/registry/remote"
)

// inventoryClient validates raw inventory responses before ORAS decodes them.
// Only marked enumeration requests use it; actual referrer fetches keep their
// original context because their manifest responses are not inventory indexes.
type inventoryClient struct {
	remote.Client
	subject  v1.Descriptor
	maxBytes int64
	budget   *destinationInspectionBudget
}

func (c inventoryClient) Do(request *http.Request) (*http.Response, error) {
	response, err := c.Client.Do(request)
	if err != nil {
		return response, err
	}
	if request.Method != http.MethodGet || response.StatusCode != http.StatusOK {
		return response, nil
	}
	limit := c.maxBytes
	if limit <= 0 {
		limit = 4 * 1024 * 1024
	}
	aggregateLimit := c.budget != nil && c.budget.remaining <= limit
	if aggregateLimit {
		limit = c.budget.remaining
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	invalid := func(reason string) (*http.Response, error) {
		return nil, fmt.Errorf("%w: invalid referrers inventory: %s", ErrIntegrity, reason)
	}
	if int64(len(raw)) > limit {
		if aggregateLimit {
			return nil, fmt.Errorf("%w: destination inspection metadata byte limit exceeded", ErrUnsupported)
		}
		return invalid("metadata size limit exceeded")
	}
	if response.ContentLength >= 0 && response.ContentLength != int64(len(raw)) {
		return invalid("response content length mismatch")
	}
	if response.Header.Get("Content-Type") != v1.MediaTypeImageIndex {
		return invalid("response is not an OCI image index")
	}
	if value := response.Header.Get("Docker-Content-Digest"); value != "" {
		advertised := digest.Digest(value)
		if err := advertised.Validate(); err != nil {
			return invalid("malformed content digest header")
		}
		if !advertised.Algorithm().Available() || advertised.Algorithm().FromBytes(raw) != advertised {
			return invalid("content digest header mismatch")
		}
	}
	if c.budget != nil {
		if err := c.budget.charge(int64(len(raw))); err != nil {
			return nil, err
		}
		if c.budget.maxReferrers > 0 {
			// Count entries with a streaming decoder before OCI envelope parsing retains
			// typed descriptors. The enclosing raw page is already byte bounded.
			decoder := json.NewDecoder(bytes.NewReader(raw))
			token, err := decoder.Token()
			if err != nil || token != json.Delim('{') {
				return invalid("inventory must be an object")
			}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return invalid(err.Error())
				}
				if key == "manifests" {
					token, err := decoder.Token()
					if err != nil || token != json.Delim('[') {
						return invalid("manifests must be an array")
					}
					for decoder.More() {
						if c.budget.referrers >= c.budget.maxReferrers {
							return nil, fmt.Errorf("%w: destination inspection referrer count limit exceeded", ErrUnsupported)
						}
						var item json.RawMessage
						if err := decoder.Decode(&item); err != nil {
							return invalid(err.Error())
						}
						c.budget.referrers++
					}
					if _, err := decoder.Token(); err != nil {
						return invalid(err.Error())
					}
				} else {
					var value json.RawMessage
					if err := decoder.Decode(&value); err != nil {
						return invalid(err.Error())
					}
				}
			}
		}
	}
	descriptor := v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
	envelope, err := bundle.ParseEnvelope(descriptor, raw)
	if err != nil {
		return invalid(err.Error())
	}
	if envelope.Subject != nil {
		return invalid("inventory cannot have a subject")
	}
	fallback := "/manifests/" + strings.Replace(c.subject.Digest.String(), ":", "-", 1)
	if strings.HasSuffix(request.URL.Path, fallback) && c.subject.Digest.Algorithm().FromBytes(raw) == c.subject.Digest {
		return invalid("fallback inventory resolves to the subject itself")
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	return response, nil
}

// The marker is private to a single enumeration call. Actual envelope requests
// never carry it, so OCI indexes with subjects remain ordinary artifact reads.
type inventoryValidationKey struct{}
type inventoryValidationOptions struct {
	subject  v1.Descriptor
	maxBytes int64
}

type inventoryValidationClient struct{ remote.Client }

func (c inventoryValidationClient) Do(request *http.Request) (*http.Response, error) {
	if options, ok := request.Context().Value(inventoryValidationKey{}).(inventoryValidationOptions); ok {
		return (inventoryClient{Client: c.Client, subject: options.subject, maxBytes: options.maxBytes, budget: inspectionBudget(request)}).Do(request)
	}
	return c.Client.Do(request)
}

func inspectionBudget(request *http.Request) *destinationInspectionBudget {
	budget, _ := request.Context().Value(destinationInspectionBudgetKey{}).(*destinationInspectionBudget)
	return budget
}
