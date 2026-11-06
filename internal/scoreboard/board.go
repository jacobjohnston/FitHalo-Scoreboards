package scoreboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/avatar"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/password"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/problem"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// boardAvatarLifetime is how long one instance serves the board avatar from memory before reading it again.
const boardAvatarLifetime = time.Minute

// The board limits the admin portal enforces.
const (
	// MaximumBoardName is the most characters in a board's name.
	MaximumBoardName = 60

	// MaximumDescription is the most characters in a board's description.
	MaximumDescription = 500

	// MaximumLandingMessage is the most characters in a board's landing message.
	MaximumLandingMessage = 140

	// MinimumRetentionWeeks is the fewest weeks a board may keep.
	MinimumRetentionWeeks = 1

	// MaximumRetentionWeeks is the most weeks a board may keep, about ten years.
	MaximumRetentionWeeks = 520

	// MinimumPasswordLength is the fewest characters in a board's password.
	MinimumPasswordLength = 8

	// MaximumPasswordLength is the most characters in a board's password.
	MaximumPasswordLength = 256

	// DefaultMaximumPending is how many join requests may wait for approval on a new board.
	DefaultMaximumPending = 10

	// MinimumPending is the lowest pending limit an admin may set; zero would close the board silently.
	MinimumPending = 1

	// MaximumPendingLimit is the highest pending limit an admin may set, as many as one could read.
	MaximumPendingLimit = 999

	// MaximumMembers is the most members, waiting and active together, a board keeps.
	MaximumMembers = 1000
)

// BlockedMember is a member the admin deleted and blocked from joining again.
type BlockedMember struct {
	// ID is the blocked member's identifier.
	ID string

	// BlockedAt is when the admin blocked them.
	BlockedAt time.Time
}

// PendingLimit is how many join requests may wait for approval, with the default for a board stored
// before the limit existed.
func (b Board) PendingLimit() int {
	if b.MaximumPending < MinimumPending || b.MaximumPending > MaximumPendingLimit {
		return DefaultMaximumPending
	}
	return b.MaximumPending
}

