package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

// newKey returns a fresh P-256 private key.
func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// TestMessageIsExact pins the signed message, byte for byte, to the README.
func TestMessageIsExact(t *testing.T) {
	message := Message("put", "/api/v1/members/me/weekly-challenges/2026-10-19", 1792800000, "AAAAAAAAAAAAAAAAAAAAAA", nil)
	expected := "FitHalo-ECDSA-1\nPUT\n/api/v1/members/me/weekly-challenges/2026-10-19\n1792800000:AAAAAAAAAAAAAAAAAAAAAA\n" +
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if string(message) != expected {
		t.Fatalf("message was\n%s", message)
	}
}

// TestSignedHeaderVerifies signs a request as a client would and verifies it as the server does.
func TestSignedHeaderVerifies(t *testing.T) {
	key := newKey(t)
	now := time.Unix(1792800000, 0)
	body := []byte(`{"progress":0.5}`)
	value, err := AuthorizationValue(key, "PUT", "/api/v1/members/me/weekly-challenges/2026-10-19", now, body)
	if err != nil {
		t.Fatal(err)
	}
	header, err := ParseHeader(value)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, raw, err := ParsePublicKey(EncodePublicKey(&key.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if header.Member != MemberID(raw) || len(header.Member) != 22 {
		t.Fatalf("member %q", header.Member)
	}
	message := Message("PUT", "/api/v1/members/me/weekly-challenges/2026-10-19", header.Time, header.Nonce, body)
	if !Verify(publicKey, message, header.Signature) {
		t.Fatal("a genuine signature did not verify")
	}
	tampered := Message("PUT", "/api/v1/members/me/weekly-challenges/2026-10-19", header.Time, header.Nonce, []byte(`{"progress":5}`))
	if Verify(publicKey, tampered, header.Signature) {
		t.Fatal("a signature verified over a different body")
	}
	if Verify(&newKey(t).PublicKey, message, header.Signature) {
		t.Fatal("a signature verified under another key")
	}
}

// TestMalformedHeadersAreRefused covers every way a header can be wrong.
func TestMalformedHeadersAreRefused(t *testing.T) {
	member := "AAAAAAAAAAAAAAAAAAAAAA"
	signature := strings.Repeat("A", 86)
	for _, value := range []string{
		"",
		"Bearer abc",
		"FitHalo-ECDSA member=" + member + ",time=1,nonce=" + member,
		"FitHalo-ECDSA member=" + member + ",time=1,nonce=" + member + ",signature=" + signature + ",extra=1",
		"FitHalo-ECDSA member=" + member + ",member=" + member + ",time=1,signature=" + signature,
		"FitHalo-ECDSA member=short,time=1,nonce=" + member + ",signature=" + signature,
		"FitHalo-ECDSA member=" + member + ",time=soon,nonce=" + member + ",signature=" + signature,
		"FitHalo-ECDSA member=" + member + ",time=1,nonce=" + member + ",signature=AAAA",
	} {
		if _, err := ParseHeader(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
	spaced := "FitHalo-ECDSA member=" + member + ", time=1, nonce=" + member + ", signature=" + signature
	if _, err := ParseHeader(spaced); err != nil {
		t.Errorf("refused spaces after commas: %v", err)
	}
}

// TestPublicKeyMustBeOnTheCurve refuses a point that is not on P-256.
func TestPublicKeyMustBeOnTheCurve(t *testing.T) {
	encoded := EncodePublicKey(&newKey(t).PublicKey)
	raw, _ := encoding.DecodeString(encoded)
	raw[64] ^= 1
	if _, _, err := ParsePublicKey(encoding.EncodeToString(raw)); err == nil {
		t.Fatal("accepted a point off the curve")
	}
	if _, _, err := ParsePublicKey(encoded[:40]); err == nil {
		t.Fatal("accepted a short key")
	}
}

// TestSkewAndNonces checks the time window and that a nonce works once.
func TestSkewAndNonces(t *testing.T) {
	now := time.Unix(1792800000, 0)
	if !WithinSkew(now.Add(-299*time.Second).Unix(), now) || WithinSkew(now.Add(301*time.Second).Unix(), now) {
		t.Fatal("the skew window is not 300 seconds")
	}
	cache := NewNonceCache()
	if !cache.Use("member", "nonce", now) || cache.Use("member", "nonce", now.Add(time.Minute)) {
		t.Fatal("a nonce was accepted twice")
	}
	if !cache.Use("other", "nonce", now) {
		t.Fatal("one member's nonce blocked another's")
	}
	if !cache.Use("member", "nonce", now.Add(NonceLifetime+time.Second)) {
		t.Fatal("a nonce was remembered past its lifetime")
	}
}

// TestMalformedParametersAreNotEchoed keeps a malformed or repeated parameter out of the error text.
func TestMalformedParametersAreNotEchoed(t *testing.T) {
	member := "AAAAAAAAAAAAAAAAAAAAAA"
	for _, value := range []string{
		"FitHalo-ECDSA member=" + member + ",<script>echo</script>",
		"FitHalo-ECDSA <script>echo</script>=1,<script>echo</script>=2",
	} {
		_, err := ParseHeader(value)
		if err == nil || strings.Contains(err.Error(), "echo") {
			t.Errorf("ParseHeader(%q) = %v, want an error without the parameter", value, err)
		}
	}
}
