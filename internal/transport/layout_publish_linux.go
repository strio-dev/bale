package transport

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func publishLayoutDirectory(temp, target string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, temp, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOTEMPTY) {
			return fmt.Errorf("%w: layout path already exists: %s", ErrConflict, target)
		}
		return err
	}
	return nil
}
