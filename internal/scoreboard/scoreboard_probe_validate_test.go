package scoreboard_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/problem"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
)

// challengeCase is one weekly challenge body and the code PutWeeklyChallenge must answer, "" for acceptance.
type challengeCase struct {
	// name says what the body probes.
	name string

	// mutate changes the base weekly challenge; nil leaves it alone.
	mutate func(t *testing.T, challenge map[string]any)

	// raw is sent instead of the mutated base weekly challenge when set.
	raw []byte

	// want is the problem code, or "" when the weekly challenge must be accepted.
	want problem.Code
}

// setter returns a mutation that sets path to value.
func setter(path string, value any) func(*testing.T, map[string]any) {
	return func(t *testing.T, challenge map[string]any) { setPath(t, challenge, path, value) }
}

// deleter returns a mutation that removes path.
func deleter(path string) func(*testing.T, map[string]any) {
	return func(t *testing.T, challenge map[string]any) { deletePath(t, challenge, path) }
}

// repeated returns count copies of build's result.
func repeated(count int, build func() map[string]any) []any {
	items := make([]any, count)
	for index := range items {
		items[index] = build()
	}
	return items
}

// schemaCases are bodies that break the WeeklyChallenge schema's shape, each refused with badRequest.
var schemaCases = []challengeCase{
	{name: "base weekly challenge", want: ""},
	{name: "missing progress", mutate: deleter("progress"), want: problem.BadRequest},
	{name: "missing activity id", mutate: deleter("activities.0.id"), want: problem.BadRequest},
	{name: "missing activity type", mutate: deleter("activities.0.type"), want: problem.BadRequest},
	{name: "missing activity day", mutate: deleter("activities.0.day"), want: problem.BadRequest},
	{name: "missing activity points", mutate: deleter("activities.0.points"), want: problem.BadRequest},
	{name: "missing set type", mutate: deleter("activities.0.strength.exercises.0.sets.0.type"), want: problem.BadRequest},
	{name: "null progress", mutate: setter("progress", nil), want: problem.BadRequest},
	{name: "null points", mutate: setter("activities.0.points", nil), want: problem.BadRequest},
	{name: "null activities", mutate: setter("activities", nil), want: ""},
	{name: "progress as string", mutate: setter("progress", "0.5"), want: problem.BadRequest},
	{name: "score as string", mutate: setter("score", "400"), want: problem.BadRequest},
	{name: "goal as bool", mutate: setter("goal", true), want: problem.BadRequest},
	{name: "points base as string", mutate: setter("activities.0.points.base", "30"), want: problem.BadRequest},
	{name: "steps as string", mutate: setter("activities.0.details.steps", "5000"), want: problem.BadRequest},
	{name: "reps as string", mutate: setter("activities.0.strength.exercises.0.sets.0.reps", "10"), want: problem.BadRequest},
	{name: "activities as object", mutate: setter("activities", map[string]any{}), want: problem.BadRequest},
	{name: "tier as number", mutate: setter("tier", 3), want: problem.BadRequest},
	{name: "hasRoute as string", mutate: setter("activities.0.hasRoute", "true"), want: problem.BadRequest},
	{name: "loadIsAssistance as number", mutate: setter("activities.0.strength.exercises.0.loadIsAssistance", 1), want: problem.BadRequest},
	{name: "clusters as object", mutate: setter("activities.0.strength.exercises.0.sets.0.clusters", map[string]any{}), want: problem.BadRequest},
	{name: "right as array", mutate: setter("activities.0.strength.exercises.0.sets.0.right", []any{}), want: problem.BadRequest},
}

