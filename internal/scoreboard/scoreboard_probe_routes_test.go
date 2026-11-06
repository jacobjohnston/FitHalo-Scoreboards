package scoreboard_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/problem"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
)

// putRoute stores a route for member's activity in probeWeek.
func (p *probe) putRoute(member scoreboard.Member, weekStart, activityID string, body []byte) error {
	return p.service.PutRoute(context.Background(), member, weekStart, activityID, body)
}

// route reads a route.
func (p *probe) route(memberID, weekStart, activityID string) (scoreboard.Route, error) {
	return p.service.Route(context.Background(), memberID, weekStart, activityID)
}

// usage reads a member's usage.
func (p *probe) usage(memberID string) scoreboard.Usage {
	p.t.Helper()
	usage, err := p.service.Usage(context.Background(), memberID)
	if err != nil {
		p.t.Fatal(err)
	}
	return usage
}

// TestPutRouteNeedsTheChallengeAndTheActivity answers notFound before the weekly challenge exists and for an activity not in it.
func TestPutRouteNeedsTheChallengeAndTheActivity(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Runner", sharing.Highest)
	expectCode(t, p.putRoute(member, probeWeek, probeActivity, shortRoute(t, 5)), problem.NotFound)
	p.mustPutWeeklyChallenge(member, probeWeek, encode(t, baseChallenge()))
	expectCode(t, p.putRoute(member, probeWeek, "BBBBBBBBBBBBBBBBBBBBBB", shortRoute(t, 5)), problem.NotFound)
	expectCode(t, p.putRoute(member, "2026-10-12", probeActivity, shortRoute(t, 5)), problem.NotFound)
	expectCode(t, p.putRoute(member, "2026-10-20", probeActivity, shortRoute(t, 5)), problem.WeekOutOfRange)
	for _, id := range []string{"not-an-id", "../" + probeActivity, "", probeActivity + "/x"} {
		if code := codeOf(p.putRoute(member, probeWeek, id, shortRoute(t, 5))); code != problem.NotFound && code != problem.BadRequest {
			t.Errorf("PutRoute with activity %q: %q, want notFound or badRequest", id, code)
		}
	}
	expectCode(t, p.putRoute(member, probeWeek, probeActivity, shortRoute(t, 5)), "")
}

// TestPutRouteRefusedWhenRoutesNotShared refuses a route from a member whose effective sharing has none.
func TestPutRouteRefusedWhenRoutesNotShared(t *testing.T) {
	p := newProbe(t, nil)
	noRoutes := p.join("Private", sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityFull})
	p.mustPutWeeklyChallenge(noRoutes, probeWeek, encode(t, baseChallenge()))
	expectCode(t, p.putRoute(noRoutes, probeWeek, probeActivity, shortRoute(t, 5)), problem.RoutesNotShared)
	// PROBE: with no weekly challenge stored and no route sharing, both notFound and routesNotShared apply; the contract
	// does not order them. Recorded.
	fresh := p.join("Fresh", sharing.Lowest)
	t.Logf("no weekly challenge and no route sharing: %v", p.putRoute(fresh, probeWeek, probeActivity, shortRoute(t, 5)))
	capped := newProbe(t, func(board *scoreboard.Board) {
		board.MaximumSharing = sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityFull, StrengthSets: true, StrengthWeights: true}
	})
	member := capped.join("Capped", sharing.Highest)
	capped.mustPutWeeklyChallenge(member, probeWeek, encode(t, baseChallenge()))
	expectCode(t, capped.putRoute(member, probeWeek, probeActivity, shortRoute(t, 5)), problem.RoutesNotShared)
}

