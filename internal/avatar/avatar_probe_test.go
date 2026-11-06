package avatar_test

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"sync"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/avatar"
)

// jpegMagic is the start-of-image marker every JPEG begins with.
var jpegMagic = []byte{0xFF, 0xD8, 0xFF}

// gradient returns an opaque NRGBA image of the given size whose pixels vary by position.
func gradient(width, height int) *image.NRGBA {
	picture := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			picture.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: uint8(x ^ y), A: 255})
		}
	}
	return picture
}

// solid returns an image of one NRGBA colour.
func solid(width, height int, fill color.NRGBA) *image.NRGBA {
	picture := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			picture.Set(x, y, fill)
		}
	}
	return picture
}

// encodePNG encodes any image as PNG.
func encodePNG(t *testing.T, picture image.Image) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := png.Encode(&output, picture); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

// encodeJPEG encodes any image as a JPEG at quality 90.
func encodeJPEG(t *testing.T, picture image.Image) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := jpeg.Encode(&output, picture, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

// decodeOutput decodes Normalize's output and asserts it is a JPEG.
func decodeOutput(t *testing.T, data []byte) image.Image {
	t.Helper()
	if !bytes.HasPrefix(data, jpegMagic) {
		t.Fatalf("output does not start with the JPEG marker: % X", data[:min(len(data), 4)])
	}
	picture, format, err := image.Decode(bytes.NewReader(data))
	if err != nil || format != "jpeg" {
		t.Fatalf("output decoded as %q, %v", format, err)
	}
	return picture
}

// TestOutputFitsMaximumSideKeepingAspect scales each shape so the longest side is MaximumSide.
func TestOutputFitsMaximumSideKeepingAspect(t *testing.T) {
	for _, test := range []struct {
		// name describes the input shape.
		name string

		// width and height are the input size.
		width, height int

		// wantWidth and wantHeight are the expected output size.
		wantWidth, wantHeight int
	}{
		{name: "wide 4:1", width: 1024, height: 256, wantWidth: 512, wantHeight: 128},
		{name: "tall 1:4", width: 256, height: 1024, wantWidth: 128, wantHeight: 512},
		{name: "square at the limit", width: 512, height: 512, wantWidth: 512, wantHeight: 512},
		{name: "square one over", width: 513, height: 513, wantWidth: 512, wantHeight: 512},
		{name: "square one under", width: 511, height: 511, wantWidth: 511, wantHeight: 511},
		{name: "large square", width: 1700, height: 1700, wantWidth: 512, wantHeight: 512},
		{name: "small stays small", width: 100, height: 50, wantWidth: 100, wantHeight: 50},
		{name: "one pixel", width: 1, height: 1, wantWidth: 1, wantHeight: 1},
		{name: "extreme strip", width: 2048, height: 1, wantWidth: 512, wantHeight: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, encoding := range []struct {
				// contentType is the declared type.
				contentType string

				// data is the encoded input.
				data []byte
			}{
				{"image/png", encodePNG(t, gradient(test.width, test.height))},
				{"image/jpeg", encodeJPEG(t, gradient(test.width, test.height))},
			} {
				output, err := avatar.Normalize(encoding.data, encoding.contentType)
				if err != nil {
					t.Fatalf("%s: %v", encoding.contentType, err)
				}
				bounds := decodeOutput(t, output).Bounds()
				if bounds.Dx() != test.wantWidth || bounds.Dy() != test.wantHeight {
					t.Errorf("%s: came back %dx%d, want %dx%d", encoding.contentType, bounds.Dx(), bounds.Dy(), test.wantWidth, test.wantHeight)
				}
				if bounds.Dx() > avatar.MaximumSide || bounds.Dy() > avatar.MaximumSide {
					t.Errorf("%s: %dx%d exceeds MaximumSide", encoding.contentType, bounds.Dx(), bounds.Dy())
				}
			}
		})
	}
}

// TestContentTypeMismatchRefused refuses bytes of one type declared as the other.
func TestContentTypeMismatchRefused(t *testing.T) {
	pngData := encodePNG(t, gradient(8, 8))
	jpegData := encodeJPEG(t, gradient(8, 8))
	if _, err := avatar.Normalize(pngData, "image/jpeg"); !errors.Is(err, avatar.ErrInvalid) {
		t.Errorf("PNG as image/jpeg: %v", err)
	}
	if _, err := avatar.Normalize(jpegData, "image/png"); !errors.Is(err, avatar.ErrInvalid) {
		t.Errorf("JPEG as image/png: %v", err)
	}
}

