package sharing_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
	"github.com/jacobjohnston/FitHalo-Scoreboards/spec"
)

// probeSpec is spec/sharing.json decoded independently of the package, so expectations do not come from it.
type probeSpec struct {
	// Version is the file's format version.
	Version int `json:"version"`

	// Levels is each ladder's names from most private to least.
	Levels map[string][]string `json:"levels"`

	// Fields is every field and what it needs.
	Fields []probeField `json:"fields"`
}

// probeField is one entry of the file's fields.
type probeField struct {
	// Path is the field's path in the allowlist notation.
	Path string `json:"path"`

	// Requires maps a ladder to a level name or an option to true.
	Requires map[string]any `json:"requires"`
}

// loadProbeSpec decodes the embedded spec file.
func loadProbeSpec(t *testing.T) probeSpec {
	t.Helper()
	var ps probeSpec
	if err := json.Unmarshal(spec.Sharing, &ps); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	return ps
}

// mustLoad loads the real allowlist.
func mustLoad(t testing.TB) *sharing.Allowlist {
	t.Helper()
	a, err := sharing.LoadAllowlist(spec.Sharing)
	if err != nil {
		t.Fatalf("LoadAllowlist(spec.Sharing): %v", err)
	}
	return a
}

// rank returns the index of name in ladder, or -1 when the ladder does not list it, so unknown fails closed.
func rank(ladder []string, name string) int {
	return slices.Index(ladder, name)
}

// expectAllows computes from the decoded file whether s allows the listed field.
func expectAllows(t *testing.T, ps probeSpec, f probeField, s sharing.Sharing) bool {
	t.Helper()
	for key, v := range f.Requires {
		switch key {
		case "score":
			if rank(ps.Levels["score"], string(s.Score)) < rank(ps.Levels["score"], v.(string)) {
				return false
			}
		case "activity":
			if rank(ps.Levels["activity"], string(s.Activity)) < rank(ps.Levels["activity"], v.(string)) {
				return false
			}
		case "routes":
			if v == true && !s.Routes {
				return false
			}
		case "strengthSets":
			if v == true && !s.StrengthSets {
				return false
			}
		case "strengthWeights":
			if v == true && !s.StrengthWeights {
				return false
			}
		default:
			t.Fatalf("spec requires unknown key %q", key)
		}
	}
	return true
}

// expectedLeaves returns the set of listed paths s allows, from the file alone.
func expectedLeaves(t *testing.T, ps probeSpec, s sharing.Sharing) map[string]bool {
	t.Helper()
	allowed := map[string]bool{}
	for _, f := range ps.Fields {
		if expectAllows(t, ps, f, s) {
			allowed[f.Path] = true
		}
	}
	return allowed
}

// rawGrid returns every combination of known level and option, normalised or not.
func rawGrid() []sharing.Sharing {
	var grid []sharing.Sharing
	for _, score := range []sharing.ScoreLevel{sharing.ScoreProgress, sharing.ScoreScore, sharing.ScoreDetails} {
		for _, activity := range []sharing.ActivityLevel{sharing.ActivityNone, sharing.ActivityPoints, sharing.ActivitySummary, sharing.ActivityFull} {
			for bits := 0; bits < 8; bits++ {
				grid = append(grid, sharing.Sharing{Score: score, Activity: activity, Routes: bits&1 != 0, StrengthSets: bits&2 != 0, StrengthWeights: bits&4 != 0})
			}
		}
	}
	return grid
}

// unknownSharings returns sharings whose level strings the API does not define.
func unknownSharings() []sharing.Sharing {
	var out []sharing.Sharing
	for _, bad := range []string{"", "Details", " details", "details ", "DETAILS", "everything", "all", "details\x00", "détails"} {
		out = append(out, sharing.Sharing{Score: sharing.ScoreLevel(bad), Activity: sharing.ActivityFull, Routes: true, StrengthSets: true, StrengthWeights: true})
		out = append(out, sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityLevel(bad), Routes: true, StrengthSets: true, StrengthWeights: true})
	}
	return out
}

