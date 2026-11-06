package api_test

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
)

// meFields are the fields the Me schema requires.
var meFields = []string{"id", "displayName", "status", "sharing", "effectiveSharing", "hasAvatar", "joinedAt"}

// TestJoinAnswersWithACompleteMembership checks the 201 body against the Me schema.
func TestJoinAnswersWithACompleteMembership(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	result := member.join("  Ada Lovelace  ", sharing.Highest)
	expectStatus(t, result, http.StatusCreated)
	me := result.object(t)
	for _, field := range meFields {
		if _, ok := me[field]; !ok {
			t.Errorf("Me lacks %q", field)
		}
	}
	if me["displayName"] != "Ada Lovelace" {
		t.Errorf("displayName = %q, want trimmed", me["displayName"])
	}
	if me["status"] != "active" {
		t.Errorf("status = %v on an open board", me["status"])
	}
	if me["id"] != member.id() {
		t.Errorf("id = %v, want %s", me["id"], member.id())
	}
	if _, ok := me["publicKey"]; ok {
		t.Errorf("Me carries the public key, which the schema does not list")
	}
	if joined, _ := me["joinedAt"].(string); !strings.HasSuffix(joined, "Z") && !strings.Contains(joined, "+") {
		t.Errorf("joinedAt %q is not RFC 3339 with an offset", joined)
	}
}

// TestRejoiningReturnsTheExistingMembershipUnchanged checks a known key gets 200 and nothing applied.
func TestRejoiningReturnsTheExistingMembershipUnchanged(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	first := member.join("First", sharing.Lowest)
	expectStatus(t, first, http.StatusCreated)
	again := member.join("Second", sharing.Highest)
	expectStatus(t, again, http.StatusOK)
	if !reflect.DeepEqual(first.object(t), again.object(t)) {
		t.Errorf("rejoin changed the membership:\n%s\n%s", first.body, again.body)
	}
	// Even with a wrong invite the existing member must land where they were, or be told the invite is wrong;
	// the spec says "A key the board already knows gets its existing membership back", so record which.
	body := member.joinBody("Third", sharing.Lowest)
	body["inviteCode"] = "wrong"
	wrongInvite := member.json("POST", "/members", body)
	if wrongInvite.status != http.StatusOK && wrongInvite.status != http.StatusForbidden {
		t.Errorf("rejoin with a wrong invite answered %d: %s", wrongInvite.status, wrongInvite.body)
	}
	t.Logf("PROBE: rejoin with a wrong invite answers %d %s", wrongInvite.status, wrongInvite.code())
}

// TestJoinRefusalsAreCheckedInOrder checks inviteInvalid, blocked, passwordIncorrect, pendingLimitReached.
func TestJoinRefusalsAreCheckedInOrder(t *testing.T) {
	b := newBoard(t, boardOptions{configure: func(document *boardDocument) {
		document.JoinMode = scoreboard.JoinApproval
		document.MaximumPending = 1
	}})
	if err := b.service.SetBoardPassword(t.Context(), "open sesame"); err != nil {
		t.Fatal(err)
	}
	blocked := b.newPhone()
	body := blocked.joinBody("Blocked", sharing.Lowest)
	body["password"] = "open sesame"
	expectStatus(t, blocked.json("POST", "/members", body), http.StatusCreated)
	if err := b.service.DeleteMember(t.Context(), blocked.id(), true); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		// name describes the request.
		name string
		// who sends it.
		who *phone
		// invite is the invite code sent.
		invite string
		// password is the board password sent.
		password string
		// code is the expected problem code.
		code string
	}{
		{"wrong invite beats blocked and wrong password", blocked, "nope", "nope", "inviteInvalid"},
		{"blocked beats wrong password", blocked, probeInvite, "nope", "blocked"},
		{"blocked with right password", blocked, probeInvite, "open sesame", "blocked"},
		{"wrong password", b.newPhone(), probeInvite, "nope", "passwordIncorrect"},
		{"missing password", b.newPhone(), probeInvite, "", "passwordIncorrect"},
		{"invite case differs", b.newPhone(), strings.ToLower(probeInvite), "open sesame", "inviteInvalid"},
		{"invite with trailing space", b.newPhone(), probeInvite + " ", "open sesame", "inviteInvalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := c.who.joinBody("Someone", sharing.Lowest)
			body["inviteCode"] = c.invite
			if c.password != "" {
				body["password"] = c.password
			}
			expectProblem(t, c.who.json("POST", "/members", body), http.StatusForbidden, c.code)
		})
	}

	// Fill the one pending slot, then the next good request hits pendingLimitReached.
	waiting := b.newPhone()
	body = waiting.joinBody("Waiting", sharing.Lowest)
	body["password"] = "open sesame"
	result := waiting.json("POST", "/members", body)
	expectStatus(t, result, http.StatusCreated)
	if result.object(t)["status"] != "pending" {
		t.Errorf("status = %v on an approval board", result.object(t)["status"])
	}
	late := b.newPhone()
	body = late.joinBody("Late", sharing.Lowest)
	body["password"] = "open sesame"
	expectProblem(t, late.json("POST", "/members", body), http.StatusForbidden, "pendingLimitReached")
	// A wrong password at the limit is still passwordIncorrect, the order in the spec.
	body["password"] = "nope"
	expectProblem(t, late.json("POST", "/members", body), http.StatusForbidden, "passwordIncorrect")
	// The waiting member rejoining still gets their membership back.
	body = waiting.joinBody("Waiting", sharing.Lowest)
	body["password"] = "open sesame"
	expectStatus(t, waiting.json("POST", "/members", body), http.StatusOK)
	// The board says it is no longer accepting requests.
	if accepting := (&phone{board: b}).get("/board", nil).object(t)["acceptingRequests"]; accepting != false {
		t.Errorf("acceptingRequests = %v at the pending limit", accepting)
	}
}

