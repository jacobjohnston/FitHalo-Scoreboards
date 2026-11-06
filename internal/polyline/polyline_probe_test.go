package polyline_test

import (
	"errors"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/polyline"
)

// encodeValue appends one coordinate delta in Google's published 5-bit chunk form.
func encodeValue(builder *strings.Builder, value int64) {
	shifted := value << 1
	if value < 0 {
		shifted = ^shifted
	}
	for shifted >= 0x20 {
		builder.WriteByte(byte((0x20 | (shifted & 0x1f)) + 63))
		shifted >>= 5
	}
	builder.WriteByte(byte(shifted + 63))
}

// encode is an independent encoder written from the Encoded Polyline Algorithm Format description.
func encode(points []polyline.Point) string {
	var builder strings.Builder
	var previousLatitude, previousLongitude int64
	for _, point := range points {
		latitude := int64(math.Round(point.Latitude * 1e5))
		longitude := int64(math.Round(point.Longitude * 1e5))
		encodeValue(&builder, latitude-previousLatitude)
		encodeValue(&builder, longitude-previousLongitude)
		previousLatitude, previousLongitude = latitude, longitude
	}
	return builder.String()
}

// randomPoints returns count points anywhere on the globe, snapped to precision 5.
func randomPoints(random *rand.Rand, count int) []polyline.Point {
	points := make([]polyline.Point, count)
	for index := range points {
		points[index] = polyline.Point{
			Latitude:  float64(random.Int63n(180_00001)-90_00000) / 1e5,
			Longitude: float64(random.Int63n(360_00001)-180_00000) / 1e5,
		}
	}
	return points
}

// samePoints reports whether two routes agree to a billionth of a degree.
func samePoints(got, want []polyline.Point) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if math.Abs(got[index].Latitude-want[index].Latitude) > 1e-9 || math.Abs(got[index].Longitude-want[index].Longitude) > 1e-9 {
			return false
		}
	}
	return true
}

// TestGoogleDocumentedExample decodes the example from Google's algorithm description.
func TestGoogleDocumentedExample(t *testing.T) {
	points, err := polyline.Decode("_p~iF~ps|U_ulLnnqC_mqNvxq`@")
	if err != nil {
		t.Fatal(err)
	}
	want := []polyline.Point{{38.5, -120.2}, {40.7, -120.95}, {43.252, -126.453}}
	if !samePoints(points, want) {
		t.Fatalf("decoded %v, want %v", points, want)
	}
}

// TestRoundTripRandomRoutes decodes routes from an independent encoder back to their points.
func TestRoundTripRandomRoutes(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	for trial := range 200 {
		count := random.Intn(polyline.MaximumPoints + 1)
		points := randomPoints(random, count)
		got, err := polyline.Decode(encode(points))
		if err != nil {
			t.Fatalf("trial %d (%d points): %v", trial, count, err)
		}
		if !samePoints(got, points) {
			t.Fatalf("trial %d (%d points): decoded route differs", trial, count)
		}
	}
}

// TestExtremeCoordinates accepts every corner of the globe and the single-point route.
func TestExtremeCoordinates(t *testing.T) {
	for _, test := range []struct {
		// name describes the route.
		name string

		// points is the route to encode and decode.
		points []polyline.Point
	}{
		{name: "one point at the origin", points: []polyline.Point{{0, 0}}},
		{name: "north pole and date line", points: []polyline.Point{{90, 180}}},
		{name: "south pole and antimeridian", points: []polyline.Point{{-90, -180}}},
		{name: "every corner in turn", points: []polyline.Point{{90, 180}, {-90, -180}, {90, -180}, {-90, 180}, {0, 0}}},
		{name: "smallest step", points: []polyline.Point{{0, 0}, {0.00001, -0.00001}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := polyline.Decode(encode(test.points))
			if err != nil {
				t.Fatal(err)
			}
			if !samePoints(got, test.points) {
				t.Fatalf("decoded %v, want %v", got, test.points)
			}
		})
	}
}

