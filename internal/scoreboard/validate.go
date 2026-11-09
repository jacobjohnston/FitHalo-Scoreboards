package scoreboard

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/problem"
)

// The limits openapi.yaml publishes.
const (
	// MaximumDisplayNameLength is the most characters in a display name, after trimming.
	MaximumDisplayNameLength = 40

	// MaximumActivities is the most activities in a weekly challenge.
	MaximumActivities = 200

	// MaximumExercises is the most exercises in an activity.
	MaximumExercises = 50

	// MaximumSets is the most sets in an exercise.
	MaximumSets = 50

	// MaximumClusters is the most clusters in a set.
	MaximumClusters = 20

	// MaximumExerciseName is the most characters in a custom exercise's name.
	MaximumExerciseName = 100

	// MaximumModifiers is the most modifiers on an exercise.
	MaximumModifiers = 20

	// MaximumMuscles is the most muscle groups in an activity.
	MaximumMuscles = 30

	// MaximumAngleDegrees is the steepest an exercise's angle may be either side of flat.
	MaximumAngleDegrees = 90

	// MaximumSlugLength is the most bytes in a vocabulary slug.
	MaximumSlugLength = 64
)

// dateLayout is the YYYY-MM-DD form of every week start and activity day.
const dateLayout = "2006-01-02"

var (
	// identifierPattern matches 16 bytes in unpadded base64url.
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

	// slugPattern matches a lower camel case vocabulary slug.
	slugPattern = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)

	// uuidPattern matches a UUID in either case.
	uuidPattern = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)
)

// weekdays maps each WeekStartDay value to its weekday.
var weekdays = map[string]time.Weekday{
	"monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday, "thursday": time.Thursday,
	"friday": time.Friday, "saturday": time.Saturday, "sunday": time.Sunday,
}

// IsIdentifier reports whether value is 16 bytes in unpadded base64url.
func IsIdentifier(value string) bool {
	return identifierPattern.MatchString(value)
}

// CurrentWeekStart is the start of the board's week containing now, by the server's UTC date.
func CurrentWeekStart(board Board, now time.Time) time.Time {
	utc := now.UTC()
	today := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	daysSinceStart := (int(today.Weekday()) - int(weekdays[board.WeekStartDay]) + 7) % 7
	return today.AddDate(0, 0, -daysSinceStart)
}

// OldestKeptWeek is the start of the oldest week the board keeps.
func OldestKeptWeek(board Board, now time.Time) time.Time {
	return CurrentWeekStart(board, now).AddDate(0, 0, -7*(board.RetentionWeeks-1))
}

// ParseWeekStart checks a week's date against the board: on its start day, no older than it keeps, and
// no later than next week, which leaves room for members ahead of the server's time zone.
func ParseWeekStart(board Board, value string, now time.Time) (time.Time, *problem.Problem) {
	weekStart, err := time.Parse(dateLayout, value)
	if err != nil || weekStart.Format(dateLayout) != value {
		return time.Time{}, problem.New(problem.BadRequest, "the week is not a YYYY-MM-DD date")
	}
	if weekStart.Weekday() != weekdays[board.WeekStartDay] {
		return time.Time{}, problem.New(problem.WeekOutOfRange, "the board's weekly challenges start on "+board.WeekStartDay)
	}
	if weekStart.Before(OldestKeptWeek(board, now)) {
		return time.Time{}, problem.New(problem.WeekOutOfRange, "the board no longer keeps that week")
	}
	if weekStart.After(CurrentWeekStart(board, now).AddDate(0, 0, 7)) {
		return time.Time{}, problem.New(problem.WeekOutOfRange, "that week has not started")
	}
	return weekStart, nil
}

// NormalizeDisplayName trims a display name, collapses each run of spaces to one, and rejects one that is
// empty, too long, invisible, or has characters or sequences that could make it look like someone else's.
func NormalizeDisplayName(name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("the display name must be 1 to %d characters", MaximumDisplayNameLength)
	}
	name = collapseSpaces(strings.TrimSpace(name))
	if length := utf8.RuneCountInString(name); length == 0 || length > MaximumDisplayNameLength {
		return "", fmt.Errorf("the display name must be 1 to %d characters", MaximumDisplayNameLength)
	}
	if hasHiddenCharacter(name) {
		return "", errors.New("the display name has a control character")
	}
	if hasUndefinedCharacter(name) {
		return "", errors.New("the display name has a private-use or unassigned character")
	}
	if hasOverlongSequence(name) {
		return "", errors.New("the display name stacks too many marks or tags")
	}
	if !hasVisibleCharacter(name) {
		return "", errors.New("the display name has no visible character")
	}
	return name, nil
}

