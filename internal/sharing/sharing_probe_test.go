package sharing_test

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
)

// scoreLadder is the score levels in order, from the openapi enum.
var scoreLadder = []string{"progress", "score", "details"}

// activityLadder is the activity levels in order, from the openapi enum.
var activityLadder = []string{"none", "points", "summary", "full"}

// expectNormalized applies the five rules of the openapi Sharing description independently of the package.
func expectNormalized(s sharing.Sharing) sharing.Sharing {
	score, activity := s.Score, s.Activity
	if rank(scoreLadder, string(score)) < 0 {
		score = sharing.ScoreProgress
	}
	if rank(activityLadder, string(activity)) < 0 {
		activity = sharing.ActivityNone
	}
	if score != sharing.ScoreDetails {
		activity = sharing.ActivityNone
	}
	routes, sets, weights := s.Routes, s.StrengthSets, s.StrengthWeights
	if activity == sharing.ActivityNone {
		routes, sets, weights = false, false, false
	}
	if !sets {
		weights = false
	}
	return sharing.Sharing{Score: score, Activity: activity, Routes: routes, StrengthSets: sets, StrengthWeights: weights}
}

// expectEffective takes the lower of each field by ladder rank, then normalises.
func expectEffective(a, b sharing.Sharing) sharing.Sharing {
	lowerScore := a.Score
	if rank(scoreLadder, string(b.Score)) < rank(scoreLadder, string(a.Score)) {
		lowerScore = b.Score
	}
	lowerActivity := a.Activity
	if rank(activityLadder, string(b.Activity)) < rank(activityLadder, string(a.Activity)) {
		lowerActivity = b.Activity
	}
	return expectNormalized(sharing.Sharing{
		Score:           lowerScore,
		Activity:        lowerActivity,
		Routes:          a.Routes && b.Routes,
		StrengthSets:    a.StrengthSets && b.StrengthSets,
		StrengthWeights: a.StrengthWeights && b.StrengthWeights,
	})
}

// expectCovers compares normalised views field by field.
func expectCovers(s, other sharing.Sharing) bool {
	s, other = expectNormalized(s), expectNormalized(other)
	return rank(scoreLadder, string(s.Score)) >= rank(scoreLadder, string(other.Score)) &&
		rank(activityLadder, string(s.Activity)) >= rank(activityLadder, string(other.Activity)) &&
		(s.Routes || !other.Routes) && (s.StrengthSets || !other.StrengthSets) && (s.StrengthWeights || !other.StrengthWeights)
}

// everySharing is All() plus every raw combination and every unknown level string.
func everySharing() []sharing.Sharing {
	return slices.Concat(sharing.All(), rawGrid(), unknownSharings())
}

// TestAllIsEveryNormalizedSharing lists each normalised combination exactly once, including Highest and Lowest.
func TestAllIsEveryNormalizedSharing(t *testing.T) {
	all := sharing.All()
	if len(all) != 21 {
		t.Errorf("len(All()) = %d, want 21", len(all))
	}
	seen := map[sharing.Sharing]bool{}
	for _, s := range all {
		if seen[s] {
			t.Errorf("All() repeats %+v", s)
		}
		seen[s] = true
		if s.Normalized() != s {
			t.Errorf("All() holds %+v, which is not normalised", s)
		}
		if err := s.Validate(); err != nil {
			t.Errorf("All() holds %+v, which Validate refuses: %v", s, err)
		}
	}
	if !seen[sharing.Highest] || !seen[sharing.Lowest] {
		t.Errorf("All() lacks Highest or Lowest")
	}
	want := map[sharing.Sharing]bool{}
	for _, s := range rawGrid() {
		want[expectNormalized(s)] = true
	}
	if !maps.Equal(seen, want) {
		t.Errorf("All() = %v\nwant %v", slices.Collect(maps.Keys(seen)), slices.Collect(maps.Keys(want)))
	}
}

// TestAllReturnsAFreshSlice lets a caller sort or edit the result without changing the next call's.
func TestAllReturnsAFreshSlice(t *testing.T) {
	first := sharing.All()
	first[0] = sharing.Sharing{Score: "mutated"}
	for _, s := range sharing.All() {
		if s.Score == "mutated" {
			t.Fatalf("All() shares its backing array between calls")
		}
	}
}

