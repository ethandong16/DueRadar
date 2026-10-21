//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package instance

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func acquireFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open data directory lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrInUse
		}
		return nil, fmt.Errorf("lock data directory: %w", err)
	}
	return file, nil
}