// The characters that may sit inside an emoji sequence although Unicode classes them as format characters.
const (
	// zeroWidthJoiner joins two emoji into one, as in 🏳️‍🌈.
	zeroWidthJoiner = '\u200D'

	// emojiPresentation asks for an emoji's colour form, and may precede a joiner.
	emojiPresentation = '\uFE0F'

	// blackFlag starts a subdivision flag such as 🏴󠁧󠁢󠁳󠁣󠁴󠁿, whose tag characters follow it.
	blackFlag = '\U0001F3F4'

	// firstTag is the lowest tag character in a subdivision flag.
	firstTag = '\U000E0020'

	// cancelTag ends a subdivision flag.
	cancelTag = '\U000E007F'
)

// The longest runs a display name may hold.
const (
	// maximumMarkRun is the most combining marks one character may carry.
	maximumMarkRun = 3

	// maximumFlagTags is the most tag characters a subdivision flag may hold before its cancel tag.
	maximumFlagTags = 6
)

// collapseSpaces replaces each run of space separators, such as no-break or ideographic spaces, with one space.
func collapseSpaces(text string) string {
	var result strings.Builder
	inRun := false
	for _, character := range text {
		if unicode.Is(unicode.Zs, character) {
			if !inRun {
				result.WriteByte(' ')
			}
			inRun = true
			continue
		}
		inRun = false
		result.WriteRune(character)
	}
	return result.String()
}

// hasHiddenCharacter reports whether text has a control, format (which includes the text-direction
// characters), line or paragraph separator character, apart from the joiners and tags of an emoji.
func hasHiddenCharacter(text string) bool {
	characters := []rune(text)
	for index, character := range characters {
		if unicode.IsControl(character) || unicode.In(character, unicode.Zl, unicode.Zp) {
			return true
		}
		if unicode.Is(unicode.Cf, character) && !inEmojiSequence(characters, index) {
			return true
		}
	}
	return false
}

// inEmojiSequence reports whether the format character at index joins two emoji or tags a flag.
func inEmojiSequence(characters []rune, index int) bool {
	if index == 0 {
		return false
	}
	character, previous := characters[index], characters[index-1]
	switch {
	case character == zeroWidthJoiner:
		endsEmoji := unicode.In(previous, unicode.So, unicode.Sk) || previous == emojiPresentation
		return endsEmoji && index+1 < len(characters) && unicode.Is(unicode.So, characters[index+1])
	case character >= firstTag && character <= cancelTag:
		return previous == blackFlag || previous >= firstTag && previous < cancelTag
	}
	return false
}

// hasUndefinedCharacter reports whether text has a private-use or unassigned character, which renders
// differently on every device.
func hasUndefinedCharacter(text string) bool {
	for _, character := range text {
		// Go's unicode.C includes the unassigned characters, so the assigned ones are listed by class.
		if unicode.Is(unicode.Co, character) || !unicode.In(character,
			unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z, unicode.Cc, unicode.Cf) {
			return true
		}
	}
	return false
}

// hasOverlongSequence reports whether text stacks more than maximumMarkRun marks on a character, or has a
// flag's tag sequence longer than maximumFlagTags or not closed by a cancel tag.
func hasOverlongSequence(text string) bool {
	marks, tags := 0, 0
	for _, character := range text {
		if !unicode.IsMark(character) {
			marks = 0
		} else if marks++; marks > maximumMarkRun {
			return true
		}
		switch {
		case character == cancelTag:
			tags = 0
		case character >= firstTag && character < cancelTag:
			if tags++; tags > maximumFlagTags {
				return true
			}
		case tags > 0:
			return true
		}
	}
	return tags > 0
}

// blankCharacters are letters and symbols that render as nothing: the Hangul fillers and the empty Braille cell.
var blankCharacters = map[rune]bool{0x115F: true, 0x1160: true, 0x3164: true, 0xFFA0: true, 0x2800: true}

