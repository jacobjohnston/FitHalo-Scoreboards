// Package store keeps a board's data as collections of small documents behind a four-call interface,
// so the same server runs on a directory, DynamoDB or Cosmos DB.
package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"
)

// conflictRetriesBeforeBackoff is how many conflicts Update retries at once before it starts waiting.
const conflictRetriesBeforeBackoff = 3

// conflictBackoff is the longest wait between Update's later retries, jittered down to half.
const conflictBackoff = 10 * time.Millisecond

// AnyVersion overwrites or deletes a document whatever its version.
const AnyVersion int64 = -1

// ErrNotFound means the document does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrConflict means the document's version is not the one the write expected.
var ErrConflict = errors.New("store: version conflict")

// Document is a stored value and the version it was read at.
type Document struct {
	// Data is the stored bytes, nil when the document does not exist.
	Data []byte

	// Version counts the document's writes, zero when it does not exist.
	Version int64
}

// Item is a document and its identifier, as listed from a collection.
type Item struct {
	// ID is the document's identifier within its collection.
	ID string

	// Document is the item's data and version.
	Document
}

// Store is the interface every backend implements.
//
// Collections are slash-separated paths such as "weeks/2026-10-19"; identifiers are single segments.
// Versions start at 1. A put with expectedVersion 0 creates a document that must not exist yet.
type Store interface {
	// Get returns a document, or ErrNotFound.
	Get(ctx context.Context, collection, id string) (Document, error)

	// Put writes a document if its version is still expectedVersion, and returns the new version.
	Put(ctx context.Context, collection, id string, data []byte, expectedVersion int64) (int64, error)

	// Delete removes a document if its version is still expectedVersion. A missing document is not an
	// error when expectedVersion is AnyVersion.
	Delete(ctx context.Context, collection, id string, expectedVersion int64) error

	// List returns every document in a collection, in identifier order.
	List(ctx context.Context, collection string) ([]Item, error)

	// Collections returns the collections directly beneath prefix, which ends in a slash.
	Collections(ctx context.Context, prefix string) ([]string, error)

	// DeleteCollection removes a collection, every document in it, and every collection beneath it.
	DeleteCollection(ctx context.Context, collection string) error

	// Close releases the store.
	Close() error
}

// segmentPattern matches one collection segment or identifier.
var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// reservedPrefix begins the backends' internal partitions, so no collection's first segment may.
const reservedPrefix = "_"

// ValidateCollection rejects a collection path that is empty, has a segment outside [A-Za-z0-9_-], or
// begins with reservedPrefix.
func ValidateCollection(collection string) error {
	if strings.HasPrefix(collection, reservedPrefix) {
		return fmt.Errorf("store: reserved collection %q", collection)
	}
	for _, segment := range strings.Split(collection, "/") {
		if !segmentPattern.MatchString(segment) {
			return fmt.Errorf("store: invalid collection %q", collection)
		}
	}
	return nil
}

// ValidateID rejects an identifier outside [A-Za-z0-9_-].
func ValidateID(id string) error {
	if !segmentPattern.MatchString(id) {
		return fmt.Errorf("store: invalid identifier %q", id)
	}
	return nil
}

// ValidatePrefix rejects a prefix that is not a valid collection followed by a slash.
func ValidatePrefix(prefix string) error {
	if !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("store: prefix %q does not end in a slash", prefix)
	}
	return ValidateCollection(strings.TrimSuffix(prefix, "/"))
}

// Update reads a document, applies change, and writes it back, retrying when another writer got there
// first. A missing document reaches change as nil data and is created.
func Update(ctx context.Context, store Store, collection, id string, change func(data []byte) ([]byte, error)) error {
	for attempt := 0; ; attempt++ {
		document, err := store.Get(ctx, collection, id)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		updated, err := change(document.Data)
		if err != nil {
			return err
		}
		_, err = store.Put(ctx, collection, id, updated, document.Version)
		if !errors.Is(err, ErrConflict) {
			return err
		}
		// Another writer got there first, so it made progress; wait a little before reading again.
		if attempt >= conflictRetriesBeforeBackoff {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(rand.IntN(int(conflictBackoff))) + conflictBackoff/2):
			}
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}
