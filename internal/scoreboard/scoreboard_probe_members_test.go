package scoreboard_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"strings"
	"testing"
	"time"

	_ "image/jpeg"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/password"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/problem"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// joinWith joins who with a request the test shaped.
func (p *probe) joinWith(who identity, request scoreboard.JoinRequest) (scoreboard.Member, bool, error) {
	return p.service.Join(context.Background(), request, who.id)
}

// TestJoinReturnsExistingMembershipUnchanged gives a known key its membership back without applying the request.
func TestJoinReturnsExistingMembershipUnchanged(t *testing.T) {
	p := newProbe(t, nil)
	who := newIdentity(t)
	first, created, err := p.joinWith(who, p.joinRequest(who, "First", sharing.Lowest))
	if err != nil || !created {
		t.Fatalf("first join: created %t, %v", created, err)
	}
	again, created, err := p.joinWith(who, p.joinRequest(who, "Second", sharing.Highest))
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("a second join created a membership")
	}
	if again.DisplayName != "First" || again.Sharing != sharing.Lowest || again.ID != first.ID {
		t.Errorf("second join returned %+v, want the first membership", again)
	}
	if stored := p.member(who.id); stored.DisplayName != "First" || stored.Sharing != sharing.Lowest {
		t.Errorf("second join changed the stored membership to %+v", stored)
	}
	// An existing member gets in with the wrong invite, password or display name too.
	bad := p.joinRequest(who, "", sharing.Lowest)
	bad.InviteCode = "wrong"
	// PROBE: the contract says a known key gets its membership back; whether that happens before or after
	// the invite is checked is not written down. Recorded.
	_, _, err = p.joinWith(who, bad)
	t.Logf("existing member with a wrong invite and empty name: %v", err)
}

// TestJoinChecksInviteThenBlockThenPassword answers inviteInvalid, blocked and passwordIncorrect in that order.
func TestJoinChecksInviteThenBlockThenPassword(t *testing.T) {
	hash, err := password.Hash("correct horse battery", nil)
	if err != nil {
		t.Fatal(err)
	}
	p := newProbe(t, func(board *scoreboard.Board) { board.PasswordHash = hash })
	blocked := newIdentity(t)
	request := p.joinRequest(blocked, "Blocked", sharing.Lowest)
	request.Password = "correct horse battery"
	if _, _, err := p.joinWith(blocked, request); err != nil {
		t.Fatal(err)
	}
	if err := p.service.DeleteMember(context.Background(), blocked.id, true); err != nil {
		t.Fatal(err)
	}
	stranger := newIdentity(t)
	cases := []struct {
		// name says which checks the request fails.
		name string

		// who sends the request.
		who identity

		// invite is the invite code sent.
		invite string

		// password is the password sent.
		password string

		// want is the code expected.
		want problem.Code
	}{
		{name: "wrong invite, blocked, wrong password", who: blocked, invite: "wrong", password: "wrong", want: problem.InviteInvalid},
		{name: "wrong invite, blocked, right password", who: blocked, invite: "wrong", password: "correct horse battery", want: problem.InviteInvalid},
		{name: "right invite, blocked, wrong password", who: blocked, invite: probeInvite, password: "wrong", want: problem.Blocked},
		{name: "right invite, blocked, right password", who: blocked, invite: probeInvite, password: "correct horse battery", want: problem.Blocked},
		{name: "wrong invite, stranger, right password", who: stranger, invite: "wrong", password: "correct horse battery", want: problem.InviteInvalid},
		{name: "empty invite", who: stranger, invite: "", password: "correct horse battery", want: problem.InviteInvalid},
		{name: "invite with different case", who: stranger, invite: strings.ToLower(probeInvite), password: "correct horse battery", want: problem.InviteInvalid},
		{name: "invite with trailing space", who: stranger, invite: probeInvite + " ", password: "correct horse battery", want: problem.InviteInvalid},
		{name: "invite as prefix", who: stranger, invite: probeInvite[:len(probeInvite)-1], password: "correct horse battery", want: problem.InviteInvalid},
		{name: "missing password", who: stranger, invite: probeInvite, password: "", want: problem.PasswordIncorrect},
		{name: "wrong password", who: stranger, invite: probeInvite, password: "wrong", want: problem.PasswordIncorrect},
		{name: "password with trailing space", who: stranger, invite: probeInvite, password: "correct horse battery ", want: problem.PasswordIncorrect},
		{name: "password with different case", who: stranger, invite: probeInvite, password: "Correct Horse Battery", want: problem.PasswordIncorrect},
		{name: "password as prefix", who: stranger, invite: probeInvite, password: "correct horse", want: problem.PasswordIncorrect},
		{name: "everything right", who: stranger, invite: probeInvite, password: "correct horse battery", want: ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := p.joinRequest(test.who, "Someone", sharing.Lowest)
			request.InviteCode = test.invite
			request.Password = test.password
			_, _, err := p.joinWith(test.who, request)
			expectCode(t, err, test.want)
		})
	}
}

// TestPasswordWithTrailingNulIsWrong refuses a password that is the board's with NUL bytes appended: the contract
// says a wrong password is passwordIncorrect, and a different byte string is a different password.
func TestPasswordWithTrailingNulIsWrong(t *testing.T) {
	hash, err := password.Hash("correct horse battery", nil)
	if err != nil {
		t.Fatal(err)
	}
	p := newProbe(t, func(board *scoreboard.Board) { board.PasswordHash = hash })
	for _, wrong := range []string{"correct horse battery\x00", "correct horse battery\x00\x00"} {
		who := newIdentity(t)
		request := p.joinRequest(who, "Knock", sharing.Lowest)
		request.Password = wrong
		_, _, err := p.joinWith(who, request)
		expectCode(t, err, problem.PasswordIncorrect)
	}
}

