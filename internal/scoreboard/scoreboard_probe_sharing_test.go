package scoreboard_test

import (
	"context"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/problem"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
)

// boardMaxima are the board maxima the stripping property runs every member choice against.
var boardMaxima = []sharing.Sharing{
	sharing.Highest,
	sharing.Lowest,
	{Score: sharing.ScoreScore, Activity: sharing.ActivityNone},
	{Score: sharing.ScoreDetails, Activity: sharing.ActivityNone},
	{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, StrengthSets: true},
	{Score: sharing.ScoreDetails, Activity: sharing.ActivitySummary, Routes: true},
	{Score: sharing.ScoreDetails, Activity: sharing.ActivityFull, StrengthSets: true, StrengthWeights: true},
}

// allowedLeaves is every allowlist path that effective allows, with updatedAt, which the server always sets.
func (p *probe) allowedLeaves(effective sharing.Sharing) map[string]bool {
	set := map[string]bool{}
	for _, path := range p.allowlist.Paths() {
		if p.allowlist.Allows(path, effective) {
			set[path] = true
		}
	}
	return set
}

// checkLeaves fails unless object's leaves are exactly the allowed ones for effective.
func (p *probe) checkLeaves(label string, object map[string]any, effective sharing.Sharing) {
	p.t.Helper()
	got := leafSet(object)
	want := p.allowedLeaves(effective)
	for path := range got {
		if !want[path] {
			p.t.Errorf("%s: %s is present but not allowed at %s", label, path, describeSharing(effective))
		}
	}
	for path := range want {
		if !got[path] {
			p.t.Errorf("%s: %s is allowed at %s but missing from a full weekly challenge", label, path, describeSharing(effective))
		}
	}
}

// checkSummary fails unless a scoreboard summary shows exactly what effective allows.
func (p *probe) checkSummary(label string, summary *scoreboard.WeeklyChallengeSummary, effective sharing.Sharing) {
	p.t.Helper()
	if summary == nil {
		p.t.Errorf("%s: no weekly challenge summary", label)
		return
	}
	scoreShared := p.allowlist.Allows("score", effective)
	if (summary.Score != nil) != scoreShared || (summary.Goal != nil) != scoreShared || (summary.Tier != nil) != scoreShared {
		p.t.Errorf("%s: score/goal/tier presence %t/%t/%t, want %t", label, summary.Score != nil, summary.Goal != nil, summary.Tier != nil, scoreShared)
	}
	if breakdownShared := p.allowlist.Allows("breakdown.exercise", effective); (summary.Breakdown != nil) != breakdownShared {
		p.t.Errorf("%s: breakdown present %t, want %t", label, summary.Breakdown != nil, breakdownShared)
	}
	if countShared := effective.Activity != sharing.ActivityNone; (summary.ActivityCount != nil) != countShared {
		p.t.Errorf("%s: activityCount present %t, want %t", label, summary.ActivityCount != nil, countShared)
	} else if countShared && *summary.ActivityCount != 1 {
		p.t.Errorf("%s: activityCount %d, want 1", label, *summary.ActivityCount)
	}
	if summary.Progress != 0.8 {
		p.t.Errorf("%s: progress %v, want 0.8", label, summary.Progress)
	}
	if summary.UpdatedAt.IsZero() {
		p.t.Errorf("%s: updatedAt is zero", label)
	}
}

