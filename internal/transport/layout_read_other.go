//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package transport

import (
	"fmt"
	"os"
)

func openLayoutFiles(string) (layoutFiles, error) {
	return layoutFiles{}, fmt.Errorf("%w: descriptor-relative OCI layout reads are unsupported on this platform", ErrUnsupported)
}

func (layoutFiles) openLayoutFile(string) (*os.File, error) {
	return nil, ErrUnsupported
}
