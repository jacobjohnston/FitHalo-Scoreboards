// Package polyline validates routes in the Encoded Polyline Algorithm Format.
package polyline

import (
	"errors"
	"math"
)

// MaximumPoints is the most points a route may have.
const MaximumPoints = 5000

// precision is the factor the format multiplies each coordinate by before rounding it.
const precision = 1e5

// Point is a latitude and longitude in degrees.
type Point struct {
	// Latitude is degrees north of the equator, from -90 to 90.
	Latitude float64

	// Longitude is degrees east of the prime meridian, from -180 to 180.
	Longitude float64
}

// ErrInvalid means the string is not a well-formed polyline of valid coordinates.
var ErrInvalid = errors.New("polyline: not a valid encoded polyline")

// ErrTooManyPoints means the route has more than MaximumPoints.
var ErrTooManyPoints = errors.New("polyline: too many points")

// Decode returns a polyline's points, refusing one that is malformed, leaves a coordinate out of range,
// or has more than MaximumPoints.
func Decode(encoded string) ([]Point, error) {
	var points []Point
	var latitude, longitude int64
	for index := 0; index < len(encoded); {
		latitudeChange, next, err := decodeValue(encoded, index)
		if err != nil {
			return nil, err
		}
		longitudeChange, next, err := decodeValue(encoded, next)
		if err != nil {
			return nil, err
		}
		index = next
		latitude += latitudeChange
		longitude += longitudeChange
		point := Point{Latitude: float64(latitude) / precision, Longitude: float64(longitude) / precision}
		if math.Abs(point.Latitude) > 90 || math.Abs(point.Longitude) > 180 {
			return nil, ErrInvalid
		}
		if len(points) == MaximumPoints {
			return nil, ErrTooManyPoints
		}
		points = append(points, point)
	}
	return points, nil
}

// decodeValue reads one signed value starting at index and returns it with the index after it.
func decodeValue(encoded string, index int) (int64, int, error) {
	var result int64
	for shift := uint(0); ; shift += 5 {
		if index >= len(encoded) || shift > 30 {
			return 0, 0, ErrInvalid
		}
		chunk := int64(encoded[index]) - 63
		index++
		if chunk < 0 || chunk > 63 {
			return 0, 0, ErrInvalid
		}
		result |= (chunk & 0x1f) << shift
		if chunk < 0x20 {
			break
		}
	}
	if result&1 != 0 {
		return ^(result >> 1), index, nil
	}
	return result >> 1, index, nil
}