// hasVisibleCharacter reports whether text has a graphic character that is neither a space, a mark nor blank.
func hasVisibleCharacter(text string) bool {
	for _, character := range text {
		if unicode.IsGraphic(character) && !unicode.IsSpace(character) && !unicode.IsMark(character) &&
			!blankCharacters[character] {
			return true
		}
	}
	return false
}

// validateWeeklyChallenge checks a decoded weekly challenge's shape and limits.
func validateWeeklyChallenge(challenge *WeeklyChallenge, weekStart time.Time) error {
	if challenge.Progress == nil {
		return errors.New("progress is required")
	}
	checks := []error{
		nonNegative("progress", challenge.Progress),
		nonNegative("score", challenge.Score),
		nonNegative("goal", challenge.Goal),
	}
	if challenge.Goal != nil && *challenge.Goal == 0 {
		checks = append(checks, errors.New("goal must be above zero"))
	}
	if challenge.Tier != nil {
		checks = append(checks, slug("tier", *challenge.Tier))
	}
	if challenge.Breakdown != nil {
		checks = append(checks,
			nonNegative("breakdown.exercise", &challenge.Breakdown.Exercise),
			nonNegative("breakdown.dailyBonus", &challenge.Breakdown.DailyBonus),
			nonNegative("breakdown.varietyBonus", &challenge.Breakdown.VarietyBonus))
	}
	if err := errors.Join(checks...); err != nil {
		return err
	}
	if challenge.Activities == nil {
		return nil
	}
	if len(*challenge.Activities) > MaximumActivities {
		return fmt.Errorf("a weekly challenge has at most %d activities", MaximumActivities)
	}
	seen := map[string]bool{}
	firstDay, lastDay := weekStart, weekStart.AddDate(0, 0, 6)
	for index := range *challenge.Activities {
		activity := &(*challenge.Activities)[index]
		where := fmt.Sprintf("activities[%d]", index)
		if !IsIdentifier(activity.ID) {
			return fmt.Errorf("%s.id is not an identifier", where)
		}
		if seen[activity.ID] {
			return fmt.Errorf("%s.id repeats another activity's", where)
		}
		seen[activity.ID] = true
		if err := slug(where+".type", activity.Type); err != nil {
			return err
		}
		day, err := time.Parse(dateLayout, activity.Day)
		if err != nil || day.Before(firstDay) || day.After(lastDay) {
			return fmt.Errorf("%s.day is not a day of this week", where)
		}
		if activity.Points == nil {
			return fmt.Errorf("%s.points is required", where)
		}
		if err := errors.Join(
			nonNegative(where+".points.base", &activity.Points.Base),
			nonNegative(where+".points.bonus", &activity.Points.Bonus),
			nonNegative(where+".durationSeconds", activity.DurationSeconds),
			nonNegative(where+".distanceMeters", activity.DistanceMeters),
			validateDetails(where+".details", activity.Details),
			validateStrength(where+".strength", activity.Strength),
		); err != nil {
			return err
		}
	}
	return nil
}

// validateDetails checks an activity's details, naming each field from where.
func validateDetails(where string, details *ActivityDetails) error {
	if details == nil {
		return nil
	}
	checks := []error{
		nonNegative(where+".activeEnergyKilocalories", details.ActiveEnergyKilocalories),
		nonNegative(where+".averageHeartRate", details.AverageHeartRate),
		nonNegative(where+".maximumHeartRate", details.MaximumHeartRate),
		nonNegative(where+".minimumHeartRate", details.MinimumHeartRate),
		nonNegative(where+".averageSpeedMetersPerSecond", details.AverageSpeedMetersPerSecond),
		nonNegative(where+".elevationGainMeters", details.ElevationGainMeters),
		nonNegative(where+".elevationLossMeters", details.ElevationLossMeters),
		nonNegativeInteger(where+".steps", details.Steps),
		nonNegative(where+".averageCadence", details.AverageCadence),
		nonNegative(where+".averagePowerWatts", details.AveragePowerWatts),
		nonNegative(where+".averageMETs", details.AverageMETs),
		nonNegative(where+".carriedLoadKilograms", details.CarriedLoadKilograms),
		finite(where+".inclinePercent", details.InclinePercent),
		nonNegative(where+".strollerLoadKilograms", details.StrollerLoadKilograms),
		finite(where+".exposureTemperatureCelsius", details.ExposureTemperatureCelsius),
	}
	if details.Weather != nil {
		checks = append(checks, finite(where+".weather.temperatureCelsius", details.Weather.TemperatureCelsius))
		if humidity := details.Weather.HumidityPercent; humidity != nil && (*humidity < 0 || *humidity > 100) {
			checks = append(checks, fmt.Errorf("%s.weather.humidityPercent is not a percentage", where))
		}
	}
	return errors.Join(checks...)
}

