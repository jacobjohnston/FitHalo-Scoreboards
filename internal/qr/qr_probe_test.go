package qr_test

import (
	"bytes"
	"encoding/xml"
	"errors"
	"strings"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/qr"
)

// formatMask is the XOR mask ISO/IEC 18004 applies to the 15 format bits.
const formatMask = 0x5412

// byteCapacityM is the byte-mode capacity of each version at level M, from the standard's table 7.
var byteCapacityM = map[int]int{1: 14, 2: 26, 3: 42, 4: 62, 5: 84, 6: 106, 7: 122, 8: 152, 9: 180, 10: 213, 11: 251, 12: 287, 13: 331, 14: 362, 15: 412}

// blocksM describes level M's Reed-Solomon blocks: data codewords per block, how many blocks, and EC codewords per block.
var blocksM = map[int]struct {
	// ecc is the error correction codewords in every block.
	ecc int

	// blocks lists each block's data codeword count in order.
	blocks []int
}{
	1: {10, []int{16}}, 2: {16, []int{28}}, 3: {26, []int{44}}, 4: {18, []int{32, 32}}, 5: {24, []int{43, 43}},
	6: {16, []int{27, 27, 27, 27}}, 7: {18, []int{31, 31, 31, 31}}, 8: {22, []int{38, 38, 39, 39}},
	9: {22, []int{36, 36, 36, 37, 37}}, 10: {26, []int{43, 43, 43, 43, 44}}, 11: {30, []int{50, 51, 51, 51, 51}},
	12: {22, []int{36, 36, 36, 36, 36, 36, 37, 37}}, 13: {22, []int{37, 37, 37, 37, 37, 37, 37, 37, 38}},
	14: {24, []int{40, 40, 40, 40, 41, 41, 41, 41, 41}}, 15: {24, []int{41, 41, 41, 41, 41, 42, 42, 42, 42, 42}},
}

// bytesOf returns count bytes that vary by position.
func bytesOf(count int) []byte {
	data := make([]byte, count)
	for index := range data {
		data[index] = byte(index*7 + 3)
	}
	return data
}

// versionOf derives the version from a code's size.
func versionOf(code *qr.Code) int {
	return (code.Size - 17) / 4
}

// TestSizeIsSeventeenPlusFourTimesVersion checks every produced size is a legal version size.
func TestSizeIsSeventeenPlusFourTimesVersion(t *testing.T) {
	for count := 0; count <= 412; count++ {
		code, err := qr.Encode(bytesOf(count))
		if err != nil {
			t.Fatalf("%d bytes: %v", count, err)
		}
		if (code.Size-17)%4 != 0 || code.Size < 21 || code.Size > 77 || code.Size%2 == 0 {
			t.Fatalf("%d bytes: size %d is not 17+4V for V in 1..15", count, code.Size)
		}
	}
}

// TestSmallestVersionChosen checks each capacity boundary from table 7 picks the smallest version.
func TestSmallestVersionChosen(t *testing.T) {
	for version := 1; version <= 15; version++ {
		capacity := byteCapacityM[version]
		code, err := qr.Encode(bytesOf(capacity))
		if err != nil {
			t.Fatalf("%d bytes: %v", capacity, err)
		}
		if versionOf(code) != version {
			t.Errorf("%d bytes: version %d (size %d), want version %d", capacity, versionOf(code), code.Size, version)
		}
		if version == 15 {
			continue
		}
		code, err = qr.Encode(bytesOf(capacity + 1))
		if err != nil {
			t.Fatalf("%d bytes: %v", capacity+1, err)
		}
		if versionOf(code) != version+1 {
			t.Errorf("%d bytes: version %d, want version %d", capacity+1, versionOf(code), version+1)
		}
	}
}

// TestTooLong accepts 412 bytes at size 77 and refuses 413.
func TestTooLong(t *testing.T) {
	code, err := qr.Encode(bytesOf(412))
	if err != nil || code.Size != 77 {
		t.Fatalf("412 bytes: size %v, %v", code, err)
	}
	for _, count := range []int{413, 414, 1000, 1 << 16} {
		if _, err := qr.Encode(bytesOf(count)); !errors.Is(err, qr.ErrTooLong) {
			t.Errorf("%d bytes: %v, want ErrTooLong", count, err)
		}
	}
}