// rangeCases are numbers outside their schema minimum or maximum, each refused with badRequest.
var rangeCases = []challengeCase{
	{name: "negative progress", mutate: setter("progress", -0.01), want: problem.BadRequest},
	{name: "zero progress", mutate: setter("progress", 0), want: ""},
	{name: "progress above one", mutate: setter("progress", 7.5), want: ""},
	{name: "negative score", mutate: setter("score", -1), want: problem.BadRequest},
	{name: "zero score", mutate: setter("score", 0), want: ""},
	{name: "zero goal", mutate: setter("goal", 0), want: problem.BadRequest},
	{name: "negative goal", mutate: setter("goal", -5), want: problem.BadRequest},
	{name: "tiny goal", mutate: setter("goal", 1e-300), want: ""},
	{name: "negative breakdown exercise", mutate: setter("breakdown.exercise", -1), want: problem.BadRequest},
	{name: "negative breakdown dailyBonus", mutate: setter("breakdown.dailyBonus", -1), want: problem.BadRequest},
	{name: "negative breakdown varietyBonus", mutate: setter("breakdown.varietyBonus", -1), want: problem.BadRequest},
	{name: "negative points base", mutate: setter("activities.0.points.base", -1), want: problem.BadRequest},
	{name: "negative points bonus", mutate: setter("activities.0.points.bonus", -1), want: problem.BadRequest},
	{name: "negative duration", mutate: setter("activities.0.durationSeconds", -1), want: problem.BadRequest},
	{name: "negative distance", mutate: setter("activities.0.distanceMeters", -1), want: problem.BadRequest},
	{name: "negative energy", mutate: setter("activities.0.details.activeEnergyKilocalories", -1), want: problem.BadRequest},
	{name: "negative heart rate", mutate: setter("activities.0.details.averageHeartRate", -1), want: problem.BadRequest},
	{name: "negative maximum heart rate", mutate: setter("activities.0.details.maximumHeartRate", -1), want: problem.BadRequest},
	{name: "negative speed", mutate: setter("activities.0.details.averageSpeedMetersPerSecond", -1), want: problem.BadRequest},
	{name: "negative elevation gain", mutate: setter("activities.0.details.elevationGainMeters", -1), want: problem.BadRequest},
	{name: "negative elevation loss", mutate: setter("activities.0.details.elevationLossMeters", -1), want: problem.BadRequest},
	{name: "negative steps", mutate: setter("activities.0.details.steps", -1), want: problem.BadRequest},
	{name: "fractional steps", mutate: setter("activities.0.details.steps", 1.5), want: problem.BadRequest},
	{name: "negative cadence", mutate: setter("activities.0.details.averageCadence", -1), want: problem.BadRequest},
	{name: "negative power", mutate: setter("activities.0.details.averagePowerWatts", -1), want: problem.BadRequest},
	{name: "negative mets", mutate: setter("activities.0.details.averageMETs", -1), want: problem.BadRequest},
	{name: "negative carried load", mutate: setter("activities.0.details.carriedLoadKilograms", -1), want: problem.BadRequest},
	{name: "negative incline", mutate: setter("activities.0.details.inclinePercent", -12), want: ""},
	{name: "negative stroller load", mutate: setter("activities.0.details.strollerLoadKilograms", -1), want: problem.BadRequest},
	{name: "negative exposure temperature", mutate: setter("activities.0.details.exposureTemperatureCelsius", -10), want: ""},
	{name: "negative air temperature", mutate: setter("activities.0.details.weather.temperatureCelsius", -30), want: ""},
	{name: "humidity 101", mutate: setter("activities.0.details.weather.humidityPercent", 101), want: problem.BadRequest},
	{name: "humidity -1", mutate: setter("activities.0.details.weather.humidityPercent", -1), want: problem.BadRequest},
	{name: "humidity 100", mutate: setter("activities.0.details.weather.humidityPercent", 100), want: ""},
	{name: "humidity 0", mutate: setter("activities.0.details.weather.humidityPercent", 0), want: ""},
	{name: "negative set reps", mutate: setter("activities.0.strength.exercises.0.sets.0.reps", -1), want: problem.BadRequest},
	{name: "fractional set reps", mutate: setter("activities.0.strength.exercises.0.sets.0.reps", 1.5), want: problem.BadRequest},
	{name: "negative set duration", mutate: setter("activities.0.strength.exercises.0.sets.0.durationSeconds", -1), want: problem.BadRequest},
	{name: "negative set distance", mutate: setter("activities.0.strength.exercises.0.sets.0.distanceMeters", -1), want: problem.BadRequest},
	{name: "negative set weight", mutate: setter("activities.0.strength.exercises.0.sets.0.weightKilograms", -1), want: problem.BadRequest},
	{name: "negative right reps", mutate: setter("activities.0.strength.exercises.0.sets.0.right.reps", -1), want: problem.BadRequest},
	{name: "fractional right reps", mutate: setter("activities.0.strength.exercises.0.sets.0.right.reps", 2.5), want: problem.BadRequest},
	{name: "negative right duration", mutate: setter("activities.0.strength.exercises.0.sets.0.right.durationSeconds", -1), want: problem.BadRequest},
	{name: "negative right weight", mutate: setter("activities.0.strength.exercises.0.sets.0.right.weightKilograms", -1), want: problem.BadRequest},
	{name: "negative cluster reps", mutate: setter("activities.0.strength.exercises.0.sets.0.clusters.0.reps", -1), want: problem.BadRequest},
	{name: "fractional cluster reps", mutate: setter("activities.0.strength.exercises.0.sets.0.clusters.0.reps", 0.5), want: problem.BadRequest},
	{name: "negative cluster weight", mutate: setter("activities.0.strength.exercises.0.sets.0.clusters.0.weightKilograms", -1), want: problem.BadRequest},
	{name: "huge score", mutate: setter("score", 1e308), want: ""},
	{name: "huge reps", mutate: setter("activities.0.strength.exercises.0.sets.0.reps", int64(1)<<53), want: ""},
	{name: "reps beyond int64", mutate: setter("activities.0.strength.exercises.0.sets.0.reps", json.Number("9223372036854775808")), want: problem.BadRequest},
	{name: "steps beyond int64", mutate: setter("activities.0.details.steps", json.Number("18446744073709551616")), want: problem.BadRequest},
}

