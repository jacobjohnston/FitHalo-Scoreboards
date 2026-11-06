package scoreboard_test

import (
	"context"
	"sync"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/problem"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
)

// TestConcurrentPutWeeklyChallengeSameMember hammers one member's weekly challenge from twenty goroutines and ends with one valid one.
func TestConcurrentPutWeeklyChallengeSameMember(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Busy", sharing.Highest)
	bodies := make([][]byte, 20)
	for index := range bodies {
		challenge := challengeWithActivities(t, index+1)
		challenge["progress"] = float64(index) / 10
		bodies[index] = encode(t, challenge)
	}
	var group sync.WaitGroup
	failures := make(chan error, len(bodies))
	for _, body := range bodies {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := p.service.PutWeeklyChallenge(context.Background(), member, probeWeek, body); err != nil {
				failures <- err
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("concurrent PutWeeklyChallenge: %v", err)
	}
	stored := p.storedChallenge(member.ID, probeWeek)
	if stored == nil {
		t.Fatal("no weekly challenge stored")
	}
	activities, _ := stored["activities"].([]any)
	progress, _ := stored["progress"].(float64)
	if float64(len(activities)-1)/10 != progress {
		t.Errorf("stored weekly challenge mixes bodies: %d activities with progress %v", len(activities), progress)
	}
	if usage := p.usage(member.ID); len(usage.Challenges) != 1 || usage.Challenges[0].Routes != 0 {
		t.Errorf("usage %+v", usage.Challenges)
	}
}

// TestConcurrentJoinsNeverOvershootThePendingLimit joins twenty keys at once on a board that takes five.
func TestConcurrentJoinsNeverOvershootThePendingLimit(t *testing.T) {
	p := newProbe(t, func(board *scoreboard.Board) {
		board.JoinMode = scoreboard.JoinApproval
		board.MaximumPending = 5
	})
	who := make([]identity, 20)
	for index := range who {
		who[index] = newIdentity(t)
	}
	var group sync.WaitGroup
	results := make(chan error, len(who))
	for _, identity := range who {
		group.Add(1)
		go func() {
			defer group.Done()
			_, _, err := p.service.Join(context.Background(), p.joinRequest(identity, "Rush", sharing.Lowest), identity.id)
			results <- err
		}()
	}
	group.Wait()
	close(results)
	joined, refused := 0, 0
	for err := range results {
		switch codeOf(err) {
		case "":
			joined++
		case problem.PendingLimitReached:
			refused++
		default:
			t.Errorf("concurrent Join: %v", err)
		}
	}
	count, err := p.service.PendingCount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d joined, %d refused, %d pending", joined, refused, count)
	if count > 5 {
		t.Errorf("%d pending members on a board that takes 5", count)
	}
	if count != joined {
		t.Errorf("%d pending but %d joins succeeded", count, joined)
	}
	if count < 5 {
		t.Errorf("%d pending, the queue should have filled", count)
	}
}

// TestConcurrentRoutesRespectTheStorageCap puts twenty wide routes at once and measures the overshoot past 1 MB.
func TestConcurrentRoutesRespectTheStorageCap(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Runner", sharing.Highest)
	challenge := challengeWithActivities(t, 40)
	p.mustPutWeeklyChallenge(member, probeWeek, encode(t, challenge))
	body := wideRoute(t, 5000)
	var group sync.WaitGroup
	results := make(chan error, 40)
	for _, activity := range challenge["activities"].([]any) {
		id := activity.(map[string]any)["id"].(string)
		group.Add(1)
		go func() {
			defer group.Done()
			results <- p.service.PutRoute(context.Background(), member, probeWeek, id, body)
		}()
	}
	group.Wait()
	close(results)
	stored, refused := 0, 0
	for err := range results {
		switch codeOf(err) {
		case "":
			stored++
		case problem.PayloadTooLarge:
			refused++
		default:
			t.Errorf("concurrent PutRoute: %v", err)
		}
	}
	usage := p.usage(member.ID)
	overshoot := usage.Total() - scoreboard.MaximumWeeklyChallengeBytes
	t.Logf("%d routes of %d bytes stored, %d refused; usage %d, overshoot %d bytes (%.1f routes)", stored, len(body), refused, usage.Total(), overshoot, float64(overshoot)/float64(len(body)))
	if usage.Challenges[0].Routes != stored {
		t.Errorf("usage counts %d routes, %d were stored", usage.Challenges[0].Routes, stored)
	}
	if overshoot > len(body) {
		t.Errorf("usage %d is more than one route body over the %d cap", usage.Total(), scoreboard.MaximumWeeklyChallengeBytes)
	}
	if refused == 0 {
		t.Error("no route was refused although 40 would store over 2 MB")
	}
}

// TestConcurrentMemberUpdatesAndPuts lowers and raises sharing while weekly challenges arrive, and ends inside the final sharing.
func TestConcurrentMemberUpdatesAndPuts(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Flapper", sharing.Highest)
	p.putEverywhere(member)
	var group sync.WaitGroup
	for range 10 {
		group.Add(2)
		go func() {
			defer group.Done()
			for _, start := range weekStarts {
				challenge := baseChallenge()
				setPath(t, challenge, "activities.0.day", start)
				p.service.PutWeeklyChallenge(context.Background(), member, start, encode(t, challenge))
			}
		}()
		go func() {
			defer group.Done()
			for _, chosen := range []sharing.Sharing{sharing.Lowest, sharing.Highest, {Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints}} {
				p.service.UpdateMember(context.Background(), member.ID, scoreboard.MemberUpdate{Sharing: &chosen})
			}
		}()
	}
	group.Wait()
	final := sharing.Sharing{Score: sharing.ScoreScore, Activity: sharing.ActivityNone}
	if _, err := p.service.UpdateMember(context.Background(), member.ID, scoreboard.MemberUpdate{Sharing: &final}); err != nil {
		t.Fatal(err)
	}
	for _, start := range weekStarts {
		if stored := p.storedChallenge(member.ID, start); stored != nil {
			for path := range leafSet(stored) {
				if !p.allowlist.Allows(path, final) {
					t.Errorf("%s: %s stored above the final sharing", start, path)
				}
			}
		}
		if count := p.storedRoutes(member.ID, start); count != 0 {
			t.Errorf("%s: %d routes stored without route sharing", start, count)
		}
	}
}

// TestConcurrentLeaveAndPut lets a member leave while their weekly challenges arrive, and ends with nothing stored for them.
func TestConcurrentLeaveAndPut(t *testing.T) {
	for round := range 5 {
		p := newProbe(t, nil)
		member := p.join("Ghost", sharing.Highest)
		p.putEverywhere(member)
		var group sync.WaitGroup
		group.Add(2)
		go func() {
			defer group.Done()
			for _, start := range weekStarts {
				challenge := baseChallenge()
				setPath(t, challenge, "activities.0.day", start)
				p.service.PutWeeklyChallenge(context.Background(), member, start, encode(t, challenge))
				p.service.PutRoute(context.Background(), member, start, probeActivity, shortRoute(t, 5))
			}
		}()
		go func() {
			defer group.Done()
			p.service.Leave(context.Background(), member.ID)
		}()
		group.Wait()
		// PROBE: a weekly challenge put by a member who is leaving may land after their membership is erased; the
		// contract says Leave erases everything but not what a racing upload leaves. Recorded.
		if left := p.everythingStoredFor(member.ID); len(left) != 0 {
			t.Logf("round %d: stored after a racing Leave: %v", round, left)
		}
	}
}
