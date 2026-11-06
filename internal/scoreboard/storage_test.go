package scoreboard

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/fixture"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// storageFixtures holds documents exactly as the 1.0 server stores them. They must never change, since
// every later server has to read them; see docs/specifications.md.
const storageFixtures = "testdata/storage-1"

// The fixtures' identifiers, as their documents are keyed in the store.
const (
	// fixtureMemberID is the member in member.json, derived from its public key.
	fixtureMemberID = "WJlWhMsgEjcbPz3g3zLheA"

	// fixtureBlockedID is the member blocked.json records.
	fixtureBlockedID = "YmxvY2tlZC1maXh0dXJlLTE"

	// fixtureActivityID is the activity in challenge.json that route.json belongs to.
	fixtureActivityID = "AAAAAAAAAAAAAAAAAAAAAA"
)

// readFixture returns one of storageFixtures.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	return fixture.Read(t, filepath.Join(storageFixtures, name))
}

// TestStorageFixturesDecode decodes every 1.0 document with no field left unknown, and encodes it back
// with every field it had, so a renamed, removed or retyped field fails here.
func TestStorageFixturesDecode(t *testing.T) {
	documents := map[string]any{
		"board.json":     &Board{},
		"member.json":    &Member{},
		"blocked.json":   &blocked{},
		"challenge.json": &WeeklyChallenge{},
		"route.json":     &Route{},
	}
	for name, value := range documents {
		t.Run(name, func(t *testing.T) {
			fixture.ExpectRoundTrip(t, readFixture(t, name), value)
		})
	}
}

// TestStorageFixturesServe stores the 1.0 documents where 1.0 keeps them, compressed as 1.0 writes them,
// and checks the board serves each one as it was.
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
		{boardCollection, boardID, "board.json"},
		{membersCollection, fixtureMemberID, "member.json"},
		{blockedCollection, fixtureBlockedID, "blocked.json"},
		{weeksPrefix + testWeek, fixtureMemberID, "challenge.json"},
		{routeCollection(fixtureMemberID, testWeek), fixtureActivityID, "route.json"},
	}
	for _, document := range documents {
		if _, err := backend.Put(ctx, document.collection, document.id, readFixture(t, document.name), 0); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{
		Store:     backend,
		Allowlist: loadAllowlist(t),
		Now:       func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) },
	}

	board, err := service.Board(ctx)
	if err != nil || board.Name != "Fixture Gym" || board.StorageVersion != 1 {
		t.Fatalf("board: %+v, %v", board, err)
	}
	if _, err := service.Member(ctx, fixtureMemberID); err != nil {
		t.Fatalf("member: %v", err)
	}
	blockedMembers, err := service.Blocked(ctx)
	if err != nil || len(blockedMembers) != 1 || blockedMembers[0].ID != fixtureBlockedID {
		t.Fatalf("blocked: %+v, %v", blockedMembers, err)
	}
	memberWeeklyChallenge, err := service.MemberWeeklyChallenge(ctx, fixtureMemberID, testWeek)
	if err != nil {
		t.Fatalf("weekly challenge: %v", err)
	}
	encoded, err := json.Marshal(memberWeeklyChallenge.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	fixture.ExpectContains(t, readFixture(t, "challenge.json"), encoded)
	scoreboard, err := service.Scoreboard(ctx, testWeek)
	if err != nil || len(scoreboard.Entries) != 1 || scoreboard.Entries[0].Challenge == nil {
		t.Fatalf("scoreboard: %+v, %v", scoreboard, err)
	}
	route, err := service.Route(ctx, fixtureMemberID, testWeek, fixtureActivityID)
	if err != nil || route.Polyline != "_p~iF~ps|U_ulLnnqC_mqNvxq`@" {
		t.Fatalf("route: %+v, %v", route, err)
	}
}
