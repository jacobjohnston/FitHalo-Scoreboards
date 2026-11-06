package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/auth"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/password"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/ratelimit"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
	"github.com/jacobjohnston/FitHalo-Scoreboards/spec"
)

const (
	// thisWeek is the week the harness's clock is in.
	thisWeek = "2026-10-19"

	// inviteCode is every test board's invite code.
	inviteCode = "InviteCodeInviteCode00"
)

// full shares everything.
var full = sharing.Highest

// pointsOnly shares score details and activity points, and nothing finer.
var pointsOnly = sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints}

// harness is a board served over HTTP from a real directory store, with a clock the test controls.
type harness struct {
	// t is the test the harness fails.
	t *testing.T

	// server serves the API.
	server *httptest.Server

	// service is the board behind server, for setup the API cannot do.
	service *scoreboard.Service

	// root is the directory store's folder.
	root string

	// mutex guards now.
	mutex sync.Mutex

	// now is the board's clock.
	now time.Time
}

// newHarness serves an open board, letting configure change it before it is created.
func newHarness(t *testing.T, configure func(*scoreboard.Board)) *harness {
	t.Helper()
	root := t.TempDir()
	directory, err := store.OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { directory.Close() })
	allowlist, err := sharing.LoadAllowlist(spec.Sharing)
	if err != nil {
		t.Fatal(err)
	}

	testHarness := &harness{t: t, root: root, now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	testHarness.service = &scoreboard.Service{Store: directory, Allowlist: allowlist, Now: testHarness.clock}
	board, err := scoreboard.DefaultBoard()
	if err != nil {
		t.Fatal(err)
	}
	board.JoinMode = scoreboard.JoinOpen
	board.InviteCode = inviteCode
	// The tests' dates are Mondays; a new board starts on Sunday, as FitHalo's own weeks do.
	board.WeekStartDay = "monday"
	if configure != nil {
		configure(&board)
	}
	if _, _, err := testHarness.service.EnsureBoard(context.Background(), board); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		Service:       testHarness.service,
		Nonces:        auth.NewNonceCache(),
		Limiter:       ratelimit.New(1000, 1000),
		JoinLimiter:   ratelimit.New(1000, 1000),
		ServerVersion: "test",
	}
	testHarness.server = httptest.NewServer(server.Handler(""))
	t.Cleanup(testHarness.server.Close)
	return testHarness
}

// clock returns the board's time.
func (h *harness) clock() time.Time {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return h.now
}

// advance moves the board's clock on by duration.
func (h *harness) advance(duration time.Duration) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.now = h.now.Add(duration)
}

// client is a member's phone: a key, and requests signed with it.
type client struct {
	// harness is the board the client talks to.
	harness *harness

	// key signs the client's requests; nil sends them unsigned.
	key *ecdsa.PrivateKey
}

// newClient returns a client with a new key.
func (h *harness) newClient() *client {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		h.t.Fatal(err)
	}
	return &client{harness: h, key: key}
}

// id returns the member identifier the client's key derives.
func (c *client) id() string {
	_, raw, _ := auth.ParsePublicKey(auth.EncodePublicKey(&c.key.PublicKey))
	return auth.MemberID(raw)
}

// response is an answer read in full.
type response struct {
	// status is the HTTP status.
	status int

	// header is the response's headers.
	header http.Header

	// body is the whole body.
	body []byte
}

// decode unmarshals the body into value, failing the test when it cannot.
func (r response) decode(t *testing.T, value any) {
	t.Helper()
	if err := json.Unmarshal(r.body, value); err != nil {
		t.Fatalf("decoding %s: %v", r.body, err)
	}
}

// code returns the body's problem code.
func (r response) code(t *testing.T) string {
	t.Helper()
	var failure struct {
		Code string `json:"code"`
	}
	r.decode(t, &failure)
	return failure.Code
}

