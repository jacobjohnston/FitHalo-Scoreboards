package password_test

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/password"
)

// pepper is a pepper the probes share.
var pepper = []byte("a pepper that the server configuration holds")

// otherPepper is a second, different pepper.
var otherPepper = []byte("a different pepper altogether")

// hashCache keeps one hash per password and pepper, since each costs 600,000 iterations.
var hashCache sync.Map

// mustHash hashes or fails the test, reusing an earlier hash of the same password and pepper.
func mustHash(t *testing.T, text string, pepper []byte) string {
	t.Helper()
	key := text + "\x00" + string(pepper)
	if cached, ok := hashCache.Load(key); ok {
		return cached.(string)
	}
	stored := freshHash(t, text, pepper)
	hashCache.Store(key, stored)
	return stored
}

// freshHash always computes a new hash.
func freshHash(t *testing.T, text string, pepper []byte) string {
	t.Helper()
	stored, err := password.Hash(text, pepper)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

// decodeField decodes a salt or hash field, trying the encodings a stored value might use, and names the one that worked.
func decodeField(field string) ([]byte, string) {
	for _, candidate := range []struct {
		// name is the encoding's name.
		name string

		// decode attempts it.
		decode func(string) ([]byte, error)
	}{
		{"base64 standard", base64.StdEncoding.DecodeString},
		{"base64 raw standard", base64.RawStdEncoding.DecodeString},
		{"base64 url", base64.URLEncoding.DecodeString},
		{"base64 raw url", base64.RawURLEncoding.DecodeString},
		{"hex", hex.DecodeString},
	} {
		if decoded, err := candidate.decode(field); err == nil {
			return decoded, candidate.name
		}
	}
	return nil, "none"
}

// TestStoredValueFormat splits a hash on dollar signs and checks each field against the documented format.
func TestStoredValueFormat(t *testing.T) {
	plain := mustHash(t, "correct horse battery staple", nil)
	peppered := mustHash(t, "correct horse battery staple", pepper)
	for _, test := range []struct {
		// name describes the hash.
		name string

		// stored is the value under test.
		stored string

		// fields is how many dollar-separated fields it must have.
		fields int
	}{
		{name: "without pepper", stored: plain, fields: 4},
		{name: "with pepper", stored: peppered, fields: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			parts := strings.Split(test.stored, "$")
			if len(parts) != test.fields {
				t.Fatalf("%d fields in %q, want %d", len(parts), test.stored, test.fields)
			}
			if parts[0] == "" || strings.ContainsAny(parts[0], " \t") {
				t.Errorf("algorithm field %q", parts[0])
			}
			if iterations, err := strconv.Atoi(parts[1]); err != nil || iterations != password.Iterations {
				t.Errorf("iterations field %q, want %d", parts[1], password.Iterations)
			}
			salt, saltEncoding := decodeField(parts[2])
			hash, hashEncoding := decodeField(parts[3])
			t.Logf("salt is %d bytes as %s; hash is %d bytes as %s", len(salt), saltEncoding, len(hash), hashEncoding)
			if len(salt) != 16 {
				t.Errorf("salt decodes to %d bytes (%s), want 16", len(salt), saltEncoding)
			}
			if len(hash) < 32 {
				t.Errorf("hash decodes to %d bytes (%s), want at least 32", len(hash), hashEncoding)
			}
			if test.fields == 5 && parts[4] != password.PepperID(pepper) {
				t.Errorf("pepper field %q, want PepperID %q", parts[4], password.PepperID(pepper))
			}
			for _, part := range parts {
				if part == "" {
					t.Errorf("empty field in %q", test.stored)
				}
			}
		})
	}
}

// TestSaltsDifferAndBothVerify hashes one password twice and verifies both.
func TestSaltsDifferAndBothVerify(t *testing.T) {
	seen := map[string]bool{}
	for range 3 {
		stored := freshHash(t, "same password", pepper)
		if seen[stored] {
			t.Fatal("two hashes of one password were identical")
		}
		seen[stored] = true
		if ok, err := password.Verify(stored, "same password", pepper); err != nil || !ok {
			t.Fatalf("verify: %v, %v", ok, err)
		}
	}
}

