package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Directory keeps each collection as a folder and each document as a JSON file, written atomically.
// One process owns a directory: OpenDirectory locks it, and a mutex serialises every access.
type Directory struct {
	// root is the folder holding every collection.
	root string

	// lock is the open lock file, or nil where locking is unavailable.
	lock *os.File

	// mutex serialises every read and write.
	mutex sync.Mutex
}

// documentFile is a document as it sits on disk. JSON documents are kept as JSON so the files stay
// readable; anything else, such as an avatar, is kept as base64.
type documentFile struct {
	// Version is the document's version.
	Version int64 `json:"version"`

	// JSON is the data when it is valid JSON.
	JSON json.RawMessage `json:"json,omitempty"`

	// Base64 is the data in standard base64 when it is not JSON.
	Base64 string `json:"base64,omitempty"`
}

// documentExtension ends every document's file name.
const documentExtension = ".json"

// OpenDirectory opens, creating if needed, a directory store at root.
func OpenDirectory(root string) (*Directory, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("store: creating %s: %w", root, err)
	}
	lock, err := lockDirectory(root)
	if err != nil {
		return nil, err
	}
	return &Directory{root: root, lock: lock}, nil
}

// Close releases the directory's lock.
func (d *Directory) Close() error {
	if d.lock == nil {
		return nil
	}
	return d.lock.Close()
}

// Get returns a document, or ErrNotFound.
func (d *Directory) Get(_ context.Context, collection, id string) (Document, error) {
	if err := validateKey(collection, id); err != nil {
		return Document{}, err
	}
	d.mutex.Lock()
	defer d.mutex.Unlock()
	return d.read(d.documentPath(collection, id))
}

// Put writes a document if its version is still expectedVersion.
func (d *Directory) Put(_ context.Context, collection, id string, data []byte, expectedVersion int64) (int64, error) {
	if err := validateKey(collection, id); err != nil {
		return 0, err
	}
	d.mutex.Lock()
	defer d.mutex.Unlock()

	path := d.documentPath(collection, id)
	current, err := d.read(path)
	switch {
	case errors.Is(err, ErrNotFound):
		if expectedVersion != 0 && expectedVersion != AnyVersion {
			return 0, ErrConflict
		}
	case err != nil:
		return 0, err
	case expectedVersion != AnyVersion && expectedVersion != current.Version:
		return 0, ErrConflict
	}

	version := current.Version + 1
	var encoded []byte
	if json.Valid(data) && len(bytes.TrimSpace(data)) == len(data) {
		// The bytes go in as they are, so nothing compacts or escapes them.
		encoded = append(append([]byte(fmt.Sprintf(`{"version":%d,"json":`, version)), data...), '}')
	} else {
		var err error
		if encoded, err = json.Marshal(documentFile{Version: version, Base64: base64.StdEncoding.EncodeToString(data)}); err != nil {
			return 0, err
		}
	}
	if err := writeAtomically(path, encoded); err != nil {
		return 0, err
	}
	return version, nil
}

// Delete removes a document if its version is still expectedVersion.
func (d *Directory) Delete(_ context.Context, collection, id string, expectedVersion int64) error {
	if err := validateKey(collection, id); err != nil {
		return err
	}
	d.mutex.Lock()
	defer d.mutex.Unlock()

	path := d.documentPath(collection, id)
	current, err := d.read(path)
	if errors.Is(err, ErrNotFound) {
		if expectedVersion == AnyVersion {
			return nil
		}
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if expectedVersion != AnyVersion && expectedVersion != current.Version {
		return ErrConflict
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

// List returns every document in a collection, in identifier order.
func (d *Directory) List(_ context.Context, collection string) ([]Item, error) {
	if err := ValidateCollection(collection); err != nil {
		return nil, err
	}
	d.mutex.Lock()
	defer d.mutex.Unlock()

	entries, err := os.ReadDir(d.collectionPath(collection))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var items []Item
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, documentExtension) || strings.HasPrefix(name, ".") {
			continue
		}
		id := strings.TrimSuffix(name, documentExtension)
		document, err := d.read(filepath.Join(d.collectionPath(collection), name))
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		items = append(items, Item{ID: id, Document: document})
	}
	sort.Slice(items, func(first, second int) bool { return items[first].ID < items[second].ID })
	return items, nil
}

// Collections returns the collections directly beneath prefix.
func (d *Directory) Collections(_ context.Context, prefix string) ([]string, error) {
	if err := ValidatePrefix(prefix); err != nil {
		return nil, err
	}
	d.mutex.Lock()
	defer d.mutex.Unlock()

	entries, err := os.ReadDir(d.collectionPath(strings.TrimSuffix(prefix, "/")))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var collections []string
	for _, entry := range entries {
		if entry.IsDir() && segmentPattern.MatchString(entry.Name()) {
			collections = append(collections, prefix+entry.Name())
		}
	}
	sort.Strings(collections)
	return collections, nil
}

// DeleteCollection removes a collection and everything beneath it.
func (d *Directory) DeleteCollection(_ context.Context, collection string) error {
	if err := ValidateCollection(collection); err != nil {
		return err
	}
	d.mutex.Lock()
	defer d.mutex.Unlock()

	path := d.collectionPath(collection)
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

// collectionPath returns the folder that holds collection.
func (d *Directory) collectionPath(collection string) string {
	return filepath.Join(append([]string{d.root}, strings.Split(collection, "/")...)...)
}

// documentPath returns the file that holds a document.
func (d *Directory) documentPath(collection, id string) string {
	return filepath.Join(d.collectionPath(collection), id+documentExtension)
}

// read decodes the document file at path, or returns ErrNotFound; the caller holds the mutex.
func (d *Directory) read(path string) (Document, error) {
	encoded, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, err
	}
	var file documentFile
	if err := json.Unmarshal(encoded, &file); err != nil {
		return Document{}, fmt.Errorf("store: reading %s: %w", path, err)
	}
	if file.Base64 != "" {
		data, err := base64.StdEncoding.DecodeString(file.Base64)
		if err != nil {
			return Document{}, fmt.Errorf("store: reading %s: %w", path, err)
		}
		return Document{Data: data, Version: file.Version}, nil
	}
	return Document{Data: file.JSON, Version: file.Version}, nil
}

// validateKey rejects an invalid collection or identifier.
func validateKey(collection, id string) error {
	if err := ValidateCollection(collection); err != nil {
		return err
	}
	return ValidateID(id)
}

// writeAtomically writes to a temporary file beside path, syncs it and renames it over path, so a power
// cut leaves either the old document or the new one.
func writeAtomically(path string, data []byte) error {
	folder := filepath.Dir(path)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return err
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	temporaryPath := filepath.Join(folder, ".tmp-"+base64.RawURLEncoding.EncodeToString(suffix))
	file, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		os.Remove(temporaryPath)
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		os.Remove(temporaryPath)
		return err
	}
	return syncDirectory(folder)
}

// syncDirectory flushes a folder's entries to disk, ignoring a folder that no longer exists.
func syncDirectory(path string) error {
	folder, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer folder.Close()
	return folder.Sync()
}
