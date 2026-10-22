package flux

import (
	"regexp"
	"strings"
	"testing"
)

func TestExactImagePatterns(t *testing.T) {
	a := "sha256:" + strings.Repeat("a", 64)
	b := "sha256:" + strings.Repeat("b", 64)
	mapping := map[string]string{"docker.io/library/nginx:latest": "mirror.example/nginx@" + a, "registry.example/app:v1": "mirror.example/app@" + a, "registry.example/app:v2": "mirror.example/app@" + b, "docker.io/library/nginx@" + a: "mirror.example/nginx@" + a}
	images, err := Images(mapping)
	if err != nil {
		t.Fatal(err)
	}
	// Match the additional suffix used by Kustomize image.IsImageMatched.
	matches := func(value string) []string {
		var out []string
		for _, image := range images {
			if regexp.MustCompile(image.Name + "(:[a-zA-Z0-9_.-]*)?(@sha256:[a-zA-Z0-9_.-]*)?").MatchString(value) {
				out = append(out, image.NewName+"@"+image.Digest)
			}
		}
		return out
	}
	for _, tc := range []struct{ value, target string }{
		{"nginx", "mirror.example/nginx@" + a}, {"library/nginx", "mirror.example/nginx@" + a}, {"docker.io/nginx:latest", "mirror.example/nginx@" + a}, {"index.docker.io/library/nginx:latest", "mirror.example/nginx@" + a},
		{"nginx:ignored@" + a, "mirror.example/nginx@" + a}, {"registry.example/app:v1", "mirror.example/app@" + a}, {"registry.example/app:v2", "mirror.example/app@" + b},
	} {
		got := matches(tc.value)
		if len(got) != 1 || got[0] != tc.target {
			t.Errorf("%s: %v", tc.value, got)
		}
	}
	for _, value := range []string{"registryXexample/app:v1", "registry.example/app:v3", "registry.example/app:v1@" + b, "nginx@" + b, "unknown.example/app:v1", "prefixregistry.example/app:v1", "nginx:latest-junk"} {
		if got := matches(value); len(got) != 0 {
			t.Errorf("unknown %s unexpectedly matched %v", value, got)
		}
	}
}
func TestDeterministicImagesAndSourceNames(t *testing.T) {
	if SourceName("web", "a@x") == SourceName("web", "b@x") {
		t.Fatal("source identity omitted repository")
	}
	if SourceName("web", "a@x") == SourceName("web", "a@y") {
		t.Fatal("source identity omitted digest")
	}
	if _, err := Images(map[string]string{"nginx": "target.example/app:latest"}); err == nil {
		t.Fatal("accepted unpinned target")
	}
}

func TestDockerHubNamespaceDoesNotAliasRegistry(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	for _, namespace := range []string{"localhost", "example.com", "example:5000"} {
		images, err := Images(map[string]string{"docker.io/" + namespace + "/app:v1": "mirror.example/app@" + d})
		if namespace == "example:5000" { // A colon is not a valid Docker Hub namespace.
			if err == nil {
				t.Fatal("accepted invalid namespace")
			}
			continue
		}
		if err != nil || len(images) != 1 {
			t.Fatalf("%s: %v %v", namespace, images, err)
		}
		pattern := regexp.MustCompile(images[0].Name)
		if !pattern.MatchString("docker.io/"+namespace+"/app:v1") || pattern.MatchString(namespace+"/app:v1") {
			t.Fatalf("namespace became registry alias: %s", pattern)
		}
	}
}
