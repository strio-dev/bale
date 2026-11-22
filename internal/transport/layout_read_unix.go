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

func openLayoutMemberAt(parent *os.File, name string, directory bool) (*os.File, error) {
	// Pass a single component to the kernel. O_NOFOLLOW here rejects links
	// rather than requesting os.Root's in-root symlink resolution.
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
