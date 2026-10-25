package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func duplicateCommandPipe(file *os.File) (*os.File, func(), func() error, error) {
	var duplicate windows.Handle
	process := windows.CurrentProcess()
	if err := windows.DuplicateHandle(process, windows.Handle(file.Fd()), process, &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, nil, nil, err
	}
	owned := os.NewFile(uintptr(duplicate), file.Name())
	closePipe := func() error {
		err := owned.Close()
		if errors.Is(err, os.ErrClosed) {
			return nil
		}
		return err
	}
	// Go's Windows pipe Close uses CancelIoEx to interrupt pending pipe I/O.
	return owned, func() { _ = closePipe() }, closePipe, nil
}
