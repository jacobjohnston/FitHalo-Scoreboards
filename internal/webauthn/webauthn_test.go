package webauthn

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// TestCBORAppendixA decodes RFC 8949 Appendix A's examples in this subset and refuses some it leaves out.
func TestCBORAppendixA(t *testing.T) {
	cases := map[string]any{
		"00":                 int64(0),
		"17":                 int64(23),
		"1818":               int64(24),
		"1903e8":             int64(1000),
		"1b000000e8d4a51000": int64(1000000000000),
		"20":                 int64(-1),
		"3903e7":             int64(-1000),
		"40":                 []byte{},
		"4401020304":         []byte{1, 2, 3, 4},
		"60":                 "",
		"6449455446":         "IETF",
		"80":                 []any{},
		"83010203":           []any{int64(1), int64(2), int64(3)},
		"a0":                 map[any]any{},
		"a201020304":         map[any]any{int64(1): int64(2), int64(3): int64(4)},
		"a26161016162820203": map[any]any{"a": int64(1), "b": []any{int64(2), int64(3)}},
		"f4":                 false,
		"f5":                 true,
		"f6":                 nil,
	}
	for encoded, expected := range cases {
		data, _ := hex.DecodeString(encoded)
		value, rest, err := decodeCBOR(data)
		if err != nil || len(rest) != 0 || !reflect.DeepEqual(value, expected) {
			t.Errorf("%s decoded to %#v with %d left over, %v", encoded, value, len(rest), err)
		}
	}
	for _, encoded := range []string{"f93c00", "5f42010243030405ff", "9fff", "c11a514b67b0", "19", "a101", "a2010201", "a1f401"} {
		data, _ := hex.DecodeString(encoded)
		if _, _, err := decodeCBOR(data); err == nil {
			t.Errorf("accepted %s", encoded)
		}
	}
}

// encodeCBOR is the inverse for tests: just enough to build what an authenticator sends.
func encodeCBOR(value any) []byte {
	head := func(major byte, argument uint64) []byte {
		switch {
		case argument < 24:
			return []byte{major<<5 | byte(argument)}
		case argument < 1<<8:
			return []byte{major<<5 | 24, byte(argument)}
		case argument < 1<<16:
			return binary.BigEndian.AppendUint16([]byte{major<<5 | 25}, uint16(argument))
		default:
			return binary.BigEndian.AppendUint32([]byte{major<<5 | 26}, uint32(argument))
		}
	}
	switch typed := value.(type) {
	case int:
		if typed < 0 {
			return head(1, uint64(-1-typed))
		}
		return head(0, uint64(typed))
	case []byte:
		return append(head(2, uint64(len(typed))), typed...)
	case string:
		return append(head(3, uint64(len(typed))), typed...)
	case map[any]any:
		keys := make([]any, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(first, second int) bool {
			return string(encodeCBOR(keys[first])) < string(encodeCBOR(keys[second]))
		})
		encoded := head(5, uint64(len(typed)))
		for _, key := range keys {
			encoded = append(encoded, encodeCBOR(key)...)
			encoded = append(encoded, encodeCBOR(typed[key])...)
		}
		return encoded
	}
	panic("unsupported")
}

// authenticator is a software passkey: it builds and signs responses the way a real one does.
type authenticator struct {
	// t fails the test when signing fails.
	t testing.TB

	// party is the relying party the passkey registers with.
	party RelyingParty

	// id is the credential identifier.
	id []byte

	// algorithm is the passkey's COSE algorithm.
	algorithm int

	// signer holds the private key.
	signer crypto.Signer

	// coseKey is the public key as the authenticator encodes it.
	coseKey map[any]any

	// signCount is the signature count, raised before each assertion.
	signCount uint32
}

// newAuthenticator returns a passkey for party with a fresh key in algorithm.
func newAuthenticator(t testing.TB, party RelyingParty, algorithm int) *authenticator {
	t.Helper()
	device := &authenticator{t: t, party: party, id: []byte("credential-" + party.ID), algorithm: algorithm}
	switch algorithm {
	case AlgorithmES256:
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		x, y := make([]byte, 32), make([]byte, 32)
		key.X.FillBytes(x)
		key.Y.FillBytes(y)
		device.signer = key
		device.coseKey = map[any]any{1: 2, 3: AlgorithmES256, -1: 1, -2: x, -3: y}
	case AlgorithmEdDSA:
		public, private, _ := ed25519.GenerateKey(rand.Reader)
		device.signer = private
		device.coseKey = map[any]any{1: 1, 3: AlgorithmEdDSA, -1: 6, -2: []byte(public)}
	case AlgorithmRS256:
		key, _ := rsa.GenerateKey(rand.Reader, 2048)
		device.signer = key
		device.coseKey = map[any]any{1: 3, 3: AlgorithmRS256, -1: key.N.Bytes(), -2: []byte{1, 0, 1}}
	}
	return device
}

