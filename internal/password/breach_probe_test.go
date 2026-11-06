package password_test

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/password"
)

// sha1Hex returns the upper-case SHA-1 of a password.
func sha1Hex(text string) string {
	sum := sha1.Sum([]byte(text))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// recordedRequest is what the fake range API saw.
type recordedRequest struct {
	// mu guards the fields.
	mu sync.Mutex

	// path is the request path.
	path string

	// query is the raw query string.
	query string

	// body is the request body.
	body string

	// header is a copy of the headers.
	header http.Header

	// method is the HTTP method.
	method string
}

// rangeServer serves a fixed body for every range request and records the latest one.
func rangeServer(t *testing.T, status int, body string) (*httptest.Server, *recordedRequest) {
	t.Helper()
	record := &recordedRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received, _ := io.ReadAll(request.Body)
		record.mu.Lock()
		record.path = request.URL.Path
		record.query = request.URL.RawQuery
		record.body = string(received)
		record.header = request.Header.Clone()
		record.method = request.Method
		record.mu.Unlock()
		writer.WriteHeader(status)
		io.WriteString(writer, body)
	}))
	t.Cleanup(server.Close)
	return server, record
}

// TestOnlyThePrefixLeaves checks the request carries the first five hex characters and nothing else about the password.
func TestOnlyThePrefixLeaves(t *testing.T) {
	const secret = "hunter2-very-secret"
	hash := sha1Hex(secret)
	server, record := rangeServer(t, http.StatusOK, hash[5:]+":3\r\n")
	checker := password.BreachChecker{BaseURL: server.URL + "/range/"}
	breached, err := checker.Breached(context.Background(), secret)
	if err != nil || !breached {
		t.Fatalf("breached %v, %v", breached, err)
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.path != "/range/"+hash[:5] {
		t.Errorf("path %q, want /range/%s", record.path, hash[:5])
	}
	if record.query != "" || record.body != "" || record.method != http.MethodGet {
		t.Errorf("query %q body %q method %q; only a GET of the prefix may leave", record.query, record.body, record.method)
	}
	everything := record.path + record.query + record.body
	for name, values := range record.header {
		everything += name + strings.Join(values, "")
	}
	if strings.Contains(everything, secret) || strings.Contains(strings.ToUpper(everything), hash[5:11]) {
		t.Error("more than the five-character prefix left the server")
	}
	if padding := record.header.Get("Add-Padding"); padding != "true" {
		// PROBE: the doc says the answer is padded; Have I Been Pwned pads only when Add-Padding: true is sent.
		t.Errorf("Add-Padding header %q, want \"true\"", padding)
	}
}

// TestPrefixIsUpperCase checks a password whose hash starts with letters is requested in upper case.
func TestPrefixIsUpperCase(t *testing.T) {
	var secret string
	for candidate := 0; ; candidate++ {
		secret = "candidate" + strings.Repeat("x", candidate%7) + string(rune('a'+candidate%26))
		if prefix := sha1Hex(secret)[:5]; strings.ContainsAny(prefix, "ABCDEF") {
			break
		}
	}
	server, record := rangeServer(t, http.StatusOK, "00000:1\r\n")
	checker := password.BreachChecker{BaseURL: server.URL + "/range/"}
	if _, err := checker.Breached(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.path != "/range/"+sha1Hex(secret)[:5] {
		t.Errorf("path %q, want upper-case prefix /range/%s", record.path, sha1Hex(secret)[:5])
	}
}

// TestResponseParsing feeds the checker range bodies and checks the verdict.
func TestResponseParsing(t *testing.T) {
	const secret = "password123!"
	suffix := sha1Hex(secret)[5:]
	for _, test := range []struct {
		// name describes the body.
		name string

		// body is what the range API answers.
		body string

		// want is the verdict.
		want bool
	}{
		{name: "match first line", body: suffix + ":42\r\n0123456789ABCDEF0123456789ABCDEF012:1\r\n", want: true},
		{name: "match last line without newline", body: "0123456789ABCDEF0123456789ABCDEF012:1\r\n" + suffix + ":1", want: true},
		{name: "match with LF only", body: "0123456789ABCDEF0123456789ABCDEF012:1\n" + suffix + ":7\n", want: true},
		{name: "match with huge count", body: suffix + ":99999999999999999999\r\n", want: true},
		{name: "no match", body: "0123456789ABCDEF0123456789ABCDEF012:1\r\nFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF:2\r\n", want: false},
		{name: "empty body", body: "", want: false},
		{name: "padded zero count is not a breach", body: suffix + ":0\r\n0123456789ABCDEF0123456789ABCDEF012:1\r\n", want: false},
		{name: "lower case suffix", body: strings.ToLower(suffix) + ":5\r\n", want: true},
		{name: "suffix as a prefix of a longer token is no match", body: suffix + "AB:5\r\n", want: false},
		{name: "suffix embedded in another line is no match", body: "AB" + suffix[2:] + ":5\r\n", want: false},
		{name: "malformed lines ignored", body: "garbage\r\n:::\r\n" + suffix + "\r\n" + ":5\r\n" + suffix + ":2\r\n", want: true},
		{name: "count not a number", body: suffix + ":lots\r\n", want: true},
		{name: "whitespace around line", body: "  " + suffix + ":3  \r\n", want: true},
		{name: "full hash on a line is no match", body: sha1Hex(secret) + ":3\r\n", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, _ := rangeServer(t, http.StatusOK, test.body)
			checker := password.BreachChecker{BaseURL: server.URL + "/range/"}
			got, err := checker.Breached(context.Background(), secret)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				// PROBE: lower-case suffixes, whitespace and non-numeric counts are not what HIBP sends; a strict parser may differ.
				t.Errorf("breached %v, want %v", got, test.want)
			}
		})
	}
}