// TestPendingLimitHoldsUnderConcurrentJoins checks the approval queue never overshoots its limit.
func TestPendingLimitHoldsUnderConcurrentJoins(t *testing.T) {
	b := newBoard(t, boardOptions{configure: func(document *boardDocument) {
		document.JoinMode = scoreboard.JoinApproval
		document.MaximumPending = 3
	}})
	const joiners = 24
	var wg sync.WaitGroup
	created := make([]bool, joiners)
	for i := range created {
		member := b.newPhone()
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			created[i] = member.join(fmt.Sprintf("Joiner %d", i), sharing.Lowest).status == http.StatusCreated
		}(i)
	}
	wg.Wait()
	count := 0
	for _, ok := range created {
		if ok {
			count++
		}
	}
	pending, err := b.service.PendingCount(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if count > 3 || pending > 3 {
		t.Fatalf("%d joins succeeded and %d are pending with a limit of 3", count, pending)
	}
}

// TestJoinValidation checks the JoinRequest schema's constraints answer badRequest.
func TestJoinValidation(t *testing.T) {
	b := newBoard(t, boardOptions{})
	cases := []struct {
		// name describes the body.
		name string
		// change edits a valid join body.
		change func(body map[string]any)
	}{
		{"display name empty", func(body map[string]any) { body["displayName"] = "" }},
		{"display name spaces only", func(body map[string]any) { body["displayName"] = "   " }},
		{"display name 41 characters", func(body map[string]any) { body["displayName"] = strings.Repeat("a", 41) }},
		{"display name with newline", func(body map[string]any) { body["displayName"] = "Ada\nLovelace" }},
		{"display name with zero width space", func(body map[string]any) { body["displayName"] = "Ada​Lovelace" }},
		{"display name with right to left override", func(body map[string]any) { body["displayName"] = "Ada‮Lovelace" }},
		{"display name missing", func(body map[string]any) { delete(body, "displayName") }},
		{"display name not a string", func(body map[string]any) { body["displayName"] = 12 }},
		{"sharing missing", func(body map[string]any) { delete(body, "sharing") }},
		{"sharing score unknown", func(body map[string]any) {
			body["sharing"] = map[string]any{"score": "everything", "activity": "none", "routes": false, "strengthSets": false, "strengthWeights": false}
		}},
		{"sharing activity unknown", func(body map[string]any) {
			body["sharing"] = map[string]any{"score": "details", "activity": "all", "routes": false, "strengthSets": false, "strengthWeights": false}
		}},
		{"sharing score uppercase", func(body map[string]any) {
			body["sharing"] = map[string]any{"score": "Details", "activity": "none", "routes": false, "strengthSets": false, "strengthWeights": false}
		}},
		{"sharing routes as string", func(body map[string]any) {
			body["sharing"] = map[string]any{"score": "details", "activity": "full", "routes": "true", "strengthSets": false, "strengthWeights": false}
		}},
		{"invite missing", func(body map[string]any) { delete(body, "inviteCode") }},
		{"invite 65 characters", func(body map[string]any) { body["inviteCode"] = strings.Repeat("x", 65) }},
		{"public key missing", func(body map[string]any) { delete(body, "publicKey") }},
		{"public key padded", func(body map[string]any) { body["publicKey"] = body["publicKey"].(string) + "=" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			member := b.newPhone()
			body := member.joinBody("Valid Name", sharing.Lowest)
			c.change(body)
			result := member.json("POST", "/members", body)
			if result.status == http.StatusCreated {
				t.Fatalf("accepted: %s", result.body)
			}
			if result.status != http.StatusBadRequest && result.status != http.StatusUnauthorized && result.status != http.StatusForbidden {
				t.Fatalf("got %d %s, want 400 badRequest", result.status, result.body)
			}
			if result.status != http.StatusBadRequest {
				t.Logf("PROBE: answered %d %s rather than 400", result.status, result.code())
			}
		})
	}
}

