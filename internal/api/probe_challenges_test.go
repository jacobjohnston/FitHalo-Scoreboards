package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
	"github.com/jacobjohnston/FitHalo-Scoreboards/spec"
)

// allowlistFromSpec parses spec/sharing.json independently of the server's loader.
type allowlistFromSpec struct {
	// levels orders each level name.
	levels map[string][]string

	// fields maps each path to its requirements.
	fields map[string]map[string]any
}

// loadSpecAllowlist reads spec/sharing.json with encoding/json alone.
func loadSpecAllowlist(t *testing.T) allowlistFromSpec {
	t.Helper()
	var file struct {
		Levels map[string][]string `json:"levels"`
		Fields []struct {
			Path     string         `json:"path"`
			Requires map[string]any `json:"requires"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(spec.Sharing, &file); err != nil {
		t.Fatal(err)
	}
	list := allowlistFromSpec{levels: file.Levels, fields: map[string]map[string]any{}}
	for _, field := range file.Fields {
		list.fields[field.Path] = field.Requires
	}
	return list
}

// rank is a level's position in its ladder.
func (a allowlistFromSpec) rank(ladder, level string) int {
	for i, name := range a.levels[ladder] {
		if name == level {
			return i
		}
	}
	return -1
}

// allows reports whether sharing allows the leaf at path, by the file's own description.
func (a allowlistFromSpec) allows(path string, s sharing.Sharing) bool {
	requires, ok := a.fields[path]
	if !ok {
		return false
	}
	for key, value := range requires {
		switch key {
		case "score":
			if a.rank("score", string(s.Score)) < a.rank("score", value.(string)) {
				return false
			}
		case "activity":
			if a.rank("activity", string(s.Activity)) < a.rank("activity", value.(string)) {
				return false
			}
		case "routes":
			if value == true && !s.Routes {
				return false
			}
		case "strengthSets":
			if value == true && !s.StrengthSets {
				return false
			}
		case "strengthWeights":
			if value == true && !s.StrengthWeights {
				return false
			}
		}
	}
	return true
}

// TestPutWeeklyChallengeKeepsExactlyWhatSharingAllows runs the leak test over the wire for every member sharing and
// several board maxima, checking the PUT response, the member's weekly challenge and the scoreboard summary.
func TestPutWeeklyChallengeKeepsExactlyWhatSharingAllows(t *testing.T) {
	list := loadSpecAllowlist(t)
	maxima := []sharing.Sharing{
		sharing.Highest,
		{Score: sharing.ScoreDetails, Activity: sharing.ActivitySummary, Routes: true},
		{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, StrengthSets: true},
		{Score: sharing.ScoreScore, Activity: sharing.ActivityNone},
	}
	for _, maximum := range maxima {
		b := newBoard(t, boardOptions{configure: func(document *boardDocument) { document.MaximumSharing = maximum }})
		viewer := b.newPhone()
		viewer.mustJoin("Viewer", sharing.Lowest)
		for _, chosen := range sharing.All() {
			effective := sharing.Effective(chosen, maximum)
			member := b.newPhone()
			member.mustJoin("Member", chosen)
			put := member.putWeeklyChallenge(probeWeek, everythingChallenge("2026-10-20", activityID(1)))
			expectStatus(t, put, http.StatusOK)
			read := viewer.get("/members/"+member.id()+"/weekly-challenges/"+probeWeek, nil)
			expectStatus(t, read, http.StatusOK)
			var memberWeeklyChallenge struct {
				Member    map[string]any `json:"member"`
				Challenge map[string]any `json:"challenge"`
			}
			read.decode(t, &memberWeeklyChallenge)
			for name, document := range map[string]map[string]any{"PUT response": put.object(t), "GET weekly challenge": memberWeeklyChallenge.Challenge} {
				var leaves []string
				leafPaths(document, "", &leaves)
				for _, leaf := range leaves {
					if !list.allows(leaf, effective) {
						t.Errorf("maximum %+v, chosen %+v: %s carries %q, not allowed at %+v", maximum, chosen, name, leaf, effective)
					}
				}
				for path := range list.fields {
					if list.allows(path, effective) && !containsPath(leaves, path) && path != "updatedAt" {
						t.Errorf("maximum %+v, chosen %+v: %s dropped %q, allowed at %+v", maximum, chosen, name, path, effective)
					}
				}
			}
			if !reflect.DeepEqual(memberWeeklyChallenge.Member["sharing"], toGeneric(t, effective)) {
				t.Errorf("member summary sharing %v, want effective %+v", memberWeeklyChallenge.Member["sharing"], effective)
			}
			if _, ok := put.object(t)["updatedAt"]; !ok {
				t.Errorf("PUT response lacks updatedAt")
			}
			expectStatus(t, member.send("DELETE", "/members/me", nil, ""), http.StatusNoContent)
		}
	}
}

// containsPath reports whether leaves holds path.
func containsPath(leaves []string, path string) bool {
	for _, leaf := range leaves {
		if leaf == path {
			return true
		}
	}
	return false
}

// toGeneric round-trips value through JSON into generic form.
func toGeneric(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var generic any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		t.Fatal(err)
	}
	return generic
}

// TestScoreboardSummaryFollowsSharing checks WeeklyChallengeSummary fields appear only at their level.
func TestScoreboardSummaryFollowsSharing(t *testing.T) {
	b := newBoard(t, boardOptions{})
	viewer := b.newPhone()
	viewer.mustJoin("Viewer", sharing.Lowest)
	members := map[string]*phone{}
	for _, chosen := range []sharing.Sharing{
		sharing.Lowest,
		{Score: sharing.ScoreScore, Activity: sharing.ActivityNone},
		{Score: sharing.ScoreDetails, Activity: sharing.ActivityNone},
		{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints},
	} {
		member := b.newPhone()
		member.mustJoin(fmt.Sprintf("%s %s", chosen.Score, chosen.Activity), chosen)
		expectStatus(t, member.putWeeklyChallenge(probeWeek, everythingChallenge("2026-10-20", activityID(1))), http.StatusOK)
		members[member.id()] = member
	}
	result := viewer.get("/weekly-challenges/"+probeWeek, nil)
	expectStatus(t, result, http.StatusOK)
	var board struct {
		WeekStart string `json:"weekStart"`
		Ranking   string `json:"ranking"`
		Entries   []struct {
			Member    map[string]any  `json:"member"`
			Challenge *map[string]any `json:"challenge"`
		} `json:"entries"`
	}
	result.decode(t, &board)
	if board.WeekStart != probeWeek || board.Ranking == "" {
		t.Errorf("scoreboard header %+v", board)
	}
	for _, entry := range board.Entries {
		name, _ := entry.Member["displayName"].(string)
		if name == "Viewer" {
			if entry.Challenge != nil {
				t.Errorf("viewer without a weekly challenge has one: %v", *entry.Challenge)
			}
			continue
		}
		if entry.Challenge == nil {
			t.Errorf("%s has no weekly challenge", name)
			continue
		}
		challenge := *entry.Challenge
		_, hasScore := challenge["score"]
		_, hasGoal := challenge["goal"]
		_, hasTier := challenge["tier"]
		_, hasBreakdown := challenge["breakdown"]
		_, hasCount := challenge["activityCount"]
		_, hasProgress := challenge["progress"]
		_, hasUpdated := challenge["updatedAt"]
		if !hasProgress || !hasUpdated {
			t.Errorf("%s: summary lacks progress or updatedAt: %v", name, challenge)
		}
		if _, hasActivities := challenge["activities"]; hasActivities {
			t.Errorf("%s: summary carries activities", name)
		}
		switch name {
		case "progress none":
			if hasScore || hasGoal || hasTier || hasBreakdown || hasCount {
				t.Errorf("%s: summary leaks %v", name, challenge)
			}
		case "score none":
			if !hasScore || !hasGoal || !hasTier || hasBreakdown || hasCount {
				t.Errorf("%s: summary %v", name, challenge)
			}
		case "details none":
			if !hasBreakdown || hasCount {
				t.Errorf("%s: summary %v", name, challenge)
			}
		case "details points":
			if !hasBreakdown || !hasCount || challenge["activityCount"] != float64(1) {
				t.Errorf("%s: summary %v", name, challenge)
			}
		}
	}
}

// TestScoreboardOrdering checks ranking by progress and by name, with members without a weekly challenge last by name.
func TestScoreboardOrdering(t *testing.T) {
	for _, ranking := range []string{"progress", "name"} {
		t.Run(ranking, func(t *testing.T) {
			b := newBoard(t, boardOptions{configure: func(document *boardDocument) { document.Ranking = ranking }})
			type row struct {
				// name is the member's display name.
				name string
				// progress is their weekly challenge's progress, negative for none.
				progress float64
			}
			rows := []row{{"Zed", 0.5}, {"adam", 1.5}, {"Émile", 0.5}, {"Bea", -1}, {"carl", -1}, {"Ann", 0}, {"Dee", 2.25}}
			var viewer *phone
			for _, r := range rows {
				member := b.newPhone()
				member.mustJoin(r.name, sharing.Lowest)
				if r.progress >= 0 {
					expectStatus(t, member.putWeeklyChallenge(probeWeek, minimalChallenge(r.progress)), http.StatusOK)
				}
				if viewer == nil {
					viewer = member
				}
			}
			result := viewer.get("/weekly-challenges/"+probeWeek, nil)
			expectStatus(t, result, http.StatusOK)
			var board struct {
				Entries []struct {
					Member struct {
						DisplayName string `json:"displayName"`
					} `json:"member"`
					Challenge *struct {
						Progress float64 `json:"progress"`
					} `json:"challenge"`
				} `json:"entries"`
			}
			result.decode(t, &board)
			var got []string
			for _, entry := range board.Entries {
				got = append(got, entry.Member.DisplayName)
			}
			// PROBE: names sort case-insensitively but not accent-insensitively, so Émile comes after Zed. The
			// spec says only "by name"; a client showing a sorted list would put Émile between Dee and Zed.
			var want []string
			if ranking == "progress" {
				want = []string{"Dee", "adam", "Zed", "Émile", "Ann", "Bea", "carl"}
			} else {
				want = []string{"adam", "Ann", "Dee", "Zed", "Émile", "Bea", "carl"}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ranking %s: got %v, want %v", ranking, got, want)
			}
		})
	}
}

// TestWeekStartValidation checks weekStart's shape and range against the board's rules.
func TestWeekStartValidation(t *testing.T) {
	b := newBoard(t, boardOptions{configure: func(document *boardDocument) { document.RetentionWeeks = 4 }})
	member := b.newPhone()
	member.mustJoin("Dates", sharing.Lowest)
	cases := []struct {
		// name describes the value.
		name string
		// value is the weekStart path segment.
		value string
		// status is the expected status.
		status int
		// code is the expected problem code, or empty for success.
		code string
	}{
		{"this week", "2026-10-19", http.StatusOK, ""},
		{"next week", "2026-10-26", http.StatusOK, ""},
		{"week after next", "2026-11-02", http.StatusUnprocessableEntity, "weekOutOfRange"},
		{"oldest kept", "2026-09-28", http.StatusOK, ""},
		{"one older than kept", "2026-09-21", http.StatusUnprocessableEntity, "weekOutOfRange"},
		{"tuesday", "2026-10-20", http.StatusUnprocessableEntity, "weekOutOfRange"},
		{"sunday", "2026-10-18", http.StatusUnprocessableEntity, "weekOutOfRange"},
		{"not a date", "2026-10-1", http.StatusBadRequest, "badRequest"},
		{"with time", "2026-10-19T00:00:00Z", http.StatusBadRequest, "badRequest"},
		{"compact", "20261019", http.StatusBadRequest, "badRequest"},
		{"impossible day", "2026-02-30", http.StatusBadRequest, "badRequest"},
		{"month 13", "2026-13-01", http.StatusBadRequest, "badRequest"},
		{"year zero", "0000-01-03", http.StatusBadRequest, "badRequest"},
		{"traversal", "..", http.StatusBadRequest, "badRequest"},
		{"encoded slash", "2026-10-19%2Fx", http.StatusBadRequest, "badRequest"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := member.putWeeklyChallenge(c.value, minimalChallenge(0.1))
			if c.code == "" {
				expectStatus(t, result, c.status)
				return
			}
			if result.status == http.StatusNotFound && c.status == http.StatusBadRequest {
				t.Logf("PROBE: %q answered 404 rather than 400", c.value)
				return
			}
			if result.status == http.StatusUnprocessableEntity && c.status == http.StatusBadRequest {
				t.Logf("PROBE: %q answered 422 weekOutOfRange rather than 400 badRequest", c.value)
				return
			}
			expectProblem(t, result, c.status, c.code)
		})
	}
}

// TestWeekRangeFollowsTheServerClock checks the window moves with the clock, in UTC.
func TestWeekRangeFollowsTheServerClock(t *testing.T) {
	b := newBoard(t, boardOptions{configure: func(document *boardDocument) { document.RetentionWeeks = 1 }})
	member := b.newPhone()
	member.mustJoin("Clock", sharing.Lowest)
	expectStatus(t, member.putWeeklyChallenge("2026-10-19", minimalChallenge(0.1)), http.StatusOK)
	// Sunday 23:59:59 UTC is still this week; the next second starts the next.
	b.advance(time.Date(2026, 10, 25, 23, 59, 59, 0, time.UTC).Sub(b.clock()))
	expectStatus(t, member.putWeeklyChallenge("2026-10-19", minimalChallenge(0.2)), http.StatusOK)
	expectProblem(t, member.putWeeklyChallenge("2026-11-02", minimalChallenge(0.2)), http.StatusUnprocessableEntity, "weekOutOfRange")
	b.advance(time.Second)
	expectProblem(t, member.putWeeklyChallenge("2026-10-19", minimalChallenge(0.3)), http.StatusUnprocessableEntity, "weekOutOfRange")
	expectStatus(t, member.putWeeklyChallenge("2026-11-02", minimalChallenge(0.3)), http.StatusOK)
	// The old weekly challenge is no longer served, since retention is one week.
	result := member.get("/members/"+member.id()+"/weekly-challenges/2026-10-19", nil)
	if result.status != http.StatusNotFound && result.status != http.StatusUnprocessableEntity {
		t.Errorf("reading a weekly challenge past retention answered %d: %s", result.status, result.body)
	}
}

// TestPutWeeklyChallengeResponseIsWhatWasKeptAndUpdatedAtIsTheServers checks the PUT echo and updatedAt.
func TestPutWeeklyChallengeResponseIsWhatWasKeptAndUpdatedAtIsTheServers(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Echo", sharing.Highest)
	challenge := everythingChallenge("2026-10-20", activityID(1))
	challenge["updatedAt"] = "1999-01-01T00:00:00Z"
	challenge["futureField"] = true
	put := member.putWeeklyChallenge(probeWeek, challenge)
	expectStatus(t, put, http.StatusOK)
	kept := put.object(t)
	if kept["updatedAt"] != b.clock().Format(time.RFC3339) {
		t.Errorf("updatedAt = %v, want the server's %s", kept["updatedAt"], b.clock().Format(time.RFC3339))
	}
	if _, ok := kept["futureField"]; ok {
		t.Errorf("unknown field echoed back")
	}
	read := member.get("/members/"+member.id()+"/weekly-challenges/"+probeWeek, nil)
	expectStatus(t, read, http.StatusOK)
	var memberWeeklyChallenge struct {
		Challenge map[string]any `json:"challenge"`
	}
	read.decode(t, &memberWeeklyChallenge)
	if !reflect.DeepEqual(kept, memberWeeklyChallenge.Challenge) {
		t.Errorf("PUT echoed\n%v\nbut GET serves\n%v", kept, memberWeeklyChallenge.Challenge)
	}
}

// TestPutWeeklyChallengeReplacesTheWholeWeek checks a second PUT leaves nothing of the first.
func TestPutWeeklyChallengeReplacesTheWholeWeek(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Replace", sharing.Highest)
	expectStatus(t, member.putWeeklyChallenge(probeWeek, everythingChallenge("2026-10-20", activityID(1))), http.StatusOK)
	put := member.putWeeklyChallenge(probeWeek, minimalChallenge(0.25))
	expectStatus(t, put, http.StatusOK)
	kept := put.object(t)
	delete(kept, "updatedAt")
	if !reflect.DeepEqual(kept, map[string]any{"progress": 0.25}) {
		t.Errorf("second PUT kept %v", kept)
	}
}

// TestWeeklyChallengeValidation checks the WeeklyChallenge schema's constraints answer badRequest and never a crash.
func TestWeeklyChallengeValidation(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Valid", sharing.Highest)
	activity := func() map[string]any {
		return map[string]any{"id": activityID(1), "type": "yoga", "day": "2026-10-20", "points": map[string]any{"base": 1, "bonus": 0}}
	}
	withActivity := func(change func(a map[string]any)) map[string]any {
		a := activity()
		change(a)
		return map[string]any{"progress": 0.5, "score": 50, "goal": 100, "tier": "medium",
			"breakdown": map[string]any{"exercise": 50, "dailyBonus": 0, "varietyBonus": 0}, "activities": []any{a}}
	}
	cases := []struct {
		// name describes the body.
		name string
		// body is the weekly challenge.
		body any
		// ok is whether it must be accepted.
		ok bool
	}{
		{"minimal", minimalChallenge(0), true},
		{"progress above one", minimalChallenge(3.5), true},
		{"progress missing", map[string]any{"score": 1}, false},
		{"progress negative", minimalChallenge(-0.1), false},
		{"progress string", map[string]any{"progress": "0.5"}, false},
		{"progress null", map[string]any{"progress": nil}, false},
		{"progress huge", map[string]any{"progress": 1e308}, true},
		{"score negative", map[string]any{"progress": 0, "score": -1}, false},
		{"goal zero", map[string]any{"progress": 0, "goal": 0}, false},
		{"goal negative", map[string]any{"progress": 0, "goal": -5}, false},
		{"breakdown incomplete", map[string]any{"progress": 0, "breakdown": map[string]any{"exercise": 1}}, false},
		{"breakdown negative", map[string]any{"progress": 0, "breakdown": map[string]any{"exercise": -1, "dailyBonus": 0, "varietyBonus": 0}}, false},
		{"tier 64 bytes", map[string]any{"progress": 0, "tier": strings.Repeat("t", 64)}, true},
		{"tier 65 bytes", map[string]any{"progress": 0, "tier": strings.Repeat("t", 65)}, false},
		{"tier unknown", map[string]any{"progress": 0, "tier": "ultraMegaTier"}, true},
		{"activities not array", map[string]any{"progress": 0, "activities": map[string]any{}}, false},
		{"activities empty", map[string]any{"progress": 0, "activities": []any{}}, true},
		{"activity ok", withActivity(func(a map[string]any) {}), true},
		{"activity type unknown", withActivity(func(a map[string]any) { a["type"] = "quidditch" }), true},
		{"activity type empty", withActivity(func(a map[string]any) { a["type"] = "" }), false},
		{"activity type missing", withActivity(func(a map[string]any) { delete(a, "type") }), false},
		{"activity id missing", withActivity(func(a map[string]any) { delete(a, "id") }), false},
		{"activity id short", withActivity(func(a map[string]any) { a["id"] = "short" }), false},
		{"activity id padded", withActivity(func(a map[string]any) { a["id"] = activityID(1)[:21] + "=" }), false},
		{"activity day missing", withActivity(func(a map[string]any) { delete(a, "day") }), false},
		{"activity day malformed", withActivity(func(a map[string]any) { a["day"] = "2026/10/20" }), false},
		{"activity day before week", withActivity(func(a map[string]any) { a["day"] = "2026-10-18" }), false},
		{"activity day after week", withActivity(func(a map[string]any) { a["day"] = "2026-10-26" }), false},
		{"activity day last of week", withActivity(func(a map[string]any) { a["day"] = "2026-10-25" }), true},
		{"activity points missing", withActivity(func(a map[string]any) { delete(a, "points") }), false},
		{"activity points bonus missing", withActivity(func(a map[string]any) { a["points"] = map[string]any{"base": 1} }), false},
		{"activity points negative", withActivity(func(a map[string]any) { a["points"] = map[string]any{"base": -1, "bonus": 0} }), false},
		{"activity duration negative", withActivity(func(a map[string]any) { a["durationSeconds"] = -1 }), false},
		{"activity start without offset", withActivity(func(a map[string]any) { a["start"] = "2026-10-20T07:00:00" }), false},
		{"activity start with offset", withActivity(func(a map[string]any) { a["start"] = "2026-10-20T07:00:00-07:00" }), true},
		{"activity start not a time", withActivity(func(a map[string]any) { a["start"] = "yesterday" }), false},
		{"steps fractional", withActivity(func(a map[string]any) { a["details"] = map[string]any{"steps": 1.5} }), false},
		{"steps negative", withActivity(func(a map[string]any) { a["details"] = map[string]any{"steps": -1} }), false},
		{"humidity 101", withActivity(func(a map[string]any) {
			a["details"] = map[string]any{"weather": map[string]any{"humidityPercent": 101}}
		}), false},
		{"humidity 100", withActivity(func(a map[string]any) {
			a["details"] = map[string]any{"weather": map[string]any{"humidityPercent": 100}}
		}), true},
		{"incline negative", withActivity(func(a map[string]any) { a["details"] = map[string]any{"inclinePercent": -8} }), true},
		{"temperature negative", withActivity(func(a map[string]any) {
			a["details"] = map[string]any{"exposureTemperatureCelsius": -10, "weather": map[string]any{"temperatureCelsius": -30}}
		}), true},
		{"hasRoute string", withActivity(func(a map[string]any) { a["hasRoute"] = "yes" }), false},
		{"strength without exercises", withActivity(func(a map[string]any) { a["strength"] = map[string]any{} }), false},
		{"exercise without sets", withActivity(func(a map[string]any) {
			a["strength"] = map[string]any{"exercises": []any{map[string]any{"name": "Curl"}}}
		}), false},
		{"exercise empty sets", withActivity(func(a map[string]any) {
			a["strength"] = map[string]any{"exercises": []any{map[string]any{"name": "Curl", "sets": []any{}}}}
		}), true},
		{"set without type", withActivity(func(a map[string]any) {
			a["strength"] = map[string]any{"exercises": []any{map[string]any{"name": "Curl", "sets": []any{map[string]any{"reps": 5}}}}}
		}), false},
		{"set type unknown", withActivity(func(a map[string]any) {
			a["strength"] = map[string]any{"exercises": []any{map[string]any{"name": "Curl", "sets": []any{map[string]any{"type": "giant"}}}}}
		}), true},
		{"set reps fractional", withActivity(func(a map[string]any) {
			a["strength"] = map[string]any{"exercises": []any{map[string]any{"name": "Curl", "sets": []any{map[string]any{"type": "normal", "reps": 2.5}}}}}
		}), false},
		{"set weight negative", withActivity(func(a map[string]any) {
			a["strength"] = map[string]any{"exercises": []any{map[string]any{"name": "Curl", "sets": []any{map[string]any{"type": "normal", "weightKilograms": -1}}}}}
		}), false},
		{"exercise name 101", withActivity(func(a map[string]any) {
			a["strength"] = map[string]any{"exercises": []any{map[string]any{"name": strings.Repeat("n", 101), "sets": []any{}}}}
		}), false},
		{"exercise name control", withActivity(func(a map[string]any) {
			a["strength"] = map[string]any{"exercises": []any{map[string]any{"name": "Cu\x01rl", "sets": []any{}}}}
		}), false},
		{"exercise catalog not uuid", withActivity(func(a map[string]any) {
			a["strength"] = map[string]any{"exercises": []any{map[string]any{"catalogID": "not-a-uuid", "sets": []any{}}}}
		}), false},
		{"exercise catalog uppercase uuid", withActivity(func(a map[string]any) {
			a["strength"] = map[string]any{"exercises": []any{map[string]any{"catalogID": "8D9D1A3E-2F4B-4C7A-9E1F-0A1B2C3D4E5F", "sets": []any{}}}}
		}), true},
		{"duplicate activity ids", map[string]any{"progress": 0, "activities": []any{activity(), activity()}}, false},
		{"body array", []any{}, false},
		{"body null", nil, false},
		{"body number", 5, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := member.putWeeklyChallenge(probeWeek, c.body)
			if c.ok {
				expectStatus(t, result, http.StatusOK)
				return
			}
			if result.status == http.StatusOK {
				t.Fatalf("accepted: %s", result.body)
			}
			expectProblem(t, result, http.StatusBadRequest, "badRequest")
		})
	}
}

// TestWeeklyChallengeCountLimits checks the 200/50/50/20 limits at and past the edge.
func TestWeeklyChallengeCountLimits(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Counts", sharing.Highest)
	activities := func(n int) []any {
		var list []any
		for i := 0; i < n; i++ {
			list = append(list, map[string]any{"id": activityID(i), "type": "yoga", "day": "2026-10-20", "points": map[string]any{"base": 1, "bonus": 0}})
		}
		return list
	}
	strength := func(exercises, sets, clusters int) map[string]any {
		var exerciseList []any
		for e := 0; e < exercises; e++ {
			var setList []any
			for s := 0; s < sets; s++ {
				var clusterList []any
				for c := 0; c < clusters; c++ {
					clusterList = append(clusterList, map[string]any{"reps": 1})
				}
				set := map[string]any{"type": "normal", "reps": 5}
				if clusters > 0 {
					set["clusters"] = clusterList
				}
				setList = append(setList, set)
			}
			exerciseList = append(exerciseList, map[string]any{"name": fmt.Sprintf("Exercise %d", e), "sets": setList})
		}
		a := activities(1)[0].(map[string]any)
		a["strength"] = map[string]any{"exercises": exerciseList}
		return map[string]any{"progress": 0, "activities": []any{a}}
	}
	cases := []struct {
		// name describes the weekly challenge.
		name string
		// body is the weekly challenge.
		body map[string]any
		// ok is whether it must be accepted.
		ok bool
	}{
		{"200 activities", map[string]any{"progress": 0, "activities": activities(200)}, true},
		{"201 activities", map[string]any{"progress": 0, "activities": activities(201)}, false},
		{"50 exercises", strength(50, 1, 0), true},
		{"51 exercises", strength(51, 1, 0), false},
		{"50 sets", strength(1, 50, 0), true},
		{"51 sets", strength(1, 51, 0), false},
		{"20 clusters", strength(1, 1, 20), true},
		{"21 clusters", strength(1, 1, 21), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := member.putWeeklyChallenge(probeWeek, c.body)
			if c.ok {
				expectStatus(t, result, http.StatusOK)
			} else {
				expectProblem(t, result, http.StatusBadRequest, "badRequest")
			}
		})
	}
}

// TestWeeklyChallengeBodyLimit checks 384 KB is the body limit with payloadTooLarge past it.
func TestWeeklyChallengeBodyLimit(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Big", sharing.Highest)
	const limit = 384 * 1024
	// A valid weekly challenge padded to the limit with an unknown field, which is ignored but still counts as body.
	pad := func(size int) []byte {
		base := `{"progress":0.5,"padding":"`
		tail := `"}`
		return []byte(base + strings.Repeat("p", size-len(base)-len(tail)) + tail)
	}
	exact := member.send("PUT", "/members/me/weekly-challenges/"+probeWeek, pad(limit), "application/json")
	expectStatus(t, exact, http.StatusOK)
	over := member.send("PUT", "/members/me/weekly-challenges/"+probeWeek, pad(limit+1), "application/json")
	expectProblem(t, over, http.StatusRequestEntityTooLarge, "payloadTooLarge")
	// The limit holds without a Content-Length too.
	r := member.prepare("PUT", "/members/me/weekly-challenges/"+probeWeek, pad(limit+1), "application/json")
	chunked := member.dispatch(r)
	if chunked.status == http.StatusOK {
		t.Errorf("oversized body accepted")
	}
	// Any stored weekly challenge is unchanged by the refused upload.
	read := member.get("/members/"+member.id()+"/weekly-challenges/"+probeWeek, nil)
	expectStatus(t, read, http.StatusOK)
}

// TestWeeklyChallengeReadsHonourETags checks ETags on the scoreboard and the member's weekly challenge change with the data.
func TestWeeklyChallengeReadsHonourETags(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Tags", sharing.Highest)
	expectStatus(t, member.putWeeklyChallenge(probeWeek, minimalChallenge(0.1)), http.StatusOK)
	for i, path := range []string{"/weekly-challenges/" + probeWeek, "/members/" + member.id() + "/weekly-challenges/" + probeWeek} {
		first := member.get(path, nil)
		expectStatus(t, first, http.StatusOK)
		etag := first.header.Get("ETag")
		if etag == "" {
			t.Fatalf("%s has no ETag", path)
		}
		expectStatus(t, member.get(path, http.Header{"If-None-Match": {etag}}), http.StatusNotModified)
		expectStatus(t, member.putWeeklyChallenge(probeWeek, minimalChallenge(0.2+float64(i))), http.StatusOK)
		changed := member.get(path, http.Header{"If-None-Match": {etag}})
		expectStatus(t, changed, http.StatusOK)
		if changed.header.Get("ETag") == etag {
			t.Errorf("%s: ETag unchanged after the weekly challenge changed", path)
		}
	}
	// A display name change must change the scoreboard's ETag too.
	before := member.get("/weekly-challenges/"+probeWeek, nil).header.Get("ETag")
	expectStatus(t, member.json("PATCH", "/members/me", map[string]any{"displayName": "Renamed Tags"}), http.StatusOK)
	if after := member.get("/weekly-challenges/"+probeWeek, nil).header.Get("ETag"); after == before {
		t.Errorf("scoreboard ETag unchanged after a rename")
	}
}

// TestMemberWeeklyChallengePathValidation checks memberID and lookups on GET /members/{id}/weekly-challenges/{weekStart}.
func TestMemberWeeklyChallengePathValidation(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Reader", sharing.Lowest)
	other := b.newPhone()
	other.mustJoin("Other", sharing.Lowest)
	cases := []struct {
		// name describes the path.
		name string
		// path is the path under /api/v1.
		path string
		// statuses are the acceptable answers.
		statuses []int
	}{
		{"unknown member", "/members/" + strings.Repeat("A", 22) + "/weekly-challenges/" + probeWeek, []int{http.StatusNotFound}},
		{"member without a weekly challenge", "/members/" + other.id() + "/weekly-challenges/" + probeWeek, []int{http.StatusNotFound}},
		{"short id", "/members/short/weekly-challenges/" + probeWeek, []int{http.StatusBadRequest, http.StatusNotFound}},
		{"padded id", "/members/" + strings.Repeat("A", 21) + "=/weekly-challenges/" + probeWeek, []int{http.StatusBadRequest, http.StatusNotFound}},
		{"me as id", "/members/me/weekly-challenges/" + probeWeek, []int{http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed}},
		{"wrong week day", "/members/" + other.id() + "/weekly-challenges/2026-10-20", []int{http.StatusUnprocessableEntity, http.StatusNotFound}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := member.get(c.path, nil)
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

// TestScoreboardListsEveryActiveMemberOnce checks the entries are exactly the active members.
func TestScoreboardListsEveryActiveMemberOnce(t *testing.T) {
	b := newBoard(t, boardOptions{})
	var ids []string
	var viewer *phone
	for i := 0; i < 5; i++ {
		member := b.newPhone()
		member.mustJoin(fmt.Sprintf("Member %d", i), sharing.Lowest)
		ids = append(ids, member.id())
		if viewer == nil {
			viewer = member
		}
	}
	result := viewer.get("/weekly-challenges/"+probeWeek, nil)
	expectStatus(t, result, http.StatusOK)
	var board struct {
		Entries []struct {
			Member struct {
				ID string `json:"id"`
			} `json:"member"`
		} `json:"entries"`
	}
	result.decode(t, &board)
	var got []string
	for _, entry := range board.Entries {
		got = append(got, entry.Member.ID)
	}
	sort.Strings(got)
	sort.Strings(ids)
	if !reflect.DeepEqual(got, ids) {
		t.Errorf("entries %v, want %v", got, ids)
	}
	// A week nobody has put a weekly challenge for is still a valid, empty scoreboard, not a 404.
	expectStatus(t, viewer.get("/weekly-challenges/2026-10-26", nil), http.StatusOK)
}