// TestJoinValidatesNameAndSharing refuses a hidden name or an unknown level, and normalises the sharing it stores.
func TestJoinValidatesNameAndSharing(t *testing.T) {
	p := newProbe(t, nil)
	for _, name := range []string{"", "   ", "\u200b", "\u202eEve", strings.Repeat("x", 41), "Ev\x00e"} {
		who := newIdentity(t)
		_, _, err := p.joinWith(who, p.joinRequest(who, name, sharing.Lowest))
		expectCode(t, err, problem.BadRequest)
		if _, err := p.service.Member(context.Background(), who.id); codeOf(err) != problem.NotFound {
			t.Errorf("a refused join for %q left a membership: %v", name, err)
		}
	}
	for _, bad := range []sharing.Sharing{{Score: "loud"}, {Score: sharing.ScoreDetails, Activity: "all"}, {}, {Score: "PROGRESS", Activity: "none"}} {
		who := newIdentity(t)
		_, _, err := p.joinWith(who, p.joinRequest(who, "Eve", bad))
		expectCode(t, err, problem.BadRequest)
	}
	who := newIdentity(t)
	odd := sharing.Sharing{Score: sharing.ScoreProgress, Activity: sharing.ActivityFull, Routes: true, StrengthSets: true, StrengthWeights: true}
	member, _, err := p.joinWith(who, p.joinRequest(who, "  Eve  ", odd))
	if err != nil {
		t.Fatal(err)
	}
	if member.Sharing != odd.Normalized() {
		t.Errorf("stored sharing %s, want normalized %s", describeSharing(member.Sharing), describeSharing(odd.Normalized()))
	}
	if member.DisplayName != "Eve" {
		t.Errorf("stored name %q, want %q", member.DisplayName, "Eve")
	}
	if member.ID != who.id || member.PublicKey != who.publicKey || member.Status != scoreboard.StatusActive {
		t.Errorf("membership %+v does not match the identity", member)
	}
	if !member.JoinedAt.Equal(probeNow) || member.JoinedAt.Location() != time.UTC || member.JoinedAt.Nanosecond() != 0 {
		t.Errorf("joinedAt %v, want %v in UTC to the second", member.JoinedAt, probeNow)
	}
	if member.HasAvatar {
		t.Error("a new member has an avatar")
	}
}

// TestJoinRecordsMismatchedKey records what happens when the public key in the body is not the caller's.
func TestJoinRecordsMismatchedKey(t *testing.T) {
	p := newProbe(t, nil)
	caller, other := newIdentity(t), newIdentity(t)
	request := p.joinRequest(other, "Imposter", sharing.Lowest)
	member, created, err := p.joinWith(caller, request)
	// PROBE: the doc says the caller has already verified the signature; the service is trusted. Recorded:
	// which identifier and key the membership carries when they disagree.
	t.Logf("mismatched key: created=%t err=%v id=%s (caller %s, body %s) key matches body=%t", created, err, member.ID, caller.id, other.id, member.PublicKey == other.publicKey)
	for _, key := range []string{"", "not-a-key", strings.Repeat("A", 87), strings.Repeat("A", 86)} {
		who := newIdentity(t)
		request := p.joinRequest(who, "Keyless", sharing.Lowest)
		request.PublicKey = key
		_, _, err := p.joinWith(who, request)
		t.Logf("public key %.12q: %v", key, err)
	}
}

// TestApprovalBoardQueue keeps new members pending, counts them, and closes the queue at its limit.
func TestApprovalBoardQueue(t *testing.T) {
	p := newProbe(t, func(board *scoreboard.Board) {
		board.JoinMode = scoreboard.JoinApproval
		board.MaximumPending = 3
	})
	board := p.board()
	if board.PendingLimit() != 3 {
		t.Fatalf("PendingLimit %d, want 3", board.PendingLimit())
	}
	var pending []identity
	for index := range 3 {
		accepting, err := p.service.AcceptingRequests(context.Background(), board)
		if err != nil || !accepting {
			t.Errorf("before request %d: accepting %t, %v", index+1, accepting, err)
		}
		who := newIdentity(t)
		member, created, err := p.joinWith(who, p.joinRequest(who, "Waiting", sharing.Lowest))
		if err != nil || !created || member.Status != scoreboard.StatusPending {
			t.Fatalf("join %d: created %t, status %q, %v", index+1, created, member.Status, err)
		}
		pending = append(pending, who)
		if count, err := p.service.PendingCount(context.Background()); err != nil || count != index+1 {
			t.Errorf("pending count %d, want %d (%v)", count, index+1, err)
		}
	}
	if accepting, _ := p.service.AcceptingRequests(context.Background(), board); accepting {
		t.Error("accepting requests with the queue full")
	}
	stranger := newIdentity(t)
	_, _, err := p.joinWith(stranger, p.joinRequest(stranger, "Late", sharing.Lowest))
	expectCode(t, err, problem.PendingLimitReached)
	_, created, err := p.joinWith(pending[0], p.joinRequest(pending[0], "Again", sharing.Lowest))
	if err != nil || created {
		t.Errorf("a pending member re-joining at the limit: created %t, %v", created, err)
	}
	// Approving one reopens the queue; the approved member is no longer pending.
	if err := p.service.SetStatus(context.Background(), pending[0].id, scoreboard.StatusActive); err != nil {
		t.Fatal(err)
	}
	if count, _ := p.service.PendingCount(context.Background()); count != 2 {
		t.Errorf("pending count %d after approving one, want 2", count)
	}
	if accepting, _ := p.service.AcceptingRequests(context.Background(), board); !accepting {
		t.Error("not accepting after approving one")
	}
	if _, _, err := p.joinWith(stranger, p.joinRequest(stranger, "Late", sharing.Lowest)); err != nil {
		t.Errorf("join after approving one: %v", err)
	}
	// Declining (deleting) a pending member frees a slot too.
	if err := p.service.DeleteMember(context.Background(), pending[1].id, false); err != nil {
		t.Fatal(err)
	}
	if count, _ := p.service.PendingCount(context.Background()); count != 2 {
		t.Errorf("pending count %d after declining one, want 2", count)
	}
	// An open board accepts whatever waits.
	open := newProbe(t, nil)
	if accepting, err := open.service.AcceptingRequests(context.Background(), open.board()); err != nil || !accepting {
		t.Errorf("open board accepting %t, %v", accepting, err)
	}
}

