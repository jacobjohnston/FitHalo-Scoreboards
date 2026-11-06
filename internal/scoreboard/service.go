package scoreboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/avatar"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/password"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/polyline"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/problem"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// The store's collections and the prefixes of its per-week collections, whose stored names stay as they are.
const (
	// boardCollection holds the single board document.
	boardCollection = "board"

	// boardID is the board document's identifier.
	boardID = "board"

	// boardAvatarID is the board's own avatar as JPEG, beside the board document.
	boardAvatarID = "avatar"

	// membersCollection holds a Member per member identifier.
	membersCollection = "members"

	// blockedCollection holds a blocked record per blocked member identifier.
	blockedCollection = "blocked"

	// avatarsCollection holds each member's avatar as JPEG.
	avatarsCollection = "avatars"

	// weeksPrefix begins each weekly challenge's collection, weeks/<weekStart>, of a WeeklyChallenge per member.
	weeksPrefix = "weeks/"

	// routesPrefix begins each member's routes for one weekly challenge, routes/<memberID>/<weekStart>.
	routesPrefix = "routes/"
)

// identifierBytes is how many random bytes an identifier holds, 22 characters in unpadded base64url.
const identifierBytes = 16

// newDocument is the expected version that writes a document only when none exists yet.
const newDocument int64 = 0

// MaximumWeeklyChallengeBytes is the most one member's weekly challenge may store, its routes included.
const MaximumWeeklyChallengeBytes = 1 << 20

// StorageVersion is the newest layout of stored data this server reads and writes, from docs/specifications.md.
const StorageVersion = 1

// ErrStorageNewer means a newer server has laid out the stored data, so this one must not touch it.
var ErrStorageNewer = errors.New("scoreboard: a newer server has laid out the stored data")

// Service is the board's behaviour over its store.
type Service struct {
	// Store holds the board, its members, weekly challenges, routes and avatars.
	Store store.Store

	// Allowlist says which weekly challenge fields each sharing level keeps.
	Allowlist *sharing.Allowlist

	// Now returns the current time, replaceable in tests.
	Now func() time.Time

	// Pepper is mixed into every password hash, from the server's configuration. It may be empty.
	Pepper []byte

	// Logger records stored weekly challenges that can no longer be read; nil uses slog.Default.
	Logger *slog.Logger

	// joins serialises counting the approval queue and creating a membership, so the queue cannot overshoot.
	joins sync.Mutex

	// writes serialises each member's uploads, so a weekly challenge and its routes are measured against the cap together.
	writes keyedMutex

	// boardAvatarMutex guards boardAvatar and boardAvatarFetched.
	boardAvatarMutex sync.Mutex

	// boardAvatar is the board avatar last read from the store, or nil when none is cached.
	boardAvatar []byte

	// boardAvatarFetched is when boardAvatar was read.
	boardAvatarFetched time.Time
}

// keyedMutex is a set of mutexes, one per key, for serialising work on one member at a time.
type keyedMutex struct {
	// mutex guards locks.
	mutex sync.Mutex

	// locks holds each key's mutex and how many goroutines want it, so it can be dropped when none do.
	locks map[string]*keyedLock
}

// keyedLock is one key's mutex and its waiters.
type keyedLock struct {
	// mutex is the key's lock.
	mutex sync.Mutex

	// waiting counts the goroutines holding or waiting for mutex.
	waiting int
}

// lock takes the key's mutex and returns the function that releases it.
func (k *keyedMutex) lock(key string) func() {
	k.mutex.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	lock := k.locks[key]
	if lock == nil {
		lock = &keyedLock{}
		k.locks[key] = lock
	}
	lock.waiting++
	k.mutex.Unlock()
	lock.mutex.Lock()
	return func() {
		lock.mutex.Unlock()
		k.mutex.Lock()
		lock.waiting--
		if lock.waiting == 0 {
			delete(k.locks, key)
		}
		k.mutex.Unlock()
	}
}

// NewIdentifier returns 16 random bytes in unpadded base64url, the shape of every identifier.
func NewIdentifier() (string, error) {
	random := make([]byte, identifierBytes)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}

// DefaultBoard is a new board: joined by approval, everything allowed, and 52 weeks kept.
func DefaultBoard() (Board, error) {
	id, err := NewIdentifier()
	if err != nil {
		return Board{}, err
	}
	inviteCode, err := NewIdentifier()
	if err != nil {
		return Board{}, err
	}
	return Board{
		ID:             id,
		WeekStartDay:   "sunday",
		RetentionWeeks: 52,
		JoinMode:       JoinApproval,
		MaximumPending: DefaultMaximumPending,
		AvatarsAllowed: true,
		MaximumSharing: sharing.Highest,
		Ranking:        RankByName,
		InviteCode:     inviteCode,
		ShowInvite:     true,
	}, nil
}

// EnsureBoard creates the board from initial if the store has none, and returns the board and whether
// it was created.
func (s *Service) EnsureBoard(ctx context.Context, initial Board) (Board, bool, error) {
	existing, err := s.Board(ctx)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return Board{}, false, err
	}
	initial.StorageVersion = StorageVersion
	encoded, err := json.Marshal(initial)
	if err != nil {
		return Board{}, false, err
	}
	_, err = s.Store.Put(ctx, boardCollection, boardID, encoded, newDocument)
	if errors.Is(err, store.ErrConflict) {
		board, err := s.Board(ctx)
		return board, false, err
	}
	if err != nil {
		return Board{}, false, err
	}
	return initial, true, nil
}

