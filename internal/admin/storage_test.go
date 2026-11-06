package admin

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/fixture"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// storageFixtures holds the admin's documents exactly as the 1.0 server stores them; see docs/specifications.md.
const storageFixtures = "testdata/storage-1"

// fixtureSessionToken is the session token whose tokenKey session.json is stored under.
const fixtureSessionToken = "c2Vzc2lvbi10b2tlbi1maXh0dXJl"

// fixtureChallengeID is the challenge identifier challenge.json is stored under.
const fixtureChallengeID = "Y2hhbGxlbmdlLWZpeHR1cmU"

// readFixture returns one of storageFixtures.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	return fixture.Read(t, filepath.Join(storageFixtures, name))
}

// TestStorageFixturesDecode decodes every 1.0 admin document with no field left unknown, and encodes it
// back with every field it had.
func TestStorageFixturesDecode(t *testing.T) {
	documents := map[string]any{
		"account.json":   &Account{},
		"session.json":   &Session{},
		"challenge.json": &challenge{},
	}
	for name, value := range documents {
		t.Run(name, func(t *testing.T) {
			fixture.ExpectRoundTrip(t, readFixture(t, name), value)
		})
	}
}

// TestStorageFixturesServe stores the 1.0 admin documents where 1.0 keeps them, and signs in, resumes
// the session and takes the challenge with them.
func TestStorageFixturesServe(t *testing.T) {
	ctx := context.Background()
	directory, err := store.OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { directory.Close() })
	backend := store.Compress(directory)
	documents := []struct {
		// collection and id are where 1.0 keeps the document.
		collection, id string

		// name is the document's fixture.
		name string
	}{
		{accountCollection, accountID, "account.json"},
		{sessionsCollection, tokenKey(fixtureSessionToken), "session.json"},
		{challengeCollection, fixtureChallengeID, "challenge.json"},
	}
	for _, document := range documents {
		if _, err := backend.Put(ctx, document.collection, document.id, readFixture(t, document.name), 0); err != nil {
			t.Fatal(err)
		}
	}
	administrator := &Admin{Store: backend, Now: func() time.Time { return time.Date(2026, 10, 21, 8, 1, 0, 0, time.UTC) }}

	if setUp, err := administrator.IsSetUp(ctx); err != nil || !setUp {
		t.Fatalf("set up: %v, %v", setUp, err)
	}
	account, err := administrator.Account(ctx)
	if err != nil || len(account.Passkeys) != 1 || account.Passkeys[0].SignCount != 7 {
		t.Fatalf("account: %+v, %v", account, err)
	}
	if err := administrator.SignIn(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("signing in: %v", err)
	}
	if session, err := administrator.Session(ctx, fixtureSessionToken); err != nil || session.CSRF != "Y3NyZi1maXh0dXJlLTEyMzQ" {
		t.Fatalf("session: %+v, %v", session, err)
	}
	value, err := administrator.TakeChallenge(ctx, fixtureChallengeID, "signIn")
	if err != nil || !bytes.Equal(value, []byte("challenge-fixture-32-bytes-long!")) {
		t.Fatalf("challenge: %q, %v", value, err)
	}
}
