package api_test

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
)

// boardDocument is the stored board, named for configure callbacks.
type boardDocument = scoreboard.Board

// jsonMarshal encodes value as JSON.
func jsonMarshal(value any) ([]byte, error) { return json.Marshal(value) }

// pngBytes encodes a solid image of the given size as PNG.
func pngBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			picture.Set(x, y, color.RGBA{R: 200, G: 30, B: 30, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, picture); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// jpegBytes encodes a solid image of the given size as JPEG.
func jpegBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			picture.Set(x, y, color.RGBA{R: 30, G: 30, B: 200, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, picture, nil); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// timeSeconds converts whole seconds to a duration.
func timeSeconds(n int) time.Duration { return time.Duration(n) * time.Second }