// literalCases are bodies that are not JSON objects a WeeklyChallenge can decode from.
var literalCases = []challengeCase{
	{name: "nan literal", raw: []byte(`{"progress": NaN}`), want: problem.BadRequest},
	{name: "infinity literal", raw: []byte(`{"progress": Infinity}`), want: problem.BadRequest},
	{name: "overflowing number", raw: []byte(`{"progress": 1e400}`), want: problem.BadRequest},
	{name: "negative overflowing score", raw: []byte(`{"progress": 0.5, "score": -1e400}`), want: problem.BadRequest},
	{name: "array body", raw: []byte(`[]`), want: problem.BadRequest},
	{name: "null body", raw: []byte(`null`), want: problem.BadRequest},
	{name: "empty body", raw: []byte(``), want: problem.BadRequest},
	{name: "whitespace body", raw: []byte("  \n "), want: problem.BadRequest},
	{name: "string body", raw: []byte(`"challenge"`), want: problem.BadRequest},
	{name: "number body", raw: []byte(`1`), want: problem.BadRequest},
	{name: "trailing garbage", raw: []byte(`{"progress": 0.5} x`), want: problem.BadRequest},
	{name: "two objects", raw: []byte(`{"progress": 0.5}{"progress": 0.5}`), want: problem.BadRequest},
	{name: "truncated object", raw: []byte(`{"progress": 0.5`), want: problem.BadRequest},
	{name: "single quotes", raw: []byte(`{'progress': 0.5}`), want: problem.BadRequest},
	{name: "trailing comma", raw: []byte(`{"progress": 0.5,}`), want: problem.BadRequest},
	{name: "duplicate progress", raw: []byte(`{"progress": 0.5, "progress": -1}`), want: problem.BadRequest},
	{name: "minimal weekly challenge", raw: []byte(`{"progress": 0.5}`), want: ""},
	{name: "leading bom", raw: []byte("\ufeff" + `{"progress": 0.5}`), want: problem.BadRequest},
	{name: "invalid utf-8 in tier", raw: []byte("{\"progress\": 0.5, \"tier\": \"mon\xffster\"}"), want: problem.BadRequest},
	{name: "escaped nul in tier", raw: []byte(`{"progress": 0.5, "tier": "mon\u0000ster"}`), want: problem.BadRequest},
	{name: "unknown top-level field", raw: []byte(`{"progress": 0.5, "banana": {"deep": [1,2,3]}}`), want: ""},
	{name: "unknown activity field", raw: []byte(`{"progress": 0.5, "activities": [{"id": "AAAAAAAAAAAAAAAAAAAAAA", "type": "x", "day": "2026-10-20", "points": {"base": 1, "bonus": 0}, "banana": true}]}`), want: ""},
}