// TestDecodedPrecisionIsExact checks each decoded value is value/1e5 to within a billionth.
func TestDecodedPrecisionIsExact(t *testing.T) {
	points, err := polyline.Decode(encode([]polyline.Point{{12.34567, -98.76543}, {12.34568, -98.76544}}))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(points[1].Latitude-12.34568) > 1e-9 || math.Abs(points[1].Longitude+98.76544) > 1e-9 {
		t.Fatalf("decoded %v", points)
	}
}

// TestMaximumPointsBoundary accepts exactly MaximumPoints and refuses one more.
func TestMaximumPointsBoundary(t *testing.T) {
	random := rand.New(rand.NewSource(11))
	atLimit := randomPoints(random, polyline.MaximumPoints)
	if points, err := polyline.Decode(encode(atLimit)); err != nil || len(points) != polyline.MaximumPoints {
		t.Fatalf("%d points: %d decoded, %v", polyline.MaximumPoints, len(points), err)
	}
	overLimit := randomPoints(random, polyline.MaximumPoints+1)
	if _, err := polyline.Decode(encode(overLimit)); !errors.Is(err, polyline.ErrTooManyPoints) {
		t.Fatalf("%d points: %v, want ErrTooManyPoints", polyline.MaximumPoints+1, err)
	}
}

// TestTooManyPointsWithBadTailReportsWhich records which error wins when a route is both too long and malformed.
func TestTooManyPointsWithBadTailReportsWhich(t *testing.T) {
	random := rand.New(rand.NewSource(13))
	overLimit := encode(randomPoints(random, polyline.MaximumPoints+1))
	// PROBE: the contract does not order the checks; record which error a too-long route with an out-of-range final point gets.
	outOfRange := encode(append(randomPoints(random, polyline.MaximumPoints+1), polyline.Point{Latitude: 91}))
	_, truncatedErr := polyline.Decode(overLimit[:len(overLimit)-1])
	_, rangeErr := polyline.Decode(outOfRange)
	t.Logf("too many points and truncated: %v; too many points and out of range: %v", truncatedErr, rangeErr)
	if truncatedErr == nil || rangeErr == nil {
		t.Fatal("a route that is both too long and malformed was accepted")
	}
}

// TestEmptyStringRecordsResult records whether an empty polyline is a zero-point route or invalid.
func TestEmptyStringRecordsResult(t *testing.T) {
	// PROBE: the contract does not say whether a route may have zero points.
	points, err := polyline.Decode("")
	t.Logf("empty string: %d points, %v", len(points), err)
	if err == nil && len(points) != 0 {
		t.Fatalf("empty string decoded to %v", points)
	}
}

// TestOutOfRangeCoordinates refuses a coordinate one unit past each bound.
func TestOutOfRangeCoordinates(t *testing.T) {
	for _, test := range []struct {
		// name describes the point.
		name string

		// point is the single out-of-range point.
		point polyline.Point
	}{
		{name: "latitude just over 90", point: polyline.Point{Latitude: 90.00001}},
		{name: "latitude just under -90", point: polyline.Point{Latitude: -90.00001}},
		{name: "longitude just over 180", point: polyline.Point{Longitude: 180.00001}},
		{name: "longitude just under -180", point: polyline.Point{Longitude: -180.00001}},
		{name: "latitude far out", point: polyline.Point{Latitude: 1000}},
		{name: "longitude far out", point: polyline.Point{Longitude: -1000}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := polyline.Decode(encode([]polyline.Point{test.point})); !errors.Is(err, polyline.ErrInvalid) {
				t.Fatalf("got %v, want ErrInvalid", err)
			}
		})
	}
}

// TestDeltaLeavesRangeMidway refuses a route whose accumulated position leaves the globe between valid points.
func TestDeltaLeavesRangeMidway(t *testing.T) {
	route := encode([]polyline.Point{{89, 0}, {90.5, 0}, {89, 0}})
	if _, err := polyline.Decode(route); !errors.Is(err, polyline.ErrInvalid) {
		t.Fatalf("got %v, want ErrInvalid", err)
	}
}

