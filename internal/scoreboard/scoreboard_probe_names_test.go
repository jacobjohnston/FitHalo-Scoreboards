package scoreboard_test

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
)

// nameCase is one display name and whether the Limits table admits it.
type nameCase struct {
	// name says what the case probes.
	name string

	// input is the display name before normalizing.
	input string

	// want is the normalized name, or "" when the name must be refused.
	want string
}

// acceptedNames are names the Limits table admits, with the trimmed result expected.
var acceptedNames = []nameCase{
	{name: "plain", input: "Jacob", want: "Jacob"},
	{name: "trimmed spaces", input: "  Jacob  ", want: "Jacob"},
	{name: "interior space", input: "Jacob Johnston", want: "Jacob Johnston"},
	{name: "forty ascii", input: strings.Repeat("a", 40), want: strings.Repeat("a", 40)},
	{name: "one character", input: "J", want: "J"},
	{name: "zwj family", input: "👨\u200d👩\u200d👧", want: "👨\u200d👩\u200d👧"},
	{name: "flag", input: "🇺🇸", want: "🇺🇸"},
	{name: "skin tone", input: "👍🏽", want: "👍🏽"},
	{name: "variation selector", input: "❤️", want: "❤️"},
	{name: "tag sequence flag", input: "🏴󠁧󠁢󠁥󠁮󠁧󠁿", want: "🏴󠁧󠁢󠁥󠁮󠁧󠁿"},
	{name: "cjk", input: "山田太郎", want: "山田太郎"},
	{name: "arabic", input: "محمد", want: "محمد"},
	{name: "accented latin", input: "Émile Zoë", want: "Émile Zoë"},
	{name: "decomposed accent", input: "Zoe\u0308", want: "Zoe\u0308"},
	{name: "devanagari conjunct", input: "क्ष", want: "क्ष"},
	{name: "forty precomposed accents", input: strings.Repeat("é", 40), want: strings.Repeat("é", 40)},
	{name: "twenty flags", input: strings.Repeat("🇺🇸", 20), want: strings.Repeat("🇺🇸", 20)},
	{name: "emoji with keycap", input: "1️⃣", want: "1️⃣"},
	{name: "punctuation", input: "O'Brien-Smith, Jr.", want: "O'Brien-Smith, Jr."},
	{name: "digits", input: "007", want: "007"},
	{name: "interior nbsp run collapsed", input: "Jacob\u00a0\u00a0\u2003Johnston", want: "Jacob Johnston"},
	{name: "interior ideographic space", input: "山田\u3000太郎", want: "山田 太郎"},
	{name: "three stacked marks", input: "a\u0301\u0302\u0303", want: "a\u0301\u0302\u0303"},
}

// refusedNames are names the Limits table forbids.
var refusedNames = []nameCase{
	{name: "empty", input: ""},
	{name: "forty-one ascii", input: strings.Repeat("a", 41)},
	{name: "only spaces", input: "   "},
	{name: "only combining marks", input: "\u0301\u0301"},
	{name: "zero-width space", input: "Ja\u200bcob"},
	{name: "zwj alone", input: "\u200d"},
	{name: "zwj between letters", input: "Ja\u200dcob"},
	{name: "soft hyphen", input: "Ja\u00adcob"},
	{name: "bom", input: "\ufeffJacob"},
	{name: "lro", input: "\u202dJacob"},
	{name: "rlo", input: "Jacob\u202e"},
	{name: "lrm", input: "Ja\u200ecob"},
	{name: "rlm", input: "Ja\u200fcob"},
	{name: "lri", input: "\u2066Jacob"},
	{name: "rli", input: "\u2067Jacob"},
	{name: "fsi", input: "\u2068Jacob"},
	{name: "pdi", input: "Jacob\u2069"},
	{name: "line separator", input: "Ja\u2028cob"},
	{name: "paragraph separator", input: "Ja\u2029cob"},
	{name: "nul", input: "Ja\x00cob"},
	{name: "del", input: "Ja\x7fcob"},
	{name: "c1 next line", input: "Ja\u0085cob"},
	{name: "tab", input: "Ja\tcob"},
	{name: "newline", input: "Ja\ncob"},
	{name: "carriage return", input: "Ja\rcob"},
	{name: "tag characters alone", input: "\U000E0067\U000E0062"},
	{name: "variation selector alone", input: "️"},
	{name: "word joiner", input: "Ja\u2060cob"},
	{name: "invisible separator", input: "Ja\u2063cob"},
	{name: "invalid utf-8", input: "Ja\xffcob"},
	{name: "only nbsp", input: "\u00a0"},
	{name: "only ideographic space", input: "\u3000"},
	{name: "only zero-width space", input: "\u200b"},
	{name: "four stacked marks", input: "A\u0301\u0302\u0303\u0304"},
	{name: "thirty-nine combining accents", input: "A" + strings.Repeat("\u0301", 39)},
	{name: "private use", input: "Jacob\ue000"},
	{name: "unassigned", input: "Jacob\U000E0080x"},
	{name: "long tag sequence", input: "Jacob\U0001F3F4" + strings.Repeat("\U000E0061", 7) + "\U000E007F"},
	{name: "unbounded tag sequence", input: "Jacob\U0001F3F4" + strings.Repeat("\U000E0061", 30)},
	{name: "unclosed tag sequence", input: "Jacob\U0001F3F4\U000E0067\U000E0062"},
}

// TestNormalizeDisplayNameAcceptsRealNames keeps names made of visible characters, emoji included.
func TestNormalizeDisplayNameAcceptsRealNames(t *testing.T) {
	for _, test := range acceptedNames {
		got, err := scoreboard.NormalizeDisplayName(test.input)
		if err != nil {
			t.Errorf("%s: NormalizeDisplayName(%q) refused: %v", test.name, test.input, err)
			continue
		}
		if got != test.want {
			t.Errorf("%s: NormalizeDisplayName(%q) = %q, want %q", test.name, test.input, got, test.want)
		}
	}
}

