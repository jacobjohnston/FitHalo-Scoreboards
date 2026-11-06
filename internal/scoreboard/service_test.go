package scoreboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/polyline"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/problem"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// testWeek is testWeekStart as a path segment.
const testWeek = "2026-10-19"

// hookStore is a store that runs beforeChallengePut once, just before the next write of a weekly challenge.
type hookStore struct {
	// Store is the backend every call reaches.
	store.Store

	// beforeChallengePut runs before the next weekly challenge write, then is cleared.
	beforeChallengePut func()
}

// Put runs beforeChallengePut when collection holds weekly challenges, then writes.
func (h *hookStore) Put(ctx context.Context, collection, id string, data []byte, expectedVersion int64) (int64, error) {
	if hook := h.beforeChallengePut; hook != nil && strings.HasPrefix(collection, weeksPrefix) {
		h.beforeChallengePut = nil
		hook()
	}
	return h.Store.Put(ctx, collection, id, data, expectedVersion)
}

// getHookStore is a store that runs afterGet once, just after the next read of a member.
type getHookStore struct {
	// Store is the backend every call reaches.
	store.Store

	// afterGet runs after the next read of a member, then is cleared.
	afterGet func()
}

// Get reads, then runs afterGet when collection holds members.
func (g *getHookStore) Get(ctx context.Context, collection, id string) (store.Document, error) {
	document, err := g.Store.Get(ctx, collection, id)
	if hook := g.afterGet; hook != nil && collection == membersCollection {
		g.afterGet = nil
		hook()
	}
	return document, err
}

// newTestService returns a service over a new directory store, with an open board whose weeks start on
// Monday and a clock in testWeekStart's week.
func newTestService(t *testing.T) (*Service, *hookStore) {
	t.Helper()
	directory, err := store.OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { directory.Close() })
	backend := &hookStore{Store: directory}
	service := &Service{
		Store:     backend,
		Allowlist: loadAllowlist(t),
		Now:       func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) },
		Logger:    slog.New(slog.DiscardHandler),
	}
	board, err := DefaultBoard()
	if err != nil {
		t.Fatal(err)
	}
	board.JoinMode = JoinOpen
	board.WeekStartDay = "monday"
	if _, _, err := service.EnsureBoard(context.Background(), board); err != nil {
		t.Fatal(err)
	}
	return service, backend
}

