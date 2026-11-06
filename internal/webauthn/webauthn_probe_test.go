package webauthn_test

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
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/webauthn"
)

// Authenticator data flag bits from WebAuthn Level 2 §6.1.
const (
	// flagUP is the user-present bit.
	flagUP byte = 0x01

	// flagUV is the user-verified bit.
	flagUV byte = 0x04

	// flagAT marks attested credential data present.
	flagAT byte = 0x40

	// flagED marks extension data present.
	flagED byte = 0x80
)

// maximumCredentialID is the longest credential identifier WebAuthn Level 2 §6.1 allows.
const maximumCredentialID = 1023

// probeParty is the relying party every probe registers with.
var probeParty = webauthn.RelyingParty{ID: "board.example.com", Origin: "https://board.example.com"}

// probeChallenge is a challenge whose standard base64 differs from its base64url ("+/" versus "-_").
var probeChallenge = append([]byte{0xfb, 0xff, 0xfe, 0xff}, []byte("a challenge with 32 bytes!!!")...)

// rawCBOR is already-encoded CBOR spliced in as is.
type rawCBOR []byte

// cborPairs is a map written in the given order, duplicates included.
type cborPairs [][2]any

// cborHead returns the initial bytes for major type with argument.
func cborHead(major byte, argument uint64) []byte {
	switch {
	case argument < 24:
		return []byte{major<<5 | byte(argument)}
	case argument <= math.MaxUint8:
		return []byte{major<<5 | 24, byte(argument)}
	case argument <= math.MaxUint16:
		return binary.BigEndian.AppendUint16([]byte{major<<5 | 25}, uint16(argument))
	case argument <= math.MaxUint32:
		return binary.BigEndian.AppendUint32([]byte{major<<5 | 26}, uint32(argument))
	default:
		return binary.BigEndian.AppendUint64([]byte{major<<5 | 27}, argument)
	}
}

// cbor encodes value in the subset an authenticator sends: integers, byte and text strings, arrays, maps,
// booleans and null, with map keys in RFC 8949 §4.2.1 order.
func cbor(value any) []byte {
	switch typed := value.(type) {
	case rawCBOR:
		return []byte(typed)
	case int:
		if typed < 0 {
			return cborHead(1, uint64(-1-typed))
		}
		return cborHead(0, uint64(typed))
	case uint64:
		return cborHead(0, typed)
	case []byte:
		return append(cborHead(2, uint64(len(typed))), typed...)
	case string:
		return append(cborHead(3, uint64(len(typed))), typed...)
	case []any:
		encoded := cborHead(4, uint64(len(typed)))
		for _, item := range typed {
			encoded = append(encoded, cbor(item)...)
		}
		return encoded
	case cborPairs:
		encoded := cborHead(5, uint64(len(typed)))
		for _, pair := range typed {
			encoded = append(encoded, cbor(pair[0])...)
			encoded = append(encoded, cbor(pair[1])...)
		}
		return encoded
	case map[any]any:
		pairs := make(cborPairs, 0, len(typed))
		for key, item := range typed {
			pairs = append(pairs, [2]any{key, item})
		}
		sort.Slice(pairs, func(first, second int) bool {
			return bytes.Compare(cbor(pairs[first][0]), cbor(pairs[second][0])) < 0
		})
		return cbor(pairs)
	case bool:
		if typed {
			return []byte{0xf5}
		}
		return []byte{0xf4}
	case nil:
		return []byte{0xf6}
	}
	panic(fmt.Sprintf("cbor: unsupported %T", value))
}

// sharedRSAKey is one 2048-bit key for every probe, since generating them is slow.
var sharedRSAKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

// otherRSAKey is a second 2048-bit key, for a signer the board has not registered.
var otherRSAKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

// strangerPasskey returns a passkey for algorithm whose key no other probe passkey holds.
func strangerPasskey(tb testing.TB, algorithm int) *passkey {
	tb.Helper()
	device := newPasskey(tb, algorithm)
	if algorithm == webauthn.AlgorithmRS256 {
		key := otherRSAKey()
		device.signer = key
		device.coseKey = map[any]any{1: 3, 3: webauthn.AlgorithmRS256, -1: key.N.Bytes(), -2: []byte{1, 0, 1}}
	}
	return device
}

// passkey is a software authenticator that builds and signs ceremony responses.
type passkey struct {
	// id is the credential identifier.
	id []byte

	// algorithm is the COSE algorithm the key signs with.
	algorithm int

	// signer holds the private key.
	signer crypto.Signer

	// coseKey is the public key as COSE_Key labels and values.
	coseKey map[any]any

	// signCount is the count the next authenticator data carries.
	signCount uint32
}

// newPasskey returns a fresh passkey for algorithm.
func newPasskey(tb testing.TB, algorithm int) *passkey {
	tb.Helper()
	device := &passkey{id: []byte("probe-credential-id"), algorithm: algorithm}
	switch algorithm {
	case webauthn.AlgorithmES256:
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			tb.Fatal(err)
		}
		device.signer = key
		device.coseKey = map[any]any{1: 2, 3: webauthn.AlgorithmES256, -1: 1, -2: padded(key.X, 32), -3: padded(key.Y, 32)}
	case webauthn.AlgorithmEdDSA:
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			tb.Fatal(err)
		}
		device.signer = private
		device.coseKey = map[any]any{1: 1, 3: webauthn.AlgorithmEdDSA, -1: 6, -2: []byte(public)}
	case webauthn.AlgorithmRS256:
		key := sharedRSAKey()
		device.signer = key
		device.coseKey = map[any]any{1: 3, 3: webauthn.AlgorithmRS256, -1: key.N.Bytes(), -2: []byte{1, 0, 1}}
	default:
		tb.Fatalf("no passkey for algorithm %d", algorithm)
	}
	return device
}

// padded returns value as size big-endian bytes.
func padded(value *big.Int, size int) []byte {
	out := make([]byte, size)
	value.FillBytes(out)
	return out
}

// clientFields returns the client data members for a ceremony of kind.
func clientFields(kind string, challenge []byte, origin string) map[string]any {
	return map[string]any{
		"type": kind, "challenge": base64.RawURLEncoding.EncodeToString(challenge), "origin": origin, "crossOrigin": false,
	}
}

// clientJSON marshals client data members.
func clientJSON(fields map[string]any) []byte {
	encoded, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return encoded
}

// clientData returns client data JSON for a ceremony of kind.
func clientData(kind string, challenge []byte, origin string) []byte {
	return clientJSON(clientFields(kind, challenge, origin))
}

// authData returns authenticator data per §6.1 with the given trailing attested credential data and extensions.
func authData(rpID string, flags byte, count uint32, attested, extensions []byte) []byte {
	digest := sha256.Sum256([]byte(rpID))
	data := append(digest[:], flags)
	data = binary.BigEndian.AppendUint32(data, count)
	data = append(data, attested...)
	return append(data, extensions...)
}

// attested returns the passkey's attested credential data: AAGUID, length, identifier and COSE key.
func (p *passkey) attested() []byte {
	data := make([]byte, 16)
	data = binary.BigEndian.AppendUint16(data, uint16(len(p.id)))
	data = append(data, p.id...)
	return append(data, cbor(p.coseKey)...)
}

// attestation returns an attestation object of format carrying data.
func attestation(format string, data []byte) []byte {
	return cbor(map[any]any{"fmt": format, "attStmt": map[any]any{}, "authData": data})
}

