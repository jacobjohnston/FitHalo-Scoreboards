package sharing_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
)

// arrayWidth is how many elements every array of the probe document holds.
const arrayWidth = 3

// leafValue names a leaf by its path and the indices of every array on the way, so order and identity can be checked.
func leafValue(path string, indices []int) any {
	return fmt.Sprintf("%s#%v", path, indices)
}

// insertPath adds one allowlist path to node, filling every array with arrayWidth elements.
func insertPath(node map[string]any, segs []string, path string, indices []int) {
	seg := segs[0]
	key := strings.TrimSuffix(seg, "[]")
	isArray := key != seg
	if len(segs) == 1 {
		if isArray {
			arr := make([]any, arrayWidth)
			for i := range arr {
				arr[i] = leafValue(path, append(slices.Clone(indices), i))
			}
			node[key] = arr
			return
		}
		node[key] = leafValue(path, indices)
		return
	}
	if isArray {
		arr, ok := node[key].([]any)
		if !ok {
			arr = make([]any, arrayWidth)
			for i := range arr {
				arr[i] = map[string]any{}
			}
			node[key] = arr
		}
		for i, el := range arr {
			insertPath(el.(map[string]any), segs[1:], path, append(slices.Clone(indices), i))
		}
		return
	}
	child, ok := node[key].(map[string]any)
	if !ok {
		child = map[string]any{}
		node[key] = child
	}
	insertPath(child, segs[1:], path, indices)
}

// buildDocument fills every listed path with a leaf naming it.
func buildDocument(paths []string) map[string]any {
	doc := map[string]any{}
	for _, p := range paths {
		insertPath(doc, strings.Split(p, "."), p, nil)
	}
	return doc
}

// fullDocument is the probe document built from the real allowlist's paths.
func fullDocument(t testing.TB) map[string]any {
	t.Helper()
	return buildDocument(mustLoad(t).Paths())
}

// walkLeaves visits every value that is neither object nor array with its exact location and its allowlist notation.
func walkLeaves(v any, exact, notation string, visit func(exact, notation string, value any)) {
	switch node := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(node)) {
			childExact, childNotation := k, k
			if exact != "" {
				childExact, childNotation = exact+"."+k, notation+"."+k
			}
			walkLeaves(node[k], childExact, childNotation, visit)
		}
	case []any:
		for i, el := range node {
			walkLeaves(el, fmt.Sprintf("%s[%d]", exact, i), notation+"[]", visit)
		}
	default:
		visit(exact, notation, v)
	}
}

// notationsOf collects the set of allowlist notations of a document's leaves.
func notationsOf(doc map[string]any) map[string]bool {
	out := map[string]bool{}
	walkLeaves(doc, "", "", func(_, notation string, _ any) { out[notation] = true })
	return out
}

// valueBag counts a document's leaves by notation and encoded value, so Strip can be shown to invent nothing.
func valueBag(t testing.TB, doc map[string]any) map[string]int {
	t.Helper()
	bag := map[string]int{}
	walkLeaves(doc, "", "", func(_, notation string, value any) {
		bag[notation+"="+string(mustJSON(t, value))]++
	})
	return bag
}

// mustJSON encodes a document for a deep before-and-after comparison.
func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// underListedLeaf reports whether notation lies beneath a path the file lists as a leaf.
func underListedLeaf(paths []string, notation string) bool {
	for _, p := range paths {
		if strings.HasPrefix(notation, p+".") || strings.HasPrefix(notation, p+"[]") {
			return true
		}
	}
	return false
}

// checkStripInvariants checks the properties every Strip call must satisfy on any document.
func checkStripInvariants(t testing.TB, a *sharing.Allowlist, doc map[string]any, s sharing.Sharing) map[string]any {
	t.Helper()
	return checkStripInvariantsExcept(t, a, doc, s, nil)
}

// checkStripInvariantsExcept is checkStripInvariants with a known-defect filter on the leaves it reports.
func checkStripInvariantsExcept(t testing.TB, a *sharing.Allowlist, doc map[string]any, s sharing.Sharing, known func(notation string) bool) map[string]any {
	t.Helper()
	before := mustJSON(t, doc)
	out := a.Strip(doc, s)
	if after := mustJSON(t, doc); !bytes.Equal(before, after) {
		t.Errorf("Strip mutated its input at %+v:\n%s\n%s", s, before, after)
	}
	bag := valueBag(t, doc)
	for key := range valueBag(t, out) {
		if bag[key] <= 0 {
			t.Errorf("Strip at %+v added or changed %s", s, key)
		}
		bag[key]--
	}
	for notation := range notationsOf(out) {
		if !a.Allows(notation, s) && (known == nil || !known(notation)) {
			t.Errorf("Strip at %+v kept %s, which Allows refuses", s, notation)
		}
	}
	for _, leaf := range sharing.LeafPaths(out) {
		if !a.Allows(leaf, s) && (known == nil || !known(leaf)) {
			t.Errorf("Strip at %+v kept %s by LeafPaths, which Allows refuses", s, leaf)
		}
	}
	if again := a.Strip(out, s); !reflect.DeepEqual(again, out) {
		t.Errorf("Strip not idempotent at %+v:\n%s\n%s", s, mustJSON(t, out), mustJSON(t, again))
	}
	return out
}

