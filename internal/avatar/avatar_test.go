package avatar

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
	"time"
)

// encodedPNG returns a translucent gradient PNG of the given size.
func encodedPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	source := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			source.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 100, A: 128})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, source); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

// TestScalesAndReencodes checks a large PNG comes back as a JPEG no bigger than MaximumSide.
func TestScalesAndReencodes(t *testing.T) {
	normalized, err := Normalize(encodedPNG(t, 1000, 500), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(normalized))
	if err != nil || format != "jpeg" || config.Width != MaximumSide || config.Height != MaximumSide/2 {
		t.Fatalf("came back as %s %dx%d, %v", format, config.Width, config.Height, err)
	}
}

// TestSmallImagesAreNotEnlarged keeps a small avatar at its own size.
func TestSmallImagesAreNotEnlarged(t *testing.T) {
	normalized, err := Normalize(encodedPNG(t, 64, 48), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	config, _ := jpeg.DecodeConfig(bytes.NewReader(normalized))
	if config.Width != 64 || config.Height != 48 {
		t.Fatalf("came back %dx%d", config.Width, config.Height)
	}
}

// TestRefusesMismatchAndGarbage refuses a PNG labelled as JPEG, and bytes that are no image.
func TestRefusesMismatchAndGarbage(t *testing.T) {
	if _, err := Normalize(encodedPNG(t, 10, 10), "image/jpeg"); err != ErrInvalid {
		t.Fatalf("mislabelled: %v", err)
	}
	if _, err := Normalize([]byte("not an image"), "image/png"); err != ErrInvalid {
		t.Fatalf("garbage: %v", err)
	}
}

// encoded16BitPNG returns an opaque 16-bit-a-channel PNG of the given size, the costliest to decode.
func encoded16BitPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	source := image.NewNRGBA64(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			source.SetNRGBA64(x, y, color.NRGBA64{R: uint16(x * 31), G: uint16(y * 31), B: 0x8000, A: 0xFFFF})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, source); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

// TestRefusesTooManyPixels refuses a maximum-side 16-bit PNG whose pixel count exceeds the decode bound.
func TestRefusesTooManyPixels(t *testing.T) {
	if _, err := Normalize(encoded16BitPNG(t, maximumSourceSide, maximumSourceSide), "image/png"); err != ErrInvalid {
		t.Fatalf("Normalize(%d×%d 16-bit PNG) = %v, want ErrInvalid", maximumSourceSide, maximumSourceSide, err)
	}
	if _, err := Normalize(encodedPNG(t, maximumSourceSide+1, 1), "image/png"); err != ErrInvalid {
		t.Fatalf("Normalize(%d×1 PNG) = %v, want ErrInvalid", maximumSourceSide+1, err)
	}
}

// TestDecodesTheLargestAllowedImage accepts a 16-bit PNG at the pixel bound and scales it to MaximumSide.
func TestDecodesTheLargestAllowedImage(t *testing.T) {
	width, height := maximumSourceSide, maximumSourcePixels/maximumSourceSide
	normalized, err := Normalize(encoded16BitPNG(t, width, height), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	config, _ := jpeg.DecodeConfig(bytes.NewReader(normalized))
	if config.Width != MaximumSide || config.Height != MaximumSide*height/width {
		t.Fatalf("got %dx%d, want %dx%d", config.Width, config.Height, MaximumSide, MaximumSide*height/width)
	}
}

// TestDecodesWaitForAFreeSlot proves Normalize waits while maximumConcurrentDecodes decodes are running.
func TestDecodesWaitForAFreeSlot(t *testing.T) {
	data := encodedPNG(t, 8, 8)
	for range maximumConcurrentDecodes {
		decodeSlots <- struct{}{}
	}
	done := make(chan error, 1)
	go func() {
		_, err := Normalize(data, "image/png")
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("Normalize decoded while every slot was taken")
	case <-time.After(50 * time.Millisecond):
	}
	for range maximumConcurrentDecodes {
		<-decodeSlots
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