// TestEmptyDataRecordsResult records whether zero bytes encode.
func TestEmptyDataRecordsResult(t *testing.T) {
	// PROBE: the standard permits an empty byte segment; the package doc does not say whether it is refused.
	code, err := qr.Encode(nil)
	t.Logf("empty data: %v", err)
	if err == nil && code.Size != 21 {
		t.Fatalf("empty data gave size %d", code.Size)
	}
}

// TestFinderPatterns checks all three finders are dark-light-dark with light separators.
func TestFinderPatterns(t *testing.T) {
	for _, count := range []int{1, 14, 15, 100, 412} {
		code, err := qr.Encode(bytesOf(count))
		if err != nil {
			t.Fatal(err)
		}
		size := code.Size
		for _, origin := range [][2]int{{0, 0}, {size - 7, 0}, {0, size - 7}} {
			for dy := -1; dy <= 7; dy++ {
				for dx := -1; dx <= 7; dx++ {
					x, y := origin[0]+dx, origin[1]+dy
					if x < 0 || y < 0 || x >= size || y >= size {
						continue
					}
					want := finderModule(dx, dy)
					if code.Dark(x, y) != want {
						t.Fatalf("%d bytes: finder at %v module (%d,%d) dark=%v, want %v", count, origin, x, y, code.Dark(x, y), want)
					}
				}
			}
		}
	}
}

// finderModule returns the expected darkness of a finder's module at offsets dx, dy, with -1 and 7 being the separator.
func finderModule(dx, dy int) bool {
	if dx < 0 || dy < 0 || dx > 6 || dy > 6 {
		return false
	}
	if dx == 0 || dy == 0 || dx == 6 || dy == 6 {
		return true
	}
	if dx == 1 || dy == 1 || dx == 5 || dy == 5 {
		return false
	}
	return true
}

// TestTimingPatterns checks row 6 and column 6 alternate between the finders, dark on even indices.
func TestTimingPatterns(t *testing.T) {
	for _, count := range []int{1, 50, 412} {
		code, err := qr.Encode(bytesOf(count))
		if err != nil {
			t.Fatal(err)
		}
		for index := 8; index <= code.Size-9; index++ {
			if code.Dark(index, 6) != (index%2 == 0) {
				t.Errorf("%d bytes: timing row module %d dark=%v", count, index, code.Dark(index, 6))
			}
			if code.Dark(6, index) != (index%2 == 0) {
				t.Errorf("%d bytes: timing column module %d dark=%v", count, index, code.Dark(6, index))
			}
		}
	}
}

// TestDarkModule checks the always-dark module at column 8, row 4V+9.
func TestDarkModule(t *testing.T) {
	for count := 0; count <= 412; count += 13 {
		code, err := qr.Encode(bytesOf(count))
		if err != nil {
			t.Fatal(err)
		}
		if !code.Dark(8, code.Size-8) {
			t.Errorf("%d bytes: module (8,%d) is light", count, code.Size-8)
		}
	}
}

// alignmentCentres returns the alignment pattern centre coordinates for a version, as the standard's table E.1 gives.
func alignmentCentres(version, size int) []int {
	if version == 1 {
		return nil
	}
	count := version/7 + 2
	step := (version*4 + count*2 + 1) / (count*2 - 2) * 2
	centres := []int{6}
	for index := count - 1; index >= 1; index-- {
		centres = append(centres, size-7-(index-1)*step)
	}
	return centres
}

// TestAlignmentPatterns checks each alignment pattern is a 5×5 dark-light-dark square at the table positions.
func TestAlignmentPatterns(t *testing.T) {
	for version := 2; version <= 15; version++ {
		code, err := qr.Encode(bytesOf(byteCapacityM[version]))
		if err != nil {
			t.Fatal(err)
		}
		centres := alignmentCentres(version, code.Size)
		for _, cx := range centres {
			for _, cy := range centres {
				if (cx == 6 && cy == 6) || (cx == 6 && cy == code.Size-7) || (cx == code.Size-7 && cy == 6) {
					continue
				}
				for dy := -2; dy <= 2; dy++ {
					for dx := -2; dx <= 2; dx++ {
						want := max(abs(dx), abs(dy)) != 1
						if code.Dark(cx+dx, cy+dy) != want {
							t.Fatalf("version %d: alignment at (%d,%d) module (%d,%d) dark=%v, want %v", version, cx, cy, cx+dx, cy+dy, code.Dark(cx+dx, cy+dy), want)
						}
					}
				}
			}
		}
	}
}

