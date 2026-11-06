package password

import (
	"errors"
	"strings"
	"testing"
)

// TestHashAndVerify checks a password verifies, a wrong one does not, and salts differ.
func TestHashAndVerify(t *testing.T) {
	first, err := Hash("correct horse", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "pbkdf2-sha256$600000$") || strings.Count(first, "$") != 3 {
		t.Fatalf("stored value %q", first)
	}
	if matches, err := Verify(first, "correct horse", nil); err != nil || !matches {
		t.Fatalf("the right password failed: %v", err)
	}
	if matches, _ := Verify(first, "correct horsf", nil); matches {
		t.Fatal("a wrong password verified")
	}
	second, _ := Hash("correct horse", nil)
	if first == second {
		t.Fatal("two hashes of one password were identical, so the salt is not random")
	}
	if _, err := Verify("md5$abc", "anything", nil); err == nil {
		t.Fatal("accepted an unknown algorithm")
	}
}

// TestPepper checks a peppered hash needs the same pepper, and says so when it changed.
func TestPepper(t *testing.T) {
	pepper := []byte("a pepper from the server's configuration")
	stored, err := Hash("correct horse", pepper)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(stored, "$"+PepperID(pepper)) || strings.Contains(stored, string(pepper)) {
		t.Fatalf("stored value %q", stored)
	}
	if matches, err := Verify(stored, "correct horse", pepper); err != nil || !matches {
		t.Fatalf("the right password and pepper failed: %v", err)
	}
	if _, err := Verify(stored, "correct horse", []byte("another pepper")); !errors.Is(err, ErrPepperChanged) {
		t.Fatalf("a changed pepper: %v", err)
	}
	if _, err := Verify(stored, "correct horse", nil); !errors.Is(err, ErrPepperChanged) {
		t.Fatalf("a missing pepper: %v", err)
	}
	unpeppered, _ := Hash("correct horse", nil)
	if matches, err := Verify(unpeppered, "correct horse", pepper); err != nil || !matches {
		t.Fatalf("an older hash without a pepper stopped verifying once one was set: %v", err)
	}
}
