// Package scoreboard is the board itself: its members, their weekly challenges and routes, and the rules that keep
// what is stored inside what each member shares.
package scoreboard

import (
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
)

// Board is the board's identity and rules, stored as the single document board/board.
type Board struct {
	// ID is created at setup and never changed, so the board keeps its members if it moves.
	ID string `json:"id"`

	// Name is the board's title.
	Name string `json:"name"`

	// Description is the admin's optional text about the board.
	Description string `json:"description,omitempty"`

	// HasAvatar reports whether the admin has stored an avatar for the board itself.
	HasAvatar bool `json:"hasAvatar,omitempty"`

	// WeekStartDay is the lowercase English weekday every week starts on.
	WeekStartDay string `json:"weekStartDay"`

	// RetentionWeeks is how many weeks the board keeps, counting the current one.
	RetentionWeeks int `json:"retentionWeeks"`

	// JoinMode is JoinOpen or JoinApproval.
	JoinMode string `json:"joinMode"`

	// MaximumPending is the admin's limit on waiting join requests; PendingLimit reads it.
	MaximumPending int `json:"maximumPending,omitempty"`

	// PasswordHash is the hash of the password new members need, or empty when none is needed.
	PasswordHash string `json:"passwordHash,omitempty"`

	// AvatarsAllowed reports whether members may upload and show avatars.
	AvatarsAllowed bool `json:"avatarsAllowed"`

	// MaximumSharing caps every member's sharing.
	MaximumSharing sharing.Sharing `json:"maximumSharing"`

	// Ranking is RankByProgress or RankByName.
	Ranking string `json:"ranking"`

	// InviteCode is the secret in every invite link and QR code.
	InviteCode string `json:"inviteCode"`

	// LandingMessage replaces the landing page's default message when set.
	LandingMessage string `json:"landingMessage,omitempty"`

	// ShowInvite reports whether the board's own address offers the invite, not only the invite link.
	ShowInvite bool `json:"showInvite"`

	// StorageVersion is the layout of the board's stored data, StorageVersion or older; zero reads as 1.
	StorageVersion int `json:"storageVersion"`
}

// The join modes.
const (
	// JoinOpen makes a new member active at once.
	JoinOpen = "open"

	// JoinApproval makes a new member pending until the admin approves them.
	JoinApproval = "approval"
)

// The rankings.
const (
	// RankByProgress orders the scoreboard by progress, highest first.
	RankByProgress = "progress"

	// RankByName orders the scoreboard by display name.
	RankByName = "name"
)

// Member is one membership, stored as members/<id>.
type Member struct {
	// ID is derived from PublicKey.
	ID string `json:"id"`

	// PublicKey is the member's P-256 key, an uncompressed point in unpadded base64url.
	PublicKey string `json:"publicKey"`

	// DisplayName is the normalized name everyone on the board sees.
	DisplayName string `json:"displayName"`

	// Status is StatusPending or StatusActive.
	Status string `json:"status"`

	// CanApproveMembers reports whether the admin lets this member approve and reject join requests.
	CanApproveMembers bool `json:"canApproveMembers,omitempty"`

	// Sharing is what the member chose, before the board's maximum.
	Sharing sharing.Sharing `json:"sharing"`

	// HasAvatar reports whether the member has stored an avatar.
	HasAvatar bool `json:"hasAvatar"`

	// JoinedAt is when the member joined, in UTC to the second.
	JoinedAt time.Time `json:"joinedAt"`
}

// The member statuses.
const (
	// StatusPending is a member waiting for the admin's approval.
	StatusPending = "pending"

	// StatusActive is a member on the scoreboard.
	StatusActive = "active"
)

// blocked is a member the admin deleted and blocked, stored as blocked/<id>.
type blocked struct {
	// BlockedAt is when the admin blocked the member.
	BlockedAt time.Time `json:"blockedAt"`
}

