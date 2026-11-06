package scoreboard

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
	"github.com/jacobjohnston/FitHalo-Scoreboards/spec"
)

// loadAllowlist loads the allowlist from spec/sharing.json.
func loadAllowlist(t *testing.T) *sharing.Allowlist {
	t.Helper()
	allowlist, err := sharing.LoadAllowlist(spec.Sharing)
	if err != nil {
		t.Fatal(err)
	}
	return allowlist
}

// pointer returns a pointer to a copy of value.
func pointer[Value any](value Value) *Value {
	return &value
}

// testWeekStart is the Monday the week in fullChallenge starts.
var testWeekStart = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)

// fullChallenge is a weekly challenge with every field filled in, so stripping has something to remove at every level.
func fullChallenge() WeeklyChallenge {
	side := &StrengthSide{Reps: pointer[int64](8), DurationSeconds: pointer(30.0), WeightKilograms: pointer(20.0)}
	clusters := []StrengthCluster{{Reps: pointer[int64](4), WeightKilograms: pointer(15.0)}}
	activities := []Activity{{
		ID:              "AAAAAAAAAAAAAAAAAAAAAA",
		Type:            "traditionalStrengthTraining",
		Day:             "2026-10-20",
		Points:          &ActivityPoints{Base: 30, Bonus: 5},
		DurationSeconds: pointer(3600.0),
		DistanceMeters:  pointer(1200.0),
		Start:           pointer(time.Date(2026, 10, 20, 7, 30, 0, 0, time.UTC)),
		Details: &ActivityDetails{
			ActiveEnergyKilocalories:    pointer(400.0),
			AverageHeartRate:            pointer(130.0),
			MaximumHeartRate:            pointer(170.0),
			AverageSpeedMetersPerSecond: pointer(1.4),
			ElevationGainMeters:         pointer(30.0),
			ElevationLossMeters:         pointer(28.0),
			Steps:                       pointer[int64](5000),
			AverageCadence:              pointer(110.0),
			AveragePowerWatts:           pointer(180.0),
			AverageMETs:                 pointer(6.0),
			CarriedLoadKilograms:        pointer(13.6),
			InclinePercent:              pointer(2.0),
			StrollerLoadKilograms:       pointer(15.0),
			ExposureTemperatureCelsius:  pointer(80.0),
			Weather:                     &Weather{TemperatureCelsius: pointer(12.0), HumidityPercent: pointer(60.0)},
		},
		HasRoute: pointer(true),
		Strength: &Strength{Exercises: []StrengthExercise{{
			CatalogID:        pointer("6F1C2E3A-1B2C-4D5E-8F90-123456789ABC"),
			Name:             pointer("Landmine Press"),
			LoadIsAssistance: pointer(false),
			Sets: []StrengthSet{{
				Type:            "dropset",
				Reps:            pointer[int64](10),
				DurationSeconds: pointer(40.0),
				DistanceMeters:  pointer(20.0),
				WeightKilograms: pointer(25.0),
				Right:           side,
				Clusters:        &clusters,
			}},
		}}},
	}}
	return WeeklyChallenge{
		Progress:   pointer(0.8),
		Score:      pointer(400.0),
		Goal:       pointer(500.0),
		Tier:       pointer("monster"),
		Breakdown:  &Breakdown{Exercise: 350, DailyBonus: 30, VarietyBonus: 20},
		Activities: &activities,
		UpdatedAt:  pointer(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)),
	}
}

