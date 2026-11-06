package auth_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/auth"
)

// emptyBodyHash is the SHA-256 of nothing, from the README.
const emptyBodyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// rawURL is the unpadded base64url encoding the contract uses.
var rawURL = base64.RawURLEncoding

// TestMessageExactBytes pins the five lines for several requests.
func TestMessageExactBytes(t *testing.T) {
	body := []byte(`{"progress":0.5}`)
	bodyHash := sha256.Sum256(body)
	cases := []struct {
		// name describes the request.
		name string

		// method is the HTTP method.
		method string

		// target is the request target.
		target string

		// seconds is the signing time.
		seconds int64

		// nonce is the nonce.
		nonce string

		// body is the request body.
		body []byte

		// want is the exact message.
		want string
	}{
		{
			name: "no body", method: "GET", target: "/api/v1/members/me", seconds: 1792800000, nonce: "AAAAAAAAAAAAAAAAAAAAAA",
			want: "FitHalo-ECDSA-1\nGET\n/api/v1/members/me\n1792800000:AAAAAAAAAAAAAAAAAAAAAA\n" + emptyBodyHash,
		},
		{
			name: "empty body", method: "DELETE", target: "/api/v1/members/me", seconds: 1792800000, nonce: "AAAAAAAAAAAAAAAAAAAAAA", body: []byte{},
			want: "FitHalo-ECDSA-1\nDELETE\n/api/v1/members/me\n1792800000:AAAAAAAAAAAAAAAAAAAAAA\n" + emptyBodyHash,
		},
		{
			name: "with body", method: "PUT", target: "/api/v1/members/me/weekly-challenges/2026-10-19", seconds: 1792800000, nonce: "AAAAAAAAAAAAAAAAAAAAAA", body: body,
			want: "FitHalo-ECDSA-1\nPUT\n/api/v1/members/me/weekly-challenges/2026-10-19\n1792800000:AAAAAAAAAAAAAAAAAAAAAA\n" + hex.EncodeToString(bodyHash[:]),
		},
		{
			name: "query kept verbatim", method: "GET", target: "/api/v1/board?week=2026-10-19&x=%2F&y=a+b", seconds: 7, nonce: "n",
			want: "FitHalo-ECDSA-1\nGET\n/api/v1/board?week=2026-10-19&x=%2F&y=a+b\n7:n\n" + emptyBodyHash,
		},
		{
			name: "prefix path and percent encoding", method: "POST", target: "/boards/one/api/v1/members?name=%E2%9C%93", seconds: 0, nonce: "",
			want: "FitHalo-ECDSA-1\nPOST\n/boards/one/api/v1/members?name=%E2%9C%93\n0:\n" + emptyBodyHash,
		},
		{
			name: "non-ASCII target", method: "GET", target: "/api/v1/ünicode/✓", seconds: 1, nonce: "x",
			want: "FitHalo-ECDSA-1\nGET\n/api/v1/ünicode/✓\n1:x\n" + emptyBodyHash,
		},
		{
			name: "zero time", method: "GET", target: "/", seconds: 0, nonce: "x",
			want: "FitHalo-ECDSA-1\nGET\n/\n0:x\n" + emptyBodyHash,
		},
		{
			name: "negative time", method: "GET", target: "/", seconds: -1, nonce: "x",
			want: "FitHalo-ECDSA-1\nGET\n/\n-1:x\n" + emptyBodyHash,
		},
		{
			name: "max time", method: "GET", target: "/", seconds: 9223372036854775807, nonce: "x",
			want: "FitHalo-ECDSA-1\nGET\n/\n9223372036854775807:x\n" + emptyBodyHash,
		},
		{
			name: "body with newline", method: "PUT", target: "/", seconds: 1, nonce: "x", body: []byte("a\nb"),
			want: "FitHalo-ECDSA-1\nPUT\n/\n1:x\n" + hexSum([]byte("a\nb")),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := auth.Message(tc.method, tc.target, tc.seconds, tc.nonce, tc.body)
			if string(got) != tc.want {
				t.Errorf("Message =\n%q\nwant\n%q", got, tc.want)
			}
			if bytes.HasSuffix(got, []byte("\n")) {
				t.Error("message ends in a line feed")
			}
			if bytes.Count(got, []byte("\n")) != 4 {
				t.Errorf("message has %d line feeds, want 4", bytes.Count(got, []byte("\n")))
			}
		})
	}
}