// abs returns the magnitude of an int.
func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// bit reads bit index of value.
func bit(value, index int) bool {
	return (value>>index)&1 == 1
}

// readFormat reads both copies of the 15 format bits and returns them raw.
func readFormat(code *qr.Code) (first, second int) {
	size := code.Size
	set := func(target *int, index int, dark bool) {
		if dark {
			*target |= 1 << index
		}
	}
	for index := range 6 {
		set(&first, index, code.Dark(8, index))
	}
	set(&first, 6, code.Dark(8, 7))
	set(&first, 7, code.Dark(8, 8))
	set(&first, 8, code.Dark(7, 8))
	for index := 9; index < 15; index++ {
		set(&first, index, code.Dark(14-index, 8))
	}
	for index := range 8 {
		set(&second, index, code.Dark(size-1-index, 8))
	}
	for index := 8; index < 15; index++ {
		set(&second, index, code.Dark(8, size-15+index))
	}
	return first, second
}

// bchFormat returns the 15-bit format word for five data bits using generator 0x537.
func bchFormat(data int) int {
	remainder := data
	for range 10 {
		remainder = (remainder << 1) ^ ((remainder >> 9) * 0x537)
	}
	return data<<10 | remainder
}

// formatInfo returns the mask pattern of a code after checking its format information.
func formatInfo(t *testing.T, code *qr.Code) int {
	t.Helper()
	first, second := readFormat(code)
	if first != second {
		t.Fatalf("format copies differ: %015b and %015b", first, second)
	}
	unmasked := first ^ formatMask
	data := unmasked >> 10
	if bchFormat(data) != unmasked {
		t.Fatalf("format %015b fails its BCH check", unmasked)
	}
	if data>>3 != 0 {
		t.Fatalf("error correction level bits %02b, want 00 for M", data>>3)
	}
	return data & 7
}

// TestFormatInformation checks both copies agree, the BCH code holds and the level is M for every version.
func TestFormatInformation(t *testing.T) {
	for count := 0; count <= 412; count += 7 {
		code, err := qr.Encode(bytesOf(count))
		if err != nil {
			t.Fatal(err)
		}
		formatInfo(t, code)
	}
}

// TestVersionInformation checks versions 7 and up carry their version in both 18-bit copies with a valid BCH code.
func TestVersionInformation(t *testing.T) {
	for version := 7; version <= 15; version++ {
		code, err := qr.Encode(bytesOf(byteCapacityM[version]))
		if err != nil {
			t.Fatal(err)
		}
		remainder := version
		for range 12 {
			remainder = (remainder << 1) ^ ((remainder >> 11) * 0x1F25)
		}
		want := version<<12 | remainder
		for index := range 18 {
			a, b := code.Size-11+index%3, index/3
			if code.Dark(a, b) != bit(want, index) || code.Dark(b, a) != bit(want, index) {
				t.Fatalf("version %d: version information bit %d wrong", version, index)
			}
		}
	}
}

// TestDarkRatio checks the chosen mask leaves between 40 and 60 percent of modules dark.
func TestDarkRatio(t *testing.T) {
	for _, data := range [][]byte{{}, {0}, bytes.Repeat([]byte{0}, 100), bytes.Repeat([]byte{0xff}, 200), bytesOf(412), []byte("https://example.com/join?code=ABCDEFGH")} {
		code, err := qr.Encode(data)
		if err != nil {
			t.Fatal(err)
		}
		dark := 0
		for y := range code.Size {
			for x := range code.Size {
				if code.Dark(x, y) {
					dark++
				}
			}
		}
		ratio := float64(dark) / float64(code.Size*code.Size)
		if ratio < 0.4 || ratio > 0.6 {
			t.Errorf("%d bytes: %.1f%% dark", len(data), ratio*100)
		}
	}
}

// TestDeterministic encodes the same data twice and compares every module and the SVG.
func TestDeterministic(t *testing.T) {
	data := []byte("https://example.com/join?code=ABCDEFGH")
	first, _ := qr.Encode(data)
	second, _ := qr.Encode(data)
	if first.Size != second.Size || first.SVG("t") != second.SVG("t") {
		t.Fatal("two encodings differ")
	}
	for y := range first.Size {
		for x := range first.Size {
			if first.Dark(x, y) != second.Dark(x, y) {
				t.Fatalf("module (%d,%d) differs", x, y)
			}
		}
	}
}

