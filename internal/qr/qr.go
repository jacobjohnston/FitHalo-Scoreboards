// Package qr draws QR codes (ISO/IEC 18004) for invites: byte mode, error correction level M, versions
// 1 to 15, which holds up to 412 bytes, far more than an invite needs.
package qr

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a drawn QR code: size × size modules, true for dark.
type Code struct {
	// Size is the modules along each side, quiet zone excluded.
	Size int

	// modules holds each row's modules, top to bottom.
	modules [][]bool
}

// ErrTooLong means the data does not fit in the largest version this package draws.
var ErrTooLong = errors.New("qr: too much data")

// A block describes one version's error correction blocks at level M.
type block struct {
	// ecc is the error correction codewords in each block.
	ecc int

	// groupOneBlocks is the blocks in the first group.
	groupOneBlocks int

	// groupOne is the data codewords in each block of the first group.
	groupOne int

	// groupTwoBlocks is the blocks in the second group, zero when there is none.
	groupTwoBlocks int

	// groupTwo is the data codewords in each block of the second group.
	groupTwo int

	// alignmentPatternPositions is the row and column centres of the alignment patterns.
	alignmentPatternPositions []int
}

// levelM is ISO/IEC 18004 table 9 for level M, with table E.1's alignment pattern centres.
var levelM = [...]block{
	1:  {10, 1, 16, 0, 0, nil},
	2:  {16, 1, 28, 0, 0, []int{6, 18}},
	3:  {26, 1, 44, 0, 0, []int{6, 22}},
	4:  {18, 2, 32, 0, 0, []int{6, 26}},
	5:  {24, 2, 43, 0, 0, []int{6, 30}},
	6:  {16, 4, 27, 0, 0, []int{6, 34}},
	7:  {18, 4, 31, 0, 0, []int{6, 22, 38}},
	8:  {22, 2, 38, 2, 39, []int{6, 24, 42}},
	9:  {22, 3, 36, 2, 37, []int{6, 26, 46}},
	10: {26, 4, 43, 1, 44, []int{6, 28, 50}},
	11: {30, 1, 50, 4, 51, []int{6, 30, 54}},
	12: {22, 6, 36, 2, 37, []int{6, 32, 58}},
	13: {22, 8, 37, 1, 38, []int{6, 34, 62}},
	14: {24, 4, 40, 5, 41, []int{6, 26, 46, 66}},
	15: {24, 5, 41, 5, 42, []int{6, 26, 48, 70}},
}

// maximumVersion is the largest version levelM describes.
const maximumVersion = 15

// Encode draws data as the smallest QR code that holds it.
func Encode(data []byte) (*Code, error) {
	for version := 1; version <= maximumVersion; version++ {
		specification := levelM[version]
		dataCodewords := specification.groupOneBlocks*specification.groupOne + specification.groupTwoBlocks*specification.groupTwo
		countBits := 8
		if version >= 10 {
			countBits = 16
		}
		if 4+countBits+8*len(data) > 8*dataCodewords {
			continue
		}
		codewords := encodeData(data, countBits, dataCodewords)
		return draw(version, interleave(codewords, specification)), nil
	}
	return nil, ErrTooLong
}

// Dark reports whether the module at column x and row y is dark.
func (c *Code) Dark(x, y int) bool {
	if x < 0 || y < 0 || x >= c.Size || y >= c.Size {
		return false
	}
	return c.modules[y][x]
}

// SVG draws the code with a four-module quiet zone, dark on light, scaled by its container.
func (c *Code) SVG(title string) string {
	const quietZone = 4
	side := c.Size + 2*quietZone
	var path strings.Builder
	for y := range c.Size {
		for x := 0; x < c.Size; {
			if !c.modules[y][x] {
				x++
				continue
			}
			start := x
			for x < c.Size && c.modules[y][x] {
				x++
			}
			fmt.Fprintf(&path, "M%d %dh%dv1h-%dz", start+quietZone, y+quietZone, x-start, x-start)
		}
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img" aria-label="%s">`+
		`<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="%s"/></svg>`,
		side, side, escapeAttribute(title), side, side, path.String())
}

// escapeAttribute escapes value for a double-quoted XML attribute.
func escapeAttribute(value string) string {
	return strings.NewReplacer("&", "&amp;", `"`, "&quot;", "<", "&lt;", ">", "&gt;").Replace(value)
}

// encodeData lays out byte mode: the mode, the count, the bytes, a terminator, and padding.
func encodeData(data []byte, countBits, dataCodewords int) []byte {
	var bits bitBuffer
	bits.append(0b0100, 4)
	bits.append(uint32(len(data)), countBits)
	for _, value := range data {
		bits.append(uint32(value), 8)
	}
	capacity := 8 * dataCodewords
	bits.append(0, min(4, capacity-len(bits)))
	bits.append(0, (8-len(bits)%8)%8)
	for pad := 0; len(bits) < capacity; pad++ {
		bits.append([]uint32{0xEC, 0x11}[pad%2], 8)
	}
	codewords := make([]byte, dataCodewords)
	for index, bit := range bits {
		if bit {
			codewords[index/8] |= 1 << (7 - index%8)
		}
	}
	return codewords
}

