package helmcli

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	p, err := parse([]string{"--context", "production", "upgrade", "release", "oci://registry.example/charts/app", "--install", "--version=1.2.3", "--namespace", "test", "--plain-http", "--set", "x=a b", "--ca-file", "ca.pem"})
	if err != nil {
		t.Fatal(err)
	}
	if p.chart != "registry.example/charts/app:1.2.3" || p.options.Context != "production" || p.registry.CAFile != "ca.pem" || !reflect.DeepEqual(p.registry.PlainHTTPRegistries, []string{"registry.example"}) {
		t.Fatalf("%+v", p)
	}
	want := []string{"upgrade", "release", "oci://registry.example/charts/app", "--install", "--namespace", "test", "--plain-http", "--set", "x=a b", "--ca-file", "ca.pem"}
	if !reflect.DeepEqual(p.args, want) {
		t.Fatalf("args %q", p.args)
	}
}
func TestParseRejectsAmbiguousInputs(t *testing.T) {
	for _, suffix := range []string{"", "--version ^1.2", "--version 1.*", "--version >=1.0", "--version 1.0 --version 2.0", "--version", "--plain-http=yes", "--plain-http --plain-http", "--post-renderer other", "--post-renderer-args=x", "--ca-file --debug"} {
		t.Run(suffix, func(t *testing.T) {
			args := []string{"install", "release", "oci://registry.example/app"}
			if suffix != "" {
				args = append(args, strings.Fields(suffix)...)
			}
			if _, err := parse(args); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	for _, args := range [][]string{{"install", "release", "oci://registry.example/app:1", "--version", "1"}, {"rollback", "release", "oci://registry.example/app:1"}, {"install", "--debug", "release", "oci://registry.example/app:1"}, {"--context", "a", "--context", "b", "install", "release", "oci://registry.example/app:1"}} {
		if _, err := parse(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}

func TestParseExactVersionBuildMetadata(t *testing.T) {
	for _, flag := range [][]string{{"--version", "1.2.3+build.1"}, {"--version=1.2.3-rc.1+build.1"}} {
		args := append([]string{"template", "release", "oci://registry.example:5000/charts/app", "-f", "values.yaml"}, flag...)
		p, err := parse(args)
		if err != nil {
			t.Fatal(err)
		}
		version := "1.2.3_build.1"
		if len(flag) == 1 {
			version = "1.2.3-rc.1_build.1"
		}
		if p.chart != "registry.example:5000/charts/app:"+version {
			t.Fatalf("chart = %q", p.chart)
		}
		if !reflect.DeepEqual(p.args, args[:5]) {
			t.Fatalf("Helm argv = %q", p.args)
		}
	}
	if _, err := parse([]string{"template", "release", "oci://registry.example/app", "--version", "^1.2.3+build.1"}); err == nil {
		t.Fatal("accepted a version range")
	}
}

func TestParseCredentialBeginningWithDash(t *testing.T) {
	for _, flag := range [][]string{{"--password=--secret"}, {"--password", "--secret"}} {
		args := append([]string{"template", "release", "oci://registry.example/app:1"}, flag...)
		p, err := parse(args)
		if err != nil {
			t.Fatal(err)
		}
		if p.registry.Password != "--secret" || !reflect.DeepEqual(p.args, args) {
			t.Fatal("credential or Helm arguments changed")
		}
	}
}