// Board returns the board, store.ErrNotFound before it is created, or ErrStorageNewer.
func (s *Service) Board(ctx context.Context) (Board, error) {
	var board Board
	if err := s.read(ctx, boardCollection, boardID, &board); err != nil {
		return Board{}, err
	}
	if err := board.checkStorage(); err != nil {
		return Board{}, err
	}
	return board, nil
}

// checkStorage returns ErrStorageNewer when the board's data is laid out newer than StorageVersion.
func (b Board) checkStorage() error {
	if b.StorageVersion > StorageVersion {
		return fmt.Errorf("%w: storage version %d, and this server reads up to %d; upgrade the server",
			ErrStorageNewer, b.StorageVersion, StorageVersion)
	}
	return nil
}

// Member returns a member, or a notFound problem.
func (s *Service) Member(ctx context.Context, id string) (Member, error) {
	var member Member
	err := s.read(ctx, membersCollection, id, &member)
	if errors.Is(err, store.ErrNotFound) {
		return Member{}, problem.New(problem.NotFound, "no such member")
	}
	return member, err
}

// Effective is a member's effective sharing on the board.
func Effective(member Member, board Board) sharing.Sharing {
	return sharing.Effective(member.Sharing, board.MaximumSharing)
}

// Join makes the holder of a public key a member, or returns their existing membership. The caller has
// already verified that the request was signed by that key.
func (s *Service) Join(ctx context.Context, request JoinRequest, memberID string) (Member, bool, error) {
	board, err := s.Board(ctx)
	if err != nil {
		return Member{}, false, err
	}
	existing, err := s.Member(ctx, memberID)
	if err == nil {
		return existing, false, nil
	}
	if !isNotFound(err) {
		return Member{}, false, err
	}
	if subtle.ConstantTimeCompare([]byte(request.InviteCode), []byte(board.InviteCode)) != 1 {
		return Member{}, false, problem.New(problem.InviteInvalid, "")
	}
	_, err = s.Store.Get(ctx, blockedCollection, memberID)
	if err == nil {
		return Member{}, false, problem.New(problem.Blocked, "")
	}
	if !errors.Is(err, store.ErrNotFound) {
		return Member{}, false, err
	}
	if board.PasswordHash != "" {
		matches, err := password.Verify(board.PasswordHash, request.Password, s.Pepper)
		if err != nil {
			return Member{}, false, err
		}
		if !matches {
			return Member{}, false, problem.New(problem.PasswordIncorrect, "")
		}
	}
	displayName, err := NormalizeDisplayName(request.DisplayName)
	if err != nil {
		return Member{}, false, problem.New(problem.BadRequest, err.Error())
	}
	if err := request.Sharing.Validate(); err != nil {
		return Member{}, false, problem.New(problem.BadRequest, err.Error())
	}
	s.joins.Lock()
	defer s.joins.Unlock()
	count, err := s.MemberCount(ctx)
	if err != nil {
		return Member{}, false, err
	}
	if count >= MaximumMembers {
		return Member{}, false, problem.New(problem.MemberLimitReached, "")
	}
	accepting, err := s.AcceptingRequests(ctx, board)
	if err != nil {
		return Member{}, false, err
	}
	if !accepting {
		return Member{}, false, problem.New(problem.PendingLimitReached, "")
	}

	member := Member{
		ID:          memberID,
		PublicKey:   request.PublicKey,
		DisplayName: displayName,
		Status:      StatusPending,
		Sharing:     request.Sharing.Normalized(),
		JoinedAt:    s.Now().UTC().Truncate(time.Second),
	}
	if board.JoinMode == JoinOpen {
		member.Status = StatusActive
	}
	encoded, err := json.Marshal(member)
	if err != nil {
		return Member{}, false, err
	}
	_, err = s.Store.Put(ctx, membersCollection, memberID, encoded, newDocument)
	if errors.Is(err, store.ErrConflict) {
		existing, err = s.Member(ctx, memberID)
		return existing, false, err
	}
	if err != nil {
		return Member{}, false, err
	}
	// Another instance may have filled the board meanwhile; the newest members past the limit withdraw.
	overshoot, err := s.pastMemberLimit(ctx, member)
	if err != nil {
		return Member{}, false, err
	}
	if overshoot {
		if err := s.Store.Delete(ctx, membersCollection, memberID, store.AnyVersion); err != nil {
			return Member{}, false, err
		}
		return Member{}, false, problem.New(problem.MemberLimitReached, "")
	}
	if member.Status == StatusPending {
		// Another instance may have filled the queue meanwhile; the newest requests past the limit withdraw.
		overshoot, err := s.pastPendingLimit(ctx, board, member)
		if err != nil {
			return Member{}, false, err
		}
		if overshoot {
			if err := s.Store.Delete(ctx, membersCollection, memberID, store.AnyVersion); err != nil {
				return Member{}, false, err
			}
			return Member{}, false, problem.New(problem.PendingLimitReached, "")
		}
	}
	return member, true, nil
}

// pastMemberLimit reports whether member, just stored, ranks beyond MaximumMembers by when each member joined
// and then by identifier, so concurrent joins agree on who withdraws.
func (s *Service) pastMemberLimit(ctx context.Context, member Member) (bool, error) {
	members, err := s.Members(ctx)
	if err != nil || len(members) <= MaximumMembers {
		return false, err
	}
	rank := 0
	for _, other := range members {
		if other.ID == member.ID {
			continue
		}
		if other.JoinedAt.Before(member.JoinedAt) || (other.JoinedAt.Equal(member.JoinedAt) && other.ID < member.ID) {
			rank++
		}
	}
	return rank >= MaximumMembers, nil
}

