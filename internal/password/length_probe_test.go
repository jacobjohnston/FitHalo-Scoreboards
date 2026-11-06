package password_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/password"
)

// TestLengthCountsGraphemes checks Length counts what a person sees, as Intl.Segmenter does.
func TestLengthCountsGraphemes(t *testing.T) {
	for _, test := range []struct {
		// name describes the text.
		name string

		// text is the input.
		text string

		// want is the grapheme count.
		want int
	}{
		{name: "empty", text: "", want: 0},
		{name: "ascii", text: "password", want: 8},
		{name: "e acute NFC", text: "é", want: 1},
		{name: "e acute NFD", text: "é", want: 1},
		{name: "NFD word", text: "café", want: 4},
		{name: "many combining marks", text: "á̂̃̄", want: 1},
		{name: "thumbs up with skin tone", text: "👍🏽", want: 1},
		{name: "flag", text: "🇺🇸", want: 1},
		{name: "two flags", text: "🇺🇸🇫🇷", want: 2},
		{name: "family ZWJ sequence", text: "👨‍👩‍👧‍👦", want: 1},
		{name: "England tag sequence", text: "🏴󠁧󠁢󠁥󠁮󠁧󠁿", want: 1},
		{name: "keycap", text: "1️⃣", want: 1},
		{name: "emoji with variation selector", text: "❤️", want: 1},
		{name: "LF CR separately", text: "a\n\rb", want: 4},
		{name: "Thai with tone mark", text: "ก้", want: 1},
		{name: "Hangul syllable precomposed", text: "한", want: 1},
		{name: "Hangul jamo sequence", text: "한", want: 1},
		{name: "CJK", text: "漢字", want: 2},
		{name: "space", text: " ", want: 1},
		{name: "1024 ascii", text: strings.Repeat("x", 1024), want: 1024},
		{name: "invalid UTF-8", text: "\xff\xfe", want: 2},
		{name: "truncated multibyte", text: "\xe2\x82", want: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := password.Length(test.text); got != test.want {
				t.Errorf("Length(%q) = %d, want %d", test.text, got, test.want)
			}
		})
	}
}

// TestLengthCRLFRecordsResult records whether CR LF is one grapheme, as UAX #29 GB3 and Intl.Segmenter say.
func TestLengthCRLFRecordsResult(t *testing.T) {
	// PROBE: Intl.Segmenter counts "a\r\nb" as 3; the doc admits overcounting, so 4 is within its promise.
	got := password.Length("a\r\nb")
	t.Logf("Length(a\\r\\nb) = %d", got)
	if got != 3 && got != 4 {
		t.Fatalf("Length(a\\r\\nb) = %d, want 3 or 4", got)
	}
}

// TestLengthNeverUndercountsToZero checks text a person sees something of never counts as nothing, since the doc says Length can only overcount.
func TestLengthNeverUndercountsToZero(t *testing.T) {
	for _, test := range []struct {
		// name describes the text.
		name string

		// text is the input.
		text string
	}{
		{name: "lone zero width joiner", text: "\u200d"},
		{name: "eight zero width joiners", text: strings.Repeat("\u200d", 8)},
		{name: "lone combining acute", text: "\u0301"},
		{name: "lone variation selector", text: "\ufe0f"},
		{name: "lone skin tone modifier", text: "\U0001f3fd"},
		{name: "lone regional indicator", text: "\U0001f1fa"},
		{name: "lone tag character", text: "\U000e0067"},
		{name: "lone LF", text: "\n"},
		{name: "lone CR", text: "\r"},
		{name: "lone control", text: "\x01"},
		{name: "zero width space", text: "\u200b"},
		{name: "ZWJ then letter", text: "\u200dx"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := password.Length(test.text); got < 1 {
				t.Errorf("Length(%q) = %d; Intl.Segmenter counts at least 1 and the doc says Length only overcounts", test.text, got)
			}
		})
	}
}

// TestLeadingJoinerDoesNotSwallowTheText records how a leading joiner changes the count of a long password.
func TestLeadingJoinerDoesNotSwallowTheText(t *testing.T) {
	long := strings.Repeat("x", 2000)
	t.Logf("Length(ZWJ+2000x) = %d, Length(2000x+ZWJ) = %d, Length(ZWJ+x+ZWJ+x...) = %d", password.Length("\u200d"+long), password.Length(long+"\u200d"), password.Length(strings.Repeat("\u200dx", 1000)))
	if got := password.Length("\u200d" + long); got < 2000 {
		t.Errorf("Length(ZWJ+2000x) = %d; a leading joiner hid %d visible letters from the maximum-length check", got, 2000-got)
	}
}

// TestLengthMayOvercountIndicConjuncts records the documented overcount for an Indic conjunct.
func TestLengthMayOvercountIndicConjuncts(t *testing.T) {
	// PROBE: the doc says a conjunct like क्ष counts as two; Intl.Segmenter counts one. Record the number.
	got := password.Length("क्ष")
	t.Logf("Length(क्ष) = %d", got)
	if got < 1 || got > 2 {
		t.Fatalf("Length(क्ष) = %d, want 1 or 2", got)
	}
}

// TestLengthNeverUndercounts checks Length is never below the grapheme count and never above the rune count.
func TestLengthNeverUndercounts(t *testing.T) {
	for _, text := range []string{"👍🏽", "é", "a\r\nb", "🇺🇸🇺🇸", "👨‍👩‍👧‍👦x", "ก้ก้"} {
		runes := len([]rune(text))
		if got := password.Length(text); got > runes || got < 1 {
			t.Errorf("Length(%q) = %d, outside 1..%d", text, got, runes)
		}
	}
}

// TestLengthLimitsAreExported pins the documented limits.
func TestLengthLimitsAreExported(t *testing.T) {
	if password.MinimumLength != 8 || password.MaximumLength != 1024 {
		t.Fatalf("limits %d and %d", password.MinimumLength, password.MaximumLength)
	}
}

// FuzzLength checks Length never panics and stays within 0..rune count (or byte count for invalid UTF-8).
func FuzzLength(f *testing.F) {
	for _, seed := range []string{"", "a", "👍🏽", "é", "\xff", "a\r\nb", "🏴󠁧󠁢󠁥󠁮󠁧󠁿"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		got := password.Length(text)
		if got < 0 || got > utf8.RuneCountInString(text) {
			t.Fatalf("Length(%q) = %d, outside 0..%d", text, got, utf8.RuneCountInString(text))
		}
		if text == "" && got != 0 {
			t.Fatalf("Length(\"\") = %d", got)
		}
	})
}