// JoinRequest is the body of POST /members.
type JoinRequest struct {
	// InviteCode is from the invite link or QR code.
	InviteCode string `json:"inviteCode"`

	// Password is the board's password, when it has one.
	Password string `json:"password,omitempty"`

	// PublicKey is the key that signed the request, in Member.PublicKey's form.
	PublicKey string `json:"publicKey"`

	// DisplayName is the name to show, before normalizing.
	DisplayName string `json:"displayName"`

	// Sharing is what the member chooses to share.
	Sharing sharing.Sharing `json:"sharing"`
}

// MemberUpdate is the body of PATCH /members/me. Only the fields present change.
type MemberUpdate struct {
	// DisplayName is the new name, or nil to keep the old one.
	DisplayName *string `json:"displayName,omitempty"`

	// Sharing is the new sharing, or nil to keep the old one.
	Sharing *sharing.Sharing `json:"sharing,omitempty"`
}

// WeeklyChallenge is one member's weekly challenge, stored as weeks/<weekStart>/<memberID>. Optional fields are
// pointers so a stripped weekly challenge encodes back to exactly the fields it kept.
type WeeklyChallenge struct {
	// Progress is score divided by goal, above 1 once the goal is passed.
	Progress *float64 `json:"progress,omitempty"`

	// Score is the weekly challenge's points.
	Score *float64 `json:"score,omitempty"`

	// Goal is the weekly challenge's target score, above zero.
	Goal *float64 `json:"goal,omitempty"`

	// Tier is the slug of the challenge tier whose goal this is.
	Tier *string `json:"tier,omitempty"`

	// Breakdown is where Score came from.
	Breakdown *Breakdown `json:"breakdown,omitempty"`

	// Activities is the weekly challenge's workouts that count for points, in the order they started.
	Activities *[]Activity `json:"activities,omitempty"`

	// UpdatedAt is when the server stored the weekly challenge; a request's value is ignored.
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

// Breakdown is where a weekly challenge's score came from. Its parts sum to the score.
type Breakdown struct {
	// Exercise is the points the workouts earned.
	Exercise float64 `json:"exercise"`

	// DailyBonus is the weekly challenge's daily bonus points.
	DailyBonus float64 `json:"dailyBonus"`

	// VarietyBonus is the weekly challenge's variety bonus points.
	VarietyBonus float64 `json:"varietyBonus"`
}

// Activity is one workout in a weekly challenge.
type Activity struct {
	// ID is stable across re-uploads of the same workout, and different on every board.
	ID string `json:"id"`

	// Type is the workout's slug from spec/vocabularies.json.
	Type string `json:"type"`

	// Day is the YYYY-MM-DD date it started, in the member's time zone.
	Day string `json:"day"`

	// Points is what the workout earned; validation requires it.
	Points *ActivityPoints `json:"points,omitempty"`

	// DurationSeconds is how long the workout lasted.
	DurationSeconds *float64 `json:"durationSeconds,omitempty"`

	// DistanceMeters is how far the workout went.
	DistanceMeters *float64 `json:"distanceMeters,omitempty"`

	// Start is when the workout started, the only place its time of day appears.
	Start *time.Time `json:"start,omitempty"`

	// Details is everything else the workout records.
	Details *ActivityDetails `json:"details,omitempty"`

	// HasRoute reports whether a route can be fetched for the activity.
	HasRoute *bool `json:"hasRoute,omitempty"`

	// Strength is a strength workout's exercises and sets.
	Strength *Strength `json:"strength,omitempty"`
}

// ActivityPoints is an activity's base and bonus points.
type ActivityPoints struct {
	// Base is the points for the workout itself.
	Base float64 `json:"base"`

	// Bonus is the extra points the workout earned.
	Bonus float64 `json:"bonus"`
}

// ActivityDetails is everything a workout records apart from its route and sets.
type ActivityDetails struct {
	// ActiveEnergyKilocalories is the energy burned above resting.
	ActiveEnergyKilocalories *float64 `json:"activeEnergyKilocalories,omitempty"`

	// AverageHeartRate is in beats per minute.
	AverageHeartRate *float64 `json:"averageHeartRate,omitempty"`

	// MaximumHeartRate is in beats per minute.
	MaximumHeartRate *float64 `json:"maximumHeartRate,omitempty"`

	// MinimumHeartRate is in beats per minute. Added in 1.1.
	MinimumHeartRate *float64 `json:"minimumHeartRate,omitempty"`

	// AverageSpeedMetersPerSecond is shown as pace or speed as the activity calls for.
	AverageSpeedMetersPerSecond *float64 `json:"averageSpeedMetersPerSecond,omitempty"`

	// ElevationGainMeters is the total climb.
	ElevationGainMeters *float64 `json:"elevationGainMeters,omitempty"`

	// ElevationLossMeters is the total descent.
	ElevationLossMeters *float64 `json:"elevationLossMeters,omitempty"`

	// Steps is the step count.
	Steps *int64 `json:"steps,omitempty"`

	// AverageCadence is in steps or revolutions per minute.
	AverageCadence *float64 `json:"averageCadence,omitempty"`

	// AveragePowerWatts is the mean power output.
	AveragePowerWatts *float64 `json:"averagePowerWatts,omitempty"`

	// AverageMETs is the mean metabolic equivalent.
	AverageMETs *float64 `json:"averageMETs,omitempty"`

	// CarriedLoadKilograms is a ruck, a weighted vest or a loaded carry.
	CarriedLoadKilograms *float64 `json:"carriedLoadKilograms,omitempty"`

	// InclinePercent is the grade, which may be negative.
	InclinePercent *float64 `json:"inclinePercent,omitempty"`

	// StrollerLoadKilograms is the load pushed in a stroller.
	StrollerLoadKilograms *float64 `json:"strollerLoadKilograms,omitempty"`

	// ExposureTemperatureCelsius is a sauna's or a cold plunge's temperature, which may be negative.
	ExposureTemperatureCelsius *float64 `json:"exposureTemperatureCelsius,omitempty"`

	// Weather is the weather outdoors during the workout.
	Weather *Weather `json:"weather,omitempty"`
}

// Weather is the weather during an outdoor workout.
type Weather struct {
	// TemperatureCelsius is the air temperature, which may be negative.
	TemperatureCelsius *float64 `json:"temperatureCelsius,omitempty"`

	// HumidityPercent is the relative humidity, from 0 to 100.
	HumidityPercent *float64 `json:"humidityPercent,omitempty"`
}

// Strength is a strength workout's exercises.
type Strength struct {
	// Exercises is the exercises in the order they were done.
	Exercises []StrengthExercise `json:"exercises"`

	// Deload reports whether the session was run as a deload. Added in 1.1.
	Deload *bool `json:"deload,omitempty"`

	// Muscles is how hard the session worked each muscle group. Added in 1.1.
	Muscles *[]StrengthMuscle `json:"muscles,omitempty"`
}

// StrengthMuscle is one muscle group a strength workout worked. Added in 1.1.
type StrengthMuscle struct {
	// Group is the muscle group's slug from spec/vocabularies.json.
	Group string `json:"group"`

	// Intensity is the hardest the group was worked, a slug from spec/vocabularies.json.
	Intensity string `json:"intensity"`
}

// StrengthExercise is one exercise and its sets.
type StrengthExercise struct {
	// CatalogID is a bundled exercise's UUID, so each viewer reads its name in their own language.
	CatalogID *string `json:"catalogID,omitempty"`

	// Name is a custom exercise's name.
	Name *string `json:"name,omitempty"`

	// LoadIsAssistance reports whether the weights are assistance subtracted from bodyweight.
	LoadIsAssistance *bool `json:"loadIsAssistance,omitempty"`

	// Sets is the exercise's sets in order.
	Sets []StrengthSet `json:"sets"`

	// AngleDegrees is the angle the exercise was set up at, such as a bench's incline. Added in 1.1.
	AngleDegrees *int64 `json:"angleDegrees,omitempty"`

	// Injured reports whether the exercise was trained around an injury. Added in 1.1.
	Injured *bool `json:"injured,omitempty"`

	// Modifiers is the load, range and cadence behaviours applied to the exercise. Added in 1.1.
	Modifiers *[]StrengthModifier `json:"modifiers,omitempty"`

	// PersonalRecord reports whether the exercise beat every earlier session of the same movement. Added in 1.1.
	PersonalRecord *bool `json:"personalRecord,omitempty"`

	// VolumeKilograms is the load the exercise moved across its sets. Added in 1.1.
	VolumeKilograms *float64 `json:"volumeKilograms,omitempty"`
}

// StrengthModifier is one behaviour applied to an exercise, carrying at most one value. Added in 1.1.
type StrengthModifier struct {
	// Kind is the modifier's slug from spec/vocabularies.json.
	Kind string `json:"kind"`

	// Assist is how the machine or a spotter steps in, a slug from spec/vocabularies.json.
	Assist *string `json:"assist,omitempty"`

	// PartialRange is the part of the range the reps went through, a slug from spec/vocabularies.json.
	PartialRange *string `json:"partialRange,omitempty"`

	// Percent is a share of the working load, 125 meaning 125%.
	Percent *float64 `json:"percent,omitempty"`

	// Tempo is the seconds spent in each phase of a rep.
	Tempo *StrengthTempo `json:"tempo,omitempty"`

	// VelocityMetersPerSecond is a target cable velocity.
	VelocityMetersPerSecond *float64 `json:"velocityMetersPerSecond,omitempty"`

	// WeightKilograms is an absolute load, such as a band's or chains'.
	WeightKilograms *float64 `json:"weightKilograms,omitempty"`

	// WeightPerRepKilograms is a load added or removed after every rep.
	WeightPerRepKilograms *float64 `json:"weightPerRepKilograms,omitempty"`
}

// StrengthTempo is the seconds spent in each phase of a rep, in the order a lifter counts them. Added in 1.1.
type StrengthTempo struct {
	// EccentricSeconds is the lowering phase.
	EccentricSeconds int64 `json:"eccentricSeconds"`

	// BottomPauseSeconds is the pause at the bottom.
	BottomPauseSeconds int64 `json:"bottomPauseSeconds"`

	// ConcentricSeconds is the lifting phase.
	ConcentricSeconds int64 `json:"concentricSeconds"`

	// TopPauseSeconds is the pause at the top.
	TopPauseSeconds int64 `json:"topPauseSeconds"`
}

// StrengthSet is one set. A set split per side keeps the left side at the top level.
type StrengthSet struct {
	// Type is the set's slug from spec/vocabularies.json.
	Type string `json:"type"`

	// Reps is the repetitions done.
	Reps *int64 `json:"reps,omitempty"`

	// DurationSeconds is how long a timed set lasted.
	DurationSeconds *float64 `json:"durationSeconds,omitempty"`

	// DistanceMeters is how far a distance set went.
	DistanceMeters *float64 `json:"distanceMeters,omitempty"`

	// WeightKilograms is the load lifted.
	WeightKilograms *float64 `json:"weightKilograms,omitempty"`

	// Right is the right side of a set split per side.
	Right *StrengthSide `json:"right,omitempty"`

	// Clusters is the efforts after the first in a drop set or a myo-rep set.
	Clusters *[]StrengthCluster `json:"clusters,omitempty"`

	// RepsInReserve is how many more reps the lifter reported they could have done. Added in 1.1.
	RepsInReserve *int64 `json:"repsInReserve,omitempty"`
}

// StrengthSide is the right side of a set split per side.
type StrengthSide struct {
	// Reps is the right side's repetitions.
	Reps *int64 `json:"reps,omitempty"`

	// DurationSeconds is how long the right side lasted.
	DurationSeconds *float64 `json:"durationSeconds,omitempty"`

	// WeightKilograms is the right side's load.
	WeightKilograms *float64 `json:"weightKilograms,omitempty"`

	// RepsInReserve is the right side's reported reps in reserve. Added in 1.1.
	RepsInReserve *int64 `json:"repsInReserve,omitempty"`
}

// StrengthCluster is one effort after the first in a drop set or a myo-rep set.
type StrengthCluster struct {
	// Reps is the effort's repetitions.
	Reps *int64 `json:"reps,omitempty"`

	// WeightKilograms is the effort's load.
	WeightKilograms *float64 `json:"weightKilograms,omitempty"`
}

// Route is one activity's route, stored as routes/<memberID>/<weekStart>/<activityID>.
type Route struct {
	// Polyline is the route as an encoded polyline of at least two points.
	Polyline string `json:"polyline"`
}

// MemberSummary is a member as everyone on the board sees them.
type MemberSummary struct {
	// ID is the member's identifier.
	ID string `json:"id"`

	// DisplayName is the member's name.
	DisplayName string `json:"displayName"`

	// HasAvatar reports whether the member has an avatar and the board shows avatars.
	HasAvatar bool `json:"hasAvatar"`

	// Sharing is the member's effective sharing, so a client knows what to expect in their weekly challenge.
	Sharing sharing.Sharing `json:"sharing"`
}

// MemberRequest is a member waiting for approval, as a member who approves requests sees them.
type MemberRequest struct {
	// ID is the member's identifier.
	ID string `json:"id"`

	// DisplayName is the member's name.
	DisplayName string `json:"displayName"`

	// JoinedAt is when the member asked to join.
	JoinedAt time.Time `json:"joinedAt"`
}

// MemberRequests is the body of GET /member-requests.
type MemberRequests struct {
	// Requests are the waiting members, oldest first.
	Requests []MemberRequest `json:"requests"`
}

// WeeklyChallengeSummary is a weekly challenge as the scoreboard shows it, without activities.
type WeeklyChallengeSummary struct {
	// Progress is the weekly challenge's progress, zero when it has none.
	Progress float64 `json:"progress"`

	// Score is the weekly challenge's points, when shared.
	Score *float64 `json:"score,omitempty"`

	// Goal is the weekly challenge's target score, when shared.
	Goal *float64 `json:"goal,omitempty"`

	// Tier is the weekly challenge's tier, when shared.
	Tier *string `json:"tier,omitempty"`

	// Breakdown is where the score came from, when shared.
	Breakdown *Breakdown `json:"breakdown,omitempty"`

	// ActivityCount is present when activities are shared.
	ActivityCount *int `json:"activityCount,omitempty"`

	// UpdatedAt is when the server stored the weekly challenge.
	UpdatedAt time.Time `json:"updatedAt"`
}

// ScoreboardEntry is one row of the scoreboard.
type ScoreboardEntry struct {
	// Member is the row's member.
	Member MemberSummary `json:"member"`

	// Challenge is the member's weekly challenge, or nil when they have not uploaded one.
	Challenge *WeeklyChallengeSummary `json:"challenge,omitempty"`
}

// Scoreboard is the scoreboard for one week.
type Scoreboard struct {
	// WeekStart is the week's YYYY-MM-DD start date.
	WeekStart string `json:"weekStart"`

	// Ranking is the board's ranking, which orders Entries.
	Ranking string `json:"ranking"`

	// Entries is every active member, members with a weekly challenge first.
	Entries []ScoreboardEntry `json:"entries"`
}

// MemberWeeklyChallenge is one member and their weekly challenge.
type MemberWeeklyChallenge struct {
	// Member is the weekly challenge's member.
	Member MemberSummary `json:"member"`

	// Challenge is the member's weekly challenge, stripped to their sharing now.
	Challenge WeeklyChallenge `json:"challenge"`
}