// TestVerifyRejectsNearMisses refuses passwords that differ in space, case or normalisation.
func TestVerifyRejectsNearMisses(t *testing.T) {
	stored := mustHash(t, "café Passw0rd", nil)
	for _, test := range []struct {
		// name describes the variant.
		name string

		// attempt is the password tried.
		attempt string
	}{
		{name: "trailing space", attempt: "café Passw0rd "},
		{name: "leading space", attempt: " café Passw0rd"},
		{name: "different case", attempt: "café passw0rd"},
		{name: "empty", attempt: ""},
		{name: "prefix", attempt: "café Passw0r"},
		{name: "longer", attempt: "café Passw0rd1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if ok, err := password.Verify(stored, test.attempt, nil); ok || err != nil {
				t.Fatalf("verify: %v, %v", ok, err)
			}
		})
	}
}

// TestTrailingNULRecordsResult records that a trailing NUL byte verifies, which HMAC's zero-padded key makes inherent to PBKDF2.
func TestTrailingNULRecordsResult(t *testing.T) {
	// PROBE: HMAC pads a short key with zero bytes, so PBKDF2("p") == PBKDF2("p\x00") by construction; a pepper mixed into the key would not change that.
	stored := mustHash(t, "caf\u00e9 Passw0rd", nil)
	ok, err := password.Verify(stored, "caf\u00e9 Passw0rd\x00", nil)
	t.Logf("trailing NUL verifies: %v, %v", ok, err)
}

// TestUnicodeNormalizationRecordsResult records whether NFC and NFD spellings of é are the same password.
func TestUnicodeNormalizationRecordsResult(t *testing.T) {
	// PROBE: the doc does not say whether passwords are normalised; bytes-as-given is the usual PBKDF2 behaviour.
	stored := mustHash(t, "café", nil)
	ok, err := password.Verify(stored, "café", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("NFD spelling verifies against an NFC hash: %v", ok)
}

// TestLongAndUnusualPasswords hashes and verifies the maximum length and binary content.
func TestLongAndUnusualPasswords(t *testing.T) {
	for _, test := range []struct {
		// name describes the password.
		name string

		// text is the password.
		text string
	}{
		{name: "empty", text: ""},
		{name: "1024 characters", text: strings.Repeat("ab", 512)},
		{name: "emoji", text: "👍🏽🇺🇸👨‍👩‍👧‍👦"},
		{name: "dollar signs", text: "$$$$$"},
		{name: "invalid UTF-8", text: "\xff\xfe\xc0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			stored := mustHash(t, test.text, pepper)
			if strings.Count(stored, "$") != 4 {
				t.Errorf("stored value has %d dollar signs: %q", strings.Count(stored, "$"), stored)
			}
			if ok, err := password.Verify(stored, test.text, pepper); err != nil || !ok {
				t.Fatalf("verify: %v, %v", ok, err)
			}
		})
	}
}

// TestPepperMismatch expects ErrPepperChanged when the pepper differs, and records the no-pepper cases.
func TestPepperMismatch(t *testing.T) {
	peppered := mustHash(t, "secret", pepper)
	if ok, err := password.Verify(peppered, "secret", otherPepper); !errors.Is(err, password.ErrPepperChanged) || ok {
		t.Errorf("wrong pepper: %v, %v; want ErrPepperChanged", ok, err)
	}
	if ok, err := password.Verify(peppered, "secret", nil); !errors.Is(err, password.ErrPepperChanged) || ok {
		t.Errorf("peppered hash with no pepper: %v, %v; want ErrPepperChanged", ok, err)
	}
	plain := mustHash(t, "secret", nil)
	ok, err := password.Verify(plain, "secret", pepper)
	// PROBE: a hash made before a pepper was configured; either verifying (migration) or ErrPepperChanged is defensible.
	t.Logf("unpeppered hash with a pepper now configured: %v, %v", ok, err)
	if err == nil && !ok {
		t.Error("an unpeppered hash silently fails with a pepper configured; it should verify or say ErrPepperChanged")
	}
}

// TestPepperDoesNotLeakIntoHash checks the pepper itself never appears in the stored value in any obvious encoding.
func TestPepperDoesNotLeakIntoHash(t *testing.T) {
	stored := mustHash(t, "secret", pepper)
	for _, encoded := range []string{string(pepper), hex.EncodeToString(pepper), base64.StdEncoding.EncodeToString(pepper), base64.RawURLEncoding.EncodeToString(pepper)} {
		if strings.Contains(stored, encoded) || strings.Contains(stored, encoded[:8]) {
			t.Fatalf("the stored value contains the pepper as %q", encoded[:8])
		}
	}
}