// TestRouteBodies checks the polyline's shape and the published limits.
func TestRouteBodies(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Runner", sharing.Highest)
	p.mustPutWeeklyChallenge(member, probeWeek, encode(t, baseChallenge()))
	cases := []struct {
		// name says what the body probes.
		name string

		// body is the route body.
		body []byte

		// want is the code, or "" when stored.
		want problem.Code
	}{
		{name: "two points", body: shortRoute(t, 2), want: ""},
		{name: "one point", body: shortRoute(t, 1), want: problem.BadRequest},
		{name: "no points", body: []byte(`{"polyline": ""}`), want: problem.BadRequest},
		{name: "missing polyline", body: []byte(`{}`), want: problem.BadRequest},
		{name: "null polyline", body: []byte(`{"polyline": null}`), want: problem.BadRequest},
		{name: "polyline as number", body: []byte(`{"polyline": 1}`), want: problem.BadRequest},
		{name: "array body", body: []byte(`[]`), want: problem.BadRequest},
		{name: "empty body", body: nil, want: problem.BadRequest},
		{name: "garbage polyline", body: []byte(`{"polyline": "\u0000\u0001"}`), want: problem.BadRequest},
		{name: "truncated polyline", body: []byte("{\"polyline\": \"_p~iF~ps|U_ulLnnqC_mqNvxq`@_\"}"), want: problem.BadRequest},
		{name: "5000 points", body: shortRoute(t, 5000), want: ""},
		{name: "5001 points", body: shortRoute(t, 5001), want: problem.BadRequest},
		{name: "trailing garbage", body: []byte(`{"polyline": "_p~iF~ps|U_ulLnnqC"} x`), want: problem.BadRequest},
		{name: "latitude off the globe", body: encode(t, map[string]any{"polyline": encodePolyline([][2]float64{{91, 0}, {92, 0}})}), want: problem.BadRequest},
		{name: "longitude off the globe", body: encode(t, map[string]any{"polyline": encodePolyline([][2]float64{{0, 181}, {0, 182}})}), want: problem.BadRequest},
		{name: "wide route", body: wideRoute(t, 100), want: ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := p.putRoute(member, probeWeek, probeActivity, test.body)
			expectCode(t, err, test.want)
		})
	}
}

// TestRouteIgnoresUnknownFields takes a route body with a field it does not know, as the API's conventions say
// both sides do, instead of refusing it.
func TestRouteIgnoresUnknownFields(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Runner", sharing.Highest)
	p.mustPutWeeklyChallenge(member, probeWeek, encode(t, baseChallenge()))
	body := []byte("{\"polyline\": \"_p~iF~ps|U_ulLnnqC_mqNvxq`@\", \"banana\": 1}")
	expectCode(t, p.putRoute(member, probeWeek, probeActivity, body), "")
}

// TestRouteBodyLimit refuses a route body over 256 KB with payloadTooLarge.
func TestRouteBodyLimit(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Runner", sharing.Highest)
	p.mustPutWeeklyChallenge(member, probeWeek, encode(t, baseChallenge()))
	const limit = 256 * 1024
	polyline := encodePolyline([][2]float64{{0, 0}, {0.00001, 0}})
	pad := func(size int) []byte {
		prefix := []byte(`{"polyline":"` + polyline + `"`)
		suffix := []byte(`}`)
		return append(append(prefix, bytes.Repeat([]byte(" "), size-len(prefix)-len(suffix))...), suffix...)
	}
	// PROBE: the Limits table caps a route body at 256 KB; whether the service or only the HTTP layer enforces
	// it on the raw body is not written down, so a whitespace-padded body's answer and its counted size are
	// recorded rather than failed.
	t.Logf("256 KB + 1 body padded with whitespace: %v", p.putRoute(member, probeWeek, probeActivity, pad(limit+1)))
	expectCode(t, p.putRoute(member, probeWeek, probeActivity, pad(limit)), "")
	t.Logf("usage after a 256 KB padded route: %+v", p.usage(member.ID).Challenges)
	// A polyline over maxLength is refused one way or another.
	long := encode(t, map[string]any{"polyline": strings.Repeat("?", limit+1)})
	if code := codeOf(p.putRoute(member, probeWeek, probeActivity, long)); code != problem.PayloadTooLarge && code != problem.BadRequest {
		t.Errorf("polyline of %d characters: %q, want payloadTooLarge or badRequest", limit+1, code)
	} else {
		t.Logf("polyline of %d characters: %s", limit+1, code)
	}
}

