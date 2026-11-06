// Package avatar re-encodes uploaded avatars, which drops their metadata and bounds their size.
package avatar

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png" // Registers PNG with image.Decode.
	"io"
)

// MaximumSide is the longest side of a stored avatar, in pixels.
const MaximumSide = 512

// maximumSourceSide is the longest side of an upload worth decoding.
const maximumSourceSide = 2048

// maximumSourcePixels is the most pixels an upload may have, which bounds a decode to about 24 MiB
// even at 16 bits a channel.
const maximumSourcePixels = 2048 * 1536

// maximumConcurrentDecodes is how many uploads Normalize decodes at once, so a burst cannot pile up heap.
const maximumConcurrentDecodes = 2

// jpegQuality is the quality stored avatars are encoded at.
const jpegQuality = 85

// ErrInvalid means the upload is not an image of the type it claims, or is too large to decode.
var ErrInvalid = errors.New("avatar: not a valid image")

// decodeSlots holds a token for each decode in progress, up to maximumConcurrentDecodes.
var decodeSlots = make(chan struct{}, maximumConcurrentDecodes)

// Normalize decodes a JPEG or PNG, composites any transparency onto white, scales it to fit
// MaximumSide, and encodes it as JPEG. Nothing from the original file survives but its pixels.
// At most maximumConcurrentDecodes calls decode at once; the rest wait.
func Normalize(data []byte, contentType string) ([]byte, error) {
	return NormalizeFrom(bytes.NewReader(data), contentType)
}

// NormalizeFrom is Normalize reading the upload from source, so an upload already held elsewhere is not copied.
func NormalizeFrom(source io.ReadSeeker, contentType string) ([]byte, error) {
	expectedFormat := map[string]string{"image/jpeg": "jpeg", "image/png": "png"}[contentType]
	config, format, err := image.DecodeConfig(source)
	if err != nil || format != expectedFormat {
		return nil, ErrInvalid
	}
	if config.Width < 1 || config.Height < 1 || config.Width > maximumSourceSide || config.Height > maximumSourceSide ||
		config.Width*config.Height > maximumSourcePixels {
		return nil, ErrInvalid
	}
	decodeSlots <- struct{}{}
	defer func() { <-decodeSlots }()
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	decoded, _, err := image.Decode(source)
	if err != nil {
		return nil, ErrInvalid
	}

	bounds := decoded.Bounds()
	flattened := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(flattened, flattened.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(flattened, flattened.Bounds(), decoded, bounds.Min, draw.Over)

	var output bytes.Buffer
	if err := jpeg.Encode(&output, scaleToFit(flattened, MaximumSide), &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// scaleToFit shrinks an image so its longer side is at most side, averaging every source pixel that
// falls in each destination pixel. It never enlarges.
func scaleToFit(source *image.RGBA, side int) *image.RGBA {
	width, height := source.Bounds().Dx(), source.Bounds().Dy()
	if width <= side && height <= side {
		return source
	}
	scaledWidth, scaledHeight := side, side
	if width > height {
		scaledHeight = max(1, height*side/width)
	} else {
		scaledWidth = max(1, width*side/height)
	}

	scaled := image.NewRGBA(image.Rect(0, 0, scaledWidth, scaledHeight))
	for scaledY := range scaledHeight {
		top, bottom := scaledY*height/scaledHeight, max((scaledY+1)*height/scaledHeight, scaledY*height/scaledHeight+1)
		for scaledX := range scaledWidth {
			left, right := scaledX*width/scaledWidth, max((scaledX+1)*width/scaledWidth, scaledX*width/scaledWidth+1)
			var red, green, blue, count int
			for sourceY := top; sourceY < bottom; sourceY++ {
				row := source.Pix[sourceY*source.Stride:]
				for sourceX := left; sourceX < right; sourceX++ {
					pixel := row[sourceX*4:]
					red += int(pixel[0])
					green += int(pixel[1])
					blue += int(pixel[2])
					count++
				}
			}
			offset := scaledY*scaled.Stride + scaledX*4
			scaled.Pix[offset] = uint8(red / count)
			scaled.Pix[offset+1] = uint8(green / count)
			scaled.Pix[offset+2] = uint8(blue / count)
			scaled.Pix[offset+3] = 255
		}
	}
	return scaled
}