// TestUnsupportedContentTypes refuses every declared type that is not exactly image/png or image/jpeg.
func TestUnsupportedContentTypes(t *testing.T) {
	pngData := encodePNG(t, gradient(8, 8))
	jpegData := encodeJPEG(t, gradient(8, 8))
	for _, test := range []struct {
		// name describes the case.
		name string

		// contentType is the declared type.
		contentType string

		// data is the body sent with it.
		data []byte
	}{
		{name: "gif", contentType: "image/gif", data: pngData},
		{name: "webp", contentType: "image/webp", data: pngData},
		{name: "empty type", contentType: "", data: pngData},
		{name: "upper case PNG", contentType: "IMAGE/PNG", data: pngData},
		{name: "png with parameter", contentType: "image/png; charset=x", data: pngData},
		{name: "jpg alias", contentType: "image/jpg", data: jpegData},
		{name: "jpeg with parameter", contentType: "image/jpeg; q=1", data: jpegData},
		{name: "leading space", contentType: " image/png", data: pngData},
		{name: "svg", contentType: "image/svg+xml", data: []byte("<svg/>")},
	} {
		t.Run(test.name, func(t *testing.T) {
			// PROBE: the contract says only "JPEG or PNG"; record whether the error is ErrInvalid or another error.
			output, err := avatar.Normalize(test.data, test.contentType)
			t.Logf("%q: %v", test.contentType, err)
			if err == nil {
				t.Fatalf("accepted %q and produced %d bytes", test.contentType, len(output))
			}
		})
	}
}

// TestMalformedBodies refuses empty, tiny and truncated bodies with ErrInvalid.
func TestMalformedBodies(t *testing.T) {
	pngData := encodePNG(t, gradient(64, 64))
	jpegData := encodeJPEG(t, gradient(64, 64))
	for _, test := range []struct {
		// name describes the body.
		name string

		// contentType is the declared type.
		contentType string

		// data is the body.
		data []byte
	}{
		{name: "nil png", contentType: "image/png", data: nil},
		{name: "empty jpeg", contentType: "image/jpeg", data: []byte{}},
		{name: "one byte", contentType: "image/png", data: []byte{0x89}},
		{name: "png signature only", contentType: "image/png", data: pngData[:8]},
		{name: "half a png", contentType: "image/png", data: pngData[:len(pngData)/2]},
		{name: "half a jpeg", contentType: "image/jpeg", data: jpegData[:len(jpegData)/2]},
		{name: "png missing last byte", contentType: "image/png", data: pngData[:len(pngData)-1]},
		{name: "jpeg missing last byte", contentType: "image/jpeg", data: jpegData[:len(jpegData)-1]},
		{name: "jpeg header only", contentType: "image/jpeg", data: jpegData[:2]},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := avatar.Normalize(test.data, test.contentType)
			if !errors.Is(err, avatar.ErrInvalid) {
				t.Fatalf("got %d bytes and %v, want ErrInvalid", len(output), err)
			}
		})
	}
}

// TestPixelLimits applies the openapi Limits row: at most 2048 a side and 3,145,728 pixels in all.
func TestPixelLimits(t *testing.T) {
	for _, test := range []struct {
		// name describes the size.
		name string

		// width and height are the input size.
		width, height int

		// accepted says whether the contract admits it.
		accepted bool
	}{
		{name: "2048x1536 is exactly the pixel limit", width: 2048, height: 1536, accepted: true},
		{name: "1536x2048 is exactly the pixel limit", width: 1536, height: 2048, accepted: true},
		{name: "2048x1537 is one row over", width: 2048, height: 1537, accepted: false},
		{name: "2048x2048 is over the pixel limit", width: 2048, height: 2048, accepted: false},
		{name: "2049x1 is over the side limit", width: 2049, height: 1, accepted: false},
		{name: "1x2049 is over the side limit", width: 1, height: 2049, accepted: false},
		{name: "2048x1 is at the side limit", width: 2048, height: 1, accepted: true},
		{name: "1x2048 is at the side limit", width: 1, height: 2048, accepted: true},
		{name: "1773x1773 is just under the pixel limit", width: 1773, height: 1773, accepted: true},
		{name: "1774x1774 is just over the pixel limit", width: 1774, height: 1774, accepted: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, encoding := range []struct {
				// contentType is the declared type.
				contentType string

				// data is the encoded input.
				data []byte
			}{
				{"image/png", encodePNG(t, solid(test.width, test.height, color.NRGBA{R: 30, G: 60, B: 90, A: 255}))},
				{"image/jpeg", encodeJPEG(t, solid(test.width, test.height, color.NRGBA{R: 30, G: 60, B: 90, A: 255}))},
			} {
				output, err := avatar.Normalize(encoding.data, encoding.contentType)
				if test.accepted && err != nil {
					t.Errorf("%s %dx%d: refused with %v", encoding.contentType, test.width, test.height, err)
				}
				if !test.accepted && !errors.Is(err, avatar.ErrInvalid) {
					t.Errorf("%s %dx%d: got %d bytes and %v, want ErrInvalid", encoding.contentType, test.width, test.height, len(output), err)
				}
			}
		})
	}
}