// TestHighestAndLowestAreNormalized keeps the two named sharings fixed points of Normalized.
func TestHighestAndLowestAreNormalized(t *testing.T) {
	if sharing.Highest.Normalized() != sharing.Highest || sharing.Lowest.Normalized() != sharing.Lowest {
		t.Errorf("Highest or Lowest is not normalised")
	}
	if sharing.Lowest != (sharing.Sharing{Score: sharing.ScoreProgress, Activity: sharing.ActivityNone}) {
		t.Errorf("Lowest = %+v", sharing.Lowest)
	}
}

// TestNormalizedFollowsTheFiveRules compares Normalized with the openapi rules on every combination.
func TestNormalizedFollowsTheFiveRules(t *testing.T) {
	for _, s := range everySharing() {
		got, want := s.Normalized(), expectNormalized(s)
		if got != want {
			t.Errorf("Normalized(%+v) = %+v, want %+v", s, got, want)
		}
		if again := got.Normalized(); again != got {
			t.Errorf("Normalized not idempotent on %+v: %+v then %+v", s, got, again)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("Normalized(%+v) = %+v fails Validate: %v", s, got, err)
		}
	}
}

// TestNormalizedUnknownLevelsReadAsLowest rewrites a level string the API does not define to the lowest.
func TestNormalizedUnknownLevelsReadAsLowest(t *testing.T) {
	cases := []struct {
		// name describes the case.
		name string

		// in is the sharing normalised.
		in sharing.Sharing

		// want is the expected result.
		want sharing.Sharing
	}{
		{name: "unknown score drops everything", in: sharing.Sharing{Score: "everything", Activity: sharing.ActivityFull, Routes: true, StrengthSets: true, StrengthWeights: true}, want: sharing.Lowest},
		{name: "empty score", in: sharing.Sharing{Score: "", Activity: sharing.ActivityPoints}, want: sharing.Lowest},
		{name: "mixed case score", in: sharing.Sharing{Score: "Details", Activity: sharing.ActivityPoints}, want: sharing.Lowest},
		{name: "whitespace score", in: sharing.Sharing{Score: " details", Activity: sharing.ActivityPoints}, want: sharing.Lowest},
		{name: "unknown activity keeps details", in: sharing.Sharing{Score: sharing.ScoreDetails, Activity: "all", Routes: true}, want: sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityNone}},
		{name: "empty activity keeps details", in: sharing.Sharing{Score: sharing.ScoreDetails, Activity: "", StrengthSets: true}, want: sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityNone}},
		{name: "empty activity at progress", in: sharing.Sharing{Score: sharing.ScoreProgress, Activity: ""}, want: sharing.Lowest},
		{name: "mixed case activity", in: sharing.Sharing{Score: sharing.ScoreDetails, Activity: "Full"}, want: sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityNone}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Normalized(); got != tc.want {
				t.Errorf("Normalized(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// TestEffectiveProperties checks Effective on every pair against the field-wise minimum and its algebraic laws.
func TestEffectiveProperties(t *testing.T) {
	every := everySharing()
	for _, a := range every {
		if got := sharing.Effective(a, sharing.Highest); got != a.Normalized() {
			t.Errorf("Effective(%+v, Highest) = %+v, want %+v", a, got, a.Normalized())
		}
		if got := sharing.Effective(a, sharing.Lowest); got != sharing.Lowest {
			t.Errorf("Effective(%+v, Lowest) = %+v", a, got)
		}
		if got := sharing.Effective(a, a); got != a.Normalized() {
			t.Errorf("Effective(s, s) = %+v, want %+v", got, a.Normalized())
		}
		for _, b := range every {
			got := sharing.Effective(a, b)
			if want := expectEffective(a, b); got != want {
				t.Errorf("Effective(%+v, %+v) = %+v, want %+v", a, b, got, want)
			}
			if swapped := sharing.Effective(b, a); swapped != got {
				t.Errorf("Effective not commutative: %+v vs %+v", got, swapped)
			}
			if got.Normalized() != got {
				t.Errorf("Effective(%+v, %+v) = %+v is not normalised", a, b, got)
			}
			if !a.Covers(got) || !b.Covers(got) {
				t.Errorf("Effective(%+v, %+v) = %+v exceeds one side", a, b, got)
			}
			if viaNormalized := sharing.Effective(a.Normalized(), b.Normalized()); viaNormalized != got {
				t.Errorf("Effective(%+v, %+v) = %+v but on normalised inputs %+v", a, b, got, viaNormalized)
			}
		}
	}
}

// TestEffectiveAllowsExactlyTheIntersection allows a field only when both sides allow it.
func TestEffectiveAllowsExactlyTheIntersection(t *testing.T) {
	a := mustLoad(t)
	paths := a.Paths()
	all := sharing.All()
	for _, member := range all {
		for _, board := range all {
			effective := sharing.Effective(member, board)
			for _, p := range paths {
				if got, want := a.Allows(p, effective), a.Allows(p, member) && a.Allows(p, board); got != want {
					t.Errorf("Allows(%s, Effective(%+v, %+v)) = %v, want %v", p, member, board, got, want)
				}
			}
		}
	}
}

// TestEffectiveIsTheGreatestLowerBound makes Effective the largest sharing both sides cover.
func TestEffectiveIsTheGreatestLowerBound(t *testing.T) {
	all := sharing.All()
	for _, a := range all {
		for _, b := range all {
			effective := sharing.Effective(a, b)
			for _, c := range all {
				if a.Covers(c) && b.Covers(c) && !effective.Covers(c) {
					t.Errorf("%+v is under both %+v and %+v but not under Effective %+v", c, a, b, effective)
				}
			}
		}
	}
}

// TestCoversIsAPartialOrder makes Covers reflexive, antisymmetric and transitive over All().
func TestCoversIsAPartialOrder(t *testing.T) {
	all := sharing.All()
	for _, a := range all {
		if !a.Covers(a) {
			t.Errorf("%+v does not cover itself", a)
		}
		if !sharing.Highest.Covers(a) || !a.Covers(sharing.Lowest) {
			t.Errorf("Highest or Lowest misordered against %+v", a)
		}
		for _, b := range all {
			if a != b && a.Covers(b) && b.Covers(a) {
				t.Errorf("%+v and %+v cover each other", a, b)
			}
			if !a.Covers(b) {
				continue
			}
			for _, c := range all {
				if b.Covers(c) && !a.Covers(c) {
					t.Errorf("Covers not transitive: %+v ⊒ %+v ⊒ %+v", a, b, c)
				}
			}
		}
	}
}

// TestCoversMatchesTheAllowlist makes Covers mean "allows every path the other allows".
func TestCoversMatchesTheAllowlist(t *testing.T) {
	a := mustLoad(t)
	paths := a.Paths()
	every := everySharing()
	for _, s := range every {
		for _, other := range every {
			want := true
			for _, p := range paths {
				if a.Allows(p, other) && !a.Allows(p, s) {
					want = false
					break
				}
			}
			if got := s.Covers(other); got != want {
				t.Errorf("%+v.Covers(%+v) = %v, but by the allowlist %v", s, other, got, want)
			}
			if got, byRules := s.Covers(other), expectCovers(s, other); got != byRules {
				t.Errorf("%+v.Covers(%+v) = %v, but by the field rules %v", s, other, got, byRules)
			}
		}
	}
}

// TestCoversRecognisesALowering tells a plain lowering from a change that moves two fields opposite ways.
func TestCoversRecognisesALowering(t *testing.T) {
	cases := []struct {
		// name describes the change.
		name string

		// from is the sharing before the change.
		from sharing.Sharing

		// to is the sharing after.
		to sharing.Sharing

		// lowering says from covers to.
		lowering bool

		// raising says to covers from.
		raising bool
	}{
		{name: "routes turned off", from: sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, Routes: true}, to: sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints}, lowering: true, raising: false},
		{name: "weights turned off", from: sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, StrengthSets: true, StrengthWeights: true}, to: sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, StrengthSets: true}, lowering: true, raising: false},
		{name: "sets turned off drops weights", from: sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, StrengthSets: true, StrengthWeights: true}, to: sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, StrengthWeights: true}, lowering: true, raising: false},
		{name: "activity up routes down", from: sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, Routes: true}, to: sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivitySummary}, lowering: false, raising: false},
		{name: "score down activity up is a lowering", from: sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints}, to: sharing.Sharing{Score: sharing.ScoreScore, Activity: sharing.ActivityFull, Routes: true}, lowering: true, raising: false},
		{name: "same after normalising", from: sharing.Sharing{Score: sharing.ScoreScore, Activity: sharing.ActivityFull, Routes: true}, to: sharing.Sharing{Score: sharing.ScoreScore}, lowering: true, raising: true},
		{name: "progress to score", from: sharing.Lowest, to: sharing.Sharing{Score: sharing.ScoreScore}, lowering: false, raising: true},
		{name: "unknown level is below everything", from: sharing.Lowest, to: sharing.Sharing{Score: "everything", Activity: "all", Routes: true, StrengthSets: true, StrengthWeights: true}, lowering: true, raising: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.from.Covers(tc.to); got != tc.lowering {
				t.Errorf("from.Covers(to) = %v, want %v", got, tc.lowering)
			}
			if got := tc.to.Covers(tc.from); got != tc.raising {
				t.Errorf("to.Covers(from) = %v, want %v", got, tc.raising)
			}
		})
	}
}