// probeLevels is the levels block every hand-written allowlist shares.
const probeLevels = `"levels":{"score":["progress","score","details"],"activity":["none","points","summary","full"]}`

// allowlistWith builds an allowlist document around the given fields JSON.
func allowlistWith(fields string) []byte {
	return []byte(`{"version":1,"description":"probe",` + probeLevels + `,"fields":[` + fields + `]}`)
}

// loadWant says what a LoadAllowlist case expects.
type loadWant int

const (
	// wantError means the document must be refused.
	wantError loadWant = iota

	// wantOK means the document must load.
	wantOK

	// wantObserve means the contract does not say; the outcome is logged.
	wantObserve
)

// TestLoadAllowlistFailsClosed refuses documents that name a level or option the package does not know.
func TestLoadAllowlistFailsClosed(t *testing.T) {
	cases := []struct {
		// name describes the document.
		name string

		// data is the document.
		data []byte

		// want is the expected outcome.
		want loadWant
	}{
		{name: "real spec", data: spec.Sharing, want: wantOK},
		{name: "invalid JSON", data: []byte(`{`), want: wantError},
		{name: "empty input", data: nil, want: wantError},
		{name: "JSON null", data: []byte(`null`), want: wantObserve},
		{name: "JSON array", data: []byte(`[]`), want: wantError},
		{name: "empty object", data: []byte(`{}`), want: wantObserve},
		{name: "unknown requires key", data: allowlistWith(`{"path":"x","requires":{"heartRate":true}}`), want: wantError},
		{name: "unknown score level name", data: allowlistWith(`{"path":"x","requires":{"score":"everything"}}`), want: wantError},
		{name: "unknown activity level name", data: allowlistWith(`{"path":"x","requires":{"activity":"all"}}`), want: wantError},
		{name: "activity name in score requires", data: allowlistWith(`{"path":"x","requires":{"score":"points"}}`), want: wantError},
		{name: "mixed case level name", data: allowlistWith(`{"path":"x","requires":{"score":"Details"}}`), want: wantError},
		{name: "boolean as string", data: allowlistWith(`{"path":"x","requires":{"routes":"true"}}`), want: wantError},
		{name: "level as boolean", data: allowlistWith(`{"path":"x","requires":{"score":true}}`), want: wantError},
		{name: "level as number", data: allowlistWith(`{"path":"x","requires":{"activity":2}}`), want: wantError},
		{name: "requires as array", data: allowlistWith(`{"path":"x","requires":["score"]}`), want: wantError},
		{name: "path as number", data: allowlistWith(`{"path":1,"requires":{}}`), want: wantError},
		{name: "fields as object", data: []byte(`{"version":1,` + probeLevels + `,"fields":{}}`), want: wantError},
		{name: "levels with an unknown name", data: []byte(`{"version":1,"levels":{"score":["progress","score","details","everything"],"activity":["none","points","summary","full"]},"fields":[]}`), want: wantError},
		{name: "levels with unknown ladder", data: []byte(`{"version":1,"levels":{"score":["progress","score","details"],"activity":["none","points","summary","full"],"heartRate":["low","high"]},"fields":[]}`), want: wantError},
		{name: "levels missing the activity ladder", data: []byte(`{"version":1,"levels":{"score":["progress","score","details"]},"fields":[{"path":"x","requires":{"activity":"points"}}]}`), want: wantError},
		{name: "levels missing a known name", data: []byte(`{"version":1,"levels":{"score":["progress","details"],"activity":["none","points","summary","full"]},"fields":[]}`), want: wantObserve},
		{name: "levels out of order", data: []byte(`{"version":1,"levels":{"score":["details","score","progress"],"activity":["none","points","summary","full"]},"fields":[]}`), want: wantObserve},
		{name: "levels missing entirely", data: []byte(`{"version":1,"fields":[{"path":"x","requires":{"score":"details"}}]}`), want: wantError},
		{name: "explicit routes false", data: allowlistWith(`{"path":"x","requires":{"routes":false}}`), want: wantObserve},
		{name: "duplicate path", data: allowlistWith(`{"path":"x","requires":{}},{"path":"x","requires":{"score":"details"}}`), want: wantObserve},
		{name: "empty path", data: allowlistWith(`{"path":"","requires":{}}`), want: wantObserve},
		{name: "missing path", data: allowlistWith(`{"requires":{}}`), want: wantObserve},
		{name: "missing requires", data: allowlistWith(`{"path":"x"}`), want: wantObserve},
		{name: "bare array element path", data: allowlistWith(`{"path":"activities[]","requires":{}}`), want: wantObserve},
		{name: "double dot path", data: allowlistWith(`{"path":"a..b","requires":{}}`), want: wantObserve},
		{name: "trailing dot path", data: allowlistWith(`{"path":"a.","requires":{}}`), want: wantObserve},
		{name: "leading dot path", data: allowlistWith(`{"path":".a","requires":{}}`), want: wantObserve},
		{name: "brackets at top", data: allowlistWith(`{"path":"[]","requires":{}}`), want: wantObserve},
		{name: "brackets mid segment", data: allowlistWith(`{"path":"a[]b","requires":{}}`), want: wantObserve},
		{name: "version 2", data: []byte(`{"version":2,` + probeLevels + `,"fields":[]}`), want: wantObserve},
		{name: "version missing", data: []byte(`{` + probeLevels + `,"fields":[]}`), want: wantObserve},
		{name: "fields missing", data: []byte(`{"version":1,` + probeLevels + `}`), want: wantObserve},
		{name: "unknown top-level key", data: []byte(`{"version":1,"extra":1,` + probeLevels + `,"fields":[]}`), want: wantObserve},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := sharing.LoadAllowlist(tc.data)
			switch tc.want {
			case wantError:
				if err == nil {
					t.Errorf("loaded %q without error; paths %v", tc.data, a.Paths())
				}
			case wantOK:
				if err != nil {
					t.Errorf("refused: %v", err)
				}
			case wantObserve:
				// PROBE: the contract does not say whether this document is refused.
				if err != nil {
					t.Logf("refused: %v", err)
				} else {
					t.Logf("loaded; paths %v", a.Paths())
				}
			}
		})
	}
}