// TestRouteLifecycle stores, serves, keeps and deletes a route as the weekly challenge changes around it.
func TestRouteLifecycle(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Runner", sharing.Highest)
	p.mustPutWeeklyChallenge(member, probeWeek, encode(t, baseChallenge()))
	body := shortRoute(t, 20)
	if err := p.putRoute(member, probeWeek, probeActivity, body); err != nil {
		t.Fatal(err)
	}
	memberWeeklyChallenge, err := p.service.MemberWeeklyChallenge(context.Background(), member.ID, probeWeek)
	if err != nil {
		t.Fatal(err)
	}
	if got := (*memberWeeklyChallenge.Challenge.Activities)[0].HasRoute; got == nil || !*got {
		t.Errorf("hasRoute after PutRoute: %s, want true", formatBool(got))
	}
	route, err := p.route(member.ID, probeWeek, probeActivity)
	if err != nil {
		t.Fatal(err)
	}
	if want := decodeObject(t, body)["polyline"]; route.Polyline != want {
		t.Errorf("route %q, want %q", route.Polyline, want)
	}
	// Putting the weekly challenge again with the activity keeps the route.
	p.mustPutWeeklyChallenge(member, probeWeek, encode(t, baseChallenge()))
	if _, err := p.route(member.ID, probeWeek, probeActivity); err != nil {
		t.Errorf("route gone after putting the same weekly challenge: %v", err)
	}
	// Putting the weekly challenge without the activity deletes the route.
	p.mustPutWeeklyChallenge(member, probeWeek, encode(t, challengeWithActivities(t, 1)))
	expectCode(t, mustErr(p.route(member.ID, probeWeek, probeActivity)), problem.NotFound)
	if count := p.storedRoutes(member.ID, probeWeek); count != 0 {
		t.Errorf("%d routes stored after the activity left the weekly challenge", count)
	}
	// Putting the activity back does not bring the route back.
	p.mustPutWeeklyChallenge(member, probeWeek, encode(t, baseChallenge()))
	expectCode(t, mustErr(p.route(member.ID, probeWeek, probeActivity)), problem.NotFound)
	memberWeeklyChallenge, err = p.service.MemberWeeklyChallenge(context.Background(), member.ID, probeWeek)
	if err != nil {
		t.Fatal(err)
	}
	// PROBE: hasRoute is the client's word in the WeeklyChallenge schema, and here the client said true while the server
	// deleted the route when the activity left the weekly challenge; what MemberWeeklyChallenge serves is recorded.
	t.Logf("hasRoute after the activity returned without its route: %s", formatBool((*memberWeeklyChallenge.Challenge.Activities)[0].HasRoute))
	// DeleteRoute is idempotent.
	if err := p.putRoute(member, probeWeek, probeActivity, body); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := p.service.DeleteRoute(context.Background(), member, probeWeek, probeActivity); err != nil {
			t.Errorf("DeleteRoute: %v", err)
		}
	}
	expectCode(t, mustErr(p.route(member.ID, probeWeek, probeActivity)), problem.NotFound)
	if err := p.service.DeleteRoute(context.Background(), member, probeWeek, "BBBBBBBBBBBBBBBBBBBBBB"); err != nil {
		t.Errorf("DeleteRoute of an activity not in the weekly challenge: %v", err)
	}
}

// mustErr returns only the error of a two-value call.
func mustErr[Value any](_ Value, err error) error { return err }

// TestRouteOfMemberWhoStopsSharingIsGone answers notFound and empties storage when routes turn off.
func TestRouteOfMemberWhoStopsSharingIsGone(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Runner", sharing.Highest)
	p.mustPutWeeklyChallenge(member, probeWeek, encode(t, baseChallenge()))
	if err := p.putRoute(member, probeWeek, probeActivity, shortRoute(t, 5)); err != nil {
		t.Fatal(err)
	}
	lowered := sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityFull, StrengthSets: true, StrengthWeights: true}
	if _, err := p.service.UpdateMember(context.Background(), member.ID, scoreboard.MemberUpdate{Sharing: &lowered}); err != nil {
		t.Fatal(err)
	}
	expectCode(t, mustErr(p.route(member.ID, probeWeek, probeActivity)), problem.NotFound)
	if count := p.storedRoutes(member.ID, probeWeek); count != 0 {
		t.Errorf("%d routes stored after routes turned off", count)
	}
	memberWeeklyChallenge, err := p.service.MemberWeeklyChallenge(context.Background(), member.ID, probeWeek)
	if err != nil {
		t.Fatal(err)
	}
	if got := (*memberWeeklyChallenge.Challenge.Activities)[0].HasRoute; got != nil {
		t.Errorf("hasRoute %s served without route sharing", formatBool(got))
	}
}

