// Package sharing applies a member's sharing to their weekly challenge: which levels exist, how a member's choice
// meets a board's maximum, and which fields of a weekly challenge each level allows.
package sharing

import (
	"fmt"
	"slices"
)

// ScoreLevel is how much of the weekly challenge's score is shared, from most private to least.
type ScoreLevel string

// The score levels, in order.
const (
	// ScoreProgress shares only the percentage of the goal reached.
	ScoreProgress ScoreLevel = "progress"

	// ScoreScore shares the score itself.
	ScoreScore ScoreLevel = "score"

	// ScoreDetails shares the score with how it was earned, and opens activity sharing.
	ScoreDetails ScoreLevel = "details"
)

// ActivityLevel is how much of each activity is shared, from most private to least.
type ActivityLevel string

// The activity levels, in order.
const (
	// ActivityNone shares no activities.
	ActivityNone ActivityLevel = "none"

	// ActivityPoints shares each activity's points.
	ActivityPoints ActivityLevel = "points"

	// ActivitySummary shares each activity's summary.
	ActivitySummary ActivityLevel = "summary"

	// ActivityFull shares everything recorded about each activity.
	ActivityFull ActivityLevel = "full"
)

// scoreOrder is the score levels from most private to least.
var scoreOrder = []ScoreLevel{ScoreProgress, ScoreScore, ScoreDetails}

// activityOrder is the activity levels from most private to least.
var activityOrder = []ActivityLevel{ActivityNone, ActivityPoints, ActivitySummary, ActivityFull}

// Sharing is what a member shares, or the most a board allows.
type Sharing struct {
	// Score is how much of the weekly challenge's score is shared.
	Score ScoreLevel `json:"score"`

	// Activity is how much of each activity is shared; it applies only at ScoreDetails.
	Activity ActivityLevel `json:"activity"`

	// Routes shares activities' routes; it applies only with some activity shared.
	Routes bool `json:"routes"`

	// StrengthSets shares strength workouts' sets; it applies only with some activity shared.
	StrengthSets bool `json:"strengthSets"`

	// StrengthWeights shares the weights of strength sets; it applies only with StrengthSets.
	StrengthWeights bool `json:"strengthWeights"`
}

// Lowest is where every new membership starts: a percentage of the goal and nothing else.
var Lowest = Sharing{Score: ScoreProgress, Activity: ActivityNone}

// Highest is everything, the default maximum for a new board.
var Highest = Sharing{Score: ScoreDetails, Activity: ActivityFull, Routes: true, StrengthSets: true, StrengthWeights: true}

// Validate rejects a level the API does not define.
func (s Sharing) Validate() error {
	if !slices.Contains(scoreOrder, s.Score) {
		return fmt.Errorf("unknown score level %q", s.Score)
	}
	if !slices.Contains(activityOrder, s.Activity) {
		return fmt.Errorf("unknown activity level %q", s.Activity)
	}
	return nil
}

// Normalized lowers every field that does not apply: activity without details, the options without
// activity, and weights without sets. A level it does not know reads as the lowest, so it fails closed.
func (s Sharing) Normalized() Sharing {
	if !slices.Contains(scoreOrder, s.Score) {
		s.Score = ScoreProgress
	}
	if !slices.Contains(activityOrder, s.Activity) {
		s.Activity = ActivityNone
	}
	if s.Score != ScoreDetails {
		s.Activity = ActivityNone
	}
	if s.Activity == ActivityNone {
		s.Routes = false
		s.StrengthSets = false
	}
	if !s.StrengthSets {
		s.StrengthWeights = false
	}
	return s
}

// Effective is the lower of a member's choice and a board's maximum, field by field.
func Effective(member, board Sharing) Sharing {
	member = member.Normalized()
	board = board.Normalized()
	return Sharing{
		Score:           scoreOrder[min(slices.Index(scoreOrder, member.Score), slices.Index(scoreOrder, board.Score))],
		Activity:        activityOrder[min(slices.Index(activityOrder, member.Activity), slices.Index(activityOrder, board.Activity))],
		Routes:          member.Routes && board.Routes,
		StrengthSets:    member.StrengthSets && board.StrengthSets,
		StrengthWeights: member.StrengthWeights && board.StrengthWeights,
	}.Normalized()
}

// Covers reports whether s allows at least everything other does, which is how a change is recognised
// as lowering.
func (s Sharing) Covers(other Sharing) bool {
	s = s.Normalized()
	other = other.Normalized()
	return slices.Index(scoreOrder, s.Score) >= slices.Index(scoreOrder, other.Score) &&
		slices.Index(activityOrder, s.Activity) >= slices.Index(activityOrder, other.Activity) &&
		(s.Routes || !other.Routes) &&
		(s.StrengthSets || !other.StrengthSets) &&
		(s.StrengthWeights || !other.StrengthWeights)
}

// All returns every distinct normalized sharing, for tests that try each one.
func All() []Sharing {
	var all []Sharing
	seen := map[Sharing]bool{}
	for _, score := range scoreOrder {
		for _, activity := range activityOrder {
			for options := range 8 {
				sharing := Sharing{
					Score:           score,
					Activity:        activity,
					Routes:          options&1 != 0,
					StrengthSets:    options&2 != 0,
					StrengthWeights: options&4 != 0,
				}.Normalized()
				if !seen[sharing] {
					seen[sharing] = true
					all = append(all, sharing)
				}
			}
		}
	}
	return all
}