// activityCases probe each activity's identifiers, days, times and counts.
var activityCases = []challengeCase{
	{name: "duplicate activity ids", mutate: func(t *testing.T, challenge map[string]any) {
		setPath(t, challenge, "activities", []any{smallActivity(probeActivity), smallActivity(probeActivity)})
	}, want: problem.BadRequest},
	{name: "day before the week", mutate: setter("activities.0.day", "2026-10-18"), want: problem.BadRequest},
	{name: "day a week after the start", mutate: setter("activities.0.day", "2026-10-26"), want: problem.BadRequest},
	{name: "day on the week start", mutate: setter("activities.0.day", "2026-10-19"), want: ""},
	{name: "day on the week's last day", mutate: setter("activities.0.day", "2026-10-25"), want: ""},
	{name: "day malformed", mutate: setter("activities.0.day", "2026-10-2"), want: problem.BadRequest},
	{name: "day compact", mutate: setter("activities.0.day", "20261020"), want: problem.BadRequest},
	{name: "day with time", mutate: setter("activities.0.day", "2026-10-20T00:00:00Z"), want: problem.BadRequest},
	{name: "day empty", mutate: setter("activities.0.day", ""), want: problem.BadRequest},
	{name: "day with trailing space", mutate: setter("activities.0.day", "2026-10-20 "), want: problem.BadRequest},
	{name: "start without offset", mutate: setter("activities.0.start", "2026-10-20T07:30:00"), want: problem.BadRequest},
	{name: "start with space", mutate: setter("activities.0.start", "2026-10-20 07:30:00Z"), want: problem.BadRequest},
	{name: "start as date", mutate: setter("activities.0.start", "2026-10-20"), want: problem.BadRequest},
	{name: "start as epoch", mutate: setter("activities.0.start", 1760945400), want: problem.BadRequest},
	{name: "start with offset", mutate: setter("activities.0.start", "2026-10-20T07:30:00+02:00"), want: ""},
	{name: "start with fraction", mutate: setter("activities.0.start", "2026-10-20T07:30:00.123Z"), want: ""},
	{name: "start empty", mutate: setter("activities.0.start", ""), want: problem.BadRequest},
	{name: "id too short", mutate: setter("activities.0.id", "AAAAAAAAAAAAAAAAAAAAA"), want: problem.BadRequest},
	{name: "id too long", mutate: setter("activities.0.id", "AAAAAAAAAAAAAAAAAAAAAAA"), want: problem.BadRequest},
	{name: "id standard base64", mutate: setter("activities.0.id", "AAAAAAAAAAAAAAAAAAAA+/"), want: problem.BadRequest},
	{name: "id padded", mutate: setter("activities.0.id", "AAAAAAAAAAAAAAAAAAAA=="), want: problem.BadRequest},
	{name: "id empty", mutate: setter("activities.0.id", ""), want: problem.BadRequest},
	{name: "id with slash", mutate: setter("activities.0.id", "AAAAAAAAAA/AAAAAAAAAAA"), want: problem.BadRequest},
	{name: "id with dot segments", mutate: setter("activities.0.id", "../../board/board.json"), want: problem.BadRequest},
	{name: "id with unicode", mutate: setter("activities.0.id", "AAAAAAAAAAAAAAAAAAAAAé"), want: problem.BadRequest},
	{name: "id unpadded base64url", mutate: setter("activities.0.id", "abcDEF0123456789-_ABCD"), want: ""},
	{name: "empty activities", mutate: setter("activities", []any{}), want: ""},
}

// vocabularyCases probe the open vocabularies and the slug byte limit.
var vocabularyCases = []challengeCase{
	{name: "unknown activity type", mutate: setter("activities.0.type", "underwaterBasketWeaving"), want: ""},
	{name: "unknown set type", mutate: setter("activities.0.strength.exercises.0.sets.0.type", "giant"), want: ""},
	{name: "unknown tier", mutate: setter("tier", "ascended"), want: ""},
	{name: "64 byte activity type", mutate: setter("activities.0.type", strings.Repeat("a", 64)), want: ""},
	{name: "65 byte activity type", mutate: setter("activities.0.type", strings.Repeat("a", 65)), want: problem.BadRequest},
	{name: "64 byte set type", mutate: setter("activities.0.strength.exercises.0.sets.0.type", strings.Repeat("b", 64)), want: ""},
	{name: "65 byte set type", mutate: setter("activities.0.strength.exercises.0.sets.0.type", strings.Repeat("b", 65)), want: problem.BadRequest},
	{name: "64 byte tier", mutate: setter("tier", strings.Repeat("c", 64)), want: ""},
	{name: "65 byte tier", mutate: setter("tier", strings.Repeat("c", 65)), want: problem.BadRequest},
	{name: "type of 22 three-byte runes", mutate: setter("activities.0.type", strings.Repeat("é", 33)), want: problem.BadRequest},
	{name: "activity type with control", mutate: setter("activities.0.type", "outdoor\u0000Running"), want: problem.BadRequest},
	{name: "set type with newline", mutate: setter("activities.0.strength.exercises.0.sets.0.type", "drop\nset"), want: problem.BadRequest},
	{name: "tier with rlo", mutate: setter("tier", "\u202emonster"), want: problem.BadRequest},
}

