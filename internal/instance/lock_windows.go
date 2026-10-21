package instance

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

const errorSharingViolation syscall.Errno = 32 // Win32 ERROR_SHARING_VIOLATION

func acquireFile(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("invalid data directory lock path: %w", err)
	}
	// Other processes may inspect the marker, but cannot open it for writing
	// or delete it until this handle closes. OPEN_ALWAYS preserves the inode.
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, syscall.FILE_SHARE_READ, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errorSharingViolation) {
			return nil, ErrInUse
		}
		return nil, fmt.Errorf("lock data directory: %w", err)
	}
	return os.NewFile(uintptr(handle), path), nil
}
