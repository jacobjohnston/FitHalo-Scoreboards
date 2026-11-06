package web_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/web"
)

// languageCase is one Accept-Language header and the language expected for it.
type languageCase struct {
	// name names the case.
	name string

	// header is the Accept-Language value.
	header string

	// want is the language expected.
	want string
}

// TestMatchLanguageTable maps regions to variants and falls back to English.
func TestMatchLanguageTable(t *testing.T) {
	cases := []languageCase{
		{"empty", "", "en"},
		{"german", "de", "de"},
		{"austrian german", "de-AT", "de"},
		{"swiss german", "de-CH", "de"},
		{"mexican spanish", "es-MX", "es-419"},
		{"argentine spanish", "es-AR", "es-419"},
		{"us spanish", "es-US", "es-419"},
		{"latin american spanish", "es-419", "es-419"},
		{"spain spanish", "es-ES", "es"},
		{"plain spanish", "es", "es"},
		{"plain portuguese", "pt", "pt-BR"},
		{"brazilian portuguese", "pt-BR", "pt-BR"},
		{"plain chinese", "zh", "zh-Hans"},
		{"mainland chinese", "zh-CN", "zh-Hans"},
		{"simplified chinese with region", "zh-Hans-CN", "zh-Hans"},
		{"singapore chinese", "zh-SG", "zh-Hans"},
		{"canadian french", "fr-CA", "fr"},
		{"japanese", "ja-JP", "ja"},
		{"korean", "ko-KR", "ko"},
		{"italian", "it-IT", "it"},
		{"uppercase", "EN-us", "en"},
		{"uppercase german", "DE", "de"},
		{"q values pick the higher", "fr;q=0.3, de;q=0.8", "de"},
		{"q zero excludes", "de;q=0, en", "en"},
		{"q zero excludes only one", "de;q=0, fr;q=0.5", "fr"},
		{"wildcard", "*", "en"},
		{"unknown then known", "xx, de", "de"},
		{"unknown only", "xx", "en"},
		{"german with variant", "de-DE-1996", "de"},
		{"grandfathered tag", "i-klingon", "en"},
		{"grandfathered then german", "i-klingon, de", "de"},
		{"spaces around", "  de  ", "de"},
		{"three way tie goes first", "fr, de, it", "fr"},
		{"tie with q", "fr;q=0.5, de;q=0.5", "fr"},
		{"higher later", "fr;q=0.5, de;q=0.9", "de"},
		{"region variant beats base later", "es-MX, es;q=0.9", "es-419"},
		{"english region", "en-GB", "en"},
		{"english with variant q", "en-US,en;q=0.9,de;q=0.8", "en"},
		{"lowercase script", "zh-hans", "zh-Hans"},
		{"mixed script casing", "ZH-HANS-cn", "zh-Hans"},
		{"portuguese portugal", "pt-PT", "pt-BR"},
		{"french then unknown", "fr, zz", "fr"},
	}
	for _, c := range cases {
		if got := web.MatchLanguage(c.header); got != c.want {
			t.Errorf("%s: MatchLanguage(%q) = %q, want %q", c.name, c.header, got, c.want)
		}
	}
}

// TestMatchLanguageTraditionalChinese records what Traditional Chinese preferences map to.
func TestMatchLanguageTraditionalChinese(t *testing.T) {
	// PROBE: zh-TW and zh-Hant are not among the ten; falling to zh-Hans or to English are both defensible.
	for _, header := range []string{"zh-TW", "zh-Hant", "zh-Hant-HK", "zh-HK", "zh-TW, en;q=0.5", "zh-TW, zh-CN;q=0.5"} {
		t.Logf("MatchLanguage(%q) = %q", header, web.MatchLanguage(header))
	}
}

// TestMatchLanguageMalformedNeverPanics returns a known language for garbage headers.
func TestMatchLanguageMalformedNeverPanics(t *testing.T) {
	garbage := []string{"de;q=abc", ";;;", " , ,de", ",", ";", "de;", "de;q", "de;q=", "de;q=1.5", "de;q=-1", "de;;q=0.5", "=", "q=0.5", "de-", "-de", "de--AT", "de;q=0.5;q=0.9",
		strings.Repeat("de,", 5000), strings.Repeat("x", 10*1024), strings.Repeat("de-", 300) + "x", "de\x00", "de\r\nX: y", "ünïcödé", "de;q=0.00000000000001", "de;q=.5", "de;Q=0.5", "de ; q = 0.5", "*;q=0", "*;q=0, de", "de, *;q=0"}
	for _, header := range garbage {
		got := web.MatchLanguage(header)
		if !slices.Contains(web.Languages, got) {
			t.Errorf("MatchLanguage(%.40q) = %q, not one of the ten", header, got)
		}
	}
	for header, want := range map[string]string{" , ,de": "de", "de;": "de", "de ; q = 0.5": "de", "*;q=0, de": "de", "de;q=.5": "de"} {
		if got := web.MatchLanguage(header); got != want {
			t.Logf("PROBE: MatchLanguage(%q) = %q (a lenient parser would say %q)", header, got, want)
		}
	}
}