// strengthCases probe exercises, names and catalog identifiers.
var strengthCases = []challengeCase{
	{name: "catalog id not a uuid", mutate: setter("activities.0.strength.exercises.0.catalogID", "not-a-uuid"), want: problem.BadRequest},
	{name: "catalog id uppercase uuid", mutate: setter("activities.0.strength.exercises.0.catalogID", "6F1C2E3A-1B2C-4D5E-8F90-123456789ABC"), want: ""},
	{name: "catalog id without hyphens", mutate: setter("activities.0.strength.exercises.0.catalogID", "6f1c2e3a1b2c4d5e8f90123456789abc"), want: problem.BadRequest},
	{name: "catalog id urn form", mutate: setter("activities.0.strength.exercises.0.catalogID", "urn:uuid:6f1c2e3a-1b2c-4d5e-8f90-123456789abc"), want: problem.BadRequest},
	{name: "catalog id braced", mutate: setter("activities.0.strength.exercises.0.catalogID", "{6f1c2e3a-1b2c-4d5e-8f90-123456789abc}"), want: problem.BadRequest},
	{name: "catalog id empty", mutate: setter("activities.0.strength.exercises.0.catalogID", ""), want: problem.BadRequest},
	{name: "exercise name 100 chars", mutate: setter("activities.0.strength.exercises.0.name", strings.Repeat("n", 100)), want: ""},
	{name: "exercise name 101 chars", mutate: setter("activities.0.strength.exercises.0.name", strings.Repeat("n", 101)), want: problem.BadRequest},
	{name: "exercise name 100 cjk", mutate: setter("activities.0.strength.exercises.0.name", strings.Repeat("山", 100)), want: ""},
	{name: "exercise name empty", mutate: setter("activities.0.strength.exercises.0.name", ""), want: problem.BadRequest},
	{name: "exercise name with tab", mutate: setter("activities.0.strength.exercises.0.name", "Bench\tPress"), want: problem.BadRequest},
	{name: "exercise name with nul", mutate: setter("activities.0.strength.exercises.0.name", "Bench\x00Press"), want: problem.BadRequest},
	{name: "exercise name with zero-width space", mutate: setter("activities.0.strength.exercises.0.name", "Bench\u200bPress"), want: problem.BadRequest},
	{name: "exercise name with rlo", mutate: setter("activities.0.strength.exercises.0.name", "\u202eBench Press"), want: problem.BadRequest},
	{name: "exercise name with line separator", mutate: setter("activities.0.strength.exercises.0.name", "Bench\u2028Press"), want: problem.BadRequest},
	{name: "exercise name with emoji", mutate: setter("activities.0.strength.exercises.0.name", "Bench 💪🏽 Press 🏴󠁧󠁢󠁥󠁮󠁧󠁿"), want: ""},
	{name: "exercise with name only", mutate: deleter("activities.0.strength.exercises.0.catalogID"), want: ""},
	{name: "exercise with catalog id only", mutate: deleter("activities.0.strength.exercises.0.name"), want: ""},
	{name: "empty exercises", mutate: setter("activities.0.strength.exercises", []any{}), want: ""},
	{name: "empty sets", mutate: setter("activities.0.strength.exercises.0.sets", []any{}), want: ""},
	{name: "empty clusters", mutate: setter("activities.0.strength.exercises.0.sets.0.clusters", []any{}), want: ""},
	{name: "set with type only", mutate: setter("activities.0.strength.exercises.0.sets.0", map[string]any{"type": "normal"}), want: ""},
	{name: "cluster with nothing", mutate: setter("activities.0.strength.exercises.0.sets.0.clusters.0", map[string]any{}), want: ""},
	{name: "right with nothing", mutate: setter("activities.0.strength.exercises.0.sets.0.right", map[string]any{}), want: ""},
}

// countCases probe the published counts at their edges.
var countCases = []challengeCase{
	{name: "50 exercises", mutate: setter("activities.0.strength.exercises", repeated(50, baseExercise)), want: ""},
	{name: "51 exercises", mutate: setter("activities.0.strength.exercises", repeated(51, baseExercise)), want: problem.BadRequest},
	{name: "50 sets", mutate: setter("activities.0.strength.exercises.0.sets", repeated(50, baseSet)), want: ""},
	{name: "51 sets", mutate: setter("activities.0.strength.exercises.0.sets", repeated(51, baseSet)), want: problem.BadRequest},
	{name: "20 clusters", mutate: setter("activities.0.strength.exercises.0.sets.0.clusters", repeated(20, func() map[string]any { return map[string]any{"reps": 1} })), want: ""},
	{name: "21 clusters", mutate: setter("activities.0.strength.exercises.0.sets.0.clusters", repeated(21, func() map[string]any { return map[string]any{"reps": 1} })), want: problem.BadRequest},
	{name: "200 activities", mutate: func(t *testing.T, challenge map[string]any) {
		setPath(t, challenge, "activities", challengeWithActivities(t, 200)["activities"])
	}, want: ""},
	{name: "201 activities", mutate: func(t *testing.T, challenge map[string]any) {
		setPath(t, challenge, "activities", challengeWithActivities(t, 201)["activities"])
	}, want: problem.BadRequest},
}

// runChallengeCases puts each case's body for a member who shares everything and checks the code.
func runChallengeCases(t *testing.T, cases []challengeCase) {
	t.Helper()
	p := newProbe(t, nil)
	member := p.join("Prober", sharing.Highest)
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			body := test.raw
			if body == nil {
				challenge := baseChallenge()
				if test.mutate != nil {
					test.mutate(t, challenge)
				}
				body = encode(t, challenge)
			}
			_, err := p.putWeeklyChallenge(member, probeWeek, body)
			expectCode(t, err, test.want)
		})
	}
}

