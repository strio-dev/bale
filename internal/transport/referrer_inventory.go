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
	fallback := "/manifests/" + strings.Replace(c.subject.Digest.String(), ":", "-", 1)
	raw, validationErr := validateInventoryPage(ReferrerPage{
		MediaType: response.Header.Get("Content-Type"),
		Digest:    digest.Digest(response.Header.Get("Docker-Content-Digest")),
		Size:      response.ContentLength,
		Content:   response.Body,
	}, c.subject, c.maxBytes, c.budget, strings.HasSuffix(request.URL.Path, fallback))
	if err := errors.Join(validationErr, response.Body.Close()); err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	return response, nil
}

// validateInventoryPage is shared by HTTP-backed ORAS responses and opaque
// caller-owned backends. Validate and bound raw bytes before retaining entries.
func validateInventoryPage(page ReferrerPage, subject v1.Descriptor, maxBytes int64, budget *destinationInspectionBudget, fallback bool) ([]byte, error) {
	invalid := func(reason string) ([]byte, error) {
		return nil, fmt.Errorf("%w: invalid referrers inventory: %s", ErrIntegrity, reason)
	}
	if page.Content == nil {
		return invalid("missing content")
	}
	if page.Size < -1 {
		return invalid("invalid content length")
	}
	limit := inventoryByteLimit(maxBytes)
	aggregateLimit := budget != nil && budget.remaining <= limit
	if aggregateLimit {
		limit = budget.remaining
	}
	raw, err := io.ReadAll(io.LimitReader(page.Content, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		if aggregateLimit {
			return nil, fmt.Errorf("%w: destination inspection metadata byte limit exceeded", ErrUnsupported)
		}
		return invalid("metadata size limit exceeded")
	}
	if page.Size >= 0 && page.Size != int64(len(raw)) {
		return invalid("response content length mismatch")
	}
	if page.MediaType != v1.MediaTypeImageIndex {
		return invalid("response is not an OCI image index")
	}
	if advertised := page.Digest; advertised != "" {
		if err := advertised.Validate(); err != nil {
			return invalid("malformed content digest header")
		}
		if !advertised.Algorithm().Available() || advertised.Algorithm().FromBytes(raw) != advertised {
			return invalid("content digest header mismatch")
		}
	}
	if budget != nil {
		if err := budget.charge(int64(len(raw))); err != nil {
			return nil, err
		}
		if budget.maxReferrers > 0 {
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
						if budget.referrers >= budget.maxReferrers {
							return nil, fmt.Errorf("%w: destination inspection referrer count limit exceeded", ErrUnsupported)
						}
						var item json.RawMessage
						if err := decoder.Decode(&item); err != nil {
							return invalid(err.Error())
						}
						budget.referrers++
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
	if fallback && subject.Digest.Algorithm().Available() && subject.Digest.Algorithm().FromBytes(raw) == subject.Digest {
		return invalid("fallback inventory resolves to the subject itself")
	}
	return raw, nil
}

func inventoryByteLimit(configured int64) int64 {
	if configured > 0 {
		return configured
	}
	return 4 * 1024 * 1024
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
