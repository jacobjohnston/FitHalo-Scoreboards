package nonces

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/auth"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// TestSharedAcrossInstances proves a nonce used at one instance is refused at another sharing its store.
func TestSharedAcrossInstances(t *testing.T) {
	directory, err := store.OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	first, second := &Store{Store: directory}, &Store{Store: directory}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if !first.Use("member", "nonce", now) {
		t.Fatal("a new nonce was refused")
	}
	if second.Use("member", "nonce", now.Add(time.Minute)) {
		t.Fatal("a nonce was accepted again by another instance")
	}
	if !second.Use("other", "nonce", now) {
		t.Fatal("one member's nonce blocked another's")
	}
	later := now.Add(auth.NonceLifetime + time.Second)
	if err := first.Sweep(context.Background(), later); err != nil {
		t.Fatal(err)
	}
	if items, _ := directory.List(context.Background(), collection); len(items) != 0 {
		t.Fatalf("the sweep left %d old nonces", len(items))
	}
}

// failingStore is a store whose writes fail with putError and whose reads fail with getError.
type failingStore struct {
	// Store supplies the methods Use never calls, and panics if one is.
	store.Store

	// putError is what every Put returns.
	putError error

	// getError is what every Get returns.
	getError error
}

// Put fails with putError.
func (f *failingStore) Put(context.Context, string, string, []byte, int64) (int64, error) {
	return 0, f.putError
}

// Get fails with getError.
func (f *failingStore) Get(context.Context, string, string) (store.Document, error) {
	return store.Document{}, f.getError
}

// TestFailsClosedOnStoreErrors proves a nonce is refused when the store cannot record or read it.
func TestFailsClosedOnStoreErrors(t *testing.T) {
	unavailable := errors.New("store unavailable")
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for name, backend := range map[string]*failingStore{
		"put fails":            {putError: unavailable},
		"conflict, get fails":  {putError: store.ErrConflict, getError: unavailable},
		"conflict, get misses": {putError: store.ErrConflict, getError: store.ErrNotFound},
	} {
		if (&Store{Store: backend}).Use("member", "nonce", now) {
			t.Errorf("%s: Use accepted the nonce", name)
		}
	}
}