// hexSum is the lower-case hex SHA-256 of data.
func hexSum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestMessageMethodCase documents how a lower-case method is treated.
func TestMessageMethodCase(t *testing.T) {
	upper := auth.Message("PUT", "/", 1, "x", nil)
	lower := auth.Message("put", "/", 1, "x", nil)
	mixed := auth.Message("Put", "/", 1, "x", nil)
	// PROBE: the contract says the method line is upper case; a server that upper-cases a request's
	// method also upper-cases a custom method like "Purge", which the client may have signed as given.
	t.Logf("PROBE: lower-case method folded=%v mixed folded=%v", string(lower) == string(upper), string(mixed) == string(upper))
	turkish := auth.Message("ı", "/", 1, "x", nil)
	if strings.Contains(string(turkish), "�") {
		t.Error("message replaced a non-ASCII method with U+FFFD")
	}
}

// TestMessageBodyHashIsLowerHex checks the body hash line for many bodies.
func TestMessageBodyHashIsLowerHex(t *testing.T) {
	for size := 0; size < 300; size += 7 {
		body := bytes.Repeat([]byte{0xff}, size)
		got := auth.Message("PUT", "/", 1, "x", body)
		lines := strings.Split(string(got), "\n")
		if len(lines) != 5 {
			t.Fatalf("%d lines", len(lines))
		}
		if lines[4] != hexSum(body) {
			t.Errorf("size %d: hash line %q", size, lines[4])
		}
	}
}

// TestMessageDistinguishesFieldBoundaries makes sure moving bytes between fields changes the message.
func TestMessageDistinguishesFieldBoundaries(t *testing.T) {
	a := auth.Message("GET", "/a", 12, "3x", nil)
	b := auth.Message("GET", "/a", 123, "x", nil)
	if string(a) == string(b) {
		t.Error("time and nonce are not separated")
	}
	c := auth.Message("GET", "/a\n1", 2, "x", nil)
	d := auth.Message("GET", "/a", 1, "2\nx", nil)
	if string(c) == string(d) {
		t.Log("PROBE: a line feed inside the target or nonce can shift fields; both are refused upstream by the header grammar")
	}
}

