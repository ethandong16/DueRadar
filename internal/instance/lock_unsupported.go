//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package instance

import (
	"errors"
	"os"
)

func acquireFile(string) (*os.File, error) {
	return nil, errors.New("data directory locking is not supported on this operating system")
}