// TestMalformedStrings refuses truncated chunks and characters outside the alphabet.
func TestMalformedStrings(t *testing.T) {
	valid := encode([]polyline.Point{{38.5, -120.2}})
	for _, test := range []struct {
		// name describes the corruption.
		name string

		// encoded is the malformed string.
		encoded string
	}{
		{name: "truncated final chunk", encoded: valid[:len(valid)-1]},
		{name: "only a latitude", encoded: "_p~iF"},
		{name: "lone continuation chunk", encoded: "`"},
		{name: "space inside", encoded: "_p~iF ~ps|U"},
		{name: "character below the alphabet", encoded: "_p~iF>ps|U"},
		{name: "DEL above the alphabet", encoded: "_p~iF\x7fps|U"},
		{name: "NUL", encoded: "\x00"},
		{name: "non-ASCII", encoded: "_p~iFé"},
		{name: "newline", encoded: valid + "\n"},
		{name: "overlong chunk run", encoded: strings.Repeat("\x7e", 20) + "?"},
		{name: "overflowing positive value", encoded: strings.Repeat("~", 7) + "O?"},
		{name: "overflowing latitude then valid longitude", encoded: strings.Repeat("~", 13) + "_?"},
	} {
		t.Run(test.name, func(t *testing.T) {
			points, err := polyline.Decode(test.encoded)
			if !errors.Is(err, polyline.ErrInvalid) {
				t.Fatalf("got %v points and %v, want ErrInvalid", points, err)
			}
		})
	}
}

// TestSingleChunkMinusOne decodes the chunk '@' (bits 00001) as -1, so -0.00001, per the inversion rule.
func TestSingleChunkMinusOne(t *testing.T) {
	points, err := polyline.Decode("@@")
	if err != nil || !samePoints(points, []polyline.Point{{-0.00001, -0.00001}}) {
		t.Fatalf("got %v, %v", points, err)
	}
}

// TestNegativeZeroEncodingRecordsResult records how the non-canonical 32-bit "negative zero" chunk run decodes.
func TestNegativeZeroEncodingRecordsResult(t *testing.T) {
	// PROBE: ~0 as a 32-bit shifted value is six chunks of ones then bits 11; the format never emits it, so refusing or reading 0 are both defensible.
	points, err := polyline.Decode("~~~~~~B~~~~~~B")
	t.Logf("negative zero: %v, %v", points, err)
	if err == nil && (len(points) != 1 || points[0].Latitude != 0 || points[0].Longitude != 0) {
		t.Fatalf("negative zero decoded to %v", points)
	}
}

// TestLongValidRouteFromTinySteps checks many small deltas do not drift from their exact sum.
func TestLongValidRouteFromTinySteps(t *testing.T) {
	points := make([]polyline.Point, polyline.MaximumPoints)
	for index := range points {
		points[index] = polyline.Point{Latitude: float64(index) / 1e5, Longitude: -float64(index) / 1e5}
	}
	got, err := polyline.Decode(encode(points))
	if err != nil {
		t.Fatal(err)
	}
	if !samePoints(got, points) {
		t.Fatal("decoded route drifted from its deltas")
	}
}

// FuzzDecodeProbe checks Decode never panics and that whatever it accepts round-trips through the encoder.
func FuzzDecodeProbe(f *testing.F) {
	f.Add("_p~iF~ps|U_ulLnnqC_mqNvxq`@")
	f.Add("")
	f.Add("@@")
	f.Add(strings.Repeat("~", 20))
	f.Add("?")
	f.Fuzz(func(t *testing.T, encoded string) {
		points, err := polyline.Decode(encoded)
		if err != nil {
			if !errors.Is(err, polyline.ErrInvalid) && !errors.Is(err, polyline.ErrTooManyPoints) {
				t.Fatalf("unexpected error %v", err)
			}
			return
		}
		if len(points) > polyline.MaximumPoints {
			t.Fatalf("accepted %d points", len(points))
		}
		for _, point := range points {
			if point.Latitude < -90 || point.Latitude > 90 || point.Longitude < -180 || point.Longitude > 180 {
				t.Fatalf("accepted out-of-range point %v", point)
			}
		}
		again, err := polyline.Decode(encode(points))
		if err != nil || !samePoints(again, points) {
			t.Fatalf("re-encoding %q gave %v, %v", encoded, again, err)
		}
	})
}
