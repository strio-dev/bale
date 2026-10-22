package transport

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func publishLayoutDirectory(temp, target string) error {
	// RENAME_EXCL fails atomically if any destination entry already exists.
	if err := unix.RenamexNp(temp, target, unix.RENAME_EXCL); err != nil {
		if errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOTEMPTY) {
			return fmt.Errorf("%w: layout path already exists: %s", ErrConflict, target)
		}
		return err
	}
	return nil
}