// TestWeeklyChallengeStorageCapCountsChallengeAndRoutes refuses the route that would take a member's weekly challenge over 1 MB.
func TestWeeklyChallengeStorageCapCountsChallengeAndRoutes(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Runner", sharing.Highest)
	challenge := challengeWithActivities(t, 40)
	p.mustPutWeeklyChallenge(member, probeWeek, encode(t, challenge))
	ids := make([]string, 0, 40)
	for _, activity := range challenge["activities"].([]any) {
		ids = append(ids, activity.(map[string]any)["id"].(string))
	}
	body := wideRoute(t, 5000)
	stored := 0
	refused := false
	for _, id := range ids {
		err := p.putRoute(member, probeWeek, id, body)
		if err == nil {
			stored++
			continue
		}
		expectCode(t, err, problem.PayloadTooLarge)
		refused = true
		break
	}
	usage := p.usage(member.ID)
	if !refused {
		t.Fatalf("40 routes of %d bytes stored without reaching the cap: usage %d", len(body), usage.Total())
	}
	if usage.Total() > scoreboard.MaximumWeeklyChallengeBytes {
		t.Errorf("usage %d over the %d cap", usage.Total(), scoreboard.MaximumWeeklyChallengeBytes)
	}
	if usage.Challenges[0].Routes != stored {
		t.Errorf("usage counts %d routes, %d were stored", usage.Challenges[0].Routes, stored)
	}
	t.Logf("%d routes of %d bytes stored under a %d byte weekly challenge; usage %d", stored, len(body), usage.Challenges[0].ChallengeBytes, usage.Total())
	// Replacing a stored route counts only the new one, so it never trips the cap.
	for range 5 {
		if err := p.putRoute(member, probeWeek, ids[0], body); err != nil {
			t.Errorf("replacing a route at the cap: %v", err)
		}
	}
	if again := p.usage(member.ID); again.Total() != usage.Total() {
		t.Errorf("usage changed from %d to %d by replacing a route", usage.Total(), again.Total())
	}
	// A weekly challenge that no longer fits beside its routes is refused too: the same activities, so the routes stay,
	// padded with exercises to about 250 KB, under the 384 KB body limit but over 1 MB with the routes.
	bigChallenge := map[string]any{"progress": 0.5, "activities": []any{}}
	for _, id := range ids {
		activity := smallActivity(id)
		activity["strength"] = map[string]any{"exercises": repeated(50, func() map[string]any {
			return map[string]any{"name": string(bytes.Repeat([]byte("n"), 100)), "sets": []any{}}
		})}
		bigChallenge["activities"] = append(bigChallenge["activities"].([]any), activity)
	}
	bigBody := encode(t, bigChallenge)
	if len(bigBody) > 384*1024 || len(bigBody)+usage.Challenges[0].RouteBytes <= scoreboard.MaximumWeeklyChallengeBytes {
		t.Fatalf("big weekly challenge is %d bytes, routes %d; the case needs under 384 KB and over the cap together", len(bigBody), usage.Challenges[0].RouteBytes)
	}
	_, err := p.putWeeklyChallenge(member, probeWeek, bigBody)
	expectCode(t, err, problem.PayloadTooLarge)
	if after := p.usage(member.ID); after.Total() != usage.Total() || after.Challenges[0].Routes != stored {
		t.Errorf("a refused weekly challenge changed usage from %+v to %+v", usage.Challenges, after.Challenges)
	}
}

// TestReplacingARouteCountsOnlyTheNewOne puts one route many times and sees usage stay flat.
func TestReplacingARouteCountsOnlyTheNewOne(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Runner", sharing.Highest)
	p.mustPutWeeklyChallenge(member, probeWeek, encode(t, baseChallenge()))
	body := wideRoute(t, 5000)
	if err := p.putRoute(member, probeWeek, probeActivity, body); err != nil {
		t.Fatal(err)
	}
	first := p.usage(member.ID)
	for range 25 {
		if err := p.putRoute(member, probeWeek, probeActivity, body); err != nil {
			t.Fatalf("replacing the route: %v", err)
		}
	}
	if again := p.usage(member.ID); again.Total() != first.Total() || again.Challenges[0].Routes != 1 {
		t.Errorf("usage %+v after replacing, was %+v", again.Challenges, first.Challenges)
	}
	if first.Challenges[0].RouteBytes < len(body)/2 {
		t.Errorf("route of %d bytes counted as %d", len(body), first.Challenges[0].RouteBytes)
	}
}

// TestRouteOfUnknownMemberOrChallenge answers notFound rather than anything else.
func TestRouteOfUnknownMemberOrChallenge(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Runner", sharing.Highest)
	expectCode(t, mustErr(p.route("AAAAAAAAAAAAAAAAAAAAAA", probeWeek, probeActivity)), problem.NotFound)
	expectCode(t, mustErr(p.route(member.ID, probeWeek, probeActivity)), problem.NotFound)
	expectCode(t, mustErr(p.route(member.ID, "2026-10-20", probeActivity)), problem.WeekOutOfRange)
	for _, id := range []string{"../board", "", "x"} {
		if _, err := p.route(member.ID, probeWeek, id); err == nil {
			t.Errorf("Route with activity %q succeeded", id)
		}
		if _, err := p.route(id, probeWeek, probeActivity); err == nil {
			t.Errorf("Route for member %q succeeded", id)
		}
	}
}
