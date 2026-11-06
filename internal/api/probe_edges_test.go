package api_test

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
)

// TestConcurrentJoinsWithOneKeyMakeOneMembership checks a key racing itself joins once.
func TestConcurrentJoinsWithOneKeyMakeOneMembership(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	const attempts = 12
	statuses := make([]int, attempts)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = member.join("Twin", sharing.Lowest).status
		}(i)
	}
	wg.Wait()
	created, returned := 0, 0
	for _, status := range statuses {
		switch status {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			returned++
		default:
			t.Errorf("a concurrent join answered %d", status)
		}
	}
	if created != 1 {
		t.Errorf("%d joins created a membership, want 1", created)
	}
	members, err := b.service.Members(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 {
		t.Errorf("%d members after one key joined, want 1", len(members))
	}
}

// TestInvalidUTF8IsRefusedNotStored checks bytes that are not UTF-8 answer badRequest, not 500 or storage.
func TestInvalidUTF8IsRefusedNotStored(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	body := []byte(`{"inviteCode":"` + probeInvite + `","publicKey":"` + member.publicKey() + `","displayName":"Ad\xffa","sharing":{"score":"progress","activity":"none","routes":false,"strengthSets":false,"strengthWeights":false}}`)
	result := member.send("POST", "/members", body, "application/json")
	if result.status == http.StatusCreated {
		me := result.object(t)
		t.Errorf("invalid UTF-8 accepted and stored as %q", me["displayName"])
		return
	}
	expectProblem(t, result, http.StatusBadRequest, "badRequest")
	member.mustJoin("Valid", sharing.Highest)
	challenge := []byte(`{"progress":0.5,"tier":"mon\xffster"}`)
	result = member.send("PUT", "/members/me/weekly-challenges/"+probeWeek, challenge, "application/json")
	if result.status == http.StatusOK {
		t.Errorf("invalid UTF-8 in a weekly challenge accepted: %s", result.body)
	} else {
		expectProblem(t, result, http.StatusBadRequest, "badRequest")
	}
}

// TestCompressedRequestBodiesAreNotSilentlyMisread checks a gzip request body is refused cleanly.
func TestCompressedRequestBodiesAreNotSilentlyMisread(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Zip", sharing.Lowest)
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	writer.Write([]byte(`{"displayName":"Zipped"}`))
	writer.Close()
	r := member.prepare("PATCH", "/members/me", buffer.Bytes(), "application/json")
	r.header.Set("Content-Encoding", "gzip")
	result := member.dispatch(r)
	if result.status == http.StatusOK {
		t.Errorf("a gzip request body was accepted; it must have been inflated, which the API does not promise")
	} else if result.status >= 500 {
		t.Errorf("a gzip request body crashed the operation: %d %s", result.status, result.body)
	}
}