// TestPepperIDProperties checks PepperID is deterministic, distinct per pepper, bounded and not a slice of the pepper's encodings.
func TestPepperIDProperties(t *testing.T) {
	id := password.PepperID(pepper)
	if id != password.PepperID(pepper) {
		t.Fatal("PepperID is not deterministic")
	}
	if id == password.PepperID(otherPepper) {
		t.Fatal("two peppers share a PepperID")
	}
	if id == "" || len(id) > 64 || strings.Contains(id, "$") {
		t.Fatalf("PepperID %q is empty, long or contains a dollar sign", id)
	}
	for _, encoded := range []string{hex.EncodeToString(pepper), base64.StdEncoding.EncodeToString(pepper), base64.RawURLEncoding.EncodeToString(pepper), string(pepper)} {
		if strings.Contains(encoded, id) || strings.Contains(id, encoded[:4]) {
			t.Fatalf("PepperID %q reveals part of the pepper", id)
		}
	}
	oneBitOff := append([]byte{}, pepper...)
	oneBitOff[0] ^= 1
	if password.PepperID(oneBitOff) == id {
		t.Fatal("a one-bit change in the pepper keeps the PepperID")
	}
	t.Logf("PepperID is %d characters: %q", len(id), id)
}

// TestPepperIDOfEmptyPepperRecordsResult records what an empty pepper is named.
func TestPepperIDOfEmptyPepperRecordsResult(t *testing.T) {
	// PROBE: Hash treats a nil pepper as none; record whether PepperID(nil) and PepperID([]byte{}) agree and are stable.
	t.Logf("PepperID(nil)=%q PepperID(empty)=%q", password.PepperID(nil), password.PepperID([]byte{}))
}

// TestMalformedStoredValues returns an error, never a match and never a panic, for stored values that are not hashes.
func TestMalformedStoredValues(t *testing.T) {
	good := mustHash(t, "secret", nil)
	parts := strings.Split(good, "$")
	for _, test := range []struct {
		// name describes the corruption.
		name string

		// stored is the malformed value.
		stored string
	}{
		{name: "empty", stored: ""},
		{name: "one letter", stored: "x"},
		{name: "three fields", stored: "a$b$c"},
		{name: "only dollars", stored: "$$$"},
		{name: "unknown algorithm", stored: "argon2id$" + strings.Join(parts[1:], "$")},
		{name: "iterations not a number", stored: parts[0] + "$many$" + parts[2] + "$" + parts[3]},
		{name: "iterations negative", stored: parts[0] + "$-1$" + parts[2] + "$" + parts[3]},
		{name: "iterations zero", stored: parts[0] + "$0$" + parts[2] + "$" + parts[3]},
		{name: "iterations huge", stored: parts[0] + "$99999999999999999999$" + parts[2] + "$" + parts[3]},
		{name: "salt not decodable", stored: parts[0] + "$" + parts[1] + "$!!!!$" + parts[3]},
		{name: "hash not decodable", stored: parts[0] + "$" + parts[1] + "$" + parts[2] + "$!!!!"},
		{name: "empty salt", stored: parts[0] + "$" + parts[1] + "$$" + parts[3]},
		{name: "empty hash", stored: parts[0] + "$" + parts[1] + "$" + parts[2] + "$"},
		{name: "six fields", stored: good + "$x$y"},
		{name: "trailing dollar", stored: good + "$"},
		{name: "leading dollar", stored: "$" + good},
		{name: "whitespace around", stored: " " + good + " "},
		{name: "upper case algorithm", stored: strings.ToUpper(parts[0]) + "$" + strings.Join(parts[1:], "$")},
	} {
		t.Run(test.name, func(t *testing.T) {
			ok, err := password.Verify(test.stored, "secret", nil)
			if ok {
				t.Fatalf("malformed value %q verified", test.stored)
			}
			if err == nil {
				// PROBE: a malformed stored value could be reported as "no match" rather than an error; record which.
				t.Logf("malformed value %q gave no error", test.stored)
			}
		})
	}
}