// TestJoinBodyShapes checks malformed bodies and media types on POST /members.
func TestJoinBodyShapes(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	valid, _ := jsonBytes(member.joinBody("Shape", sharing.Lowest))
	cases := []struct {
		// name describes the body.
		name string
		// body is the raw body.
		body []byte
		// contentType is the Content-Type sent.
		contentType string
		// statuses are the acceptable answers.
		statuses []int
	}{
		{"no content type", valid, "", []int{http.StatusCreated, http.StatusUnsupportedMediaType, http.StatusBadRequest}},
		{"json with charset", valid, "application/json; charset=utf-8", []int{http.StatusCreated}},
		{"json uppercase type", valid, "Application/JSON", []int{http.StatusCreated}},
		{"text plain", valid, "text/plain", []int{http.StatusUnsupportedMediaType, http.StatusBadRequest}},
		{"empty body", nil, "application/json", []int{http.StatusBadRequest}},
		{"null", []byte("null"), "application/json", []int{http.StatusBadRequest}},
		{"array", []byte("[]"), "application/json", []int{http.StatusBadRequest}},
		{"truncated", valid[:len(valid)/2], "application/json", []int{http.StatusBadRequest}},
		{"trailing garbage", append(append([]byte{}, valid...), []byte("xyz")...), "application/json", []int{http.StatusBadRequest}},
		{"two documents", append(append([]byte{}, valid...), valid...), "application/json", []int{http.StatusBadRequest}},
		{"byte order mark", append([]byte("\xef\xbb\xbf"), valid...), "application/json", []int{http.StatusBadRequest, http.StatusCreated}},
		{"deeply nested", []byte(strings.Repeat("[", 100000) + strings.Repeat("]", 100000)), "application/json", []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fresh := b.newPhone()
			body := c.body
			if string(body) == string(valid) || strings.HasPrefix(string(body), "\xef\xbb\xbf") || (len(body) > len(valid) && string(body[:len(valid)]) == string(valid)) {
				// Re-sign with a fresh key by rebuilding the body around it.
				rebuilt, _ := jsonBytes(fresh.joinBody("Shape", sharing.Lowest))
				body = []byte(strings.Replace(string(body), string(valid), string(rebuilt), -1))
			}
			result := fresh.send("POST", "/members", body, c.contentType)
			for _, status := range c.statuses {
				if result.status == status {
					if status != c.statuses[0] {
						t.Logf("PROBE: answered %d %s", result.status, result.code())
					}
					return
				}
			}
			t.Fatalf("got %d %s, want one of %v", result.status, result.body, c.statuses)
		})
	}
}

// jsonBytes marshals value, returning the bytes and any error.
func jsonBytes(value any) ([]byte, error) {
	return jsonMarshal(value)
}

// TestUnknownFieldsAreIgnored checks both sides may add optional fields.
func TestUnknownFieldsAreIgnored(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	body := member.joinBody("Future", sharing.Lowest)
	body["futureField"] = map[string]any{"deep": []any{1, 2, 3}}
	body["sharing"] = map[string]any{"score": "progress", "activity": "none", "routes": false, "strengthSets": false, "strengthWeights": false, "newOption": true}
	expectStatus(t, member.json("POST", "/members", body), http.StatusCreated)
	expectStatus(t, member.json("PATCH", "/members/me", map[string]any{"displayName": "Future", "unknown": 1}), http.StatusOK)
}