// send makes a request to path under /api/v1, signed when the client has a key.
func (c *client) send(method, path string, body []byte, contentType string, header http.Header) response {
	c.harness.t.Helper()
	target := "/api/v1" + path
	request, err := http.NewRequest(method, c.harness.server.URL+target, bytes.NewReader(body))
	if err != nil {
		c.harness.t.Fatal(err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if c.key != nil {
		authorization, err := auth.AuthorizationValue(c.key, method, target, c.harness.clock(), body)
		if err != nil {
			c.harness.t.Fatal(err)
		}
		request.Header.Set("Authorization", authorization)
	}
	for key, values := range header {
		request.Header[key] = values
	}
	return do(c.harness.t, request)
}

// do sends request and reads the whole response.
func do(t *testing.T, request *http.Request) response {
	t.Helper()
	result, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	body, _ := io.ReadAll(result.Body)
	return response{status: result.StatusCode, header: result.Header, body: body}
}

// json sends value as a signed JSON body.
func (c *client) json(method, path string, value any) response {
	c.harness.t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		c.harness.t.Fatal(err)
	}
	return c.send(method, path, encoded, "application/json", nil)
}

// join asks to join the board with the invite, a display name and sharing.
func (c *client) join(name string, chosen sharing.Sharing) response {
	c.harness.t.Helper()
	return c.json("POST", "/members", map[string]any{
		"inviteCode":  inviteCode,
		"publicKey":   auth.EncodePublicKey(&c.key.PublicKey),
		"displayName": name,
		"sharing":     chosen,
	})
}

// expectStatus fails the test unless result has status.
func expectStatus(t *testing.T, result response, status int) {
	t.Helper()
	if result.status != status {
		t.Fatalf("got status %d, want %d: %s", result.status, status, result.body)
	}
}

// expectProblem fails the test unless result is a problem with status and code.
func expectProblem(t *testing.T, result response, status int, code string) {
	t.Helper()
	expectStatus(t, result, status)
	if got := result.code(t); got != code {
		t.Fatalf("got code %q, want %q", got, code)
	}
	if contentType := result.header.Get("Content-Type"); contentType != "application/problem+json" {
		t.Fatalf("problem served as %q", contentType)
	}
}

// fullChallenge is a weekly challenge with an activity carrying every kind of field.
func fullChallenge(activityID string) map[string]any {
	return map[string]any{
		"progress": 0.8, "score": 400, "goal": 500, "tier": "monster",
		"breakdown": map[string]any{"exercise": 350, "dailyBonus": 30, "varietyBonus": 20},
		"activities": []any{map[string]any{
			"id": activityID, "type": "outdoorRucking", "day": "2026-10-20",
			"points":          map[string]any{"base": 30, "bonus": 5},
			"durationSeconds": 3600, "distanceMeters": 5000, "start": "2026-10-20T07:30:00-07:00",
			"details":  map[string]any{"averageHeartRate": 130, "carriedLoadKilograms": 13.6},
			"hasRoute": true,
			"strength": map[string]any{"exercises": []any{map[string]any{
				"name": "Farmer's Carry",
				"sets": []any{map[string]any{"type": "normal", "reps": 1, "distanceMeters": 40, "weightKilograms": 32}},
			}}},
		}},
	}
}

// activityID is the identifier of fullChallenge's activity in most tests.
const activityID = "ActivityActivityActi00"

// routeBody is a valid route of three points.
const routeBody = `{"polyline":"_p~iF~ps|U_ulLnnqC_mqNvxq` + "`" + `@"}`

// TestJoinPutAndRead walks the whole path: two members join, put their weekly challenges, and read the board.
func TestJoinPutAndRead(t *testing.T) {
	board := newHarness(t, nil)
	first, second := board.newClient(), board.newClient()

	expectStatus(t, first.join("Avery", full), http.StatusCreated)
	expectStatus(t, first.join("Avery again", full), http.StatusOK)
	expectStatus(t, second.join("Blake", sharing.Lowest), http.StatusCreated)

	var me struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	result := first.send("GET", "/members/me", nil, "", nil)
	expectStatus(t, result, http.StatusOK)
	result.decode(t, &me)
	if me.ID != first.id() || me.Status != scoreboard.StatusActive {
		t.Fatalf("me is %+v", me)
	}

	expectStatus(t, first.json("PUT", "/members/me/weekly-challenges/"+thisWeek, fullChallenge(activityID)), http.StatusOK)
	lowered := second.json("PUT", "/members/me/weekly-challenges/"+thisWeek, fullChallenge(activityID))
	expectStatus(t, lowered, http.StatusOK)
	var kept map[string]any
	lowered.decode(t, &kept)
	if len(kept) != 2 || kept["progress"] == nil || kept["updatedAt"] == nil {
		t.Fatalf("a progress-only member's weekly challenge kept %v", kept)
	}

	result = second.send("GET", "/weekly-challenges/"+thisWeek, nil, "", nil)
	expectStatus(t, result, http.StatusOK)
	var scoreboardResult scoreboard.Scoreboard
	result.decode(t, &scoreboardResult)
	if len(scoreboardResult.Entries) != 2 || scoreboardResult.Entries[0].Member.DisplayName != "Avery" {
		t.Fatalf("scoreboard %s", result.body)
	}
	if count := scoreboardResult.Entries[0].Challenge.ActivityCount; count == nil || *count != 1 {
		t.Fatalf("Avery's activity count %v", count)
	}
	if blake := scoreboardResult.Entries[1].Challenge; blake.Score != nil || blake.ActivityCount != nil {
		t.Fatalf("Blake's summary carried more than progress: %+v", blake)
	}

	etag := result.header.Get("ETag")
	unchanged := second.send("GET", "/weekly-challenges/"+thisWeek, nil, "", http.Header{"If-None-Match": {etag}})
	expectStatus(t, unchanged, http.StatusNotModified)

	result = second.send("GET", "/members/"+first.id()+"/weekly-challenges/"+thisWeek, nil, "", nil)
	expectStatus(t, result, http.StatusOK)
	if !strings.Contains(string(result.body), `"weightKilograms":32`) {
		t.Fatalf("Avery shares weights but the weekly challenge came back as %s", result.body)
	}
}

// TestBoardIsPublicAndCarriesNoSecrets reads the board unsigned and finds no invite or password hash.
func TestBoardIsPublicAndCarriesNoSecrets(t *testing.T) {
	hash, err := password.Hash("desk password", nil)
	if err != nil {
		t.Fatal(err)
	}
	board := newHarness(t, func(board *scoreboard.Board) { board.PasswordHash = hash })
	request, _ := http.NewRequest("GET", board.server.URL+"/api/v1/board", nil)
	result := do(t, request)
	expectStatus(t, result, http.StatusOK)
	if strings.Contains(string(result.body), inviteCode) || strings.Contains(string(result.body), "pbkdf2") {
		t.Fatalf("the board leaked a secret: %s", result.body)
	}
	if !strings.Contains(string(result.body), `"passwordRequired":true`) {
		t.Fatalf("the board hid its password requirement: %s", result.body)
	}
}

// TestJoiningRules covers the invite, the password, approval and blocking.
func TestJoiningRules(t *testing.T) {
	hash, err := password.Hash("desk password", nil)
	if err != nil {
		t.Fatal(err)
	}
	board := newHarness(t, func(board *scoreboard.Board) {
		board.PasswordHash = hash
		board.JoinMode = scoreboard.JoinApproval
	})
	member := board.newClient()
	request := map[string]any{
		"inviteCode": "WrongWrongWrongWrong00", "publicKey": auth.EncodePublicKey(&member.key.PublicKey),
		"displayName": "Casey", "sharing": sharing.Lowest,
	}
	expectProblem(t, member.json("POST", "/members", request), http.StatusForbidden, "inviteInvalid")
	request["inviteCode"] = inviteCode
	expectProblem(t, member.json("POST", "/members", request), http.StatusForbidden, "passwordIncorrect")
	request["password"] = "desk password"
	expectStatus(t, member.json("POST", "/members", request), http.StatusCreated)

	expectProblem(t, member.json("PUT", "/members/me/weekly-challenges/"+thisWeek, fullChallenge(activityID)), http.StatusForbidden, "memberPending")
	expectStatus(t, member.send("GET", "/members/me", nil, "", nil), http.StatusOK)

	if err := board.service.SetStatus(context.Background(), member.id(), scoreboard.StatusActive); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, member.json("PUT", "/members/me/weekly-challenges/"+thisWeek, fullChallenge(activityID)), http.StatusOK)

	if err := board.service.DeleteMember(context.Background(), member.id(), true); err != nil {
		t.Fatal(err)
	}
	expectProblem(t, member.send("GET", "/members/me", nil, "", nil), http.StatusUnauthorized, "unauthenticated")
	expectProblem(t, member.json("POST", "/members", request), http.StatusForbidden, "blocked")
}