// register returns a complete registration response for rp.
func (p *passkey) register(rp webauthn.RelyingParty, challenge []byte) (clientDataJSON, attestationObject []byte) {
	data := authData(rp.ID, flagUP|flagUV|flagAT, p.signCount, p.attested(), nil)
	return clientData("webauthn.create", challenge, rp.Origin), attestation("none", data)
}

// sign signs authenticator data and client data the way the passkey's algorithm requires.
func (p *passkey) sign(tb testing.TB, data, clientDataJSON []byte) []byte {
	tb.Helper()
	digest := sha256.Sum256(clientDataJSON)
	signed := append(append([]byte(nil), data...), digest[:]...)
	var signature []byte
	var err error
	if p.algorithm == webauthn.AlgorithmEdDSA {
		signature, err = p.signer.Sign(rand.Reader, signed, crypto.Hash(0))
	} else {
		hashed := sha256.Sum256(signed)
		signature, err = p.signer.Sign(rand.Reader, hashed[:], crypto.SHA256)
	}
	if err != nil {
		tb.Fatal(err)
	}
	return signature
}

// assert returns a sign-in response for rp with flags, advancing the count first.
func (p *passkey) assert(tb testing.TB, rp webauthn.RelyingParty, challenge []byte, flags byte) (clientDataJSON, data, signature []byte) {
	tb.Helper()
	p.signCount++
	clientDataJSON = clientData("webauthn.get", challenge, rp.Origin)
	data = authData(rp.ID, flags, p.signCount, nil, nil)
	return clientDataJSON, data, p.sign(tb, data, clientDataJSON)
}

// registered returns a passkey for algorithm and the credential the board stored for it.
func registered(tb testing.TB, algorithm int) (*passkey, webauthn.Credential) {
	tb.Helper()
	device := newPasskey(tb, algorithm)
	clientDataJSON, attestationObject := device.register(probeParty, probeChallenge)
	credential, err := probeParty.VerifyRegistration(probeChallenge, clientDataJSON, attestationObject)
	if err != nil {
		tb.Fatalf("algorithm %d: registration refused: %v", algorithm, err)
	}
	return device, credential
}

// TestProbeRegistrationReturnsTheCredential checks every field of the stored credential for each algorithm.
func TestProbeRegistrationReturnsTheCredential(t *testing.T) {
	for _, algorithm := range webauthn.SupportedAlgorithms {
		device := newPasskey(t, algorithm)
		device.signCount = 7
		device.id = bytes.Repeat([]byte{0xAB}, 64)
		clientDataJSON, attestationObject := device.register(probeParty, probeChallenge)
		credential, err := probeParty.VerifyRegistration(probeChallenge, clientDataJSON, attestationObject)
		if err != nil {
			t.Fatalf("algorithm %d: %v", algorithm, err)
		}
		if !bytes.Equal(credential.ID, device.id) {
			t.Errorf("algorithm %d: ID %x, want %x", algorithm, credential.ID, device.id)
		}
		if credential.Algorithm != algorithm {
			t.Errorf("algorithm %d: Algorithm %d", algorithm, credential.Algorithm)
		}
		if credential.SignCount != 7 {
			t.Errorf("algorithm %d: SignCount %d, want 7", algorithm, credential.SignCount)
		}
		parsed, err := x509.ParsePKIXPublicKey(credential.PublicKey)
		if err != nil {
			t.Fatalf("algorithm %d: PublicKey is not PKIX DER: %v", algorithm, err)
		}
		type equaler interface{ Equal(crypto.PublicKey) bool }
		if !parsed.(equaler).Equal(device.signer.Public()) {
			t.Errorf("algorithm %d: PublicKey is not the authenticator's key", algorithm)
		}
	}
}

// registrationCase is one registration response and whether the board must accept it.
type registrationCase struct {
	// name says what the response gets wrong, or right.
	name string

	// accept is whether VerifyRegistration must succeed.
	accept bool

	// build returns the challenge the board issued and the response to it.
	build func(t *testing.T, device *passkey) (challenge, clientDataJSON, attestationObject []byte)
}

// goodRegistration returns a response that only differs from device.register by the changes applied.
func goodRegistration(device *passkey, changeFields func(fields map[string]any), changeAuth func(flags *byte, extensions *[]byte)) (clientDataJSON, attestationObject []byte) {
	fields := clientFields("webauthn.create", probeChallenge, probeParty.Origin)
	if changeFields != nil {
		changeFields(fields)
	}
	flags := flagUP | flagUV | flagAT
	var extensions []byte
	if changeAuth != nil {
		changeAuth(&flags, &extensions)
	}
	attested := device.attested()
	if flags&flagAT == 0 {
		attested = nil
	}
	return clientJSON(fields), attestation("none", authData(probeParty.ID, flags, device.signCount, attested, extensions))
}

// clientDataCase builds a registration case that only changes the client data members.
func clientDataCase(name string, accept bool, change func(fields map[string]any)) registrationCase {
	return registrationCase{name: name, accept: accept, build: func(_ *testing.T, device *passkey) ([]byte, []byte, []byte) {
		clientDataJSON, attestationObject := goodRegistration(device, change, nil)
		return probeChallenge, clientDataJSON, attestationObject
	}}
}

// authDataCase builds a registration case that only changes the flags or extensions.
func authDataCase(name string, accept bool, change func(flags *byte, extensions *[]byte)) registrationCase {
	return registrationCase{name: name, accept: accept, build: func(_ *testing.T, device *passkey) ([]byte, []byte, []byte) {
		clientDataJSON, attestationObject := goodRegistration(device, nil, change)
		return probeChallenge, clientDataJSON, attestationObject
	}}
}

// coseKeyCase builds a registration case with a different COSE key.
func coseKeyCase(name string, accept bool, key func(device *passkey) map[any]any) registrationCase {
	return registrationCase{name: name, accept: accept, build: func(_ *testing.T, device *passkey) ([]byte, []byte, []byte) {
		device.coseKey = key(device)
		clientDataJSON, attestationObject := device.register(probeParty, probeChallenge)
		return probeChallenge, clientDataJSON, attestationObject
	}}
}

// rsaModulus returns an odd modulus of bits with its top bit set.
func rsaModulus(bits int) []byte {
	value := make([]byte, bits/8)
	rand.Read(value)
	value[0] |= 0x80
	value[len(value)-1] |= 1
	return value
}

// offCurvePoint returns P-256 coordinates where y is one more than the curve point's.
func offCurvePoint(device *passkey) ([]byte, []byte) {
	key := device.signer.(*ecdsa.PrivateKey)
	y := new(big.Int).Add(key.Y, big.NewInt(1))
	return padded(key.X, 32), padded(y, 32)
}

