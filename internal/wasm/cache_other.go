//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package wasm

import (
	"errors"
	"github.com/wago-org/wago"
)

type trustedCache struct{}

func openTrustedCache(string) (*trustedCache, error) {
	return nil, errors.New("trusted plugin disk cache is unavailable on this platform; leave CacheDirectory empty")
}
func (*trustedCache) Close() {}
func (*trustedCache) load(string) (*wago.Compiled, error) {
	return nil, errors.New("plugin cache unavailable")
}
func (*trustedCache) save(string, *wago.Compiled) error {
	return errors.New("plugin cache unavailable")
}