// TestServerErrors expects an error, never a verdict, for failing responses.
func TestServerErrors(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadRequest, http.StatusNotFound, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		server, _ := rangeServer(t, status, "00000:1\r\n")
		checker := password.BreachChecker{BaseURL: server.URL + "/range/"}
		if breached, err := checker.Breached(context.Background(), "secret"); err == nil {
			t.Errorf("status %d: breached %v with no error", status, breached)
		}
	}
}

// TestUnreachableServer expects an error when nothing listens.
func TestUnreachableServer(t *testing.T) {
	server, _ := rangeServer(t, http.StatusOK, "")
	server.Close()
	checker := password.BreachChecker{BaseURL: server.URL + "/range/"}
	if _, err := checker.Breached(context.Background(), "secret"); err == nil {
		t.Fatal("no error from a closed server")
	}
}

// TestClientTimeout expects the caller's client timeout to end a hanging request with an error.
func TestClientTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-release:
		case <-request.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	checker := password.BreachChecker{BaseURL: server.URL + "/range/", Client: &http.Client{Timeout: 100 * time.Millisecond}}
	started := time.Now()
	_, err := checker.Breached(context.Background(), "secret")
	if err == nil {
		t.Fatal("no error from a hanging server")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("took %v to time out", elapsed)
	}
}

// TestContextCancelled expects a cancelled context to end the request with an error.
func TestContextCancelled(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-release:
		case <-request.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	checker := password.BreachChecker{BaseURL: server.URL + "/range/"}
	if _, err := checker.Breached(ctx, "secret"); err == nil {
		t.Fatal("no error from a cancelled context")
	}
	already, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := checker.Breached(already, "secret"); err == nil {
		t.Fatal("no error from an already cancelled context")
	}
}

// TestHugeResponseBody feeds 10 MB of lines and checks the checker finishes and still finds the suffix at the end.
func TestHugeResponseBody(t *testing.T) {
	const secret = "a password with a big range"
	suffix := sha1Hex(secret)[5:]
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		line := []byte("0123456789ABCDEF0123456789ABCDEF012:1\r\n")
		for written := 0; written < 10<<20; written += len(line) {
			writer.Write(line)
		}
		io.WriteString(writer, suffix+":1\r\n")
	}))
	t.Cleanup(server.Close)
	checker := password.BreachChecker{BaseURL: server.URL + "/range/"}
	done := make(chan struct{})
	var breached bool
	var err error
	go func() {
		breached, err = checker.Breached(context.Background(), secret)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a 10 MB body did not finish in 30 s")
	}
	// PROBE: a real range answer is under 50 KB; the checker may cap the body and report an error or a miss. Record which.
	t.Logf("10 MB body: breached %v, %v", breached, err)
	if err == nil && !breached {
		t.Log("the suffix at the end of a 10 MB body was not seen; the body is capped and read as a miss")
	}
}

// TestBaseURLWithoutTrailingSlashRecordsResult records how a BaseURL without its trailing slash is joined.
func TestBaseURLWithoutTrailingSlashRecordsResult(t *testing.T) {
	server, record := rangeServer(t, http.StatusOK, "")
	checker := password.BreachChecker{BaseURL: server.URL + "/range"}
	if _, err := checker.Breached(context.Background(), "secret"); err != nil {
		t.Logf("BaseURL without trailing slash: %v", err)
		return
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	// PROBE: BreachRangeURL ends in a slash; a caller's BaseURL without one may produce /rangeXXXXX.
	t.Logf("BaseURL without trailing slash requested %q", record.path)
}

// TestDefaultsPointAtHaveIBeenPwned checks the documented constant.
func TestDefaultsPointAtHaveIBeenPwned(t *testing.T) {
	if password.BreachRangeURL != "https://api.pwnedpasswords.com/range/" {
		t.Fatalf("BreachRangeURL %q", password.BreachRangeURL)
	}
}

// TestConcurrentChecks runs several checks at once against one checker under the race detector.
func TestConcurrentChecks(t *testing.T) {
	server, _ := rangeServer(t, http.StatusOK, "00000:1\r\n")
	checker := password.BreachChecker{BaseURL: server.URL + "/range/"}
	var group sync.WaitGroup
	for index := range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := checker.Breached(context.Background(), "secret"+string(rune('a'+index))); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
}