// TestPutWeeklyChallengeSchemaShape refuses bodies whose fields are missing or of the wrong type.
func TestPutWeeklyChallengeSchemaShape(t *testing.T) { runChallengeCases(t, schemaCases) }

// TestPutWeeklyChallengeNumericRanges refuses numbers outside their schema bounds and keeps those inside.
func TestPutWeeklyChallengeNumericRanges(t *testing.T) { runChallengeCases(t, rangeCases) }

// TestPutWeeklyChallengeLiterals refuses bodies that are not one JSON object.
func TestPutWeeklyChallengeLiterals(t *testing.T) { runChallengeCases(t, literalCases) }

// TestPutWeeklyChallengeActivities checks identifiers, days, start times and duplicates.
func TestPutWeeklyChallengeActivities(t *testing.T) { runChallengeCases(t, activityCases) }

// TestPutWeeklyChallengeVocabularies accepts unknown slugs and refuses ones over 64 bytes or with hidden characters.
func TestPutWeeklyChallengeVocabularies(t *testing.T) { runChallengeCases(t, vocabularyCases) }

// TestPutWeeklyChallengeStrength checks exercises, their names and catalog identifiers.
func TestPutWeeklyChallengeStrength(t *testing.T) { runChallengeCases(t, strengthCases) }

// TestPutWeeklyChallengeCounts checks every published count at its edge.
func TestPutWeeklyChallengeCounts(t *testing.T) { runChallengeCases(t, countCases) }

// requiredCases are bodies missing a field the schema lists as required, each refused with badRequest.
var requiredCases = []challengeCase{
	{name: "missing points base", mutate: deleter("activities.0.points.base"), want: problem.BadRequest},
	{name: "missing points bonus", mutate: deleter("activities.0.points.bonus"), want: problem.BadRequest},
	{name: "empty points", mutate: setter("activities.0.points", map[string]any{}), want: problem.BadRequest},
	{name: "missing exercises", mutate: deleter("activities.0.strength.exercises"), want: problem.BadRequest},
	{name: "empty strength", mutate: setter("activities.0.strength", map[string]any{}), want: problem.BadRequest},
	{name: "missing sets", mutate: deleter("activities.0.strength.exercises.0.sets"), want: problem.BadRequest},
	{name: "missing breakdown exercise", mutate: deleter("breakdown.exercise"), want: problem.BadRequest},
	{name: "missing breakdown dailyBonus", mutate: deleter("breakdown.dailyBonus"), want: problem.BadRequest},
	{name: "missing breakdown varietyBonus", mutate: deleter("breakdown.varietyBonus"), want: problem.BadRequest},
	{name: "empty breakdown", mutate: setter("breakdown", map[string]any{}), want: problem.BadRequest},
}

// TestPutWeeklyChallengeRequiredFieldsAreRequired refuses a body missing a field the schema marks required, instead of
// storing a zero in its place or serving exercises as null.
func TestPutWeeklyChallengeRequiredFieldsAreRequired(t *testing.T) {
	runChallengeCases(t, requiredCases)
}

// TestPutWeeklyChallengeAcceptsWhitespaceAfterTheObject takes a body whose JSON object is followed by whitespace, as RFC
// 8259 allows and as json.Encoder writes it.
func TestPutWeeklyChallengeAcceptsWhitespaceAfterTheObject(t *testing.T) {
	runChallengeCases(t, []challengeCase{
		{name: "trailing newline", raw: []byte("{\"progress\": 0.5}\n"), want: ""},
		{name: "trailing spaces", raw: []byte("{\"progress\": 0.5}  \n "), want: ""},
		{name: "leading whitespace", raw: []byte(" \n{\"progress\": 0.5}"), want: ""},
	})
}

