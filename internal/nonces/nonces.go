// Package nonces records accepted request nonces in the store, so every instance refuses a replay.
package nonces

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/auth"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// collection is the store collection that holds nonce records.
const collection = "nonces"

// idLength is how many bytes of a nonce's digest name its document.
const idLength = 16

// Store records nonces as documents that must not already exist.
type Store struct {
	// Store holds the records, shared by every instance.
	Store store.Store

	// Logger reports store failures; nil discards them.
	Logger *slog.Logger
}

// record is a stored nonce.
type record struct {
	// UsedAt is when the nonce was accepted, in UTC.
	UsedAt time.Time `json:"usedAt"`
}

// Use records a member's nonce and reports whether it was new. It refuses when the store cannot say,
// since accepting a replay is worse than asking a client to try again.
func (s *Store) Use(member, nonce string, now time.Time) bool {
	ctx := context.Background()
	digest := sha256.Sum256([]byte(member + ":" + nonce))
	id := base64.RawURLEncoding.EncodeToString(digest[:idLength])
	encoded, _ := json.Marshal(record{UsedAt: now.UTC()})

	_, err := s.Store.Put(ctx, collection, id, encoded, 0)
	if err == nil {
		return true
	}
	if !errors.Is(err, store.ErrConflict) {
		s.log(err)
		return false
	}
	existing, err := s.Store.Get(ctx, collection, id)
	if err != nil {
		s.log(err)
		return false
	}
	var previous record
	if json.Unmarshal(existing.Data, &previous) == nil && now.Sub(previous.UsedAt) > auth.NonceLifetime {
		_, err := s.Store.Put(ctx, collection, id, encoded, existing.Version)
		return err == nil
	}
	return false
}

// Sweep deletes nonces older than their lifetime.
func (s *Store) Sweep(ctx context.Context, now time.Time) error {
	items, err := s.Store.List(ctx, collection)
	if err != nil {
		return err
	}
	for _, item := range items {
		var previous record
		if json.Unmarshal(item.Data, &previous) != nil || now.Sub(previous.UsedAt) <= auth.NonceLifetime {
			continue
		}
		err := s.Store.Delete(ctx, collection, item.ID, item.Version)
		if err != nil && !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	return nil
}

// log reports a store failure when a logger is set.
func (s *Store) log(err error) {
	if s.Logger != nil {
		s.Logger.Error("recording a nonce failed", "error", err)
	}
}