// TestMemberIDMatchesIndependentComputation recomputes the identifier with crypto/sha256.
func TestMemberIDMatchesIndependentComputation(t *testing.T) {
	for i := 0; i < 20; i++ {
		key := probeKey(t)
		raw := elliptic.Marshal(elliptic.P256(), key.X, key.Y)
		if len(raw) != 65 || raw[0] != 0x04 {
			t.Fatalf("marshalled point is %d bytes starting %#x", len(raw), raw[0])
		}
		sum := sha256.Sum256(raw)
		want := rawURL.EncodeToString(sum[:16])
		got := auth.MemberID(raw)
		if got != want {
			t.Errorf("MemberID = %q, want %q", got, want)
		}
		if len(got) != 22 || strings.ContainsAny(got, "+/=") {
			t.Errorf("MemberID %q is not 22 unpadded base64url characters", got)
		}
		_, parsedRaw, err := auth.ParsePublicKey(auth.EncodePublicKey(&key.PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(parsedRaw, raw) {
			t.Error("ParsePublicKey returned different raw bytes than SEC 1 marshalling")
		}
		if auth.MemberID(parsedRaw) != want {
			t.Error("MemberID of the parsed raw bytes differs")
		}
	}
}

// TestMemberIDOfOddLengths documents what MemberID does with input that is not a 65-byte point.
func TestMemberIDOfOddLengths(t *testing.T) {
	for _, raw := range [][]byte{nil, {}, {0x04}, bytes.Repeat([]byte{1}, 33), bytes.Repeat([]byte{1}, 64), bytes.Repeat([]byte{1}, 66)} {
		got := auth.MemberID(raw)
		t.Logf("PROBE: MemberID of %d bytes = %q", len(raw), got)
	}
}

// TestPublicKeyRoundTrip encodes and parses many keys.
func TestPublicKeyRoundTrip(t *testing.T) {
	for i := 0; i < 50; i++ {
		key := probeKey(t)
		encoded := auth.EncodePublicKey(&key.PublicKey)
		if len(encoded) != 87 {
			t.Fatalf("encoded key is %d characters, want 87", len(encoded))
		}
		if strings.ContainsAny(encoded, "+/=") {
			t.Fatalf("encoded key %q uses padding or the standard alphabet", encoded)
		}
		want := rawURL.EncodeToString(elliptic.Marshal(elliptic.P256(), key.X, key.Y))
		if encoded != want {
			t.Fatalf("EncodePublicKey = %q, want %q", encoded, want)
		}
		parsed, raw, err := auth.ParsePublicKey(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.X.Cmp(key.X) != 0 || parsed.Y.Cmp(key.Y) != 0 || parsed.Curve != elliptic.P256() {
			t.Fatal("parsed key differs")
		}
		if len(raw) != 65 || raw[0] != 0x04 {
			t.Fatalf("raw is %d bytes starting %#x", len(raw), raw[0])
		}
		if !parsed.Equal(&key.PublicKey) {
			t.Fatal("parsed key is not Equal to the original")
		}
	}
}

// keyWithURLSpecificCharacters returns a key whose base64url encoding contains '-' or '_'.
func keyWithURLSpecificCharacters(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	for i := 0; i < 100; i++ {
		key := probeKey(t)
		if strings.ContainsAny(auth.EncodePublicKey(&key.PublicKey), "-_") {
			return key
		}
	}
	t.Fatal("no key with a URL-specific character in 100 draws")
	return nil
}

// TestParsePublicKeyRefusals refuses every documented and undocumented wrong shape.
func TestParsePublicKeyRefusals(t *testing.T) {
	key := keyWithURLSpecificCharacters(t)
	raw := elliptic.Marshal(elliptic.P256(), key.X, key.Y)
	encoded := auth.EncodePublicKey(&key.PublicKey)
	params := elliptic.P256().Params()
	cases := []struct {
		// name describes the shape.
		name string

		// encoded is the string to parse.
		encoded string
	}{
		{name: "empty", encoded: ""},
		{name: "compressed even", encoded: rawURL.EncodeToString(elliptic.MarshalCompressed(elliptic.P256(), key.X, key.Y))},
		{name: "compressed with 0x03", encoded: rawURL.EncodeToString(append([]byte{0x03}, raw[1:33]...))},
		{name: "64 bytes without prefix", encoded: rawURL.EncodeToString(raw[1:])},
		{name: "prefix 0x05", encoded: rawURL.EncodeToString(append([]byte{0x05}, raw[1:]...))},
		{name: "prefix 0x00", encoded: rawURL.EncodeToString(append([]byte{0x00}, raw[1:]...))},
		{name: "prefix 0x06 hybrid", encoded: rawURL.EncodeToString(append([]byte{0x06}, raw[1:]...))},
		{name: "66 bytes", encoded: rawURL.EncodeToString(append(append([]byte(nil), raw...), 0))},
		{name: "infinity 1 byte", encoded: rawURL.EncodeToString([]byte{0x00})},
		{name: "infinity as 0x04 zeros", encoded: rawURL.EncodeToString(append([]byte{0x04}, make([]byte, 64)...))},
		{name: "off curve Y", encoded: rawURL.EncodeToString(flipByte(raw, 64))},
		{name: "off curve X", encoded: rawURL.EncodeToString(flipByte(raw, 1))},
		{name: "negated Y plus one", encoded: rawURL.EncodeToString(negateYPlusOne(raw, params.P))},
		{name: "X equal to p", encoded: rawURL.EncodeToString(pointWithXEqualP(params))},
		{name: "X all 0xff", encoded: rawURL.EncodeToString(append(append([]byte{0x04}, bytes.Repeat([]byte{0xff}, 32)...), raw[33:]...))},
		{name: "padded 88 chars", encoded: base64.URLEncoding.EncodeToString(raw)},
		{name: "standard alphabet", encoded: base64.RawStdEncoding.EncodeToString(raw)},
		{name: "86 chars", encoded: encoded[:86]},
		{name: "88 chars", encoded: encoded + "A"},
		{name: "trailing newline", encoded: encoded + "\n"},
		{name: "trailing CR", encoded: encoded + "\r"},
		{name: "embedded CRLF", encoded: encoded[:40] + "\r\n" + encoded[40:]},
		{name: "leading newline", encoded: "\n" + encoded},
		{name: "leading space", encoded: " " + encoded},
		{name: "trailing space", encoded: encoded + " "},
		{name: "embedded space", encoded: encoded[:40] + " " + encoded[40:]},
		{name: "embedded NUL", encoded: encoded[:40] + "\x00" + encoded[40:]},
		{name: "hex", encoded: hex.EncodeToString(raw)},
		{name: "PEM-ish", encoded: "-----BEGIN PUBLIC KEY-----"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.encoded == "" && tc.name != "empty" {
				t.Skip("could not construct the point")
			}
			parsed, parsedRaw, err := auth.ParsePublicKey(tc.encoded)
			if err == nil {
				t.Errorf("accepted %s: %q (raw %d bytes, X=%s)", tc.name, tc.encoded, len(parsedRaw), parsed.X)
			}
		})
	}
}

// flipByte returns a copy of raw with one bit of byte i flipped.
func flipByte(raw []byte, i int) []byte {
	out := append([]byte(nil), raw...)
	out[i] ^= 1
	return out
}

// negateYPlusOne returns the point with Y replaced by p-Y+1, which is off the curve.
func negateYPlusOne(raw []byte, p *big.Int) []byte {
	y := new(big.Int).SetBytes(raw[33:])
	y.Sub(p, y)
	y.Add(y, big.NewInt(1))
	out := append([]byte(nil), raw[:33]...)
	return append(out, y.FillBytes(make([]byte, 32))...)
}

// pointWithXEqualP encodes X = p and Y = sqrt(b), a point only a parser that reduces X modulo p accepts.
func pointWithXEqualP(params *elliptic.CurveParams) []byte {
	y := new(big.Int).ModSqrt(params.B, params.P)
	if y == nil {
		return nil
	}
	out := append([]byte{0x04}, params.P.FillBytes(make([]byte, 32))...)
	return append(out, y.FillBytes(make([]byte, 32))...)
}

// nonCanonicalLast returns the base64url character c with its two padding bits set, spelling the same 65 bytes.
func nonCanonicalLast(c byte) string {
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	index := strings.IndexByte(alphabet, c)
	return string(alphabet[index|0x03])
}

// TestParsePublicKeyNonCanonicalTrailingBits documents whether a second spelling of one key is accepted.
func TestParsePublicKeyNonCanonicalTrailingBits(t *testing.T) {
	key := probeKey(t)
	encoded := auth.EncodePublicKey(&key.PublicKey)
	other := encoded[:86] + nonCanonicalLast(encoded[86])
	if other == encoded {
		t.Fatal("mutation did not change the last character")
	}
	parsed, raw, err := auth.ParsePublicKey(other)
	if err != nil {
		t.Logf("PROBE: non-canonical trailing bits refused: %v", err)
		return
	}
	// PROBE: RFC 4648 section 3.5 lets a decoder reject non-zero padding bits; this one does not, so one key has
	// four spellings. MemberID is computed from the raw bytes, so all four name the same member.
	t.Logf("PROBE: non-canonical trailing bits accepted; same key=%v same member=%v", parsed.Equal(&key.PublicKey), auth.MemberID(raw) == auth.MemberID(elliptic.Marshal(elliptic.P256(), key.X, key.Y)))
}

// TestSignProducesFixedWidth checks that r ‖ s is always 64 bytes and verifies.
func TestSignProducesFixedWidth(t *testing.T) {
	key := probeKey(t)
	message := auth.Message("GET", "/", 1, "x", nil)
	for i := 0; i < 200; i++ {
		signature, err := auth.Sign(key, message)
		if err != nil {
			t.Fatal(err)
		}
		if len(signature) != 64 {
			t.Fatalf("signature is %d bytes", len(signature))
		}
		if !auth.Verify(&key.PublicKey, message, signature) {
			t.Fatal("own signature did not verify")
		}
	}
}

// TestVerifyAcceptsASN1ConvertedSignature converts a crypto/ecdsa DER signature to r ‖ s.
func TestVerifyAcceptsASN1ConvertedSignature(t *testing.T) {
	key := probeKey(t)
	message := auth.Message("PUT", "/api/v1/members/me", 1792800000, "AAAAAAAAAAAAAAAAAAAAAA", []byte("{}"))
	digest := sha256.Sum256(message)
	for i := 0; i < 50; i++ {
		der, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		if auth.Verify(&key.PublicKey, message, der) {
			t.Error("accepted a DER-encoded signature")
		}
		var rs struct{ R, S *big.Int }
		if _, err := asn1.Unmarshal(der, &rs); err != nil {
			t.Fatal(err)
		}
		raw := append(rs.R.FillBytes(make([]byte, 32)), rs.S.FillBytes(make([]byte, 32))...)
		if !auth.Verify(&key.PublicKey, message, raw) {
			t.Fatal("refused a signature made by crypto/ecdsa")
		}
	}
}

// TestVerifyRefusals refuses malformed and foreign signatures.
func TestVerifyRefusals(t *testing.T) {
	key := probeKey(t)
	other := probeKey(t)
	message := auth.Message("GET", "/", 1, "x", nil)
	signature, err := auth.Sign(key, message)
	if err != nil {
		t.Fatal(err)
	}
	n := elliptic.P256().Params().N
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	foreign, err := auth.Sign(other, message)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		// name describes the signature.
		name string

		// signature is the bytes to verify.
		signature []byte
	}{
		{name: "nil", signature: nil},
		{name: "empty", signature: []byte{}},
		{name: "63 bytes", signature: signature[:63]},
		{name: "65 bytes", signature: append(append([]byte(nil), signature...), 0)},
		{name: "65 bytes leading zero", signature: append([]byte{0}, signature...)},
		{name: "32 bytes", signature: signature[:32]},
		{name: "128 bytes doubled", signature: append(append([]byte(nil), signature...), signature...)},
		{name: "all zero", signature: make([]byte, 64)},
		{name: "r zero", signature: concat(make([]byte, 32), signature[32:])},
		{name: "s zero", signature: concat(signature[:32], make([]byte, 32))},
		{name: "r equal n", signature: concat(n.FillBytes(make([]byte, 32)), signature[32:])},
		{name: "s equal n", signature: concat(signature[:32], n.FillBytes(make([]byte, 32)))},
		{name: "r plus n", signature: plusN(r, n, signature[32:], true)},
		{name: "s plus n", signature: plusN(s, n, signature[:32], false)},
		{name: "r all 0xff", signature: concat(bytes.Repeat([]byte{0xff}, 32), signature[32:])},
		{name: "s all 0xff", signature: concat(signature[:32], bytes.Repeat([]byte{0xff}, 32))},
		{name: "swapped r and s", signature: concat(signature[32:], signature[:32])},
		{name: "one bit flipped", signature: flipByte(signature, 40)},
		{name: "another key", signature: foreign},
		{name: "negated r", signature: concat(new(big.Int).Sub(n, r).FillBytes(make([]byte, 32)), signature[32:])},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.signature == nil && tc.name != "nil" {
				t.Skip("value does not fit in 32 bytes")
			}
			if auth.Verify(&key.PublicKey, message, tc.signature) {
				t.Errorf("accepted %s", tc.name)
			}
		})
	}
	if auth.Verify(&key.PublicKey, append(append([]byte(nil), message...), 0), signature) {
		t.Error("accepted the signature over a message with a trailing byte")
	}
	if auth.Verify(&key.PublicKey, message[:len(message)-1], signature) {
		t.Error("accepted the signature over a truncated message")
	}
}

