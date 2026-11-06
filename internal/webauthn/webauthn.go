// Package webauthn registers passkeys and verifies sign-ins with them under WebAuthn Level 2, without
// attestation, since the board only needs to know the same passkey signs in each time.
package webauthn

import (
	"bytes"
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
)

// The COSE algorithms a passkey may use.
const (
	// AlgorithmES256 is ECDSA over P-256 with SHA-256.
	AlgorithmES256 = -7

	// AlgorithmEdDSA is EdDSA, accepted only over Ed25519.
	AlgorithmEdDSA = -8

	// AlgorithmRS256 is RSASSA-PKCS1-v1_5 with SHA-256.
	AlgorithmRS256 = -257
)

// SupportedAlgorithms is what the board asks for, in order of preference.
var SupportedAlgorithms = []int{AlgorithmES256, AlgorithmEdDSA, AlgorithmRS256}

const (
	// flagUserPresent is the authenticator data flag set when the user touched the authenticator.
	flagUserPresent = 0x01

	// flagUserVerified is the flag set when the authenticator verified the user, by PIN or biometric.
	flagUserVerified = 0x04

	// flagAttestedData is the flag set when attested credential data follows the sign count.
	flagAttestedData = 0x40

	// authenticatorDataBase is the fixed prefix: RP ID hash (32 bytes), flags (1) and sign count (4).
	authenticatorDataBase = 37

	// aaguidLength is the bytes of authenticator model identifier that open attested credential data.
	aaguidLength = 16

	// maximumCredentialIDLength is the longest credential identifier WebAuthn allows.
	maximumCredentialIDLength = 1023

	// minimumRSABits is the smallest RSA modulus accepted.
	minimumRSABits = 2048

	// minimumRSAExponent is the smallest RSA exponent crypto/rsa verifies with; it must also be odd.
	minimumRSAExponent = 3

	// maximumRSAExponent is the largest RSA exponent crypto/rsa verifies with.
	maximumRSAExponent = 1<<31 - 1
)

// RelyingParty is the board as WebAuthn sees it.
type RelyingParty struct {
	// ID is the board's hostname, whose SHA-256 a passkey's authenticator data must carry.
	ID string

	// Origin is the board's origin, which the client data must name exactly.
	Origin string
}

// Credential is a registered passkey's public part.
type Credential struct {
	// ID is the credential identifier the authenticator chose.
	ID []byte

	// Algorithm is the passkey's COSE algorithm.
	Algorithm int

	// PublicKey is the passkey's public key as PKIX DER.
	PublicKey []byte

	// SignCount is the last signature count seen, or zero when the passkey keeps none.
	SignCount uint32
}

// clientData is the browser's collected client data.
type clientData struct {
	// Type is the ceremony, webauthn.create or webauthn.get.
	Type string `json:"type"`

	// Challenge is the challenge as the browser saw it, in unpadded base64url.
	Challenge string `json:"challenge"`

	// Origin is the origin of the page that ran the ceremony.
	Origin string `json:"origin"`

	// CrossOrigin is true when the ceremony ran in a cross-origin frame.
	CrossOrigin bool `json:"crossOrigin"`
}

