//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package localstore

import (
	"context"
	"fmt"
	"os"
)

func lock(context.Context, string, bool) (*os.File, error) {
	return nil, fmt.Errorf("managed storage requires supported advisory locks")
}
