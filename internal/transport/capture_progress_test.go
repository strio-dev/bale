package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/strio-dev/bale/internal/config"
	"github.com/strio-dev/bale/internal/dependency"
)

func TestCaptureProgressPrecedesWorkAndDryRunDoesNotPublish(t *testing.T) {
	r, repo, s := testRegistry(t)
	subject := object(t, r.store, "subject")
	r.tags["chart"] = subject
	module := capturePluginFixture(t, r)
	var events []Progress
	s.onProgress = func(p Progress) { events = append(events, p) }
	has := func(operation string) bool {
		for _, event := range events {
			if event.Operation == operation {
				return true
			}
		}
		return false
	}
	s.client.Client = &http.Client{Transport: deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodHead && req.Method != http.MethodGet {
			t.Fatalf("dry run wrote registry: %s", req.Method)
		}
		if req.Method == http.MethodHead && strings.HasSuffix(req.URL.Path, "/manifests/chart") && !has("resolving") {
			t.Fatal("root resolve preceded callback")
		}
		if req.Method == http.MethodHead && strings.HasSuffix(req.URL.Path, "/manifests/plugin") && !has("resolving-plugin") {
			t.Fatal("plugin resolve preceded callback")
		}
		if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/manifests/"+module.Digest.String()) && !has("fetching-plugin") {
			t.Fatal("plugin fetch preceded callback")
		}
		return http.DefaultTransport.RoundTrip(req)
	})}
	calls := 0
	s.pluginRunner = func(context.Context, PluginRequest) (dependency.Declaration, error) {
		calls++
		if !has("running-plugin") {
			t.Fatal("runner preceded callback")
		}
		return dependency.Declaration{}, nil
	}
	input := dependencySelection(repo + ":chart")
	input.Plugin = &config.Plugin{Ref: repo + ":plugin", Input: json.RawMessage(`{}`)}
	result, err := s.Create(ctx, config.Recipe{Include: []config.Selection{input, input}}, repo+":bundle", CreateOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || r.headTags["chart"] != 1 || r.headTags["plugin"] != 1 {
		t.Fatalf("progress changed observation: calls=%d heads=%v", calls, r.headTags)
	}
	want := []string{"resolving", "resolving-plugin", "fetching-plugin", "running-plugin", "fetching-plugin", "running-plugin", "capturing", "capturing", "captured"}
	if len(events) != len(want) {
		t.Fatalf("unexpected events: %+v", events)
	}
	for i, event := range events {
		if event.Operation != want[i] {
			t.Fatalf("event[%d]=%s want %s", i, event.Operation, want[i])
		}
		if event.Operation == "captured" {
			if event.Input != nil || event.Descriptor.Digest != result.Descriptor.Digest {
				t.Fatalf("wrong global completion: %+v", event)
			}
			continue
		}
		if event.Input == nil || event.Reference != input.Ref || event.Plugin != input.Plugin.Ref || event.Repository != repo {
			t.Fatalf("lost recipe context: %+v", event)
		}
		if event.Operation != "resolving" && event.Descriptor.Digest != subject.Digest {
			t.Fatalf("progress does not identify subject: %+v", event)
		}
	}
	if result.Published || !result.DryRun {
		t.Fatalf("wrong dry run result: %+v", result)
	}
	if _, ok := r.tags["bundle"]; ok {
		t.Fatal("dry run published bundle")
	}
}