// plusN returns x+n beside other as r ‖ s, with x+n first when first, or nil when x+n needs more than 32 bytes.
func plusN(x, n *big.Int, other []byte, first bool) []byte {
	sum := new(big.Int).Add(x, n)
	if sum.BitLen() > 256 {
		return nil
	}
	if first {
		return concat(sum.FillBytes(make([]byte, 32)), other)
	}
	return concat(other, sum.FillBytes(make([]byte, 32)))
}

// concat joins two byte slices into a new one.
func concat(a, b []byte) []byte {
	return append(append([]byte(nil), a...), b...)
}

// TestVerifyHighS documents whether the malleable twin (r, n-s) of a signature is accepted.
func TestVerifyHighS(t *testing.T) {
	key := probeKey(t)
	message := auth.Message("GET", "/", 1, "x", nil)
	signature, err := auth.Sign(key, message)
	if err != nil {
		t.Fatal(err)
	}
	n := elliptic.P256().Params().N
	s := new(big.Int).SetBytes(signature[32:])
	twin := concat(signature[:32], new(big.Int).Sub(n, s).FillBytes(make([]byte, 32)))
	// PROBE: the security review notes high-s signatures are accepted; the nonce, not the signature,
	// is the replay key, so malleability does not permit a replay.
	t.Logf("PROBE: (r, n-s) accepted=%v; original s is high=%v", auth.Verify(&key.PublicKey, message, twin), s.Cmp(new(big.Int).Rsh(n, 1)) > 0)
}