// TestMatchLanguageEveryLanguageMatchesItself returns each of the ten for its own tag.
func TestMatchLanguageEveryLanguageMatchesItself(t *testing.T) {
	for _, language := range web.Languages {
		if got := web.MatchLanguage(language); got != language {
			t.Errorf("MatchLanguage(%q) = %q", language, got)
		}
		if got := web.MatchLanguage(strings.ToUpper(language)); got != language {
			t.Errorf("MatchLanguage(%q) = %q", strings.ToUpper(language), got)
		}
		if got := web.MatchLanguage("xx;q=1, " + language + ";q=0.1"); got != language {
			t.Errorf("MatchLanguage(%q) = %q", "xx;q=1, "+language+";q=0.1", got)
		}
	}
}

// FuzzMatchLanguage checks MatchLanguage always returns one of the ten and never panics.
func FuzzMatchLanguage(f *testing.F) {
	for _, seed := range []string{"", "de", "de-AT,de;q=0.9", "es-MX", "zh-Hans-CN", "fr;q=0.3, de;q=0.8", ";;;", "de;q=abc", "*", "i-klingon", "zh-TW,ja;q=0.5"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, header string) {
		got := web.MatchLanguage(header)
		if !slices.Contains(web.Languages, got) {
			t.Fatalf("MatchLanguage(%q) = %q", header, got)
		}
	})
}

// englishStrings is the English file every localizer probe starts from.
var englishStrings = map[string]string{
	"Greeting": "Hello {name}",
	"Count":    "{count} members of {name}",
	"Plain":    "Plain text",
	"Braces":   "Keep {this} and {that}",
	"Literal":  "100% sure",
}

// translate returns a copy of englishStrings with every value prefixed by language.
func translate(language string) map[string]string {
	result := map[string]string{}
	for key, value := range englishStrings {
		result[key] = language + ": " + value
	}
	return result
}

// stringsFS builds an fs.FS with strings/<language>.json for every language, overriding some.
func stringsFS(t *testing.T, overrides map[string]any) fstest.MapFS {
	t.Helper()
	files := fstest.MapFS{}
	for _, language := range web.Languages {
		var content any = translate(language)
		if language == "en" {
			content = englishStrings
		}
		if override, ok := overrides[language]; ok {
			if override == nil {
				continue
			}
			content = override
		}
		var data []byte
		if raw, ok := content.([]byte); ok {
			data = raw
		} else {
			data, _ = json.Marshal(content)
		}
		files["strings/"+language+".json"] = &fstest.MapFile{Data: data}
	}
	return files
}

// withoutKey returns strings for language without key.
func withoutKey(language, key string) map[string]string {
	result := translate(language)
	delete(result, key)
	return result
}

// withKey returns strings for language with an extra key.
func withKey(language, key, value string) map[string]string {
	result := translate(language)
	result[key] = value
	return result
}

