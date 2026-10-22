package flux

import (
	"strings"
	"testing"
)

func TestConfigStrict(t *testing.T) {
	good := "releases:\n- name: web\n  chart: registry.example/chart:v1\n  selection:\n    context: production\n"
	c, err := LoadConfig(strings.NewReader(good))
	if err != nil || len(c.Releases) != 1 {
		t.Fatalf("%+v %v", c, err)
	}
	for _, raw := range []string{"", "{}", "releases: []", "releases: null", good + "unknown: true\n", strings.Replace(good, "name: web", "name: web\n  name: other", 1), strings.Replace(good, "context: production", "context: null", 1), strings.Replace(good, "context: production", "typo: production", 1), good + "---\n{}", strings.Replace(good, "name: web", "name: Invalid", 1), strings.Replace(good, "registry.example/chart:v1", "registry.example/chart", 1), "releases:\n- &x {name: web, chart: registry.example/chart:v1}\n- *x\n"} {
		if _, err := LoadConfig(strings.NewReader(raw)); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestEmptyAndInvalidSelectors(t *testing.T) {
	for _, selection := range []string{`context: ""`, `context: " padded "`, `bundle: registry.example/bundle:tag`, `source: short@sha256:bad`, `capture: bad`, `declaration: bad`, `context: true`} {
		raw := "releases:\n- name: web\n  chart: registry.example/chart:v1\n  selection:\n    " + selection + "\n"
		if _, err := LoadConfig(strings.NewReader(raw)); err == nil {
			t.Errorf("accepted %s", selection)
		}
	}
}
