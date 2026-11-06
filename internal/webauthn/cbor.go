package webauthn

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// This file decodes the subset of CBOR (RFC 8949) passkeys use: integers, byte and text strings,
// arrays, maps and simple values. Indefinite lengths, floats and tags are refused; no passkey uses them.

// maximumDepth is the deepest nesting of arrays and maps accepted.
const maximumDepth = 16

// errMalformed is returned for any CBOR outside the subset or cut short.
var errMalformed = errors.New("webauthn: malformed CBOR")

// decodeCBOR decodes one value and returns it with the bytes after it. Integers come back as int64,
// byte strings as []byte, text as string, arrays as []any and maps as map[any]any.
func decodeCBOR(data []byte) (any, []byte, error) {
	return decodeValue(data, 0)
}

// decodeValue decodes one value nested depth levels deep.
func decodeValue(data []byte, depth int) (any, []byte, error) {
	if depth > maximumDepth || len(data) == 0 {
		return nil, nil, errMalformed
	}
	major, additional := data[0]>>5, data[0]&0x1f
	argument, rest, err := readArgument(additional, data[1:])
	if err != nil {
		return nil, nil, err
	}
	switch major {
	case 0: // unsigned integer
		if argument > math.MaxInt64 {
			return nil, nil, errMalformed
		}
		return int64(argument), rest, nil
	case 1: // negative integer
		if argument > math.MaxInt64 {
			return nil, nil, errMalformed
		}
		return -1 - int64(argument), rest, nil
	case 2, 3: // byte string, text string
		if argument > uint64(len(rest)) {
			return nil, nil, errMalformed
		}
		content := rest[:argument]
		if major == 3 {
			return string(content), rest[argument:], nil
		}
		copied := make([]byte, len(content))
		copy(copied, content)
		return copied, rest[argument:], nil
	case 4: // array
		if argument > uint64(len(rest)) {
			return nil, nil, errMalformed
		}
		items := make([]any, 0, argument)
		for range argument {
			var item any
			if item, rest, err = decodeValue(rest, depth+1); err != nil {
				return nil, nil, err
			}
			items = append(items, item)
		}
		return items, rest, nil
	case 5: // map
		if argument > uint64(len(rest)) {
			return nil, nil, errMalformed
		}
		entries := make(map[any]any, argument)
		for range argument {
			var key, value any
			if key, rest, err = decodeValue(rest, depth+1); err != nil {
				return nil, nil, err
			}
			switch key.(type) {
			case int64, string:
			default:
				return nil, nil, fmt.Errorf("webauthn: a CBOR map key of type %T", key)
			}
			if value, rest, err = decodeValue(rest, depth+1); err != nil {
				return nil, nil, err
			}
			if _, repeated := entries[key]; repeated {
				return nil, nil, errMalformed
			}
			entries[key] = value
		}
		return entries, rest, nil
	case 7: // simple value
		switch additional {
		case 20:
			return false, rest, nil
		case 21:
			return true, rest, nil
		case 22:
			return nil, rest, nil
		}
	}
	return nil, nil, errMalformed
}

// readArgument reads the argument that follows an initial byte with the given additional information.
func readArgument(additional byte, data []byte) (uint64, []byte, error) {
	switch {
	case additional < 24:
		return uint64(additional), data, nil
	case additional == 24 && len(data) >= 1:
		return uint64(data[0]), data[1:], nil
	case additional == 25 && len(data) >= 2:
		return uint64(binary.BigEndian.Uint16(data)), data[2:], nil
	case additional == 26 && len(data) >= 4:
		return uint64(binary.BigEndian.Uint32(data)), data[4:], nil
	case additional == 27 && len(data) >= 8:
		return binary.BigEndian.Uint64(data), data[8:], nil
	}
	return 0, nil, errMalformed
}