// TestStripKeepsExactlyTheAllowedLeaves makes the leaves of Strip(full document) the allowed paths, for every sharing.
func TestStripKeepsExactlyTheAllowedLeaves(t *testing.T) {
	ps := loadProbeSpec(t)
	a := mustLoad(t)
	doc := fullDocument(t)
	for _, s := range everySharing() {
		out := checkStripInvariants(t, a, doc, s)
		want := expectedLeaves(t, ps, s)
		got := notationsOf(out)
		if !maps.Equal(got, want) {
			t.Errorf("Strip at %+v kept %v\nwant %v", s, slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
		}
		if byLeafPaths := sharing.LeafPaths(out); !maps.Equal(setOf(byLeafPaths), want) {
			t.Errorf("LeafPaths(Strip at %+v) = %v\nwant %v", s, byLeafPaths, slices.Sorted(maps.Keys(want)))
		}
	}
}

// setOf turns a slice into a set.
func setOf(items []string) map[string]bool {
	out := map[string]bool{}
	for _, item := range items {
		out[item] = true
	}
	return out
}

// TestStripAtHighestKeepsEverythingListed loses nothing from a document that holds only listed fields.
func TestStripAtHighestKeepsEverythingListed(t *testing.T) {
	a := mustLoad(t)
	doc := fullDocument(t)
	if out := a.Strip(doc, sharing.Highest); !reflect.DeepEqual(out, doc) {
		t.Errorf("Strip at Highest changed the document:\n%s\n%s", mustJSON(t, doc), mustJSON(t, out))
	}
}

// TestStripAtLowestKeepsOnlyProgress leaves the two fields that need nothing.
func TestStripAtLowestKeepsOnlyProgress(t *testing.T) {
	a := mustLoad(t)
	out := a.Strip(fullDocument(t), sharing.Lowest)
	want := map[string]any{"progress": leafValue("progress", nil), "updatedAt": leafValue("updatedAt", nil)}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("Strip at Lowest = %s", mustJSON(t, out))
	}
}

// TestStripKeepsArrayElementsInOrder strips each element on its own and keeps the array's length and order.
func TestStripKeepsArrayElementsInOrder(t *testing.T) {
	a := mustLoad(t)
	points := sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints}
	out := a.Strip(fullDocument(t), points)
	activities, ok := out["activities"].([]any)
	if !ok || len(activities) != arrayWidth {
		t.Fatalf("activities = %#v, want %d elements", out["activities"], arrayWidth)
	}
	for i, el := range activities {
		activity, ok := el.(map[string]any)
		if !ok {
			t.Fatalf("activities[%d] = %#v", i, el)
		}
		if got, want := activity["id"], leafValue("activities[].id", []int{i}); got != want {
			t.Errorf("activities[%d].id = %v, want %v", i, got, want)
		}
		if _, leaked := activity["durationSeconds"]; leaked {
			t.Errorf("activities[%d] kept durationSeconds at points", i)
		}
		if _, leaked := activity["strength"]; leaked {
			t.Errorf("activities[%d] kept strength without strengthSets", i)
		}
	}
}