// A single pinned module can fail for a later subject/profile. The error must
// identify the selection being executed rather than the first plugin user.
func TestCapturePluginFailureContextAndCause(t *testing.T) {
	for _, mode := range []string{"runner", "cancel", "invalid declaration", "binding", "fetch"} {
		t.Run(mode, func(t *testing.T) {
			r, repo, s := testRegistry(t)
			first := object(t, r.store, "first subject")
			second := object(t, r.store, "second subject")
			r.tags["first"], r.tags["second"] = first, second
			module := capturePluginFixture(t, r)
			inputs := []config.Selection{dependencySelection(repo + ":first"), dependencySelection(repo + ":second")}
			for i := range inputs {
				inputs[i].Plugin = &config.Plugin{Ref: repo + ":plugin", Input: json.RawMessage(fmt.Sprintf(`{"profile":%d}`, i))}
			}
			if mode == "binding" {
				inputs[1].Plugin.Blobs = map[string]config.PluginBlob{"chart": {MediaType: "missing/type"}}
			}
			sentinel := fmt.Errorf("guest diagnostic: %w", ErrUnsupported)
			calls := 0
			var events []Progress
			s.onProgress = func(p Progress) { events = append(events, p) }
			captureCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			s.pluginRunner = func(_ context.Context, request PluginRequest) (dependency.Declaration, error) {
				calls++
				if calls == 1 {
					return dependency.Declaration{}, nil
				}
				switch mode {
				case "runner":
					return dependency.Declaration{}, sentinel
				case "cancel":
					cancel()
					return dependency.Declaration{}, nil
				case "invalid declaration":
					return dependency.Declaration{SchemaVersion: 99}, nil
				}
				return dependency.Declaration{}, nil
			}
			if mode == "fetch" {
				s.client.Client = &http.Client{Transport: deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
					if len(events) > 0 && events[len(events)-1].Operation == "fetching-plugin" && *events[len(events)-1].Input == 1 && req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/manifests/"+module.Digest.String()) {
						return nil, sentinel
					}
					return http.DefaultTransport.RoundTrip(req)
				})}
			}
			_, _, err := s.capture(captureCtx, config.Recipe{Include: inputs})
			if err == nil {
				t.Fatal("accepted failed capture")
			}
			for _, part := range []string{"include[1]", inputs[1].Ref, second.Digest.String(), inputs[1].Plugin.Ref} {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("missing %q: %v", part, err)
				}
			}
			if mode == "runner" && (!errors.Is(err, sentinel) || !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "guest diagnostic")) {
				t.Fatalf("lost runner cause: %v", err)
			}
			if mode == "fetch" {
				var requestError *url.Error
				if !errors.Is(err, sentinel) || !errors.As(err, &requestError) {
					t.Fatalf("lost fetch cause/type: %v", err)
				}
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			if mode == "invalid declaration" && !errors.Is(err, ErrInvalid) {
				t.Fatalf("lost invalid classification: %v", err)
			}
			for _, event := range events {
				if event.Operation == "captured" {
					t.Fatal("reported capture success on failure")
				}
			}
		})
	}
}

func TestCaptureResolutionFailureContext(t *testing.T) {
	for _, pluginFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("plugin=%t", pluginFailure), func(t *testing.T) {
			r, repo, s := testRegistry(t)
			subject := object(t, r.store, "subject")
			r.tags["first"], r.tags["second"] = subject, subject
			capturePluginFixture(t, r)
			inputs := []config.Selection{dependencySelection(repo + ":first"), dependencySelection(repo + ":second")}
			s.pluginRunner = func(context.Context, PluginRequest) (dependency.Declaration, error) {
				t.Fatal("resolution failure invoked plugin")
				return dependency.Declaration{}, nil
			}
			if pluginFailure {
				inputs[1].Plugin = &config.Plugin{Ref: repo + ":missing", Input: json.RawMessage(`{}`)}
			}
			sentinel := errors.New("resolution failed")
			target := "second"
			if pluginFailure {
				target = "missing"
			}
			s.client.Client = &http.Client{Transport: deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/manifests/"+target) {
					return nil, sentinel
				}
				return http.DefaultTransport.RoundTrip(req)
			})}
			_, _, err := s.capture(ctx, config.Recipe{Include: inputs})
			if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "include[1]") || !strings.Contains(err.Error(), inputs[1].Ref) {
				t.Fatalf("lost selection/cause: %v", err)
			}
			if pluginFailure && (!strings.Contains(err.Error(), subject.Digest.String()) || !strings.Contains(err.Error(), inputs[1].Plugin.Ref)) {
				t.Fatalf("lost plugin subject: %v", err)
			}
		})
	}
}