// pastPendingLimit reports whether member, just made pending, ranks beyond the board's limit among the waiting
// requests, ordered by when they joined and then by identifier, so concurrent joins agree on who withdraws.
func (s *Service) pastPendingLimit(ctx context.Context, board Board, member Member) (bool, error) {
	members, err := s.Members(ctx)
	if err != nil {
		return false, err
	}
	rank := 0
	for _, other := range members {
		if other.Status != StatusPending || other.ID == member.ID {
			continue
		}
		if other.JoinedAt.Before(member.JoinedAt) || (other.JoinedAt.Equal(member.JoinedAt) && other.ID < member.ID) {
			rank++
		}
	}
	return rank >= board.PendingLimit(), nil
}

// UpdateMember changes a member's display name or sharing, and deletes whatever falls outside the new
// sharing before returning.
func (s *Service) UpdateMember(ctx context.Context, id string, update MemberUpdate) (Member, error) {
	if update.DisplayName == nil && update.Sharing == nil {
		return Member{}, problem.New(problem.BadRequest, "nothing to change")
	}
	if update.DisplayName != nil {
		name, err := NormalizeDisplayName(*update.DisplayName)
		if err != nil {
			return Member{}, problem.New(problem.BadRequest, err.Error())
		}
		update.DisplayName = &name
	}
	if update.Sharing != nil {
		if err := update.Sharing.Validate(); err != nil {
			return Member{}, problem.New(problem.BadRequest, err.Error())
		}
	}

	var before, after Member
	err := s.updateMember(ctx, id, func(member *Member) error {
		before = *member
		if update.DisplayName != nil {
			member.DisplayName = *update.DisplayName
		}
		if update.Sharing != nil {
			member.Sharing = update.Sharing.Normalized()
		}
		after = *member
		return nil
	})
	if err != nil {
		return Member{}, err
	}
	if !after.Sharing.Covers(before.Sharing) {
		board, err := s.Board(ctx)
		if err != nil {
			return Member{}, err
		}
		if err := s.EnforceSharing(ctx, after, board); err != nil {
			return Member{}, err
		}
	}
	return after, nil
}

// SetStatus approves a pending member. The admin portal calls it.
func (s *Service) SetStatus(ctx context.Context, id, status string) error {
	if status != StatusActive && status != StatusPending {
		return fmt.Errorf("scoreboard: unknown status %q", status)
	}
	return s.updateMember(ctx, id, func(member *Member) error {
		member.Status = status
		return nil
	})
}

// SetCanApproveMembers lets a member approve and reject join requests, or stops them. The admin portal calls it.
func (s *Service) SetCanApproveMembers(ctx context.Context, id string, allowed bool) error {
	return s.updateMember(ctx, id, func(member *Member) error {
		member.CanApproveMembers = allowed
		return nil
	})
}

// PendingRequests returns the members waiting for approval, oldest first.
func (s *Service) PendingRequests(ctx context.Context) (MemberRequests, error) {
	members, err := s.Members(ctx)
	if err != nil {
		return MemberRequests{}, err
	}
	requests := make([]MemberRequest, 0)
	for _, member := range members {
		if member.Status == StatusPending {
			requests = append(requests, MemberRequest{ID: member.ID, DisplayName: member.DisplayName, JoinedAt: member.JoinedAt})
		}
	}
	sort.SliceStable(requests, func(first, second int) bool {
		if requests[first].JoinedAt.Equal(requests[second].JoinedAt) {
			return requests[first].ID < requests[second].ID
		}
		return requests[first].JoinedAt.Before(requests[second].JoinedAt)
	})
	return MemberRequests{Requests: requests}, nil
}

// ApproveRequest makes a waiting member active. Anyone not waiting answers notFound, so the permission reaches no
// active member.
func (s *Service) ApproveRequest(ctx context.Context, id string) error {
	return s.updateMember(ctx, id, func(member *Member) error {
		if member.Status != StatusPending {
			return noSuchRequest()
		}
		member.Status = StatusActive
		return nil
	})
}