// TestEveryStoredAndServedLeafIsAllowed puts a full weekly challenge at every member choice and board maximum and checks
// what PutWeeklyChallenge returns, what is stored, what MemberWeeklyChallenge serves and what the scoreboard summarises.
func TestEveryStoredAndServedLeafIsAllowed(t *testing.T) {
	for _, maximum := range boardMaxima {
		p := newProbe(t, func(board *scoreboard.Board) { board.MaximumSharing = maximum })
		board := p.board()
		for _, chosen := range sharing.All() {
			member := p.join("M "+describeSharing(chosen), chosen)
			effective := scoreboard.Effective(member, board)
			if want := sharing.Effective(chosen, maximum).Normalized(); effective != want {
				t.Errorf("Effective(%s, %s) = %s, want %s", describeSharing(chosen), describeSharing(maximum), describeSharing(effective), describeSharing(want))
			}
			label := describeSharing(chosen) + " on " + describeSharing(maximum)
			kept := p.mustPutWeeklyChallenge(member, probeWeek, encode(t, baseChallenge()))
			p.checkLeaves(label+" returned", asObject(t, kept), effective)
			p.checkLeaves(label+" stored", p.storedChallenge(member.ID, probeWeek), effective)
			memberWeeklyChallenge, err := p.service.MemberWeeklyChallenge(context.Background(), member.ID, probeWeek)
			if err != nil {
				t.Fatal(err)
			}
			p.checkLeaves(label+" served", asObject(t, memberWeeklyChallenge.Challenge), effective)
			if memberWeeklyChallenge.Member.Sharing != effective {
				t.Errorf("%s: MemberSummary.Sharing %s, want effective %s", label, describeSharing(memberWeeklyChallenge.Member.Sharing), describeSharing(effective))
			}
			board, err := p.service.Scoreboard(context.Background(), probeWeek)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range board.Entries {
				if entry.Member.ID == member.ID {
					p.checkSummary(label+" summary", entry.Challenge, effective)
				}
			}
		}
	}
}

// weekStarts are three week starts the board keeps around the probe clock.
var weekStarts = []string{"2026-10-12", "2026-10-19", "2026-10-26"}

// putEverywhere puts a full weekly challenge, with a route, in every week of weekStarts.
func (p *probe) putEverywhere(member scoreboard.Member) {
	p.t.Helper()
	for _, start := range weekStarts {
		challenge := baseChallenge()
		setPath(p.t, challenge, "activities.0.day", start)
		p.mustPutWeeklyChallenge(member, start, encode(p.t, challenge))
		if err := p.service.PutRoute(context.Background(), member, start, probeActivity, shortRoute(p.t, 10)); err != nil {
			p.t.Fatalf("putting route in %s: %v", start, err)
		}
	}
}

// checkEverywhere checks every kept weekly challenge and its route against effective.
func (p *probe) checkEverywhere(label string, member scoreboard.Member, effective sharing.Sharing) {
	p.t.Helper()
	for _, start := range weekStarts {
		p.checkLeaves(label+" "+start, p.storedChallenge(member.ID, start), effective)
		_, err := p.service.Route(context.Background(), member.ID, start, probeActivity)
		if effective.Routes {
			if err != nil {
				p.t.Errorf("%s %s: route gone: %v", label, start, err)
			}
		} else {
			if codeOf(err) != problem.NotFound {
				p.t.Errorf("%s %s: route still served: %v", label, start, err)
			}
			if count := p.storedRoutes(member.ID, start); count != 0 {
				p.t.Errorf("%s %s: %d routes still stored", label, start, count)
			}
		}
	}
}

// TestLoweringMemberSharingStripsEveryKeptChallenge strips three weekly challenges, deletes routes and weights as the member steps down.
func TestLoweringMemberSharingStripsEveryKeptChallenge(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Stepper", sharing.Highest)
	p.putEverywhere(member)
	steps := []sharing.Sharing{
		{Score: sharing.ScoreDetails, Activity: sharing.ActivityFull, StrengthSets: true, Routes: true},
		{Score: sharing.ScoreDetails, Activity: sharing.ActivityFull, StrengthSets: true},
		{Score: sharing.ScoreDetails, Activity: sharing.ActivitySummary},
		{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints},
		{Score: sharing.ScoreDetails, Activity: sharing.ActivityNone},
		{Score: sharing.ScoreScore, Activity: sharing.ActivityNone},
		sharing.Lowest,
	}
	for _, step := range steps {
		updated, err := p.service.UpdateMember(context.Background(), member.ID, scoreboard.MemberUpdate{Sharing: &step})
		if err != nil {
			t.Fatal(err)
		}
		if updated.Sharing != step {
			t.Errorf("stored sharing %s, want %s", describeSharing(updated.Sharing), describeSharing(step))
		}
		p.checkEverywhere("after lowering to "+describeSharing(step), updated, step)
	}
	// Raising restores nothing.
	raised, err := p.service.UpdateMember(context.Background(), member.ID, scoreboard.MemberUpdate{Sharing: &sharing.Highest})
	if err != nil {
		t.Fatal(err)
	}
	p.checkEverywhere("after raising", raised, sharing.Lowest)
}

