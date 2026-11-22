//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package transport

import (
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

func openLayoutDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// openLayoutMemberRoot resolves relative OCI header and blob links below the
// same directory used for strict capture marker reads. The literal "/."
// requires a directory without cleaning the selected path or changing aliases.
func openLayoutMemberRoot(path string, dir *os.File) (_ *layoutMemberRoot, retErr error) {
	root, err := os.OpenRoot(path + "/.")
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = root.Close()
		}
	}()
	directory, err := dir.Stat()
	if err != nil {
		return nil, err
	}
	anchored, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	if !os.SameFile(directory, anchored) {
		return nil, fmt.Errorf("%w: layout directory changed while opening", ErrIntegrity)
	}
	return &layoutMemberRoot{root: root, flags: unix.O_RDONLY | unix.O_NONBLOCK}, nil
}

func openLayoutMemberAt(parent *os.File, name string, directory bool) (*os.File, error) {
	// Pass a single component to the kernel. O_NOFOLLOW keeps capture metadata strict.
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(int(parent.Fd()), name, flags, 0)
	runtime.KeepAlive(parent)
	if err != nil {
		// Kernels disagree on the errno for O_NOFOLLOW. DragonFly uses
		// EINVAL; NetBSD uses EFTYPE (not defined on every Unix).
		if errno, ok := err.(unix.Errno); ok {
			switch unix.ErrnoName(errno) {
			case "ELOOP", "ENOTDIR", "EMLINK", "EFTYPE":
				return nil, fmt.Errorf("%w: symlink or invalid directory component: %w", ErrIntegrity, err)
			case "EINVAL":
				if runtime.GOOS == "dragonfly" {
					return nil, fmt.Errorf("%w: symlink or invalid directory component: %w", ErrIntegrity, err)
				}
			}
		}
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