// TestLoadAllowlistExplicitFalseIsNoRequirement reads routes:false as no requirement rather than an impossible one.
func TestLoadAllowlistExplicitFalseIsNoRequirement(t *testing.T) {
	a, err := sharing.LoadAllowlist(allowlistWith(`{"path":"x","requires":{"routes":false,"strengthSets":false}}`))
	if err != nil {
		t.Skipf("PROBE: explicit false is refused: %v", err)
	}
	if !a.Allows("x", sharing.Lowest) {
		t.Errorf("routes:false counted as a requirement: x not allowed at Lowest")
	}
}

// TestLoadAllowlistDuplicatePathKeepsStricter checks which entry wins when a path is listed twice.
func TestLoadAllowlistDuplicatePathKeepsStricter(t *testing.T) {
	a, err := sharing.LoadAllowlist(allowlistWith(`{"path":"x","requires":{"score":"details"}},{"path":"x","requires":{}}`))
	if err != nil {
		t.Skipf("PROBE: duplicate path refused: %v", err)
	}
	// PROBE: a duplicate was accepted; the laxer second entry must not open the field at Lowest.
	if a.Allows("x", sharing.Lowest) {
		t.Errorf("duplicate path: the laxer entry won; x allowed at Lowest")
	}
	if got := a.Paths(); len(got) != 1 {
		t.Errorf("Paths() = %v, want the duplicate listed once", got)
	}
}

