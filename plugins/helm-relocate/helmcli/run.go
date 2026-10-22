package helmcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	relocate "github.com/strio-dev/bale/plugins/helm-relocate"
)

// Streams are passed directly to the installed Helm process.
type Streams struct {
	In       io.Reader
	Out, Err io.Writer
}

const Help = `Bale Helm integration

Usage:
  helm bale [Bale selectors] install|upgrade|template RELEASE OCI_CHART [Helm flags]

The chart must have an explicit tag, digest, or exact --version.
Bale selects its destination attachment and pins the chart before invoking Helm.

Bale selectors (before the action):
  --bundle REFERENCE       Select a delivered bundle repository@digest
  --source REFERENCE       Select an original chart repository@digest
  --capture DIGEST         Select a capture
  --declaration DIGEST     Select a dependency declaration
  --context CONTEXT        Select a declaration context

Helm flags follow OCI_CHART. Existing --post-renderer flags are unsupported.
Other actions, including rollback and uninstall, use ordinary Helm directly.
`

type resolver func(context.Context, invocation) (relocate.Selection, error)
type executor func(context.Context, string, []string, []string, Streams) error

// Run resolves the delivered chart and invokes Helm with the private post-renderer.
func Run(ctx context.Context, args []string, streams Streams) error {
	return run(ctx, args, streams, os.Environ(), func(ctx context.Context, p invocation) (relocate.Selection, error) {
		client, err := relocate.NewRegistryClient(p.registry)
		if err != nil {
			return relocate.Selection{}, err
		}
		return relocate.Resolve(ctx, client, p.chart, p.options)
	}, execute)
}

func run(ctx context.Context, args []string, streams Streams, env []string, resolve resolver, exec executor) error {
	if len(args) == 0 || len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		_, err := io.WriteString(streams.Out, Help)
		return err
	}
	p, err := parse(args)
	if err != nil {
		return err
	}
	p.registry.RegistryConfig = envValue(env, "HELM_REGISTRY_CONFIG")
	binary := envValue(env, "HELM_BIN")
	if binary == "" {
		return fmt.Errorf("HELM_BIN is missing; run through the installed Helm plugin")
	}
	pluginDir := envValue(env, "HELM_PLUGIN_DIR")
	if pluginDir == "" {
		return fmt.Errorf("HELM_PLUGIN_DIR is missing; run through the installed Helm plugin")
	}
	pluginDir, err = filepath.Abs(pluginDir)
	if err != nil {
		return err
	}
	privateDir := filepath.Join(pluginDir, "postrenderers")
	if _, err = os.Stat(filepath.Join(privateDir, "bale-postrenderer", "plugin.yaml")); err != nil {
		return fmt.Errorf("private post-renderer: %w", err)
	}
	binary, err = helmExecutable(binary, env)
	if err != nil {
		return err
	}
	selection, err := resolve(ctx, p)
	if err != nil {
		return fmt.Errorf("resolve chart destinations: %w", err)
	}
	raw, err := selection.MarshalRecords()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "bale-helm-records-*.json")
	if err != nil {
		return fmt.Errorf("create records file: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	p.args[2] = "oci://" + selection.Chart
	postArgs := []string{"--chart", selection.Chart, "--records-file", f.Name(), "--bundle", selection.Record.Bundle, "--source", selection.Record.Source, "--capture", selection.Resolution.Capture.String()}
	if selection.Resolution.Declaration != nil {
		postArgs = append(postArgs, "--declaration", selection.Resolution.Declaration.Digest.String())
		if selection.Resolution.Declaration.Context != "" {
			postArgs = append(postArgs, "--context", selection.Resolution.Declaration.Context)
		}
	}
	p.args = append(p.args, "--post-renderer", "bale-postrenderer")
	for _, arg := range postArgs {
		p.args = append(p.args, "--post-renderer-args="+arg)
	}
	paths := privateDir
	if inherited := envValue(env, "HELM_PLUGINS"); inherited != "" {
		paths += string(os.PathListSeparator) + inherited
	}
	env = setEnv(env, "HELM_PLUGINS", paths)
	return exec(ctx, binary, p.args, env, streams)
}

func envValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], key+"="); ok {
			return v
		}
	}
	return ""
}
func setEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, key+"=") {
			out = append(out, entry)
		}
	}
	return append(out, key+"="+value)
}
func helmExecutable(binary string, env []string) (string, error) {
	if strings.ContainsRune(binary, os.PathSeparator) {
		p, err := filepath.Abs(binary)
		if err != nil {
			return "", err
		}
		binary = p
	} else {
		found := ""
		for _, dir := range filepath.SplitList(envValue(env, "PATH")) {
			candidate := filepath.Join(dir, binary)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
				found = candidate
				break
			}
		}
		if found == "" {
			return "", fmt.Errorf("cannot find HELM_BIN %q", binary)
		}
		var err error
		binary, err = filepath.Abs(found)
		if err != nil {
			return "", err
		}
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	actual, err := os.Stat(binary)
	if err != nil {
		return "", fmt.Errorf("HELM_BIN: %w", err)
	}
	me, err := os.Stat(self)
	if err != nil {
		return "", err
	}
	if os.SameFile(actual, me) {
		return "", errors.New("HELM_BIN points to the Bale adapter itself")
	}
	return binary, nil
}
