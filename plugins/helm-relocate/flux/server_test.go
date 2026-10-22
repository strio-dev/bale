package flux

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	bale "github.com/strio-dev/bale"
)

type inspectorFunc func(context.Context, string) ([]bale.InspectedDestination, error)

func (f inspectorFunc) InspectDestinations(ctx context.Context, ref string) ([]bale.InspectedDestination, error) {
	return f(ctx, ref)
}
func records() []bale.InspectedDestination {
	chart := digest.FromString("chart")
	image := digest.FromString("image")
	return []bale.InspectedDestination{{Repository: "mirror.example/charts/web", Subject: v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: chart, Size: 100}, Descriptor: v1.Descriptor{MediaType: v1.MediaTypeImageManifest, ArtifactType: "application/vnd.bale.destination.v1+json", Digest: digest.FromString("attachment"), Size: 100}, Local: true, Record: bale.DestinationRecord{SchemaVersion: 1, Bundle: "mirror.example/bundle@" + digest.FromString("bundle").String(), Source: "source.example/charts/web@" + chart.String(), Destination: "mirror.example/charts/web@" + chart.String(), Resolutions: []bale.DestinationResolution{{Capture: digest.FromString("capture"), Outcome: "selected", Policy: bale.DependencyPolicy{Follow: "auto"}, Declaration: &bale.DestinationDeclaration{Digest: digest.FromString("declaration"), Context: "production"}, Dependencies: []bale.DestinationDependency{{Ref: "docker.io/library/nginx:latest", Policy: bale.DependencyPolicy{Follow: "auto"}, Source: "docker.io/library/nginx@" + image.String(), Destination: "mirror.example/nginx@" + image.String()}}}}}}}
}
func config() Config {
	return Config{Releases: []Release{{Name: "web", Chart: "mirror.example/charts/web:v1"}}}
}

