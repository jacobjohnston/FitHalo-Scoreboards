package avatar_test

import "hash/crc32"

// crc32Of returns the big-endian CRC a PNG chunk carries over its type and data.
func crc32Of(typeAndData []byte) []byte {
	sum := crc32.ChecksumIEEE(typeAndData)
	return []byte{byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum)}
}
