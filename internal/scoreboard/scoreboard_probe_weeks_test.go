package scoreboard_test

import (
	"regexp"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/problem"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
)

// oneWeek is seven days.
const oneWeek = 7 * 24 * time.Hour

// weekStartPattern is the only shape a week start may take.
var weekStartPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// sampleNows is a set of clock readings on the edges the week arithmetic must get right.
var sampleNows = []struct {
	// name says which edge the reading sits on.
	name string

	// now is the reading.
	now time.Time
}{
	{name: "monday midnight", now: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)},
	{name: "monday last second", now: time.Date(2026, 10, 19, 23, 59, 59, 0, time.UTC)},
	{name: "sunday noon", now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)},
	{name: "sunday last nanosecond", now: time.Date(2026, 10, 18, 23, 59, 59, 999999999, time.UTC)},
	{name: "leap day", now: time.Date(2028, 2, 29, 8, 0, 0, 0, time.UTC)},
	{name: "new year's eve", now: time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)},
	{name: "new year's day", now: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
	{name: "ahead of UTC", now: time.Date(2026, 10, 19, 10, 0, 0, 0, time.FixedZone("+14", 14*3600))},
	{name: "behind UTC", now: time.Date(2026, 10, 18, 20, 0, 0, 0, time.FixedZone("-12", -12*3600))},
	{name: "epoch", now: time.Unix(0, 0).UTC()},
}

// boardStarting returns a board whose weeks start on day and that keeps retention weeks.
func boardStarting(t *testing.T, day string, retention int) scoreboard.Board {
	t.Helper()
	board, err := scoreboard.DefaultBoard()
	if err != nil {
		t.Fatal(err)
	}
	board.WeekStartDay = day
	board.RetentionWeeks = retention
	return board
}

// TestIsWeekStartDayKnowsOnlyLowercaseEnglish accepts the seven lowercase weekdays and nothing else.
func TestIsWeekStartDayKnowsOnlyLowercaseEnglish(t *testing.T) {
	for _, day := range weekStartDays {
		if !scoreboard.IsWeekStartDay(day) {
			t.Errorf("IsWeekStartDay(%q) = false", day)
		}
	}
	for _, day := range []string{"Monday", "mon", "", "MONDAY", " monday", "monday ", "lundi", "0", "1"} {
		if scoreboard.IsWeekStartDay(day) {
			t.Errorf("IsWeekStartDay(%q) = true", day)
		}
	}
}

// TestCurrentWeekStartIsOnTheStartDayByUTC checks the current week start for every start day and sample clock.
func TestCurrentWeekStartIsOnTheStartDayByUTC(t *testing.T) {
	for _, day := range weekStartDays {
		for _, sample := range sampleNows {
			board := boardStarting(t, day, 52)
			start := scoreboard.CurrentWeekStart(board, sample.now)
			utc := start.UTC()
			if utc.Weekday().String() != capitalise(day) {
				t.Errorf("%s/%s: start %s is a %s", day, sample.name, utc, utc.Weekday())
			}
			if hour, minute, second := utc.Clock(); hour != 0 || minute != 0 || second != 0 || utc.Nanosecond() != 0 {
				t.Errorf("%s/%s: start %s is not midnight UTC", day, sample.name, utc)
			}
			if start.After(sample.now) {
				t.Errorf("%s/%s: start %s is after now %s", day, sample.name, start, sample.now)
			}
			if sample.now.Sub(start) >= oneWeek {
				t.Errorf("%s/%s: start %s is a week or more before now %s", day, sample.name, start, sample.now)
			}
			if !start.Equal(scoreboard.CurrentWeekStart(board, sample.now.UTC())) {
				t.Errorf("%s/%s: start differs between %s and its UTC reading", day, sample.name, sample.now)
			}
		}
	}
}

// capitalise upper-cases the first letter of an ASCII word.
func capitalise(word string) string {
	if word == "" {
		return word
	}
	return string(word[0]-'a'+'A') + word[1:]
}