// registrationCases lists what a registration must and must not get past.
func registrationCases() []registrationCase {
	originCase := func(origin string) registrationCase {
		return clientDataCase("origin "+origin, false, func(fields map[string]any) { fields["origin"] = origin })
	}
	return []registrationCase{
		clientDataCase("a sign-in type", false, func(fields map[string]any) { fields["type"] = "webauthn.get" }),
		clientDataCase("an empty type", false, func(fields map[string]any) { fields["type"] = "" }),
		clientDataCase("a numeric type", false, func(fields map[string]any) { fields["type"] = 1 }),
		clientDataCase("no type", false, func(fields map[string]any) { delete(fields, "type") }),
		clientDataCase("another challenge", false, func(fields map[string]any) {
			fields["challenge"] = base64.RawURLEncoding.EncodeToString([]byte("another challenge of 32 bytes!!!"))
		}),
		clientDataCase("a padded base64url challenge", false, func(fields map[string]any) {
			fields["challenge"] = base64.URLEncoding.EncodeToString(probeChallenge)
		}),
		clientDataCase("a standard base64 challenge", false, func(fields map[string]any) {
			fields["challenge"] = base64.RawStdEncoding.EncodeToString(probeChallenge)
		}),
		clientDataCase("a challenge with one more byte", false, func(fields map[string]any) {
			fields["challenge"] = base64.RawURLEncoding.EncodeToString(append(append([]byte(nil), probeChallenge...), 0))
		}),
		clientDataCase("a challenge missing a byte", false, func(fields map[string]any) {
			fields["challenge"] = base64.RawURLEncoding.EncodeToString(probeChallenge[:len(probeChallenge)-1])
		}),
		clientDataCase("an empty challenge", false, func(fields map[string]any) { fields["challenge"] = "" }),
		clientDataCase("no challenge", false, func(fields map[string]any) { delete(fields, "challenge") }),
		clientDataCase("a challenge that is not a string", false, func(fields map[string]any) { fields["challenge"] = 5 }),
		originCase("http://board.example.com"),
		originCase("https://board.example.com:443"),
		originCase("https://board.example.com:8443"),
		originCase("https://board.example.com/"),
		originCase("https://BOARD.example.com"),
		originCase("https://sub.board.example.com"),
		originCase("https://board.example.com.evil.example"),
		originCase("https://board.example.co"),
		originCase(" https://board.example.com"),
		originCase(""),
		clientDataCase("no origin", false, func(fields map[string]any) { delete(fields, "origin") }),
		clientDataCase("a cross-origin frame", false, func(fields map[string]any) { fields["crossOrigin"] = true }),
		clientDataCase("crossOrigin as a string", false, func(fields map[string]any) { fields["crossOrigin"] = "false" }),
		clientDataCase("no crossOrigin member", true, func(fields map[string]any) { delete(fields, "crossOrigin") }),
		clientDataCase("crossOrigin null", true, func(fields map[string]any) { fields["crossOrigin"] = nil }),
		clientDataCase("unknown extra members", true, func(fields map[string]any) {
			fields["tokenBinding"] = map[string]any{"status": "supported"}
			fields["other_keys_can_be_added_here"] = "do not compare clientDataJSON against a template. See https://goo.gl/yabPex"
		}),
		clientDataCase("a 1 MB client data", true, func(fields map[string]any) {
			fields["padding"] = strings.Repeat("x", 1<<20)
		}),
		authDataCase("user presence not shown", false, func(flags *byte, _ *[]byte) { *flags &^= flagUP }),
		authDataCase("user not verified", false, func(flags *byte, _ *[]byte) { *flags &^= flagUV }),
		authDataCase("no attested credential", false, func(flags *byte, _ *[]byte) { *flags &^= flagAT }),
		authDataCase("extensions present and flagged", true, func(flags *byte, extensions *[]byte) {
			*flags |= flagED
			*extensions = cbor(map[any]any{"credProtect": 2})
		}),
		// PROBE: §6.1 says extension data follows when ED is set; the board accepts the flag with nothing after it.
		authDataCase("extensions flagged but absent", true, func(flags *byte, _ *[]byte) { *flags |= flagED }),
		// PROBE: §6.1 leaves no room for bytes after the attested credential data when ED is clear; the board ignores them.
		authDataCase("trailing bytes without the ED flag", true, func(_ *byte, extensions *[]byte) { *extensions = []byte{0, 0, 0} }),
		authDataCase("reserved flag bits set", true, func(flags *byte, _ *[]byte) { *flags |= 0x02 | 0x08 | 0x10 | 0x20 }),
		{name: "an empty credential identifier", accept: false, build: func(_ *testing.T, device *passkey) ([]byte, []byte, []byte) {
			device.id = nil
			clientDataJSON, attestationObject := device.register(probeParty, probeChallenge)
			return probeChallenge, clientDataJSON, attestationObject
		}},
		{name: "a 1023-byte credential identifier", accept: true, build: func(_ *testing.T, device *passkey) ([]byte, []byte, []byte) {
			device.id = bytes.Repeat([]byte{7}, maximumCredentialID)
			clientDataJSON, attestationObject := device.register(probeParty, probeChallenge)
			return probeChallenge, clientDataJSON, attestationObject
		}},
		{name: "a 1024-byte credential identifier", accept: false, build: func(_ *testing.T, device *passkey) ([]byte, []byte, []byte) {
			device.id = bytes.Repeat([]byte{7}, maximumCredentialID+1)
			clientDataJSON, attestationObject := device.register(probeParty, probeChallenge)
			return probeChallenge, clientDataJSON, attestationObject
		}},
		{name: "a credential identifier shorter than its length field", accept: false, build: func(_ *testing.T, device *passkey) ([]byte, []byte, []byte) {
			attested := device.attested()
			binary.BigEndian.PutUint16(attested[16:], uint16(len(device.id)+1))
			data := authData(probeParty.ID, flagUP|flagUV|flagAT, 0, attested, nil)
			return probeChallenge, clientData("webauthn.create", probeChallenge, probeParty.Origin), attestation("none", data)
		}},
		{name: "a credential identifier longer than its length field", accept: false, build: func(_ *testing.T, device *passkey) ([]byte, []byte, []byte) {
			attested := device.attested()
			binary.BigEndian.PutUint16(attested[16:], uint16(len(device.id)-1))
			data := authData(probeParty.ID, flagUP|flagUV|flagAT, 0, attested, nil)
			return probeChallenge, clientData("webauthn.create", probeChallenge, probeParty.Origin), attestation("none", data)
		}},
		{name: "another host's rpIdHash", accept: false, build: func(_ *testing.T, device *passkey) ([]byte, []byte, []byte) {
			data := authData("other.example.com", flagUP|flagUV|flagAT, 0, device.attested(), nil)
			return probeChallenge, clientData("webauthn.create", probeChallenge, probeParty.Origin), attestation("none", data)
		}},
		{name: "the origin's rpIdHash rather than the host's", accept: false, build: func(_ *testing.T, device *passkey) ([]byte, []byte, []byte) {
			data := authData(probeParty.Origin, flagUP|flagUV|flagAT, 0, device.attested(), nil)
			return probeChallenge, clientData("webauthn.create", probeChallenge, probeParty.Origin), attestation("none", data)
		}},
		{name: "a packed attestation format", accept: true, build: func(_ *testing.T, device *passkey) ([]byte, []byte, []byte) {
			data := authData(probeParty.ID, flagUP|flagUV|flagAT, 0, device.attested(), nil)
			return probeChallenge, clientData("webauthn.create", probeChallenge, probeParty.Origin), attestation("packed", data)
		}},
		coseKeyCase("PS256", false, func(device *passkey) map[any]any {
			device.coseKey[3] = -37
			return device.coseKey
		}),
		coseKeyCase("ES384 with a P-256 key", false, func(device *passkey) map[any]any {
			device.coseKey[3] = -35
			return device.coseKey
		}),
		coseKeyCase("ES256 over P-384", false, func(device *passkey) map[any]any {
			device.coseKey[-1] = 2
			return device.coseKey
		}),
		coseKeyCase("EC2 point off the curve", false, func(device *passkey) map[any]any {
			x, y := offCurvePoint(device)
			return map[any]any{1: 2, 3: webauthn.AlgorithmES256, -1: 1, -2: x, -3: y}
		}),
		coseKeyCase("EC2 x of 31 bytes", false, func(device *passkey) map[any]any {
			device.coseKey[-2] = device.coseKey[-2].([]byte)[1:]
			return device.coseKey
		}),
		coseKeyCase("EC2 y of 33 bytes", false, func(device *passkey) map[any]any {
			device.coseKey[-3] = append([]byte{0}, device.coseKey[-3].([]byte)...)
			return device.coseKey
		}),
		coseKeyCase("EC2 missing y", false, func(device *passkey) map[any]any {
			delete(device.coseKey, -3)
			return device.coseKey
		}),
		coseKeyCase("EC2 with y as a boolean", false, func(device *passkey) map[any]any {
			device.coseKey[-3] = true
			return device.coseKey
		}),
		coseKeyCase("EC2 kty with the EdDSA algorithm", false, func(device *passkey) map[any]any {
			device.coseKey[3] = webauthn.AlgorithmEdDSA
			return device.coseKey
		}),
		coseKeyCase("no algorithm label", false, func(device *passkey) map[any]any {
			delete(device.coseKey, 3)
			return device.coseKey
		}),
		coseKeyCase("algorithm as a text string", false, func(device *passkey) map[any]any {
			device.coseKey[3] = "ES256"
			return device.coseKey
		}),
		coseKeyCase("no kty label", false, func(device *passkey) map[any]any {
			delete(device.coseKey, 1)
			return device.coseKey
		}),
		coseKeyCase("OKP over X25519", false, func(_ *passkey) map[any]any {
			public, _, _ := ed25519.GenerateKey(rand.Reader)
			return map[any]any{1: 1, 3: webauthn.AlgorithmEdDSA, -1: 4, -2: []byte(public)}
		}),
		coseKeyCase("OKP Ed25519 key of 31 bytes", false, func(_ *passkey) map[any]any {
			public, _, _ := ed25519.GenerateKey(rand.Reader)
			return map[any]any{1: 1, 3: webauthn.AlgorithmEdDSA, -1: 6, -2: []byte(public)[:31]}
		}),
		coseKeyCase("OKP kty with the ES256 algorithm", false, func(_ *passkey) map[any]any {
			public, _, _ := ed25519.GenerateKey(rand.Reader)
			return map[any]any{1: 1, 3: webauthn.AlgorithmES256, -1: 6, -2: []byte(public)}
		}),
		coseKeyCase("RSA of 1024 bits", false, func(_ *passkey) map[any]any {
			return map[any]any{1: 3, 3: webauthn.AlgorithmRS256, -1: rsaModulus(1024), -2: []byte{1, 0, 1}}
		}),
		coseKeyCase("RSA of 2040 bits", false, func(_ *passkey) map[any]any {
			return map[any]any{1: 3, 3: webauthn.AlgorithmRS256, -1: rsaModulus(2040), -2: []byte{1, 0, 1}}
		}),
		coseKeyCase("RSA exponent 1", false, func(_ *passkey) map[any]any {
			return map[any]any{1: 3, 3: webauthn.AlgorithmRS256, -1: rsaModulus(2048), -2: []byte{1}}
		}),
		coseKeyCase("RSA exponent 65538", false, func(_ *passkey) map[any]any {
			return map[any]any{1: 3, 3: webauthn.AlgorithmRS256, -1: rsaModulus(2048), -2: []byte{1, 0, 2}}
		}),
		// PROBE: an even modulus is not an RSA modulus, but no signature can verify under it either.
		coseKeyCase("RSA even modulus", true, func(_ *passkey) map[any]any {
			modulus := rsaModulus(2048)
			modulus[len(modulus)-1] &^= 1
			return map[any]any{1: 3, 3: webauthn.AlgorithmRS256, -1: modulus, -2: []byte{1, 0, 1}}
		}),
		coseKeyCase("RSA 2048 with exponent 65537", true, func(_ *passkey) map[any]any {
			return map[any]any{1: 3, 3: webauthn.AlgorithmRS256, -1: rsaModulus(2048), -2: []byte{1, 0, 1}}
		}),
		coseKeyCase("RSA 4096 with exponent 65537", true, func(_ *passkey) map[any]any {
			return map[any]any{1: 3, 3: webauthn.AlgorithmRS256, -1: rsaModulus(4096), -2: []byte{1, 0, 1}}
		}),
		coseKeyCase("RSA kty with the ES256 algorithm", false, func(_ *passkey) map[any]any {
			return map[any]any{1: 3, 3: webauthn.AlgorithmES256, -1: rsaModulus(2048), -2: []byte{1, 0, 1}}
		}),
		coseKeyCase("an empty COSE key", false, func(_ *passkey) map[any]any { return map[any]any{} }),
	}
}

