//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package localstore

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func lock(ctx context.Context, path string, exclusive bool) (*os.File, error) {
	fp, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = fp.Close()
			return nil, err
		}
		err = unix.Flock(int(fp.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			return fp, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = fp.Close()
			return nil, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = fp.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