// TestPendingLimit refuses new requests once as many wait as the board allows, tells the join screen so,
// still answers someone already waiting, and takes requests again once one is approved.
func TestPendingLimit(t *testing.T) {
	board := newHarness(t, func(board *scoreboard.Board) {
		board.JoinMode = scoreboard.JoinApproval
		board.MaximumPending = 2
	})
	accepting := func() bool {
		request, _ := http.NewRequest("GET", board.server.URL+"/api/v1/board", nil)
		var description struct {
			AcceptingRequests bool `json:"acceptingRequests"`
		}
		do(t, request).decode(t, &description)
		return description.AcceptingRequests
	}
	first, second, third := board.newClient(), board.newClient(), board.newClient()
	expectStatus(t, first.join("Avery", sharing.Lowest), http.StatusCreated)
	if !accepting() {
		t.Fatal("the board stopped accepting with one request of two waiting")
	}
	expectStatus(t, second.join("Blake", sharing.Lowest), http.StatusCreated)
	if accepting() {
		t.Fatal("the board kept accepting with its queue full")
	}
	expectProblem(t, third.join("Casey", sharing.Lowest), http.StatusForbidden, "pendingLimitReached")
	expectStatus(t, first.join("Avery", sharing.Lowest), http.StatusOK)

	if err := board.service.SetStatus(context.Background(), first.id(), scoreboard.StatusActive); err != nil {
		t.Fatal(err)
	}
	if !accepting() {
		t.Fatal("approving a request did not free a place")
	}
	expectStatus(t, third.join("Casey", sharing.Lowest), http.StatusCreated)
}

// newApprovalBoard serves a board that approves members, with one active member allowed to approve requests.
func newApprovalBoard(t *testing.T) (*harness, *client) {
	t.Helper()
	board := newHarness(t, func(board *scoreboard.Board) { board.JoinMode = scoreboard.JoinApproval })
	approver := board.newClient()
	expectStatus(t, approver.join("Avery", sharing.Lowest), http.StatusCreated)
	ctx := context.Background()
	if err := board.service.SetStatus(ctx, approver.id(), scoreboard.StatusActive); err != nil {
		t.Fatal(err)
	}
	if err := board.service.SetCanApproveMembers(ctx, approver.id(), true); err != nil {
		t.Fatal(err)
	}
	return board, approver
}

// TestMemberRequestsNeedThePermission refuses all three operations to everyone the admin has not allowed.
func TestMemberRequestsNeedThePermission(t *testing.T) {
	board, approver := newApprovalBoard(t)
	ctx := context.Background()
	requester := board.newClient()
	expectStatus(t, requester.join("Blake", sharing.Lowest), http.StatusCreated)
	member := board.newClient()
	expectStatus(t, member.join("Casey", sharing.Lowest), http.StatusCreated)
	if err := board.service.SetStatus(ctx, member.id(), scoreboard.StatusActive); err != nil {
		t.Fatal(err)
	}
	stranger := board.newClient()
	target := "/member-requests/" + requester.id()

	refused := func(caller *client, status int, code string) {
		t.Helper()
		expectProblem(t, caller.send("GET", "/member-requests", nil, "", nil), status, code)
		expectProblem(t, caller.send("POST", target+"/approve", nil, "", nil), status, code)
		expectProblem(t, caller.send("DELETE", target, nil, "", nil), status, code)
	}
	refused(member, http.StatusForbidden, "notPermitted")
	refused(requester, http.StatusForbidden, "memberPending")
	refused(stranger, http.StatusUnauthorized, "unauthenticated")

	// A request itself never carries the permission, even one given before the member is approved.
	if err := board.service.SetCanApproveMembers(ctx, requester.id(), true); err != nil {
		t.Fatal(err)
	}
	refused(requester, http.StatusForbidden, "memberPending")
	if err := board.service.SetCanApproveMembers(ctx, requester.id(), false); err != nil {
		t.Fatal(err)
	}

	// Stopping the permission takes effect on the next request.
	expectStatus(t, approver.send("GET", "/member-requests", nil, "", nil), http.StatusOK)
	if err := board.service.SetCanApproveMembers(ctx, approver.id(), false); err != nil {
		t.Fatal(err)
	}
	refused(approver, http.StatusForbidden, "notPermitted")
	if stored, err := board.service.Member(ctx, requester.id()); err != nil || stored.Status != scoreboard.StatusPending {
		t.Fatalf("a refused call changed the request: %+v, %v", stored, err)
	}
}