// TestPendingMemberIsInvisible keeps a pending member off the scoreboard and their weekly challenge unreadable until approved.
func TestPendingMemberIsInvisible(t *testing.T) {
	p := newProbe(t, func(board *scoreboard.Board) { board.JoinMode = scoreboard.JoinApproval })
	waiting := p.join("Waiting", sharing.Highest)
	active := p.join("Active", sharing.Highest)
	if err := p.service.SetStatus(context.Background(), active.ID, scoreboard.StatusActive); err != nil {
		t.Fatal(err)
	}
	active = p.member(active.ID)
	p.mustPutWeeklyChallenge(active, probeWeek, encode(t, baseChallenge()))
	// PROBE: the HTTP layer answers memberPending before PutWeeklyChallenge runs; what the service itself does with a
	// pending member's weekly challenge is recorded.
	_, err := p.putWeeklyChallenge(waiting, probeWeek, encode(t, baseChallenge()))
	t.Logf("PutWeeklyChallenge by a pending member: %v", err)
	board, err := p.service.Scoreboard(context.Background(), probeWeek)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range board.Entries {
		if entry.Member.ID == waiting.ID {
			t.Error("a pending member is on the scoreboard")
		}
	}
	if len(board.Entries) != 1 || board.Entries[0].Member.ID != active.ID {
		t.Errorf("scoreboard entries %+v, want only the active member", board.Entries)
	}
	_, err = p.service.MemberWeeklyChallenge(context.Background(), waiting.ID, probeWeek)
	expectCode(t, err, problem.NotFound)
	if err := p.service.SetStatus(context.Background(), waiting.ID, scoreboard.StatusActive); err != nil {
		t.Fatal(err)
	}
	board, _ = p.service.Scoreboard(context.Background(), probeWeek)
	if len(board.Entries) != 2 {
		t.Errorf("%d entries after approval, want 2", len(board.Entries))
	}
	expectCode(t, p.service.SetStatus(context.Background(), "AAAAAAAAAAAAAAAAAAAAAA", scoreboard.StatusActive), problem.NotFound)
	// PROBE: whether SetStatus takes "pending" or an unknown status is not written down; recorded.
	t.Logf("SetStatus back to pending: %v", p.service.SetStatus(context.Background(), waiting.ID, scoreboard.StatusPending))
	t.Logf("SetStatus to an unknown status: %v", p.service.SetStatus(context.Background(), waiting.ID, "banned"))
	if status := p.member(waiting.ID).Status; status != scoreboard.StatusActive && status != scoreboard.StatusPending {
		t.Errorf("member status %q after an unknown status was set", status)
	}
}

// everythingStoredFor lists every document and collection under the store that names the member.
func (p *probe) everythingStoredFor(memberID string) []string {
	p.t.Helper()
	var found []string
	var visit func(collection string)
	visit = func(collection string) {
		if strings.Contains(collection, memberID) {
			found = append(found, collection+"/")
		}
		items, err := p.directory.List(context.Background(), collection)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			p.t.Fatal(err)
		}
		for _, item := range items {
			if item.ID == memberID || strings.Contains(collection, memberID) {
				found = append(found, collection+"/"+item.ID)
			}
		}
		children, err := p.directory.Collections(context.Background(), collection+"/")
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			p.t.Fatal(err)
		}
		for _, child := range children {
			if !strings.HasPrefix(child, collection+"/") {
				child = collection + "/" + child
			}
			visit(child)
		}
	}
	for _, top := range []string{"members", "weeks", "routes", "avatars", "blocked", "board"} {
		visit(top)
	}
	return found
}

// TestLeaveErasesEverything removes the membership, weekly challenges, routes and avatar, and lets the key join afresh.
func TestLeaveErasesEverything(t *testing.T) {
	p := newProbe(t, nil)
	who := newIdentity(t)
	member := p.joinAs(who, "Leaver", sharing.Highest)
	stayer := p.join("Stayer", sharing.Highest)
	p.putEverywhere(member)
	p.putEverywhere(stayer)
	if err := p.service.SetAvatar(context.Background(), member, encodedPNG(t, 40), "image/png"); err != nil {
		t.Fatal(err)
	}
	if before := p.everythingStoredFor(member.ID); len(before) < 7 {
		t.Fatalf("expected the member's documents before leaving, found %v", before)
	}
	if err := p.service.Leave(context.Background(), member.ID); err != nil {
		t.Fatal(err)
	}
	if after := p.everythingStoredFor(member.ID); len(after) != 0 {
		t.Errorf("still stored after leaving: %v", after)
	}
	expectCode(t, mustErr(p.service.Member(context.Background(), member.ID)), problem.NotFound)
	expectCode(t, mustErr(p.service.Avatar(context.Background(), member.ID)), problem.NotFound)
	expectCode(t, mustErr(p.service.MemberWeeklyChallenge(context.Background(), member.ID, probeWeek)), problem.NotFound)
	if usage, err := p.service.Usage(context.Background(), member.ID); err == nil && usage.Total() != 0 {
		t.Errorf("usage %d after leaving", usage.Total())
	}
	if others := p.everythingStoredFor(stayer.ID); len(others) < 7 {
		t.Errorf("leaving took the other member's data: %v", others)
	}
	// Leaving twice is harmless and the key joins again as new.
	// PROBE: whether a second Leave is notFound or a no-op is not written down; recorded.
	t.Logf("second Leave: %v", p.service.Leave(context.Background(), member.ID))
	again, created, err := p.joinWith(who, p.joinRequest(who, "Returned", sharing.Lowest))
	if err != nil || !created || again.DisplayName != "Returned" {
		t.Errorf("re-join after leaving: created %t, %+v, %v", created, again, err)
	}
	if err := p.service.Leave(context.Background(), stayer.ID); err != nil {
		t.Fatal(err)
	}
	if left := p.everythingStoredFor(stayer.ID); len(left) != 0 {
		t.Errorf("still stored after the last member left: %v", left)
	}
	for _, start := range weekStarts {
		if items, err := p.directory.List(context.Background(), "weeks/"+start); err == nil && len(items) != 0 {
			t.Errorf("weeks/%s still holds %d documents", start, len(items))
		}
	}
}