// clientData returns client data JSON for a ceremony of kind.
func (a *authenticator) clientData(kind string, challenge []byte, origin string) []byte {
	encoded, _ := json.Marshal(map[string]any{
		"type": kind, "challenge": base64.RawURLEncoding.EncodeToString(challenge), "origin": origin, "crossOrigin": false,
	})
	return encoded
}

// authenticatorData returns authenticator data, with the credential when attested.
func (a *authenticator) authenticatorData(rpID string, flags byte, attested bool) []byte {
	rpIDHash := sha256.Sum256([]byte(rpID))
	data := append(rpIDHash[:], flags)
	data = binary.BigEndian.AppendUint32(data, a.signCount)
	if attested {
		data = append(data, make([]byte, aaguidLength)...)
		data = binary.BigEndian.AppendUint16(data, uint16(len(a.id)))
		data = append(data, a.id...)
		data = append(data, encodeCBOR(a.coseKey)...)
	}
	return data
}

// register returns the client data and attestation object for a registration.
func (a *authenticator) register(challenge []byte) ([]byte, []byte) {
	authenticatorData := a.authenticatorData(a.party.ID, flagUserPresent|flagUserVerified|flagAttestedData, true)
	attestation := encodeCBOR(map[any]any{"fmt": "none", "attStmt": map[any]any{}, "authData": authenticatorData})
	return a.clientData("webauthn.create", challenge, a.party.Origin), attestation
}

// assert returns the client data, authenticator data and signature for a sign-in.
func (a *authenticator) assert(challenge []byte, rpID, origin string, flags byte) ([]byte, []byte, []byte) {
	return a.assertClientData(a.clientData("webauthn.get", challenge, origin), rpID, flags)
}

// assertClientData signs a sign-in over the given client data, returning it with the authenticator data and
// signature.
func (a *authenticator) assertClientData(clientDataJSON []byte, rpID string, flags byte) ([]byte, []byte, []byte) {
	a.signCount++
	authenticatorData := a.authenticatorData(rpID, flags, false)
	clientDigest := sha256.Sum256(clientDataJSON)
	signed := append(append([]byte(nil), authenticatorData...), clientDigest[:]...)
	var signature []byte
	var err error
	switch a.algorithm {
	case AlgorithmEdDSA:
		signature, err = a.signer.Sign(rand.Reader, signed, crypto.Hash(0))
	default:
		digest := sha256.Sum256(signed)
		signature, err = a.signer.Sign(rand.Reader, digest[:], crypto.SHA256)
	}
	if err != nil {
		a.t.Fatal(err)
	}
	return clientDataJSON, authenticatorData, signature
}

// party is the relying party every test registers with.
var party = RelyingParty{ID: "board.example.com", Origin: "https://board.example.com"}

// TestRegisterAndSignIn runs both ceremonies for every supported algorithm.
func TestRegisterAndSignIn(t *testing.T) {
	for _, algorithm := range SupportedAlgorithms {
		device := newAuthenticator(t, party, algorithm)
		challenge := []byte("registration challenge, 32 bytes")
		clientDataJSON, attestation := device.register(challenge)
		credential, err := party.VerifyRegistration(challenge, clientDataJSON, attestation)
		if err != nil {
			t.Fatalf("algorithm %d: registration: %v", algorithm, err)
		}
		if string(credential.ID) != string(device.id) || credential.Algorithm != algorithm {
			t.Fatalf("algorithm %d: credential %+v", algorithm, credential)
		}

		signInChallenge := []byte("sign-in challenge, also 32 bytes")
		data, authenticatorData, signature := device.assert(signInChallenge, party.ID, party.Origin, flagUserPresent|flagUserVerified)
		count, err := party.VerifyAssertion(signInChallenge, credential, data, authenticatorData, signature)
		if err != nil || count != 1 {
			t.Fatalf("algorithm %d: sign-in: count %d, %v", algorithm, count, err)
		}
		credential.SignCount = count

		data, authenticatorData, signature = device.assert(signInChallenge, party.ID, party.Origin, flagUserPresent|flagUserVerified)
		authenticatorData[len(authenticatorData)-1] ^= 1
		if _, err := party.VerifyAssertion(signInChallenge, credential, data, authenticatorData, signature); err == nil {
			t.Fatalf("algorithm %d: a tampered sign-in verified", algorithm)
		}
	}
}