// RejectRequest deletes a waiting member, who stores nothing else and may ask again. Anyone not waiting answers
// notFound, so the permission reaches no active member, even one approved during the call.
func (s *Service) RejectRequest(ctx context.Context, id string) error {
	for {
		document, err := s.Store.Get(ctx, membersCollection, id)
		if errors.Is(err, store.ErrNotFound) {
			return noSuchRequest()
		}
		if err != nil {
			return err
		}
		var member Member
		if err := json.Unmarshal(document.Data, &member); err != nil {
			return err
		}
		if member.Status != StatusPending {
			return noSuchRequest()
		}
		err = s.Store.Delete(ctx, membersCollection, id, document.Version)
		if errors.Is(err, store.ErrNotFound) {
			return noSuchRequest()
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// Leave erases a member and everything they shared. Their data goes first, so a failure part way
// leaves a membership that can leave again.
func (s *Service) Leave(ctx context.Context, id string) error {
	challengeCollections, err := s.Store.Collections(ctx, weeksPrefix)
	if err != nil {
		return err
	}
	for _, collection := range challengeCollections {
		if err := s.Store.Delete(ctx, collection, id, store.AnyVersion); err != nil {
			return err
		}
	}
	if err := s.Store.DeleteCollection(ctx, routesPrefix+id); err != nil {
		return err
	}
	if err := s.Store.Delete(ctx, avatarsCollection, id, store.AnyVersion); err != nil {
		return err
	}
	return s.Store.Delete(ctx, membersCollection, id, store.AnyVersion)
}

// DeleteMember erases a member, as Leave does, and can block them from joining again. The admin portal
// calls it.
func (s *Service) DeleteMember(ctx context.Context, id string, block bool) error {
	if block {
		encoded, err := json.Marshal(blocked{BlockedAt: s.Now().UTC().Truncate(time.Second)})
		if err != nil {
			return err
		}
		if _, err := s.Store.Put(ctx, blockedCollection, id, encoded, store.AnyVersion); err != nil {
			return err
		}
	}
	return s.Leave(ctx, id)
}

// SetAvatar stores a member's avatar, re-encoded.
func (s *Service) SetAvatar(ctx context.Context, member Member, data []byte, contentType string) error {
	board, err := s.Board(ctx)
	if err != nil {
		return err
	}
	if !board.AvatarsAllowed {
		return problem.New(problem.AvatarsNotAllowed, "")
	}
	normalized, err := avatar.Normalize(data, contentType)
	if errors.Is(err, avatar.ErrInvalid) {
		return problem.New(problem.ImageInvalid, "")
	}
	if err != nil {
		return err
	}
	if _, err := s.Store.Put(ctx, avatarsCollection, member.ID, normalized, store.AnyVersion); err != nil {
		return err
	}
	if err := s.updateMember(ctx, member.ID, func(stored *Member) error {
		stored.HasAvatar = true
		return nil
	}); err != nil {
		if isNotFound(err) {
			// The member left while this was stored, after Leave deleted their avatar.
			return errors.Join(err, s.Store.Delete(ctx, avatarsCollection, member.ID, store.AnyVersion))
		}
		return err
	}
	// The admin may have turned avatars off while this one was stored, after RemoveAvatars ran.
	if board, err = s.Board(ctx); err != nil {
		return err
	}
	if !board.AvatarsAllowed {
		if err := s.DeleteAvatar(ctx, member.ID); err != nil {
			return err
		}
		return problem.New(problem.AvatarsNotAllowed, "")
	}
	return nil
}

// DeleteAvatar removes a member's avatar.
func (s *Service) DeleteAvatar(ctx context.Context, id string) error {
	if err := s.updateMember(ctx, id, func(stored *Member) error {
		stored.HasAvatar = false
		return nil
	}); err != nil {
		return err
	}
	return s.Store.Delete(ctx, avatarsCollection, id, store.AnyVersion)
}

// Avatar returns a member's avatar as JPEG, or notFound when they have none or the board shows none.
func (s *Service) Avatar(ctx context.Context, id string) ([]byte, error) {
	board, err := s.Board(ctx)
	if err != nil {
		return nil, err
	}
	member, err := s.activeMember(ctx, id)
	if err != nil {
		return nil, err
	}
	if !board.AvatarsAllowed || !member.HasAvatar {
		return nil, problem.New(problem.NotFound, "no avatar")
	}
	document, err := s.Store.Get(ctx, avatarsCollection, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, problem.New(problem.NotFound, "no avatar")
	}
	return document.Data, err
}

// PutWeeklyChallenge replaces a member's weekly challenge with body, stripped to their effective sharing, and
// returns what was kept. Routes for activities no longer in the weekly challenge are deleted.
func (s *Service) PutWeeklyChallenge(ctx context.Context, member Member, weekStartValue string, body []byte) (WeeklyChallenge, error) {
	board, err := s.Board(ctx)
	if err != nil {
		return WeeklyChallenge{}, err
	}
	if member, err = s.Member(ctx, member.ID); err != nil {
		return WeeklyChallenge{}, err
	}
	weekStart, failure := ParseWeekStart(board, weekStartValue, s.Now())
	if failure != nil {
		return WeeklyChallenge{}, failure
	}
	document, err := decodeObject(body)
	if err != nil {
		return WeeklyChallenge{}, problem.New(problem.BadRequest, err.Error())
	}
	delete(document, "updatedAt")
	// A field of the wrong shape is malformed whether or not sharing keeps it, so it is refused, not stripped.
	if err := checkShape(document); err != nil {
		return WeeklyChallenge{}, problem.New(problem.BadRequest, err.Error())
	}
	effective := Effective(member, board)
	challenge, err := s.challengeFrom(document, effective, weekStart)
	if err != nil {
		return WeeklyChallenge{}, problem.New(problem.BadRequest, err.Error())
	}
	updatedAt := s.Now().UTC().Truncate(time.Second)
	challenge.UpdatedAt = &updatedAt

	encoded, err := json.Marshal(challenge)
	if err != nil {
		return WeeklyChallenge{}, err
	}
	defer s.writes.lock(member.ID)()
	routeBytes, err := s.keptRouteBytes(ctx, member.ID, weekStartValue, challenge, effective)
	if err != nil {
		return WeeklyChallenge{}, err
	}
	if len(encoded)+routeBytes > MaximumWeeklyChallengeBytes {
		return WeeklyChallenge{}, challengeFull()
	}
	collection := weeksPrefix + weekStartValue
	if _, err := s.Store.Put(ctx, collection, member.ID, encoded, store.AnyVersion); err != nil {
		return WeeklyChallenge{}, err
	}
	if err := s.pruneRoutes(ctx, member.ID, weekStartValue, challenge, effective); err != nil {
		return WeeklyChallenge{}, err
	}

	// Sharing may have gone down, and its enforcement already run, between the reads above and the write.
	current, err := s.currentSharing(ctx, member.ID)
	if isNotFound(err) {
		return WeeklyChallenge{}, errors.Join(err, s.eraseWeeklyChallenge(ctx, collection, member.ID, weekStartValue))
	}
	if err != nil {
		return WeeklyChallenge{}, err
	}
	if current.Covers(effective) {
		return challenge, nil
	}
	if err := s.restripStored(ctx, collection, member.ID, current, weekStart); err != nil {
		return WeeklyChallenge{}, err
	}
	if challenge, err = s.challengeFrom(document, current, weekStart); err != nil {
		return WeeklyChallenge{}, err
	}
	challenge.UpdatedAt = &updatedAt
	return challenge, nil
}

// currentSharing re-reads a member and the board and returns the member's effective sharing now.
func (s *Service) currentSharing(ctx context.Context, memberID string) (sharing.Sharing, error) {
	board, err := s.Board(ctx)
	if err != nil {
		return sharing.Sharing{}, err
	}
	member, err := s.Member(ctx, memberID)
	if err != nil {
		return sharing.Sharing{}, err
	}
	return Effective(member, board), nil
}

// eraseWeeklyChallenge deletes one member's stored weekly challenge and its routes.
func (s *Service) eraseWeeklyChallenge(ctx context.Context, collection, memberID, weekStartValue string) error {
	if err := s.Store.Delete(ctx, collection, memberID, store.AnyVersion); err != nil {
		return err
	}
	return s.Store.DeleteCollection(ctx, routeCollection(memberID, weekStartValue))
}

// MemberWeeklyChallenge returns an active member's weekly challenge, stripped to their sharing as it is now.
func (s *Service) MemberWeeklyChallenge(ctx context.Context, id, weekStartValue string) (MemberWeeklyChallenge, error) {
	board, err := s.Board(ctx)
	if err != nil {
		return MemberWeeklyChallenge{}, err
	}
	weekStart, failure := ParseWeekStart(board, weekStartValue, s.Now())
	if failure != nil {
		return MemberWeeklyChallenge{}, failure
	}
	member, err := s.activeMember(ctx, id)
	if err != nil {
		return MemberWeeklyChallenge{}, err
	}
	challenge, err := s.storedChallenge(ctx, member, board, weekStartValue, weekStart)
	if err != nil {
		return MemberWeeklyChallenge{}, err
	}
	if challenge == nil {
		return MemberWeeklyChallenge{}, problem.New(problem.NotFound, "no weekly challenge")
	}
	return MemberWeeklyChallenge{Member: summary(member, board), Challenge: *challenge}, nil
}

// Scoreboard returns every active member and a summary of their weekly challenge, in the board's ranking order.
func (s *Service) Scoreboard(ctx context.Context, weekStartValue string) (Scoreboard, error) {
	board, err := s.Board(ctx)
	if err != nil {
		return Scoreboard{}, err
	}
	weekStart, failure := ParseWeekStart(board, weekStartValue, s.Now())
	if failure != nil {
		return Scoreboard{}, failure
	}
	memberItems, err := s.Store.List(ctx, membersCollection)
	if err != nil {
		return Scoreboard{}, err
	}
	challengeItems, err := s.Store.List(ctx, weeksPrefix+weekStartValue)
	if err != nil {
		return Scoreboard{}, err
	}
	challengesByMember := map[string][]byte{}
	for _, item := range challengeItems {
		challengesByMember[item.ID] = item.Data
	}

	result := Scoreboard{WeekStart: weekStartValue, Ranking: board.Ranking, Entries: []ScoreboardEntry{}}
	for _, item := range memberItems {
		var member Member
		if err := json.Unmarshal(item.Data, &member); err != nil {
			return Scoreboard{}, err
		}
		if member.Status != StatusActive {
			continue
		}
		entry := ScoreboardEntry{Member: summary(member, board)}
		if data, found := challengesByMember[member.ID]; found {
			challenge, err := s.restrip(data, Effective(member, board), weekStart)
			if err == nil {
				entry.Challenge = challengeSummary(challenge)
			} else {
				s.logger().Error("skipping a stored weekly challenge that cannot be read", "weekStart", weekStartValue, "error", err)
			}
		}
		result.Entries = append(result.Entries, entry)
	}
	rank(result.Entries, board.Ranking)
	return result, nil
}

// PutRoute stores one activity's route, which must belong to an activity in the member's stored weekly challenge.
func (s *Service) PutRoute(ctx context.Context, member Member, weekStartValue, activityID string, body []byte) error {
	board, err := s.Board(ctx)
	if err != nil {
		return err
	}
	if !Effective(member, board).Routes {
		return problem.New(problem.RoutesNotShared, "")
	}
	weekStart, failure := ParseWeekStart(board, weekStartValue, s.Now())
	if failure != nil {
		return failure
	}
	challenge, err := s.storedChallenge(ctx, member, board, weekStartValue, weekStart)
	if err != nil {
		return err
	}
	if challenge == nil || !hasActivity(challenge, activityID) {
		return problem.New(problem.NotFound, "no such activity in that weekly challenge")
	}
	var route Route
	if err := decodeKnown(body, &route); err != nil {
		return problem.New(problem.BadRequest, err.Error())
	}
	points, err := polyline.Decode(route.Polyline)
	if err != nil {
		return problem.New(problem.BadRequest, err.Error())
	}
	if len(points) < 2 {
		return problem.New(problem.BadRequest, "a route needs at least two points")
	}
	encoded, err := json.Marshal(route)
	if err != nil {
		return err
	}
	encodedChallenge, err := json.Marshal(challenge)
	if err != nil {
		return err
	}
	collection := routeCollection(member.ID, weekStartValue)
	defer s.writes.lock(member.ID)()
	total, err := s.routeTotal(ctx, collection, activityID, len(encodedChallenge)+len(encoded))
	if err != nil {
		return err
	}
	if total > MaximumWeeklyChallengeBytes {
		return challengeFull()
	}
	if _, err := s.Store.Put(ctx, collection, activityID, encoded, store.AnyVersion); err != nil {
		return err
	}
	// Another instance may have stored a route meanwhile; measure again and withdraw this one if the challenge is over.
	if total, err = s.routeTotal(ctx, collection, activityID, len(encodedChallenge)+len(encoded)); err != nil {
		return err
	}
	if total > MaximumWeeklyChallengeBytes {
		return errors.Join(challengeFull(), s.Store.Delete(ctx, collection, activityID, store.AnyVersion))
	}
	// Routes may have gone off, or the member left, with its deletion already run, since the check above.
	current, err := s.currentSharing(ctx, member.ID)
	if err != nil && !isNotFound(err) {
		return err
	}
	if err == nil && current.Routes {
		return nil
	}
	if err == nil {
		err = problem.New(problem.RoutesNotShared, "")
	}
	return errors.Join(err, s.Store.DeleteCollection(ctx, collection))
}

// DeleteRoute removes one activity's route.
func (s *Service) DeleteRoute(ctx context.Context, member Member, weekStartValue, activityID string) error {
	board, err := s.Board(ctx)
	if err != nil {
		return err
	}
	if _, failure := ParseWeekStart(board, weekStartValue, s.Now()); failure != nil {
		return failure
	}
	return s.Store.Delete(ctx, routeCollection(member.ID, weekStartValue), activityID, store.AnyVersion)
}

// Route returns one activity's route, or notFound when the member does not share routes.
func (s *Service) Route(ctx context.Context, memberID, weekStartValue, activityID string) (Route, error) {
	board, err := s.Board(ctx)
	if err != nil {
		return Route{}, err
	}
	if _, failure := ParseWeekStart(board, weekStartValue, s.Now()); failure != nil {
		return Route{}, failure
	}
	member, err := s.activeMember(ctx, memberID)
	if err != nil {
		return Route{}, err
	}
	if !Effective(member, board).Routes {
		return Route{}, problem.New(problem.NotFound, "no route")
	}
	var route Route
	err = s.read(ctx, routeCollection(memberID, weekStartValue), activityID, &route)
	if errors.Is(err, store.ErrNotFound) {
		return Route{}, problem.New(problem.NotFound, "no route")
	}
	return route, err
}

// EnforceSharing strips every weekly challenge the board keeps for a member to their effective sharing, and deletes
// their routes when it has none. It runs whenever effective sharing goes down.
func (s *Service) EnforceSharing(ctx context.Context, member Member, board Board) error {
	effective := Effective(member, board)
	challengeCollections, err := s.Store.Collections(ctx, weeksPrefix)
	if err != nil {
		return err
	}
	for _, collection := range challengeCollections {
		weekStartValue := strings.TrimPrefix(collection, weeksPrefix)
		weekStart, err := time.Parse(dateLayout, weekStartValue)
		if err != nil {
			continue
		}
		if err := s.restripStored(ctx, collection, member.ID, effective, weekStart); err != nil {
			return err
		}
	}
	return nil
}

// restripStored strips one stored weekly challenge to effective and writes it back, trying again when an upload
// lands in between, and prunes its routes.
func (s *Service) restripStored(ctx context.Context, collection, memberID string, effective sharing.Sharing, weekStart time.Time) error {
	const attempts = 5
	for range attempts {
		document, err := s.Store.Get(ctx, collection, memberID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := decodeObject(document.Data); err != nil {
			// A weekly challenge that is not a JSON object is never served and cannot be stripped, so it goes.
			s.logger().Error("deleting a stored weekly challenge that cannot be read", "collection", collection, "error", err)
			err = s.Store.Delete(ctx, collection, memberID, document.Version)
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			if err != nil {
				return err
			}
			return s.Store.DeleteCollection(ctx, routeCollection(memberID, weekStart.Format(dateLayout)))
		}
		challenge, err := s.restrip(document.Data, effective, weekStart)
		if err != nil {
			// One that reads but no longer validates is kept, since every read path re-strips it before serving.
			s.logger().Error("keeping a stored weekly challenge that no longer validates", "collection", collection, "error", err)
			if !effective.Routes {
				return s.Store.DeleteCollection(ctx, routeCollection(memberID, weekStart.Format(dateLayout)))
			}
			return nil
		}
		encoded, err := json.Marshal(challenge)
		if err != nil {
			return err
		}
		_, err = s.Store.Put(ctx, collection, memberID, encoded, document.Version)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			return err
		}
		return s.pruneRoutes(ctx, memberID, weekStart.Format(dateLayout), challenge, effective)
	}
	return store.ErrConflict
}

// SweepRetention deletes every weekly challenge, and every route in it, older than the board keeps.
func (s *Service) SweepRetention(ctx context.Context) error {
	board, err := s.Board(ctx)
	if err != nil {
		return err
	}
	oldest := OldestKeptWeek(board, s.Now())
	isExpired := func(collection, prefix string) bool {
		weekStart, err := time.Parse(dateLayout, strings.TrimPrefix(collection, prefix))
		return err == nil && weekStart.Before(oldest)
	}

	challengeCollections, err := s.Store.Collections(ctx, weeksPrefix)
	if err != nil {
		return err
	}
	for _, collection := range challengeCollections {
		if isExpired(collection, weeksPrefix) {
			if err := s.Store.DeleteCollection(ctx, collection); err != nil {
				return err
			}
		}
	}
	memberRoutes, err := s.Store.Collections(ctx, routesPrefix)
	if err != nil {
		return err
	}
	for _, memberCollection := range memberRoutes {
		prefix := memberCollection + "/"
		routeCollections, err := s.Store.Collections(ctx, prefix)
		if err != nil {
			return err
		}
		for _, collection := range routeCollections {
			if isExpired(collection, prefix) {
				if err := s.Store.DeleteCollection(ctx, collection); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// EraseHistory deletes every member's weekly challenges and routes, which a change of the board's weekly challenge
// start day leaves on days the board no longer reads.
func (s *Service) EraseHistory(ctx context.Context) error {
	challengeCollections, err := s.Store.Collections(ctx, weeksPrefix)
	if err != nil {
		return err
	}
	memberRoutes, err := s.Store.Collections(ctx, routesPrefix)
	if err != nil {
		return err
	}
	for _, memberCollection := range memberRoutes {
		routeCollections, err := s.Store.Collections(ctx, memberCollection+"/")
		if err != nil {
			return err
		}
		challengeCollections = append(challengeCollections, routeCollections...)
	}
	for _, collection := range challengeCollections {
		if err := s.Store.DeleteCollection(ctx, collection); err != nil {
			return err
		}
	}
	return nil
}

// activeMember returns an active member, or a notFound problem for a pending or missing one.
func (s *Service) activeMember(ctx context.Context, id string) (Member, error) {
	member, err := s.Member(ctx, id)
	if err != nil {
		return Member{}, err
	}
	if member.Status != StatusActive {
		return Member{}, problem.New(problem.NotFound, "no such member")
	}
	return member, nil
}

// storedChallenge returns a member's stored weekly challenge stripped to their sharing now, or nil when there is none.
func (s *Service) storedChallenge(ctx context.Context, member Member, board Board, weekStartValue string, weekStart time.Time) (*WeeklyChallenge, error) {
	document, err := s.Store.Get(ctx, weeksPrefix+weekStartValue, member.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	challenge, err := s.restrip(document.Data, Effective(member, board), weekStart)
	if err != nil {
		s.logger().Error("skipping a stored weekly challenge that cannot be read", "weekStart", weekStartValue, "error", err)
		return nil, nil
	}
	return &challenge, nil
}

// challengeFrom strips a decoded weekly challenge to sharing and decodes what is left into a validated WeeklyChallenge.
func (s *Service) challengeFrom(document map[string]any, effective sharing.Sharing, weekStart time.Time) (WeeklyChallenge, error) {
	stripped := s.Allowlist.Strip(document, effective)
	if err := requireMembers(stripped); err != nil {
		return WeeklyChallenge{}, err
	}
	encoded, err := json.Marshal(stripped)
	if err != nil {
		return WeeklyChallenge{}, err
	}
	var challenge WeeklyChallenge
	if err := decodeStrict(encoded, &challenge); err != nil {
		return WeeklyChallenge{}, err
	}
	if err := validateWeeklyChallenge(&challenge, weekStart); err != nil {
		return WeeklyChallenge{}, err
	}
	return challenge, nil
}

// restrip passes a stored weekly challenge through the filter again, so nothing above sharing as it is now is ever
// served, even before EnforceSharing has caught up.
func (s *Service) restrip(data []byte, effective sharing.Sharing, weekStart time.Time) (WeeklyChallenge, error) {
	document, err := decodeObject(data)
	if err != nil {
		return WeeklyChallenge{}, fmt.Errorf("scoreboard: stored weekly challenge: %w", err)
	}
	updatedAt := document["updatedAt"]
	challenge, err := s.challengeFrom(document, effective, weekStart)
	if err != nil {
		return WeeklyChallenge{}, fmt.Errorf("scoreboard: stored weekly challenge: %w", err)
	}
	if text, isText := updatedAt.(string); isText && challenge.UpdatedAt == nil {
		if parsed, err := time.Parse(time.RFC3339, text); err == nil {
			challenge.UpdatedAt = &parsed
		}
	}
	return challenge, nil
}

// pruneRoutes deletes the routes of a weekly challenge that are no longer allowed or no longer have an activity.
func (s *Service) pruneRoutes(ctx context.Context, memberID, weekStartValue string, challenge WeeklyChallenge, effective sharing.Sharing) error {
	collection := routeCollection(memberID, weekStartValue)
	if !effective.Routes || challenge.Activities == nil {
		return s.Store.DeleteCollection(ctx, collection)
	}
	routes, err := s.Store.List(ctx, collection)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if !hasActivity(&challenge, route.ID) {
			if err := s.Store.Delete(ctx, collection, route.ID, store.AnyVersion); err != nil {
				return err
			}
		}
	}
	return nil
}

// keptRouteBytes is the size of the stored routes a new weekly challenge keeps: those of its activities, when shared.
func (s *Service) keptRouteBytes(ctx context.Context, memberID, weekStartValue string, challenge WeeklyChallenge,
	effective sharing.Sharing) (int, error) {
	if !effective.Routes || challenge.Activities == nil {
		return 0, nil
	}
	routes, err := s.Store.List(ctx, routeCollection(memberID, weekStartValue))
	if err != nil {
		return 0, err
	}
	total := 0
	for _, route := range routes {
		if hasActivity(&challenge, route.ID) {
			total += len(route.Data)
		}
	}
	return total, nil
}

// routeTotal is what a weekly challenge would store with a route of routeBytes for activityID beside its other routes.
func (s *Service) routeTotal(ctx context.Context, collection, activityID string, routeBytes int) (int, error) {
	stored, err := s.Store.List(ctx, collection)
	if err != nil {
		return 0, err
	}
	total := routeBytes
	for _, other := range stored {
		if other.ID != activityID {
			total += len(other.Data)
		}
	}
	return total, nil
}

// noSuchRequest is the problem for a member who is not waiting for approval.
func noSuchRequest() error {
	return problem.New(problem.NotFound, "no such request")
}

// challengeFull is the problem for an upload that would take a weekly challenge past MaximumWeeklyChallengeBytes.
func challengeFull() error {
	return problem.New(problem.PayloadTooLarge, "the weekly challenge and its routes would store more than 1 MB")
}

// updateMember applies change to a stored member, or returns a notFound problem when there is none.
func (s *Service) updateMember(ctx context.Context, id string, change func(*Member) error) error {
	return store.Update(ctx, s.Store, membersCollection, id, func(data []byte) ([]byte, error) {
		if data == nil {
			return nil, problem.New(problem.NotFound, "no such member")
		}
		var member Member
		if err := json.Unmarshal(data, &member); err != nil {
			return nil, err
		}
		if err := change(&member); err != nil {
			return nil, err
		}
		return json.Marshal(member)
	})
}

// logger is Logger, or slog.Default when it is nil.
func (s *Service) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// read decodes a stored document into value, or returns store.ErrNotFound.
func (s *Service) read(ctx context.Context, collection, id string, value any) error {
	document, err := s.Store.Get(ctx, collection, id)
	if err != nil {
		return err
	}
	return json.Unmarshal(document.Data, value)
}

// routeCollection is the collection of a member's routes for one weekly challenge.
func routeCollection(memberID, weekStartValue string) string {
	return routesPrefix + memberID + "/" + weekStartValue
}

// hasActivity reports whether challenge has an activity with activityID.
func hasActivity(challenge *WeeklyChallenge, activityID string) bool {
	if challenge.Activities == nil {
		return false
	}
	for _, activity := range *challenge.Activities {
		if activity.ID == activityID {
			return true
		}
	}
	return false
}

// summary is member as the board shows them, with avatars and sharing capped by board.
func summary(member Member, board Board) MemberSummary {
	return MemberSummary{
		ID:          member.ID,
		DisplayName: member.DisplayName,
		HasAvatar:   member.HasAvatar && board.AvatarsAllowed,
		Sharing:     Effective(member, board),
	}
}

// challengeSummary is challenge as the scoreboard shows it, with an activity count in place of activities.
func challengeSummary(challenge WeeklyChallenge) *WeeklyChallengeSummary {
	result := &WeeklyChallengeSummary{Score: challenge.Score, Goal: challenge.Goal, Tier: challenge.Tier, Breakdown: challenge.Breakdown}
	if challenge.Progress != nil {
		result.Progress = *challenge.Progress
	}
	if challenge.UpdatedAt != nil {
		result.UpdatedAt = *challenge.UpdatedAt
	}
	if challenge.Activities != nil {
		count := len(*challenge.Activities)
		result.ActivityCount = &count
	}
	return result
}

// rank orders a scoreboard: by progress or by name, with members who have no weekly challenge last, by name.
func rank(entries []ScoreboardEntry, ranking string) {
	byName := func(first, second ScoreboardEntry) bool {
		firstName, secondName := strings.ToLower(first.Member.DisplayName), strings.ToLower(second.Member.DisplayName)
		if firstName != secondName {
			return firstName < secondName
		}
		return first.Member.ID < second.Member.ID
	}
	sort.SliceStable(entries, func(firstIndex, secondIndex int) bool {
		first, second := entries[firstIndex], entries[secondIndex]
		if (first.Challenge == nil) != (second.Challenge == nil) {
			return first.Challenge != nil
		}
		if ranking == RankByProgress && first.Challenge != nil && first.Challenge.Progress != second.Challenge.Progress {
			return first.Challenge.Progress > second.Challenge.Progress
		}
		return byName(first, second)
	})
}

// isNotFound reports whether err is a notFound problem.
func isNotFound(err error) bool {
	var failure *problem.Problem
	return errors.As(err, &failure) && failure.Code == problem.NotFound
}

// decodeObject decodes a JSON object, keeping numbers exactly as written.
func decodeObject(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("the body is not a JSON object")
	}
	if document == nil {
		return nil, errors.New("the body is not a JSON object")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("the body has data after its JSON object")
	}
	return document, nil
}

// requireMembers checks that every object the schema marks required members on, and that a stripped weekly challenge
// still carries, has them: a breakdown's parts, an activity's points, a strength's exercises and an exercise's sets.
func requireMembers(document map[string]any) error {
	if breakdown, ok := document["breakdown"].(map[string]any); ok {
		for _, key := range []string{"exercise", "dailyBonus", "varietyBonus"} {
			if _, present := breakdown[key]; !present {
				return fmt.Errorf("breakdown.%s is required", key)
			}
		}
	}
	activities, _ := document["activities"].([]any)
	for index, element := range activities {
		activity, _ := element.(map[string]any)
		where := fmt.Sprintf("activities[%d]", index)
		if points, ok := activity["points"].(map[string]any); ok {
			for _, key := range []string{"base", "bonus"} {
				if _, present := points[key]; !present {
					return fmt.Errorf("%s.points.%s is required", where, key)
				}
			}
		}
		strength, ok := activity["strength"].(map[string]any)
		if !ok {
			continue
		}
		exercises, present := strength["exercises"]
		if !present {
			return fmt.Errorf("%s.strength.exercises is required", where)
		}
		list, _ := exercises.([]any)
		for exerciseIndex, element := range list {
			exercise, _ := element.(map[string]any)
			if _, present := exercise["sets"]; !present {
				return fmt.Errorf("%s.strength.exercises[%d].sets is required", where, exerciseIndex)
			}
		}
	}
	return nil
}

// checkShape reports a field of a decoded weekly challenge whose value has the wrong JSON type for the schema.
func checkShape(document map[string]any) error {
	encoded, err := json.Marshal(document)
	if err != nil {
		return err
	}
	return decodeKnown(encoded, &WeeklyChallenge{})
}

// decodeKnown decodes a JSON body into value, ignoring unknown fields as the API's conventions ask.
func decodeKnown(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("the body does not match the schema: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("the body has data after its JSON object")
	}
	return nil
}

// decodeStrict decodes JSON into value, refusing fields value does not have and trailing data.
func decodeStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("the body does not match the schema: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("the body has data after its JSON object")
	}
	return nil
}