// TestTransparencyCompositedOntoWhite turns fully transparent pixels white and half-alpha red pink.
func TestTransparencyCompositedOntoWhite(t *testing.T) {
	for _, test := range []struct {
		// name describes the input colour.
		name string

		// fill is the input pixel.
		fill color.NRGBA

		// wantR, wantG and wantB are the expected output channels.
		wantR, wantG, wantB uint8
	}{
		{name: "fully transparent black", fill: color.NRGBA{A: 0}, wantR: 255, wantG: 255, wantB: 255},
		{name: "fully transparent red", fill: color.NRGBA{R: 255, A: 0}, wantR: 255, wantG: 255, wantB: 255},
		{name: "half transparent red", fill: color.NRGBA{R: 255, A: 128}, wantR: 255, wantG: 127, wantB: 127},
		{name: "opaque blue", fill: color.NRGBA{B: 255, A: 255}, wantR: 0, wantG: 0, wantB: 255},
		{name: "half transparent black", fill: color.NRGBA{A: 128}, wantR: 127, wantG: 127, wantB: 127},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := avatar.Normalize(encodePNG(t, solid(16, 16, test.fill)), "image/png")
			if err != nil {
				t.Fatal(err)
			}
			r, g, b, _ := decodeOutput(t, output).At(8, 8).RGBA()
			gotR, gotG, gotB := uint8(r>>8), uint8(g>>8), uint8(b>>8)
			if diff(gotR, test.wantR) > 6 || diff(gotG, test.wantG) > 6 || diff(gotB, test.wantB) > 6 {
				t.Fatalf("centre pixel (%d,%d,%d), want about (%d,%d,%d)", gotR, gotG, gotB, test.wantR, test.wantG, test.wantB)
			}
		})
	}
}

