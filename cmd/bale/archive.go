package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

func runArchive(ctx context.Context, command string, opts commandOptions, out, errOut io.Writer) int {
	if len(opts.args) != 1 {
		return fail(errOut, fmt.Errorf("invalid arguments\nSee 'bale %s --help'", command))
	}
	if err := validateArchiveStream(command, opts, out); err != nil {
		return fail(errOut, err)
	}
	formatter, err := newOutputFormatter(opts.format, false)
	if err != nil {
		return fail(errOut, err)
	}
	options := bale.ClientOptions{}
	if command == "save" {
		options.PlainHTTPRegistries = opts.plainHTTP
		options.Credentials = lazyDockerCredentials(opts.authFile)
	}
	client, err := bale.NewClient(options)
	if err != nil {
		return fail(errOut, err)
	}
	var result bale.ArchiveResult
	if command == "save" {
		if archiveStream(opts.archiveOutput) {
			result, err = client.SaveTo(ctx, opts.args[0], out)
		} else {
			result, err = client.Save(ctx, opts.args[0], opts.archiveOutput)
		}
	} else {
		if archiveStream(opts.archiveInput) {
			result, err = client.LoadFrom(ctx, os.Stdin, opts.args[0])
		} else {
			result, err = client.Load(ctx, opts.archiveInput, opts.args[0])
		}
	}
	if err != nil {
		return fail(errOut, err)
	}
	if command == "save" && archiveStream(opts.archiveOutput) {
		return 0
	}
	if err := formatter.writeResult(out, result, result.Descriptor.Digest.String()); err != nil {
		return fail(errOut, err)
	}
	return 0
}

func archiveStream(path string) bool { return path == "" || path == "-" }

func validateArchiveStream(command string, opts commandOptions, out io.Writer) error {
	if command == "save" && archiveStream(opts.archiveOutput) {
		if opts.formatSet {
			return fmt.Errorf("--format requires --output FILE; stdout is reserved for archive bytes")
		}
		if streamIsTerminal(out) {
			return fmt.Errorf("refusing to write an archive to a terminal; use --output FILE or redirect stdout")
		}
	}
	if command == "load" && archiveStream(opts.archiveInput) && streamIsTerminal(os.Stdin) {
		return fmt.Errorf("cannot load an archive from a terminal; use --input FILE or redirect stdin")
	}
	return nil
}

// A complete local bundle stays independent of ambient Docker configuration.
// Only saving a metadata capture can require an authenticated source request.
func lazyDockerCredentials(authFile string) auth.CredentialFunc {
	initialize := sync.OnceValues(func() (auth.CredentialFunc, error) {
		path := authFile
		if path == "" {
			path = os.Getenv("REGISTRY_AUTH_FILE")
		}
		var store *credentials.DynamicStore
		var err error
		if path == "" {
			store, err = credentials.NewStoreFromDocker(credentials.StoreOptions{})
		} else {
			if _, err := os.Stat(path); err != nil {
				return nil, fmt.Errorf("read authentication file: %w", err)
			}
			store, err = credentials.NewStore(path, credentials.StoreOptions{})
		}
		if err != nil {
			return nil, err
		}
		return credentials.Credential(store), nil
	})
	return func(ctx context.Context, host string) (auth.Credential, error) {
		lookup, err := initialize()
		if err != nil {
			return auth.Credential{}, err
		}
		return lookup(ctx, host)
	}
}
