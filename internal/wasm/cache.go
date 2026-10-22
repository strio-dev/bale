package wasm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"

	"github.com/wago-org/wago"
)

// Native cache entries are executable host code, not portable plugin artifacts.
const cacheFileLimit = (1 << 30) + (256 << 20) + (64 << 10)

// Bind native artifacts to the actual linked engine, WASI provider, toolchain
// and build flags without maintaining a second dependency-version list. Hashing
// once also distinguishes test binaries and builds with local module replaces.
var hostBuildDigest = sync.OnceValues(func() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "linux" {
		// Open the running image, including after atomic executable replacement.
		path = "/proc/self/exe"
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
})

func openModule(path string) (*os.File, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	stat, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	if !stat.Mode().IsRegular() || stat.Size() < 0 || stat.Size() > ModuleLimit {
		_ = file.Close()
		return nil, 0, errors.New("plugin module must be a regular file at most 128 MiB")
	}
	return file, stat.Size(), nil
}

func exactEOF(file *os.File) error {
	var extra [1]byte
	if n, err := file.Read(extra[:]); n != 0 || err != io.EOF {
		return errors.New("plugin module changed while reading")
	}
	return nil
}

func moduleHash(file *os.File, size int64) (string, error) {
	hash := sha256.New()
	if _, err := io.CopyN(hash, file, size); err != nil {
		return "", err
	}
	if err := exactEOF(file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func compilerKey(source string, cfg *wago.RuntimeConfig) (string, error) {
	build, err := hostBuildDigest()
	if err != nil {
		return "", fmt.Errorf("identify host build for trusted plugin cache: %w", err)
	}
	data, err := json.Marshal(struct {
		Source, Build, Toolchain, OS, Arch, Config string
		Optimizations                              any
		Schema                                     int
	}{source, build, runtime.Version(), runtime.GOOS, runtime.GOARCH,
		cfg.String(), cfg.OptimizationInfos(), 1})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func compileModule(ctx context.Context, runtime *wago.Runtime, path, directory string, cfg *wago.RuntimeConfig) (*wago.Module, error) {
	file, size, err := openModule(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var cache *trustedCache
	var key, source string
	if directory != "" {
		source, err = moduleHash(file, size)
		if err != nil {
			return nil, err
		}
		key, err = compilerKey(source, cfg)
		if err != nil {
			return nil, err
		}
		cache, err = openTrustedCache(directory)
		if err != nil {
			return nil, err
		}
		defer cache.Close()
		compiled, loadErr := cache.load(key)
		if loadErr == nil {
			return runtime.AdoptModule(compiled)
		}
		if !errors.Is(loadErr, os.ErrNotExist) {
			return nil, fmt.Errorf("trusted plugin cache: %w", loadErr)
		}
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data := make([]byte, int(size))
	if _, err = io.ReadFull(file, data); err != nil {
		return nil, err
	}
	if err = exactEOF(file); err != nil {
		return nil, err
	}
	if source != "" {
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != source {
			return nil, errors.New("plugin module changed between hashing and compilation")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Compile has no cancellation API. Keep it synchronous and hold the permit.
	module, err := runtime.Compile(data)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, errors.Join(err, module.Close())
	}
	if cache != nil {
		if err = cache.save(key, module.Compiled()); err != nil {
			return nil, errors.Join(fmt.Errorf("save trusted plugin cache: %w", err), module.Close())
		}
	}
	return module, nil
}

func readCompiled(file *os.File) (*wago.Compiled, error) {
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm() != 0600 || stat.Size() < 0 || stat.Size() > cacheFileLimit {
		return nil, errors.New("trusted plugin cache entry must be a regular file with mode 0600 and bounded size")
	}
	compiled := new(wago.Compiled)
	count, err := compiled.ReadFromWithLimits(io.LimitReader(file, stat.Size()), wago.DefaultArtifactLimits())
	if err != nil {
		_ = compiled.Close()
		return nil, err
	}
	if count != stat.Size() {
		_ = compiled.Close()
		return nil, errors.New("trailing plugin cache bytes")
	}
	return compiled, nil
}

type limitedWriter struct {
	io.Writer
	remaining int64
}

func (writer *limitedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > writer.remaining {
		return 0, errors.New("compiled plugin cache exceeds size limit")
	}
	count, err := writer.Writer.Write(data)
	writer.remaining -= int64(count)
	return count, err
}