// TestEncodeDoesNotAliasInput checks mutating the input afterwards does not change the code.
func TestEncodeDoesNotAliasInput(t *testing.T) {
	data := []byte("alias check")
	code, _ := qr.Encode(data)
	before := code.SVG("t")
	for index := range data {
		data[index] = 'z'
	}
	if code.SVG("t") != before {
		t.Fatal("the code changed when its input did")
	}
}

// TestDarkOutOfRange checks Dark outside the grid reports light rather than panicking.
func TestDarkOutOfRange(t *testing.T) {
	code, _ := qr.Encode([]byte("x"))
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("Dark panicked out of range: %v", recovered)
		}
	}()
	for _, point := range [][2]int{{-1, 0}, {0, -1}, {code.Size, 0}, {0, code.Size}, {-1, -1}, {1 << 30, 1 << 30}, {-1 << 30, 5}} {
		if code.Dark(point[0], point[1]) {
			// PROBE: the doc does not define Dark outside the grid; treating it as light is the natural reading.
			t.Errorf("Dark%v reports dark outside the grid", point)
		}
	}
}

// svgDocument is the subset of an SVG element the tests inspect.
type svgDocument struct {
	// ViewBox is the root viewBox attribute.
	ViewBox string `xml:"viewBox,attr"`

	// Title is the accessible name carried in aria-label.
	Title string `xml:"aria-label,attr"`
}

// TestSVGIsWellFormedWithQuietZone parses the SVG, checks the viewBox spans Size+8 and the title is escaped.
func TestSVGIsWellFormedWithQuietZone(t *testing.T) {
	for _, count := range []int{1, 14, 15, 412} {
		code, err := qr.Encode(bytesOf(count))
		if err != nil {
			t.Fatal(err)
		}
		title := `Join <b>&"team"</b> 'now' ]]> ü`
		svg := code.SVG(title)
		var document svgDocument
		if err := xml.Unmarshal([]byte(svg), &document); err != nil {
			t.Fatalf("%d bytes: SVG does not parse: %v\n%s", count, err, svg)
		}
		if document.Title != title {
			t.Errorf("%d bytes: title %q, want %q", count, document.Title, title)
		}
		fields := strings.Fields(document.ViewBox)
		want := []string{"0", "0", itoa(code.Size + 8), itoa(code.Size + 8)}
		if len(fields) != 4 || fields[2] != want[2] || fields[3] != want[3] {
			t.Errorf("%d bytes: viewBox %q, want %v", count, document.ViewBox, want)
		}
		if strings.Contains(svg, "<b>") {
			t.Errorf("%d bytes: raw title markup reached the SVG", count)
		}
		if !strings.HasPrefix(strings.TrimSpace(svg), "<svg") {
			t.Logf("%d bytes: SVG starts with %q", count, svg[:min(len(svg), 20)])
		}
	}
}

// itoa formats a small non-negative int.
func itoa(value int) string {
	return strings.TrimSpace(strings.Repeat(" ", 0) + string(appendInt(nil, value)))
}

// appendInt appends value's decimal digits.
func appendInt(buffer []byte, value int) []byte {
	if value >= 10 {
		buffer = appendInt(buffer, value/10)
	}
	return append(buffer, byte('0'+value%10))
}

// TestSVGEmptyTitle checks an empty title still gives well-formed SVG.
func TestSVGEmptyTitle(t *testing.T) {
	code, _ := qr.Encode([]byte("x"))
	var document svgDocument
	if err := xml.Unmarshal([]byte(code.SVG("")), &document); err != nil {
		t.Fatal(err)
	}
}

// TestSVGTitleWithControlCharacters records whether a title with characters XML forbids still yields a parseable document.
func TestSVGTitleWithControlCharacters(t *testing.T) {
	code, _ := qr.Encode([]byte("x"))
	// PROBE: U+0000 and U+0001 cannot appear in XML 1.0 at all, even escaped; the doc does not say whether SVG strips them.
	svg := code.SVG("bad\x00\x01\x7fchars")
	var document svgDocument
	if err := xml.Unmarshal([]byte(svg), &document); err != nil {
		t.Skipf("SVG with control characters in the title does not parse as XML: %v", err)
	}
	t.Logf("title came back as %q", document.Title)
}