// TestValidateAcceptsEveryDefinedLevel accepts each combination of the openapi enums.
func TestValidateAcceptsEveryDefinedLevel(t *testing.T) {
	for _, s := range rawGrid() {
		if s.Normalized() != s {
			continue
		}
		if err := s.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v", s, err)
		}
	}
}

// TestValidateRefusesUndefinedLevels refuses every level string outside the enums, however close.
func TestValidateRefusesUndefinedLevels(t *testing.T) {
	for _, s := range unknownSharings() {
		if err := s.Validate(); err == nil {
			t.Errorf("Validate(%+v) accepted an undefined level", s)
		}
	}
	if err := (sharing.Sharing{}).Validate(); err == nil {
		t.Errorf("Validate(zero Sharing) accepted empty levels")
	}
}

// TestValidateOnUnnormalizedCombination records whether a defined but not applying combination is accepted.
func TestValidateOnUnnormalizedCombination(t *testing.T) {
	// PROBE: the openapi says a field that does not apply "reads as its lowest value", which suggests such input is
	// accepted and normalised rather than refused; Validate's doc only promises to refuse a level the API does not define.
	for _, s := range rawGrid() {
		if s.Normalized() == s {
			continue
		}
		if err := s.Validate(); err != nil {
			t.Logf("Validate(%+v) = %v", s, err)
		}
	}
}

