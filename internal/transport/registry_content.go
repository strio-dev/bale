package transport

import (
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
)

// Go method results are invariant, so accept both Bale's minimal blob interface
// and existing ORAS repositories without widening the caller-owned contract.
func registryBlobs(value any) (RegistryBlobStore, bool) {
	switch repository := value.(type) {
	case interface{ Blobs() RegistryBlobStore }:
		return repository.Blobs(), true
	case interface{ Blobs() registry.BlobStore }:
		return repository.Blobs(), true
	default:
		return nil, false
	}
}

func registryMount(value any) (registry.Mounter, registry.Reference, bool) {
	switch repository := value.(type) {
	case *registryRepository:
		if repository.remote != nil {
			return repository.remote, repository.Reference, true
		}
		mounter, ok := repository.backend.(registry.Mounter)
		return mounter, repository.Reference, ok
	case *remote.Repository:
		return repository, repository.Reference, true
	default:
		return nil, registry.Reference{}, false
	}
}
