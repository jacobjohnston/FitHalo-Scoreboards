package store_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// walkModes returns the permission bits of every entry beneath root, keyed by relative path.
func walkModes(t *testing.T, root string) (dirs, files map[string]fs.FileMode) {
	t.Helper()
	dirs, files = map[string]fs.FileMode{}, map[string]fs.FileMode{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		if entry.IsDir() {
			dirs[relative] = info.Mode().Perm()
		} else {
			files[relative] = info.Mode().Perm()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dirs, files
}

// TestOpenDirectoryCreatesRootWithMode0700 checks a new root is private to the owner.
func TestOpenDirectoryCreatesRootWithMode0700(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	directory, err := store.OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("root is %v, want a directory with mode 0700", info.Mode())
	}
}

// TestOpenDirectoryNestedMissingParents records whether missing parents are created.
func TestOpenDirectoryNestedMissingParents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a", "b", "c")
	directory, err := store.OpenDirectory(root)
	// PROBE: "creating if needed" does not say whether parent directories are created too.
	t.Logf("OpenDirectory with two missing parents: %v", err)
	if err == nil {
		directory.Close()
		if info, statErr := os.Stat(root); statErr != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("nested root: %v, %v", info, statErr)
		}
	}
}

// TestDirectoryAndFileModes checks every collection folder is 0700 and every document file 0600.
func TestDirectoryAndFileModes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	directory, err := store.OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	for _, b := range []store.Store{directory, store.Compress(directory)} {
		for _, collection := range []string{"weeks", "weeks/2026-10-19", "routes/member/2026-10-19"} {
			for _, id := range []string{"a", "b"} {
				mustPut(t, b, collection, id, []byte(`{"n":1}`), store.AnyVersion)
			}
		}
	}
	dirs, files := walkModes(t, root)
	for path, mode := range dirs {
		if mode != 0o700 {
			t.Errorf("directory %q has mode %o, want 0700", path, mode)
		}
	}
	if len(files) == 0 {
		t.Fatal("no files were written")
	}
	for path, mode := range files {
		if mode != 0o600 {
			t.Errorf("file %q has mode %o, want 0600", path, mode)
		}
	}
}

// TestOpenDirectoryTightensExistingRoot records whether a pre-existing, group-readable root is tightened to 0700.
func TestOpenDirectoryTightensExistingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	directory, err := store.OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	mustPut(t, directory, "weeks", "w", []byte("x"), 0)
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	// PROBE: the review says directories are 0700; an operator-created root (a mounted volume) stays as made.
	t.Logf("pre-existing root mode after OpenDirectory: %o", info.Mode().Perm())
	_, files := walkModes(t, root)
	for path, mode := range files {
		if mode != 0o600 {
			t.Errorf("file %q beneath a loose root has mode %o", path, mode)
		}
	}
}

// TestFileModesSurviveOverwrite checks a rewritten document keeps mode 0600.
func TestFileModesSurviveOverwrite(t *testing.T) {
	root := t.TempDir()
	directory, err := store.OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	var version int64
	for i := 0; i < 20; i++ {
		version = mustPut(t, directory, "weeks", "w", []byte(fmt.Sprint(i)), version)
	}
	_, files := walkModes(t, root)
	for path, mode := range files {
		if mode != 0o600 {
			t.Errorf("file %q has mode %o after rewrites, want 0600", path, mode)
		}
	}
}