// TestBlockingAndUnblocking refuses a blocked key, lists blocks newest first, and lets an unblocked key join afresh.
func TestBlockingAndUnblocking(t *testing.T) {
	p := newProbe(t, nil)
	first, second := newIdentity(t), newIdentity(t)
	p.joinAs(first, "First", sharing.Highest)
	p.joinAs(second, "Second", sharing.Highest)
	p.putEverywhere(p.member(first.id))
	if err := p.service.DeleteMember(context.Background(), first.id, true); err != nil {
		t.Fatal(err)
	}
	if left := p.everythingStoredFor(first.id); len(left) > 1 {
		t.Errorf("blocking left %v", left)
	}
	p.advance(time.Hour)
	if err := p.service.DeleteMember(context.Background(), second.id, true); err != nil {
		t.Fatal(err)
	}
	blocked, err := p.service.Blocked(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 2 || blocked[0].ID != second.id || blocked[1].ID != first.id {
		t.Errorf("Blocked() = %+v, want second then first", blocked)
	}
	if !blocked[0].BlockedAt.After(blocked[1].BlockedAt) {
		t.Errorf("BlockedAt not newest first: %v then %v", blocked[0].BlockedAt, blocked[1].BlockedAt)
	}
	_, _, err = p.joinWith(first, p.joinRequest(first, "First", sharing.Highest))
	expectCode(t, err, problem.Blocked)
	expectCode(t, mustErr(p.service.Member(context.Background(), first.id)), problem.NotFound)
	if err := p.service.Unblock(context.Background(), first.id); err != nil {
		t.Fatal(err)
	}
	if blocked, _ := p.service.Blocked(context.Background()); len(blocked) != 1 || blocked[0].ID != second.id {
		t.Errorf("Blocked() after unblocking = %+v", blocked)
	}
	member, created, err := p.joinWith(first, p.joinRequest(first, "Fresh", sharing.Lowest))
	if err != nil || !created || member.DisplayName != "Fresh" || member.Sharing != sharing.Lowest {
		t.Errorf("join after unblocking: created %t, %+v, %v", created, member, err)
	}
	if usage, err := p.service.Usage(context.Background(), first.id); err != nil || usage.Total() != 0 {
		t.Errorf("a fresh membership carries usage %d (%v)", usage.Total(), err)
	}
	// PROBE: the contract does not say whether unblocking a key that is not blocked or deleting a member
	// who does not exist is an error. Recorded.
	t.Logf("Unblock of an unblocked key: %v", p.service.Unblock(context.Background(), first.id))
	t.Logf("DeleteMember of a stranger with block: %v", p.service.DeleteMember(context.Background(), "AAAAAAAAAAAAAAAAAAAAAA", true))
	t.Logf("DeleteMember of a stranger without block: %v", p.service.DeleteMember(context.Background(), "AAAAAAAAAAAAAAAAAAAAAA", false))
	t.Logf("Blocked after deleting a stranger with block: %+v", mustValue(p.service.Blocked(context.Background())))
	// Deleting without a block lets the key straight back in.
	if err := p.service.DeleteMember(context.Background(), first.id, false); err != nil {
		t.Fatal(err)
	}
	if _, created, err := p.joinWith(first, p.joinRequest(first, "Back", sharing.Lowest)); err != nil || !created {
		t.Errorf("join after an unblocked delete: created %t, %v", created, err)
	}
}

// mustValue returns only the value of a two-value call.
func mustValue[Value any](value Value, _ error) Value { return value }

// TestMembersAreListedByName records the order Members returns and checks it is stable and complete.
func TestMembersAreListedByName(t *testing.T) {
	p := newProbe(t, func(board *scoreboard.Board) {
		board.JoinMode = scoreboard.JoinApproval
		board.MaximumPending = 50
	})
	names := []string{"zed", "Émile", "adam", "Bob", "Ångström", "bob", "Zed", "éa", "Eb", "山田", "10", "9"}
	for _, name := range names {
		p.join(name, sharing.Lowest)
	}
	members, err := p.service.Members(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != len(names) {
		t.Fatalf("Members() has %d, want %d including pending", len(members), len(names))
	}
	var listed []string
	for _, member := range members {
		listed = append(listed, member.DisplayName)
	}
	// PROBE: "by name" does not say whether the order ignores case or accents; the order is recorded.
	t.Logf("Members() order: %q", listed)
	for index := 1; index < len(listed); index++ {
		if strings.ToLower(listed[index-1]) > strings.ToLower(listed[index]) && !strings.EqualFold(listed[index-1], listed[index]) {
			if listed[index-1] < listed[index] {
				continue
			}
			t.Errorf("Members() not by name: %q before %q", listed[index-1], listed[index])
		}
	}
	again, _ := p.service.Members(context.Background())
	for index := range again {
		if again[index].ID != members[index].ID {
			t.Fatalf("Members() order changed between calls")
		}
	}
}

// scoreboardNames lists the display names of a scoreboard in order, with a mark for members without a weekly challenge.
func scoreboardNames(board scoreboard.Scoreboard) []string {
	var names []string
	for _, entry := range board.Entries {
		name := entry.Member.DisplayName
		if entry.Challenge == nil {
			name += "(none)"
		}
		names = append(names, name)
	}
	return names
}

// TestScoreboardRankByProgress orders highest progress first and members without a weekly challenge last by name.
func TestScoreboardRankByProgress(t *testing.T) {
	p := newProbe(t, func(board *scoreboard.Board) { board.Ranking = scoreboard.RankByProgress })
	put := func(name string, progress float64) {
		member := p.join(name, sharing.Lowest)
		p.mustPutWeeklyChallenge(member, probeWeek, encode(t, map[string]any{"progress": progress}))
	}
	put("Cara", 0.5)
	put("Abe", 1.5)
	put("Bea", 0.5)
	put("Zero", 0)
	put("Huge", 1e308)
	p.join("Nancy", sharing.Lowest)
	p.join("Mike", sharing.Lowest)
	board, err := p.service.Scoreboard(context.Background(), probeWeek)
	if err != nil {
		t.Fatal(err)
	}
	if board.Ranking != scoreboard.RankByProgress || board.WeekStart != probeWeek {
		t.Errorf("scoreboard ranking %q week %q", board.Ranking, board.WeekStart)
	}
	names := scoreboardNames(board)
	want := []string{"Huge", "Abe", "Bea", "Cara", "Zero", "Mike(none)", "Nancy(none)"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		// PROBE: ties and the order among them are not written down beyond "highest first"; by name is
		// assumed here and the real order is recorded.
		t.Logf("progress order %q, expected %q", names, want)
	}
	for index := 1; index < len(board.Entries); index++ {
		previous, current := board.Entries[index-1], board.Entries[index]
		if previous.Challenge == nil && current.Challenge != nil {
			t.Errorf("%s without a weekly challenge ranks above %s with one", previous.Member.DisplayName, current.Member.DisplayName)
		}
		if previous.Challenge != nil && current.Challenge != nil && previous.Challenge.Progress < current.Challenge.Progress {
			t.Errorf("%s (%v) ranks above %s (%v)", previous.Member.DisplayName, previous.Challenge.Progress, current.Member.DisplayName, current.Challenge.Progress)
		}
		if previous.Challenge == nil && current.Challenge == nil && previous.Member.DisplayName > current.Member.DisplayName {
			t.Errorf("members without a weekly challenge not by name: %s before %s", previous.Member.DisplayName, current.Member.DisplayName)
		}
	}
	if board.Entries[4].Member.DisplayName != "Zero" || board.Entries[4].Challenge == nil {
		t.Errorf("a progress 0 weekly challenge should rank above members without one: %q", names)
	}
}

// TestScoreboardRankByName orders every member by name, with weekly challenges first.
func TestScoreboardRankByName(t *testing.T) {
	p := newProbe(t, func(board *scoreboard.Board) { board.Ranking = scoreboard.RankByName })
	for _, name := range []string{"Zed", "adam", "Émile", "Bob", "bob"} {
		member := p.join(name, sharing.Lowest)
		p.mustPutWeeklyChallenge(member, probeWeek, encode(t, map[string]any{"progress": 0.5}))
	}
	p.join("Yvonne", sharing.Lowest)
	p.join("ada", sharing.Lowest)
	board, err := p.service.Scoreboard(context.Background(), probeWeek)
	if err != nil {
		t.Fatal(err)
	}
	names := scoreboardNames(board)
	// PROBE: whether "by name" ignores case and accents is not written down; the order is recorded.
	t.Logf("name order: %q", names)
	if board.Ranking != scoreboard.RankByName {
		t.Errorf("ranking %q", board.Ranking)
	}
	if len(board.Entries) != 7 {
		t.Fatalf("%d entries, want 7", len(board.Entries))
	}
	for index := 1; index < 5; index++ {
		if board.Entries[index].Challenge == nil {
			t.Errorf("member without a weekly challenge at position %d among members with one", index)
		}
	}
	if names[5] != "ada(none)" || names[6] != "Yvonne(none)" {
		t.Errorf("members without a weekly challenge last by name: %q", names[5:])
	}
	lowered := func(index int) string { return strings.ToLower(board.Entries[index].Member.DisplayName) }
	for index := 1; index < 5; index++ {
		if lowered(index-1) > lowered(index) && lowered(index - 1)[0] < 0x80 && lowered(index)[0] < 0x80 {
			t.Errorf("not case-insensitively by name: %s before %s", board.Entries[index-1].Member.DisplayName, board.Entries[index].Member.DisplayName)
		}
	}
}

// TestScoreboardWeekRange answers weekOutOfRange for weeks the board does not keep and an empty scoreboard otherwise.
func TestScoreboardWeekRange(t *testing.T) {
	p := newProbe(t, func(board *scoreboard.Board) { board.RetentionWeeks = 2 })
	p.join("Only", sharing.Lowest)
	for _, start := range []string{"2026-10-12", "2026-10-19", "2026-10-26"} {
		board, err := p.service.Scoreboard(context.Background(), start)
		if err != nil {
			t.Errorf("Scoreboard(%s): %v", start, err)
			continue
		}
		if len(board.Entries) != 1 || board.Entries[0].Challenge != nil {
			t.Errorf("Scoreboard(%s) entries %+v", start, board.Entries)
		}
	}
	for _, start := range []string{"2026-10-05", "2026-11-02", "2026-10-20", "2026-10-18"} {
		expectCode(t, mustErr(p.service.Scoreboard(context.Background(), start)), problem.WeekOutOfRange)
	}
	expectCode(t, mustErr(p.service.Scoreboard(context.Background(), "20261019")), problem.BadRequest)
}

// TestMemberWeeklyChallengeAnswersNotFound answers notFound for an unknown member and for a member without a weekly challenge that week.
func TestMemberWeeklyChallengeAnswersNotFound(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Only", sharing.Lowest)
	expectCode(t, mustErr(p.service.MemberWeeklyChallenge(context.Background(), member.ID, probeWeek)), problem.NotFound)
	expectCode(t, mustErr(p.service.MemberWeeklyChallenge(context.Background(), "AAAAAAAAAAAAAAAAAAAAAA", probeWeek)), problem.NotFound)
	expectCode(t, mustErr(p.service.MemberWeeklyChallenge(context.Background(), member.ID, "2026-10-20")), problem.WeekOutOfRange)
	// PROBE: an identifier that is not one reaches the store, which refuses it with its own error rather than a
	// problem; the HTTP layer validates the path first. Recorded.
	for _, id := range []string{"../board", "", "AAAAAAAAAAAAAAAAAAAAA", "board/board"} {
		_, err := p.service.MemberWeeklyChallenge(context.Background(), id, probeWeek)
		if err == nil {
			t.Errorf("MemberWeeklyChallenge(%q) succeeded", id)
		}
		_, memberErr := p.service.Member(context.Background(), id)
		if memberErr == nil {
			t.Errorf("Member(%q) succeeded", id)
		}
		t.Logf("id %q: MemberWeeklyChallenge %v; Member %v", id, err, memberErr)
	}
}

// TestRetentionSweepDeletesOnlyWhatIsNoLongerKept sweeps weekly challenges and their routes past retention and nothing newer.
func TestRetentionSweepDeletesOnlyWhatIsNoLongerKept(t *testing.T) {
	p := newProbe(t, func(board *scoreboard.Board) { board.RetentionWeeks = 4 })
	member := p.join("Keeper", sharing.Highest)
	starts := []string{"2026-09-28", "2026-10-05", "2026-10-12", "2026-10-19", "2026-10-26"}
	for _, start := range starts {
		challenge := baseChallenge()
		setPath(t, challenge, "activities.0.day", start)
		p.mustPutWeeklyChallenge(member, start, encode(t, challenge))
		if err := p.putRoute(member, start, probeActivity, shortRoute(t, 5)); err != nil {
			t.Fatal(err)
		}
	}
	expectCode(t, mustErr(p.putWeeklyChallenge(member, "2026-09-21", encode(t, baseChallenge()))), problem.WeekOutOfRange)
	if err := p.service.SweepRetention(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.expectChallenges(member.ID, starts, nil)
	// A week on, the oldest week falls out of retention.
	p.advance(oneWeek)
	if err := p.service.SweepRetention(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.expectChallenges(member.ID, starts[1:], starts[:1])
	// Shortening retention deletes at once.
	if _, err := p.service.UpdateBoard(context.Background(), func(board *scoreboard.Board) error {
		board.RetentionWeeks = 2
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p.expectChallenges(member.ID, starts[3:], starts[:3])
	// Lengthening restores nothing.
	if _, err := p.service.UpdateBoard(context.Background(), func(board *scoreboard.Board) error {
		board.RetentionWeeks = 52
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p.expectChallenges(member.ID, starts[3:], starts[:3])
	usage := p.usage(member.ID)
	if len(usage.Challenges) != 2 || usage.Challenges[0].WeekStart != "2026-10-26" || usage.Challenges[1].WeekStart != "2026-10-19" {
		t.Errorf("usage challenges %+v, want 2026-10-26 then 2026-10-19", usage.Challenges)
	}
	for _, used := range usage.Challenges {
		if used.Routes != 1 || used.RouteBytes == 0 || used.ChallengeBytes == 0 {
			t.Errorf("usage %+v, want one route and a weekly challenge", used)
		}
	}
}

// expectChallenges checks which of a member's weekly challenges and routes are stored and which are gone.
func (p *probe) expectChallenges(memberID string, kept, gone []string) {
	p.t.Helper()
	for _, start := range kept {
		if p.storedChallenge(memberID, start) == nil {
			p.t.Errorf("weekly challenge %s is gone", start)
		}
		if p.storedRoutes(memberID, start) != 1 {
			p.t.Errorf("weekly challenge %s lost its route", start)
		}
	}
	for _, start := range gone {
		if p.storedChallenge(memberID, start) != nil {
			p.t.Errorf("weekly challenge %s is still stored", start)
		}
		if count := p.storedRoutes(memberID, start); count != 0 {
			p.t.Errorf("weekly challenge %s still has %d routes", start, count)
		}
	}
}

// TestEraseHistoryAfterChangingWeekStartDay removes every weekly challenge and route for every member.
func TestEraseHistoryAfterChangingWeekStartDay(t *testing.T) {
	p := newProbe(t, nil)
	members := []scoreboard.Member{p.join("One", sharing.Highest), p.join("Two", sharing.Highest)}
	for _, member := range members {
		p.putEverywhere(member)
	}
	if _, err := p.service.UpdateBoard(context.Background(), func(board *scoreboard.Board) error {
		board.WeekStartDay = "sunday"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.service.EraseHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		p.expectChallenges(member.ID, nil, weekStarts)
		if usage := p.usage(member.ID); usage.Total() != 0 {
			t.Errorf("%s still uses %d bytes", member.DisplayName, usage.Total())
		}
		if _, err := p.service.Member(context.Background(), member.ID); err != nil {
			t.Errorf("%s lost their membership: %v", member.DisplayName, err)
		}
	}
	if collections, err := p.directory.Collections(context.Background(), "weeks/"); err == nil {
		for _, collection := range collections {
			if items, _ := p.directory.List(context.Background(), "weeks/"+collection); len(items) != 0 {
				t.Errorf("weeks/%s still holds %d documents", collection, len(items))
			}
		}
	}
	// The board now reads Sundays.
	expectCode(t, mustErr(p.putWeeklyChallenge(members[0], "2026-10-18", encode(t, map[string]any{"progress": 0.5}))), "")
	expectCode(t, mustErr(p.putWeeklyChallenge(members[0], "2026-10-19", encode(t, map[string]any{"progress": 0.5}))), problem.WeekOutOfRange)
}

// TestAvatars stores a PNG, serves it as JPEG within 512 a side, and removes it on request or when the board stops showing avatars.
func TestAvatars(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Face", sharing.Lowest)
	if err := p.service.SetAvatar(context.Background(), member, encodedPNG(t, 1024), "image/png"); err != nil {
		t.Fatal(err)
	}
	if !p.member(member.ID).HasAvatar {
		t.Error("HasAvatar false after SetAvatar")
	}
	data, err := p.service.Avatar(context.Background(), member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
		t.Errorf("avatar does not start with the JPEG magic: % x", data[:min(4, len(data))])
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "jpeg" {
		t.Fatalf("avatar decodes as %q: %v", format, err)
	}
	if config.Width > 512 || config.Height > 512 {
		t.Errorf("avatar %dx%d, want at most 512 a side", config.Width, config.Height)
	}
	board, err := p.service.Scoreboard(context.Background(), probeWeek)
	if err != nil || len(board.Entries) != 1 || !board.Entries[0].Member.HasAvatar {
		t.Errorf("scoreboard HasAvatar false with an avatar stored (%v)", err)
	}
	for _, bad := range []struct {
		// name says what the upload is.
		name string

		// data is the body.
		data []byte

		// contentType is the declared type.
		contentType string

		// want is the code.
		want problem.Code
	}{
		{name: "not an image", data: []byte("not an image"), contentType: "image/png", want: problem.ImageInvalid},
		{name: "empty", data: nil, contentType: "image/png", want: problem.ImageInvalid},
		{name: "png header only", data: []byte("\x89PNG\r\n\x1a\n"), contentType: "image/png", want: problem.ImageInvalid},
		{name: "gif", data: []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"), contentType: "image/gif", want: problem.UnsupportedMediaType},
	} {
		err := p.service.SetAvatar(context.Background(), member, bad.data, bad.contentType)
		if got := codeOf(err); got != bad.want {
			// PROBE: which layer refuses which bad upload is not written down; recorded.
			t.Logf("%s: %v (expected %s)", bad.name, err, bad.want)
		}
	}
	if !p.member(member.ID).HasAvatar {
		t.Error("a refused upload removed the avatar")
	}
	for range 2 {
		if err := p.service.DeleteAvatar(context.Background(), member.ID); err != nil {
			t.Errorf("DeleteAvatar: %v", err)
		}
	}
	expectCode(t, mustErr(p.service.Avatar(context.Background(), member.ID)), problem.NotFound)
	if p.member(member.ID).HasAvatar {
		t.Error("HasAvatar true after DeleteAvatar")
	}
	// Turning avatars off deletes every avatar and hides HasAvatar everywhere.
	if err := p.service.SetAvatar(context.Background(), member, encodedPNG(t, 40), "image/png"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.service.UpdateBoard(context.Background(), func(board *scoreboard.Board) error {
		board.AvatarsAllowed = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	expectCode(t, mustErr(p.service.Avatar(context.Background(), member.ID)), problem.NotFound)
	if p.member(member.ID).HasAvatar {
		t.Error("HasAvatar true after avatars were turned off")
	}
	if board, _ := p.service.Scoreboard(context.Background(), probeWeek); len(board.Entries) == 1 && board.Entries[0].Member.HasAvatar {
		t.Error("scoreboard shows HasAvatar on a board without avatars")
	}
	if found := p.everythingStoredFor(member.ID); len(found) != 1 {
		t.Errorf("stored for the member after avatars went off: %v, want only the membership", found)
	}
	expectCode(t, p.service.SetAvatar(context.Background(), member, encodedPNG(t, 40), "image/png"), problem.AvatarsNotAllowed)
	expectCode(t, mustErr(p.service.Avatar(context.Background(), "AAAAAAAAAAAAAAAAAAAAAA")), problem.NotFound)
}

// TestAvatarsNotAllowedBoard refuses an upload outright.
func TestAvatarsNotAllowedBoard(t *testing.T) {
	p := newProbe(t, func(board *scoreboard.Board) { board.AvatarsAllowed = false })
	member := p.join("Face", sharing.Lowest)
	expectCode(t, p.service.SetAvatar(context.Background(), member, encodedPNG(t, 40), "image/png"), problem.AvatarsNotAllowed)
	if p.member(member.ID).HasAvatar {
		t.Error("HasAvatar true after a refused upload")
	}
	if found := p.everythingStoredFor(member.ID); len(found) != 1 {
		t.Errorf("stored after a refused upload: %v", found)
	}
}

// TestDefaultBoard checks a new board's defaults and the limits around its password and invite.
func TestDefaultBoard(t *testing.T) {
	board, err := scoreboard.DefaultBoard()
	if err != nil {
		t.Fatal(err)
	}
	if board.JoinMode != scoreboard.JoinApproval {
		t.Errorf("JoinMode %q, want approval", board.JoinMode)
	}
	if board.MaximumSharing != sharing.Highest {
		t.Errorf("MaximumSharing %s, want everything", describeSharing(board.MaximumSharing))
	}
	if board.RetentionWeeks != 52 {
		t.Errorf("RetentionWeeks %d, want 52", board.RetentionWeeks)
	}
	if !scoreboard.IsWeekStartDay(board.WeekStartDay) {
		t.Errorf("WeekStartDay %q is not a weekday", board.WeekStartDay)
	}
	t.Logf("default week start day %q, ranking %q, avatars %t, show invite %t", board.WeekStartDay, board.Ranking, board.AvatarsAllowed, board.ShowInvite)
	if !scoreboard.IsIdentifier(board.ID) {
		t.Errorf("ID %q is not an identifier", board.ID)
	}
	if board.InviteCode == "" || len(board.InviteCode) > 64 {
		t.Errorf("InviteCode %q", board.InviteCode)
	}
	if board.PendingLimit() != scoreboard.DefaultMaximumPending || board.MaximumPending != 0 && board.PendingLimit() != board.MaximumPending {
		t.Errorf("PendingLimit %d with MaximumPending %d", board.PendingLimit(), board.MaximumPending)
	}
	board.MaximumPending = 0
	if board.PendingLimit() != 10 {
		t.Errorf("PendingLimit %d with MaximumPending 0, want 10", board.PendingLimit())
	}
	board.MaximumPending = 7
	if board.PendingLimit() != 7 {
		t.Errorf("PendingLimit %d with MaximumPending 7", board.PendingLimit())
	}
	second, _ := scoreboard.DefaultBoard()
	if second.ID == board.ID || second.InviteCode == board.InviteCode {
		t.Error("two default boards share an identifier or invite")
	}
	if board.PasswordHash != "" {
		t.Error("a new board has a password")
	}
}

// TestValidatePassword accepts 8 to 256 characters.
func TestValidatePassword(t *testing.T) {
	for _, ok := range []string{strings.Repeat("a", 8), strings.Repeat("a", 256), "pässwörd", strings.Repeat("山", 8), strings.Repeat("🇺🇸", 8)} {
		if err := scoreboard.ValidatePassword(ok); err != nil {
			t.Errorf("ValidatePassword(%d chars) = %v", len([]rune(ok)), err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("a", 7), strings.Repeat("a", 257), strings.Repeat("山", 7)} {
		if err := scoreboard.ValidatePassword(bad); err == nil {
			t.Errorf("ValidatePassword(%d chars) accepted", len([]rune(bad)))
		}
	}
	// PROBE: the password limit is "characters"; 256 runes of a 4-byte script and 257 2-byte runes decide
	// whether it counts bytes or runes. Recorded.
	t.Logf("256 cjk: %v; 257 cjk: %v; 300 bytes of 2-byte runes (150 runes): %v", scoreboard.ValidatePassword(strings.Repeat("山", 256)), scoreboard.ValidatePassword(strings.Repeat("山", 257)), scoreboard.ValidatePassword(strings.Repeat("é", 150)))
}

// TestBoardPasswordAndInvite sets, checks and removes the password, and retires the old invite.
func TestBoardPasswordAndInvite(t *testing.T) {
	p := newProbe(t, nil)
	if err := p.service.SetBoardPassword(context.Background(), "open sesame now"); err != nil {
		t.Fatal(err)
	}
	if p.board().PasswordHash == "" {
		t.Fatal("no hash after SetBoardPassword")
	}
	who := newIdentity(t)
	expectCode(t, mustErr3(p.joinWith(who, p.joinRequest(who, "Knock", sharing.Lowest))), problem.PasswordIncorrect)
	request := p.joinRequest(who, "Knock", sharing.Lowest)
	request.Password = "open sesame now"
	expectCode(t, mustErr3(p.joinWith(who, request)), "")
	for _, short := range []string{"short", strings.Repeat("a", 257)} {
		if err := p.service.SetBoardPassword(context.Background(), short); err == nil {
			t.Errorf("SetBoardPassword(%d chars) accepted", len(short))
		}
	}
	if err := p.service.SetBoardPassword(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if p.board().PasswordHash != "" {
		t.Error("hash remains after SetBoardPassword(\"\")")
	}
	other := newIdentity(t)
	expectCode(t, mustErr3(p.joinWith(other, p.joinRequest(other, "Walk in", sharing.Lowest))), "")
	before := p.board().InviteCode
	replaced, err := p.service.ReplaceInvite(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if replaced.InviteCode == before || replaced.InviteCode == "" || p.board().InviteCode != replaced.InviteCode {
		t.Errorf("invite %q after replacing %q; stored %q", replaced.InviteCode, before, p.board().InviteCode)
	}
	third := newIdentity(t)
	expectCode(t, mustErr3(p.joinWith(third, p.joinRequest(third, "Old link", sharing.Lowest))), problem.InviteInvalid)
	request = p.joinRequest(third, "New link", sharing.Lowest)
	request.InviteCode = replaced.InviteCode
	expectCode(t, mustErr3(p.joinWith(third, request)), "")
	if again, _ := p.service.ReplaceInvite(context.Background()); again.InviteCode == replaced.InviteCode {
		t.Error("ReplaceInvite repeated a code")
	}
}

// mustErr3 returns only the error of a three-value call.
func mustErr3[First, Second any](_ First, _ Second, err error) error { return err }

// TestUpdateBoardRefusesAndRollsBack keeps the board when change returns an error.
func TestUpdateBoardRefusesAndRollsBack(t *testing.T) {
	p := newProbe(t, nil)
	before := p.board()
	refused := errors.New("no")
	_, err := p.service.UpdateBoard(context.Background(), func(board *scoreboard.Board) error {
		board.Name = "Changed"
		return refused
	})
	if !errors.Is(err, refused) {
		t.Errorf("UpdateBoard returned %v, want the change's error", err)
	}
	if after := p.board(); after != before {
		t.Errorf("board changed by a refused update: %+v", after)
	}
	// EnsureBoard never replaces an existing board.
	other, _ := scoreboard.DefaultBoard()
	board, created, err := p.service.EnsureBoard(context.Background(), other)
	if err != nil || created || board.ID != before.ID {
		t.Errorf("EnsureBoard on an existing board: created %t, id %s, %v", created, board.ID, err)
	}
}