// TestLoadAllowlistHonoursFileOrderOfLevels checks what happens when the file's ladder order differs from the constants.
func TestLoadAllowlistHonoursFileOrderOfLevels(t *testing.T) {
	data := []byte(`{"version":1,"levels":{"score":["details","score","progress"],"activity":["none","points","summary","full"]},"fields":[{"path":"x","requires":{"score":"score"}}]}`)
	a, err := sharing.LoadAllowlist(data)
	if err != nil {
		t.Skipf("PROBE: reordered levels refused: %v", err)
	}
	// PROBE: the file is "the authority" and says levels are "in the order given by levels"; by the file, progress
	// is now the highest and details the lowest. Record which order Allows follows.
	t.Logf("file order details<score<progress: Allows(x, progress)=%v Allows(x, details)=%v",
		a.Allows("x", sharing.Sharing{Score: sharing.ScoreProgress}), a.Allows("x", sharing.Sharing{Score: sharing.ScoreDetails}))
}

// TestSpecFileIsConsistent checks the shipped file against the ladder the docs describe.
func TestSpecFileIsConsistent(t *testing.T) {
	ps := loadProbeSpec(t)
	if ps.Version != 2 {
		t.Errorf("version %d", ps.Version)
	}
	if !slices.Equal(ps.Levels["score"], []string{"progress", "score", "details"}) || !slices.Equal(ps.Levels["activity"], []string{"none", "points", "summary", "full"}) {
		t.Errorf("levels %v do not match the openapi enums", ps.Levels)
	}
	seen := map[string]bool{}
	for _, f := range ps.Fields {
		if f.Path == "" || seen[f.Path] {
			t.Errorf("path %q empty or duplicated", f.Path)
		}
		seen[f.Path] = true
		for _, seg := range strings.Split(f.Path, ".") {
			name := strings.TrimSuffix(seg, "[]")
			if name == "" || strings.ContainsAny(name, "[]. ") {
				t.Errorf("path %q has a malformed segment %q", f.Path, seg)
			}
		}
		req := f.Requires
		if req["activity"] != nil && req["score"] != "details" {
			t.Errorf("%s needs activity without details", f.Path)
		}
		for _, option := range []string{"routes", "strengthSets", "strengthWeights"} {
			if req[option] == true && (req["activity"] == nil || req["activity"] == "none") {
				t.Errorf("%s needs %s without activity", f.Path, option)
			}
		}
		if req["strengthWeights"] == true && req["strengthSets"] != true {
			t.Errorf("%s needs weights without sets", f.Path)
		}
	}
	for _, must := range []string{"progress", "updatedAt", "score", "goal", "tier", "breakdown.exercise", "activities[].hasRoute"} {
		if !seen[must] {
			t.Errorf("spec lacks %s, which docs/specifications.md describes", must)
		}
	}
}

// TestPathsMatchTheFile returns every listed path once, sorted.
func TestPathsMatchTheFile(t *testing.T) {
	ps := loadProbeSpec(t)
	got := mustLoad(t).Paths()
	var want []string
	for _, f := range ps.Fields {
		want = append(want, f.Path)
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("Paths() = %v\nwant %v", got, want)
	}
	if !slices.IsSorted(got) {
		t.Errorf("Paths() not sorted")
	}
	got[0] = "mutated"
	if again := mustLoad(t).Paths(); again[0] == "mutated" {
		t.Errorf("Paths() shares its backing array with the allowlist")
	}
}

// TestAllowsMatchesTheFile compares Allows with an independent reading of the file for every path and sharing.
func TestAllowsMatchesTheFile(t *testing.T) {
	ps := loadProbeSpec(t)
	a := mustLoad(t)
	var sharings []sharing.Sharing
	sharings = append(sharings, sharing.All()...)
	sharings = append(sharings, rawGrid()...)
	sharings = append(sharings, unknownSharings()...)
	for _, f := range ps.Fields {
		for _, s := range sharings {
			if got, want := a.Allows(f.Path, s), expectAllows(t, ps, f, s); got != want {
				t.Errorf("Allows(%s, %+v) = %v, want %v", f.Path, s, got, want)
			}
		}
	}
}