// TestMeSaysWhetherTheMemberCanApprove reports the permission and keeps it through a change to the member.
func TestMeSaysWhetherTheMemberCanApprove(t *testing.T) {
	board, approver := newApprovalBoard(t)
	canApprove := func(caller *client) bool {
		t.Helper()
		var me struct {
			CanApproveMembers bool `json:"canApproveMembers"`
		}
		caller.send("GET", "/members/me", nil, "", nil).decode(t, &me)
		return me.CanApproveMembers
	}
	if !canApprove(approver) {
		t.Fatal("an allowed member reads as not allowed")
	}
	expectStatus(t, approver.json("PATCH", "/members/me", map[string]any{"displayName": "Avery B"}), http.StatusOK)
	if !canApprove(approver) {
		t.Fatal("changing the name lost the permission")
	}
	other := board.newClient()
	expectStatus(t, other.join("Blake", sharing.Lowest), http.StatusCreated)
	if canApprove(other) {
		t.Fatal("a new member reads as allowed")
	}
}

// TestApprovingAMemberRequest lists requests oldest first with nothing but their names and dates, and lets one in.
func TestApprovingAMemberRequest(t *testing.T) {
	board, approver := newApprovalBoard(t)
	first, second := board.newClient(), board.newClient()
	expectStatus(t, first.join("Zoe", sharing.Lowest), http.StatusCreated)
	board.advance(time.Minute)
	expectStatus(t, second.join("Blake", sharing.Lowest), http.StatusCreated)

	listed := approver.send("GET", "/member-requests", nil, "", nil)
	expectStatus(t, listed, http.StatusOK)
	var requests struct {
		Requests []map[string]any `json:"requests"`
	}
	listed.decode(t, &requests)
	if len(requests.Requests) != 2 || requests.Requests[0]["id"] != first.id() || requests.Requests[1]["id"] != second.id() {
		t.Fatalf("requests: %s", listed.body)
	}
	if len(requests.Requests[0]) != 3 || requests.Requests[0]["displayName"] != "Zoe" || requests.Requests[0]["joinedAt"] == nil {
		t.Fatalf("a request shows more or less than its id, name and date: %v", requests.Requests[0])
	}

	expectStatus(t, approver.send("POST", "/member-requests/"+first.id()+"/approve", nil, "", nil), http.StatusNoContent)
	var me struct {
		Status string `json:"status"`
	}
	first.send("GET", "/members/me", nil, "", nil).decode(t, &me)
	if me.Status != scoreboard.StatusActive {
		t.Fatalf("status after approving: %s", me.Status)
	}
	expectProblem(t, approver.send("POST", "/member-requests/"+first.id()+"/approve", nil, "", nil), http.StatusNotFound, "notFound")
	expectStatus(t, approver.send("GET", "/weekly-challenges/2026-10-19", nil, "", nil), http.StatusOK)
	expectStatus(t, first.send("GET", "/weekly-challenges/2026-10-19", nil, "", nil), http.StatusOK)
}

// TestRejectingAMemberRequest deletes a request, whose member may ask again, and reaches no active member.
func TestRejectingAMemberRequest(t *testing.T) {
	board, approver := newApprovalBoard(t)
	requester, member := board.newClient(), board.newClient()
	expectStatus(t, requester.join("Blake", sharing.Lowest), http.StatusCreated)
	expectStatus(t, member.join("Casey", sharing.Lowest), http.StatusCreated)
	ctx := context.Background()
	if err := board.service.SetStatus(ctx, member.id(), scoreboard.StatusActive); err != nil {
		t.Fatal(err)
	}

	expectStatus(t, approver.send("DELETE", "/member-requests/"+requester.id(), nil, "", nil), http.StatusNoContent)
	expectProblem(t, requester.send("GET", "/members/me", nil, "", nil), http.StatusUnauthorized, "unauthenticated")
	expectProblem(t, approver.send("DELETE", "/member-requests/"+requester.id(), nil, "", nil), http.StatusNotFound, "notFound")
	expectStatus(t, requester.join("Blake", sharing.Lowest), http.StatusCreated)

	for _, activeID := range []string{member.id(), approver.id()} {
		expectProblem(t, approver.send("DELETE", "/member-requests/"+activeID, nil, "", nil), http.StatusNotFound, "notFound")
		expectProblem(t, approver.send("POST", "/member-requests/"+activeID+"/approve", nil, "", nil), http.StatusNotFound, "notFound")
		if stored, err := board.service.Member(ctx, activeID); err != nil || stored.Status != scoreboard.StatusActive {
			t.Fatalf("an active member was touched: %+v, %v", stored, err)
		}
	}
	expectProblem(t, approver.send("DELETE", "/member-requests/not-an-identifier", nil, "", nil), http.StatusNotFound, "notFound")
}