// PendingCount is how many join requests are waiting for approval.
func (s *Service) PendingCount(ctx context.Context) (int, error) {
	members, err := s.Members(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, member := range members {
		if member.Status == StatusPending {
			count++
		}
	}
	return count, nil
}

// MemberCount is how many members the board has, waiting and active together.
func (s *Service) MemberCount(ctx context.Context) (int, error) {
	items, err := s.Store.List(ctx, membersCollection)
	return len(items), err
}

// AcceptingRequests reports whether a new member could join now: while the board has fewer than
// MaximumMembers, and on one that approves members only while fewer requests are waiting than its limit.
func (s *Service) AcceptingRequests(ctx context.Context, board Board) (bool, error) {
	if count, err := s.MemberCount(ctx); err != nil || count >= MaximumMembers {
		return false, err
	}
	if board.JoinMode != JoinApproval {
		return true, nil
	}
	count, err := s.PendingCount(ctx)
	return count < board.PendingLimit(), err
}

// IsWeekStartDay reports whether day is a weekday the board's weeks can start on.
func IsWeekStartDay(day string) bool {
	_, known := weekdays[day]
	return known
}

// UpdateBoard applies change to the board. When the change lowers the board's maximum sharing, every
// member's stored weekly challenges are stripped to it before returning; when it turns avatars off, every avatar is
// deleted; when it shortens retention, the weekly challenges no longer kept are deleted.
func (s *Service) UpdateBoard(ctx context.Context, change func(*Board) error) (Board, error) {
	var before, after Board
	err := store.Update(ctx, s.Store, boardCollection, boardID, func(data []byte) ([]byte, error) {
		if data == nil {
			return nil, store.ErrNotFound
		}
		var board Board
		if err := json.Unmarshal(data, &board); err != nil {
			return nil, err
		}
		if err := board.checkStorage(); err != nil {
			return nil, err
		}
		before = board
		if err := change(&board); err != nil {
			return nil, err
		}
		after = board
		return json.Marshal(board)
	})
	if err != nil {
		return Board{}, err
	}
	if !after.MaximumSharing.Covers(before.MaximumSharing) {
		if err := s.EnforceAll(ctx, after); err != nil {
			return Board{}, err
		}
	}
	if before.AvatarsAllowed && !after.AvatarsAllowed {
		if err := s.RemoveAvatars(ctx); err != nil {
			return Board{}, err
		}
	}
	if after.RetentionWeeks < before.RetentionWeeks {
		if err := s.SweepRetention(ctx); err != nil {
			return Board{}, err
		}
	}
	return after, nil
}

// SetBoardPassword sets the password new members need, or removes it when newPassword is empty.
func (s *Service) SetBoardPassword(ctx context.Context, newPassword string) error {
	hash := ""
	if newPassword != "" {
		if err := ValidatePassword(newPassword); err != nil {
			return err
		}
		var err error
		if hash, err = password.Hash(newPassword, s.Pepper); err != nil {
			return err
		}
	}
	_, err := s.UpdateBoard(ctx, func(board *Board) error {
		board.PasswordHash = hash
		return nil
	})
	return err
}

// SetBoardAvatar stores the board's own avatar, re-encoded, whether or not members may have avatars.
func (s *Service) SetBoardAvatar(ctx context.Context, source io.ReadSeeker, contentType string) error {
	defer s.forgetBoardAvatar()
	normalized, err := avatar.NormalizeFrom(source, contentType)
	if errors.Is(err, avatar.ErrInvalid) {
		return problem.New(problem.ImageInvalid, "")
	}
	if err != nil {
		return err
	}
	if _, err := s.Store.Put(ctx, boardCollection, boardAvatarID, normalized, store.AnyVersion); err != nil {
		return err
	}
	_, err = s.UpdateBoard(ctx, func(board *Board) error {
		board.HasAvatar = true
		return nil
	})
	return err
}

// RemoveBoardAvatar deletes the board's own avatar.
func (s *Service) RemoveBoardAvatar(ctx context.Context) error {
	defer s.forgetBoardAvatar()
	if _, err := s.UpdateBoard(ctx, func(board *Board) error {
		board.HasAvatar = false
		return nil
	}); err != nil {
		return err
	}
	if err := s.Store.Delete(ctx, boardCollection, boardAvatarID, store.AnyVersion); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// BoardAvatar returns the board's own avatar as JPEG, or notFound when it has none.
func (s *Service) BoardAvatar(ctx context.Context) ([]byte, error) {
	board, err := s.Board(ctx)
	if err != nil {
		return nil, err
	}
	if !board.HasAvatar {
		return nil, problem.New(problem.NotFound, "no avatar")
	}
	s.boardAvatarMutex.Lock()
	defer s.boardAvatarMutex.Unlock()
	if s.boardAvatar != nil && s.Now().Sub(s.boardAvatarFetched) < boardAvatarLifetime {
		return s.boardAvatar, nil
	}
	document, err := s.Store.Get(ctx, boardCollection, boardAvatarID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, problem.New(problem.NotFound, "no avatar")
	}
	if err != nil {
		return nil, err
	}
	s.boardAvatar, s.boardAvatarFetched = document.Data, s.Now()
	return document.Data, nil
}

// forgetBoardAvatar drops the cached board avatar, so the next request reads the stored one.
func (s *Service) forgetBoardAvatar() {
	s.boardAvatarMutex.Lock()
	defer s.boardAvatarMutex.Unlock()
	s.boardAvatar = nil
}

// ReplaceInvite gives the board a new invite code, which retires every link, QR code and poster made
// from the old one.
func (s *Service) ReplaceInvite(ctx context.Context) (Board, error) {
	code, err := NewIdentifier()
	if err != nil {
		return Board{}, err
	}
	return s.UpdateBoard(ctx, func(board *Board) error {
		board.InviteCode = code
		return nil
	})
}

// Members returns every member, pending and active, by name.
func (s *Service) Members(ctx context.Context) ([]Member, error) {
	items, err := s.Store.List(ctx, membersCollection)
	if err != nil {
		return nil, err
	}
	members := make([]Member, 0, len(items))
	for _, item := range items {
		var member Member
		if err := json.Unmarshal(item.Data, &member); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	sort.SliceStable(members, func(first, second int) bool {
		return strings.ToLower(members[first].DisplayName) < strings.ToLower(members[second].DisplayName)
	})
	return members, nil
}

// Blocked returns every blocked member, most recently blocked first.
func (s *Service) Blocked(ctx context.Context) ([]BlockedMember, error) {
	items, err := s.Store.List(ctx, blockedCollection)
	if err != nil {
		return nil, err
	}
	result := make([]BlockedMember, 0, len(items))
	for _, item := range items {
		var record blocked
		if err := json.Unmarshal(item.Data, &record); err != nil {
			return nil, err
		}
		result = append(result, BlockedMember{ID: item.ID, BlockedAt: record.BlockedAt})
	}
	sort.SliceStable(result, func(first, second int) bool {
		return result[first].BlockedAt.After(result[second].BlockedAt)
	})
	return result, nil
}

// Unblock lets a blocked member join again.
func (s *Service) Unblock(ctx context.Context, id string) error {
	return s.Store.Delete(ctx, blockedCollection, id, store.AnyVersion)
}

// EnforceAll strips every member's stored weekly challenges to their effective sharing on board.
func (s *Service) EnforceAll(ctx context.Context, board Board) error {
	members, err := s.Members(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, member := range members {
		failures = append(failures, s.EnforceSharing(ctx, member, board))
	}
	return errors.Join(failures...)
}

// RemoveAvatars deletes every stored avatar and clears each member's HasAvatar.
func (s *Service) RemoveAvatars(ctx context.Context) error {
	members, err := s.Members(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, member := range members {
		if !member.HasAvatar {
			continue
		}
		if err := s.DeleteAvatar(ctx, member.ID); err != nil && !isNotFound(err) {
			failures = append(failures, err)
		}
	}
	if err := errors.Join(failures...); err != nil {
		return err
	}
	return s.Store.DeleteCollection(ctx, avatarsCollection)
}

// ValidatePassword checks a password's length.
func ValidatePassword(value string) error {
	if length := len([]rune(value)); length < MinimumPasswordLength || length > MaximumPasswordLength {
		return errors.New("password length")
	}
	return nil
}

// WeeklyChallengeUsage is what one member stores for one weekly challenge, counted before compression.
type WeeklyChallengeUsage struct {
	// WeekStart is the week's first day, as YYYY-MM-DD.
	WeekStart string

	// ChallengeBytes is the size of the stored weekly challenge, or zero when only routes are stored.
	ChallengeBytes int

	// Routes is how many routes the weekly challenge stores.
	Routes int

	// RouteBytes is the size of the weekly challenge's routes together.
	RouteBytes int
}

// Total is the weekly challenge's bytes and its routes' together, which MaximumWeeklyChallengeBytes caps.
func (w WeeklyChallengeUsage) Total() int {
	return w.ChallengeBytes + w.RouteBytes
}

// Usage is everything one member stores, counted before compression.
type Usage struct {
	// Challenges is each weekly challenge with stored data or a route, newest first.
	Challenges []WeeklyChallengeUsage

	// AvatarBytes is the size of the member's avatar, or zero when there is none.
	AvatarBytes int
}

// Total is every weekly challenge's bytes and the avatar's together.
func (u Usage) Total() int {
	total := u.AvatarBytes
	for _, challenge := range u.Challenges {
		total += challenge.Total()
	}
	return total
}

// Usage returns what a member stores, for the admin. It reads every weekly challenge the board keeps, so it is for
// an occasional look rather than every request.
func (s *Service) Usage(ctx context.Context, memberID string) (Usage, error) {
	usages := map[string]*WeeklyChallengeUsage{}
	usageFor := func(weekStart string) *WeeklyChallengeUsage {
		if usages[weekStart] == nil {
			usages[weekStart] = &WeeklyChallengeUsage{WeekStart: weekStart}
		}
		return usages[weekStart]
	}
	challengeCollections, err := s.Store.Collections(ctx, weeksPrefix)
	if err != nil {
		return Usage{}, err
	}
	for _, collection := range challengeCollections {
		document, err := s.Store.Get(ctx, collection, memberID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return Usage{}, err
		}
		usageFor(strings.TrimPrefix(collection, weeksPrefix)).ChallengeBytes = len(document.Data)
	}
	memberRoutes := routesPrefix + memberID + "/"
	routeCollections, err := s.Store.Collections(ctx, memberRoutes)
	if err != nil {
		return Usage{}, err
	}
	for _, collection := range routeCollections {
		routes, err := s.Store.List(ctx, collection)
		if err != nil {
			return Usage{}, err
		}
		usage := usageFor(strings.TrimPrefix(collection, memberRoutes))
		for _, route := range routes {
			usage.Routes++
			usage.RouteBytes += len(route.Data)
		}
	}

	var result Usage
	for _, usage := range usages {
		if usage.Total() > 0 {
			result.Challenges = append(result.Challenges, *usage)
		}
	}
	sort.Slice(result.Challenges, func(first, second int) bool {
		return result.Challenges[first].WeekStart > result.Challenges[second].WeekStart
	})
	avatar, err := s.Store.Get(ctx, avatarsCollection, memberID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Usage{}, err
	}
	result.AvatarBytes = len(avatar.Data)
	return result, nil
}
