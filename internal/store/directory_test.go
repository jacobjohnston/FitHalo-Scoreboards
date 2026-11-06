package store

import "testing"

// openTestDirectory opens a directory store in a temporary folder, closed when the test ends.
func openTestDirectory(t *testing.T) Store {
	t.Helper()
	directory, err := OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { directory.Close() })
	return directory
}

// TestDirectory holds the directory backend to the store's contract.
func TestDirectory(t *testing.T) {
	runContract(t, openTestDirectory)
}

// TestOneServerPerDirectory checks a second server cannot open a directory in use.
func TestOneServerPerDirectory(t *testing.T) {
	root := t.TempDir()
	first, err := OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := OpenDirectory(root); err == nil {
		second.Close()
		t.Fatal("a second server opened the same directory")
	}
}