// TestRefusals covers each check a sign-in must pass.
func TestRefusals(t *testing.T) {
	device := newAuthenticator(t, party, AlgorithmES256)
	challenge := []byte("registration challenge, 32 bytes")
	clientDataJSON, attestation := device.register(challenge)
	credential, err := party.VerifyRegistration(challenge, clientDataJSON, attestation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := party.VerifyRegistration([]byte("another challenge"), clientDataJSON, attestation); err == nil {
		t.Error("registration accepted the wrong challenge")
	}

	verified := byte(flagUserPresent | flagUserVerified)
	cases := map[string]func() ([]byte, []byte, []byte, []byte){
		"another origin": func() ([]byte, []byte, []byte, []byte) {
			data, authenticatorData, signature := device.assert(challenge, party.ID, "https://evil.example", verified)
			return challenge, data, authenticatorData, signature
		},
		"another site's passkey": func() ([]byte, []byte, []byte, []byte) {
			data, authenticatorData, signature := device.assert(challenge, "evil.example", party.Origin, verified)
			return challenge, data, authenticatorData, signature
		},
		"no user verification": func() ([]byte, []byte, []byte, []byte) {
			data, authenticatorData, signature := device.assert(challenge, party.ID, party.Origin, flagUserPresent)
			return challenge, data, authenticatorData, signature
		},
		"the wrong challenge": func() ([]byte, []byte, []byte, []byte) {
			data, authenticatorData, signature := device.assert(challenge, party.ID, party.Origin, verified)
			return []byte("a different challenge"), data, authenticatorData, signature
		},
		"the wrong type": func() ([]byte, []byte, []byte, []byte) {
			wrongType := device.clientData("webauthn.create", challenge, party.Origin)
			data, authenticatorData, signature := device.assertClientData(wrongType, party.ID, verified)
			return challenge, data, authenticatorData, signature
		},
		"a cross-origin frame": func() ([]byte, []byte, []byte, []byte) {
			crossOrigin, _ := json.Marshal(map[string]any{
				"type": "webauthn.get", "challenge": base64.RawURLEncoding.EncodeToString(challenge), "origin": party.Origin, "crossOrigin": true,
			})
			data, authenticatorData, signature := device.assertClientData(crossOrigin, party.ID, verified)
			return challenge, data, authenticatorData, signature
		},
	}
	for name, build := range cases {
		expected, data, authenticatorData, signature := build()
		if _, err := party.VerifyAssertion(expected, credential, data, authenticatorData, signature); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	credential.SignCount = 50
	data, authenticatorData, signature := device.assert(challenge, party.ID, party.Origin, verified)
	if _, err := party.VerifyAssertion(challenge, credential, data, authenticatorData, signature); err == nil {
		t.Error("accepted a signature count that went backwards")
	}
}

// TestRegistrationRefusals covers each check a new passkey must pass.
func TestRegistrationRefusals(t *testing.T) {
	challenge := []byte("registration challenge, 32 bytes")
	modulus := func(bits int) []byte {
		value := make([]byte, bits/8)
		rand.Read(value)
		value[0] |= 0x80
		value[len(value)-1] |= 1
		return value
	}
	rsaKey := func(modulus, exponent []byte) map[any]any {
		return map[any]any{1: 3, 3: AlgorithmRS256, -1: modulus, -2: exponent}
	}
	cases := map[string]func(device *authenticator){
		"the wrong type":            func(device *authenticator) {},
		"a cross-origin frame":      func(device *authenticator) {},
		"an oversize credential ID": func(device *authenticator) { device.id = make([]byte, maximumCredentialIDLength+1) },
		"an unsupported algorithm":  func(device *authenticator) { device.coseKey[3] = -35 },
		"RSA under 2048 bits":       func(device *authenticator) { device.coseKey = rsaKey(modulus(1024), []byte{1, 0, 1}) },
		"an RSA exponent of 1":      func(device *authenticator) { device.coseKey = rsaKey(modulus(2048), []byte{1}) },
		"an even RSA exponent":      func(device *authenticator) { device.coseKey = rsaKey(modulus(2048), []byte{1, 0, 0}) },
		"an RSA exponent over 2^31": func(device *authenticator) { device.coseKey = rsaKey(modulus(2048), []byte{0x80, 0, 0, 1}) },
	}
	for name, change := range cases {
		device := newAuthenticator(t, party, AlgorithmES256)
		change(device)
		clientDataJSON, attestation := device.register(challenge)
		switch name {
		case "the wrong type":
			clientDataJSON = device.clientData("webauthn.get", challenge, party.Origin)
		case "a cross-origin frame":
			clientDataJSON, _ = json.Marshal(map[string]any{
				"type": "webauthn.create", "challenge": base64.RawURLEncoding.EncodeToString(challenge), "origin": party.Origin, "crossOrigin": true,
			})
		}
		if _, err := party.VerifyRegistration(challenge, clientDataJSON, attestation); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	device := newAuthenticator(t, party, AlgorithmES256)
	device.coseKey = rsaKey(modulus(2048), []byte{1, 0, 1})
	clientDataJSON, attestation := device.register(challenge)
	if _, err := party.VerifyRegistration(challenge, clientDataJSON, attestation); err != nil {
		t.Errorf("refused a 2048-bit RSA key with exponent 65537: %v", err)
	}
}

// TestSignCountAdvances accepts a higher count, or zero twice for a passkey that keeps none.
func TestSignCountAdvances(t *testing.T) {
	for _, test := range []struct {
		stored, received uint32
		want             bool
	}{{0, 0, true}, {0, 1, true}, {4, 5, true}, {5, 5, false}, {5, 4, false}, {5, 0, false}} {
		if got := SignCountAdvances(test.stored, test.received); got != test.want {
			t.Errorf("SignCountAdvances(%d, %d) = %v, want %v", test.stored, test.received, got, test.want)
		}
	}
}

// FuzzDecodeCBOR decodes arbitrary bytes without panicking, and returns only a suffix of its input.
func FuzzDecodeCBOR(f *testing.F) {
	seeds := []string{"00", "1b000000e8d4a51000", "3903e7", "4401020304", "6449455446", "83010203",
		"a26161016162820203", "f6", "9fff", "a2010201"}
	for _, seed := range seeds {
		data, _ := hex.DecodeString(seed)
		f.Add(data)
	}
	f.Add(encodeCBOR(map[any]any{"fmt": "none", "attStmt": map[any]any{}, "authData": []byte{1, 2, 3}}))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, rest, err := decodeCBOR(data)
		if err == nil && (len(rest) > len(data) || !bytes.Equal(rest, data[len(data)-len(rest):])) {
			t.Fatalf("decodeCBOR(%x) left %x, not a suffix", data, rest)
		}
	})
}

// FuzzVerifyRegistration verifies arbitrary responses without panicking, storing only keys that parse.
func FuzzVerifyRegistration(f *testing.F) {
	challenge := []byte("registration challenge, 32 bytes")
	for _, algorithm := range SupportedAlgorithms {
		clientDataJSON, attestation := newAuthenticator(f, party, algorithm).register(challenge)
		f.Add(clientDataJSON, attestation)
	}
	f.Fuzz(func(t *testing.T, clientDataJSON, attestation []byte) {
		credential, err := party.VerifyRegistration(challenge, clientDataJSON, attestation)
		if err != nil {
			return
		}
		if len(credential.ID) == 0 || len(credential.ID) > maximumCredentialIDLength {
			t.Fatalf("accepted a credential identifier of %d bytes", len(credential.ID))
		}
		if _, err := x509.ParsePKIXPublicKey(credential.PublicKey); err != nil {
			t.Fatalf("stored a public key that does not parse: %v", err)
		}
	})
}

// FuzzVerifyAssertion verifies arbitrary sign-ins without panicking, accepting only an advancing count.
func FuzzVerifyAssertion(f *testing.F) {
	challenge := []byte("sign-in challenge, also 32 bytes")
	device := newAuthenticator(f, party, AlgorithmES256)
	clientDataJSON, attestation := device.register(challenge)
	credential, err := party.VerifyRegistration(challenge, clientDataJSON, attestation)
	if err != nil {
		f.Fatal(err)
	}
	credential.SignCount = 3
	device.signCount = 3
	for _, flags := range []byte{flagUserPresent | flagUserVerified, flagUserPresent} {
		data, authenticatorData, signature := device.assert(challenge, party.ID, party.Origin, flags)
		f.Add(data, authenticatorData, signature)
	}
	f.Fuzz(func(t *testing.T, clientDataJSON, authenticatorData, signature []byte) {
		count, err := party.VerifyAssertion(challenge, credential, clientDataJSON, authenticatorData, signature)
		if err == nil && count <= credential.SignCount {
			t.Fatalf("accepted a signature count of %d after %d", count, credential.SignCount)
		}
	})
}