// TestCurrentWeekStartUsesTheUTCDate pins the doc's "by the server's UTC date" at a zone where the local date differs.
func TestCurrentWeekStartUsesTheUTCDate(t *testing.T) {
	board := boardStarting(t, "monday", 52)
	// Monday 10:00 at +14 is Sunday 20:00 UTC, so the current week is still the one starting 2026-10-12.
	ahead := time.Date(2026, 10, 19, 10, 0, 0, 0, time.FixedZone("+14", 14*3600))
	if got, want := scoreboard.CurrentWeekStart(board, ahead), time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("ahead of UTC: got %s, want %s", got, want)
	}
	// Sunday 20:00 at -12 is Monday 08:00 UTC, so the current week starts 2026-10-19.
	behind := time.Date(2026, 10, 18, 20, 0, 0, 0, time.FixedZone("-12", -12*3600))
	if got, want := scoreboard.CurrentWeekStart(board, behind), time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("behind UTC: got %s, want %s", got, want)
	}
}

// TestOldestKeptWeekCountsTheCurrentWeek keeps retention weeks including the current one.
func TestOldestKeptWeekCountsTheCurrentWeek(t *testing.T) {
	for _, retention := range []int{1, 2, 52, 520} {
		for _, day := range weekStartDays {
			for _, sample := range sampleNows {
				board := boardStarting(t, day, retention)
				current := scoreboard.CurrentWeekStart(board, sample.now)
				oldest := scoreboard.OldestKeptWeek(board, sample.now)
				if want := current.Add(-time.Duration(retention-1) * oneWeek); !oldest.Equal(want) {
					t.Errorf("retention %d, %s/%s: oldest %s, want %s", retention, day, sample.name, oldest, want)
				}
			}
		}
	}
}

// TestParseWeekStartWindow accepts from the oldest kept week to next week, on the start day only.
func TestParseWeekStartWindow(t *testing.T) {
	for _, retention := range []int{1, 2, 52} {
		for _, day := range weekStartDays {
			for _, sample := range sampleNows {
				board := boardStarting(t, day, retention)
				current := scoreboard.CurrentWeekStart(board, sample.now)
				label := func(offset int) string {
					return current.Add(time.Duration(offset) * oneWeek).Format("2006-01-02")
				}
				context := day + "/" + sample.name
				assertParsed(t, context+" current", board, label(0), sample.now, "")
				assertParsed(t, context+" next week", board, label(1), sample.now, "")
				assertParsed(t, context+" two weeks ahead", board, label(2), sample.now, problem.WeekOutOfRange)
				assertParsed(t, context+" oldest kept", board, label(-(retention - 1)), sample.now, "")
				assertParsed(t, context+" one past oldest", board, label(-retention), sample.now, problem.WeekOutOfRange)
				for offset := 1; offset < 7; offset++ {
					value := current.AddDate(0, 0, offset).Format("2006-01-02")
					assertParsed(t, context+" day "+value, board, value, sample.now, problem.WeekOutOfRange)
				}
			}
		}
	}
}

// assertParsed checks ParseWeekStart's code for value, and that an accepted value round-trips.
func assertParsed(t *testing.T, context string, board scoreboard.Board, value string, now time.Time, want problem.Code) {
	t.Helper()
	parsed, failure := scoreboard.ParseWeekStart(board, value, now)
	var got problem.Code
	if failure != nil {
		got = failure.Code
	}
	if got != want {
		t.Errorf("%s: ParseWeekStart(%q) code %q, want %q", context, value, got, want)
		return
	}
	if failure == nil && parsed.UTC().Format("2006-01-02") != value {
		t.Errorf("%s: ParseWeekStart(%q) = %s, does not round-trip", context, value, parsed)
	}
}