// TestProbeRegistrationChecks runs every registration case against an ES256 passkey.
func TestProbeRegistrationChecks(t *testing.T) {
	for _, test := range registrationCases() {
		t.Run(test.name, func(t *testing.T) {
			device := newPasskey(t, webauthn.AlgorithmES256)
			challenge, clientDataJSON, attestationObject := test.build(t, device)
			_, err := probeParty.VerifyRegistration(challenge, clientDataJSON, attestationObject)
			if test.accept && err != nil {
				t.Errorf("refused: %v", err)
			}
			if !test.accept && err == nil {
				t.Error("accepted")
			}
		})
	}
}

// TestProbeRegistrationRefusesEveryTruncation refuses every proper prefix of a valid attestation object.
func TestProbeRegistrationRefusesEveryTruncation(t *testing.T) {
	for _, algorithm := range webauthn.SupportedAlgorithms {
		device := newPasskey(t, algorithm)
		clientDataJSON, attestationObject := device.register(probeParty, probeChallenge)
		for length := 0; length < len(attestationObject); length++ {
			if _, err := probeParty.VerifyRegistration(probeChallenge, clientDataJSON, attestationObject[:length]); err == nil {
				t.Errorf("algorithm %d: accepted the first %d of %d bytes", algorithm, length, len(attestationObject))
			}
		}
		for length := 0; length < len(clientDataJSON); length++ {
			if _, err := probeParty.VerifyRegistration(probeChallenge, clientDataJSON[:length], attestationObject); err == nil {
				t.Errorf("algorithm %d: accepted the first %d bytes of client data", algorithm, length)
			}
		}
	}
}

// malformedCase is one hostile attestation object.
type malformedCase struct {
	// name says what is wrong with it.
	name string

	// object returns the attestation object given valid authenticator data.
	object func(data []byte) []byte
}

