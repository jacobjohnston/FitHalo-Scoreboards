package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/admin"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/auth"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/lambda"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/nonces"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// TestSignedRequestsSurviveLambda proves a signed join and read pass through the Lambda adapter intact,
// since any change to the path or body breaks the signature.
func TestSignedRequestsSurviveLambda(t *testing.T) {
	directory, err := store.OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	backend := store.Compress(directory)
	publicURL, _ := url.Parse("https://abc.lambda-url.us-east-1.on.aws")
	board, err := assemble(context.Background(), backend, publicURL, secrets{setupCode: "SetupCodeSetupCode"}, "",
		&nonces.Store{Store: backend}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		JSON scoreboard.Board `json:"json"`
	}
	document, _ := backend.Get(context.Background(), "board", "board")
	json.Unmarshal(document.Data, &stored.JSON)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	send := func(method, path string, body []byte) lambda.Response {
		authorization, err := auth.AuthorizationValue(key, method, path, time.Now(), body)
		if err != nil {
			t.Fatal(err)
		}
		event := lambda.Event{RawPath: path, Headers: map[string]string{"authorization": authorization, "content-type": "application/json"},
			Body: base64.StdEncoding.EncodeToString(body), IsBase64Encoded: true}
		event.RequestContext.DomainName = publicURL.Host
		event.RequestContext.HTTP.Method = method
		event.RequestContext.HTTP.SourceIP = "203.0.113.9"
		return lambda.Handle(context.Background(), board.handler, event)
	}

	join, _ := json.Marshal(map[string]any{
		"inviteCode": stored.JSON.InviteCode, "publicKey": auth.EncodePublicKey(&key.PublicKey), "displayName": "Through Lambda",
		"sharing": map[string]any{"score": "progress", "activity": "none", "routes": false, "strengthSets": false, "strengthWeights": false},
	})
	if response := send("POST", "/api/v1/members", join); response.StatusCode != http.StatusCreated {
		body, _ := base64.StdEncoding.DecodeString(response.Body)
		t.Fatalf("joining through Lambda: %d %s", response.StatusCode, body)
	}
	if response := send("GET", "/api/v1/members/me", nil); response.StatusCode != http.StatusOK {
		body, _ := base64.StdEncoding.DecodeString(response.Body)
		t.Fatalf("a signed read through Lambda: %d %s", response.StatusCode, body)
	}
}

// TestAssembledBoardCompresses proves static files are gzipped, API answers vary by encoding, and pages
// are never gzipped.
func TestAssembledBoardCompresses(t *testing.T) {
	directory, err := store.OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	backend := store.Compress(directory)
	publicURL, _ := url.Parse("https://board.example.com")
	board, err := assemble(context.Background(), backend, publicURL, secrets{setupCode: "SetupCodeSetupCode"}, "",
		&nonces.Store{Store: backend}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fetch := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "https://board.example.com"+path, nil)
		request.Header.Set("Accept-Encoding", "gzip")
		recorder := httptest.NewRecorder()
		board.handler.ServeHTTP(recorder, request)
		return recorder
	}

	if response := fetch("/static/style.css"); response.Code != http.StatusOK || response.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("got %d with Content-Encoding %q for the stylesheet, want 200 gzip", response.Code,
			response.Header().Get("Content-Encoding"))
	}
	if response := fetch("/api/v1/board"); response.Header().Get("Vary") != "Accept-Encoding" {
		t.Errorf("got Vary %q from the API, want Accept-Encoding", response.Header().Get("Vary"))
	}
	if response := fetch("/admin/setup"); response.Header().Get("Content-Encoding") != "" {
		t.Errorf("got Content-Encoding %q for a portal page, want none", response.Header().Get("Content-Encoding"))
	}
}

// TestServicesShareThePepper proves newServices gives the service and admin the configured pepper.
func TestServicesShareThePepper(t *testing.T) {
	keys := secrets{pepper: []byte("PepperPepperPepper")}
	service, administrator := newServices(nil, nil, keys, "", nil)
	if string(service.Pepper) != string(keys.pepper) || string(administrator.Pepper) != string(keys.pepper) {
		t.Fatalf("got peppers %q and %q, want %q", service.Pepper, administrator.Pepper, keys.pepper)
	}
}

// TestSetupBanner puts the setup code on a line of its own, boxed on a terminal and plain in a log.
func TestSetupBanner(t *testing.T) {
	const code = "4XFUaW8zo7sZv4eMxqf7jQ"
	var plain strings.Builder
	writeSetupBanner(&plain, "https://board.example.com/admin/setup", code, false, false)
	if !strings.Contains(plain.String(), "Setup code:   "+code+"\n") || strings.Contains(plain.String(), "│") {
		t.Fatalf("the log banner is not plain lines:\n%s", plain.String())
	}

	var boxed strings.Builder
	writeSetupBanner(&boxed, "https://board.example.com/admin/setup", code, true, false)
	lines := strings.Split(strings.Trim(boxed.String(), "\n"), "\n")
	width := utf8.RuneCountInString(lines[0])
	for _, line := range lines {
		if utf8.RuneCountInString(line) != width {
			t.Fatalf("the box is ragged:\n%s", boxed.String())
		}
	}
	if !strings.Contains(boxed.String(), "     "+code+" ") || strings.Contains(boxed.String(), "\x1b[") {
		t.Fatalf("the terminal banner lacks the code on its own line, or has colour it was not asked for:\n%s", boxed.String())
	}

	var coloured strings.Builder
	writeSetupBanner(&coloured, "https://board.example.com/admin/setup", code, true, true)
	if !strings.Contains(coloured.String(), styleCode+code+styleReset) {
		t.Fatal("the coloured banner does not highlight the code")
	}

	var configured strings.Builder
	writeSetupBanner(&configured, "https://board.example.com/admin/setup", "", true, false)
	lines = strings.Split(strings.Trim(configured.String(), "\n"), "\n")
	width = utf8.RuneCountInString(lines[0])
	for _, line := range lines {
		if utf8.RuneCountInString(line) != width {
			t.Fatalf("the configured-code box is ragged:\n%s", configured.String())
		}
	}
	if !strings.Contains(configured.String(), "/admin/setup") || !strings.Contains(configured.String(), setupCodeVariable) {
		t.Fatalf("the configured-code banner does not point to setup and the configured code:\n%s", configured.String())
	}
}

