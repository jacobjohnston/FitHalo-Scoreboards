// Package auth verifies the FitHalo-ECDSA request signature defined in the README.
package auth

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Scheme is the Authorization header's scheme.
const Scheme = "FitHalo-ECDSA"

// MaximumSkew is how far a request's time may be from the server's clock.
const MaximumSkew = 300 * time.Second

// NonceLifetime is how long an accepted nonce is remembered, longer than the skew allowed either way.
const NonceLifetime = 600 * time.Second

// nonceSweepInterval is the least time between sweeps of a NonceCache.
const nonceSweepInterval = NonceLifetime / 10

const (
	// publicKeyLength is the bytes of an uncompressed P-256 point.
	publicKeyLength = 65

	// uncompressedPrefix is the first byte of an uncompressed point.
	uncompressedPrefix = 4

	// signatureLength is the bytes of a signature, r and s of 32 bytes each.
	signatureLength = 64

	// nonceLength is the bytes of a decoded nonce.
	nonceLength = 16

	// memberIDLength is the bytes of a public key's SHA-256 kept as its member identifier.
	memberIDLength = 16

	// messageLabel is the first line of every signed message, naming its version.
	messageLabel = "FitHalo-ECDSA-1"
)

// encoding is unpadded base64url, used for every binary value in the header.
var encoding = base64.RawURLEncoding

// Header is a parsed Authorization header.
type Header struct {
	// Member is the signer's member identifier, in unpadded base64url.
	Member string

	// Time is when the request was signed, in Unix seconds.
	Time int64

	// Nonce is the request's random nonce, in unpadded base64url.
	Nonce string

	// Signature is the decoded signature, r ‖ s.
	Signature []byte
}

// ParsePublicKey decodes an uncompressed P-256 point in unpadded base64url, as the API sends it,
// rejecting one off the curve, and returns the key with its raw bytes.
func ParsePublicKey(encoded string) (*ecdsa.PublicKey, []byte, error) {
	if !isBase64URL(encoded) {
		return nil, nil, errors.New("auth: public key is not a 65-byte point")
	}
	raw, err := encoding.DecodeString(encoded)
	if err != nil || len(raw) != publicKeyLength {
		return nil, nil, errors.New("auth: public key is not a 65-byte point")
	}
	if _, err := ecdh.P256().NewPublicKey(raw); err != nil {
		return nil, nil, errors.New("auth: public key is not on P-256")
	}
	key := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(raw[1:33]),
		Y:     new(big.Int).SetBytes(raw[33:65]),
	}
	return key, raw, nil
}

// EncodePublicKey encodes a public key as the API sends it.
func EncodePublicKey(key *ecdsa.PublicKey) string {
	raw := make([]byte, publicKeyLength)
	raw[0] = uncompressedPrefix
	key.X.FillBytes(raw[1:33])
	key.Y.FillBytes(raw[33:65])
	return encoding.EncodeToString(raw)
}

// MemberID is the identifier of a public key: the first 16 bytes of its SHA-256.
func MemberID(rawPublicKey []byte) string {
	digest := sha256.Sum256(rawPublicKey)
	return encoding.EncodeToString(digest[:memberIDLength])
}

// ParseHeader parses an Authorization header, requiring every parameter exactly once.
func ParseHeader(value string) (Header, error) {
	scheme, parameters, found := strings.Cut(value, " ")
	if !found || scheme != Scheme {
		return Header{}, errors.New("auth: not a FitHalo-ECDSA header")
	}
	values := map[string]string{}
	for _, parameter := range strings.Split(parameters, ",") {
		// Only spaces after a comma are optional; nothing else is trimmed, so a value is exactly what was signed.
		key, parameterValue, found := strings.Cut(strings.TrimLeft(parameter, " "), "=")
		if !found {
			return Header{}, errors.New("auth: a parameter is not a key=value pair")
		}
		if _, repeated := values[key]; repeated {
			return Header{}, errors.New("auth: a parameter appears twice")
		}
		values[key] = parameterValue
	}
	if len(values) != 4 {
		return Header{}, errors.New("auth: the header needs exactly member, time, nonce and signature")
	}

	var header Header
	header.Member = values["member"]
	if !isEncodedLength(header.Member, memberIDLength) {
		return Header{}, errors.New("auth: member is not an identifier")
	}
	seconds, err := strconv.ParseInt(values["time"], 10, 64)
	if err != nil || !isCanonicalDecimal(values["time"]) {
		return Header{}, errors.New("auth: time is not a whole number of seconds")
	}
	header.Time = seconds
	header.Nonce = values["nonce"]
	if !isEncodedLength(header.Nonce, nonceLength) {
		return Header{}, errors.New("auth: nonce is not 16 bytes")
	}
	if !isBase64URL(values["signature"]) {
		return Header{}, errors.New("auth: signature is not 64 bytes")
	}
	header.Signature, err = encoding.DecodeString(values["signature"])
	if err != nil || len(header.Signature) != signatureLength {
		return Header{}, errors.New("auth: signature is not 64 bytes")
	}
	return header, nil
}