// TestOversizedBodiesOnSmallOperations checks every JSON operation has a body limit and answers 413.
func TestOversizedBodiesOnSmallOperations(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Big", sharing.Highest)
	expectStatus(t, member.putWeeklyChallenge(probeWeek, everythingChallenge("2026-10-20", activityID(1))), http.StatusOK)
	padding := strings.Repeat("p", 2<<20)
	cases := []struct {
		// name describes the operation.
		name string
		// method is the HTTP method.
		method string
		// path is the path under /api/v1.
		path string
		// body is the oversized body.
		body string
	}{
		{"patch me", "PATCH", "/members/me", `{"displayName":"Big","pad":"` + padding + `"}`},
		{"join", "POST", "/members", `{"inviteCode":"` + probeInvite + `","publicKey":"` + member.publicKey() + `","displayName":"Big","sharing":{"score":"progress","activity":"none","routes":false,"strengthSets":false,"strengthWeights":false},"pad":"` + padding + `"}`},
		{"route", "PUT", routePath(activityID(1)), `{"polyline":"` + examplePolyline + `","pad":"` + padding + `"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := member.send(c.method, c.path, []byte(c.body), "application/json")
			if result.status < 400 {
				t.Fatalf("a 2 MB body was accepted: %d", result.status)
			}
			if result.status != http.StatusRequestEntityTooLarge {
				t.Logf("PROBE: answered %d %s rather than 413", result.status, result.code())
			}
		})
	}
}

// TestBoardReflectsAdminChanges checks GET /board follows the password, join mode and maximum sharing.
func TestBoardReflectsAdminChanges(t *testing.T) {
	b := newBoard(t, boardOptions{})
	stranger := &phone{board: b}
	if err := b.service.SetBoardPassword(t.Context(), "correct horse"); err != nil {
		t.Fatal(err)
	}
	b.update(func(document *boardDocument) {
		document.JoinMode = "approval"
		document.MaximumSharing = sharing.Sharing{Score: sharing.ScoreScore, Activity: sharing.ActivityNone}
		document.Ranking = "name"
		document.RetentionWeeks = 3
	})
	document := stranger.get("/board", nil).object(t)
	if document["passwordRequired"] != true || document["joinMode"] != "approval" || document["ranking"] != "name" || document["retentionWeeks"] != float64(3) {
		t.Errorf("board = %v", document)
	}
	maximum, _ := document["maximumSharing"].(map[string]any)
	if maximum["score"] != "score" || maximum["activity"] != "none" {
		t.Errorf("maximumSharing = %v", maximum)
	}
	// Joining now needs the password.
	member := b.newPhone()
	expectProblem(t, member.join("Pass", sharing.Lowest), http.StatusForbidden, "passwordIncorrect")
	body := member.joinBody("Pass", sharing.Lowest)
	body["password"] = "correct horse"
	result := member.json("POST", "/members", body)
	expectStatus(t, result, http.StatusCreated)
	if result.object(t)["status"] != "pending" {
		t.Errorf("status = %v after switching to approval", result.object(t)["status"])
	}
	// Removing the password opens the board again.
	if err := b.service.SetBoardPassword(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	if document := stranger.get("/board", nil).object(t); document["passwordRequired"] != false {
		t.Errorf("passwordRequired still true")
	}
}

// TestLoweringTheBoardMaximumStripsEveryMember checks the admin's lowering reaches stored weekly challenges at once.
func TestLoweringTheBoardMaximumStripsEveryMember(t *testing.T) {
	b := newBoard(t, boardOptions{})
	viewer := b.newPhone()
	viewer.mustJoin("Viewer", sharing.Lowest)
	var members []*phone
	for i := 0; i < 3; i++ {
		member := b.newPhone()
		member.mustJoin("Member", sharing.Highest)
		expectStatus(t, member.putWeeklyChallenge(probeWeek, everythingChallenge("2026-10-20", activityID(i))), http.StatusOK)
		members = append(members, member)
	}
	b.update(func(document *boardDocument) {
		document.MaximumSharing = sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, StrengthSets: true}
	})
	list := loadSpecAllowlist(t)
	lowered := sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, StrengthSets: true}
	for _, member := range members {
		read := viewer.get("/members/"+member.id()+"/weekly-challenges/"+probeWeek, nil)
		expectStatus(t, read, http.StatusOK)
		var memberWeeklyChallenge struct {
			Challenge map[string]any `json:"challenge"`
		}
		read.decode(t, &memberWeeklyChallenge)
		var leaves []string
		leafPaths(memberWeeklyChallenge.Challenge, "", &leaves)
		for _, leaf := range leaves {
			if !list.allows(leaf, lowered) {
				t.Errorf("after lowering the maximum, %s is still served", leaf)
			}
		}
		if !containsPath(leaves, "activities[].strength.exercises[].sets[].reps") {
			t.Errorf("sets were dropped although the maximum keeps strengthSets: %s", read.body)
		}
	}
	// Raising the maximum again restores nothing.
	b.update(func(document *boardDocument) { document.MaximumSharing = sharing.Highest })
	read := viewer.get("/members/"+members[0].id()+"/weekly-challenges/"+probeWeek, nil)
	if strings.Contains(string(read.body), "weightKilograms") {
		t.Errorf("raising the maximum restored stripped data")
	}
}

// TestRetentionSweepRemovesOldChallengesFromReads checks weekly challenges past retention vanish after the clock moves on.
func TestRetentionSweepRemovesOldChallengesFromReads(t *testing.T) {
	b := newBoard(t, boardOptions{configure: func(document *boardDocument) { document.RetentionWeeks = 2 }})
	member := b.newPhone()
	member.mustJoin("Old", sharing.Highest)
	expectStatus(t, member.putWeeklyChallenge("2026-10-12", everythingChallenge("2026-10-13", activityID(1))), http.StatusOK)
	expectStatus(t, member.putWeeklyChallenge("2026-10-19", everythingChallenge("2026-10-20", activityID(2))), http.StatusOK)
	expectStatus(t, member.json("PUT", "/members/me/weekly-challenges/2026-10-12/activities/"+activityID(1)+"/route", map[string]any{"polyline": examplePolyline}), http.StatusNoContent)
	b.advance(7 * 24 * timeSeconds(3600))
	if err := b.service.SweepRetention(t.Context()); err != nil {
		t.Fatal(err)
	}
	old := member.get("/members/"+member.id()+"/weekly-challenges/2026-10-12", nil)
	if old.status == http.StatusOK {
		t.Errorf("a weekly challenge past retention is still served")
	}
	route := member.get("/members/"+member.id()+"/weekly-challenges/2026-10-12/activities/"+activityID(1)+"/route", nil)
	if route.status == http.StatusOK {
		t.Errorf("a route past retention is still served")
	}
	expectStatus(t, member.get("/members/"+member.id()+"/weekly-challenges/2026-10-19", nil), http.StatusOK)
	usage, err := b.service.Usage(t.Context(), member.id())
	if err != nil {
		t.Fatal(err)
	}
	for _, challenge := range usage.Challenges {
		if challenge.WeekStart == "2026-10-12" {
			t.Errorf("the swept weekly challenge still stores %d bytes", challenge.Total())
		}
	}
}

// TestConcurrentPatchesNeverFail checks a member racing itself on PATCH /members/me gets 200 every time.
func TestConcurrentPatchesNeverFail(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Racer", sharing.Lowest)
	const writers = 48
	failed, total := 0, 0
	for round := 0; round < 3; round++ {
		statuses := make([]int, writers)
		var wg sync.WaitGroup
		for i := range statuses {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				statuses[i] = member.json("PATCH", "/members/me", map[string]any{"displayName": "Name " + string(rune('A'+i%26))}).status
			}(i)
		}
		wg.Wait()
		for _, status := range statuses {
			total++
			if status != http.StatusOK {
				failed++
			}
		}
	}
	if failed > 0 {
		t.Fatalf("%d of %d concurrent PATCH requests failed (not 200)", failed, total)
	}
}

// TestWrongShapedLeavesAreRefusedNotStored checks an object where the schema has a number is refused, so nothing
// can be tunnelled through an allowed field name.
func TestWrongShapedLeavesAreRefusedNotStored(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Tunnel", sharing.Highest)
	viewer := b.newPhone()
	viewer.mustJoin("Viewer", sharing.Lowest)
	// At Lowest sharing the same bodies are stripped before decoding, so the wrong shape is dropped rather than
	// refused; nothing is stored either way, which TestPutWeeklyChallengeKeepsExactlyWhatSharingAllows covers.
	for _, body := range []string{
		`{"progress":{"secret":"payload"}}`,
		`{"progress":[0.5]}`,
		`{"progress":0.5,"score":{"x":1}}`,
		`{"progress":0.5,"tier":{"x":1}}`,
		`{"progress":0.5,"breakdown":{"exercise":{"x":1},"dailyBonus":0,"varietyBonus":0}}`,
	} {
		result := member.send("PUT", "/members/me/weekly-challenges/"+probeWeek, []byte(body), "application/json")
		if result.status == http.StatusOK {
			t.Errorf("%s accepted: %s", body, result.body)
			read := viewer.get("/members/"+member.id()+"/weekly-challenges/"+probeWeek, nil)
			if strings.Contains(string(read.body), "secret") {
				t.Errorf("tunnelled payload served: %s", read.body)
			}
			continue
		}
		expectProblem(t, result, http.StatusBadRequest, "badRequest")
	}
}
