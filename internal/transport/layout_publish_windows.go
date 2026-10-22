package transport

import (
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func publishLayoutDirectory(temp, target string) error {
	from, err := windows.UTF16PtrFromString(filepath.Clean(temp))
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(filepath.Clean(target))
	if err != nil {
		return err
	}
	// Sibling directories share a volume. Omitting REPLACE_EXISTING and
	// COPY_ALLOWED prevents replacement and any copy-and-delete fallback.
	if err := windows.MoveFileEx(from, to, 0); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return fmt.Errorf("%w: layout path already exists: %s", ErrConflict, target)
		}
		return err
	}
	return nil
}
