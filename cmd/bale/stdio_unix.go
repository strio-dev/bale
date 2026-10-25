//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func duplicateCommandPipe(file *os.File) (*os.File, func(), func() error, error) {
	raw, err := file.SyscallConn()
	if err != nil {
		return nil, nil, nil, err
	}
	var flags, fd int
	var setupErr error
	if err := raw.Control(func(original uintptr) {
		flags, setupErr = unix.FcntlInt(original, unix.F_GETFL, 0)
		if setupErr == nil {
			fd, setupErr = unix.FcntlInt(original, unix.F_DUPFD_CLOEXEC, 0)
		}
	}); err != nil {
		return nil, nil, nil, err
	}
	if setupErr != nil {
		return nil, nil, nil, setupErr
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		return nil, nil, nil, errors.Join(err, unix.Close(fd))
	}
	owned := os.NewFile(uintptr(fd), file.Name())
	cleanup := func() error {
		// dup shares the open-file description. Restore its flags before closing
		// our descriptor so callers retain their original blocking behavior.
		_, restoreErr := unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags)
		return errors.Join(restoreErr, owned.Close())
	}
	if err := owned.SetDeadline(time.Time{}); err != nil {
		return nil, nil, nil, errors.Join(err, cleanup())
	}
	return owned, func() { _ = owned.SetDeadline(time.Now()) }, cleanup, nil
}