// malformedCases lists attestation objects that are not what §6.5 describes, some reusing device's key.
func malformedCases(device *passkey) []malformedCase {
	return []malformedCase{
		{"empty", func(_ []byte) []byte { return nil }},
		{"a byte string", func(data []byte) []byte { return cbor(data) }},
		{"an integer", func(_ []byte) []byte { return cbor(7) }},
		{"an array", func(data []byte) []byte { return cbor([]any{"none", map[any]any{}, data}) }},
		{"a map missing authData", func(_ []byte) []byte { return cbor(map[any]any{"fmt": "none", "attStmt": map[any]any{}}) }},
		{"authData as a text string", func(data []byte) []byte {
			return cbor(map[any]any{"fmt": "none", "attStmt": map[any]any{}, "authData": string(data)})
		}},
		{"authData as an array of bytes", func(data []byte) []byte {
			return cbor(map[any]any{"fmt": "none", "attStmt": map[any]any{}, "authData": []any{data}})
		}},
		{"integer keys", func(data []byte) []byte { return cbor(map[any]any{1: "none", 2: map[any]any{}, 3: data}) }},
		{"indefinite-length authData", func(data []byte) []byte {
			chunks := append(append([]byte{0x5f}, cbor(data[:10])...), cbor(data[10:])...)
			return cbor(cborPairs{{"fmt", "none"}, {"attStmt", map[any]any{}}, {"authData", rawCBOR(append(chunks, 0xff))}})
		}},
		{"indefinite-length map", func(data []byte) []byte {
			body := append(append(append([]byte{0xbf}, cbor("fmt")...), cbor("none")...), cbor("attStmt")...)
			body = append(append(append(body, cbor(map[any]any{})...), cbor("authData")...), cbor(data)...)
			return append(body, 0xff)
		}},
		{"duplicate authData keys, bad first", func(data []byte) []byte {
			return cbor(cborPairs{{"fmt", "none"}, {"attStmt", map[any]any{}}, {"authData", []byte{1}}, {"authData", data}})
		}},
		{"duplicate authData keys, good first", func(data []byte) []byte {
			return cbor(cborPairs{{"fmt", "none"}, {"attStmt", map[any]any{}}, {"authData", data}, {"authData", []byte{1}}})
		}},
		{"a float in attStmt", func(data []byte) []byte {
			return cbor(cborPairs{{"fmt", "none"}, {"attStmt", cborPairs{{"x", rawCBOR{0xf9, 0x3c, 0x00}}}}, {"authData", data}})
		}},
		{"a tagged authData", func(data []byte) []byte {
			return cbor(cborPairs{{"fmt", "none"}, {"attStmt", map[any]any{}}, {"authData", rawCBOR(append([]byte{0xd8, 0x18}, cbor(data)...))}})
		}},
		{"authData length claims 4 GB", func(_ []byte) []byte {
			return cbor(cborPairs{{"fmt", "none"}, {"attStmt", map[any]any{}}, {"authData", rawCBOR{0x5a, 0xff, 0xff, 0xff, 0xff}}})
		}},
		{"authData length claims 2^64-1", func(_ []byte) []byte {
			return cbor(cborPairs{{"fmt", "none"}, {"attStmt", map[any]any{}}, {"authData", rawCBOR{0x5b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}})
		}},
		{"a map claiming 2^64-1 pairs", func(_ []byte) []byte {
			return []byte{0xbb, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
		}},
		{"an array claiming 2^32-1 items in attStmt", func(data []byte) []byte {
			return cbor(cborPairs{{"fmt", "none"}, {"attStmt", rawCBOR{0x9a, 0xff, 0xff, 0xff, 0xff}}, {"authData", data}})
		}},
		{"a text string claiming 2^32-1 bytes as fmt", func(data []byte) []byte {
			return cbor(cborPairs{{"fmt", rawCBOR{0x7a, 0xff, 0xff, 0xff, 0xff}}, {"attStmt", map[any]any{}}, {"authData", data}})
		}},
		{"a reserved additional information value", func(data []byte) []byte {
			return cbor(cborPairs{{"fmt", rawCBOR{0x7c}}, {"attStmt", map[any]any{}}, {"authData", data}})
		}},
		{"a simple value in attStmt", func(data []byte) []byte {
			return cbor(cborPairs{{"fmt", "none"}, {"attStmt", cborPairs{{"x", rawCBOR{0xf8, 0xff}}}}, {"authData", data}})
		}},
		{"trailing bytes after the map", func(data []byte) []byte { return append(attestation("none", data), 0) }},
		{"a COSE key with duplicate labels", func(_ []byte) []byte {
			_, y := offCurvePoint(device)
			key := cborPairs{{1, 2}, {3, webauthn.AlgorithmES256}, {-1, 1}, {-2, device.coseKey[-2]}, {-3, y}, {-3, device.coseKey[-3]}}
			attested := append(make([]byte, 16), 0, byte(len(device.id)))
			attested = append(append(attested, device.id...), cbor(key)...)
			return attestation("none", authData(probeParty.ID, flagUP|flagUV|flagAT, 0, attested, nil))
		}},
	}
}

// TestProbeRegistrationRefusesMalformedObjects refuses each hostile attestation object quickly and without
// allocating what its length prefixes claim.
func TestProbeRegistrationRefusesMalformedObjects(t *testing.T) {
	device := newPasskey(t, webauthn.AlgorithmES256)
	clientDataJSON := clientData("webauthn.create", probeChallenge, probeParty.Origin)
	data := authData(probeParty.ID, flagUP|flagUV|flagAT, 0, device.attested(), nil)
	for _, test := range malformedCases(device) {
		t.Run(test.name, func(t *testing.T) {
			object := test.object(data)
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			started := time.Now()
			_, err := probeParty.VerifyRegistration(probeChallenge, clientDataJSON, object)
			elapsed := time.Since(started)
			runtime.ReadMemStats(&after)
			if err == nil {
				t.Error("accepted")
			}
			if elapsed > time.Second {
				t.Errorf("took %v", elapsed)
			}
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<20 {
				t.Errorf("allocated %d MB", allocated>>20)
			}
		})
	}
}

// TestProbeRegistrationIgnoresAttestationStatement records that fmt and attStmt are not examined at all.
func TestProbeRegistrationIgnoresAttestationStatement(t *testing.T) {
	device := newPasskey(t, webauthn.AlgorithmES256)
	clientDataJSON := clientData("webauthn.create", probeChallenge, probeParty.Origin)
	data := authData(probeParty.ID, flagUP|flagUV|flagAT, 0, device.attested(), nil)
	// PROBE: §7.1 steps 19-20 read fmt as a string naming a format; the package says "without attestation" and
	// accepts a missing or non-string fmt and a missing attStmt. Recorded, not failed.
	for name, object := range map[string][]byte{
		"fmt as an integer": cbor(map[any]any{"fmt": 0, "attStmt": map[any]any{}, "authData": data}),
		"no fmt":            cbor(map[any]any{"attStmt": map[any]any{}, "authData": data}),
		"no attStmt":        cbor(map[any]any{"fmt": "none", "authData": data}),
		"trailing bytes after the COSE key": attestation("none",
			authData(probeParty.ID, flagUP|flagUV|flagAT, 0, append(device.attested(), 0xff), nil)),
	} {
		if _, err := probeParty.VerifyRegistration(probeChallenge, clientDataJSON, object); err != nil {
			t.Errorf("%s: now refused: %v", name, err)
		}
	}
}

// TestProbeRegistrationDeepNesting feeds deeply nested arrays as attStmt and expects a quick refusal or acceptance.
func TestProbeRegistrationDeepNesting(t *testing.T) {
	device := newPasskey(t, webauthn.AlgorithmES256)
	clientDataJSON := clientData("webauthn.create", probeChallenge, probeParty.Origin)
	data := authData(probeParty.ID, flagUP|flagUV|flagAT, 0, device.attested(), nil)
	for _, depth := range []int{16, 17, 64, 1000, 1 << 20} {
		nested := append(bytes.Repeat([]byte{0x81}, depth), 0x00)
		object := cbor(cborPairs{{"fmt", "none"}, {"attStmt", rawCBOR(nested)}, {"authData", data}})
		started := time.Now()
		_, err := probeParty.VerifyRegistration(probeChallenge, clientDataJSON, object)
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Errorf("depth %d took %v", depth, elapsed)
		}
		t.Logf("PROBE: attStmt nested %d deep: %v", depth, err)
	}
}

// TestProbeRegistrationClientDataNotJSON refuses client data that is not a JSON object.
func TestProbeRegistrationClientDataNotJSON(t *testing.T) {
	device := newPasskey(t, webauthn.AlgorithmES256)
	_, attestationObject := device.register(probeParty, probeChallenge)
	good := string(clientData("webauthn.create", probeChallenge, probeParty.Origin))
	for _, bad := range []string{"", "null", "[]", `"text"`, "{", good + "x", good + good, strings.ToUpper(good), "\xff" + good} {
		if _, err := probeParty.VerifyRegistration(probeChallenge, []byte(bad), attestationObject); err == nil {
			t.Errorf("accepted client data %q", bad)
		}
	}
}

// TestProbeRegistrationDuplicateJSONMembers records how duplicate client data members are read.
func TestProbeRegistrationDuplicateJSONMembers(t *testing.T) {
	device := newPasskey(t, webauthn.AlgorithmES256)
	_, attestationObject := device.register(probeParty, probeChallenge)
	encodedChallenge := base64.RawURLEncoding.EncodeToString(probeChallenge)
	// PROBE: JSON with two "type" members is ambiguous; a strict reader refuses it, Go's reads the last.
	// Either way the create ceremony must not pass with the first member right and the last wrong.
	doubled := fmt.Sprintf(`{"type":"webauthn.create","challenge":%q,"origin":%q,"crossOrigin":false,"type":"webauthn.get"}`,
		encodedChallenge, probeParty.Origin)
	if _, err := probeParty.VerifyRegistration(probeChallenge, []byte(doubled), attestationObject); err == nil {
		t.Error("accepted client data whose last type member is webauthn.get")
	}
	doubledOrigin := fmt.Sprintf(`{"type":"webauthn.create","challenge":%q,"origin":%q,"crossOrigin":false,"origin":"https://evil.example"}`,
		encodedChallenge, probeParty.Origin)
	if _, err := probeParty.VerifyRegistration(probeChallenge, []byte(doubledOrigin), attestationObject); err == nil {
		t.Error("accepted client data whose last origin member is another site")
	}
}

// TestProbeRegistrationEmptyBoardChallenge records whether a registration with an empty challenge passes.
func TestProbeRegistrationEmptyBoardChallenge(t *testing.T) {
	device := newPasskey(t, webauthn.AlgorithmES256)
	clientDataJSON, attestationObject := device.register(probeParty, nil)
	_, err := probeParty.VerifyRegistration(nil, clientDataJSON, attestationObject)
	// PROBE: the package leaves challenge length to the caller; admin always issues 32 bytes.
	t.Logf("PROBE: registration with an empty challenge on both sides: %v", err)
}

// TestProbeAssertionHappyPath signs in several times in a row with every algorithm.
func TestProbeAssertionHappyPath(t *testing.T) {
	for _, algorithm := range webauthn.SupportedAlgorithms {
		device, credential := registered(t, algorithm)
		for round := 1; round <= 5; round++ {
			clientDataJSON, data, signature := device.assert(t, probeParty, probeChallenge, flagUP|flagUV)
			count, err := probeParty.VerifyAssertion(probeChallenge, credential, clientDataJSON, data, signature)
			if err != nil {
				t.Fatalf("algorithm %d round %d: %v", algorithm, round, err)
			}
			if count != uint32(round) {
				t.Fatalf("algorithm %d round %d: count %d", algorithm, round, count)
			}
			credential.SignCount = count
		}
	}
}

// assertionCase is one sign-in response and whether the board must accept it.
type assertionCase struct {
	// name says what the response gets wrong, or right.
	name string

	// accept is whether VerifyAssertion must succeed.
	accept bool

	// build returns the challenge the board issued, the credential it holds, and the response.
	build func(t *testing.T, device *passkey, credential webauthn.Credential) (challenge []byte, held webauthn.Credential, clientDataJSON, data, signature []byte)
}

// signedCase builds an assertion case whose client data and authenticator data are made by change and
// honestly signed.
func signedCase(name string, accept bool, change func(fields map[string]any, flags *byte, rpID *string, extra *[]byte)) assertionCase {
	return assertionCase{name: name, accept: accept, build: func(t *testing.T, device *passkey, credential webauthn.Credential) ([]byte, webauthn.Credential, []byte, []byte, []byte) {
		fields := clientFields("webauthn.get", probeChallenge, probeParty.Origin)
		flags := flagUP | flagUV
		rpID := probeParty.ID
		var extra []byte
		change(fields, &flags, &rpID, &extra)
		device.signCount++
		clientDataJSON := clientJSON(fields)
		data := authData(rpID, flags, device.signCount, nil, extra)
		return probeChallenge, credential, clientDataJSON, data, device.sign(t, data, clientDataJSON)
	}}
}

// assertionCases lists what a sign-in must and must not get past.
func assertionCases() []assertionCase {
	return []assertionCase{
		signedCase("a registration type", false, func(fields map[string]any, _ *byte, _ *string, _ *[]byte) { fields["type"] = "webauthn.create" }),
		signedCase("another challenge", false, func(fields map[string]any, _ *byte, _ *string, _ *[]byte) {
			fields["challenge"] = base64.RawURLEncoding.EncodeToString([]byte("another challenge of 32 bytes!!!"))
		}),
		signedCase("a padded challenge", false, func(fields map[string]any, _ *byte, _ *string, _ *[]byte) {
			fields["challenge"] = base64.URLEncoding.EncodeToString(probeChallenge)
		}),
		signedCase("another origin", false, func(fields map[string]any, _ *byte, _ *string, _ *[]byte) { fields["origin"] = "https://evil.example" }),
		signedCase("the origin over http", false, func(fields map[string]any, _ *byte, _ *string, _ *[]byte) {
			fields["origin"] = "http://board.example.com"
		}),
		signedCase("the origin with a trailing slash", false, func(fields map[string]any, _ *byte, _ *string, _ *[]byte) {
			fields["origin"] = "https://board.example.com/"
		}),
		signedCase("a cross-origin frame", false, func(fields map[string]any, _ *byte, _ *string, _ *[]byte) { fields["crossOrigin"] = true }),
		signedCase("no crossOrigin member", true, func(fields map[string]any, _ *byte, _ *string, _ *[]byte) { delete(fields, "crossOrigin") }),
		signedCase("extra client data members", true, func(fields map[string]any, _ *byte, _ *string, _ *[]byte) { fields["extra"] = []int{1, 2} }),
		signedCase("another host's rpIdHash", false, func(_ map[string]any, _ *byte, rpID *string, _ *[]byte) { *rpID = "other.example.com" }),
		signedCase("user presence not shown", false, func(_ map[string]any, flags *byte, _ *string, _ *[]byte) { *flags = flagUV }),
		signedCase("user not verified", false, func(_ map[string]any, flags *byte, _ *string, _ *[]byte) { *flags = flagUP }),
		signedCase("no flags at all", false, func(_ map[string]any, flags *byte, _ *string, _ *[]byte) { *flags = 0 }),
		signedCase("extensions present and flagged", true, func(_ map[string]any, flags *byte, _ *string, extra *[]byte) {
			*flags |= flagED
			*extra = cbor(map[any]any{"uvm": []any{[]any{2, 4, 2}}})
		}),
		// PROBE: the signature covers the trailing bytes, so only a §6.1 layout check would refuse them; the board does not.
		signedCase("trailing bytes without the ED flag", true, func(_ map[string]any, _ *byte, _ *string, extra *[]byte) { *extra = []byte{0} }),
		signedCase("reserved flag bits set", true, func(_ map[string]any, flags *byte, _ *string, _ *[]byte) { *flags |= 0x02 | 0x08 }),
		{name: "a signature by another key", accept: false, build: func(t *testing.T, device *passkey, credential webauthn.Credential) ([]byte, webauthn.Credential, []byte, []byte, []byte) {
			other := strangerPasskey(t, device.algorithm)
			other.signCount = device.signCount
			clientDataJSON, data, signature := other.assert(t, probeParty, probeChallenge, flagUP|flagUV)
			return probeChallenge, credential, clientDataJSON, data, signature
		}},
		{name: "a signature over other authenticator data", accept: false, build: func(t *testing.T, device *passkey, credential webauthn.Credential) ([]byte, webauthn.Credential, []byte, []byte, []byte) {
			clientDataJSON, data, _ := device.assert(t, probeParty, probeChallenge, flagUP|flagUV)
			_, otherData, signature := device.assert(t, probeParty, probeChallenge, flagUP|flagUV)
			_ = otherData
			return probeChallenge, credential, clientDataJSON, data, signature
		}},
		{name: "a signature over other client data", accept: false, build: func(t *testing.T, device *passkey, credential webauthn.Credential) ([]byte, webauthn.Credential, []byte, []byte, []byte) {
			_, data, signature := device.assert(t, probeParty, probeChallenge, flagUP|flagUV)
			fields := clientFields("webauthn.get", probeChallenge, probeParty.Origin)
			fields["note"] = "re-encoded"
			return probeChallenge, credential, clientJSON(fields), data, signature
		}},
		{name: "a DER signature with a trailing byte", accept: false, build: func(t *testing.T, device *passkey, credential webauthn.Credential) ([]byte, webauthn.Credential, []byte, []byte, []byte) {
			clientDataJSON, data, signature := device.assert(t, probeParty, probeChallenge, flagUP|flagUV)
			return probeChallenge, credential, clientDataJSON, data, append(signature, 0)
		}},
		{name: "a signature missing its last byte", accept: false, build: func(t *testing.T, device *passkey, credential webauthn.Credential) ([]byte, webauthn.Credential, []byte, []byte, []byte) {
			clientDataJSON, data, signature := device.assert(t, probeParty, probeChallenge, flagUP|flagUV)
			return probeChallenge, credential, clientDataJSON, data, signature[:len(signature)-1]
		}},
		{name: "an empty signature", accept: false, build: func(t *testing.T, device *passkey, credential webauthn.Credential) ([]byte, webauthn.Credential, []byte, []byte, []byte) {
			clientDataJSON, data, _ := device.assert(t, probeParty, probeChallenge, flagUP|flagUV)
			return probeChallenge, credential, clientDataJSON, data, nil
		}},
		{name: "a raw r||s ES256 signature", accept: false, build: func(t *testing.T, device *passkey, credential webauthn.Credential) ([]byte, webauthn.Credential, []byte, []byte, []byte) {
			if device.algorithm != webauthn.AlgorithmES256 {
				t.Skip("only ES256 signatures are DER")
			}
			device.signCount++
			clientDataJSON := clientData("webauthn.get", probeChallenge, probeParty.Origin)
			data := authData(probeParty.ID, flagUP|flagUV, device.signCount, nil, nil)
			digest := sha256.Sum256(clientDataJSON)
			hashed := sha256.Sum256(append(append([]byte(nil), data...), digest[:]...))
			r, s, err := ecdsa.Sign(rand.Reader, device.signer.(*ecdsa.PrivateKey), hashed[:])
			if err != nil {
				t.Fatal(err)
			}
			return probeChallenge, credential, clientDataJSON, data, append(padded(r, 32), padded(s, 32)...)
		}},
		{name: "a credential whose key does not match its algorithm", accept: false, build: func(t *testing.T, device *passkey, credential webauthn.Credential) ([]byte, webauthn.Credential, []byte, []byte, []byte) {
			clientDataJSON, data, signature := device.assert(t, probeParty, probeChallenge, flagUP|flagUV)
			_, other := registered(t, webauthn.AlgorithmRS256)
			if device.algorithm == webauthn.AlgorithmRS256 {
				_, other = registered(t, webauthn.AlgorithmES256)
			}
			credential.PublicKey = other.PublicKey
			return probeChallenge, credential, clientDataJSON, data, signature
		}},
		{name: "a credential with an unknown algorithm", accept: false, build: func(t *testing.T, device *passkey, credential webauthn.Credential) ([]byte, webauthn.Credential, []byte, []byte, []byte) {
			clientDataJSON, data, signature := device.assert(t, probeParty, probeChallenge, flagUP|flagUV)
			credential.Algorithm = 0
			return probeChallenge, credential, clientDataJSON, data, signature
		}},
		{name: "a credential with garbage for a key", accept: false, build: func(t *testing.T, device *passkey, credential webauthn.Credential) ([]byte, webauthn.Credential, []byte, []byte, []byte) {
			clientDataJSON, data, signature := device.assert(t, probeParty, probeChallenge, flagUP|flagUV)
			credential.PublicKey = []byte{0x30, 0x03, 0x02, 0x01}
			return probeChallenge, credential, clientDataJSON, data, signature
		}},
		{name: "a credential with no key", accept: false, build: func(t *testing.T, device *passkey, credential webauthn.Credential) ([]byte, webauthn.Credential, []byte, []byte, []byte) {
			clientDataJSON, data, signature := device.assert(t, probeParty, probeChallenge, flagUP|flagUV)
			credential.PublicKey = nil
			return probeChallenge, credential, clientDataJSON, data, signature
		}},
		{name: "empty authenticator data", accept: false, build: func(t *testing.T, device *passkey, credential webauthn.Credential) ([]byte, webauthn.Credential, []byte, []byte, []byte) {
			clientDataJSON, _, signature := device.assert(t, probeParty, probeChallenge, flagUP|flagUV)
			return probeChallenge, credential, clientDataJSON, nil, signature
		}},
		{name: "36 bytes of authenticator data", accept: false, build: func(t *testing.T, device *passkey, credential webauthn.Credential) ([]byte, webauthn.Credential, []byte, []byte, []byte) {
			device.signCount++
			clientDataJSON := clientData("webauthn.get", probeChallenge, probeParty.Origin)
			data := authData(probeParty.ID, flagUP|flagUV, device.signCount, nil, nil)[:36]
			return probeChallenge, credential, clientDataJSON, data, device.sign(t, data, clientDataJSON)
		}},
		{name: "a stolen registration's attested data in the assertion", accept: true, build: func(t *testing.T, device *passkey, credential webauthn.Credential) ([]byte, webauthn.Credential, []byte, []byte, []byte) {
			device.signCount++
			clientDataJSON := clientData("webauthn.get", probeChallenge, probeParty.Origin)
			data := authData(probeParty.ID, flagUP|flagUV|flagAT, device.signCount, device.attested(), nil)
			return probeChallenge, credential, clientDataJSON, data, device.sign(t, data, clientDataJSON)
		}},
	}
}

// TestProbeAssertionChecks runs every assertion case against every algorithm.
func TestProbeAssertionChecks(t *testing.T) {
	for _, algorithm := range webauthn.SupportedAlgorithms {
		for _, test := range assertionCases() {
			t.Run(fmt.Sprintf("%d/%s", algorithm, test.name), func(t *testing.T) {
				device, credential := registered(t, algorithm)
				challenge, held, clientDataJSON, data, signature := test.build(t, device, credential)
				count, err := probeParty.VerifyAssertion(challenge, held, clientDataJSON, data, signature)
				if test.accept && err != nil {
					t.Errorf("refused: %v", err)
				}
				if test.accept && err == nil && count != device.signCount {
					t.Errorf("returned count %d, authenticator sent %d", count, device.signCount)
				}
				if !test.accept && err == nil {
					t.Error("accepted")
				}
			})
		}
	}
}

// TestProbeAssertionSignCounts checks the count rule through VerifyAssertion itself.
func TestProbeAssertionSignCounts(t *testing.T) {
	for _, test := range []struct {
		// name reads as the case.
		name string

		// stored is the credential's count.
		stored uint32

		// received is the count the authenticator sends.
		received uint32

		// accept is whether the sign-in passes.
		accept bool
	}{
		{"stored 5 received 5", 5, 5, false},
		{"stored 5 received 4", 5, 4, false},
		{"stored 5 received 6", 5, 6, true},
		{"stored 5 received 0", 5, 0, false},
		{"stored 5 received max", 5, math.MaxUint32, true},
		{"stored 0 received 0", 0, 0, true},
		{"stored 0 received 1", 0, 1, true},
		{"stored max-1 received max", math.MaxUint32 - 1, math.MaxUint32, true},
		{"stored max received max", math.MaxUint32, math.MaxUint32, false},
		{"stored max received 0", math.MaxUint32, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			device, credential := registered(t, webauthn.AlgorithmES256)
			credential.SignCount = test.stored
			device.signCount = test.received - 1
			clientDataJSON, data, signature := device.assert(t, probeParty, probeChallenge, flagUP|flagUV)
			count, err := probeParty.VerifyAssertion(probeChallenge, credential, clientDataJSON, data, signature)
			if test.accept && (err != nil || count != test.received) {
				t.Errorf("count %d, %v; want %d, nil", count, err, test.received)
			}
			if !test.accept && err == nil {
				t.Errorf("accepted with count %d", count)
			}
		})
	}
}