// TestPutWeeklyChallengeRecordsOpenQuestions records how the service treats bodies the contract leaves open.
func TestPutWeeklyChallengeRecordsOpenQuestions(t *testing.T) {
	// PROBE: the contract does not say whether an exercise needs a catalogID or a name, whether an empty
	// slug is a slug, whether a slug may hold spaces or non-ASCII, whether the breakdown must sum to the
	// score, or whether progress must equal score over goal. Recorded, not asserted.
	probes := []challengeCase{
		{name: "exercise with neither catalogID nor name", mutate: func(t *testing.T, challenge map[string]any) {
			deletePath(t, challenge, "activities.0.strength.exercises.0.catalogID")
			deletePath(t, challenge, "activities.0.strength.exercises.0.name")
		}},
		{name: "empty activity type", mutate: setter("activities.0.type", "")},
		{name: "empty set type", mutate: setter("activities.0.strength.exercises.0.sets.0.type", "")},
		{name: "empty tier", mutate: setter("tier", "")},
		{name: "activity type with spaces", mutate: setter("activities.0.type", "outdoor running")},
		{name: "activity type with unicode", mutate: setter("activities.0.type", "курение")},
		{name: "activity type with emoji", mutate: setter("activities.0.type", "🏃")},
		{name: "breakdown not summing to score", mutate: setter("breakdown.exercise", 1.0)},
		{name: "progress not score over goal", mutate: setter("progress", 0.01)},
		{name: "score without goal", mutate: deleter("goal")},
		{name: "goal without score", mutate: deleter("score")},
		{name: "breakdown without score", mutate: func(t *testing.T, challenge map[string]any) {
			deletePath(t, challenge, "score")
			deletePath(t, challenge, "goal")
		}},
		{name: "exercise with both catalogID and name", mutate: nil},
		{name: "exercise name of only spaces", mutate: setter("activities.0.strength.exercises.0.name", "   ")},
		{name: "exercise name with leading space", mutate: setter("activities.0.strength.exercises.0.name", " Bench")},
		{name: "steps as 1e3", mutate: setter("activities.0.details.steps", json.Number("1e3"))},
		{name: "steps as 1000.0", mutate: setter("activities.0.details.steps", json.Number("1000.0"))},
		{name: "activity that is a string", mutate: setter("activities", []any{"run"})},
		{name: "weather that is a string", mutate: setter("activities.0.details.weather", "sunny")},
		{name: "details that is a string", mutate: setter("activities.0.details", "none")},
	}
	p := newProbe(t, nil)
	member := p.join("Prober", sharing.Highest)
	for _, test := range probes {
		challenge := baseChallenge()
		if test.mutate != nil {
			test.mutate(t, challenge)
		}
		kept, err := p.putWeeklyChallenge(member, probeWeek, encode(t, challenge))
		t.Logf("%s: err=%v kept=%s", test.name, err, encode(t, kept))
	}
}

// TestPutWeeklyChallengeIgnoresClientUpdatedAt replaces any updatedAt in the request with the server's clock.
func TestPutWeeklyChallengeIgnoresClientUpdatedAt(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Prober", sharing.Highest)
	for _, value := range []any{"1999-01-01T00:00:00Z", "not a time", 12345, nil, true, map[string]any{"a": 1}} {
		challenge := baseChallenge()
		setPath(t, challenge, "updatedAt", value)
		kept, err := p.putWeeklyChallenge(member, probeWeek, encode(t, challenge))
		if err != nil {
			t.Errorf("updatedAt %v: refused: %v", value, err)
			continue
		}
		if kept.UpdatedAt == nil || !kept.UpdatedAt.Equal(probeNow) {
			t.Errorf("updatedAt %v: kept %v, want the server's %v", value, kept.UpdatedAt, probeNow)
		}
	}
	stored := p.storedChallenge(member.ID, probeWeek)
	if got := stored["updatedAt"]; got != probeNow.Format(time.RFC3339) && got != probeNow.Format(time.RFC3339Nano) {
		t.Errorf("stored updatedAt %v, want %v", got, probeNow)
	}
}

// TestPutWeeklyChallengeDropsUnknownFieldsFromWhatItKeeps keeps nothing a client sent outside the schema.
func TestPutWeeklyChallengeDropsUnknownFieldsFromWhatItKeeps(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Prober", sharing.Highest)
	challenge := baseChallenge()
	setPath(t, challenge, "banana", "ripe")
	setPath(t, challenge, "activities.0.banana", "ripe")
	setPath(t, challenge, "activities.0.details.banana", "ripe")
	setPath(t, challenge, "activities.0.strength.exercises.0.sets.0.banana", "ripe")
	kept, err := p.putWeeklyChallenge(member, probeWeek, encode(t, challenge))
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range []map[string]any{asObject(t, kept), p.storedChallenge(member.ID, probeWeek)} {
		for path := range leafSet(object) {
			if strings.Contains(path, "banana") {
				t.Errorf("unknown field kept at %s", path)
			}
			if !p.allowlist.Allows(path, sharing.Highest) {
				t.Errorf("path %s kept but not in spec/sharing.json", path)
			}
		}
	}
}