// TestSignedIterationCountRecordsResult records whether a "+600000" iteration field is accepted as 600000.
func TestSignedIterationCountRecordsResult(t *testing.T) {
	// PROBE: the format shows a bare decimal; strconv accepts a leading plus, which is harmless but non-canonical.
	parts := strings.Split(mustHash(t, "secret", nil), "$")
	ok, err := password.Verify(parts[0]+"$+"+parts[1]+"$"+parts[2]+"$"+parts[3], "secret", nil)
	t.Logf("+600000 iterations: %v, %v", ok, err)
}

// TestTruncatedStoredHashDoesNotVerify checks a stored value whose hash field lost its final character fails.
func TestTruncatedStoredHashDoesNotVerify(t *testing.T) {
	good := mustHash(t, "secret", nil)
	if ok, err := password.Verify(good[:len(good)-1], "secret", nil); ok {
		t.Fatalf("a stored value truncated by one character verified (%v)", err)
	}
}

// TestShortStoredHashIsRefused checks a stored value whose hash field is a one-byte prefix of the real hash does not verify.
func TestShortStoredHashIsRefused(t *testing.T) {
	good := mustHash(t, "secret", nil)
	parts := strings.Split(good, "$")
	hash, _ := decodeField(parts[3])
	for _, length := range []int{1, 4, 16, 31} {
		short := parts[0] + "$" + parts[1] + "$" + parts[2] + "$" + base64.RawStdEncoding.EncodeToString(hash[:length])
		if ok, err := password.Verify(short, "secret", nil); ok {
			t.Errorf("a stored value with a %d-byte hash verified (%v); a one-byte hash would admit one guess in 256", length, err)
		}
	}
}

// TestStoredIterationCountIsHonoured verifies a hand-made value at 1,000 iterations computed with crypto/pbkdf2.
func TestStoredIterationCountIsHonoured(t *testing.T) {
	reference := mustHash(t, "secret", nil)
	parts := strings.Split(reference, "$")
	salt, saltEncoding := decodeField(parts[2])
	referenceHash, _ := decodeField(parts[3])
	encode := map[string]func([]byte) string{
		"base64 standard":     base64.StdEncoding.EncodeToString,
		"base64 raw standard": base64.RawStdEncoding.EncodeToString,
		"base64 url":          base64.URLEncoding.EncodeToString,
		"base64 raw url":      base64.RawURLEncoding.EncodeToString,
		"hex":                 hex.EncodeToString,
	}[saltEncoding]
	if encode == nil {
		t.Fatalf("salt encoding %s unknown", saltEncoding)
	}
	expected, err := pbkdf2.Key(sha256.New, "secret", salt, password.Iterations, len(referenceHash))
	if err != nil {
		t.Fatal(err)
	}
	if encode(expected) != parts[3] {
		t.Skipf("the unpeppered hash is not plain PBKDF2-HMAC-SHA256(password, salt); cannot hand-make a value")
	}
	weak, err := pbkdf2.Key(sha256.New, "secret", salt, 1000, len(referenceHash))
	if err != nil {
		t.Fatal(err)
	}
	handMade := parts[0] + "$1000$" + parts[2] + "$" + encode(weak)
	ok, err := password.Verify(handMade, "secret", nil)
	// PROBE: honouring a low stored count lets old hashes verify; refusing it protects against downgraded stored values. Record which.
	t.Logf("1000-iteration hand-made value: %v, %v", ok, err)
	if err == nil && !ok {
		t.Fatal("a correctly computed 1000-iteration value neither verified nor errored")
	}
	wrong, _ := password.Verify(handMade, "secrets", nil)
	if wrong {
		t.Fatal("a wrong password verified against the hand-made value")
	}
}

// TestOneIterationValueRecordsResult records whether a stored value claiming one iteration is accepted.
func TestOneIterationValueRecordsResult(t *testing.T) {
	reference := mustHash(t, "secret", nil)
	parts := strings.Split(reference, "$")
	ok, err := password.Verify(parts[0]+"$1$"+parts[2]+"$"+parts[3], "secret", nil)
	t.Logf("one-iteration value: %v, %v", ok, err)
	if ok {
		t.Fatal("a one-iteration value verified with a 600000-iteration hash")
	}
}