// TestProbeSignCountAdvancesTable checks SignCountAdvances at its edges.
func TestProbeSignCountAdvancesTable(t *testing.T) {
	for _, test := range []struct {
		// name reads as the case.
		name string

		// stored and received are the two counts.
		stored, received uint32

		// want is the expected answer.
		want bool
	}{
		{"both zero", 0, 0, true},
		{"zero to one", 0, 1, true},
		{"one to one", 1, 1, false},
		{"two to one", 2, 1, false},
		{"one to zero", 1, 0, false},
		{"one to two", 1, 2, true},
		{"one to max", 1, math.MaxUint32, true},
		{"max-1 to max", math.MaxUint32 - 1, math.MaxUint32, true},
		{"max to max", math.MaxUint32, math.MaxUint32, false},
		{"max to zero", math.MaxUint32, 0, false},
	} {
		if got := webauthn.SignCountAdvances(test.stored, test.received); got != test.want {
			t.Errorf("%s: SignCountAdvances(%d, %d) = %v, want %v", test.name, test.stored, test.received, got, test.want)
		}
	}
}

// TestProbeAssertionCountProperty checks SignCountAdvances agrees with VerifyAssertion over many pairs.
func TestProbeAssertionCountProperty(t *testing.T) {
	device, credential := registered(t, webauthn.AlgorithmEdDSA)
	for stored := uint32(0); stored < 4; stored++ {
		for received := uint32(0); received < 4; received++ {
			credential.SignCount = stored
			device.signCount = received - 1
			clientDataJSON, data, signature := device.assert(t, probeParty, probeChallenge, flagUP|flagUV)
			_, err := probeParty.VerifyAssertion(probeChallenge, credential, clientDataJSON, data, signature)
			if accepted := err == nil; accepted != webauthn.SignCountAdvances(stored, received) {
				t.Errorf("stored %d received %d: VerifyAssertion %v, SignCountAdvances %v", stored, received, err, !accepted)
			}
		}
	}
}