// TestParseWeekStartMalformedValues refuses every value that is not exactly YYYY-MM-DD on the start day.
func TestParseWeekStartMalformedValues(t *testing.T) {
	board := boardStarting(t, "monday", 52)
	malformed := []string{
		"2026-1-5", "2026-10-19T00:00:00Z", "20261019", "2026-10-19 ", " 2026-10-19", "", "2026-02-30",
		"0000-01-01", "+2026-10-19", "2026-10-19\n", "2026-10-19\x00", "2026/10/19", "2026-10-19.",
		"2026-10-1９", "٢٠٢٦-10-19", "2026-10-19T", "2026-10", "2026", "9999-99-99", "-001-10-19",
		"2026-10-19 00:00", "2026-13-05", "2026-00-05", "2026-10-00", "2026-10-32", "1e3-10-19",
	}
	codes := map[string]problem.Code{}
	for _, value := range malformed {
		parsed, failure := scoreboard.ParseWeekStart(board, value, probeNow)
		if failure == nil {
			t.Errorf("ParseWeekStart(%q) accepted as %s", value, parsed)
			continue
		}
		codes[value] = failure.Code
		if failure.Code != problem.BadRequest && failure.Code != problem.WeekOutOfRange {
			t.Errorf("ParseWeekStart(%q) code %q, want badRequest or weekOutOfRange", value, failure.Code)
		}
	}
	// PROBE: the contract says badRequest for a malformed parameter and weekOutOfRange for a date off the
	// start day, too old or too far ahead; which one a value like "2026-02-30" gets is recorded here.
	t.Logf("codes for malformed week starts: %v", codes)
}

// TestParseWeekStartWithUnknownStartDay records what a board whose start day is not a weekday accepts.
func TestParseWeekStartWithUnknownStartDay(t *testing.T) {
	// PROBE: the contract validates weekStartDay as an enum elsewhere; here a board stored with "Monday",
	// "mon" or "" is observed to read as Sunday rather than refusing every week. Recorded, not asserted.
	for _, day := range []string{"Monday", "mon", ""} {
		board := boardStarting(t, day, 52)
		var accepted []string
		for offset := 0; offset < 7; offset++ {
			value := time.Date(2026, 10, 19+offset, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
			if _, failure := scoreboard.ParseWeekStart(board, value, probeNow); failure == nil {
				accepted = append(accepted, value)
			}
		}
		t.Logf("board starting %q accepts %v", day, accepted)
	}
}

// FuzzParseWeekStart checks that no value crashes the parser and every accepted one is well formed and in range.
func FuzzParseWeekStart(f *testing.F) {
	for _, seed := range []string{"2026-10-19", "2026-10-26", "2026-1-5", "", "0000-01-01", "+2026-10-19", "2026-02-30", "2026-10-19\n"} {
		f.Add(seed, int64(1761048000), 52, 1)
	}
	f.Fuzz(func(t *testing.T, value string, seconds int64, retention int, dayIndex int) {
		if seconds < -1e12 || seconds > 1e12 {
			return
		}
		if retention < scoreboard.MinimumRetentionWeeks || retention > scoreboard.MaximumRetentionWeeks {
			return
		}
		if dayIndex < 0 {
			dayIndex = -dayIndex
		}
		board := boardStarting(t, weekStartDays[dayIndex%7], retention)
		now := time.Unix(seconds, 0).UTC()
		parsed, failure := scoreboard.ParseWeekStart(board, value, now)
		if failure != nil {
			if failure.Code != problem.BadRequest && failure.Code != problem.WeekOutOfRange {
				t.Errorf("ParseWeekStart(%q) code %q", value, failure.Code)
			}
			return
		}
		if !weekStartPattern.MatchString(value) {
			t.Errorf("ParseWeekStart(%q) accepted a value that is not YYYY-MM-DD", value)
		}
		if parsed.UTC().Format("2006-01-02") != value {
			t.Errorf("ParseWeekStart(%q) = %s, does not round-trip", value, parsed)
		}
		current := scoreboard.CurrentWeekStart(board, now)
		oldest := scoreboard.OldestKeptWeek(board, now)
		if parsed.Before(oldest) || parsed.After(current.Add(oneWeek)) {
			t.Errorf("ParseWeekStart(%q) = %s outside [%s, %s]", value, parsed, oldest, current.Add(oneWeek))
		}
		if parsed.UTC().Weekday().String() != capitalise(board.WeekStartDay) {
			t.Errorf("ParseWeekStart(%q) = %s is not on %s", value, parsed, board.WeekStartDay)
		}
	})
}
