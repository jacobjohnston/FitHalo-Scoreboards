package qr_test

import (
	"bytes"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/qr"
)

// gfMultiply multiplies in GF(2^8) with the QR code's primitive polynomial 0x11D.
func gfMultiply(a, b byte) byte {
	var product byte
	for b > 0 {
		if b&1 == 1 {
			product ^= a
		}
		carry := a & 0x80
		a <<= 1
		if carry != 0 {
			a ^= 0x1D
		}
		b >>= 1
	}
	return product
}

// reedSolomonRemainder returns degree error correction codewords for data, as the standard's generator polynomials give.
func reedSolomonRemainder(data []byte, degree int) []byte {
	generator := make([]byte, degree)
	generator[degree-1] = 1
	root := byte(1)
	for range degree {
		for index := range generator {
			generator[index] = gfMultiply(generator[index], root)
			if index+1 < len(generator) {
				generator[index] ^= generator[index+1]
			}
		}
		root = gfMultiply(root, 2)
	}
	remainder := make([]byte, degree)
	for _, value := range data {
		factor := value ^ remainder[0]
		copy(remainder, remainder[1:])
		remainder[degree-1] = 0
		for index := range remainder {
			remainder[index] ^= gfMultiply(generator[index], factor)
		}
	}
	return remainder
}

// bitWriter accumulates bits most significant first.
type bitWriter struct {
	// bits is the sequence written so far.
	bits []bool
}

// write appends the low count bits of value, most significant first.
func (w *bitWriter) write(value, count int) {
	for index := count - 1; index >= 0; index-- {
		w.bits = append(w.bits, (value>>index)&1 == 1)
	}
}

// expectedCodewords builds the full interleaved codeword sequence the standard prescribes for data at level M.
func expectedCodewords(version int, data []byte) []byte {
	specification := blocksM[version]
	totalData := 0
	for _, block := range specification.blocks {
		totalData += block
	}
	var writer bitWriter
	writer.write(0b0100, 4)
	if version < 10 {
		writer.write(len(data), 8)
	} else {
		writer.write(len(data), 16)
	}
	for _, value := range data {
		writer.write(int(value), 8)
	}
	capacity := totalData * 8
	writer.write(0, min(4, capacity-len(writer.bits)))
	for len(writer.bits)%8 != 0 {
		writer.write(0, 1)
	}
	for pad := 0xEC; len(writer.bits) < capacity; pad ^= 0xEC ^ 0x11 {
		writer.write(pad, 8)
	}
	codewords := make([]byte, totalData)
	for index, dark := range writer.bits {
		if dark {
			codewords[index/8] |= 0x80 >> (index % 8)
		}
	}
	var blocks, corrections [][]byte
	offset := 0
	for _, length := range specification.blocks {
		block := codewords[offset : offset+length]
		offset += length
		blocks = append(blocks, block)
		corrections = append(corrections, reedSolomonRemainder(block, specification.ecc))
	}
	var interleaved []byte
	longest := specification.blocks[len(specification.blocks)-1]
	for index := range longest {
		for _, block := range blocks {
			if index < len(block) {
				interleaved = append(interleaved, block[index])
			}
		}
	}
	for index := range specification.ecc {
		for _, correction := range corrections {
			interleaved = append(interleaved, correction[index])
		}
	}
	return interleaved
}

// functionModules marks every module the standard reserves for patterns, format and version information.
func functionModules(version, size int) [][]bool {
	reserved := make([][]bool, size)
	for index := range reserved {
		reserved[index] = make([]bool, size)
	}
	mark := func(x, y int) {
		if x >= 0 && y >= 0 && x < size && y < size {
			reserved[y][x] = true
		}
	}
	for index := range 9 {
		for other := range 9 {
			mark(index, other)
			if index < 8 {
				mark(size-1-index, other)
				mark(other, size-1-index)
			}
		}
	}
	for index := range size {
		mark(index, 6)
		mark(6, index)
	}
	centres := alignmentCentres(version, size)
	for _, cx := range centres {
		for _, cy := range centres {
			if (cx == 6 && cy == 6) || (cx == 6 && cy == size-7) || (cx == size-7 && cy == 6) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					mark(cx+dx, cy+dy)
				}
			}
		}
	}
	if version >= 7 {
		for index := range 18 {
			a, b := size-11+index%3, index/3
			mark(a, b)
			mark(b, a)
		}
	}
	return reserved
}

