//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package main

import (
	"fmt"
	"os"
)

func duplicateCommandPipe(*os.File) (*os.File, func(), func() error, error) {
	return nil, nil, nil, fmt.Errorf("interruptible command pipes are unsupported on this platform")
}
