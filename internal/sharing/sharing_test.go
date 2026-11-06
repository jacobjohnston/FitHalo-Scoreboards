package sharing

import "testing"

// TestEffectiveIsTheLowerOfEach takes the lower of a member's choice and a board's maximum per field.
func TestEffectiveIsTheLowerOfEach(t *testing.T) {
	member := Sharing{Score: ScoreDetails, Activity: ActivityFull, Routes: true, StrengthSets: true, StrengthWeights: true}
	board := Sharing{Score: ScoreDetails, Activity: ActivitySummary, Routes: false, StrengthSets: true, StrengthWeights: false}
	effective := Effective(member, board)
	expected := Sharing{Score: ScoreDetails, Activity: ActivitySummary, StrengthSets: true}
	if effective != expected {
		t.Fatalf("effective %+v", effective)
	}
}

// TestNormalizedDropsWhatDoesNotApply lowers options that need a level the sharing does not reach.
func TestNormalizedDropsWhatDoesNotApply(t *testing.T) {
	cases := map[Sharing]Sharing{
		{Score: ScoreScore, Activity: ActivityFull, Routes: true}:                         {Score: ScoreScore, Activity: ActivityNone},
		{Score: ScoreDetails, Activity: ActivityNone, StrengthSets: true}:                 {Score: ScoreDetails, Activity: ActivityNone},
		{Score: ScoreDetails, Activity: ActivityPoints, StrengthWeights: true}:            {Score: ScoreDetails, Activity: ActivityPoints},
		{Score: ScoreDetails, Activity: ActivityPoints, StrengthSets: true, Routes: true}: {Score: ScoreDetails, Activity: ActivityPoints, StrengthSets: true, Routes: true},
	}
	for input, expected := range cases {
		if normalized := input.Normalized(); normalized != expected {
			t.Errorf("%+v normalized to %+v", input, normalized)
		}
	}
}

// TestCoversRecognisesLowering tells a lowering change from a raising one.
func TestCoversRecognisesLowering(t *testing.T) {
	points := Sharing{Score: ScoreDetails, Activity: ActivityPoints}
	withRoutes := Sharing{Score: ScoreDetails, Activity: ActivityPoints, Routes: true}
	if !withRoutes.Covers(points) || points.Covers(withRoutes) || !Lowest.Covers(Lowest) || Lowest.Covers(points) {
		t.Fatal("Covers got the order wrong")
	}
}

// TestValidateRefusesUnknownLevels refuses a level the API does not define.
func TestValidateRefusesUnknownLevels(t *testing.T) {
	if (Sharing{Score: "everything", Activity: ActivityNone}).Validate() == nil {
		t.Fatal("accepted an unknown score level")
	}
	if Highest.Validate() != nil || Lowest.Validate() != nil {
		t.Fatal("refused a real level")
	}
}

// TestAllIsEveryDistinctSharing counts the combinations a leak test walks.
func TestAllIsEveryDistinctSharing(t *testing.T) {
	// progress, score, details with no activity, then three activity levels each with routes on or off
	// and sets off, on without weights, or on with weights.
	if count := len(All()); count != 3+3*2*3 {
		t.Fatalf("%d combinations", count)
	}
}

// TestUnknownLevelsFailClosed reads a level the API does not define as the lowest instead of panicking.
func TestUnknownLevelsFailClosed(t *testing.T) {
	unknown := Sharing{Score: "everything", Activity: "all", Routes: true, StrengthSets: true, StrengthWeights: true}
	if effective := Effective(unknown, Highest); effective != Lowest {
		t.Errorf("Effective(unknown, Highest) = %+v, want %+v", effective, Lowest)
	}
	if effective := Effective(Highest, unknown); effective != Lowest {
		t.Errorf("Effective(Highest, unknown) = %+v, want %+v", effective, Lowest)
	}
	if !Lowest.Covers(unknown) || unknown.Covers(Highest) {
		t.Error("Covers did not read an unknown level as the lowest")
	}
}