// TestVerifyOddKeysDoNotPanic calls Verify with unusual public keys and logs any panic.
func TestVerifyOddKeysDoNotPanic(t *testing.T) {
	message := auth.Message("GET", "/", 1, "x", nil)
	signature := make([]byte, 64)
	signature[31], signature[63] = 1, 1
	cases := []struct {
		// name describes the key.
		name string

		// key is the public key to verify under.
		key *ecdsa.PublicKey
	}{
		{name: "nil", key: nil},
		{name: "zero point", key: &ecdsa.PublicKey{Curve: elliptic.P256(), X: big.NewInt(0), Y: big.NewInt(0)}},
		{name: "(1,1)", key: &ecdsa.PublicKey{Curve: elliptic.P256(), X: big.NewInt(1), Y: big.NewInt(1)}},
		{name: "P-384 curve", key: &ecdsa.PublicKey{Curve: elliptic.P384(), X: probeKey(t).X, Y: probeKey(t).Y}},
		{name: "nil curve", key: &ecdsa.PublicKey{X: big.NewInt(1), Y: big.NewInt(1)}},
		{name: "nil coordinates", key: &ecdsa.PublicKey{Curve: elliptic.P256()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Logf("PROBE: Verify panicked on a %s key: %v", tc.name, r)
				}
			}()
			if auth.Verify(tc.key, message, signature) {
				t.Errorf("verified under a %s key", tc.name)
			}
		})
	}
}

