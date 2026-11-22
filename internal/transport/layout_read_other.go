//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package transport

import (
	"fmt"
	"os"
)

func openLayoutDirectory(string) (*os.File, error) {
	return nil, fmt.Errorf("%w: descriptor-relative OCI layout reads are unsupported on this platform", ErrUnsupported)
}

func openLayoutMemberRoot(string, *os.File) (*layoutMemberRoot, error) { return nil, nil }

func openLayoutMemberAt(*os.File, string, bool) (*os.File, error) {
	return nil, ErrUnsupported
}