// TestStripKeepsEmptyContainersBeneathWhichSomethingIsAllowed keeps an empty object or array when its children would be.
func TestStripKeepsEmptyContainersBeneathWhichSomethingIsAllowed(t *testing.T) {
	a := mustLoad(t)
	details := sharing.Sharing{Score: sharing.ScoreDetails}
	points := sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints}
	full := sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityFull}
	sets := sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, StrengthSets: true}
	cases := []struct {
		// name describes the case.
		name string

		// doc is the input.
		doc map[string]any

		// sharing is applied.
		sharing sharing.Sharing

		// want is the expected output.
		want map[string]any
	}{
		{name: "empty breakdown at details", doc: map[string]any{"breakdown": map[string]any{}}, sharing: details, want: map[string]any{"breakdown": map[string]any{}}},
		{name: "empty breakdown at score", doc: map[string]any{"breakdown": map[string]any{}}, sharing: sharing.Sharing{Score: sharing.ScoreScore}, want: map[string]any{}},
		{name: "empty activities at points", doc: map[string]any{"activities": []any{}}, sharing: points, want: map[string]any{"activities": []any{}}},
		{name: "empty activities at details none", doc: map[string]any{"activities": []any{}}, sharing: details, want: map[string]any{}},
		{name: "empty activity element at points", doc: map[string]any{"activities": []any{map[string]any{}}}, sharing: points, want: map[string]any{"activities": []any{map[string]any{}}}},
		{name: "empty details at full", doc: map[string]any{"activities": []any{map[string]any{"details": map[string]any{}}}}, sharing: full, want: map[string]any{"activities": []any{map[string]any{"details": map[string]any{}}}}},
		{name: "empty details at points", doc: map[string]any{"activities": []any{map[string]any{"details": map[string]any{}}}}, sharing: points, want: map[string]any{"activities": []any{map[string]any{}}}},
		{name: "empty weather at full", doc: map[string]any{"activities": []any{map[string]any{"details": map[string]any{"weather": map[string]any{}}}}}, sharing: full, want: map[string]any{"activities": []any{map[string]any{"details": map[string]any{"weather": map[string]any{}}}}}},
		{name: "empty strength at sets", doc: map[string]any{"activities": []any{map[string]any{"strength": map[string]any{"exercises": []any{}}}}}, sharing: sets, want: map[string]any{"activities": []any{map[string]any{"strength": map[string]any{"exercises": []any{}}}}}},
		{name: "empty strength at points", doc: map[string]any{"activities": []any{map[string]any{"strength": map[string]any{"exercises": []any{}}}}}, sharing: points, want: map[string]any{"activities": []any{map[string]any{}}}},
		{name: "element emptied of everything stays", doc: map[string]any{"activities": []any{map[string]any{"start": "x"}}}, sharing: points, want: map[string]any{"activities": []any{map[string]any{}}}},
		{name: "empty unknown object dropped", doc: map[string]any{"heartRate": map[string]any{}}, sharing: sharing.Highest, want: map[string]any{}},
		{name: "empty document", doc: map[string]any{}, sharing: sharing.Highest, want: map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkStripInvariants(t, a, tc.doc, tc.sharing)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Strip = %s, want %s", mustJSON(t, got), mustJSON(t, tc.want))
			}
		})
	}
}

// TestStripKeepsNullLeaves treats a null as a value of its field.
func TestStripKeepsNullLeaves(t *testing.T) {
	a := mustLoad(t)
	doc := map[string]any{"progress": nil, "score": nil, "breakdown": map[string]any{"exercise": nil}, "activities": nil}
	out := checkStripInvariants(t, a, doc, sharing.Sharing{Score: sharing.ScoreScore})
	if v, ok := out["score"]; !ok || v != nil {
		t.Errorf("null score at score level: %#v, present %v", v, ok)
	}
	if v, ok := out["progress"]; !ok || v != nil {
		t.Errorf("null progress: %#v, present %v", v, ok)
	}
	if _, leaked := out["breakdown"]; leaked {
		t.Errorf("breakdown kept at score level")
	}
	// PROBE: a null where an array is expected has nothing beneath it; the contract does not say whether it stays.
	if _, kept := out["activities"]; kept {
		t.Logf("null activities kept at score level")
	}
	full := checkStripInvariants(t, a, doc, sharing.Highest)
	if _, kept := full["activities"]; kept {
		t.Logf("null activities kept at Highest")
	} else {
		t.Logf("null activities dropped at Highest")
	}
}

// TestStripDropsUnknownFieldsAtEveryDepth removes a field the file does not list wherever it sits, even at Highest.
func TestStripDropsUnknownFieldsAtEveryDepth(t *testing.T) {
	a := mustLoad(t)
	doc := fullDocument(t)
	doc["title"] = "secret"
	doc["breakdown"].(map[string]any)["heartRate"] = 1
	for _, el := range doc["activities"].([]any) {
		activity := el.(map[string]any)
		activity["title"] = "secret"
		activity["route"] = "polyline"
		activity["points"].(map[string]any)["total"] = 2
		activity["details"].(map[string]any)["heartRateSamples"] = []any{1, 2}
		activity["details"].(map[string]any)["weather"].(map[string]any)["city"] = "secret"
		for _, ex := range activity["strength"].(map[string]any)["exercises"].([]any) {
			exercise := ex.(map[string]any)
			exercise["notes"] = "secret"
			for _, st := range exercise["sets"].([]any) {
				set := st.(map[string]any)
				set["rpe"] = 9
				set["right"].(map[string]any)["rpe"] = 9
				for _, cl := range set["clusters"].([]any) {
					cl.(map[string]any)["rest"] = 30
				}
			}
		}
	}
	for _, s := range sharing.All() {
		out := checkStripInvariants(t, a, doc, s)
		data := string(mustJSON(t, out))
		for _, word := range []string{"secret", "title", "route\"", "total", "heartRate", "city", "notes", "rpe", "rest"} {
			if strings.Contains(data, word) {
				t.Errorf("Strip at %+v kept %q: %s", s, word, data)
			}
		}
	}
}