// readBanner runs announceSetup over a fresh board with setupCode and returns what it wrote.
func readBanner(t *testing.T, setupCode string) (string, *admin.Admin) {
	t.Helper()
	directory, err := store.OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { directory.Close() })
	output, err := os.Create(filepath.Join(t.TempDir(), "banner"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	administrator := &admin.Admin{Store: directory, Now: time.Now, SetupCode: setupCode}
	if err := announceSetup(context.Background(), administrator, "https://board.example.com/admin/setup", output); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(written), administrator
}

// TestConfiguredSetupCodeIsNotLogged keeps a SCOREBOARD_SETUP_CODE out of the log, naming it instead.
func TestConfiguredSetupCodeIsNotLogged(t *testing.T) {
	const code = "ConfiguredSetupCode1"
	written, administrator := readBanner(t, code)
	if strings.Contains(written, code) {
		t.Fatalf("the banner printed the configured setup code:\n%s", written)
	}
	if !strings.Contains(written, "/admin/setup") || !strings.Contains(written, setupCodeVariable) {
		t.Fatalf("the banner does not point to setup and the configured code:\n%s", written)
	}
	if administrator.SetupCode != code {
		t.Fatalf("got setup code %q, want the configured %q", administrator.SetupCode, code)
	}
}

// TestGeneratedSetupCodeIsLogged prints a setup code generated at start, the only place it can be read.
func TestGeneratedSetupCodeIsLogged(t *testing.T) {
	written, administrator := readBanner(t, "")
	if administrator.SetupCode == "" || !strings.Contains(written, "Setup code:   "+administrator.SetupCode+"\n") {
		t.Fatalf("the banner lacks the generated setup code %q:\n%s", administrator.SetupCode, written)
	}
}

// TestTrustedProxiesConfiguration reads SCOREBOARD_TRUSTED_PROXIES in proxy mode and refuses it elsewhere.
func TestTrustedProxiesConfiguration(t *testing.T) {
	t.Setenv("SCOREBOARD_TRUSTED_PROXIES", "172.16.0.0/12, ::1")
	settings, err := readConfiguration([]string{"-tls", "proxy", "-listen", "0.0.0.0:8080", "-public-url", "https://board.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.trustedProxies) != 2 || !settings.trustedProxies.Contains("172.17.0.1") {
		t.Fatalf("got trusted proxies %v, want 172.16.0.0/12 and ::1", settings.trustedProxies)
	}
	if _, err := readConfiguration([]string{"-tls", "development"}); err == nil {
		t.Fatal("trusted proxies were accepted in development mode")
	}
	t.Setenv("SCOREBOARD_TRUSTED_PROXIES", "proxy.example.com")
	if _, err := readConfiguration([]string{"-tls", "proxy", "-public-url", "https://board.example.com"}); err == nil {
		t.Fatal("a trusted proxy that is not an address was accepted")
	}
}

// TestTrustedProxiesDefault trusts only non-public peers in proxy mode on every interface, unless set to any.
func TestTrustedProxiesDefault(t *testing.T) {
	t.Setenv("SCOREBOARD_TRUSTED_PROXIES", "")
	settings, err := readConfiguration([]string{"-tls", "proxy", "-listen", "0.0.0.0:8080", "-public-url", "https://board.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !settings.trustedProxiesDefaulted || !settings.trustedProxies.Contains("172.17.0.1") || settings.trustedProxies.Contains("203.0.113.9") {
		t.Fatalf("got trusted proxies %v, want the non-public ranges", settings.trustedProxies)
	}
	if settings, err = readConfiguration([]string{"-tls", "proxy", "-listen", "127.0.0.1:8080", "-public-url", "https://board.example.com"}); err != nil || len(settings.trustedProxies) != 0 {
		t.Fatalf("loopback got trusted proxies %v, %v, want none", settings.trustedProxies, err)
	}
	t.Setenv("SCOREBOARD_TRUSTED_PROXIES", "any")
	if settings, err = readConfiguration([]string{"-tls", "proxy", "-listen", ":8080", "-public-url", "https://board.example.com"}); err != nil || len(settings.trustedProxies) != 0 || settings.trustedProxiesDefaulted {
		t.Fatalf("any got trusted proxies %v, %v, want none", settings.trustedProxies, err)
	}
	if _, err := readConfiguration([]string{"-tls", "development"}); err == nil {
		t.Fatal("any was accepted in development mode")
	}
}
