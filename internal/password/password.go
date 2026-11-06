// Package password hashes passwords with PBKDF2-HMAC-SHA256, a per-password salt and an optional
// pepper kept in the server's configuration.
package password

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
)

// Iterations is PBKDF2's iteration count for new hashes.
const Iterations = 600_000

const (
	// algorithm is the first field of every stored value.
	algorithm = "pbkdf2-sha256"

	// saltLength is the bytes of random salt in a new hash.
	saltLength = 16

	// keyLength is the bytes of derived key in a new hash.
	keyLength = 32

	// pepperIDLabel is the message a pepper signs to make its PepperID.
	pepperIDLabel = "FitHalo Scoreboard pepper"

	// pepperIDLength is the bytes of HMAC kept in a PepperID.
	pepperIDLength = 4
)

// encoding encodes the salt and hash in a stored value.
var encoding = base64.RawStdEncoding

// ErrPepperChanged means a hash was made with a pepper the server no longer has, so it can never verify.
var ErrPepperChanged = errors.New("password: the hash was made with a different pepper")

// Hash returns a stored value for password: algorithm, iterations, salt and hash joined by dollar signs,
// then the PepperID when a pepper is given.
func Hash(password string, pepper []byte) (string, error) {
	if strings.ContainsRune(password, 0) {
		return "", errors.New("password: a password cannot hold a NUL character")
	}
	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := derive(password, salt, Iterations, keyLength, pepper)
	if err != nil {
		return "", err
	}
	parts := []string{algorithm, strconv.Itoa(Iterations), encoding.EncodeToString(salt), encoding.EncodeToString(key)}
	if len(pepper) > 0 {
		parts = append(parts, PepperID(pepper))
	}
	return strings.Join(parts, "$"), nil
}

// Verify reports whether password matches a stored value, comparing in constant time.
func Verify(stored, password string, pepper []byte) (bool, error) {
	parts := strings.Split(stored, "$")
	if (len(parts) != 4 && len(parts) != 5) || parts[0] != algorithm {
		return false, errors.New("password: unrecognised stored value")
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 1 {
		return false, errors.New("password: unrecognised iteration count")
	}
	salt, saltErr := encoding.DecodeString(parts[2])
	expected, keyErr := encoding.DecodeString(parts[3])
	if saltErr != nil || keyErr != nil || len(salt) != saltLength || len(expected) != keyLength {
		return false, errors.New("password: unrecognised salt or hash")
	}
	// HMAC pads a short key with zeros, so a password with trailing NULs would match one without them.
	if strings.ContainsRune(password, 0) {
		return false, nil
	}
	var usedPepper []byte
	if len(parts) == 5 {
		if len(pepper) == 0 || parts[4] != PepperID(pepper) {
			return false, ErrPepperChanged
		}
		usedPepper = pepper
	}
	key, err := derive(password, salt, iterations, len(expected), usedPepper)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(key, expected) == 1, nil
}

// PepperID names a pepper without revealing it, so a hash records which pepper made it.
func PepperID(pepper []byte) string {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(pepperIDLabel))
	return hex.EncodeToString(mac.Sum(nil)[:pepperIDLength])
}

// derive runs PBKDF2 and, with a pepper, takes an HMAC of the result under it.
func derive(password string, salt []byte, iterations, length int, pepper []byte) ([]byte, error) {
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, length)
	if err != nil || len(pepper) == 0 {
		return key, err
	}
	mac := hmac.New(sha256.New, pepper)
	mac.Write(key)
	return mac.Sum(nil)[:length], nil
}