// VerifyRegistration checks a new passkey's response to challenge and returns the credential to store.
// The public key is kept as PKIX DER, whatever its algorithm.
func (rp RelyingParty) VerifyRegistration(challenge, clientDataJSON, attestationObject []byte) (Credential, error) {
	if err := rp.checkClientData(clientDataJSON, "webauthn.create", challenge); err != nil {
		return Credential{}, err
	}
	decoded, rest, err := decodeCBOR(attestationObject)
	if err != nil || len(rest) != 0 {
		return Credential{}, errors.New("webauthn: the attestation object is not CBOR")
	}
	object, isMap := decoded.(map[any]any)
	if !isMap {
		return Credential{}, errors.New("webauthn: the attestation object is not a map")
	}
	authenticatorData, isBytes := object["authData"].([]byte)
	if !isBytes {
		return Credential{}, errors.New("webauthn: the attestation object has no authenticator data")
	}
	flags, signCount, err := rp.checkAuthenticatorData(authenticatorData)
	if err != nil {
		return Credential{}, err
	}
	if flags&flagAttestedData == 0 || len(authenticatorData) < authenticatorDataBase+aaguidLength+2 {
		return Credential{}, errors.New("webauthn: the response carries no credential")
	}
	attested := authenticatorData[authenticatorDataBase+aaguidLength:]
	idLength := int(binary.BigEndian.Uint16(attested))
	if idLength == 0 || idLength > maximumCredentialIDLength || len(attested) < 2+idLength {
		return Credential{}, errors.New("webauthn: the credential identifier is malformed")
	}
	id := append([]byte(nil), attested[2:2+idLength]...)
	keyValue, _, err := decodeCBOR(attested[2+idLength:])
	if err != nil {
		return Credential{}, errors.New("webauthn: the credential's public key is not CBOR")
	}
	key, isKeyMap := keyValue.(map[any]any)
	if !isKeyMap {
		return Credential{}, errors.New("webauthn: the credential's public key is not a COSE key")
	}
	algorithm, publicKey, err := parseCOSEKey(key)
	if err != nil {
		return Credential{}, err
	}
	encoded, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return Credential{}, err
	}
	return Credential{ID: id, Algorithm: algorithm, PublicKey: encoded, SignCount: signCount}, nil
}

// VerifyAssertion checks a sign-in against a stored credential and returns the new signature count.
func (rp RelyingParty) VerifyAssertion(
	challenge []byte, credential Credential, clientDataJSON, authenticatorData, signature []byte,
) (uint32, error) {
	if err := rp.checkClientData(clientDataJSON, "webauthn.get", challenge); err != nil {
		return 0, err
	}
	_, signCount, err := rp.checkAuthenticatorData(authenticatorData)
	if err != nil {
		return 0, err
	}
	clientDigest := sha256.Sum256(clientDataJSON)
	signed := append(append([]byte(nil), authenticatorData...), clientDigest[:]...)
	publicKey, err := x509.ParsePKIXPublicKey(credential.PublicKey)
	if err != nil {
		return 0, err
	}
	if !verifySignature(credential.Algorithm, publicKey, signed, signature) {
		return 0, errors.New("webauthn: the signature does not verify")
	}
	if !SignCountAdvances(credential.SignCount, signCount) {
		return 0, errors.New("webauthn: the signature count went backwards, so the passkey may have been copied")
	}
	return signCount, nil
}

// SignCountAdvances reports whether received may follow stored: higher, or both zero for a passkey that
// keeps no count.
func SignCountAdvances(stored, received uint32) bool {
	return (stored == 0 && received == 0) || received > stored
}

// checkClientData checks the client data's type, challenge and origin.
func (rp RelyingParty) checkClientData(clientDataJSON []byte, expectedType string, challenge []byte) error {
	var data clientData
	if err := json.Unmarshal(clientDataJSON, &data); err != nil {
		return errors.New("webauthn: the client data is not JSON")
	}
	if data.Type != expectedType {
		return fmt.Errorf("webauthn: the client data is for %q", data.Type)
	}
	received, err := base64.RawURLEncoding.DecodeString(data.Challenge)
	if err != nil || subtle.ConstantTimeCompare(received, challenge) != 1 {
		return errors.New("webauthn: the challenge does not match")
	}
	if data.Origin != rp.Origin || data.CrossOrigin {
		return fmt.Errorf("webauthn: the response came from %q", data.Origin)
	}
	return nil
}