// validateStrength checks a strength workout's limits and every set, naming each field from where.
func validateStrength(where string, strength *Strength) error {
	if strength == nil {
		return nil
	}
	if len(strength.Exercises) > MaximumExercises {
		return fmt.Errorf("%s has more than %d exercises", where, MaximumExercises)
	}
	if err := validateMuscles(where+".muscles", strength.Muscles); err != nil {
		return err
	}
	for exerciseIndex, exercise := range strength.Exercises {
		exerciseWhere := fmt.Sprintf("%s.exercises[%d]", where, exerciseIndex)
		if exercise.CatalogID == nil && exercise.Name == nil {
			return fmt.Errorf("%s needs a catalogID or a name", exerciseWhere)
		}
		if exercise.CatalogID != nil && !uuidPattern.MatchString(*exercise.CatalogID) {
			return fmt.Errorf("%s.catalogID is not a UUID", exerciseWhere)
		}
		if exercise.Name != nil {
			if length := utf8.RuneCountInString(*exercise.Name); length == 0 || length > MaximumExerciseName {
				return fmt.Errorf("%s.name must be 1 to %d characters", exerciseWhere, MaximumExerciseName)
			}
			if hasHiddenCharacter(*exercise.Name) {
				return fmt.Errorf("%s.name has a control character", exerciseWhere)
			}
		}
		if len(exercise.Sets) > MaximumSets {
			return fmt.Errorf("%s has more than %d sets", exerciseWhere, MaximumSets)
		}
		if angle := exercise.AngleDegrees; angle != nil && (*angle < -MaximumAngleDegrees || *angle > MaximumAngleDegrees) {
			return fmt.Errorf("%s.angleDegrees is not between -%d and %d", exerciseWhere, MaximumAngleDegrees, MaximumAngleDegrees)
		}
		if err := errors.Join(
			nonNegative(exerciseWhere+".volumeKilograms", exercise.VolumeKilograms),
			validateModifiers(exerciseWhere+".modifiers", exercise.Modifiers),
		); err != nil {
			return err
		}
		for setIndex, set := range exercise.Sets {
			setWhere := fmt.Sprintf("%s.sets[%d]", exerciseWhere, setIndex)
			checks := []error{
				slug(setWhere+".type", set.Type),
				nonNegativeInteger(setWhere+".reps", set.Reps),
				nonNegative(setWhere+".durationSeconds", set.DurationSeconds),
				nonNegative(setWhere+".distanceMeters", set.DistanceMeters),
				nonNegative(setWhere+".weightKilograms", set.WeightKilograms),
				nonNegativeInteger(setWhere+".repsInReserve", set.RepsInReserve),
			}
			if set.Right != nil {
				checks = append(checks,
					nonNegativeInteger(setWhere+".right.reps", set.Right.Reps),
					nonNegative(setWhere+".right.durationSeconds", set.Right.DurationSeconds),
					nonNegative(setWhere+".right.weightKilograms", set.Right.WeightKilograms),
					nonNegativeInteger(setWhere+".right.repsInReserve", set.Right.RepsInReserve))
			}
			if set.Clusters != nil {
				if len(*set.Clusters) > MaximumClusters {
					checks = append(checks, fmt.Errorf("%s has more than %d clusters", setWhere, MaximumClusters))
				}
				for clusterIndex, cluster := range *set.Clusters {
					clusterWhere := fmt.Sprintf("%s.clusters[%d]", setWhere, clusterIndex)
					checks = append(checks,
						nonNegativeInteger(clusterWhere+".reps", cluster.Reps),
						nonNegative(clusterWhere+".weightKilograms", cluster.WeightKilograms))
				}
			}
			if err := errors.Join(checks...); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateMuscles checks an activity's muscle groups, naming each field from where.
func validateMuscles(where string, muscles *[]StrengthMuscle) error {
	if muscles == nil {
		return nil
	}
	if len(*muscles) > MaximumMuscles {
		return fmt.Errorf("%s has more than %d muscle groups", where, MaximumMuscles)
	}
	for index, muscle := range *muscles {
		muscleWhere := fmt.Sprintf("%s[%d]", where, index)
		if err := errors.Join(slug(muscleWhere+".group", muscle.Group), slug(muscleWhere+".intensity", muscle.Intensity)); err != nil {
			return err
		}
	}
	return nil
}

// validateModifiers checks an exercise's modifiers, each with at most one value, naming each field from where.
func validateModifiers(where string, modifiers *[]StrengthModifier) error {
	if modifiers == nil {
		return nil
	}
	if len(*modifiers) > MaximumModifiers {
		return fmt.Errorf("%s has more than %d modifiers", where, MaximumModifiers)
	}
	for index, modifier := range *modifiers {
		modifierWhere := fmt.Sprintf("%s[%d]", where, index)
		checks := []error{
			slug(modifierWhere+".kind", modifier.Kind),
			nonNegative(modifierWhere+".percent", modifier.Percent),
			nonNegative(modifierWhere+".velocityMetersPerSecond", modifier.VelocityMetersPerSecond),
			nonNegative(modifierWhere+".weightKilograms", modifier.WeightKilograms),
			nonNegative(modifierWhere+".weightPerRepKilograms", modifier.WeightPerRepKilograms),
		}
		if modifier.Assist != nil {
			checks = append(checks, slug(modifierWhere+".assist", *modifier.Assist))
		}
		if modifier.PartialRange != nil {
			checks = append(checks, slug(modifierWhere+".partialRange", *modifier.PartialRange))
		}
		if tempo := modifier.Tempo; tempo != nil {
			checks = append(checks,
				nonNegativeInteger(modifierWhere+".tempo.eccentricSeconds", &tempo.EccentricSeconds),
				nonNegativeInteger(modifierWhere+".tempo.bottomPauseSeconds", &tempo.BottomPauseSeconds),
				nonNegativeInteger(modifierWhere+".tempo.concentricSeconds", &tempo.ConcentricSeconds),
				nonNegativeInteger(modifierWhere+".tempo.topPauseSeconds", &tempo.TopPauseSeconds))
		}
		if modifier.valueCount() > 1 {
			checks = append(checks, fmt.Errorf("%s has more than one value", modifierWhere))
		}
		if err := errors.Join(checks...); err != nil {
			return err
		}
	}
	return nil
}

// valueCount is how many of the modifier's values are set.
func (modifier StrengthModifier) valueCount() int {
	count := 0
	for _, isSet := range []bool{
		modifier.Assist != nil,
		modifier.PartialRange != nil,
		modifier.Percent != nil,
		modifier.Tempo != nil,
		modifier.VelocityMetersPerSecond != nil,
		modifier.WeightKilograms != nil,
		modifier.WeightPerRepKilograms != nil,
	} {
		if isSet {
			count++
		}
	}
	return count
}

// slug requires value to be a vocabulary slug.
func slug(where, value string) error {
	if len(value) > MaximumSlugLength || !slugPattern.MatchString(value) {
		return fmt.Errorf("%s is not a slug", where)
	}
	return nil
}

// finite requires value, when present, to be neither NaN nor infinite.
func finite(where string, value *float64) error {
	if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
		return fmt.Errorf("%s is not a number", where)
	}
	return nil
}

// nonNegative requires value, when present, to be finite and at least zero.
func nonNegative(where string, value *float64) error {
	if err := finite(where, value); err != nil {
		return err
	}
	if value != nil && *value < 0 {
		return fmt.Errorf("%s is negative", where)
	}
	return nil
}

// nonNegativeInteger requires value, when present, to be at least zero.
func nonNegativeInteger(where string, value *int64) error {
	if value != nil && *value < 0 {
		return fmt.Errorf("%s is negative", where)
	}
	return nil
}