// TestSignatures refuses a skewed clock, a replay, a tampered body, a stranger and a borrowed key.
func TestSignatures(t *testing.T) {
	board := newHarness(t, nil)
	member := board.newClient()
	expectStatus(t, member.join("Drew", full), http.StatusCreated)
	target := "/api/v1/members/me"

	signed := func(method string, signedAt time.Time, signedBody, sentBody []byte) *http.Request {
		authorization, err := auth.AuthorizationValue(member.key, method, target, signedAt, signedBody)
		if err != nil {
			t.Fatal(err)
		}
		request, _ := http.NewRequest(method, board.server.URL+target, bytes.NewReader(sentBody))
		request.Header.Set("Authorization", authorization)
		request.Header.Set("Content-Type", "application/json")
		return request
	}

	expectProblem(t, do(t, signed("GET", board.clock().Add(-6*time.Minute), nil, nil)), http.StatusUnauthorized, "clockSkew")

	first := signed("GET", board.clock(), nil, nil)
	expectStatus(t, do(t, first), http.StatusOK)
	replay, _ := http.NewRequest("GET", board.server.URL+target, nil)
	replay.Header.Set("Authorization", first.Header.Get("Authorization"))
	expectProblem(t, do(t, replay), http.StatusUnauthorized, "replayed")

	tampered := signed("PATCH", board.clock(), []byte(`{"displayName":"Drew"}`), []byte(`{"displayName":"Mallory"}`))
	expectProblem(t, do(t, tampered), http.StatusUnauthorized, "unauthenticated")

	stranger := board.newClient()
	expectProblem(t, stranger.send("GET", "/members/me", nil, "", nil), http.StatusUnauthorized, "unauthenticated")

	borrowed := map[string]any{
		"inviteCode": inviteCode, "publicKey": auth.EncodePublicKey(&stranger.key.PublicKey),
		"displayName": "Mallory", "sharing": sharing.Lowest,
	}
	expectProblem(t, member.json("POST", "/members", borrowed), http.StatusUnauthorized, "unauthenticated")

	unsigned, _ := http.NewRequest("GET", board.server.URL+target, nil)
	expectProblem(t, do(t, unsigned), http.StatusUnauthorized, "unauthenticated")
}

// TestLoweringSharingDeletesWhatItNoLongerAllows checks the store drops what lowered sharing excludes.
func TestLoweringSharingDeletesWhatItNoLongerAllows(t *testing.T) {
	board := newHarness(t, nil)
	member, viewer := board.newClient(), board.newClient()
	expectStatus(t, member.join("Emery", full), http.StatusCreated)
	expectStatus(t, viewer.join("Finley", sharing.Lowest), http.StatusCreated)
	expectStatus(t, member.json("PUT", "/members/me/weekly-challenges/"+thisWeek, fullChallenge(activityID)), http.StatusOK)
	routePath := "/members/me/weekly-challenges/" + thisWeek + "/activities/" + activityID + "/route"
	expectStatus(t, member.send("PUT", routePath, []byte(routeBody), "application/json", nil), http.StatusNoContent)
	viewerRoutePath := "/members/" + member.id() + "/weekly-challenges/" + thisWeek + "/activities/" + activityID + "/route"
	expectStatus(t, viewer.send("GET", viewerRoutePath, nil, "", nil), http.StatusOK)

	expectStatus(t, member.json("PATCH", "/members/me", map[string]any{"sharing": pointsOnly}), http.StatusOK)

	stored := board.storedText(t)
	for _, gone := range []string{"weightKilograms", "averageHeartRate", "durationSeconds", "polyline", "exercises"} {
		if strings.Contains(stored, gone) {
			t.Errorf("the store still holds %s after sharing went down", gone)
		}
	}
	if !strings.Contains(stored, `"base":30`) {
		t.Error("lowering to points lost the points too")
	}
	expectProblem(t, viewer.send("GET", viewerRoutePath, nil, "", nil), http.StatusNotFound, "notFound")
}

// TestStrippingNeverRefuses stores what a member may share and drops the rest.
func TestStrippingNeverRefuses(t *testing.T) {
	board := newHarness(t, nil)
	member := board.newClient()
	expectStatus(t, member.join("Gray", pointsOnly), http.StatusCreated)
	result := member.json("PUT", "/members/me/weekly-challenges/"+thisWeek, fullChallenge(activityID))
	expectStatus(t, result, http.StatusOK)
	if text := string(result.body); strings.Contains(text, "details") || strings.Contains(text, "start") || !strings.Contains(text, activityID) {
		t.Fatalf("a points-only weekly challenge came back as %s", text)
	}
	expectProblem(t, member.send("PUT", "/members/me/weekly-challenges/"+thisWeek+"/activities/"+activityID+"/route", []byte(routeBody), "application/json", nil),
		http.StatusForbidden, "routesNotShared")
}

// TestWeekDates refuses a weekly challenge starting on the wrong day, one too old to keep, and one not yet started.
func TestWeekDates(t *testing.T) {
	board := newHarness(t, nil)
	member := board.newClient()
	expectStatus(t, member.join("Harper", full), http.StatusCreated)
	challenge := fullChallenge(activityID)
	expectProblem(t, member.json("PUT", "/members/me/weekly-challenges/2026-10-20", challenge), http.StatusUnprocessableEntity, "weekOutOfRange")
	expectProblem(t, member.json("PUT", "/members/me/weekly-challenges/2025-10-20", challenge), http.StatusUnprocessableEntity, "weekOutOfRange")
	expectProblem(t, member.json("PUT", "/members/me/weekly-challenges/2026-11-02", challenge), http.StatusUnprocessableEntity, "weekOutOfRange")
	expectProblem(t, member.json("PUT", "/members/me/weekly-challenges/2026-13-01", challenge), http.StatusBadRequest, "badRequest")

	nextWeek := map[string]any{"progress": 0.1}
	expectStatus(t, member.json("PUT", "/members/me/weekly-challenges/2026-10-26", nextWeek), http.StatusOK)
	challenge["activities"].([]any)[0].(map[string]any)["day"] = "2026-10-27"
	expectProblem(t, member.json("PUT", "/members/me/weekly-challenges/"+thisWeek, challenge), http.StatusBadRequest, "badRequest")
}