// TestStripKeepsNumberRepresentations keeps float64 and json.Number leaves exactly as decoded.
func TestStripKeepsNumberRepresentations(t *testing.T) {
	a := mustLoad(t)
	raw := `{"progress":0.5,"score":12345678901234567890,"goal":1e3,"tier":"deity","activities":[{"id":"a","points":{"base":1.25,"bonus":-0}}]}`
	for _, useNumber := range []bool{false, true} {
		dec := json.NewDecoder(strings.NewReader(raw))
		if useNumber {
			dec.UseNumber()
		}
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := checkStripInvariants(t, a, doc, sharing.Highest)
		if !reflect.DeepEqual(out, doc) {
			t.Errorf("useNumber=%v: Strip at Highest changed numbers:\n%s\n%s", useNumber, mustJSON(t, doc), mustJSON(t, out))
		}
		if useNumber {
			if _, ok := out["score"].(json.Number); !ok {
				t.Errorf("score became %T, want json.Number", out["score"])
			}
		}
	}
}

// TestStripSurvivesWrongShapes neither panics nor leaks when a container has the wrong JSON type.
func TestStripSurvivesWrongShapes(t *testing.T) {
	a := mustLoad(t)
	cases := []struct {
		// name describes the shape.
		name string

		// doc is the input.
		doc map[string]any
	}{
		{name: "activities as object", doc: map[string]any{"activities": map[string]any{"id": "x", "start": "y"}}},
		{name: "activities as string", doc: map[string]any{"activities": "x"}},
		{name: "activities as number", doc: map[string]any{"activities": 1.0}},
		{name: "activities as bool", doc: map[string]any{"activities": true}},
		{name: "activities of scalars", doc: map[string]any{"activities": []any{1.0, "x", nil, true}}},
		{name: "activities of arrays", doc: map[string]any{"activities": []any{[]any{map[string]any{"id": "x"}}}}},
		{name: "activities of mixed", doc: map[string]any{"activities": []any{map[string]any{"id": "x"}, "y", []any{}, nil}}},
		{name: "breakdown as array", doc: map[string]any{"breakdown": []any{map[string]any{"exercise": 1.0}}}},
		{name: "breakdown as string", doc: map[string]any{"breakdown": "x"}},
		{name: "breakdown as null", doc: map[string]any{"breakdown": nil}},
		{name: "points as array", doc: map[string]any{"activities": []any{map[string]any{"points": []any{map[string]any{"base": 1.0}}}}}},
		{name: "points as scalar", doc: map[string]any{"activities": []any{map[string]any{"points": 3.0}}}},
		{name: "exercises as object", doc: map[string]any{"activities": []any{map[string]any{"strength": map[string]any{"exercises": map[string]any{"name": "x"}}}}}},
		{name: "sets of scalars", doc: map[string]any{"activities": []any{map[string]any{"strength": map[string]any{"exercises": []any{map[string]any{"sets": []any{1.0, "x"}}}}}}}},
		{name: "weather as array", doc: map[string]any{"activities": []any{map[string]any{"details": map[string]any{"weather": []any{map[string]any{"humidityPercent": 1.0}}}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, s := range sharing.All() {
				out := checkStripInvariants(t, a, tc.doc, s)
				if s == sharing.Highest {
					t.Logf("at Highest: %s", mustJSON(t, out))
				}
			}
		})
	}
}

// TestStripDropsUnknownFieldsBeneathAListedLeaf removes a subtree sitting where the file lists a scalar.
func TestStripDropsUnknownFieldsBeneathAListedLeaf(t *testing.T) {
	a := mustLoad(t)
	cases := []struct {
		// name describes the shape.
		name string

		// doc is the input.
		doc map[string]any

		// leaked is the notation that must not survive.
		leaked string
	}{
		{name: "object under progress", doc: map[string]any{"progress": map[string]any{"x": 1.0}}, leaked: "progress.x"},
		{name: "array under progress", doc: map[string]any{"progress": []any{1.0}}, leaked: "progress[]"},
		{name: "object under updatedAt", doc: map[string]any{"updatedAt": map[string]any{"title": "secret"}}, leaked: "updatedAt.title"},
		{name: "object under breakdown.exercise", doc: map[string]any{"breakdown": map[string]any{"exercise": map[string]any{"y": 1.0}}}, leaked: "breakdown.exercise.y"},
		{name: "object under activities[].id", doc: map[string]any{"activities": []any{map[string]any{"id": map[string]any{"route": "polyline"}}}}, leaked: "activities[].id.route"},
		{name: "deep arrays under activities[].id", doc: map[string]any{"activities": []any{map[string]any{"id": deepArray(50)}}}, leaked: "activities[].id" + strings.Repeat("[]", 50)},
		{name: "object under a set weight", doc: map[string]any{"activities": []any{map[string]any{"strength": map[string]any{"exercises": []any{map[string]any{"sets": []any{map[string]any{"weightKilograms": map[string]any{"note": "x"}}}}}}}}}, leaked: "activities[].strength.exercises[].sets[].weightKilograms.note"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if a.Allows(tc.leaked, sharing.Highest) {
				t.Fatalf("Allows(%s, Highest) = true; the probe is wrong", tc.leaked)
			}
			for _, s := range []sharing.Sharing{sharing.Lowest, sharing.Highest} {
				out := a.Strip(tc.doc, s)
				if notationsOf(out)[tc.leaked] {
					t.Errorf("Strip at %+v kept %s: %s", s, tc.leaked, mustJSON(t, out))
				}
			}
		})
	}
}

// TestStripArrayElementsThatAreNotObjects records what happens to a null or scalar where an activity belongs.
func TestStripArrayElementsThatAreNotObjects(t *testing.T) {
	a := mustLoad(t)
	points := sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints}
	doc := map[string]any{"activities": []any{[]any{map[string]any{"id": "a"}}, nil, 1.0, "s", map[string]any{"id": "b"}, map[string]any{}}}
	out := checkStripInvariants(t, a, doc, points)
	// PROBE: the contract says elements are stripped, not that an element of the wrong type is removed; removing
	// one shifts the index of every later element, while an empty object element stays. Record the shape.
	t.Logf("activities after Strip at points: %s", mustJSON(t, out))
	activities, _ := out["activities"].([]any)
	if len(activities) != len(doc["activities"].([]any)) {
		t.Logf("array shrank from %d to %d elements", len(doc["activities"].([]any)), len(activities))
	}
}