// TestPendingMemberReach checks what a pending member can and cannot do.
func TestPendingMemberReach(t *testing.T) {
	b := newBoard(t, boardOptions{configure: func(document *boardDocument) { document.JoinMode = scoreboard.JoinApproval }})
	member := b.newPhone()
	result := member.join("Pending", sharing.Highest)
	expectStatus(t, result, http.StatusCreated)
	active := b.newPhone()
	active.mustJoin("Active", sharing.Highest)
	if err := b.service.SetStatus(t.Context(), active.id(), scoreboard.StatusActive); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, active.putWeeklyChallenge(probeWeek, minimalChallenge(0.5)), http.StatusOK)

	expectStatus(t, member.get("/members/me", nil), http.StatusOK)
	expectStatus(t, member.json("PATCH", "/members/me", map[string]any{"displayName": "Still Pending"}), http.StatusOK)
	expectStatus(t, (&phone{board: b}).get("/board", nil), http.StatusOK)
	expectProblem(t, member.putWeeklyChallenge(probeWeek, minimalChallenge(0.5)), http.StatusForbidden, "memberPending")
	expectProblem(t, member.get("/weekly-challenges/"+probeWeek, nil), http.StatusForbidden, "memberPending")
	expectProblem(t, member.get("/members/"+active.id()+"/weekly-challenges/"+probeWeek, nil), http.StatusForbidden, "memberPending")
	expectProblem(t, member.get("/members/"+active.id()+"/avatar", nil), http.StatusForbidden, "memberPending")
	expectProblem(t, member.get("/members/"+active.id()+"/weekly-challenges/"+probeWeek+"/activities/"+activityID(1)+"/route", nil), http.StatusForbidden, "memberPending")
	expectProblem(t, member.send("PUT", "/members/me/avatar", []byte("x"), "image/png"), http.StatusForbidden, "memberPending")
	expectProblem(t, member.json("PUT", "/members/me/weekly-challenges/"+probeWeek+"/activities/"+activityID(1)+"/route", map[string]any{"polyline": "_p~iF~ps|U_ulLnnqC"}), http.StatusForbidden, "memberPending")

	// A pending member is not on the scoreboard and their weekly challenge cannot be fetched.
	board := active.get("/weekly-challenges/"+probeWeek, nil)
	expectStatus(t, board, http.StatusOK)
	if strings.Contains(string(board.body), member.id()) {
		t.Errorf("pending member is on the scoreboard: %s", board.body)
	}
	expectProblem(t, active.get("/members/"+member.id()+"/weekly-challenges/"+probeWeek, nil), http.StatusNotFound, "notFound")

	// Leaving withdraws the request, after which the key is a stranger and can ask again.
	expectStatus(t, member.send("DELETE", "/members/me", nil, ""), http.StatusNoContent)
	expectProblem(t, member.get("/members/me", nil), http.StatusUnauthorized, "unauthenticated")
	expectStatus(t, member.join("Pending Again", sharing.Lowest), http.StatusCreated)
	// A declined request is deleted and the member becomes a stranger.
	if err := b.service.DeleteMember(t.Context(), member.id(), false); err != nil {
		t.Fatal(err)
	}
	expectProblem(t, member.get("/members/me", nil), http.StatusUnauthorized, "unauthenticated")
}

// TestPatchMe checks MemberUpdate's rules.
func TestPatchMe(t *testing.T) {
	b := newBoard(t, boardOptions{configure: func(document *boardDocument) {
		document.MaximumSharing = sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints}
	}})
	member := b.newPhone()
	member.mustJoin("Patch", sharing.Lowest)
	expectProblem(t, member.json("PATCH", "/members/me", map[string]any{}), http.StatusBadRequest, "badRequest")
	expectProblem(t, member.json("PATCH", "/members/me", map[string]any{"unknown": 1}), http.StatusBadRequest, "badRequest")
	expectProblem(t, member.json("PATCH", "/members/me", map[string]any{"displayName": ""}), http.StatusBadRequest, "badRequest")
	expectProblem(t, member.json("PATCH", "/members/me", map[string]any{"displayName": nil}), http.StatusBadRequest, "badRequest")
	expectProblem(t, member.json("PATCH", "/members/me", map[string]any{"sharing": map[string]any{"score": "details"}}), http.StatusBadRequest, "badRequest")
	named := member.json("PATCH", "/members/me", map[string]any{"displayName": " Renamed "})
	expectStatus(t, named, http.StatusOK)
	if named.object(t)["displayName"] != "Renamed" {
		t.Errorf("displayName = %v", named.object(t)["displayName"])
	}
	shared := member.json("PATCH", "/members/me", map[string]any{"sharing": sharing.Highest})
	expectStatus(t, shared, http.StatusOK)
	me := shared.object(t)
	if me["displayName"] != "Renamed" {
		t.Errorf("a sharing change reset the name: %v", me["displayName"])
	}
	chosen, _ := me["sharing"].(map[string]any)
	effective, _ := me["effectiveSharing"].(map[string]any)
	if chosen["activity"] != "full" {
		t.Errorf("sharing = %v, want what the member chose", chosen)
	}
	if effective["activity"] != "points" || effective["routes"] != false {
		t.Errorf("effectiveSharing = %v, want capped by the board's maximum", effective)
	}
	// Sharing that does not apply reads as its lowest value.
	odd := member.json("PATCH", "/members/me", map[string]any{"sharing": map[string]any{
		"score": "progress", "activity": "full", "routes": true, "strengthSets": false, "strengthWeights": true}})
	expectStatus(t, odd, http.StatusOK)
	chosen, _ = odd.object(t)["sharing"].(map[string]any)
	if chosen["activity"] != "none" || chosen["routes"] != false || chosen["strengthWeights"] != false {
		t.Errorf("sharing not normalised: %v", chosen)
	}
}