// TestLoadLocalizerHappyPath loads a complete set and fills placeholders.
func TestLoadLocalizerHappyPath(t *testing.T) {
	localizer, err := web.LoadLocalizer(stringsFS(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := localizer.Text("en", "Greeting", "name", "Avery"); got != "Hello Avery" {
		t.Errorf("Text(en, Greeting) = %q", got)
	}
	if got := localizer.Text("de", "Greeting", "name", "Avery"); got != "de: Hello Avery" {
		t.Errorf("Text(de, Greeting) = %q", got)
	}
	if got := localizer.Text("en", "Count", "count", 3, "name", "Morning Crew"); got != "3 members of Morning Crew" {
		t.Errorf("Text(en, Count) = %q", got)
	}
	if got := localizer.Text("en", "Plain"); got != "Plain text" {
		t.Errorf("Text(en, Plain) = %q", got)
	}
	if got := localizer.Text("en", "Literal"); got != "100% sure" {
		t.Errorf("Text(en, Literal) = %q", got)
	}
	keys := localizer.Keys()
	if !slices.IsSorted(keys) || len(keys) != len(englishStrings) {
		t.Errorf("Keys() = %v", keys)
	}
}

// TestLoadLocalizerRefusesMismatchedKeys errors when a language lacks or adds a key.
func TestLoadLocalizerRefusesMismatchedKeys(t *testing.T) {
	if _, err := web.LoadLocalizer(stringsFS(t, map[string]any{"de": withoutKey("de", "Plain")})); err == nil {
		t.Error("a language missing a key was accepted")
	}
	if _, err := web.LoadLocalizer(stringsFS(t, map[string]any{"ja": withKey("ja", "Extra", "x")})); err == nil {
		t.Error("a language with an extra key was accepted")
	}
	if _, err := web.LoadLocalizer(stringsFS(t, map[string]any{"fr": []byte(`{"Greeting": "x",`)})); err == nil {
		t.Error("invalid JSON was accepted")
	}
	if _, err := web.LoadLocalizer(stringsFS(t, map[string]any{"ko": nil})); err == nil {
		t.Error("a missing language file was accepted")
	}
	if _, err := web.LoadLocalizer(stringsFS(t, map[string]any{"en": nil})); err == nil {
		t.Error("a missing English file was accepted")
	}
	if _, err := web.LoadLocalizer(stringsFS(t, map[string]any{"it": []byte(`["a","b"]`)})); err == nil {
		t.Error("a JSON array was accepted")
	}
	if _, err := web.LoadLocalizer(stringsFS(t, map[string]any{"es": map[string]any{"Greeting": 1, "Count": "a", "Plain": "b", "Braces": "c", "Literal": "d"}})); err == nil {
		t.Error("a non-string value was accepted")
	}
	if _, err := web.LoadLocalizer(fstest.MapFS{}); err == nil {
		t.Error("an empty filesystem was accepted")
	}
}

// TestLoadLocalizerPlaceholderMismatch records whether a translation's placeholders are checked.
func TestLoadLocalizerPlaceholderMismatch(t *testing.T) {
	// PROBE: de's Greeting names {nam} and adds {extra}; a loader that compared placeholders would refuse.
	strings := translate("de")
	strings["Greeting"] = "Hallo {nam} {extra}"
	strings["Count"] = "Keine Platzhalter"
	localizer, err := web.LoadLocalizer(stringsFS(t, map[string]any{"de": strings}))
	if err != nil {
		t.Logf("a placeholder mismatch is refused: %v", err)
		return
	}
	t.Logf("a placeholder mismatch is accepted; Text(de, Greeting, name) = %q", localizer.Text("de", "Greeting", "name", "Avery"))
}

// TestLoadLocalizerDuplicateKey records how a duplicated JSON key is treated.
func TestLoadLocalizerDuplicateKey(t *testing.T) {
	// PROBE: encoding/json keeps the last duplicate; a strict loader might refuse.
	data := []byte(`{"Greeting":"first","Greeting":"second","Count":"c","Plain":"p","Braces":"b","Literal":"l"}`)
	localizer, err := web.LoadLocalizer(stringsFS(t, map[string]any{"it": data}))
	if err != nil {
		t.Logf("a duplicate key is refused: %v", err)
		return
	}
	t.Logf("a duplicate key is accepted; Text(it, Greeting) = %q", localizer.Text("it", "Greeting"))
}

// TestTextEdgeCases covers unknown placeholders, missing keys, unknown languages and odd arguments.
func TestTextEdgeCases(t *testing.T) {
	localizer, err := web.LoadLocalizer(stringsFS(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	// PROBE: the doc does not say what an unfilled placeholder, a missing key or an unknown language give.
	t.Logf("unfilled placeholder: %q", localizer.Text("en", "Greeting"))
	t.Logf("unknown placeholder name: %q", localizer.Text("en", "Greeting", "nope", "x"))
	t.Logf("missing key: %q", localizer.Text("en", "Missing_Key"))
	t.Logf("odd argument count: %q", localizer.Text("en", "Greeting", "name"))
	t.Logf("non-string argument name: %q", localizer.Text("en", "Greeting", 1, "x"))
	t.Logf("nil argument: %q", localizer.Text("en", "Greeting", "name", nil))
	t.Logf("value containing braces: %q", localizer.Text("en", "Greeting", "name", "{count}"))
	t.Logf("value containing dollar: %q", localizer.Text("en", "Greeting", "name", "$1 ${name}"))
	if got := localizer.Text("xx", "Plain"); got != "Plain text" {
		t.Errorf("an unknown language gave %q, want English", got)
	}
	if got := localizer.Text("", "Plain"); got != "Plain text" {
		t.Errorf("an empty language gave %q, want English", got)
	}
	if got := localizer.Text("en", "Missing_Key"); got == "" {
		t.Error("a missing key gives an empty string, which hides the mistake on the page")
	}
	if got := localizer.Text("en", "Greeting", "name", "{count}"); got != "Hello {count}" {
		t.Errorf("a value containing braces was re-expanded: %q", got)
	}
	if got := localizer.Text("en", "Greeting", "name", "$1 ${name}"); got != "Hello $1 ${name}" {
		t.Errorf("a value containing dollar signs was expanded: %q", got)
	}
	if got := localizer.Text("en", "Braces", "this", "A"); !strings.Contains(got, "A") {
		t.Errorf("a partially filled string lost its argument: %q", got)
	}
}

// TestTextDoesNotEscapeHTML returns raw text, leaving escaping to the templates.
func TestTextDoesNotEscapeHTML(t *testing.T) {
	// PROBE: html/template escapes at render time; Text escaping too would double-escape.
	localizer, err := web.LoadLocalizer(stringsFS(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Text with an HTML argument: %q", localizer.Text("en", "Greeting", "name", "<b>&</b>"))
}