// TestStripScalarAtParentPath records whether a scalar sitting where an object belongs survives.
func TestStripScalarAtParentPath(t *testing.T) {
	a := mustLoad(t)
	details := sharing.Sharing{Score: sharing.ScoreDetails}
	out := a.Strip(map[string]any{"breakdown": "not an object", "activities": "not an array"}, sharing.Highest)
	// PROBE: "breakdown" has no leaf of its own in the file; the parent rule allows the object because of what is
	// beneath it, so a scalar there is allowed by Allows yet names no listed field. The contract does not say.
	t.Logf("scalar breakdown at Highest kept: %v; scalar activities kept: %v", out["breakdown"] != nil, out["activities"] != nil)
	if v := a.Strip(map[string]any{"breakdown": "x"}, details)["breakdown"]; v != nil {
		t.Logf("scalar breakdown at details kept as %#v", v)
	}
	if v := a.Strip(map[string]any{"breakdown": "x"}, sharing.Sharing{Score: sharing.ScoreScore})["breakdown"]; v != nil {
		t.Errorf("scalar breakdown kept at score level: %#v", v)
	}
}

// TestStripKeysContainingNotation records how a key that spells a path is treated.
func TestStripKeysContainingNotation(t *testing.T) {
	a := mustLoad(t)
	points := sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints}
	doc := map[string]any{"breakdown.exercise": 1.0, "activities[].id": "x", "activities[]": []any{map[string]any{"id": "y"}}}
	out := checkStripInvariants(t, a, doc, points)
	// PROBE: these keys are not the listed fields, only spelled like them; the contract does not say whether a key
	// holding "." or "[]" is matched by notation or refused.
	t.Logf("keys spelled like paths at details/points: %s", mustJSON(t, out))
	if lowest := a.Strip(doc, sharing.Lowest); len(lowest) != 0 {
		t.Errorf("keys spelled like paths kept at Lowest: %s", mustJSON(t, lowest))
	}
}