// TestLeavingErasesEverythingAndFreesTheKey checks DELETE /members/me and a fresh join afterwards.
func TestLeavingErasesEverythingAndFreesTheKey(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Leaver", sharing.Highest)
	expectStatus(t, member.putWeeklyChallenge(probeWeek, everythingChallenge("2026-10-20", activityID(1))), http.StatusOK)
	expectStatus(t, member.json("PUT", "/members/me/weekly-challenges/"+probeWeek+"/activities/"+activityID(1)+"/route", map[string]any{"polyline": "_p~iF~ps|U_ulLnnqC"}), http.StatusNoContent)
	expectStatus(t, member.send("PUT", "/members/me/avatar", pngBytes(t, 8, 8), "image/png"), http.StatusNoContent)
	other := b.newPhone()
	other.mustJoin("Other", sharing.Lowest)

	expectStatus(t, member.send("DELETE", "/members/me", nil, ""), http.StatusNoContent)
	expectProblem(t, member.get("/members/me", nil), http.StatusUnauthorized, "unauthenticated")
	expectProblem(t, other.get("/members/"+member.id()+"/weekly-challenges/"+probeWeek, nil), http.StatusNotFound, "notFound")
	expectProblem(t, other.get("/members/"+member.id()+"/avatar", nil), http.StatusNotFound, "notFound")
	expectProblem(t, other.get("/members/"+member.id()+"/weekly-challenges/"+probeWeek+"/activities/"+activityID(1)+"/route", nil), http.StatusNotFound, "notFound")
	if board := other.get("/weekly-challenges/"+probeWeek, nil); strings.Contains(string(board.body), member.id()) {
		t.Errorf("leaver still on the scoreboard: %s", board.body)
	}
	usage, err := b.service.Usage(t.Context(), member.id())
	if err == nil && usage.Total() != 0 {
		t.Errorf("leaver still stores %d bytes", usage.Total())
	}

	// The key may join again, as a brand new member with nothing left over.
	rejoin := member.join("Returned", sharing.Lowest)
	expectStatus(t, rejoin, http.StatusCreated)
	me := rejoin.object(t)
	if me["displayName"] != "Returned" || me["hasAvatar"] != false {
		t.Errorf("rejoined membership carries old state: %s", rejoin.body)
	}
	expectProblem(t, other.get("/members/"+member.id()+"/weekly-challenges/"+probeWeek, nil), http.StatusNotFound, "notFound")
}

// TestMembersMeMethods checks methods the path does not define are refused without a crash.
func TestMembersMeMethods(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Methods", sharing.Lowest)
	for _, method := range []string{"POST", "PUT", "OPTIONS", "TRACE"} {
		result := member.send(method, "/members/me", nil, "")
		if result.status != http.StatusMethodNotAllowed && result.status != http.StatusNotFound {
			t.Errorf("%s /members/me answered %d: %s", method, result.status, result.body)
		}
		if result.status == http.StatusMethodNotAllowed && result.header.Get("Allow") == "" {
			t.Errorf("%s /members/me answered 405 without Allow", method)
		}
	}
	head := member.send("HEAD", "/members/me", nil, "")
	if head.status != http.StatusOK && head.status != http.StatusMethodNotAllowed && head.status != http.StatusNotFound {
		t.Errorf("HEAD answered %d", head.status)
	}
	if len(head.body) != 0 {
		t.Errorf("HEAD carried a body")
	}
}