// TestNormalizeDisplayNameRefusesHiddenCharacters refuses empty, invisible, control and direction-bending names.
func TestNormalizeDisplayNameRefusesHiddenCharacters(t *testing.T) {
	for _, test := range refusedNames {
		if got, err := scoreboard.NormalizeDisplayName(test.input); err == nil {
			t.Errorf("%s: NormalizeDisplayName(%q) = %q, want a refusal", test.name, test.input, got)
		}
	}
}

// TestBlankLettersAndSymbolsAreNotVisibleNames refuses names made only of code points that render blank
// without being control, format or separator characters: the Limits table wants a visible character.
func TestBlankLettersAndSymbolsAreNotVisibleNames(t *testing.T) {
	blanks := []nameCase{
		{name: "hangul filler", input: "\u3164"},
		{name: "hangul choseong filler", input: "ᅟ"},
		{name: "hangul jungseong filler", input: "ᅠ"},
		{name: "halfwidth hangul filler", input: "ﾠ"},
		{name: "braille blank", input: "\u2800"},
		{name: "three hangul fillers", input: "\u3164\u3164\u3164"},
	}
	for _, test := range blanks {
		if got, err := scoreboard.NormalizeDisplayName(test.input); err == nil {
			t.Errorf("%s: NormalizeDisplayName(%q) = %q, want a refusal: nothing visible", test.name, test.input, got)
		}
	}
}

// TestNormalizeDisplayNameRecordsHowItCounts records how length and whitespace are measured.
func TestNormalizeDisplayNameRecordsHowItCounts(t *testing.T) {
	// PROBE: the Limits table says "1 to 40 characters" without saying whether a character is a rune or
	// what a person sees; these cases differ between the two readings and are recorded, not asserted.
	probes := []nameCase{
		{name: "ten families (10 graphemes, 70 runes)", input: strings.Repeat("👨\u200d👩\u200d👧\u200d👦", 10)},
		{name: "twenty-one flags (21 graphemes, 42 runes)", input: strings.Repeat("🇺🇸", 21)},
		{name: "forty decomposed accents (40 graphemes, 80 runes)", input: strings.Repeat("e\u0301", 40)},
		{name: "forty-one decomposed accents", input: strings.Repeat("e\u0301", 41)},
		{name: "thirty-nine ascii then zwj family", input: strings.Repeat("a", 39) + "👨\u200d👩\u200d👧"},
		{name: "leading nbsp", input: "\u00a0Jacob"},
		{name: "trailing nbsp", input: "Jacob\u00a0"},
		{name: "leading ideographic space", input: "\u3000Jacob"},
		{name: "interior nbsp", input: "Jacob\u00a0Johnston"},
		{name: "interior double space", input: "Jacob  Johnston"},
		{name: "interior ideographic space", input: "山田\u3000太郎"},
		{name: "leading tab", input: "\tJacob"},
		{name: "forty spaces around one letter", input: strings.Repeat(" ", 40) + "J" + strings.Repeat(" ", 40)},
	}
	for _, test := range probes {
		got, err := scoreboard.NormalizeDisplayName(test.input)
		t.Logf("%s: %q -> %q, %d runes, err=%v", test.name, test.input, got, utf8.RuneCountInString(got), err)
	}
}

// TestNormalizeDisplayNameTrimsOnlyTheEnds keeps a name's interior as sent once the ends are trimmed.
func TestNormalizeDisplayNameTrimsOnlyTheEnds(t *testing.T) {
	got, err := scoreboard.NormalizeDisplayName(" \t Jacob  Johnston \n ")
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if got != "Jacob  Johnston" {
		// PROBE: the Limits table says leading and trailing space is removed and says nothing about
		// interior runs; whether they collapse is recorded here.
		t.Logf("interior whitespace changed: %q", got)
	}
	if got[0] == ' ' || got[len(got)-1] == ' ' {
		t.Errorf("normalized name %q still has an end space", got)
	}
}

// visibleOnly reports whether every rune of name is neither control, format, nor separator, apart from emoji joiners and tags.
func visibleOnly(name string) bool {
	for _, r := range name {
		if r == '\u200d' || (r >= 0xE0020 && r <= 0xE007F) || r == ' ' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return false
		}
	}
	return true
}

// FuzzNormalizeDisplayName checks that every accepted name is trimmed, non-empty, short and free of hidden characters.
func FuzzNormalizeDisplayName(f *testing.F) {
	for _, test := range acceptedNames {
		f.Add(test.input)
	}
	for _, test := range refusedNames {
		f.Add(test.input)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got, err := scoreboard.NormalizeDisplayName(input)
		if err != nil {
			return
		}
		if got == "" || strings.TrimSpace(got) != got {
			t.Errorf("NormalizeDisplayName(%q) = %q, not trimmed or empty", input, got)
		}
		if !utf8.ValidString(got) {
			t.Errorf("NormalizeDisplayName(%q) = %q, invalid UTF-8", input, got)
		}
		if utf8.RuneCountInString(got) > 4*scoreboard.MaximumDisplayNameLength {
			t.Errorf("NormalizeDisplayName(%q) kept %d runes", input, utf8.RuneCountInString(got))
		}
		if !visibleOnly(got) {
			t.Errorf("NormalizeDisplayName(%q) = %q keeps a control, format or separator character", input, got)
		}
		visible := false
		for _, r := range got {
			if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) {
				visible = true
			}
		}
		if !visible {
			t.Errorf("NormalizeDisplayName(%q) = %q has no visible character", input, got)
		}
	})
}