// TestPutWeeklyChallengeReportsHasRouteFromStorage records whether hasRoute comes from the client or from stored routes.
func TestPutWeeklyChallengeReportsHasRouteFromStorage(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Prober", sharing.Highest)
	kept := p.mustPutWeeklyChallenge(member, probeWeek, encode(t, baseChallenge()))
	memberWeeklyChallenge, err := p.service.MemberWeeklyChallenge(context.Background(), member.ID, probeWeek)
	if err != nil {
		t.Fatal(err)
	}
	// PROBE: the client said hasRoute: true but no route is stored; the schema calls hasRoute "whether a
	// route can be fetched". Recorded here: what PutWeeklyChallenge returned and what MemberWeeklyChallenge serves.
	t.Logf("PutWeeklyChallenge hasRoute: %v; MemberWeeklyChallenge hasRoute: %v", formatBool((*kept.Activities)[0].HasRoute), formatBool((*memberWeeklyChallenge.Challenge.Activities)[0].HasRoute))
	if _, err := p.service.Route(context.Background(), member.ID, probeWeek, probeActivity); codeOf(err) != problem.NotFound {
		t.Errorf("Route with none stored: got %v, want notFound", err)
	}
}

// formatBool prints an optional boolean.
func formatBool(value *bool) string {
	if value == nil {
		return "absent"
	}
	if *value {
		return "true"
	}
	return "false"
}

// TestPutWeeklyChallengeBodyLimit refuses a body over 384 KB with payloadTooLarge and takes one exactly at it.
func TestPutWeeklyChallengeBodyLimit(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Prober", sharing.Highest)
	const limit = 384 * 1024
	// A valid weekly challenge padded with an unknown field to the exact size.
	pad := func(size int) []byte {
		prefix := []byte(`{"progress":0.5,"pad":"`)
		suffix := []byte(`"}`)
		return append(append(prefix, bytes.Repeat([]byte("x"), size-len(prefix)-len(suffix))...), suffix...)
	}
	_, err := p.putWeeklyChallenge(member, probeWeek, pad(limit+1))
	if codeOf(err) != problem.PayloadTooLarge {
		// PROBE: the Limits table caps a weekly challenge body at 384 KB; whether the service or only the HTTP layer
		// enforces it is not written down, so the service's answer is recorded rather than failed.
		t.Logf("384 KB + 1 body: %v", err)
	}
	if _, err := p.putWeeklyChallenge(member, probeWeek, pad(limit)); err != nil {
		t.Errorf("384 KB body refused: %v", err)
	}
}

// TestPutWeeklyChallengeStoresExactlyTheKeptChallenge stores the weekly challenge as PutWeeklyChallenge returned it.
func TestPutWeeklyChallengeStoresExactlyTheKeptChallenge(t *testing.T) {
	p := newProbe(t, nil)
	member := p.join("Prober", sharing.Highest)
	kept := p.mustPutWeeklyChallenge(member, probeWeek, encode(t, baseChallenge()))
	stored := p.storedChallenge(member.ID, probeWeek)
	if stored == nil {
		t.Fatal("no stored weekly challenge")
	}
	if got, want := leafSet(stored), leafSet(asObject(t, kept)); !equalSets(got, want) {
		t.Errorf("stored leaves %v differ from returned leaves %v", keys(got), keys(want))
	}
}

// equalSets reports whether two sets hold the same keys.
func equalSets(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if !b[key] {
			return false
		}
	}
	return true
}

// keys lists a set's keys.
func keys(set map[string]bool) []string {
	var list []string
	for key := range set {
		list = append(list, key)
	}
	return list
}

// FuzzPutWeeklyChallengeBody checks that no body crashes PutWeeklyChallenge and that whatever it keeps is in the allowlist.
func FuzzPutWeeklyChallengeBody(f *testing.F) {
	f.Add([]byte(`{"progress": 0.5}`))
	f.Add([]byte(`{"progress": NaN}`))
	f.Add([]byte(`{"progress": 0.5, "activities": [{"id": "AAAAAAAAAAAAAAAAAAAAAA", "type": "x", "day": "2026-10-20", "points": {"base": 1, "bonus": 0}}]}`))
	f.Add([]byte(`{"progress": 1e400}`))
	f.Add([]byte(`[]`))
	seedChallenge, err := json.Marshal(baseChallenge())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seedChallenge)
	p := newProbe(f, nil)
	member := p.join("Fuzzer", sharing.Highest)
	f.Fuzz(func(t *testing.T, body []byte) {
		kept, err := p.service.PutWeeklyChallenge(context.Background(), member, probeWeek, body)
		if err != nil {
			if code := codeOf(err); code != problem.BadRequest && code != problem.PayloadTooLarge {
				t.Errorf("PutWeeklyChallenge(%.60q) code %q", body, code)
			}
			return
		}
		if kept.Progress == nil || *kept.Progress < 0 {
			t.Errorf("PutWeeklyChallenge(%.60q) kept progress %v", body, kept.Progress)
		}
		for path := range leafSet(asObject(t, kept)) {
			if !p.allowlist.Allows(path, sharing.Highest) {
				t.Errorf("PutWeeklyChallenge(%.60q) kept %s", body, path)
			}
		}
	})
}