// TestStripNilDocument accepts a nil map and an empty one.
func TestStripNilDocument(t *testing.T) {
	a := mustLoad(t)
	for _, s := range sharing.All() {
		if out := a.Strip(nil, s); len(out) != 0 {
			t.Errorf("Strip(nil, %+v) = %v", s, out)
		}
		if out := a.Strip(map[string]any{}, s); len(out) != 0 {
			t.Errorf("Strip(empty, %+v) = %v", s, out)
		}
	}
	if got := sharing.LeafPaths(nil); len(got) != 0 {
		t.Errorf("LeafPaths(nil) = %v", got)
	}
}

// deepObject nests depth objects under key, ending in a leaf.
func deepObject(key string, depth int) any {
	var v any = "bottom"
	for range depth {
		v = map[string]any{key: v}
	}
	return v
}

// deepArray nests depth arrays, ending in a leaf.
func deepArray(depth int) any {
	var v any = "bottom"
	for range depth {
		v = []any{v}
	}
	return v
}

// TestStripSurvivesDeepNesting handles a thousand levels of objects and arrays without overflowing.
func TestStripSurvivesDeepNesting(t *testing.T) {
	a := mustLoad(t)
	const depth = 1000
	doc := map[string]any{
		"x":          deepObject("x", depth),
		"breakdown":  map[string]any{"y": deepObject("y", depth)},
		"activities": []any{map[string]any{"notes": deepArray(depth), "details": deepObject("weather", depth)}},
		"arrays":     deepArray(depth),
	}
	for _, s := range []sharing.Sharing{sharing.Highest, sharing.Lowest, {Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints}} {
		out := checkStripInvariants(t, a, doc, s)
		if _, kept := out["x"]; kept {
			t.Errorf("deep unknown kept at %+v", s)
		}
		if _, kept := out["arrays"]; kept {
			t.Errorf("deep unknown array kept at %+v", s)
		}
	}
	if got := len(sharing.LeafPaths(doc)); got == 0 {
		t.Errorf("LeafPaths found nothing in the deep document")
	}
}

// TestStripScalesWithDocumentSize strips a wide and a deep document in time linear enough to finish quickly.
func TestStripScalesWithDocumentSize(t *testing.T) {
	a := mustLoad(t)
	wide := make([]any, 20000)
	for i := range wide {
		wide[i] = map[string]any{"id": "x", "type": "yoga", "day": 1.0, "points": map[string]any{"base": 1.0, "bonus": 2.0}, "start": "t", "title": "secret"}
	}
	deepKeys := map[string]any{}
	node := deepKeys
	for range 2000 {
		next := map[string]any{}
		node["breakdown"] = next
		node = next
	}
	docs := map[string]map[string]any{
		"wide activities":      {"activities": wide},
		"deep unknown objects": {"x": deepObject("x", 5000)},
		"deep unknown arrays":  {"x": deepArray(5000)},
		"deep parent chain":    deepKeys,
	}
	for name, doc := range docs {
		start := time.Now()
		for _, s := range sharing.All() {
			a.Strip(doc, s)
		}
		sharing.LeafPaths(doc)
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("%s: %v for 21 strips and one LeafPaths", name, elapsed)
		} else {
			t.Logf("%s: %v", name, elapsed)
		}
	}
}

// TestStripUnknownLevelFailsClosed strips to Lowest when the sharing holds a level the API does not define.
func TestStripUnknownLevelFailsClosed(t *testing.T) {
	a := mustLoad(t)
	doc := fullDocument(t)
	lowest := a.Strip(doc, sharing.Lowest)
	for _, s := range unknownSharings() {
		if s.Score == sharing.ScoreDetails {
			continue
		}
		if out := a.Strip(doc, s); !reflect.DeepEqual(out, lowest) {
			t.Errorf("Strip at %+v kept %v", s, sharing.LeafPaths(out))
		}
	}
	detailsOnly := a.Strip(doc, sharing.Sharing{Score: sharing.ScoreDetails})
	for _, s := range unknownSharings() {
		if s.Score != sharing.ScoreDetails {
			continue
		}
		if out := a.Strip(doc, s); !reflect.DeepEqual(out, detailsOnly) {
			t.Errorf("Strip at %+v kept %v", s, sharing.LeafPaths(out))
		}
	}
}

