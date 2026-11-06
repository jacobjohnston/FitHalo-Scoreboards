//go:build !unix

package store

import "os"

// lockDirectory does nothing where flock is unavailable; only one server may use a directory.
func lockDirectory(string) (*os.File, error) {
	return nil, nil
}