// TestNoTemporaryFilesLeftBehind checks many writes and deletes leave exactly the live documents on disk.
func TestNoTemporaryFilesLeftBehind(t *testing.T) {
	root := t.TempDir()
	directory, err := store.OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("d%02d", i)
		var version int64
		for j := 0; j < 10; j++ {
			version = mustPut(t, directory, "weeks", id, []byte(fmt.Sprintf("%d/%d", i, j)), version)
		}
		if i%2 == 1 {
			if err := directory.Delete(ctx, "weeks", id, version); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < 5; i++ {
		if _, err := directory.Put(ctx, "weeks", "fixed", []byte("x"), 0); err == nil && i > 0 {
			t.Fatal("create succeeded twice")
		}
	}
	live := len(mustList(t, directory, "weeks"))
	entries, err := os.ReadDir(filepath.Join(root, "weeks"))
	if err != nil {
		t.Fatalf("reading the collection folder: %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(entries) != live {
		t.Fatalf("collection folder holds %d entries for %d live documents: %q", len(entries), live, names)
	}
	for _, name := range names {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "tmp") || strings.HasSuffix(lower, "~") || strings.HasPrefix(lower, ".") {
			t.Errorf("suspicious entry %q in the collection folder", name)
		}
	}
	rootEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range rootEntries {
		t.Logf("root entry %q dir=%v", entry.Name(), entry.IsDir())
	}
}

// TestSecondOpenIsRefusedWhileLocked checks the flock keeps a second process-equivalent out until Close.
func TestSecondOpenIsRefusedWhileLocked(t *testing.T) {
	root := t.TempDir()
	first, err := store.OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := store.OpenDirectory(root); err == nil {
		second.Close()
		first.Close()
		t.Fatal("a second OpenDirectory succeeded while the first was open")
	}
	mustPut(t, first, "weeks", "w", []byte("x"), 0)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := store.OpenDirectory(root)
	if err != nil {
		t.Fatalf("OpenDirectory after Close: %v", err)
	}
	defer second.Close()
	if document := mustGet(t, second, "weeks", "w"); string(document.Data) != "x" || document.Version != 1 {
		t.Fatalf("reopened store reads %q at v%d", document.Data, document.Version)
	}
}

// TestCompressedCloseReleasesLock checks closing the wrapper unlocks the directory beneath it.
func TestCompressedCloseReleasesLock(t *testing.T) {
	root := t.TempDir()
	directory, err := store.OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := store.Compress(directory)
	mustPut(t, wrapper, "weeks", "w", []byte("x"), 0)
	if err := wrapper.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := store.OpenDirectory(root)
	if err != nil {
		t.Fatalf("OpenDirectory after Compressed.Close: %v", err)
	}
	second.Close()
}

// TestFailedOpenDoesNotHoldLock checks a refused second open leaves the first owner unaffected.
func TestFailedOpenDoesNotHoldLock(t *testing.T) {
	root := t.TempDir()
	first, err := store.OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if second, err := store.OpenDirectory(root); err == nil {
			second.Close()
			t.Fatalf("attempt %d: second open succeeded", i)
		}
	}
	mustPut(t, first, "weeks", "w", []byte("x"), 0)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := store.OpenDirectory(root)
	if err != nil {
		t.Fatalf("open after failed attempts and Close: %v", err)
	}
	third.Close()
}

// TestOpenDirectoryOnFileFails checks a root that is a regular file is refused.
func TestOpenDirectoryOnFileFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	directory, err := store.OpenDirectory(root)
	if err == nil {
		directory.Close()
		t.Fatal("OpenDirectory on a regular file succeeded")
	}
	if data, _ := os.ReadFile(root); string(data) != "not a directory" {
		t.Fatalf("the file was altered: %q", data)
	}
}

// TestOpenDirectoryOnUnwritableParentFails checks a root that cannot be created is a clean error.
func TestOpenDirectoryOnUnwritableParentFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores permission bits")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o700) })
	directory, err := store.OpenDirectory(filepath.Join(parent, "data"))
	if err == nil {
		directory.Close()
		t.Fatal("OpenDirectory under an unwritable parent succeeded")
	}
}

// TestReopenPreservesVersions checks versions persist across close and reopen, not just in memory.
func TestReopenPreservesVersions(t *testing.T) {
	root := t.TempDir()
	first, err := store.OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, first, "weeks", "w", []byte("one"), 0)
	mustPut(t, first, "weeks", "w", []byte("two"), 1)
	mustPut(t, first, "weeks", "w", []byte("three"), 2)
	mustPut(t, first, "weeks/2026", "n", []byte("nested"), 0)
	first.Close()
	second, err := store.OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if document := mustGet(t, second, "weeks", "w"); string(document.Data) != "three" || document.Version != 3 {
		t.Fatalf("reopened: %q at v%d", document.Data, document.Version)
	}
	if _, err := second.Put(context.Background(), "weeks", "w", []byte("stale"), 2); err == nil {
		t.Fatal("stale write accepted after reopen")
	}
	if got := mustPut(t, second, "weeks", "w", []byte("four"), 3); got != 4 {
		t.Fatalf("version after reopen %d, want 4", got)
	}
	if got := mustCollections(t, second, "weeks/"); len(got) != 1 || got[0] != "weeks/2026" {
		t.Fatalf("Collections after reopen = %q", got)
	}
}

// TestDocumentFileIsNotWorldReadableAfterRename checks the renamed file, not only the temp file, is 0600.
func TestDocumentFileIsNotWorldReadableAfterRename(t *testing.T) {
	root := t.TempDir()
	directory, err := store.OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	mustPut(t, directory, "secrets", "s", []byte("private"), 0)
	_, files := walkModes(t, filepath.Join(root, "secrets"))
	if len(files) != 1 {
		t.Fatalf("secrets folder holds %d files", len(files))
	}
	for path, mode := range files {
		if mode&0o077 != 0 {
			t.Fatalf("%q is readable by group or others: %o", path, mode)
		}
	}
}