// interleave splits the data into blocks, adds each block's error correction, and interleaves them.
func interleave(data []byte, specification block) []byte {
	var blocks, corrections [][]byte
	offset := 0
	for group, count := range []int{specification.groupOneBlocks, specification.groupTwoBlocks} {
		length := []int{specification.groupOne, specification.groupTwo}[group]
		for range count {
			blockData := data[offset : offset+length]
			offset += length
			blocks = append(blocks, blockData)
			corrections = append(corrections, reedSolomon(blockData, specification.ecc))
		}
	}
	var result []byte
	for index := range max(specification.groupOne, specification.groupTwo) {
		for _, blockData := range blocks {
			if index < len(blockData) {
				result = append(result, blockData[index])
			}
		}
	}
	for index := range specification.ecc {
		for _, correction := range corrections {
			result = append(result, correction[index])
		}
	}
	return result
}

// bitBuffer is a sequence of bits, most significant first.
type bitBuffer []bool

// append adds the low count bits of value, most significant first.
func (b *bitBuffer) append(value uint32, count int) {
	for bit := count - 1; bit >= 0; bit-- {
		*b = append(*b, (value>>bit)&1 == 1)
	}
}

// matrix is the code being drawn, with which modules belong to fixed patterns rather than data.
type matrix struct {
	// size is the modules along each side.
	size int

	// modules holds each row's modules, true for dark.
	modules [][]bool

	// isFunction marks the modules of fixed patterns, which data and masks skip.
	isFunction [][]bool
}

// newMatrix returns a blank matrix of size × size modules.
func newMatrix(size int) *matrix {
	grid := &matrix{size: size, modules: make([][]bool, size), isFunction: make([][]bool, size)}
	for row := range size {
		grid.modules[row] = make([]bool, size)
		grid.isFunction[row] = make([]bool, size)
	}
	return grid
}

// setFunction sets the module at x, y and marks it part of a fixed pattern.
func (m *matrix) setFunction(x, y int, dark bool) {
	m.modules[y][x] = dark
	m.isFunction[y][x] = true
}

// draw lays out the codewords for version under each of the eight masks and keeps the least penalized.
func draw(version int, codewords []byte) *Code {
	size := 4*version + 17
	best, bestPenalty := (*matrix)(nil), -1
	for mask := range 8 {
		grid := newMatrix(size)
		grid.drawFunctionPatterns(version)
		grid.drawCodewords(codewords)
		grid.applyMask(mask)
		grid.drawFormat(mask)
		if penalty := grid.penalty(); bestPenalty < 0 || penalty < bestPenalty {
			best, bestPenalty = grid, penalty
		}
	}
	return &Code{Size: size, modules: best.modules}
}

// drawFunctionPatterns draws the timing, finder and alignment patterns and reserves the format and
// version areas.
func (m *matrix) drawFunctionPatterns(version int) {
	for index := range m.size {
		m.setFunction(6, index, index%2 == 0)
		m.setFunction(index, 6, index%2 == 0)
	}
	m.drawFinder(3, 3)
	m.drawFinder(m.size-4, 3)
	m.drawFinder(3, m.size-4)

	positions := levelM[version].alignmentPatternPositions
	last := len(positions) - 1
	for first := range positions {
		for second := range positions {
			if (first == 0 && second == 0) || (first == 0 && second == last) || (first == last && second == 0) {
				continue
			}
			m.drawAlignment(positions[first], positions[second])
		}
	}
	m.drawFormat(0)
	m.drawVersion(version)
}

// drawFinder draws a finder pattern and its separator around a centre, clipped to the matrix.
func (m *matrix) drawFinder(centerX, centerY int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			x, y := centerX+dx, centerY+dy
			if x < 0 || y < 0 || x >= m.size || y >= m.size {
				continue
			}
			distance := max(abs(dx), abs(dy))
			m.setFunction(x, y, distance != 2 && distance != 4)
		}
	}
}

// drawAlignment draws a five-module alignment pattern around a centre.
func (m *matrix) drawAlignment(centerX, centerY int) {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			m.setFunction(centerX+dx, centerY+dy, max(abs(dx), abs(dy)) != 1)
		}
	}
}

// drawFormat writes level M and the mask, protected by BCH(15,5), in both copies.
func (m *matrix) drawFormat(mask int) {
	const levelMBits = 0b00
	data := levelMBits<<3 | mask
	remainder := data
	for range 10 {
		remainder = (remainder << 1) ^ ((remainder >> 9) * 0x537)
	}
	bits := (data<<10 | remainder) ^ 0x5412
	bit := func(index int) bool { return (bits>>index)&1 == 1 }

	for index := 0; index <= 5; index++ {
		m.setFunction(8, index, bit(index))
	}
	m.setFunction(8, 7, bit(6))
	m.setFunction(8, 8, bit(7))
	m.setFunction(7, 8, bit(8))
	for index := 9; index < 15; index++ {
		m.setFunction(14-index, 8, bit(index))
	}
	for index := range 8 {
		m.setFunction(m.size-1-index, 8, bit(index))
	}
	for index := 8; index < 15; index++ {
		m.setFunction(8, m.size-15+index, bit(index))
	}
	m.setFunction(8, m.size-8, true)
}

