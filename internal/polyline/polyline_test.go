package polyline

import (
	"math"
	"strings"
	"testing"
)

// TestGoogleExample decodes the example from the format's documentation.
func TestGoogleExample(t *testing.T) {
	points, err := Decode("_p~iF~ps|U_ulLnnqC_mqNvxq`@")
	if err != nil {
		t.Fatal(err)
	}
	expected := []Point{{38.5, -120.2}, {40.7, -120.95}, {43.252, -126.453}}
	if len(points) != len(expected) {
		t.Fatalf("%d points", len(points))
	}
	for index, point := range points {
		if math.Abs(point.Latitude-expected[index].Latitude) > 1e-9 || math.Abs(point.Longitude-expected[index].Longitude) > 1e-9 {
			t.Errorf("point %d is %+v", index, point)
		}
	}
}

// TestMalformed refuses truncated input, characters outside the format, and too many points.
func TestMalformed(t *testing.T) {
	for _, encoded := range []string{"_p~iF", "_p~iF~ps|", "!!!!", "_p~iF~ps|U~"} {
		if _, err := Decode(encoded); err == nil {
			t.Errorf("accepted %q", encoded)
		}
	}
	if _, err := Decode(strings.Repeat("??", MaximumPoints+1)); err != ErrTooManyPoints {
		t.Fatalf("too many points: %v", err)
	}
}

// FuzzDecode proves Decode never panics, and returns either a known error or in-range points.
func FuzzDecode(f *testing.F) {
	for _, seed := range []string{"_p~iF~ps|U_ulLnnqC_mqNvxq`@", "_p~iF", "??", "~~~~~~~~", "", "\x00\xff"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, encoded string) {
		points, err := Decode(encoded)
		if err != nil {
			if err != ErrInvalid && err != ErrTooManyPoints {
				t.Fatalf("Decode(%q) returned unexpected error %v", encoded, err)
			}
			if points != nil {
				t.Fatalf("Decode(%q) returned points with error %v", encoded, err)
			}
			return
		}
		if len(points) > MaximumPoints {
			t.Fatalf("Decode(%q) returned %d points, want at most %d", encoded, len(points), MaximumPoints)
		}
		for _, point := range points {
			if math.Abs(point.Latitude) > 90 || math.Abs(point.Longitude) > 180 {
				t.Fatalf("Decode(%q) returned out-of-range point %+v", encoded, point)
			}
		}
	})
}
