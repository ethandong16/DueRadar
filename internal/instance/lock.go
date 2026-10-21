// Package instance reserves a data directory for one running DueRadar process.
package instance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var ErrInUse = errors.New("data directory is already in use by another DueRadar process")

// Lock is held by an open OS handle, so process exit also releases ownership.
// The marker must never be removed while a process holds it: removing it would
// allow a new process to lock a different inode for the same data directory.
type Lock struct {
	file *os.File
	once sync.Once
	err  error
}

// Acquire must run before opening or updating state, keys, or credentials.
func Acquire(directory string) (*Lock, error) {
	if directory == "" {
		return nil, errors.New("data directory is empty")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	file, err := acquireFile(filepath.Join(directory, ".dueradar.lock"))
	if err != nil {
		return nil, err
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() error {
	l.once.Do(func() { l.err = l.file.Close() })
	return l.err
}
