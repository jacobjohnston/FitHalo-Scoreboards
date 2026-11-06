package password

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLengthCountsWhatAPersonSees counts each emoji, flag and accented letter once, like Intl.Segmenter.
func TestLengthCountsWhatAPersonSees(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"password", 8},
		{"\U0001F600", 1}, // an emoji
		{"\U0001F468\u200D\U0001F469\u200D\U0001F467", 1},                             // a family joined by zero-width joiners
		{"\U0001F44D\U0001F3FD", 1},                                                   // a skin tone modifier
		{"\U0001F1E8\U0001F1E6\U0001F1FA\U0001F1F8", 2},                               // two flags
		{"\U0001F3F4\U000E0067\U000E0062\U000E0073\U000E0063\U000E0074\U000E007F", 1}, // a flag spelled with tag characters
		{"e\u0301te\u0301", 3},                                                        // combining accents
		{"\u2764\uFE0F", 1},                                                           // a variation selector
		{"\uD55C\uAD6D\uC5B4", 3},                                                     // three Hangul syllables
		{"\u1100\u1161\u11A8", 1},                                                     // a syllable from conjoining jamo
	}
	for _, test := range cases {
		if got := Length(test.text); got != test.want {
			t.Errorf("Length(%q) = %d, want %d", test.text, got, test.want)
		}
	}
}

// TestBreached finds a breached password by its suffix, ignores padding, and sends only the prefix.
func TestBreached(t *testing.T) {
	digest := sha1.Sum([]byte("password123"))
	hash := strings.ToUpper(hex.EncodeToString(digest[:]))
	var askedPath, padding string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		askedPath, padding = request.URL.Path, request.Header.Get("Add-Padding")
		fmt.Fprintf(writer, "0018A45C4D1DEF81644B54AB7F969B88D65:1\r\n%s:2254650\r\nFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF:0\r\n", hash[5:])
	}))
	defer server.Close()
	checker := BreachChecker{BaseURL: server.URL + "/range/"}

	breached, err := checker.Breached(context.Background(), "password123")
	if err != nil || !breached {
		t.Fatalf("a breached password read as %v, %v", breached, err)
	}
	if askedPath != "/range/"+hash[:5] || padding != "true" {
		t.Fatalf("asked %q with padding %q", askedPath, padding)
	}
	if breached, err := checker.Breached(context.Background(), "a password nobody has used"); err != nil || breached {
		t.Fatalf("an unknown password read as %v, %v", breached, err)
	}
}

// TestBreachedPaddingIsNotAMatch ignores a padding line even when its suffix matches.
func TestBreachedPaddingIsNotAMatch(t *testing.T) {
	digest := sha1.Sum([]byte("padded"))
	hash := strings.ToUpper(hex.EncodeToString(digest[:]))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(writer, "%s:0\n", hash[5:])
	}))
	defer server.Close()
	if breached, err := (BreachChecker{BaseURL: server.URL + "/"}).Breached(context.Background(), "padded"); err != nil || breached {
		t.Fatalf("a padding line read as %v, %v", breached, err)
	}
}

// TestBreachedReportsAFailedCheck returns an error, not an answer, when the service fails.
func TestBreachedReportsAFailedCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	if _, err := (BreachChecker{BaseURL: server.URL + "/"}).Breached(context.Background(), "anything at all"); err == nil {
		t.Fatal("a failed check gave no error")
	}
}
