// Package helmcli implements the Bale Helm command adapter.
package helmcli

import (
	"fmt"
	"strings"

	"github.com/distribution/reference"
	relocate "github.com/strio-dev/bale/plugins/helm-relocate"
)

type invocation struct {
	args     []string
	chart    string
	options  relocate.Options
	registry relocate.RegistryOptions
}

func parse(args []string) (invocation, error) {
	var p invocation
	seen := map[string]bool{}
	selectors := map[string]*string{"bundle": &p.options.Bundle, "source": &p.options.Source, "capture": &p.options.Capture, "declaration": &p.options.Declaration, "context": &p.options.Context}
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		name, value, has := strings.Cut(strings.TrimPrefix(args[0], "--"), "=")
		target, ok := selectors[name]
		if !ok {
			return p, fmt.Errorf("unknown Bale option %s; put Helm flags after RELEASE OCI_CHART", args[0])
		}
		if seen[name] {
			return p, fmt.Errorf("duplicate --%s", name)
		}
		seen[name] = true
		args = args[1:]
		if !has {
			if len(args) == 0 {
				return p, fmt.Errorf("--%s requires a value", name)
			}
			value = args[0]
			args = args[1:]
		}
		if value == "" {
			return p, fmt.Errorf("--%s requires a value", name)
		}
		*target = value
	}
	if len(args) < 3 {
		return p, fmt.Errorf("expected install|upgrade|template RELEASE OCI_CHART [Helm flags]")
	}
	switch args[0] {
	case "install", "upgrade", "template":
	default:
		return p, fmt.Errorf("unsupported action %q; use Helm directly for other actions", args[0])
	}
	if strings.HasPrefix(args[1], "-") || args[1] == "" {
		return p, fmt.Errorf("RELEASE must precede OCI_CHART and Helm flags")
	}
	if !strings.HasPrefix(args[2], "oci://") {
		return p, fmt.Errorf("chart must be an oci:// reference")
	}
	p.args = append([]string(nil), args[:3]...)
	var version string
	relevant := map[string]*string{"version": &version, "ca-file": &p.registry.CAFile, "cert-file": &p.registry.CertFile, "key-file": &p.registry.KeyFile, "username": &p.registry.Username, "password": &p.registry.Password}
	booleans := map[string]*bool{"plain-http": new(bool), "insecure-skip-tls-verify": &p.registry.InsecureSkipVerify}
	for i := 3; i < len(args); i++ {
		arg := args[i]
		name, value, has := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if arg == "--" {
			return p, fmt.Errorf("positional arguments after OCI_CHART are unsupported")
		}
		if name == "post-renderer" || name == "post-renderer-args" {
			return p, fmt.Errorf("bale requires the post-renderer slot; remove --%s", name)
		}
		target, ok := relevant[name]
		if !strings.HasPrefix(arg, "--") {
			ok = false
		}
		if ok {
			if seen[name] {
				return p, fmt.Errorf("duplicate --%s", name)
			}
			seen[name] = true
			original := []string{arg}
			if !has {
				i++
				if i == len(args) {
					return p, fmt.Errorf("--%s requires a value", name)
				}
				value = args[i]
				original = append(original, value)
			}
			if value == "" {
				return p, fmt.Errorf("--%s requires a value", name)
			}
			*target = value
			if name != "version" {
				p.args = append(p.args, original...)
			}
			continue
		}
		if target, ok := booleans[name]; ok && strings.HasPrefix(arg, "--") {
			if seen[name] {
				return p, fmt.Errorf("duplicate --%s", name)
			}
			seen[name] = true
			if has && value != "true" && value != "false" {
				return p, fmt.Errorf("--%s requires true or false", name)
			}
			*target = !has || value == "true"
		}
		p.args = append(p.args, arg)
	}
	ref := strings.TrimPrefix(args[2], "oci://")
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return p, fmt.Errorf("chart reference: %w", err)
	}
	_, tagged := named.(reference.Tagged)
	_, digested := named.(reference.Digested)
	if version != "" {
		if tagged || digested {
			return p, fmt.Errorf("--version conflicts with the chart tag or digest")
		}
		if strings.ContainsAny(version, "*<>=~^ ,|\t\n") {
			return p, fmt.Errorf("--version must be an exact tag, not a version range")
		}
		// Helm encodes SemVer build metadata in OCI tags using underscores.
		named, err = reference.WithTag(named, strings.ReplaceAll(version, "+", "_"))
		if err != nil {
			return p, fmt.Errorf("--version must be an exact tag: %w", err)
		}
	} else if !tagged && !digested {
		return p, fmt.Errorf("chart requires an explicit tag, digest, or exact --version")
	}
	p.chart = named.String()
	p.registry.CredentialHost = reference.Domain(named)
	if *booleans["plain-http"] {
		p.registry.PlainHTTPRegistries = []string{p.registry.CredentialHost}
	}
	return p, nil
}