// drawVersion writes the version, protected by BCH(18,6), from version 7 up.
func (m *matrix) drawVersion(version int) {
	if version < 7 {
		return
	}
	remainder := version
	for range 12 {
		remainder = (remainder << 1) ^ ((remainder >> 11) * 0x1F25)
	}
	bits := version<<12 | remainder
	for index := range 18 {
		dark := (bits>>index)&1 == 1
		near, far := index/3, m.size-11+index%3
		m.setFunction(far, near, dark)
		m.setFunction(near, far, dark)
	}
}

// drawCodewords places the data in the zigzag of column pairs from the bottom right, skipping the
// vertical timing pattern.
func (m *matrix) drawCodewords(codewords []byte) {
	index := 0
	for right := m.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vertical := range m.size {
			for column := range 2 {
				x := right - column
				upward := (right+1)&2 == 0
				y := vertical
				if upward {
					y = m.size - 1 - vertical
				}
				if m.isFunction[y][x] || index >= len(codewords)*8 {
					continue
				}
				m.modules[y][x] = (codewords[index/8]>>(7-index%8))&1 == 1
				index++
			}
		}
	}
}

// applyMask inverts the data modules that mask pattern selects.
func (m *matrix) applyMask(mask int) {
	for y := range m.size {
		for x := range m.size {
			if m.isFunction[y][x] {
				continue
			}
			var invert bool
			switch mask {
			case 0:
				invert = (x+y)%2 == 0
			case 1:
				invert = y%2 == 0
			case 2:
				invert = x%3 == 0
			case 3:
				invert = (x+y)%3 == 0
			case 4:
				invert = (x/3+y/2)%2 == 0
			case 5:
				invert = x*y%2+x*y%3 == 0
			case 6:
				invert = (x*y%2+x*y%3)%2 == 0
			case 7:
				invert = ((x+y)%2+x*y%3)%2 == 0
			}
			m.modules[y][x] = m.modules[y][x] != invert
		}
	}
}

// penalty scores a masked code by the four rules of ISO/IEC 18004 section 7.8.3; the lowest wins.
func (m *matrix) penalty() int {
	total := 0
	finderLike := [][]bool{
		{true, false, true, true, true, false, true, false, false, false, false},
		{false, false, false, false, true, false, true, true, true, false, true},
	}
	line := func(get func(int) bool) {
		run := 1
		for index := 1; index < m.size; index++ {
			if get(index) == get(index-1) {
				run++
				continue
			}
			if run >= 5 {
				total += run - 2
			}
			run = 1
		}
		if run >= 5 {
			total += run - 2
		}
		for start := 0; start+11 <= m.size; start++ {
			for _, pattern := range finderLike {
				matches := true
				for offset, dark := range pattern {
					if get(start+offset) != dark {
						matches = false
						break
					}
				}
				if matches {
					total += 40
				}
			}
		}
	}
	dark := 0
	for index := range m.size {
		line(func(x int) bool { return m.modules[index][x] })
		line(func(y int) bool { return m.modules[y][index] })
	}
	for y := range m.size {
		for x := range m.size {
			if m.modules[y][x] {
				dark++
			}
			if x+1 < m.size && y+1 < m.size {
				color := m.modules[y][x]
				if m.modules[y][x+1] == color && m.modules[y+1][x] == color && m.modules[y+1][x+1] == color {
					total += 3
				}
			}
		}
	}
	modules := m.size * m.size
	deviation := abs(dark*20-modules*10) / modules
	return total + deviation*10
}

// abs returns the absolute value of value.
func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// reedSolomon returns the error correction codewords for data over GF(256) with polynomial 0x11D.
func reedSolomon(data []byte, degree int) []byte {
	generator := []byte{1}
	root := byte(1)
	for range degree {
		next := make([]byte, len(generator)+1)
		for index, coefficient := range generator {
			next[index] ^= coefficient
			next[index+1] ^= multiply(coefficient, root)
		}
		generator = next
		root = multiply(root, 2)
	}
	remainder := make([]byte, degree)
	for _, value := range data {
		factor := value ^ remainder[0]
		copy(remainder, remainder[1:])
		remainder[degree-1] = 0
		for index := range degree {
			remainder[index] ^= multiply(generator[index+1], factor)
		}
	}
	return remainder
}

// multiply returns the product of two elements of GF(256) with polynomial 0x11D.
func multiply(first, second byte) byte {
	var product byte
	for second > 0 {
		if second&1 == 1 {
			product ^= first
		}
		carry := first & 0x80
		first <<= 1
		if carry != 0 {
			first ^= 0x1D
		}
		second >>= 1
	}
	return product
}
