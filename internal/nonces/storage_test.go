package nonces

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/fixture"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// nonceFixture is a nonce record exactly as the 1.0 server stores it; see docs/specifications.md.
const nonceFixture = "testdata/storage-1/nonce.json"

// TestStorageFixture decodes the 1.0 record with no field unknown, and refuses the nonce it records as a
// replay, keyed as 1.0 keys it.
func TestStorageFixture(t *testing.T) {
	data := fixture.Read(t, nonceFixture)
	fixture.ExpectRoundTrip(t, data, &record{})

	ctx := context.Background()
	directory, err := store.OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { directory.Close() })
	backend := store.Compress(directory)
	digest := sha256.Sum256([]byte("WJlWhMsgEjcbPz3g3zLheA:bm9uY2UtZml4dHVyZS0x"))
	if _, err := backend.Put(ctx, "nonces", base64.RawURLEncoding.EncodeToString(digest[:16]), data, 0); err != nil {
		t.Fatal(err)
	}
	recorder := &Store{Store: backend}
	if recorder.Use("WJlWhMsgEjcbPz3g3zLheA", "bm9uY2UtZml4dHVyZS0x", time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)) {
		t.Error("the stored nonce was accepted again")
	}
}
