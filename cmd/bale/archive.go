package main

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

func runArchive(ctx context.Context, command string, opts commandOptions, out, errOut io.Writer) int {
	if len(opts.args) != 1 || (command == "save" && opts.archiveOutput == "") || (command == "load" && opts.archiveInput == "") {
		return fail(errOut, fmt.Errorf("invalid arguments\nSee 'bale %s --help'", command))
	}
	formatter, err := newOutputFormatter(opts.format, false)
	if err != nil {
		return fail(errOut, err)
	}
	options := bale.ClientOptions{}
	if command == "save" {
		options.PlainHTTPRegistries = opts.plainHTTP
		options.Credentials = lazyDockerCredentials()
	}
	client, err := bale.NewClient(options)
	if err != nil {
		return fail(errOut, err)
	}
	var result bale.ArchiveResult
	if command == "save" {
		result, err = client.Save(ctx, opts.args[0], opts.archiveOutput)
	} else {
		result, err = client.Load(ctx, opts.archiveInput, opts.args[0])
	}
	if err != nil {
		return fail(errOut, err)
	}
	if err := formatter.writeResult(out, result, result.Descriptor.Digest.String()); err != nil {
		return fail(errOut, err)
	}
	return 0
}

// A complete local bundle stays independent of ambient Docker configuration.
// Only saving a metadata capture can require an authenticated source request.
func lazyDockerCredentials() auth.CredentialFunc {
	initialize := sync.OnceValues(func() (auth.CredentialFunc, error) {
		store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
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