// masked reports whether the mask pattern flips the module at column x, row y.
func masked(pattern, x, y int) bool {
	switch pattern {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (x/3+y/2)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	default:
		return ((x+y)%2+x*y%3)%2 == 0
	}
}

// readCodewords walks the zigzag placement, unmasks each data module and returns the codewords and any leftover bits.
func readCodewords(code *qr.Code, pattern int) (codewords []byte, remainder []bool) {
	size := code.Size
	reserved := functionModules(versionOf(code), size)
	var bits []bool
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vertical := range size {
			for offset := range 2 {
				x := right - offset
				y := vertical
				if (right+1)&2 == 0 {
					y = size - 1 - vertical
				}
				if reserved[y][x] {
					continue
				}
				bits = append(bits, code.Dark(x, y) != masked(pattern, x, y))
			}
		}
	}
	whole := len(bits) / 8 * 8
	codewords = make([]byte, whole/8)
	for index, dark := range bits[:whole] {
		if dark {
			codewords[index/8] |= 0x80 >> (index % 8)
		}
	}
	return codewords, bits[whole:]
}

// TestCodewordsMatchTheStandard decodes every version's data region and compares it to the prescribed codewords.
func TestCodewordsMatchTheStandard(t *testing.T) {
	for _, data := range [][]byte{
		{}, []byte("x"), []byte("HELLO WORLD"), []byte("https://example.com/join?code=ABCDEFGH"),
		bytesOf(14), bytesOf(15), bytesOf(26), bytesOf(27), bytesOf(100), bytesOf(180), bytesOf(181), bytesOf(213),
		bytesOf(214), bytesOf(362), bytesOf(363), bytesOf(412), bytes.Repeat([]byte{0xff}, 300), bytes.Repeat([]byte{0}, 50),
	} {
		code, err := qr.Encode(data)
		if err != nil {
			t.Fatalf("%d bytes: %v", len(data), err)
		}
		version := versionOf(code)
		pattern := formatInfo(t, code)
		got, remainder := readCodewords(code, pattern)
		want := expectedCodewords(version, data)
		if len(got) != len(want) {
			t.Errorf("%d bytes (version %d): read %d codewords, standard has %d", len(data), version, len(got), len(want))
			continue
		}
		if !bytes.Equal(got, want) {
			first := 0
			for first < len(got) && got[first] == want[first] {
				first++
			}
			t.Errorf("%d bytes (version %d, mask %d): codeword %d is %02X, want %02X", len(data), version, pattern, first, got[first], want[first])
		}
		for index, dark := range remainder {
			if dark {
				t.Errorf("%d bytes (version %d): remainder bit %d is set", len(data), version, index)
				break
			}
		}
		wantRemainder := map[int]int{1: 0, 2: 7, 3: 7, 4: 7, 5: 7, 6: 7, 7: 0, 8: 0, 9: 0, 10: 0, 11: 0, 12: 0, 13: 0, 14: 3, 15: 3}[version]
		if len(remainder) != wantRemainder {
			t.Errorf("%d bytes (version %d): %d remainder bits, standard has %d", len(data), version, len(remainder), wantRemainder)
		}
	}
}

// TestEveryLengthDecodes round-trips every byte count from 0 to 412 through the codeword reader.
func TestEveryLengthDecodes(t *testing.T) {
	for count := 0; count <= 412; count++ {
		data := bytesOf(count)
		code, err := qr.Encode(data)
		if err != nil {
			t.Fatalf("%d bytes: %v", count, err)
		}
		got, _ := readCodewords(code, formatInfo(t, code))
		if want := expectedCodewords(versionOf(code), data); !bytes.Equal(got, want) {
			t.Fatalf("%d bytes: codewords differ from the standard", count)
		}
	}
}