// diff returns the distance between two channel values.
func diff(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

// TestPixelsSurvive checks an opaque image's colour comes through roughly intact.
func TestPixelsSurvive(t *testing.T) {
	output, err := avatar.Normalize(encodeJPEG(t, solid(32, 32, color.NRGBA{R: 200, G: 40, B: 90, A: 255})), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := decodeOutput(t, output).At(16, 16).RGBA()
	if diff(uint8(r>>8), 200) > 8 || diff(uint8(g>>8), 40) > 8 || diff(uint8(b>>8), 90) > 8 {
		t.Fatalf("centre pixel (%d,%d,%d), want about (200,40,90)", r>>8, g>>8, b>>8)
	}
}

// exifSegment is an APP1 segment carrying an Exif header and a recognisable payload.
func exifSegment() []byte {
	payload := []byte("Exif\x00\x00SECRET-GPS-LOCATION-PAYLOAD")
	length := len(payload) + 2
	return append([]byte{0xFF, 0xE1, byte(length >> 8), byte(length)}, payload...)
}

// TestMetadataDropped checks an APP1 Exif segment spliced into the input does not reach the output.
func TestMetadataDropped(t *testing.T) {
	jpegData := encodeJPEG(t, gradient(40, 40))
	withExif := append(append(append([]byte{}, jpegData[:2]...), exifSegment()...), jpegData[2:]...)
	if _, _, err := image.Decode(bytes.NewReader(withExif)); err != nil {
		t.Fatalf("test image with APP1 does not decode: %v", err)
	}
	output, err := avatar.Normalize(withExif, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output, []byte("Exif")) || bytes.Contains(output, []byte("SECRET-GPS")) {
		t.Fatal("the Exif segment survived normalisation")
	}
	if bytes.Contains(output, []byte{0xFF, 0xE1}) {
		t.Log("output contains an FF E1 byte pair; may be entropy-coded data, not necessarily APP1")
	}
}

// TestPNGTextChunksDropped checks a PNG tEXt chunk spliced after IHDR does not reach the output.
func TestPNGTextChunksDropped(t *testing.T) {
	pngData := encodePNG(t, gradient(40, 40))
	// IHDR is the first chunk: 8 signature + 4 length + 4 type + 13 data + 4 crc = 33 bytes.
	keyword := []byte("Comment\x00SECRET-TEXT-CHUNK")
	chunk := []byte{0, 0, 0, byte(len(keyword)), 't', 'E', 'X', 't'}
	chunk = append(chunk, keyword...)
	chunk = append(chunk, crc32Of(append([]byte("tEXt"), keyword...))...)
	withText := append(append(append([]byte{}, pngData[:33]...), chunk...), pngData[33:]...)
	if _, _, err := image.Decode(bytes.NewReader(withText)); err != nil {
		t.Fatalf("test image with tEXt does not decode: %v", err)
	}
	output, err := avatar.Normalize(withText, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output, []byte("SECRET-TEXT")) {
		t.Fatal("the tEXt chunk survived normalisation")
	}
}

// TestPNGColourModels accepts 16-bit, paletted and grayscale PNGs.
func TestPNGColourModels(t *testing.T) {
	sixteen := image.NewNRGBA64(image.Rect(0, 0, 20, 20))
	for y := range 20 {
		for x := range 20 {
			sixteen.SetNRGBA64(x, y, color.NRGBA64{R: 0xffff, G: 0x8000, B: 0, A: 0xffff})
		}
	}
	paletted := image.NewPaletted(image.Rect(0, 0, 20, 20), color.Palette{color.RGBA{0, 0, 255, 255}, color.RGBA{255, 0, 0, 255}})
	for y := range 20 {
		for x := range 20 {
			paletted.SetColorIndex(x, y, uint8((x+y)%2))
		}
	}
	gray := image.NewGray(image.Rect(0, 0, 20, 20))
	for y := range 20 {
		for x := range 20 {
			gray.SetGray(x, y, color.Gray{Y: 100})
		}
	}
	gray16 := image.NewGray16(image.Rect(0, 0, 20, 20))
	alphaPaletted := image.NewPaletted(image.Rect(0, 0, 20, 20), color.Palette{color.NRGBA{255, 0, 0, 0}})
	for _, test := range []struct {
		// name describes the colour model.
		name string

		// picture is the input image.
		picture image.Image

		// wantR, wantG and wantB are the expected centre channels, or -1 to skip the check.
		wantR, wantG, wantB int
	}{
		{name: "16-bit NRGBA", picture: sixteen, wantR: 255, wantG: 128, wantB: 0},
		{name: "paletted", picture: paletted, wantR: -1},
		{name: "grayscale", picture: gray, wantR: 100, wantG: 100, wantB: 100},
		{name: "16-bit grayscale", picture: gray16, wantR: 0, wantG: 0, wantB: 0},
		{name: "paletted with transparent entry", picture: alphaPaletted, wantR: 255, wantG: 255, wantB: 255},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := avatar.Normalize(encodePNG(t, test.picture), "image/png")
			if err != nil {
				t.Fatal(err)
			}
			decoded := decodeOutput(t, output)
			if test.wantR < 0 {
				return
			}
			r, g, b, _ := decoded.At(10, 10).RGBA()
			if diff(uint8(r>>8), uint8(test.wantR)) > 6 || diff(uint8(g>>8), uint8(test.wantG)) > 6 || diff(uint8(b>>8), uint8(test.wantB)) > 6 {
				t.Fatalf("centre pixel (%d,%d,%d), want about (%d,%d,%d)", r>>8, g>>8, b>>8, test.wantR, test.wantG, test.wantB)
			}
		})
	}
}

// TestDeterministic checks the same input gives the same bytes every time.
func TestDeterministic(t *testing.T) {
	input := encodePNG(t, gradient(300, 200))
	first, err := avatar.Normalize(input, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		again, err := avatar.Normalize(input, "image/png")
		if err != nil || !bytes.Equal(first, again) {
			t.Fatal("Normalize is not deterministic")
		}
	}
}

// TestInputNotModified checks Normalize leaves the caller's bytes alone.
func TestInputNotModified(t *testing.T) {
	input := encodeJPEG(t, gradient(100, 100))
	copied := append([]byte{}, input...)
	if _, err := avatar.Normalize(input, "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(input, copied) {
		t.Fatal("Normalize modified its input")
	}
}

// TestOutputSizeIsBounded records the output size for a noisy maximum-size image and checks it stays under 256 KB.
func TestOutputSizeIsBounded(t *testing.T) {
	noise := image.NewNRGBA(image.Rect(0, 0, 2048, 1536))
	seed := uint32(12345)
	for index := range noise.Pix {
		seed = seed*1664525 + 1013904223
		noise.Pix[index] = byte(seed >> 24)
	}
	output, err := avatar.Normalize(encodePNG(t, noise), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	// PROBE: the Limits row bounds the upload at 256 KB; the served JPEG has no written bound, so this records it.
	t.Logf("512x384 noise came out as %d bytes", len(output))
	if len(output) > 256*1024 {
		t.Fatalf("output is %d bytes, over the 256 KB upload limit", len(output))
	}
}

// TestConcurrentNormalizeCompletes runs eight maximum-size decodes at once and expects all to finish.
func TestConcurrentNormalizeCompletes(t *testing.T) {
	input := encodePNG(t, gradient(2048, 1536))
	var group sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := avatar.Normalize(input, "image/png"); err != nil {
				failures <- err
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("eight concurrent Normalize calls did not finish in 30 s")
	}
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}