// decoded encodes value and decodes it back as a JSON object, as a request body arrives.
func decoded(t *testing.T, value any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	document, err := decodeObject(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

// TestNothingLeaks checks every sharing level keeps exactly the fields of a full weekly challenge it allows.
func TestNothingLeaks(t *testing.T) {
	allowlist := loadAllowlist(t)
	service := &Service{Allowlist: allowlist}
	full := decoded(t, fullChallenge())
	fullPaths := sharing.LeafPaths(full)

	for _, level := range sharing.All() {
		challenge, err := service.challengeFrom(decoded(t, fullChallenge()), level, testWeekStart)
		if err != nil {
			t.Fatalf("%+v: %v", level, err)
		}
		kept := sharing.LeafPaths(decoded(t, challenge))
		for _, path := range kept {
			if !allowlist.Allows(path, level) {
				t.Errorf("%+v kept %s, which it does not allow", level, path)
			}
		}
		for _, path := range fullPaths {
			if allowlist.Allows(path, level) && !slices.Contains(kept, path) {
				t.Errorf("%+v dropped %s, which it allows", level, path)
			}
		}
	}
}

// TestLowestSharesOnlyProgress pins the most private level to exactly the progress and the time.
func TestLowestSharesOnlyProgress(t *testing.T) {
	service := &Service{Allowlist: loadAllowlist(t)}
	challenge, err := service.challengeFrom(decoded(t, fullChallenge()), sharing.Lowest, testWeekStart)
	if err != nil {
		t.Fatal(err)
	}
	if paths := sharing.LeafPaths(decoded(t, challenge)); !slices.Equal(paths, []string{"progress", "updatedAt"}) {
		t.Fatalf("the lowest level kept %v", paths)
	}
}

// TestWeightsNeedTheirOwnOption checks sets without weights carry reps and never a weight.
func TestWeightsNeedTheirOwnOption(t *testing.T) {
	service := &Service{Allowlist: loadAllowlist(t)}
	setsOnly := sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, StrengthSets: true}
	challenge, err := service.challengeFrom(decoded(t, fullChallenge()), setsOnly, testWeekStart)
	if err != nil {
		t.Fatal(err)
	}
	set := (*challenge.Activities)[0].Strength.Exercises[0].Sets[0]
	if set.Reps == nil || set.WeightKilograms != nil || set.Right.WeightKilograms != nil || (*set.Clusters)[0].WeightKilograms != nil {
		t.Fatalf("sets without weights came out as %+v", set)
	}
	if (*challenge.Activities)[0].Details != nil || (*challenge.Activities)[0].DurationSeconds != nil {
		t.Fatal("sets without full activity still carried details")
	}
}

// TestAllowlistMatchesWeeklyChallenge checks WeeklyChallenge has exactly the fields spec/sharing.json lists.
func TestAllowlistMatchesWeeklyChallenge(t *testing.T) {
	var fields []string
	collectFields(reflect.TypeFor[WeeklyChallenge](), "", &fields)
	slices.Sort(fields)
	listed := loadAllowlist(t).Paths()
	for _, field := range fields {
		if !slices.Contains(listed, field) {
			t.Errorf("WeeklyChallenge has %s, which sharing.json does not list", field)
		}
	}
	for _, path := range listed {
		if !slices.Contains(fields, path) {
			t.Errorf("sharing.json lists %s, which WeeklyChallenge does not have", path)
		}
	}
}

// collectFields appends the JSON path of every leaf field in fieldType, under prefix, to fields.
func collectFields(fieldType reflect.Type, prefix string, fields *[]string) {
	for fieldType.Kind() == reflect.Pointer {
		fieldType = fieldType.Elem()
	}
	switch {
	case fieldType == reflect.TypeFor[time.Time]():
		*fields = append(*fields, prefix)
	case fieldType.Kind() == reflect.Slice:
		collectFields(fieldType.Elem(), prefix+"[]", fields)
	case fieldType.Kind() == reflect.Struct:
		for index := range fieldType.NumField() {
			name, _, _ := strings.Cut(fieldType.Field(index).Tag.Get("json"), ",")
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			collectFields(fieldType.Field(index).Type, path, fields)
		}
	default:
		*fields = append(*fields, prefix)
	}
}

// TestUnknownFieldsAreDropped checks a field from a newer client is stripped rather than refused.
func TestUnknownFieldsAreDropped(t *testing.T) {
	service := &Service{Allowlist: loadAllowlist(t)}
	document := decoded(t, fullChallenge())
	document["vo2Max"] = 52
	challenge, err := service.challengeFrom(document, sharing.Highest, testWeekStart)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(sharing.LeafPaths(decoded(t, challenge)), "vo2Max") {
		t.Fatal("an unknown field was kept")
	}
}
