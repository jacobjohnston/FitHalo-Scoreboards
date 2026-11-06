//go:build unix

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockDirectory takes an exclusive lock on root, so a second server pointed at the same directory fails
// to start rather than corrupting it.
func lockDirectory(root string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: opening lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("store: %s is in use by another server: %w", root, err)
	}
	return file, nil
}
