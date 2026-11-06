package qr

import (
	"bytes"
	"strings"
	"testing"
)

// TestReedSolomonWorkedExample checks the error correction for the well-known 1-M "HELLO WORLD"
// example's data codewords.
func TestReedSolomonWorkedExample(t *testing.T) {
	data := []byte{0x20, 0x5B, 0x0B, 0x78, 0xD1, 0x72, 0xDC, 0x4D, 0x43, 0x40, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11}
	expected := []byte{0xC4, 0x23, 0x27, 0x77, 0xEB, 0xD7, 0xE7, 0xE2, 0x5D, 0x17}
	if correction := reedSolomon(data, 10); !bytes.Equal(correction, expected) {
		t.Fatalf("error correction % X", correction)
	}
}

// TestVersionsFitTheirTables checks every version's blocks add up to its size's codeword count.
func TestVersionsFitTheirTables(t *testing.T) {
	for version := 1; version <= maximumVersion; version++ {
		specification := levelM[version]
		blocks := specification.groupOneBlocks + specification.groupTwoBlocks
		total := specification.groupOneBlocks*specification.groupOne + specification.groupTwoBlocks*specification.groupTwo + blocks*specification.ecc
		size := 4*version + 17
		grid := newMatrix(size)
		grid.drawFunctionPatterns(version)
		available := 0
		for y := range size {
			for x := range size {
				if !grid.isFunction[y][x] {
					available++
				}
			}
		}
		if available/8 != total {
			t.Errorf("version %d: %d codewords in the table, room for %d", version, total, available/8)
		}
	}
}

// TestPicksTheSmallestVersion draws five bytes as version 1, refuses 413 bytes, and escapes the title.
func TestPicksTheSmallestVersion(t *testing.T) {
	small, err := Encode([]byte("hello"))
	if err != nil || small.Size != 21 {
		t.Fatalf("five bytes drew a %d-module code, %v", small.Size, err)
	}
	if _, err := Encode(bytes.Repeat([]byte("x"), 413)); err != ErrTooLong {
		t.Fatalf("413 bytes: %v", err)
	}
	if svg := small.SVG(`An "invite"`); !strings.HasPrefix(svg, "<svg") || strings.Contains(svg, `"invite"`) {
		t.Fatal("the SVG did not escape its title")
	}
}