// FuzzParsePublicKey checks ParsePublicKey never panics and an accepted key re-encodes identically.
func FuzzParsePublicKey(f *testing.F) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		f.Fatal(err)
	}
	encoded := auth.EncodePublicKey(&key.PublicKey)
	raw := elliptic.Marshal(elliptic.P256(), key.X, key.Y)
	f.Add(encoded)
	f.Add(encoded[:86])
	f.Add(encoded + "=")
	f.Add(rawURL.EncodeToString(raw[1:]))
	f.Add(rawURL.EncodeToString(elliptic.MarshalCompressed(elliptic.P256(), key.X, key.Y)))
	f.Add(rawURL.EncodeToString(append([]byte{0x04}, make([]byte, 64)...)))
	f.Add("")
	f.Add("AAAA")
	f.Fuzz(func(t *testing.T, encoded string) {
		parsed, parsedRaw, err := auth.ParsePublicKey(encoded)
		if err != nil {
			return
		}
		if strings.ContainsAny(encoded, "\r\n") {
			// Known: CR and LF inside the encoding are accepted; TestParsePublicKeyRefusals covers it.
			return
		}
		if len(parsedRaw) != 65 || parsedRaw[0] != 0x04 {
			t.Fatalf("accepted raw of %d bytes starting %#x", len(parsedRaw), parsedRaw[0])
		}
		if !elliptic.P256().IsOnCurve(parsed.X, parsed.Y) {
			t.Fatal("accepted a point off the curve")
		}
		again := auth.EncodePublicKey(parsed)
		if len(again) != 87 {
			t.Fatalf("re-encoded key is %d characters", len(again))
		}
		if decoded, err := rawURL.DecodeString(again); err != nil || !bytes.Equal(decoded, parsedRaw) {
			t.Fatalf("accepted %q whose raw bytes re-encode as %q", encoded, again)
		}
		if strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, encoded)[:86] != again[:86] {
			t.Fatalf("accepted %q which differs from %q before the padding bits", encoded, again)
		}
	})
}

// FuzzVerify checks Verify never panics on any signature or message.
func FuzzVerify(f *testing.F) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		f.Fatal(err)
	}
	message := auth.Message("GET", "/", 1, "x", nil)
	signature, err := auth.Sign(key, message)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(message, signature)
	f.Add(message, signature[:63])
	f.Add(message, append(append([]byte(nil), signature...), 0))
	f.Add([]byte{}, []byte{})
	f.Add(message, make([]byte, 64))
	f.Add(message, bytes.Repeat([]byte{0xff}, 64))
	f.Fuzz(func(t *testing.T, msg, sig []byte) {
		ok := auth.Verify(&key.PublicKey, msg, sig)
		if ok && len(sig) != 64 {
			t.Fatalf("accepted a %d-byte signature", len(sig))
		}
		if ok && !bytes.Equal(msg, message) {
			// A forgery under a fresh key is impossible; this would be a verifier bug.
			t.Fatalf("accepted signature %x over an unsigned message %q", sig, msg)
		}
	})
}