// TestStripReadsNonNormalizedSharingAsNormalized strips a raw sharing exactly like its normalised form.
func TestStripReadsNonNormalizedSharingAsNormalized(t *testing.T) {
	a := mustLoad(t)
	doc := fullDocument(t)
	for _, s := range rawGrid() {
		if out, want := a.Strip(doc, s), a.Strip(doc, s.Normalized()); !reflect.DeepEqual(out, want) {
			t.Errorf("Strip at %+v differs from its normalised form %+v", s, s.Normalized())
		}
	}
}

// TestLeafPathsNotation names every leaf the way the file does.
func TestLeafPathsNotation(t *testing.T) {
	cases := []struct {
		// name describes the document.
		name string

		// doc is the input.
		doc map[string]any

		// want is the expected set of paths.
		want []string
	}{
		{name: "scalar at top", doc: map[string]any{"a": 1.0}, want: []string{"a"}},
		{name: "every scalar kind", doc: map[string]any{"s": "x", "n": 1.0, "b": true, "z": nil, "j": json.Number("2")}, want: []string{"b", "j", "n", "s", "z"}},
		{name: "nested object", doc: map[string]any{"a": map[string]any{"b": map[string]any{"c": 1.0}}}, want: []string{"a.b.c"}},
		{name: "array of objects", doc: map[string]any{"a": []any{map[string]any{"b": 1.0}, map[string]any{"c": 2.0}}}, want: []string{"a[].b", "a[].c"}},
		{name: "array of scalars", doc: map[string]any{"a": []any{1.0, 2.0}}, want: []string{"a[]"}},
		{name: "array of arrays", doc: map[string]any{"a": []any{[]any{1.0}}}, want: []string{"a[][]"}},
		{name: "array of arrays of objects", doc: map[string]any{"a": []any{[]any{map[string]any{"b": 1.0}}}}, want: []string{"a[][].b"}},
		{name: "spec shaped", doc: map[string]any{"activities": []any{map[string]any{"points": map[string]any{"base": 1.0}, "strength": map[string]any{"exercises": []any{map[string]any{"sets": []any{map[string]any{"clusters": []any{map[string]any{"reps": 1.0}}}}}}}}}}, want: []string{"activities[].points.base", "activities[].strength.exercises[].sets[].clusters[].reps"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sharing.LeafPaths(tc.doc)
			if !maps.Equal(setOf(got), setOf(tc.want)) {
				t.Errorf("LeafPaths = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLeafPathsOfTheFullDocumentArePaths makes the notation round-trip: a document built from Paths() leafs back to Paths().
func TestLeafPathsOfTheFullDocumentArePaths(t *testing.T) {
	paths := mustLoad(t).Paths()
	got := sharing.LeafPaths(buildDocument(paths))
	if !maps.Equal(setOf(got), setOf(paths)) {
		t.Errorf("LeafPaths = %v\nwant %v", slices.Sorted(maps.Keys(setOf(got))), paths)
	}
	if len(got) != len(setOf(got)) {
		// PROBE: three activities give the same notation three times; the doc does not say whether paths repeat.
		t.Logf("LeafPaths repeats paths over array elements: %d entries, %d distinct", len(got), len(setOf(got)))
	}
}

// TestLeafPathsEmptyContainers records whether an empty object or array counts as a value.
func TestLeafPathsEmptyContainers(t *testing.T) {
	// PROBE: "neither an object nor an array" excludes an empty container, so it should produce no path; a leak test
	// then cannot see an empty unknown object, which Strip must still drop.
	for name, doc := range map[string]map[string]any{"empty object": {"a": map[string]any{}}, "empty array": {"a": []any{}}, "empty document": {}} {
		if got := sharing.LeafPaths(doc); len(got) != 0 {
			t.Logf("LeafPaths(%s) = %v", name, got)
		}
	}
}

// TestLeafPathsDoesNotMutate leaves its input untouched.
func TestLeafPathsDoesNotMutate(t *testing.T) {
	doc := fullDocument(t)
	before := mustJSON(t, doc)
	sharing.LeafPaths(doc)
	if after := mustJSON(t, doc); !bytes.Equal(before, after) {
		t.Errorf("LeafPaths changed its input")
	}
}

// TestStripConcurrently runs Strip and Allows from many goroutines on one allowlist and one document.
func TestStripConcurrently(t *testing.T) {
	a := mustLoad(t)
	doc := fullDocument(t)
	all := sharing.All()
	done := make(chan struct{})
	for i := range 16 {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			s := all[i%len(all)]
			for range 20 {
				out := a.Strip(doc, s)
				for _, leaf := range sharing.LeafPaths(out) {
					if !a.Allows(leaf, s) {
						t.Errorf("concurrent Strip kept %s at %+v", leaf, s)
					}
				}
			}
		}(i)
	}
	for range 16 {
		<-done
	}
}

// FuzzStrip strips arbitrary JSON documents at every sharing and checks the leak invariants.
func FuzzStrip(f *testing.F) {
	a := mustLoad(f)
	f.Add(string(mustJSON(f, buildDocument(a.Paths()))))
	f.Add(`{"activities":"x","breakdown":[1],"progress":{"a":1}}`)
	f.Add(`{"activities":[[{"id":"x"}],null,1,{"id":"y","details":{"weather":{"city":"z"}}}]}`)
	f.Add(`{"breakdown.exercise":1,"activities[].id":"x"}`)
	f.Add(`{"a":{"a":{"a":{"a":[[[[{"a":1}]]]]}}}}`)
	f.Add(`{"score":null,"goal":1e400,"tier":""}`)
	paths := a.Paths()
	// known is the subtree leak TestStripDropsUnknownFieldsBeneathAListedLeaf reports, set aside so other crashers surface.
	known := func(notation string) bool { return underListedLeaf(paths, notation) }
	f.Fuzz(func(t *testing.T, raw string) {
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			return
		}
		for _, s := range sharing.All() {
			checkStripInvariantsExcept(t, a, doc, s, known)
		}
		checkStripInvariantsExcept(t, a, doc, sharing.Sharing{Score: "everything", Activity: "all", Routes: true, StrengthSets: true, StrengthWeights: true}, known)
	})
}

// vocabularies is spec/vocabularies.json decoded for the sanity test.
type vocabularies struct {
	// Version is the file's format version.
	Version int `json:"version"`

	// Description explains the file.
	Description string `json:"description"`

	// ActivityTypes lists Activity.type.
	ActivityTypes vocabulary `json:"activityTypes"`

	// SetTypes lists StrengthSet.type.
	SetTypes vocabulary `json:"setTypes"`

	// Tiers lists WeeklyChallenge.tier.
	Tiers vocabulary `json:"tiers"`
}

// vocabulary is one list of the file.
type vocabulary struct {
	// Description explains the list.
	Description string `json:"description"`

	// Values are the known values in order.
	Values []struct {
		// Slug is the wire value.
		Slug string `json:"slug"`

		// Name is the English name.
		Name string `json:"name"`
	} `json:"values"`
}

// slugPattern is an ASCII identifier: a letter then letters or digits.
var slugPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)

// TestVocabularyFileIsWellFormed checks the slugs of spec/vocabularies.json are unique ASCII identifiers.
func TestVocabularyFileIsWellFormed(t *testing.T) {
	data, err := os.ReadFile("../../spec/vocabularies.json")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var v vocabularies
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.Version != 1 {
		t.Errorf("version %d", v.Version)
	}
	for name, list := range map[string]vocabulary{"activityTypes": v.ActivityTypes, "setTypes": v.SetTypes, "tiers": v.Tiers} {
		if len(list.Values) == 0 || list.Description == "" {
			t.Errorf("%s is empty or undescribed", name)
		}
		seen := map[string]bool{}
		for _, value := range list.Values {
			if seen[value.Slug] {
				t.Errorf("%s repeats slug %q", name, value.Slug)
			}
			seen[value.Slug] = true
			if value.Slug == "" || len(value.Slug) > 64 || !slugPattern.MatchString(value.Slug) {
				t.Errorf("%s slug %q is not an ASCII identifier of at most 64 bytes", name, value.Slug)
			}
			if strings.TrimSpace(value.Name) == "" || value.Name != strings.TrimSpace(value.Name) {
				t.Errorf("%s slug %q has name %q", name, value.Slug, value.Name)
			}
		}
	}
	if first, last := v.Tiers.Values[0].Slug, v.Tiers.Values[len(v.Tiers.Values)-1].Slug; first != "beginner" || last != "deity" {
		t.Errorf("tiers run %s..%s, want beginner..deity as the docs describe", first, last)
	}
	if !slices.Contains(slices.Collect(func(yield func(string) bool) {
		for _, value := range v.ActivityTypes.Values {
			if !yield(value.Slug) {
				return
			}
		}
	}), "unknown") {
		t.Errorf("activityTypes lacks the unknown value its description promises")
	}
	if !slices.Equal(slugsOf(v.SetTypes), []string{"warmup", "normal", "failure", "myo", "dropset", "isometric"}) {
		t.Errorf("setTypes = %v", slugsOf(v.SetTypes))
	}
}

// slugsOf lists a vocabulary's slugs in order.
func slugsOf(list vocabulary) []string {
	var out []string
	for _, value := range list.Values {
		out = append(out, value.Slug)
	}
	return out
}