// TestProbeRelyingPartyWithPort registers and signs in on an origin with a port, as a Raspberry Pi serves.
func TestProbeRelyingPartyWithPort(t *testing.T) {
	party := webauthn.RelyingParty{ID: "localhost", Origin: "http://localhost:8080"}
	device := newPasskey(t, webauthn.AlgorithmES256)
	clientDataJSON, attestationObject := device.register(party, probeChallenge)
	credential, err := party.VerifyRegistration(probeChallenge, clientDataJSON, attestationObject)
	if err != nil {
		t.Fatal(err)
	}
	clientDataJSON, data, signature := device.assert(t, party, probeChallenge, flagUP|flagUV)
	if _, err := party.VerifyAssertion(probeChallenge, credential, clientDataJSON, data, signature); err != nil {
		t.Fatal(err)
	}
	wrongPort := webauthn.RelyingParty{ID: "localhost", Origin: "http://localhost:8081"}
	if _, err := wrongPort.VerifyAssertion(probeChallenge, credential, clientDataJSON, data, signature); err == nil {
		t.Error("another port's origin accepted")
	}
}

// FuzzProbeAttestationObject verifies arbitrary attestation objects against valid client data without panicking.
func FuzzProbeAttestationObject(f *testing.F) {
	clientDataJSON := clientData("webauthn.create", probeChallenge, probeParty.Origin)
	for _, algorithm := range webauthn.SupportedAlgorithms {
		_, attestationObject := newPasskey(f, algorithm).register(probeParty, probeChallenge)
		f.Add(attestationObject)
	}
	device := newPasskey(f, webauthn.AlgorithmES256)
	data := authData(probeParty.ID, flagUP|flagUV|flagAT, 0, device.attested(), nil)
	for _, test := range malformedCases(device) {
		f.Add(test.object(data))
	}
	f.Fuzz(func(t *testing.T, attestationObject []byte) {
		credential, err := probeParty.VerifyRegistration(probeChallenge, clientDataJSON, attestationObject)
		if err != nil {
			return
		}
		if len(credential.ID) == 0 || len(credential.ID) > maximumCredentialID {
			t.Fatalf("accepted a credential identifier of %d bytes", len(credential.ID))
		}
		if _, err := x509.ParsePKIXPublicKey(credential.PublicKey); err != nil {
			t.Fatalf("stored a key that does not parse: %v", err)
		}
	})
}

// FuzzProbeAuthenticatorData verifies arbitrary authenticator data in a sign-in without panicking.
func FuzzProbeAuthenticatorData(f *testing.F) {
	device, credential := registered(f, webauthn.AlgorithmES256)
	credential.SignCount = 2
	device.signCount = 2
	clientDataJSON, data, signature := device.assert(f, probeParty, probeChallenge, flagUP|flagUV)
	f.Add(data)
	f.Add(data[:36])
	f.Add(authData(probeParty.ID, flagUP|flagUV|flagAT|flagED, 3, device.attested(), cbor(map[any]any{"a": 1})))
	f.Fuzz(func(t *testing.T, fuzzed []byte) {
		count, err := probeParty.VerifyAssertion(probeChallenge, credential, clientDataJSON, fuzzed, signature)
		if err == nil && !bytes.Equal(fuzzed, data) {
			t.Fatalf("accepted authenticator data %x with count %d", fuzzed, count)
		}
	})
}