// TestValidation refuses bodies that are malformed or over their limits.
func TestValidation(t *testing.T) {
	board := newHarness(t, nil)
	member := board.newClient()
	expectStatus(t, member.join("Indigo", full), http.StatusCreated)
	path := "/members/me/weekly-challenges/" + thisWeek
	for name, body := range map[string]string{
		"not JSON":            `{"progress":`,
		"an array":            `[1,2]`,
		"no progress":         `{"score":10}`,
		"negative progress":   `{"progress":-1}`,
		"a string number":     `{"progress":"0.5"}`,
		"trailing data":       `{"progress":0.5} {}`,
		"a duplicate id":      `{"progress":0.5,"activities":[{"id":"` + activityID + `","type":"yoga","day":"2026-10-19","points":{"base":1,"bonus":0}},{"id":"` + activityID + `","type":"yoga","day":"2026-10-19","points":{"base":1,"bonus":0}}]}`,
		"a bad activity type": `{"progress":0.5,"activities":[{"id":"` + activityID + `","type":"Yoga Class","day":"2026-10-19","points":{"base":1,"bonus":0}}]}`,
	} {
		if result := member.send("PUT", path, []byte(body), "application/json", nil); result.status != http.StatusBadRequest {
			t.Errorf("%s: status %d, %s", name, result.status, result.body)
		}
	}
	expectProblem(t, member.send("PUT", path, []byte(`{"progress":0.5}`), "text/plain", nil), http.StatusUnsupportedMediaType, "unsupportedMediaType")
	huge := []byte(`{"progress":0.5,"padding":"` + strings.Repeat("x", weeklyChallengeBodyLimit) + `"}`)
	expectProblem(t, member.send("PUT", path, huge, "application/json", nil), http.StatusRequestEntityTooLarge, "payloadTooLarge")
	expectProblem(t, member.json("PATCH", "/members/me", map[string]any{"displayName": "‮evil"}), http.StatusBadRequest, "badRequest")
}

// TestLeavingErasesEverything checks no file in the store mentions a member after they leave.
func TestLeavingErasesEverything(t *testing.T) {
	board := newHarness(t, nil)
	member := board.newClient()
	expectStatus(t, member.join("Jordan", full), http.StatusCreated)
	expectStatus(t, member.json("PUT", "/members/me/weekly-challenges/"+thisWeek, fullChallenge(activityID)), http.StatusOK)
	expectStatus(t, member.send("PUT", "/members/me/weekly-challenges/"+thisWeek+"/activities/"+activityID+"/route", []byte(routeBody), "application/json", nil), http.StatusNoContent)
	expectStatus(t, member.send("PUT", "/members/me/avatar", encodedPNG(t), "image/png", nil), http.StatusNoContent)

	expectStatus(t, member.send("DELETE", "/members/me", nil, "", nil), http.StatusNoContent)
	if stored := board.storedText(t); strings.Contains(stored, member.id()) || strings.Contains(stored, "Jordan") {
		t.Fatal("the store still mentions a member who left")
	}
}

// TestAvatars re-encodes an upload, serves it as JPEG, and refuses one where the board takes none.
func TestAvatars(t *testing.T) {
	board := newHarness(t, nil)
	member, viewer := board.newClient(), board.newClient()
	expectStatus(t, member.join("Kai", full), http.StatusCreated)
	expectStatus(t, viewer.join("Lane", full), http.StatusCreated)
	expectStatus(t, member.send("PUT", "/members/me/avatar", encodedPNG(t), "image/png", nil), http.StatusNoContent)
	result := viewer.send("GET", "/members/"+member.id()+"/avatar", nil, "", nil)
	expectStatus(t, result, http.StatusOK)
	if _, format, err := image.DecodeConfig(bytes.NewReader(result.body)); err != nil || format != "jpeg" {
		t.Fatalf("the avatar came back as %s, %v", format, err)
	}
	expectProblem(t, member.send("PUT", "/members/me/avatar", []byte("not an image"), "image/png", nil), http.StatusUnprocessableEntity, "imageInvalid")
	expectStatus(t, member.send("DELETE", "/members/me/avatar", nil, "", nil), http.StatusNoContent)
	expectProblem(t, viewer.send("GET", "/members/"+member.id()+"/avatar", nil, "", nil), http.StatusNotFound, "notFound")

	noAvatars := newHarness(t, func(board *scoreboard.Board) { board.AvatarsAllowed = false })
	other := noAvatars.newClient()
	expectStatus(t, other.join("Morgan", full), http.StatusCreated)
	expectProblem(t, other.send("PUT", "/members/me/avatar", encodedPNG(t), "image/png", nil), http.StatusForbidden, "avatarsNotAllowed")
}