// TestAllowsRefusesUnknownPaths allows nothing the file does not list, even at Highest.
func TestAllowsRefusesUnknownPaths(t *testing.T) {
	a := mustLoad(t)
	for _, path := range []string{
		"", "title", "heartRate", "activities.id", "activities[].title", "activities[].Id", "ACTIVITIES[].id",
		"activities[]id", "activities[0].id", "activities[][].id", "breakdown.exercise.x", "progress.x", "progress[]",
		"score ", " score", "score.", ".score", "breakdown..exercise",
		"activities[].strength.exercises[].sets[].right.weight", "activities[].route", "activities[].strength.exercises[].sets[].weightKilograms[]",
	} {
		if a.Allows(path, sharing.Highest) {
			t.Errorf("Allows(%q, Highest) = true", path)
		}
	}
}

// TestAllowsParentPaths allows an object or array when anything beneath it is allowed.
func TestAllowsParentPaths(t *testing.T) {
	a := mustLoad(t)
	details := sharing.Sharing{Score: sharing.ScoreDetails}
	score := sharing.Sharing{Score: sharing.ScoreScore}
	points := sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints}
	summary := sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivitySummary}
	full := sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityFull}
	sets := sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, StrengthSets: true}
	cases := []struct {
		// name describes the case.
		name string

		// path is the parent path.
		path string

		// sharing is the sharing asked.
		sharing sharing.Sharing

		// want is the expected answer.
		want bool
	}{
		{name: "breakdown at details", path: "breakdown", sharing: details, want: true},
		{name: "breakdown at score", path: "breakdown", sharing: score, want: false},
		{name: "activities[] at points", path: "activities[]", sharing: points, want: true},
		{name: "activities[] at details none", path: "activities[]", sharing: details, want: false},
		{name: "activities[].points at points", path: "activities[].points", sharing: points, want: true},
		{name: "activities[].points at details none", path: "activities[].points", sharing: details, want: false},
		{name: "activities[].details at full", path: "activities[].details", sharing: full, want: true},
		{name: "activities[].details at summary", path: "activities[].details", sharing: summary, want: false},
		{name: "activities[].details.weather at full", path: "activities[].details.weather", sharing: full, want: true},
		{name: "activities[].strength at sets", path: "activities[].strength", sharing: sets, want: true},
		{name: "activities[].strength at points", path: "activities[].strength", sharing: points, want: false},
		{name: "activities[].strength.exercises[].sets[].right at sets", path: "activities[].strength.exercises[].sets[].right", sharing: sets, want: true},
		{name: "activities[].strength.exercises[].sets[].clusters[] at sets", path: "activities[].strength.exercises[].sets[].clusters[]", sharing: sets, want: true},
		{name: "activities[].strength.exercises[].sets[].clusters[] at points", path: "activities[].strength.exercises[].sets[].clusters[]", sharing: points, want: false},
		{name: "unknown prefix", path: "breakdow", sharing: sharing.Highest, want: false},
		{name: "prefix without bracket of an array element", path: "activities[].strength.exercises", sharing: sets, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.Allows(tc.path, tc.sharing); got != tc.want {
				t.Errorf("Allows(%s, %+v) = %v, want %v", tc.path, tc.sharing, got, tc.want)
			}
		})
	}
}

// TestAllowsArrayItselfWithoutBrackets records whether "activities" names the array as a parent path.
func TestAllowsArrayItselfWithoutBrackets(t *testing.T) {
	a := mustLoad(t)
	points := sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints}
	// PROBE: LeafPaths names an element "activities[]" and the array has no leaf of its own; the description says an
	// array is allowed when anything beneath it is, so the array "activities" should be allowed at points and not below.
	if !a.Allows("activities", points) {
		t.Errorf("Allows(activities, points) = false; the array holds allowed fields")
	}
	if a.Allows("activities", sharing.Sharing{Score: sharing.ScoreDetails}) {
		t.Errorf("Allows(activities, details/none) = true")
	}
}