// TestLoweringBoardMaximumStripsEveryMember strips every member's weekly challenges when the admin lowers the maximum.
func TestLoweringBoardMaximumStripsEveryMember(t *testing.T) {
	p := newProbe(t, nil)
	members := []scoreboard.Member{p.join("One", sharing.Highest), p.join("Two", sharing.Highest), p.join("Three", sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, Routes: true})}
	for _, member := range members {
		p.putEverywhere(member)
	}
	for _, maximum := range []sharing.Sharing{
		{Score: sharing.ScoreDetails, Activity: sharing.ActivityFull, StrengthSets: true},
		{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints},
		sharing.Lowest,
	} {
		board, err := p.service.UpdateBoard(context.Background(), func(board *scoreboard.Board) error {
			board.MaximumSharing = maximum
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range members {
			effective := scoreboard.Effective(p.member(member.ID), board)
			p.checkEverywhere(member.DisplayName+" under "+describeSharing(maximum), member, effective)
			if stored := p.member(member.ID).Sharing; stored != member.Sharing {
				t.Errorf("%s: the member's own choice changed to %s", member.DisplayName, describeSharing(stored))
			}
		}
	}
	if _, err := p.service.UpdateBoard(context.Background(), func(board *scoreboard.Board) error {
		board.MaximumSharing = sharing.Highest
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		p.checkEverywhere(member.DisplayName+" after raising", member, sharing.Lowest)
	}
}

// TestMemberMayChooseAboveTheMaximum stores the choice as made and caps only the effective sharing.
func TestMemberMayChooseAboveTheMaximum(t *testing.T) {
	maximum := sharing.Sharing{Score: sharing.ScoreScore, Activity: sharing.ActivityNone}
	p := newProbe(t, func(board *scoreboard.Board) { board.MaximumSharing = maximum })
	member := p.join("Hopeful", sharing.Lowest)
	updated, err := p.service.UpdateMember(context.Background(), member.ID, scoreboard.MemberUpdate{Sharing: &sharing.Highest})
	if err != nil {
		t.Fatalf("choosing above the maximum refused: %v", err)
	}
	if updated.Sharing != sharing.Highest {
		t.Errorf("stored %s, want the choice %s", describeSharing(updated.Sharing), describeSharing(sharing.Highest))
	}
	if effective := scoreboard.Effective(updated, p.board()); effective != maximum {
		t.Errorf("effective %s, want %s", describeSharing(effective), describeSharing(maximum))
	}
	kept := p.mustPutWeeklyChallenge(updated, probeWeek, encode(t, baseChallenge()))
	p.checkLeaves("above maximum", asObject(t, kept), maximum)
	// Raising the maximum later lets the stored choice take effect without another update.
	board, err := p.service.UpdateBoard(context.Background(), func(board *scoreboard.Board) error {
		board.MaximumSharing = sharing.Highest
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if effective := scoreboard.Effective(p.member(member.ID), board); effective != sharing.Highest {
		t.Errorf("effective after raising %s, want everything", describeSharing(effective))
	}
}

// TestUpdateMemberNormalisesAndValidatesSharing lowers fields that do not apply and refuses unknown levels.
func TestUpdateMemberNormalisesAndValidatesSharing(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Chooser", sharing.Lowest)
	odd := sharing.Sharing{Score: sharing.ScoreProgress, Activity: sharing.ActivityFull, Routes: true, StrengthSets: true, StrengthWeights: true}
	updated, err := p.service.UpdateMember(context.Background(), member.ID, scoreboard.MemberUpdate{Sharing: &odd})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Sharing != odd.Normalized() {
		t.Errorf("stored %s, want normalized %s", describeSharing(updated.Sharing), describeSharing(odd.Normalized()))
	}
	weightsOnly := sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, StrengthWeights: true}
	updated, err = p.service.UpdateMember(context.Background(), member.ID, scoreboard.MemberUpdate{Sharing: &weightsOnly})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Sharing.StrengthWeights {
		t.Error("strengthWeights kept without strengthSets")
	}
	for _, bad := range []sharing.Sharing{
		{Score: "loud", Activity: sharing.ActivityNone},
		{Score: sharing.ScoreDetails, Activity: "everything"},
		{Score: "", Activity: ""},
		{Score: "Progress", Activity: "none"},
	} {
		_, err := p.service.UpdateMember(context.Background(), member.ID, scoreboard.MemberUpdate{Sharing: &bad})
		expectCode(t, err, problem.BadRequest)
	}
	empty := scoreboard.MemberUpdate{}
	// PROBE: MemberUpdate has minProperties 1; whether the service or only the HTTP layer refuses an empty
	// update is not written down, so the service's answer is recorded.
	_, err = p.service.UpdateMember(context.Background(), member.ID, empty)
	t.Logf("empty update: %v", err)
}

// TestUpdateMemberDisplayName normalises the new name and refuses a hidden one without touching sharing.
func TestUpdateMemberDisplayName(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Before", sharing.Highest)
	name := "  After  "
	updated, err := p.service.UpdateMember(context.Background(), member.ID, scoreboard.MemberUpdate{DisplayName: &name})
	if err != nil {
		t.Fatal(err)
	}
	if updated.DisplayName != "After" {
		t.Errorf("name %q, want %q", updated.DisplayName, "After")
	}
	if updated.Sharing != sharing.Highest {
		t.Errorf("sharing changed to %s", describeSharing(updated.Sharing))
	}
	for _, bad := range []string{"", "   ", "\u200b", "\u202eAfter", "Af\x00ter"} {
		_, err := p.service.UpdateMember(context.Background(), member.ID, scoreboard.MemberUpdate{DisplayName: &bad})
		expectCode(t, err, problem.BadRequest)
	}
	if p.member(member.ID).DisplayName != "After" {
		t.Error("a refused name changed the stored one")
	}
	missing := "Nobody"
	_, err = p.service.UpdateMember(context.Background(), "AAAAAAAAAAAAAAAAAAAAAA", scoreboard.MemberUpdate{DisplayName: &missing})
	expectCode(t, err, problem.NotFound)
}

// TestWeeklyChallengeArrivingAboveTheMaximumIsStripped puts a weekly challenge after the maximum dropped and keeps only what is allowed.
func TestWeeklyChallengeArrivingAboveTheMaximumIsStripped(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Late", sharing.Highest)
	maximum := sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints}
	if _, err := p.service.UpdateBoard(context.Background(), func(board *scoreboard.Board) error {
		board.MaximumSharing = maximum
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The member record in hand still says Highest, as a request racing the admin would carry.
	kept := p.mustPutWeeklyChallenge(member, probeWeek, encode(t, baseChallenge()))
	p.checkLeaves("stale member", asObject(t, kept), maximum)
	p.checkLeaves("stale member stored", p.storedChallenge(member.ID, probeWeek), maximum)
	err := p.service.PutRoute(context.Background(), member, probeWeek, probeActivity, shortRoute(t, 5))
	if codeOf(err) != problem.RoutesNotShared {
		t.Errorf("route with a stale member record: %v, want routesNotShared", err)
	}
	if count := p.storedRoutes(member.ID, probeWeek); count != 0 {
		t.Errorf("%d routes stored above the maximum", count)
	}
}