func TestCaptureRootGraphFailureContext(t *testing.T) {
	r, repo, s := testRegistry(t)
	first := object(t, r.store, "first")
	second := object(t, r.store, "second")
	r.tags["first"], r.tags["second"] = first, second
	sentinel := errors.New("graph fetch failed")
	s.client.Client = &http.Client{Transport: deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/manifests/"+second.Digest.String()) {
			return nil, sentinel
		}
		return http.DefaultTransport.RoundTrip(req)
	})}
	_, _, err := s.capture(ctx, config.Recipe{Include: []config.Selection{dependencySelection(repo + ":first"), dependencySelection(repo + ":second")}})
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "include[1]") || !strings.Contains(err.Error(), repo+":second") || !strings.Contains(err.Error(), second.Digest.String()) {
		t.Fatalf("lost graph failure context: %v", err)
	}
}

func TestCaptureDependencyFailureContext(t *testing.T) {
	r, repo, s := testRegistry(t)
	subject := object(t, r.store, "subject")
	r.tags["chart"] = subject
	dependencyDeclaration(t, r, subject, "profile", dependency.Reference{Ref: repo + ":missing"})
	sentinel := errors.New("dependency resolve failed")
	s.client.Client = &http.Client{Transport: deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/manifests/missing") {
			return nil, sentinel
		}
		return http.DefaultTransport.RoundTrip(req)
	})}
	_, _, err := s.capture(ctx, config.Recipe{Include: []config.Selection{dependencySelection(repo + ":chart")}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("lost dependency cause: %v", err)
	}
	for _, part := range []string{"include[0]", repo + ":chart", subject.Digest.String(), repo + ":missing"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("missing %q: %v", part, err)
		}
	}
}

func TestCaptureDoesNotWaitForAnotherPublicationCallback(t *testing.T) {
	r, repo, s := testRegistry(t)
	subject := object(t, r.store, "subject")
	r.tags["chart"] = subject
	recipe := config.Recipe{Include: []config.Selection{dependencySelection(repo + ":chart")}}
	held, release, publicationDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		close(release)
		select {
		case <-publicationDone:
		case <-time.After(time.Second):
			t.Error("publication callback did not finish")
		}
	}()
	captureEvents := 0
	s.onProgress = func(p Progress) {
		if p.Operation == "attachment-published" {
			close(held)
			<-release
			return
		}
		captureEvents++
	}
	// Reproduce the lock ordering of Attach after it confirms publication.
	go func() {
		defer close(publicationDone)
		if err := s.acquirePublication(ctx); err != nil {
			return
		}
		defer s.releasePublication()
		s.progress(Progress{Operation: "attachment-published"})
	}()
	select {
	case <-held:
	case <-time.After(time.Second):
		t.Fatal("publication callback did not start")
	}
	dryDone := make(chan error, 1)
	go func() {
		result, err := s.Create(ctx, recipe, repo+":dry", CreateOptions{DryRun: true})
		if err == nil && (!result.DryRun || result.Published) {
			err = fmt.Errorf("unexpected dry-run result: %+v", result)
		}
		dryDone <- err
	}()
	select {
	case err := <-dryDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("read-only capture waited for publication callback")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	waitDone := make(chan error, 1)
	go func() { _, err := s.Create(waitCtx, recipe, repo+":waiting", CreateOptions{}); waitDone <- err }()
	select {
	case err := <-waitDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiting Create lost cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting Create could not cancel while publication callback blocked")
	}
	if captureEvents != 0 {
		t.Fatalf("capture invoked concurrent callback: %d events", captureEvents)
	}
}
