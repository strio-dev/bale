//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package wasm

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"

	"github.com/wago-org/wago"
	"golang.org/x/sys/unix"
)

// Holding a directory descriptor prevents parent renames or symlink replacement
// from redirecting reads and publication to a different cache directory.
type trustedCache struct{ directory *os.File }

func openTrustedCache(path string) (*trustedCache, error) {
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	stat, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	var owner unix.Stat_t
	if err = unix.Fstat(fd, &owner); err != nil {
		_ = file.Close()
		return nil, err
	}
	if !stat.IsDir() || stat.Mode().Perm() != 0700 || owner.Uid != uint32(os.Geteuid()) {
		_ = file.Close()
		return nil, errors.New("trusted plugin cache must be an owned directory with mode 0700")
	}
	return &trustedCache{directory: file}, nil
}

func (cache *trustedCache) Close() { _ = cache.directory.Close() }

func (cache *trustedCache) load(key string) (*wago.Compiled, error) {
	fd, err := unix.Openat(int(cache.directory.Fd()), key+".native", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), key)
	defer func() { _ = file.Close() }()
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("trusted plugin cache entry has a different owner")
	}
	return readCompiled(file)
}

func (cache *trustedCache) save(key string, compiled *wago.Compiled) error {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	name := ".bale-" + hex.EncodeToString(random[:])
	directory := int(cache.directory.Fd())
	fd, err := unix.Openat(directory, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() { _ = file.Close(); _ = unix.Unlinkat(directory, name, 0) }()
	if _, err = compiled.WriteTo(&limitedWriter{Writer: file, remaining: cacheFileLimit}); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = unix.Renameat(directory, name, directory, key+".native"); err != nil {
		return err
	}
	return cache.directory.Sync()
}