func TestHealthDoesNotRequireTokenOrContactRegistry(t *testing.T) {
	h, err := NewHandler(config(), inspectorFunc(func(context.Context, string) ([]bale.InspectedDestination, error) {
		t.Fatal("health probe contacted registry")
		return nil, nil
	}), HandlerOptions{BearerToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK || w.Body.String() != "ok\n" {
		t.Fatalf("health check failed: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/inputs", "/inputs/web", "/healthz?chart=other.example/chart:v1"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("health route bypassed authentication for %s: %d", path, w.Code)
		}
	}
	for _, token := range []string{"", "Bearer secret"} {
		r := httptest.NewRequest(http.MethodPost, "/healthz", nil)
		r.Header.Set("Authorization", token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := http.StatusUnauthorized
		if token != "" {
			want = http.StatusMethodNotAllowed
		}
		if w.Code != want {
			t.Fatalf("non-GET health probe: %d", w.Code)
		}
	}
}
func TestInputs(t *testing.T) {
	if _, err := resolve(context.Background(), inspectorFunc(func(context.Context, string) ([]bale.InspectedDestination, error) { return records(), nil }), config().Releases[0]); err != nil {
		t.Fatal(err)
	}
	calls := 0
	h, err := NewHandler(config(), inspectorFunc(func(_ context.Context, ref string) ([]bale.InspectedDestination, error) {
		calls++
		if ref != "mirror.example/charts/web:v1" {
			t.Errorf("unexpected ref %s", ref)
		}
		return records(), nil
	}), HandlerOptions{BearerToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/inputs", "/inputs/web"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var out Response
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Inputs) != 1 || out.Inputs[0].ID != "web" || out.Inputs[0].ChartRepository != "oci://mirror.example/charts/web" || out.Inputs[0].ChartDigest != digest.FromString("chart").String() || len(out.Inputs[0].PostRenderers[0].Kustomize.Images) == 0 {
			t.Fatalf("%+v", out)
		}
	}
	for _, tc := range []struct {
		method, path, token string
		status              int
	}{{"GET", "/inputs/web", "", 401}, {"GET", "/inputs/web", "Bearer wrong", 401}, {"GET", "/inputs/missing", "Bearer secret", 404}, {"GET", "/inputs/web/extra", "Bearer secret", 404}, {"POST", "/inputs/web", "Bearer secret", 405}, {"GET", "/inputs/web?chart=evil.example/chart:v1", "Bearer secret", 400}} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("Authorization", tc.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s %s: %d", tc.method, tc.path, w.Code)
		}
	}
	if calls != 2 {
		t.Fatalf("rejected requests reached registry: %d", calls)
	}
}
func TestFailureIsNotEmptySuccess(t *testing.T) {
	for _, kind := range []string{"missing", "ambiguous", "registry", "timeout", "second"} {
		t.Run(kind, func(t *testing.T) {
			c := config()
			if kind == "second" {
				c.Releases = append(c.Releases, Release{Name: "broken", Chart: "mirror.example/charts/broken:v1"})
			}
			h, err := NewHandler(c, inspectorFunc(func(ctx context.Context, ref string) ([]bale.InspectedDestination, error) {
				switch kind {
				case "registry":
					return nil, errors.New("password=topsecret")
				case "missing":
					return nil, nil
				case "ambiguous":
					r := records()
					other := records()[0]
					other.Descriptor.Digest = digest.FromString("second")
					return append(r, other), nil
				case "timeout":
					<-ctx.Done()
					return nil, ctx.Err()
				case "second":
					if strings.Contains(ref, "broken") {
						return nil, errors.New("broken")
					}
				}
				return records(), nil
			}), HandlerOptions{Timeout: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "/inputs", nil))
			if w.Code < 400 || strings.Contains(w.Body.String(), "inputs") || strings.Contains(w.Body.String(), "topsecret") {
				t.Fatalf("unsafe failure: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
func TestSourceNameStable(t *testing.T) {
	a := SourceName("web", "mirror.example/chart@digest")
	if a != SourceName("web", "mirror.example/chart@digest") || len(a) > 63 || !namePattern.MatchString(a) {
		t.Fatal(a)
	}
	for _, other := range []string{SourceName("other", "mirror.example/chart@digest"), SourceName("web", "mirror.example/other@digest"), SourceName("web", "mirror.example/chart@other")} {
		if a == other {
			t.Fatal("source identity collapsed release, repository or digest")
		}
	}
}

func TestResolutionDiagnosticStaysServerSide(t *testing.T) {
	problem := errors.New("registry diagnostic with private details")
	var reported string
	var diagnostic error
	h, err := NewHandler(config(), inspectorFunc(func(context.Context, string) ([]bale.InspectedDestination, error) {
		return nil, problem
	}), HandlerOptions{OnError: func(release string, err error) {
		reported, diagnostic = release, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/inputs/web", nil))
	if reported != "web" || !errors.Is(diagnostic, problem) {
		t.Fatalf("missing diagnostic: %q %v", reported, diagnostic)
	}
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "private details") {
		t.Fatalf("unsafe HTTP diagnostic: %d %s", w.Code, w.Body.String())
	}
}

func TestRequestConcurrencyBound(t *testing.T) {
	started := make(chan struct{}, 16)
	release := make(chan struct{})
	h, err := NewHandler(config(), inspectorFunc(func(ctx context.Context, _ string) ([]bale.InspectedDestination, error) {
		started <- struct{}{}
		select {
		case <-release:
			return records(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}), HandlerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 16)
	for range 16 {
		go func() {
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/inputs/web", nil))
			done <- struct{}{}
		}()
	}
	for range 16 {
		<-started
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/inputs/web", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Code)
	}
	close(release)
	for range 16 {
		<-done
	}
}
func TestOversizedResultFailsWithoutPartialInputs(t *testing.T) {
	r := records()
	base := r[0].Record.Resolutions[0].Dependencies[0]
	r[0].Record.Resolutions[0].Dependencies = nil
	for i := range 1800 {
		name := strings.Repeat("a", 150) + strings.Repeat("b", i%20) + digest.FromString(string(rune(i))).Encoded()[:12]
		dep := base
		dep.Ref = "source.example/" + name + ":v1"
		dep.Source = "source.example/" + name + "@" + digest.FromString("image").String()
		dep.Destination = "mirror.example/" + name + "@" + digest.FromString("image").String()
		r[0].Record.Resolutions[0].Dependencies = append(r[0].Record.Resolutions[0].Dependencies, dep)
	}
	h, err := NewHandler(config(), inspectorFunc(func(context.Context, string) ([]bale.InspectedDestination, error) { return r, nil }), HandlerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/inputs/web", nil))
	if w.Code < 400 || w.Body.Len() > MaxResponseBytes || !strings.Contains(w.Body.String(), "exceeds limit") || strings.Contains(w.Body.String(), `"inputs"`) {
		t.Fatal(w.Code, w.Body.Len())
	}
}