// TestAllowsOnHandWrittenAllowlist checks requirements on a small file where every expectation is spelled out.
func TestAllowsOnHandWrittenAllowlist(t *testing.T) {
	a, err := sharing.LoadAllowlist(allowlistWith(
		`{"path":"a","requires":{}},` +
			`{"path":"b","requires":{"score":"score"}},` +
			`{"path":"c.d","requires":{"score":"details","activity":"summary"}},` +
			`{"path":"e[].f","requires":{"score":"details","activity":"points","strengthSets":true,"strengthWeights":true}}`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cases := []struct {
		// name describes the case.
		name string

		// path is the field asked.
		path string

		// sharing is the sharing asked.
		sharing sharing.Sharing

		// want is the expected answer.
		want bool
	}{
		{name: "no requirement at lowest", path: "a", sharing: sharing.Lowest, want: true},
		{name: "no requirement with unknown score", path: "a", sharing: sharing.Sharing{Score: "zzz"}, want: true},
		{name: "score at progress", path: "b", sharing: sharing.Lowest, want: false},
		{name: "score at score", path: "b", sharing: sharing.Sharing{Score: sharing.ScoreScore}, want: true},
		{name: "score at details", path: "b", sharing: sharing.Sharing{Score: sharing.ScoreDetails}, want: true},
		{name: "summary at points", path: "c.d", sharing: sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints}, want: false},
		{name: "summary at full", path: "c.d", sharing: sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityFull}, want: true},
		{name: "summary with score below details", path: "c.d", sharing: sharing.Sharing{Score: sharing.ScoreScore, Activity: sharing.ActivityFull}, want: false},
		{name: "parent c at summary", path: "c", sharing: sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivitySummary}, want: true},
		{name: "weights without sets", path: "e[].f", sharing: sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, StrengthWeights: true}, want: false},
		{name: "weights with sets", path: "e[].f", sharing: sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, StrengthSets: true, StrengthWeights: true}, want: true},
		{name: "weights with sets at highest", path: "e[].f", sharing: sharing.Highest, want: true},
		{name: "unknown activity fails closed", path: "e[].f", sharing: sharing.Sharing{Score: sharing.ScoreDetails, Activity: "max", StrengthSets: true, StrengthWeights: true}, want: false},
		{name: "e without brackets then f", path: "e.f", sharing: sharing.Highest, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.Allows(tc.path, tc.sharing); got != tc.want {
				t.Errorf("Allows(%s, %+v) = %v, want %v", tc.path, tc.sharing, got, tc.want)
			}
		})
	}
}

// FuzzLoadAllowlist feeds arbitrary bytes to LoadAllowlist and checks a loaded list is self-consistent.
func FuzzLoadAllowlist(f *testing.F) {
	f.Add(spec.Sharing)
	f.Add([]byte(`{`))
	f.Add([]byte(`null`))
	f.Add(allowlistWith(`{"path":"x","requires":{"routes":false}}`))
	f.Add(allowlistWith(`{"path":"a[].b","requires":{"score":"details","activity":"full"}}`))
	f.Add([]byte(`{"version":1,"levels":{"score":[],"activity":[]},"fields":[{"path":"x","requires":{"score":"progress"}}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		a, err := sharing.LoadAllowlist(data)
		if err != nil {
			return
		}
		paths := a.Paths()
		if !slices.IsSorted(paths) {
			t.Errorf("Paths() not sorted: %v", paths)
		}
		for i := 1; i < len(paths); i++ {
			if paths[i] == paths[i-1] {
				t.Errorf("Paths() repeats %q", paths[i])
			}
		}
		for _, p := range paths {
			if !a.Allows(p, sharing.Highest) {
				t.Errorf("loaded list refuses its own path %q at Highest", p)
			}
		}
		a.Allows("", sharing.Lowest)
		a.Strip(map[string]any{"x": 1, "a": []any{map[string]any{"b": 2}}}, sharing.Highest)
	})
}