// checkAuthenticatorData checks the RP ID hash and the user flags, and returns the flags and sign count.
func (rp RelyingParty) checkAuthenticatorData(data []byte) (byte, uint32, error) {
	if len(data) < authenticatorDataBase {
		return 0, 0, errors.New("webauthn: the authenticator data is too short")
	}
	expected := sha256.Sum256([]byte(rp.ID))
	if !bytes.Equal(data[:32], expected[:]) {
		return 0, 0, errors.New("webauthn: the passkey belongs to another site")
	}
	flags := data[32]
	if flags&flagUserPresent == 0 || flags&flagUserVerified == 0 {
		return 0, 0, errors.New("webauthn: the user was not verified")
	}
	return flags, binary.BigEndian.Uint32(data[33:37]), nil
}

// parseCOSEKey decodes a COSE key in one of SupportedAlgorithms and returns its algorithm and key.
func parseCOSEKey(key map[any]any) (int, crypto.PublicKey, error) {
	// COSE labels (RFC 9053): 1 key type, 3 algorithm, -1 curve or n, -2 x or e, -3 y.
	keyType, _ := key[int64(1)].(int64)
	algorithm, _ := key[int64(3)].(int64)
	switch {
	case keyType == 2 && algorithm == AlgorithmES256:
		curve, _ := key[int64(-1)].(int64)
		x, xIsBytes := key[int64(-2)].([]byte)
		y, yIsBytes := key[int64(-3)].([]byte)
		if curve != 1 || !xIsBytes || !yIsBytes || len(x) != 32 || len(y) != 32 {
			return 0, nil, errors.New("webauthn: an ES256 key that is not P-256")
		}
		point := append(append([]byte{4}, x...), y...)
		if _, err := ecdh.P256().NewPublicKey(point); err != nil {
			return 0, nil, errors.New("webauthn: an ES256 key off the curve")
		}
		publicKey := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		return AlgorithmES256, publicKey, nil
	case keyType == 1 && algorithm == AlgorithmEdDSA:
		curve, _ := key[int64(-1)].(int64)
		x, isBytes := key[int64(-2)].([]byte)
		if curve != 6 || !isBytes || len(x) != ed25519.PublicKeySize {
			return 0, nil, errors.New("webauthn: an EdDSA key that is not Ed25519")
		}
		return AlgorithmEdDSA, ed25519.PublicKey(x), nil
	case keyType == 3 && algorithm == AlgorithmRS256:
		modulus, modulusIsBytes := key[int64(-1)].([]byte)
		exponent, exponentIsBytes := key[int64(-2)].([]byte)
		if !modulusIsBytes || !exponentIsBytes || len(exponent) == 0 || len(exponent) > 4 {
			return 0, nil, errors.New("webauthn: a malformed RSA key")
		}
		publicKey := &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(new(big.Int).SetBytes(exponent).Int64())}
		if publicKey.N.BitLen() < minimumRSABits {
			return 0, nil, errors.New("webauthn: an RSA key under 2048 bits")
		}
		if publicKey.E < minimumRSAExponent || publicKey.E > maximumRSAExponent || publicKey.E%2 == 0 {
			return 0, nil, errors.New("webauthn: an RSA exponent the verifier refuses")
		}
		return AlgorithmRS256, publicKey, nil
	}
	return 0, nil, fmt.Errorf("webauthn: an unsupported key, type %d algorithm %d", keyType, algorithm)
}

// verifySignature reports whether signature is publicKey's signature of signed under algorithm.
func verifySignature(algorithm int, publicKey crypto.PublicKey, signed, signature []byte) bool {
	switch algorithm {
	case AlgorithmES256:
		key, isECDSA := publicKey.(*ecdsa.PublicKey)
		digest := sha256.Sum256(signed)
		return isECDSA && ecdsa.VerifyASN1(key, digest[:], signature)
	case AlgorithmEdDSA:
		key, isEd25519 := publicKey.(ed25519.PublicKey)
		return isEd25519 && ed25519.Verify(key, signed, signature)
	case AlgorithmRS256:
		key, isRSA := publicKey.(*rsa.PublicKey)
		digest := sha256.Sum256(signed)
		return isRSA && rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) == nil
	}
	return false
}