// TestSharingJSONMatchesTheOpenAPISchema serialises the five fields under the schema's names and reads the example back.
func TestSharingJSONMatchesTheOpenAPISchema(t *testing.T) {
	data, err := json.Marshal(sharing.Lowest)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]any
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"activity", "routes", "score", "strengthSets", "strengthWeights"}
	if got := slices.Sorted(maps.Keys(keys)); !slices.Equal(got, want) {
		t.Errorf("JSON keys %v, want %v (all five are required by the schema even when false)", got, want)
	}
	var example sharing.Sharing
	if err := json.Unmarshal([]byte(`{"score":"details","activity":"points","routes":false,"strengthSets":true,"strengthWeights":false}`), &example); err != nil {
		t.Fatalf("unmarshal example: %v", err)
	}
	if example != (sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, StrengthSets: true}) {
		t.Errorf("example decoded to %+v", example)
	}
	var unknown sharing.Sharing
	if err := json.Unmarshal([]byte(`{"score":"everything","activity":"points","routes":true,"strengthSets":true,"strengthWeights":true}`), &unknown); err != nil {
		t.Fatalf("unmarshal unknown: %v", err)
	}
	if unknown.Validate() == nil {
		t.Errorf("an unknown level survived decoding and Validate")
	}
	if unknown.Normalized() != sharing.Lowest {
		t.Errorf("decoded unknown level normalised to %+v", unknown.Normalized())
	}
}
