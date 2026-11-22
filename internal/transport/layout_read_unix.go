//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package transport

import (
	"os"

	"golang.org/x/sys/unix"
)

func openLayoutFiles(path string) (layoutFiles, error) {
	// Require a directory at open time without cleaning the selected path.
	// Leave an empty path unchanged so it does not become "/.".
	if path != "" {
		path += "/."
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return layoutFiles{}, err
	}
	return layoutFiles{root: root}, nil
}

func (f layoutFiles) openLayoutFile(name string) (*os.File, error) {
	// Don't wait on a FIFO before the caller can validate the opened file.
	return f.root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
}