// isEncodedLength reports whether value is unpadded base64url of exactly byteCount bytes.
func isEncodedLength(value string, byteCount int) bool {
	if !isBase64URL(value) {
		return false
	}
	decoded, err := encoding.DecodeString(value)
	return err == nil && len(decoded) == byteCount
}

// isCanonicalDecimal reports whether value spells a non-negative integer with no sign and no leading zero, the one
// spelling the signed message carries.
func isCanonicalDecimal(value string) bool {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return false
	}
	for _, character := range []byte(value) {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

// isBase64URL reports whether every byte of value is in the unpadded base64url alphabet, so the decoder's
// tolerance of line breaks never lets one through.
func isBase64URL(value string) bool {
	for _, character := range []byte(value) {
		switch {
		case 'A' <= character && character <= 'Z', 'a' <= character && character <= 'z',
			'0' <= character && character <= '9', character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

// Message is the exact bytes a request's signature covers.
func Message(method, target string, seconds int64, nonce string, body []byte) []byte {
	bodyDigest := sha256.Sum256(body)
	lines := []string{
		messageLabel,
		strings.ToUpper(method),
		target,
		strconv.FormatInt(seconds, 10) + ":" + nonce,
		hex.EncodeToString(bodyDigest[:]),
	}
	return []byte(strings.Join(lines, "\n"))
}

// Verify reports whether signature, as r ‖ s, is a valid signature of message by key.
func Verify(key *ecdsa.PublicKey, message, signature []byte) bool {
	if len(signature) != signatureLength {
		return false
	}
	digest := sha256.Sum256(message)
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	return ecdsa.Verify(key, digest[:], r, s)
}

// Sign signs message with key and returns r ‖ s. The server never signs; tests and the conformance
// suite act as a client with it.
func Sign(key *ecdsa.PrivateKey, message []byte) ([]byte, error) {
	digest := sha256.Sum256(message)
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return nil, err
	}
	signature := make([]byte, signatureLength)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return signature, nil
}

// AuthorizationValue builds a signed Authorization header for a request, as a client would.
func AuthorizationValue(key *ecdsa.PrivateKey, method, target string, now time.Time, body []byte) (string, error) {
	nonceBytes := make([]byte, nonceLength)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", err
	}
	nonce := encoding.EncodeToString(nonceBytes)
	seconds := now.Unix()
	signature, err := Sign(key, Message(method, target, seconds, nonce, body))
	if err != nil {
		return "", err
	}
	rawPublicKey, _ := encoding.DecodeString(EncodePublicKey(&key.PublicKey))
	return fmt.Sprintf("%s member=%s,time=%d,nonce=%s,signature=%s",
		Scheme, MemberID(rawPublicKey), seconds, nonce, encoding.EncodeToString(signature)), nil
}

// WithinSkew reports whether a request's time is close enough to now.
func WithinSkew(seconds int64, now time.Time) bool {
	difference := now.Sub(time.Unix(seconds, 0))
	return difference <= MaximumSkew && difference >= -MaximumSkew
}

// NonceCache remembers the nonces each member has used within NonceLifetime. It suits a server that
// runs as a single instance; several instances need the nonces in their shared store.
type NonceCache struct {
	// mutex guards every field below it.
	mutex sync.Mutex

	// seen holds when each member:nonce key was used.
	seen map[string]time.Time

	// lastSwept is when expired nonces were last forgotten.
	lastSwept time.Time
}

// NewNonceCache returns an empty cache.
func NewNonceCache() *NonceCache {
	return &NonceCache{seen: map[string]time.Time{}}
}

// Use records a member's nonce and reports whether it was new.
func (c *NonceCache) Use(member, nonce string, now time.Time) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if now.Sub(c.lastSwept) > nonceSweepInterval {
		for key, usedAt := range c.seen {
			if now.Sub(usedAt) > NonceLifetime {
				delete(c.seen, key)
			}
		}
		c.lastSwept = now
	}
	key := member + ":" + nonce
	if usedAt, used := c.seen[key]; used && now.Sub(usedAt) <= NonceLifetime {
		return false
	}
	c.seen[key] = now
	return true
}