// TestBoardAvatarIsPublic serves the board's own avatar unsigned, with an ETag, and only while it has one.
func TestBoardAvatarIsPublic(t *testing.T) {
	board := newHarness(t, func(board *scoreboard.Board) { board.AvatarsAllowed = false })
	stranger := &client{harness: board}
	expectProblem(t, stranger.send("GET", "/board/avatar", nil, "", nil), http.StatusNotFound, "notFound")
	if err := board.service.SetBoardAvatar(context.Background(), bytes.NewReader(encodedPNG(t)), "image/png"); err != nil {
		t.Fatal(err)
	}
	if result := stranger.send("GET", "/board", nil, "", nil); !strings.Contains(string(result.body), `"hasAvatar":true`) {
		t.Fatalf("the board hid its avatar: %s", result.body)
	}
	result := stranger.send("GET", "/board/avatar", nil, "", nil)
	expectStatus(t, result, http.StatusOK)
	if _, format, err := image.DecodeConfig(bytes.NewReader(result.body)); err != nil || format != "jpeg" {
		t.Fatalf("the board's avatar came back as %s, %v", format, err)
	}
	if got := result.header.Get("Cache-Control"); got != "public, max-age=300" {
		t.Fatalf("the board's avatar had Cache-Control %q, want shared caching", got)
	}
	again := stranger.send("GET", "/board/avatar", nil, "", http.Header{"If-None-Match": {result.header.Get("ETag")}})
	expectStatus(t, again, http.StatusNotModified)
	var larger bytes.Buffer
	png.Encode(&larger, image.NewNRGBA(image.Rect(0, 0, 32, 16)))
	if err := board.service.SetBoardAvatar(context.Background(), bytes.NewReader(larger.Bytes()), "image/png"); err != nil {
		t.Fatal(err)
	}
	replaced := stranger.send("GET", "/board/avatar", nil, "", nil)
	if config, _, err := image.DecodeConfig(bytes.NewReader(replaced.body)); err != nil || config.Width != 32 {
		t.Fatalf("the replaced avatar came back as %+v, %v, want the new 32-pixel image", config, err)
	}
	if err := board.service.RemoveBoardAvatar(context.Background()); err != nil {
		t.Fatal(err)
	}
	expectProblem(t, stranger.send("GET", "/board/avatar", nil, "", nil), http.StatusNotFound, "notFound")
}

// TestRoutes needs the activity first, refuses a bad polyline, and goes when the activity does.
func TestRoutes(t *testing.T) {
	board := newHarness(t, nil)
	member := board.newClient()
	expectStatus(t, member.join("Noel", full), http.StatusCreated)
	routePath := "/members/me/weekly-challenges/" + thisWeek + "/activities/" + activityID + "/route"
	expectProblem(t, member.send("PUT", routePath, []byte(routeBody), "application/json", nil), http.StatusNotFound, "notFound")
	expectStatus(t, member.json("PUT", "/members/me/weekly-challenges/"+thisWeek, fullChallenge(activityID)), http.StatusOK)
	expectProblem(t, member.send("PUT", routePath, []byte(`{"polyline":"_p~iF"}`), "application/json", nil), http.StatusBadRequest, "badRequest")
	expectStatus(t, member.send("PUT", routePath, []byte(routeBody), "application/json", nil), http.StatusNoContent)

	expectStatus(t, member.json("PUT", "/members/me/weekly-challenges/"+thisWeek, map[string]any{"progress": 0.8, "activities": []any{}}), http.StatusOK)
	if strings.Contains(board.storedText(t), "polyline") {
		t.Fatal("a route outlived its activity")
	}
}

// TestRetention deletes weekly challenges older than the board keeps.
func TestRetention(t *testing.T) {
	board := newHarness(t, func(board *scoreboard.Board) { board.RetentionWeeks = 2 })
	member := board.newClient()
	expectStatus(t, member.join("Oakley", full), http.StatusCreated)
	expectStatus(t, member.json("PUT", "/members/me/weekly-challenges/2026-10-12", map[string]any{"progress": 0.3}), http.StatusOK)
	expectStatus(t, member.json("PUT", "/members/me/weekly-challenges/"+thisWeek, map[string]any{"progress": 0.4}), http.StatusOK)

	board.advance(7 * 24 * time.Hour)
	if err := board.service.SweepRetention(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(board.root, "weeks", "2026-10-12")); !os.IsNotExist(err) {
		t.Fatal("a weekly challenge older than the board keeps survived the sweep")
	}
	if _, err := os.Stat(filepath.Join(board.root, "weeks", thisWeek)); err != nil {
		t.Fatal("the sweep deleted a weekly challenge the board still keeps")
	}
}

// TestRateLimit answers a burst past the limit with Retry-After.
func TestRateLimit(t *testing.T) {
	board := newHarness(t, nil)
	limited := &Server{
		Service: board.service, Nonces: auth.NewNonceCache(),
		Limiter: ratelimit.New(2, 60), JoinLimiter: ratelimit.New(2, 60),
	}
	server := httptest.NewServer(limited.Handler(""))
	defer server.Close()
	var last response
	for range 3 {
		request, _ := http.NewRequest("GET", server.URL+"/api/v1/board", nil)
		last = do(t, request)
	}
	expectProblem(t, last, http.StatusTooManyRequests, "rateLimited")
	if last.header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
}

// TestUnknownOperation answers a path or method the API does not have with its own code, not notFound.
func TestUnknownOperation(t *testing.T) {
	board := newHarness(t, nil)
	for _, operation := range [][2]string{{"GET", "/api/v1/nothing"}, {"POST", "/api/v1/board"}, {"GET", "/api/v1/members/me/plans"}} {
		request, _ := http.NewRequest(operation[0], board.server.URL+operation[1], nil)
		expectProblem(t, do(t, request), http.StatusNotFound, "unknownOperation")
	}
}

