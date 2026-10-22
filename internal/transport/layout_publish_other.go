//go:build !linux && !darwin && !windows

package transport

import "fmt"

func publishLayoutDirectory(_, _ string) error {
	return fmt.Errorf("%w: atomic layout publication is unsupported on this platform", ErrUnsupported)
}
