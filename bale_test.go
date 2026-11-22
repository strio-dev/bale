package bale_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/registry/remote/auth"
)

func TestNewClientDoesNotReadDockerConfiguration(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "config.json"), []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", directory)
	if _, err := bale.NewClient(bale.ClientOptions{}); err != nil {
		t.Fatalf("anonymous SDK construction read Docker configuration: %v", err)
	}
}

func TestRecipeErrorsAreTyped(t *testing.T) {
	for _, parse := range []func() error{
		func() error { _, err := bale.ParseRecipe([]byte("include: []\n")); return err },
		func() error { _, err := bale.NormalizeRecipe(bale.Recipe{}); return err },
	} {
		err := parse()
		var detail *bale.Error
		if !errors.Is(err, bale.ErrInvalid) || !errors.As(err, &detail) || errors.Unwrap(err) == nil {
			t.Fatalf("expected typed invalid recipe error, got %v", err)
		}
	}
}

type authenticatedDoFunc func(*http.Request) (*http.Response, error)

func (f authenticatedDoFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestAuthenticatedClientRoutesRequestsAndValidatesInventory(t *testing.T) {
	const host = "127.0.0.1:0" // The Do-only client never opens a connection.
	const subjectBody = `{"schemaVersion":2,"manifests":[]}`
	subject := digest.FromString(subjectBody)
	for _, tc := range []struct {
		name, inventory string
		wantErr         error
	}{
		{"valid inventory", subjectBody, nil},
		// ORAS decodes this as an empty index; Bale must reject its schema.
		{"invalid inventory", `{"schemaVersion":1,"manifests":[]}`, bale.ErrIntegrity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var manifests, inventories int
			doer := authenticatedDoFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != host || req.URL.Scheme != "https" {
					return nil, fmt.Errorf("unexpected registry origin %s", req.URL)
				}
				var raw, body string
				switch req.Method + " " + req.URL.Path {
				case "HEAD /v2/bale/manifests/stable":
					manifests++
					raw = subjectBody
				case "GET /v2/bale/referrers/" + subject.String():
					inventories++
					raw, body = tc.inventory, tc.inventory
				default:
					return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL)
				}
				h := make(http.Header)
				h.Set("Content-Type", v1.MediaTypeImageIndex)
				h.Set("Docker-Content-Digest", digest.FromString(raw).String())
				return &http.Response{
					StatusCode: http.StatusOK, Request: req, Header: h,
					ContentLength: int64(len(raw)), Body: io.NopCloser(strings.NewReader(body)),
				}, nil
			})
			client, err := bale.NewClient(bale.ClientOptions{AuthenticatedClient: doer})
			if err != nil {
				t.Fatal(err)
			}
			refs, err := client.ListReferrers(t.Context(), host+"/bale:stable", bale.ListReferrersOptions{})
			if !errors.Is(err, tc.wantErr) || len(refs) != 0 {
				t.Fatalf("refs=%v err=%v (want %v)", refs, err, tc.wantErr)
			}
			if manifests != 1 || inventories != 1 {
				t.Fatalf("Do calls: manifests=%d inventories=%d, want one of each", manifests, inventories)
			}
		})
	}
}

func TestAuthenticatedClientOptionConflicts(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts bale.ClientOptions
	}{
		{"HTTPClient", bale.ClientOptions{HTTPClient: &http.Client{}}},
		{"Credentials", bale.ClientOptions{Credentials: func(context.Context, string) (auth.Credential, error) {
			return auth.Credential{}, nil
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.AuthenticatedClient = authenticatedDoFunc(func(req *http.Request) (*http.Response, error) {
				t.Fatalf("construction called Do on %s", req.URL)
				return nil, nil
			})
			client, err := bale.NewClient(tc.opts)
			if client != nil || !errors.Is(err, bale.ErrInvalid) {
				t.Fatalf("conflicting options: client=%v err=%v, want ErrInvalid and no client", client, err)
			}
		})
	}
}