// TestNewerStorageIsUnavailable refuses every operation, signed or not, once a newer server has laid out the
// store, and changes nothing.
func TestNewerStorageIsUnavailable(t *testing.T) {
	board := newHarness(t, nil)
	member := board.newClient()
	expectStatus(t, member.join("Newer", sharing.Lowest), http.StatusCreated)
	setStorageVersion(t, board.service.Store, scoreboard.StorageVersion+1)

	unsigned := &client{harness: board}
	expectProblem(t, unsigned.send("GET", "/board", nil, "", nil), http.StatusServiceUnavailable, "unavailable")
	expectProblem(t, member.send("DELETE", "/members/me", nil, "", nil), http.StatusServiceUnavailable, "unavailable")
	expectProblem(t, member.send("DELETE", "/members/me/avatar", nil, "", nil), http.StatusServiceUnavailable, "unavailable")
	if _, err := board.service.Store.Get(context.Background(), "members", member.id()); err != nil {
		t.Fatalf("the member is gone: %v", err)
	}
}

// setStorageVersion rewrites the stored board with version, as a newer server would.
func setStorageVersion(t *testing.T, backend store.Store, version int) {
	t.Helper()
	ctx := context.Background()
	document, err := backend.Get(ctx, "board", "board")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(document.Data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["storageVersion"] = version
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, "board", "board", encoded, store.AnyVersion); err != nil {
		t.Fatal(err)
	}
}

// storedText returns every path and file in the store, concatenated, so a test can look for what should
// be gone, including a document whose only trace is its file name.
func (h *harness) storedText(t *testing.T) string {
	t.Helper()
	var text strings.Builder
	err := filepath.WalkDir(h.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		text.WriteString(path + "\n")
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		text.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return text.String()
}

// encodedPNG returns a 40 by 40 gradient as PNG.
func encodedPNG(t *testing.T) []byte {
	t.Helper()
	source := image.NewNRGBA(image.Rect(0, 0, 40, 40))
	for y := range 40 {
		for x := range 40 {
			source.Set(x, y, color.NRGBA{R: 200, G: uint8(x * 6), B: uint8(y * 6), A: 255})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, source); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

// detail returns the body's problem detail.
func (r response) detail(t *testing.T) string {
	t.Helper()
	var failure struct {
		Detail string `json:"detail"`
	}
	r.decode(t, &failure)
	return failure.Detail
}

// TestStrangerAndBadSignatureLookAlike answers an unknown key as it answers a member's bad signature.
func TestStrangerAndBadSignatureLookAlike(t *testing.T) {
	board := newHarness(t, nil)
	member, stranger := board.newClient(), board.newClient()
	expectStatus(t, member.join("Parker", full), http.StatusCreated)

	authorization, err := auth.AuthorizationValue(member.key, "GET", "/api/v1/members/me", board.clock(), []byte("other"))
	if err != nil {
		t.Fatal(err)
	}
	badSignature := member.send("GET", "/members/me", nil, "", http.Header{"Authorization": {authorization}})
	unknown := stranger.send("GET", "/members/me", nil, "", nil)
	expectProblem(t, badSignature, http.StatusUnauthorized, "unauthenticated")
	expectProblem(t, unknown, http.StatusUnauthorized, "unauthenticated")
	if got, want := unknown.detail(t), badSignature.detail(t); got != want {
		t.Fatalf("got stranger detail %q, want %q", got, want)
	}
}

// TestStaleTimeLooksAlikeForStrangers answers a stale time with clockSkew whether or not the key is a member.
func TestStaleTimeLooksAlikeForStrangers(t *testing.T) {
	board := newHarness(t, nil)
	member, stranger := board.newClient(), board.newClient()
	expectStatus(t, member.join("Parker", full), http.StatusCreated)
	stale := board.clock().Add(-10 * time.Minute)
	for _, client := range []*client{member, stranger} {
		authorization, err := auth.AuthorizationValue(client.key, "GET", "/api/v1/members/me", stale, nil)
		if err != nil {
			t.Fatal(err)
		}
		expectProblem(t, client.send("GET", "/members/me", nil, "", http.Header{"Authorization": {authorization}}),
			http.StatusUnauthorized, "clockSkew")
	}
}

// TestUnknownOperationIsRateLimited puts the unknown-path answer behind the API's rate limit.
func TestUnknownOperationIsRateLimited(t *testing.T) {
	board := newHarness(t, nil)
	limited := &Server{Service: board.service, Nonces: auth.NewNonceCache(), Limiter: ratelimit.New(2, 60)}
	server := httptest.NewServer(limited.Handler(""))
	defer server.Close()
	var last response
	for range 3 {
		request, _ := http.NewRequest("GET", server.URL+"/api/v1/nothing", nil)
		last = do(t, request)
	}
	expectProblem(t, last, http.StatusTooManyRequests, "rateLimited")
}

// TestBlockedKeyNeedsTheInvite answers a blocked key without the current invite as any stranger.
func TestBlockedKeyNeedsTheInvite(t *testing.T) {
	board := newHarness(t, nil)
	member := board.newClient()
	expectStatus(t, member.join("Quinn", full), http.StatusCreated)
	if err := board.service.DeleteMember(context.Background(), member.id(), true); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{
		"inviteCode": "WrongWrongWrongWrong00", "publicKey": auth.EncodePublicKey(&member.key.PublicKey),
		"displayName": "Quinn", "sharing": sharing.Lowest,
	}
	expectProblem(t, member.json("POST", "/members", request), http.StatusForbidden, "inviteInvalid")
	expectProblem(t, member.join("Quinn", full), http.StatusForbidden, "blocked")
}