// join adds an active member with a new identifier, name and sharing.
func join(t *testing.T, service *Service, name string, chosen sharing.Sharing) Member {
	t.Helper()
	board, err := service.Board(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id, err := NewIdentifier()
	if err != nil {
		t.Fatal(err)
	}
	request := JoinRequest{InviteCode: board.InviteCode, PublicKey: "key", DisplayName: name, Sharing: chosen}
	member, _, err := service.Join(context.Background(), request, id)
	if err != nil {
		t.Fatal(err)
	}
	return member
}

// fillMembers stores count active members directly, without the join checks.
func fillMembers(t *testing.T, service *Service, count int) {
	t.Helper()
	for index := range count {
		id, err := NewIdentifier()
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(Member{ID: id, PublicKey: "key", DisplayName: fmt.Sprintf("Member %d", index), Status: StatusActive})
		if _, err := service.Store.Put(context.Background(), membersCollection, id, encoded, store.AnyVersion); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRejectingLosesToAnApprovalMadeDuringTheCall keeps a member the admin approves while a rejection is under way.
func TestRejectingLosesToAnApprovalMadeDuringTheCall(t *testing.T) {
	service, backend := newTestService(t)
	ctx := context.Background()
	member := join(t, service, "Blake", sharing.Lowest)
	if err := service.SetStatus(ctx, member.ID, StatusPending); err != nil {
		t.Fatal(err)
	}
	hook := &getHookStore{Store: backend}
	service.Store = hook
	hook.afterGet = func() {
		if err := service.SetStatus(ctx, member.ID, StatusActive); err != nil {
			t.Error(err)
		}
	}

	var failure *problem.Problem
	if err := service.RejectRequest(ctx, member.ID); !errors.As(err, &failure) || failure.Code != problem.NotFound {
		t.Fatalf("rejecting: %v", err)
	}
	if stored, err := service.Member(ctx, member.ID); err != nil || stored.Status != StatusActive {
		t.Fatalf("the approved member: %+v, %v", stored, err)
	}
}

// TestMemberLimit refuses a new member once the board has MaximumMembers, and lets an existing one rejoin.
func TestMemberLimit(t *testing.T) {
	service, _ := newTestService(t)
	existing := join(t, service, "Avery", sharing.Highest)
	fillMembers(t, service, MaximumMembers-1)
	board, _ := service.Board(context.Background())
	if accepting, err := service.AcceptingRequests(context.Background(), board); err != nil || accepting {
		t.Fatalf("AcceptingRequests on a full board = %v, %v, want false", accepting, err)
	}
	request := JoinRequest{InviteCode: board.InviteCode, PublicKey: "key", DisplayName: "Blake", Sharing: sharing.Highest}
	var failure *problem.Problem
	if _, _, err := service.Join(context.Background(), request, "newcomer"); !errors.As(err, &failure) || failure.Code != problem.MemberLimitReached {
		t.Fatalf("Join on a full board = %v, want memberLimitReached", err)
	}
	request.DisplayName = "Avery"
	if member, created, err := service.Join(context.Background(), request, existing.ID); err != nil || created || member.ID != existing.ID {
		t.Fatalf("rejoining a full board = %v, %v, %v, want the existing membership", member.ID, created, err)
	}
}

// encodedChallenge returns fullChallenge as a request body.
func encodedChallenge(t *testing.T) []byte {
	t.Helper()
	encoded, err := json.Marshal(fullChallenge())
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// TestDisplayNamesNeedVisibleCharactersOnly refuses hidden and invisible names and keeps real ones.
func TestDisplayNamesNeedVisibleCharactersOnly(t *testing.T) {
	for _, name := range []string{"Avery\u200B", "Av\u00ADery", "\uFEFFAvery", "A\u200Dvery", "\u202EAvery", "Ave\u2028ry", "\u200B", " \u0301 ", "\u3000",
		"🏃\u200D", "Avery\U000E0041"} {
		if _, err := NormalizeDisplayName(name); err == nil {
			t.Errorf("NormalizeDisplayName(%q) accepted it", name)
		}
	}
	emoji := []string{"🏳️\u200D🌈", "👩🏽\u200D💻", "\U0001F3F4\U000E0067\U000E0062\U000E0073\U000E0063\U000E0074\U000E007F"}
	for _, name := range append([]string{"Avery", "Zoë", "Ana María", "李雷", "🏃"}, emoji...) {
		if _, err := NormalizeDisplayName(name); err != nil {
			t.Errorf("NormalizeDisplayName(%q) = %v, want no error", name, err)
		}
	}
}

// TestExerciseNamesRefuseHiddenCharacters applies the display name filter to custom exercises.
func TestExerciseNamesRefuseHiddenCharacters(t *testing.T) {
	for _, name := range []string{"Landmine\nPress", "\u202Esserp", "Press\u200B", "Press\u0085"} {
		challenge := fullChallenge()
		(*challenge.Activities)[0].Strength.Exercises[0].Name = &name
		if err := validateWeeklyChallenge(&challenge, testWeekStart); err == nil {
			t.Errorf("validateWeeklyChallenge accepted the exercise name %q", name)
		}
	}
	challenge := fullChallenge()
	if err := validateWeeklyChallenge(&challenge, testWeekStart); err != nil {
		t.Fatalf("validateWeeklyChallenge(fullChallenge()) = %v, want no error", err)
	}
}

// TestWeeklyChallengeWrittenAsTheMaximumDropsIsStripped strips a weekly challenge whose write lands after the board's maximum
// went down and its enforcement already ran.
func TestWeeklyChallengeWrittenAsTheMaximumDropsIsStripped(t *testing.T) {
	service, backend := newTestService(t)
	member := join(t, service, "Avery", sharing.Highest)
	backend.beforeChallengePut = func() {
		err := store.Update(context.Background(), backend.Store, boardCollection, boardID, func(data []byte) ([]byte, error) {
			var board Board
			if err := json.Unmarshal(data, &board); err != nil {
				return nil, err
			}
			board.MaximumSharing = sharing.Lowest
			return json.Marshal(board)
		})
		if err != nil {
			t.Error(err)
		}
	}
	kept, err := service.PutWeeklyChallenge(context.Background(), member, testWeek, encodedChallenge(t))
	if err != nil {
		t.Fatal(err)
	}
	if kept.Score != nil || kept.Activities != nil {
		t.Errorf("PutWeeklyChallenge answered with more than the new maximum: %+v", kept)
	}
	stored, err := service.Store.Get(context.Background(), weeksPrefix+testWeek, member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if text := string(stored.Data); strings.Contains(text, "score") || strings.Contains(text, "activities") {
		t.Fatalf("the store kept a weekly challenge above the board's maximum: %s", text)
	}
}

// TestUnreadableChallengeIsSkipped serves the scoreboard without one member's undecodable weekly challenge.
func TestUnreadableChallengeIsSkipped(t *testing.T) {
	service, _ := newTestService(t)
	broken, working := join(t, service, "Avery", sharing.Highest), join(t, service, "Blake", sharing.Highest)
	if _, err := service.Store.Put(context.Background(), weeksPrefix+testWeek, broken.ID, []byte(`{"progress":-1}`), store.AnyVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutWeeklyChallenge(context.Background(), working, testWeek, encodedChallenge(t)); err != nil {
		t.Fatal(err)
	}
	result, err := service.Scoreboard(context.Background(), testWeek)
	if err != nil {
		t.Fatalf("Scoreboard = %v, want the other members' weekly challenges", err)
	}
	if len(result.Entries) != 2 || result.Entries[0].Member.ID != working.ID || result.Entries[0].Challenge == nil || result.Entries[1].Challenge != nil {
		t.Fatalf("got entries %+v, want Blake's weekly challenge and Avery without one", result.Entries)
	}
	if _, err := service.MemberWeeklyChallenge(context.Background(), broken.ID, testWeek); !isNotFound(err) {
		t.Fatalf("MemberWeeklyChallenge of an unreadable weekly challenge = %v, want notFound", err)
	}
}

// TestEnforcementDeletesUnreadableChallenge lowers the maximum, deleting a weekly challenge that is not JSON
// and keeping one that reads but no longer validates.
func TestEnforcementDeletesUnreadableChallenge(t *testing.T) {
	service, _ := newTestService(t)
	broken, invalid := join(t, service, "Avery", sharing.Highest), join(t, service, "Blake", sharing.Highest)
	if _, err := service.Store.Put(context.Background(), weeksPrefix+testWeek, broken.ID, []byte(`{"progress":`), store.AnyVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store.Put(context.Background(), weeksPrefix+testWeek, invalid.ID, []byte(`{"progress":-1}`), store.AnyVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateBoard(context.Background(), func(board *Board) error {
		board.MaximumSharing = sharing.Lowest
		return nil
	}); err != nil {
		t.Fatalf("UpdateBoard = %v, want no error", err)
	}
	if _, err := service.Store.Get(context.Background(), weeksPrefix+testWeek, broken.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v reading the unreadable weekly challenge, want store.ErrNotFound", err)
	}
	if _, err := service.Store.Get(context.Background(), weeksPrefix+testWeek, invalid.ID); err != nil {
		t.Fatalf("got %v reading the weekly challenge that no longer validates, want it kept", err)
	}
}

// TestTurningAvatarsOffDeletesThem removes every stored avatar and clears HasAvatar when avatars go off.
func TestTurningAvatarsOffDeletesThem(t *testing.T) {
	service, _ := newTestService(t)
	member := join(t, service, "Avery", sharing.Highest)
	source := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for index := range source.Pix {
		source.Pix[index] = 200
	}
	source.Set(0, 0, color.NRGBA{A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	if err := service.SetAvatar(context.Background(), member, encoded.Bytes(), "image/png"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateBoard(context.Background(), func(board *Board) error {
		board.AvatarsAllowed = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store.Get(context.Background(), avatarsCollection, member.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("got %v reading the avatar, want store.ErrNotFound", err)
	}
	stored, err := service.Member(context.Background(), member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.HasAvatar {
		t.Error("the member still has HasAvatar after avatars were turned off")
	}
}

// TestInviteIsCheckedBeforeBlocking answers a blocked key without the invite as inviteInvalid.
func TestInviteIsCheckedBeforeBlocking(t *testing.T) {
	service, _ := newTestService(t)
	member := join(t, service, "Avery", sharing.Highest)
	if err := service.DeleteMember(context.Background(), member.ID, true); err != nil {
		t.Fatal(err)
	}
	request := JoinRequest{InviteCode: "WrongWrongWrongWrong00", PublicKey: "key", DisplayName: "Avery", Sharing: sharing.Lowest}
	_, _, err := service.Join(context.Background(), request, member.ID)
	if failure := (*problem.Problem)(nil); !errors.As(err, &failure) || failure.Code != problem.InviteInvalid {
		t.Fatalf("Join with a wrong invite from a blocked key = %v, want inviteInvalid", err)
	}
}

// challengeWithActivities returns fullChallenge with count copies of its activity, each with its own identifier.
func challengeWithActivities(count int) WeeklyChallenge {
	challenge := fullChallenge()
	template := (*challenge.Activities)[0]
	activities := make([]Activity, count)
	for index := range activities {
		activities[index] = template
		activities[index].ID = fmt.Sprintf("Activity%014d", index)
	}
	challenge.Activities = &activities
	return challenge
}

// widestRoute returns a route of polyline.MaximumPoints points, each far from the last, so it encodes to
// about the largest a valid route can be.
func widestRoute(t *testing.T) []byte {
	t.Helper()
	var encoded strings.Builder
	writeValue := func(value int64) {
		shifted := value << 1
		if value < 0 {
			shifted = ^shifted
		}
		for shifted >= 0x20 {
			encoded.WriteByte(byte((0x20 | shifted&0x1f) + 63))
			shifted >>= 5
		}
		encoded.WriteByte(byte(shifted + 63))
	}
	var latitude, longitude int64
	for index := range polyline.MaximumPoints {
		nextLatitude, nextLongitude := int64(8_000_000), int64(17_000_000)
		if index%2 == 1 {
			nextLatitude, nextLongitude = -nextLatitude, -nextLongitude
		}
		writeValue(nextLatitude - latitude)
		writeValue(nextLongitude - longitude)
		latitude, longitude = nextLatitude, nextLongitude
	}
	body, err := json.Marshal(Route{Polyline: encoded.String()})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// isChallengeFull reports whether err is the problem for a weekly challenge past MaximumWeeklyChallengeBytes.
func isChallengeFull(err error) bool {
	failure := (*problem.Problem)(nil)
	return errors.As(err, &failure) && failure.Code == problem.PayloadTooLarge
}

// TestRoutesStopAtTheChallengeCap proves routes are refused once a weekly challenge would store more than 1 MB, and
// that replacing a stored route counts it once.
func TestRoutesStopAtTheChallengeCap(t *testing.T) {
	service, backend := newTestService(t)
	ctx := context.Background()
	member := join(t, service, "Avery", sharing.Highest)
	challenge := challengeWithActivities(40)
	body, err := json.Marshal(challenge)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutWeeklyChallenge(ctx, member, testWeek, body); err != nil {
		t.Fatal(err)
	}
	route := widestRoute(t)

	stored := 0
	for _, activity := range *challenge.Activities {
		err := service.PutRoute(ctx, member, testWeek, activity.ID, route)
		if isChallengeFull(err) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		stored++
	}
	if stored == 0 || stored == len(*challenge.Activities) {
		t.Fatalf("got %d routes stored before the cap, want some but not all %d", stored, len(*challenge.Activities))
	}
	routes, err := backend.List(ctx, routeCollection(member.ID, testWeek))
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, item := range routes {
		total += len(item.Data)
	}
	if total > MaximumWeeklyChallengeBytes {
		t.Fatalf("got %d route bytes stored, want at most %d", total, MaximumWeeklyChallengeBytes)
	}
	if err := service.PutRoute(ctx, member, testWeek, (*challenge.Activities)[0].ID, route); err != nil {
		t.Fatalf("replacing a stored route at the cap: %v", err)
	}
}

// TestWeeklyChallengePastTheCapIsRefused proves a weekly challenge whose own record would pass 1 MB is not stored.
func TestWeeklyChallengePastTheCapIsRefused(t *testing.T) {
	service, backend := newTestService(t)
	ctx := context.Background()
	member := join(t, service, "Avery", sharing.Highest)
	challenge := challengeWithActivities(10)
	for index := range *challenge.Activities {
		activity := &(*challenge.Activities)[index]
		exercise := activity.Strength.Exercises[0]
		exercise.Sets = slices.Repeat(exercise.Sets, 50)
		activity.Strength = &Strength{Exercises: slices.Repeat([]StrengthExercise{exercise}, 50)}
	}
	body, err := json.Marshal(challenge)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= MaximumWeeklyChallengeBytes {
		t.Fatalf("the test weekly challenge is only %d bytes, want more than %d", len(body), MaximumWeeklyChallengeBytes)
	}
	if _, err := service.PutWeeklyChallenge(ctx, member, testWeek, body); !isChallengeFull(err) {
		t.Fatalf("PutWeeklyChallenge() = %v, want payloadTooLarge", err)
	}
	if _, err := backend.Get(ctx, weeksPrefix+testWeek, member.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v reading the refused weekly challenge, want store.ErrNotFound", err)
	}
}

// TestUsageCountsChallengesAndRoutes proves Usage reports each stored weekly challenge, its routes and their sizes.
func TestUsageCountsChallengesAndRoutes(t *testing.T) {
	service, backend := newTestService(t)
	ctx := context.Background()
	member := join(t, service, "Avery", sharing.Highest)
	other := join(t, service, "Jordan", sharing.Highest)
	challenge := challengeWithActivities(2)
	body, err := json.Marshal(challenge)
	if err != nil {
		t.Fatal(err)
	}
	route := []byte(`{"polyline":"_p~iF~ps|U_ulLnnqC_mqNvxq` + "`" + `@"}`)
	for _, someone := range []Member{member, other} {
		if _, err := service.PutWeeklyChallenge(ctx, someone, testWeek, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.PutRoute(ctx, member, testWeek, (*challenge.Activities)[0].ID, route); err != nil {
		t.Fatal(err)
	}

	usage, err := service.Usage(ctx, member.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := backend.Get(ctx, weeksPrefix+testWeek, member.ID)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := backend.List(ctx, routeCollection(member.ID, testWeek))
	if err != nil || len(routes) != 1 {
		t.Fatalf("got %d stored routes, %v; want 1", len(routes), err)
	}
	want := WeeklyChallengeUsage{WeekStart: testWeek, ChallengeBytes: len(stored.Data), Routes: 1, RouteBytes: len(routes[0].Data)}
	if len(usage.Challenges) != 1 || usage.Challenges[0] != want || usage.AvatarBytes != 0 || usage.Total() != want.Total() {
		t.Fatalf("Usage() = %+v, want one weekly challenge %+v and no avatar", usage, want)
	}
	stranger, err := service.Usage(ctx, "NoSuchMemberNoSuchMemb")
	if err != nil || len(stranger.Challenges) != 0 || stranger.Total() != 0 {
		t.Fatalf("Usage(stranger) = %+v, %v; want nothing stored", stranger, err)
	}
}

// TestBoardAvatar stores, serves and removes the board's own avatar, which members' avatars do not govern.
func TestBoardAvatar(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	if _, err := service.BoardAvatar(ctx); !isNotFound(err) {
		t.Fatalf("got %v before any avatar, want notFound", err)
	}
	var failure *problem.Problem
	if err := service.SetBoardAvatar(ctx, strings.NewReader("not an image"), "image/png"); !errors.As(err, &failure) || failure.Code != problem.ImageInvalid {
		t.Fatalf("got %v storing a bad image, want imageInvalid", err)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	if err := service.SetBoardAvatar(ctx, bytes.NewReader(encoded.Bytes()), "image/png"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateBoard(ctx, func(board *Board) error {
		board.AvatarsAllowed = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := service.BoardAvatar(ctx)
	if err != nil {
		t.Fatalf("turning members' avatars off lost the board's: %v", err)
	}
	if _, format, err := image.DecodeConfig(bytes.NewReader(data)); err != nil || format != "jpeg" {
		t.Fatalf("the board's avatar is %s, %v", format, err)
	}
	if err := service.RemoveBoardAvatar(ctx); err != nil {
		t.Fatal(err)
	}
	if board, _ := service.Board(ctx); board.HasAvatar {
		t.Error("the board still has an avatar after removing it")
	}
	if _, err := service.Store.Get(ctx, boardCollection, boardAvatarID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("got %v reading the removed avatar, want store.ErrNotFound", err)
	}
	if err := service.RemoveBoardAvatar(ctx); err != nil {
		t.Errorf("removing no avatar failed: %v", err)
	}
}

// TestStorageVersion stamps a new board with StorageVersion and refuses a board a newer server laid out.
func TestStorageVersion(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	board, err := service.Board(ctx)
	if err != nil || board.StorageVersion != StorageVersion {
		t.Fatalf("a new board's storage version is %d, %v; want %d", board.StorageVersion, err, StorageVersion)
	}

	board.StorageVersion = StorageVersion + 1
	encoded, err := json.Marshal(board)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store.Put(ctx, boardCollection, boardID, encoded, store.AnyVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Board(ctx); !errors.Is(err, ErrStorageNewer) {
		t.Errorf("Board: got %v, want ErrStorageNewer", err)
	}
	if _, _, err := service.EnsureBoard(ctx, board); !errors.Is(err, ErrStorageNewer) {
		t.Errorf("EnsureBoard: got %v, want ErrStorageNewer", err)
	}
	if _, err := service.UpdateBoard(ctx, func(board *Board) error {
		board.Name = "Renamed"
		return nil
	}); !errors.Is(err, ErrStorageNewer) {
		t.Errorf("UpdateBoard: got %v, want ErrStorageNewer", err)
	}
	if err := service.SweepRetention(ctx); !errors.Is(err, ErrStorageNewer) {
		t.Errorf("SweepRetention: got %v, want ErrStorageNewer", err)
	}
	document, err := service.Store.Get(ctx, boardCollection, boardID)
	if err != nil || !bytes.Equal(document.Data, encoded) {
		t.Errorf("the newer board was rewritten: %s, %v", document.Data, err)
	}
}

// TestStorageVersionZero reads a board stored before the field existed as version 1.
func TestStorageVersionZero(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	board, err := service.Board(ctx)
	if err != nil {
		t.Fatal(err)
	}
	board.StorageVersion = 0
	encoded, err := json.Marshal(board)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store.Put(ctx, boardCollection, boardID, encoded, store.AnyVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Board(ctx); err != nil {
		t.Errorf("a board without a storage version: %v", err)
	}
}
