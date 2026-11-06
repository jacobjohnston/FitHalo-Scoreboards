package web_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/admin"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/ratelimit"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/web"
	"github.com/jacobjohnston/FitHalo-Scoreboards/spec"
)

// probeSetupCode is the setup code every probe board accepts.
const probeSetupCode = "SetupCodeSetupCode"

// boardFormLimit is the web package's limit on the Board page's details, 8 MiB.
const boardFormLimit = 8 << 20

// probeSite is a served board and its parts.
type probeSite struct {
	// t is the test the site belongs to.
	t *testing.T

	// server serves the site.
	server *httptest.Server

	// handler is the site's handler, for requests that bypass the client's URL cleaning.
	handler http.Handler

	// service is the board's service.
	service *scoreboard.Service

	// web is the site under test.
	web *web.Site

	// origin is the Origin header a legitimate browser sends.
	origin string

	// host is the Host header a legitimate browser sends.
	host string
}

// answer is one response read whole.
type answer struct {
	// status is the status code.
	status int

	// body is the whole body.
	body string

	// header is the response header.
	header http.Header
}

// newProbeSite serves a fresh board; publicURL overrides the public address when not empty.
func newProbeSite(t *testing.T, publicURL string, configure func(*web.Site)) *probeSite {
	t.Helper()
	directory, err := store.OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { directory.Close() })
	allowlist, err := sharing.LoadAllowlist(spec.Sharing)
	if err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) }
	service := &scoreboard.Service{Store: directory, Allowlist: allowlist, Now: now}
	board, _ := scoreboard.DefaultBoard()
	board.WeekStartDay = "monday"
	if _, _, err := service.EnsureBoard(context.Background(), board); err != nil {
		t.Fatal(err)
	}
	administrator := &admin.Admin{Store: directory, Now: now, SetupCode: probeSetupCode}
	result := &probeSite{t: t, service: service}
	result.server = httptest.NewUnstartedServer(nil)
	address := result.server.Listener.Addr().String()
	if publicURL == "" {
		publicURL = "http://" + address
	}
	parsed, err := url.Parse(publicURL)
	if err != nil {
		t.Fatal(err)
	}
	result.origin = parsed.Scheme + "://" + parsed.Host
	result.host = parsed.Host
	result.web = &web.Site{Service: service, Admin: administrator, PublicURL: parsed, GlobalSignInLimiter: ratelimit.New(1000, 1000)}
	if configure != nil {
		configure(result.web)
	}
	result.handler, err = result.web.Handler()
	if err != nil {
		t.Fatal(err)
	}
	result.server.Config.Handler = result.handler
	result.server.Start()
	t.Cleanup(result.server.Close)
	return result
}

// visitor is a browser with cookies at a probe site.
type visitor struct {
	// site is the board visited.
	site *probeSite

	// client keeps cookies and does not follow redirects.
	client *http.Client
}

// newVisitor returns a browser with an empty jar that does not follow redirects.
func (s *probeSite) newVisitor() *visitor {
	jar, _ := cookiejar.New(nil)
	return &visitor{site: s, client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// do sends method to path with body and header, adding the board's Origin and Host unless header sets them.
func (v *visitor) do(method, path string, body string, header http.Header) answer {
	v.site.t.Helper()
	request, err := http.NewRequest(method, v.site.server.URL+path, strings.NewReader(body))
	if err != nil {
		v.site.t.Fatal(err)
	}
	request.Host = v.site.host
	if method == "POST" {
		request.Header.Set("Origin", v.site.origin)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for key, values := range header {
		if len(values) == 1 && values[0] == "" {
			request.Header.Del(key)
			continue
		}
		request.Header[http.CanonicalHeaderKey(key)] = values
	}
	if hosts := header["Host"]; len(hosts) == 1 {
		request.Host = hosts[0]
	}
	response, err := v.client.Do(request)
	if err != nil {
		v.site.t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return answer{status: response.StatusCode, body: string(data), header: response.Header}
}

// get fetches path.
func (v *visitor) get(path string) answer {
	v.site.t.Helper()
	return v.do("GET", path, "", nil)
}

// post submits form to path.
func (v *visitor) post(path string, form url.Values) answer {
	v.site.t.Helper()
	return v.do("POST", path, form.Encode(), nil)
}

// csrfPattern finds the CSRF token on a page.
var csrfPattern = regexp.MustCompile(`<meta name="csrf" content="([^"]+)">`)

// csrf returns the CSRF token on the page at path.
func (v *visitor) csrf(path string) string {
	v.site.t.Helper()
	match := csrfPattern.FindStringSubmatch(v.get(path).body)
	if match == nil {
		v.site.t.Fatalf("%s has no CSRF token", path)
	}
	return match[1]
}

// setupForm is a complete, valid setup form.
func setupForm() url.Values {
	return url.Values{"code": {probeSetupCode}, "boardName": {"Morning Crew"}, "username": {"avery"}, "password": {"a long password"}, "confirm": {"a long password"}}
}

// signedIn sets the board up through the form and returns the admin's browser and the setup response.
func (s *probeSite) signedIn() (*visitor, answer) {
	s.t.Helper()
	admin := s.newVisitor()
	result := admin.post("/admin/setup", setupForm())
	landed := result
	if result.status == http.StatusSeeOther || result.status == http.StatusFound {
		landed = admin.get(result.header.Get("Location"))
	}
	if landed.status != http.StatusOK || !strings.Contains(landed.body, "Morning Crew") {
		s.t.Fatalf("setup ended at %d then %d: %.300s", result.status, landed.status, landed.body)
	}
	return admin, result
}

// securityHeaders are the headers every response must carry, with a substring each must contain.
var securityHeaders = map[string]string{
	"Content-Security-Policy":    "default-src 'none'",
	"X-Content-Type-Options":     "nosniff",
	"Referrer-Policy":            "",
	"Permissions-Policy":         "",
	"Cross-Origin-Opener-Policy": "same-origin",
	"X-Frame-Options":            "DENY",
}

// checkSecurityHeaders fails unless result carries every security header.
func checkSecurityHeaders(t *testing.T, label string, result answer) {
	t.Helper()
	for name, substring := range securityHeaders {
		value := result.header.Get(name)
		if value == "" || !strings.Contains(value, substring) {
			t.Errorf("%s: %s is %q", label, name, value)
		}
	}
	if policy := result.header.Get("Content-Security-Policy"); !strings.Contains(policy, "frame-ancestors 'none'") || !strings.Contains(policy, "base-uri 'none'") {
		t.Errorf("%s: Content-Security-Policy is %q", label, policy)
	}
}

// TestEveryResponseHasSecurityHeaders carries the headers on pages, static files, errors and redirects.
func TestEveryResponseHasSecurityHeaders(t *testing.T) {
	site := newProbeSite(t, "", nil)
	visitor := site.newVisitor()
	for _, path := range []string{"/", "/admin/setup", "/admin/sign-in", "/admin/board", "/nothing-here", "/static/style.css", "/static/missing.css", "/join/OldInviteOldInvite0000", "/static/fonts/Archivo.woff2"} {
		checkSecurityHeaders(t, "GET "+path, visitor.get(path))
		checkSecurityHeaders(t, "HEAD "+path, visitor.do("HEAD", path, "", nil))
	}
	checkSecurityHeaders(t, "POST /language", visitor.post("/language", url.Values{"language": {"de"}, "return": {"/"}}))
	checkSecurityHeaders(t, "POST without origin", visitor.do("POST", "/admin/setup", "", http.Header{"Origin": {""}}))
	checkSecurityHeaders(t, "wrong method", visitor.do("PUT", "/", "", nil))
	checkSecurityHeaders(t, "OPTIONS", visitor.do("OPTIONS", "/", "", nil))
}

// TestHSTSOnlyOverHTTPS sends Strict-Transport-Security only when the public URL is https.
func TestHSTSOnlyOverHTTPS(t *testing.T) {
	plain := newProbeSite(t, "", nil)
	if hsts := plain.newVisitor().get("/").header.Get("Strict-Transport-Security"); hsts != "" {
		t.Errorf("an http board sends HSTS %q, which would lock a development browser out", hsts)
	}
	secure := newProbeSite(t, "https://board.example", nil)
	hsts := secure.newVisitor().get("/").header.Get("Strict-Transport-Security")
	if !strings.Contains(hsts, "max-age=") {
		t.Fatalf("an https board sends HSTS %q", hsts)
	}
	seconds, _ := strconv.Atoi(regexp.MustCompile(`max-age=(\d+)`).FindStringSubmatch(hsts)[1])
	if seconds < 180*24*3600 {
		t.Errorf("HSTS max-age is %d seconds, under the 180 days preload lists and the review expect", seconds)
	}
	if strings.Contains(hsts, "includeSubDomains") {
		t.Errorf("HSTS %q includes subdomains, which plans/reference-security-review.md says it must not", hsts)
	}
	if hsts404 := secure.newVisitor().get("/nothing").header.Get("Strict-Transport-Security"); hsts404 == "" {
		t.Error("the 404 page lacks HSTS")
	}
}

// TestLandingPageHeaders serves HTML with charset and a cache policy.
func TestLandingPageHeaders(t *testing.T) {
	site := newProbeSite(t, "", nil)
	landing := site.newVisitor().get("/")
	if landing.status != 200 || landing.header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("landing: %d %q", landing.status, landing.header.Get("Content-Type"))
	}
	if landing.header.Get("Cache-Control") == "" {
		t.Error("the landing page has no Cache-Control")
	}
	for _, path := range []string{"/admin/setup", "/admin/sign-in", "/nothing-here"} {
		page := site.newVisitor().get(path)
		// PROBE: the setup and sign-in pages carry a CSRF-less form and the 404 the visitor's path; without
		// Cache-Control a shared cache or the back button may keep them.
		t.Logf("%s: Cache-Control %q", path, page.header.Get("Cache-Control"))
	}
}

// TestPostNeedsTheBoardsOrigin refuses a form post without a matching Origin or Referer.
func TestPostNeedsTheBoardsOrigin(t *testing.T) {
	site := newProbeSite(t, "", nil)
	form := setupForm().Encode()
	cases := []struct {
		// name names the case.
		name string

		// header replaces the browser's Origin; an empty value removes it.
		header http.Header

		// refused says whether the post must be refused.
		refused bool
	}{
		{"no origin no referer", http.Header{"Origin": {""}}, true},
		{"foreign origin", http.Header{"Origin": {"https://evil.example"}}, true},
		{"same host other port", http.Header{"Origin": {"http://" + strings.Split(site.host, ":")[0] + ":1"}}, true},
		{"same host other scheme", http.Header{"Origin": {"https://" + site.host}}, true},
		{"null origin", http.Header{"Origin": {"null"}}, true},
		{"origin as prefix", http.Header{"Origin": {site.origin + ".evil.example"}}, true},
		{"origin with path", http.Header{"Origin": {site.origin + "/"}}, true},
		{"foreign referer only", http.Header{"Origin": {""}, "Referer": {"https://evil.example/admin/setup"}}, true},
		{"referer prefix only", http.Header{"Origin": {""}, "Referer": {site.origin + ".evil.example/admin/setup"}}, true},
		{"own referer only", http.Header{"Origin": {""}, "Referer": {site.origin + "/admin/setup"}}, false},
		{"own origin", nil, false},
	}
	for _, c := range cases {
		visitor := site.newVisitor()
		result := visitor.do("POST", "/admin/setup", form, c.header)
		if c.refused && result.status != http.StatusForbidden {
			t.Errorf("%s: answered %d, want 403", c.name, result.status)
		}
		if !c.refused && result.status == http.StatusForbidden {
			t.Errorf("%s: a legitimate post was refused", c.name)
		}
		if c.refused {
			if account, err := site.web.Admin.Account(context.Background()); err == nil && account.Username != "" {
				t.Fatalf("%s: a refused post still created the admin", c.name)
			}
		}
	}
	if _, err := site.web.Admin.Account(context.Background()); err != nil {
		t.Log("setup through an allowed post did not create the admin:", err)
	}
}

// direct sends a request straight to the handler, bypassing the client's URL and Location parsing.
func (s *probeSite) direct(method, target, body string, header http.Header) answer {
	s.t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Host = s.host
	if method == "POST" {
		request.Header.Set("Origin", s.origin)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for key, values := range header {
		request.Header[http.CanonicalHeaderKey(key)] = values
	}
	recorder := httptest.NewRecorder()
	s.handler.ServeHTTP(recorder, request)
	return answer{status: recorder.Code, body: recorder.Body.String(), header: recorder.Header()}
}

// TestLanguageSwitchRefusesOpenRedirects redirects only to a local path.
func TestLanguageSwitchRefusesOpenRedirects(t *testing.T) {
	site := newProbeSite(t, "", nil)
	cases := []struct {
		// name names the case.
		name string

		// target is the return field.
		target string

		// allowed says whether the redirect must go to target exactly.
		allowed bool
	}{
		{"scheme relative", "//evil.com", false},
		{"backslash", "\\evil.com", false},
		{"slash backslash", "/\\evil.com", false},
		{"raw crlf", "/\r\nSet-Cookie: x=1", false},
		{"absolute", "http://evil.com", false},
		{"absolute https", "https://evil.com/admin", false},
		{"javascript", "javascript:alert(1)", false},
		{"own absolute", site.origin + "/admin", false},
		{"triple slash", "///evil.com", false},
		{"admin", "/admin", true},
		{"admin page with query", "/admin/board?x=1", true},
		{"root", "/", true},
		{"empty", "", true},
		{"no field", "-", true},
	}
	for _, c := range cases {
		form := url.Values{"language": {"de"}}
		if c.target != "-" {
			form.Set("return", c.target)
		}
		result := site.direct("POST", "/language", form.Encode(), nil)
		location := result.header.Get("Location")
		if result.status < 300 || result.status > 399 {
			t.Logf("%s: return %q answered %d (no redirect)", c.name, c.target, result.status)
		}
		if strings.HasPrefix(location, "//") || strings.Contains(location, "\\") || strings.ContainsAny(location, "\r\n") || strings.Contains(location, "evil") || strings.HasPrefix(location, "javascript:") {
			t.Errorf("%s: return %q redirected to %q", c.name, c.target, location)
		}
		if parsed, err := url.Parse(location); err == nil && parsed.Host != "" && parsed.Host != site.host {
			t.Errorf("%s: return %q redirected off the board to %q", c.name, c.target, location)
		}
		for _, value := range result.header.Values("Set-Cookie") {
			if strings.HasPrefix(value, "x=") || strings.HasPrefix(value, "Set-Cookie") {
				t.Errorf("%s: a cookie was injected: %q", c.name, value)
			}
		}
		if c.allowed && c.target != "-" && c.target != "" && location != c.target {
			t.Errorf("%s: return %q redirected to %q, want it kept", c.name, c.target, location)
		}
		if !c.allowed && location == c.target {
			t.Errorf("%s: return %q was followed", c.name, c.target)
		}
	}
	// PROBE: percent-encoded CR LF and slashes stay encoded in Location, so a browser asks this origin for
	// that odd path; harmless, but a redirect to a path that cannot exist could be refused instead.
	// A raw HTAB is legal in a field value (RFC 9110) and browsers strip it, leaving a local path.
	for _, target := range []string{"/%0d%0aSet-Cookie:x", "/%2f%2fevil.com", "/%5c%5cevil.com", "/\tevil.com"} {
		result := site.direct("POST", "/language", url.Values{"language": {"de"}, "return": {target}}.Encode(), nil)
		t.Logf("return %q redirects to %q", target, result.header.Get("Location"))
	}
}

// TestLanguageSwitchSetsTheLanguage changes the page language and refuses an unknown one.
func TestLanguageSwitchSetsTheLanguage(t *testing.T) {
	site := newProbeSite(t, "", nil)
	visitor := site.newVisitor()
	result := visitor.post("/language", url.Values{"language": {"de"}, "return": {"/"}})
	if result.status < 300 || result.status > 399 {
		t.Fatalf("switching the language answered %d", result.status)
	}
	for _, value := range result.header.Values("Set-Cookie") {
		t.Logf("language cookie: %s", value)
		lower := strings.ToLower(value)
		if !strings.Contains(lower, "samesite") {
			t.Errorf("the language cookie has no SameSite: %s", value)
		}
	}
	if page := visitor.get("/"); !strings.Contains(page.body, `<html lang="de">`) {
		t.Error("after switching to de the landing page is not German")
	}
	if page := visitor.do("GET", "/", "", http.Header{"Accept-Language": {"fr"}}); !strings.Contains(page.body, `<html lang="de">`) {
		t.Error("the chosen language does not beat Accept-Language")
	}
	for _, bad := range []string{"xx", "", "de\r\nX", "../en", "<script>", strings.Repeat("d", 1000)} {
		fresh := site.newVisitor()
		result := fresh.post("/language", url.Values{"language": {bad}, "return": {"/"}})
		page := fresh.get("/")
		if !strings.Contains(page.body, `<html lang="en">`) {
			t.Errorf("language %q answered %d and left the page in another language", bad, result.status)
		}
		for _, value := range result.header.Values("Set-Cookie") {
			if strings.ContainsAny(value, "\r\n<") {
				t.Errorf("language %q set cookie %q", bad, value)
			}
		}
	}
	if result := site.newVisitor().get("/language"); result.status == 200 {
		t.Logf("GET /language answers 200")
	}
}

// TestStaticFilesAreTypedAndCached serves each type correctly with a cache policy.
func TestStaticFilesAreTypedAndCached(t *testing.T) {
	site := newProbeSite(t, "", nil)
	visitor := site.newVisitor()
	landing := visitor.get("/").body
	versioned := regexp.MustCompile(`"(/static/[^"?]+\?v=[0-9a-f]+)"`).FindAllStringSubmatch(landing, -1)
	if len(versioned) == 0 {
		t.Fatal("the landing page links no versioned static file")
	}
	for _, match := range versioned {
		file := visitor.get(match[1])
		if file.status != 200 {
			t.Errorf("%s: %d", match[1], file.status)
		}
		if cache := file.header.Get("Cache-Control"); !strings.Contains(cache, "immutable") || !strings.Contains(cache, "max-age=") {
			t.Errorf("%s: Cache-Control %q", match[1], cache)
		}
	}
	types := map[string]string{"/static/style.css": "text/css", "/static/password.js": "text/javascript", "/static/sharing.js": "text/javascript", "/static/logo.png": "image/png", "/static/fonts/Archivo.woff2": "font/woff2"}
	for path, want := range types {
		file := visitor.get(path)
		if file.status != 200 || !strings.HasPrefix(file.header.Get("Content-Type"), want) {
			t.Errorf("%s: %d %q, want %s", path, file.status, file.header.Get("Content-Type"), want)
		}
		if file.header.Get("ETag") == "" && file.header.Get("Last-Modified") == "" {
			t.Errorf("%s has neither ETag nor Last-Modified", path)
		}
		unversioned := file.header.Get("Cache-Control")
		if strings.Contains(unversioned, "immutable") {
			t.Errorf("%s without a version is immutable: %q", path, unversioned)
		}
	}
	// PROBE: the font's licence and README live beside it in the embedded tree; are they served?
	for _, path := range []string{"/static/fonts/OFL.txt", "/static/fonts/README.md", "/static/", "/static/fonts/", "/static"} {
		t.Logf("%s: %d %q", path, visitor.get(path).status, visitor.get(path).header.Get("Content-Type"))
	}
}

// TestStaticTraversalIsRefused never serves a file outside the static tree.
func TestStaticTraversalIsRefused(t *testing.T) {
	site := newProbeSite(t, "", nil)
	targets := []string{"/static/../../etc/passwd", "/static/%2e%2e/%2e%2e/etc/passwd", "/static/..%2f..%2fetc%2fpasswd", "/static/../templates/layout.html", "/static/%2e%2e/templates/layout.html",
		"/static/../strings/en.json", "/static/..%5c..%5cetc/passwd", "/static/./style.css/../../templates/layout.html", "/static//style.css", "/static/style.css%00.png", "/static/fonts/../../../go.mod", "/static/%2e%2e%2f%2e%2e%2fgo.mod"}
	for _, target := range targets {
		request := httptest.NewRequest("GET", target, nil)
		recorder := httptest.NewRecorder()
		site.handler.ServeHTTP(recorder, request)
		body := recorder.Body.String()
		if strings.Contains(body, "root:") || strings.Contains(body, "{{") || strings.Contains(body, "module github.com") || strings.Contains(body, `"Account_AddPasskey"`) {
			t.Errorf("%s served a file outside static: %d %.80q", target, recorder.Code, body)
		}
		if recorder.Code == 200 && !strings.HasSuffix(target, "/static//style.css") {
			t.Errorf("%s answered 200", target)
		}
		if location := recorder.Header().Get("Location"); location != "" {
			t.Logf("%s redirects to %s", target, location)
		}
	}
}

// TestUnknownPathIs404InTheVisitorsLanguage answers 404 as an HTML page in the right language.
func TestUnknownPathIs404InTheVisitorsLanguage(t *testing.T) {
	site := newProbeSite(t, "", nil)
	for _, language := range []string{"en", "de", "ja", "pt-BR", "zh-Hans"} {
		page := site.newVisitor().do("GET", "/nothing-here", "", http.Header{"Accept-Language": {language}})
		if page.status != 404 || page.header.Get("Content-Type") != "text/html; charset=utf-8" {
			t.Errorf("%s: %d %q", language, page.status, page.header.Get("Content-Type"))
		}
		if !strings.Contains(page.body, `<html lang="`+language+`">`) {
			t.Errorf("%s: the 404 page is in another language", language)
		}
	}
	for _, path := range []string{"/admin/nothing", "/join/", "/join/x", "/static/nothing.css", "/index.html", "/..", "/%", "/%7f", "/a%00b", "/%2e%2e/", "//admin/board", "/admin//board", "/ADMIN/board", "/admin/board/", "/./", "/%E2%80%AE"} {
		status := rawStatus(t, site, "GET "+path+" HTTP/1.1\r\nHost: "+site.host+"\r\n\r\n")
		if status >= 500 {
			t.Errorf("%s: %d", path, status)
		}
		t.Logf("%s: %d", path, status)
	}
}

// rawStatus writes one HTTP/1.1 request over a plain connection and returns its status code.
func rawStatus(t *testing.T, site *probeSite, raw string) int {
	t.Helper()
	connection, err := net.Dial("tcp", strings.TrimPrefix(site.server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(connection, raw); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the status line: %v", err)
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		t.Fatalf("bad status line %q", line)
	}
	status, _ := strconv.Atoi(fields[1])
	return status
}

// TestHEADRequests answers HEAD like GET without a body.
func TestHEADRequests(t *testing.T) {
	site := newProbeSite(t, "", nil)
	visitor := site.newVisitor()
	for _, path := range []string{"/", "/static/style.css", "/admin/setup", "/nothing"} {
		get := visitor.get(path)
		head := visitor.do("HEAD", path, "", nil)
		if head.status != get.status {
			t.Errorf("HEAD %s answered %d, GET %d", path, head.status, get.status)
		}
		if head.body != "" {
			t.Errorf("HEAD %s carried %d body bytes", path, len(head.body))
		}
		if head.header.Get("Content-Type") != get.header.Get("Content-Type") {
			t.Errorf("HEAD %s Content-Type %q, GET %q", path, head.header.Get("Content-Type"), get.header.Get("Content-Type"))
		}
	}
}

// TestSetupPageBeforeAndAfter shows setup before and not after.
func TestSetupPageBeforeAndAfter(t *testing.T) {
	site := newProbeSite(t, "", nil)
	before := site.newVisitor().get("/admin/setup")
	if before.status != 200 || !strings.Contains(before.body, `name="code"`) {
		t.Fatalf("before setup: %d", before.status)
	}
	if strings.Contains(site.newVisitor().get("/admin/sign-in").body, `name="username"`) && !strings.Contains(site.newVisitor().get("/admin/sign-in").body, `name="code"`) {
		t.Error("sign-in with a password is offered before any admin exists")
	}
	site.signedIn()
	after := site.newVisitor().get("/admin/setup")
	// PROBE: after setup the page may 404, redirect, or show sign-in; it must not show the setup form.
	t.Logf("after setup GET /admin/setup answers %d (Location %q)", after.status, after.header.Get("Location"))
	if strings.Contains(after.body, `name="code"`) && strings.Contains(after.body, `name="boardName"`) {
		t.Error("the setup form is still shown after setup")
	}
	again := site.newVisitor().post("/admin/setup", url.Values{"code": {probeSetupCode}, "boardName": {"Again"}, "username": {"mallory"}, "password": {"another password"}, "confirm": {"another password"}})
	t.Logf("a second setup post answers %d", again.status)
	if account, _ := site.web.Admin.Account(context.Background()); account.Username != "avery" {
		t.Fatal("a second setup replaced the admin")
	}
	for _, value := range again.header.Values("Set-Cookie") {
		if strings.Contains(value, "session") && !strings.Contains(value, "Max-Age=0") {
			t.Errorf("a second setup handed out a session: %s", value)
		}
	}
}

// TestSetupRefusals refuses a wrong code, mismatched passwords and a short password without creating the admin.
func TestSetupRefusals(t *testing.T) {
	site := newProbeSite(t, "", nil)
	cases := map[string]func(url.Values){
		"wrong code":          func(form url.Values) { form.Set("code", "WrongCodeWrongCode") },
		"code with spaces":    func(form url.Values) { form.Set("code", " "+probeSetupCode+" ") },
		"code lowercase":      func(form url.Values) { form.Set("code", strings.ToLower(probeSetupCode)) },
		"empty code":          func(form url.Values) { form.Set("code", "") },
		"mismatched confirm":  func(form url.Values) { form.Set("confirm", "a long passwordx") },
		"short password":      func(form url.Values) { form.Set("password", "short"); form.Set("confirm", "short") },
		"empty username":      func(form url.Values) { form.Set("username", "") },
		"65 char username":    func(form url.Values) { form.Set("username", strings.Repeat("u", 65)) },
		"1025 char password":  func(form url.Values) { p := strings.Repeat("p", 1025); form.Set("password", p); form.Set("confirm", p) },
		"board name 61 chars": func(form url.Values) { form.Set("boardName", strings.Repeat("b", 61)) },
	}
	for name, mutate := range cases {
		form := setupForm()
		mutate(form)
		result := site.newVisitor().post("/admin/setup", form)
		if result.status == 200 && strings.Contains(result.body, "Morning Crew") && !strings.Contains(result.body, `name="code"`) {
			t.Errorf("%s: setup went through (%d)", name, result.status)
		}
		if account, err := site.web.Admin.Account(context.Background()); err == nil && account.Username != "" {
			t.Fatalf("%s: the admin was created", name)
		}
		if result.status >= 500 {
			t.Errorf("%s: %d", name, result.status)
		}
		t.Logf("%s: %d", name, result.status)
	}
	// PROBE: a code with surrounding spaces or in another case is typed by hand; record whether it is accepted.
}

// TestSignInLimiter answers 429 with Retry-After once the per-address burst is spent.
func TestSignInLimiter(t *testing.T) {
	// Setup is itself one attempt from this address, so the burst is one larger than the wrong sign-ins.
	site := newProbeSite(t, "", func(s *web.Site) { s.SignInLimiter = ratelimit.New(4, 1) })
	site.signedIn()
	visitor := site.newVisitor()
	wrong := url.Values{"username": {"avery"}, "password": {"wrong password"}}
	for attempt := 1; attempt <= 3; attempt++ {
		if result := visitor.post("/admin/sign-in", wrong); result.status != http.StatusUnauthorized {
			t.Fatalf("attempt %d answered %d, want 401", attempt, result.status)
		}
	}
	fourth := visitor.post("/admin/sign-in", wrong)
	if fourth.status != http.StatusTooManyRequests {
		t.Fatalf("the fourth wrong sign-in answered %d, want 429", fourth.status)
	}
	// PROBE: the API's 429 carries Retry-After (openapi TooManyRequests) and ratelimit.Allow returns the
	// seconds for it; the portal's 429 is outside the API contract. Record whether it carries one.
	if retry, err := strconv.Atoi(fourth.header.Get("Retry-After")); err != nil || retry < 1 {
		t.Logf("the portal's 429 has Retry-After %q", fourth.header.Get("Retry-After"))
	}
	right := visitor.post("/admin/sign-in", url.Values{"username": {"avery"}, "password": {"a long password"}})
	if right.status != http.StatusTooManyRequests {
		t.Errorf("a right password while limited answered %d, want 429 (the limiter must not reveal it)", right.status)
	}
	if other := site.newVisitor().post("/admin/sign-in", url.Values{"username": {"avery"}, "password": {"a long password"}}); other.status != http.StatusTooManyRequests {
		t.Logf("a second browser from the same address answered %d", other.status)
	}
}

// TestSignInLimiterCoversSetup limits setup code guesses too.
func TestSignInLimiterCoversSetup(t *testing.T) {
	site := newProbeSite(t, "", func(s *web.Site) { s.SignInLimiter = ratelimit.New(3, 1) })
	visitor := site.newVisitor()
	form := setupForm()
	form.Set("code", "WrongCodeWrongCode")
	for attempt := 1; attempt <= 3; attempt++ {
		if result := visitor.post("/admin/setup", form); result.status == http.StatusTooManyRequests {
			t.Fatalf("attempt %d was already limited", attempt)
		}
	}
	if fourth := visitor.post("/admin/setup", form); fourth.status != http.StatusTooManyRequests {
		t.Errorf("the fourth wrong setup code answered %d, want 429", fourth.status)
	}
	if right := visitor.post("/admin/setup", setupForm()); right.status != http.StatusTooManyRequests {
		t.Errorf("the right code while limited answered %d, want 429", right.status)
	}
}

// TestSignInLimiterUsesClientAddress keys the limiter by the address ClientAddress returns.
func TestSignInLimiterUsesClientAddress(t *testing.T) {
	site := newProbeSite(t, "", func(s *web.Site) {
		s.SignInLimiter = ratelimit.New(1, 1)
		s.ClientAddress = func(request *http.Request) string { return request.Header.Get("X-Test-Address") }
	})
	site.signedIn()
	wrong := url.Values{"username": {"avery"}, "password": {"wrong password"}}.Encode()
	visitor := site.newVisitor()
	visitor.do("POST", "/admin/sign-in", wrong, http.Header{"X-Test-Address": {"1.1.1.1"}})
	if result := visitor.do("POST", "/admin/sign-in", wrong, http.Header{"X-Test-Address": {"1.1.1.1"}}); result.status != http.StatusTooManyRequests {
		t.Errorf("the same address's second attempt answered %d", result.status)
	}
	if result := visitor.do("POST", "/admin/sign-in", wrong, http.Header{"X-Test-Address": {"2.2.2.2"}}); result.status == http.StatusTooManyRequests {
		t.Error("another address was limited")
	}
}

// sessionCookie finds the session cookie among Set-Cookie values, by exclusion of the language cookie.
func sessionCookie(t *testing.T, values []string) string {
	t.Helper()
	for _, value := range values {
		lower := strings.ToLower(value)
		if strings.Contains(lower, "lang") && !strings.Contains(lower, "session") {
			continue
		}
		if strings.Contains(lower, "max-age=0") {
			continue
		}
		return value
	}
	t.Fatalf("no session cookie among %v", values)
	return ""
}

// TestSessionCookieAttributes sets a scoped, HttpOnly, SameSite=Strict cookie that lasts 12 hours.
func TestSessionCookieAttributes(t *testing.T) {
	site := newProbeSite(t, "", nil)
	_, setup := site.signedIn()
	cookie := sessionCookie(t, setup.header.Values("Set-Cookie"))
	t.Logf("session cookie: %s", cookie)
	lower := strings.ToLower(cookie)
	if !strings.Contains(lower, "httponly") {
		t.Error("the session cookie is not HttpOnly")
	}
	if !strings.Contains(lower, "samesite=strict") {
		t.Error("the session cookie is not SameSite=Strict")
	}
	if !strings.Contains(lower, "path=/admin") {
		t.Error("the session cookie is not scoped to /admin")
	}
	if strings.Contains(lower, "secure") {
		t.Error("an http board set a Secure cookie, which the browser drops")
	}
	if match := regexp.MustCompile(`(?i)max-age=(\d+)`).FindStringSubmatch(cookie); match != nil {
		seconds, _ := strconv.Atoi(match[1])
		if seconds < 11*3600 || seconds > 13*3600 {
			t.Errorf("Max-Age is %d seconds, want about 12 hours", seconds)
		}
	} else if match := regexp.MustCompile(`(?i)expires=([^;]+)`).FindStringSubmatch(cookie); match != nil {
		expires, err := http.ParseTime(match[1])
		if err != nil {
			t.Errorf("Expires %q does not parse", match[1])
		} else if lifetime := time.Until(expires); lifetime < 11*time.Hour || lifetime > 13*time.Hour {
			t.Logf("PROBE: Expires %v is %v away (the board's clock is fixed at 2026-10-21, so this is expected to drift)", expires, lifetime)
		}
	} else {
		t.Log("PROBE: the session cookie is a session cookie with no Max-Age or Expires; the server-side 12-hour expiry still ends it")
	}
	name := strings.SplitN(cookie, "=", 2)[0]
	value := strings.SplitN(strings.SplitN(cookie, ";", 2)[0], "=", 2)[1]
	if len(value) < 32 {
		t.Errorf("the session token %q is short", value)
	}
	if strings.HasPrefix(name, "__Host-") {
		t.Errorf("a __Host- cookie %q cannot have Path=/admin and will be dropped", name)
	}
}

// TestSecureCookieOverHTTPS marks the session cookie Secure when the public URL is https.
func TestSecureCookieOverHTTPS(t *testing.T) {
	site := newProbeSite(t, "https://board.example", nil)
	setup := site.newVisitor().post("/admin/setup", setupForm())
	if setup.status != http.StatusSeeOther && setup.status != http.StatusOK {
		t.Fatalf("setup answered %d", setup.status)
	}
	cookie := sessionCookie(t, setup.header.Values("Set-Cookie"))
	if !strings.Contains(strings.ToLower(cookie), "secure") {
		t.Errorf("an https board's session cookie is not Secure: %s", cookie)
	}
}

// TestSignOutClearsTheCookie expires the cookie and ends the session server-side.
func TestSignOutClearsTheCookie(t *testing.T) {
	site := newProbeSite(t, "", nil)
	admin, setup := site.signedIn()
	original := sessionCookie(t, setup.header.Values("Set-Cookie"))
	name := strings.SplitN(original, "=", 2)[0]
	token := strings.SplitN(strings.SplitN(original, ";", 2)[0], "=", 2)[1]
	out := admin.post("/admin/sign-out", url.Values{"csrf": {admin.csrf("/admin/board")}})
	if out.status != 200 && out.status != http.StatusSeeOther {
		t.Fatalf("sign-out answered %d", out.status)
	}
	cleared := false
	for _, value := range out.header.Values("Set-Cookie") {
		if strings.HasPrefix(value, name+"=") && (strings.Contains(strings.ToLower(value), "max-age=0") || strings.Contains(value, "1970")) {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("signing out did not clear %s: %v", name, out.header.Values("Set-Cookie"))
	}
	page := site.direct("GET", "/admin/board", "", http.Header{"Cookie": {name + "=" + token}})
	if page.status == 200 && strings.Contains(page.body, `name="csrf"`) {
		t.Error("the old session token still opens the portal after sign-out")
	}
}

// TestTamperedSessionCookie treats a forged or truncated cookie as signed out, never as an error.
func TestTamperedSessionCookie(t *testing.T) {
	site := newProbeSite(t, "", nil)
	_, setup := site.signedIn()
	original := sessionCookie(t, setup.header.Values("Set-Cookie"))
	name := strings.SplitN(original, "=", 2)[0]
	token := strings.SplitN(strings.SplitN(original, ";", 2)[0], "=", 2)[1]
	for _, forged := range []string{token[:len(token)-1], token + "x", strings.ToUpper(token), "", "../x", strings.Repeat("A", 2000), "\x00", token[:len(token)/2] + strings.Repeat("A", len(token)-len(token)/2)} {
		page := site.direct("GET", "/admin/board", "", http.Header{"Cookie": {name + "=" + forged}})
		if page.status >= 500 {
			t.Errorf("cookie %.20q: %d", forged, page.status)
		}
		if page.status == 200 && strings.Contains(page.body, `name="csrf"`) {
			t.Errorf("cookie %.20q opened the portal", forged)
		}
	}
}

// TestCSRFTokenRequiredOnSignedInPosts refuses a post without the token and accepts it as a header.
func TestCSRFTokenRequiredOnSignedInPosts(t *testing.T) {
	site := newProbeSite(t, "", nil)
	admin, _ := site.signedIn()
	token := admin.csrf("/admin/board")
	without := admin.post("/admin/board", url.Values{"name": {"Hijacked"}})
	if without.status != http.StatusForbidden {
		t.Errorf("a post without the token answered %d", without.status)
	}
	header := admin.do("POST", "/admin/board", url.Values{"name": {"Via Header"}}.Encode(), http.Header{"X-CSRF-Token": {token}})
	if header.status != 200 && header.status != http.StatusSeeOther {
		t.Errorf("a post with the token in X-CSRF-Token answered %d", header.status)
	}
	if board, _ := site.service.Board(context.Background()); board.Name != "Via Header" {
		t.Errorf("the board is named %q after a header-token post", board.Name)
	}
	cases := map[string]string{"wrong": "not the token", "empty": "", "prefix": token[:len(token)-1], "suffix": token + "x", "case": strings.ToUpper(token)}
	for name, bad := range cases {
		result := admin.do("POST", "/admin/board", url.Values{"name": {"Hijacked"}}.Encode(), http.Header{"X-CSRF-Token": {bad}})
		if result.status != http.StatusForbidden {
			t.Errorf("header token %s answered %d", name, result.status)
		}
		result = admin.post("/admin/board", url.Values{"name": {"Hijacked"}, "csrf": {bad}})
		if result.status != http.StatusForbidden {
			t.Errorf("form token %s answered %d", name, result.status)
		}
	}
	if board, _ := site.service.Board(context.Background()); board.Name == "Hijacked" {
		t.Fatal("a post with a bad token changed the board")
	}
	if match := csrfPattern.FindStringSubmatch(site.newVisitor().get("/admin/sign-in").body); match != nil {
		t.Errorf("the sign-in page carries a CSRF token %q for a visitor without a session", match[1])
	}
}

// TestCSRFTokenIsNotAcceptedFromAnotherSession refuses one admin session's token on another.
func TestCSRFTokenIsNotAcceptedFromAnotherSession(t *testing.T) {
	site := newProbeSite(t, "", nil)
	first, _ := site.signedIn()
	firstToken := first.csrf("/admin/board")
	second := site.newVisitor()
	if result := second.post("/admin/sign-in", url.Values{"username": {"avery"}, "password": {"a long password"}}); result.status != 200 && result.status != http.StatusSeeOther {
		t.Fatalf("second sign-in answered %d", result.status)
	}
	secondToken := second.csrf("/admin/board")
	if firstToken == secondToken {
		t.Fatal("two sessions share one CSRF token")
	}
	if result := second.post("/admin/board", url.Values{"csrf": {firstToken}, "name": {"Crossed"}}); result.status != http.StatusForbidden {
		t.Errorf("another session's token answered %d", result.status)
	}
}

// formBody builds an x-www-form-urlencoded body of exactly size bytes with the given fields first.
func formBody(fields url.Values, size int) string {
	prefix := fields.Encode() + "&pad="
	if size < len(prefix) {
		return prefix
	}
	return prefix + strings.Repeat("a", size-len(prefix))
}

// TestFormBodyLimit refuses a form 300 KB over the Board page's limit, with the token in the form and in the header.
func TestFormBodyLimit(t *testing.T) {
	site := newProbeSite(t, "", nil)
	admin, _ := site.signedIn()
	token := admin.csrf("/admin/board")
	large := formBody(url.Values{"csrf": {token}, "name": {"Big"}}, boardFormLimit+300*1024)
	inForm := admin.do("POST", "/admin/board", large, nil)
	// PROBE: with the token inside an oversize body the answer is 403 (the token was never read), not 413.
	if inForm.status != http.StatusRequestEntityTooLarge && inForm.status != http.StatusBadRequest && inForm.status != http.StatusForbidden {
		t.Errorf("an oversize form with the token in the form answered %d", inForm.status)
	}
	inHeader := admin.do("POST", "/admin/board", formBody(url.Values{"name": {"Big"}}, boardFormLimit+300*1024), http.Header{"X-CSRF-Token": {token}})
	if inHeader.status != http.StatusRequestEntityTooLarge && inHeader.status != http.StatusBadRequest {
		t.Errorf("an oversize form with the token in the header answered %d", inHeader.status)
	}
	if board, _ := site.service.Board(context.Background()); board.Name == "Big" {
		t.Error("an oversize form changed the board")
	}
	t.Logf("oversize: form token %d, header token %d", inForm.status, inHeader.status)
	for _, size := range []int{boardFormLimit - 1, boardFormLimit, boardFormLimit + 1} {
		result := admin.do("POST", "/admin/board", formBody(url.Values{"csrf": {token}, "name": {"Edge"}}, size), nil)
		t.Logf("a %d-byte form answers %d", size, result.status)
	}
	// PROBE: the sign-in and setup forms are posted without a session; record whether the limit covers them.
	for _, size := range []int{300 * 1024, 1024 * 1024, 4 * 1024 * 1024} {
		unsigned := site.newVisitor().do("POST", "/admin/sign-in", formBody(url.Values{"username": {"avery"}, "password": {"x"}}, size), nil)
		t.Logf("a %d-byte sign-in form answers %d", size, unsigned.status)
	}
	setup := newProbeSite(t, "", nil).newVisitor().do("POST", "/admin/setup", formBody(setupForm(), 300*1024), nil)
	t.Logf("a 300 KB setup form answers %d", setup.status)
}

// TestPortalNeedsASession refuses admin pages and posts without a session.
func TestPortalNeedsASession(t *testing.T) {
	site := newProbeSite(t, "", nil)
	site.signedIn()
	stranger := site.newVisitor()
	for _, path := range []string{"/admin/board", "/admin/members", "/admin/rules", "/admin/account", "/admin"} {
		page := stranger.get(path)
		if page.status >= 500 {
			t.Errorf("GET %s: %d", path, page.status)
		}
		if strings.Contains(page.body, `name="csrf"`) || strings.Contains(page.body, "Saved.") {
			t.Errorf("GET %s shows portal content without a session", path)
		}
		t.Logf("GET %s without a session: %d", path, page.status)
	}
	for _, path := range []string{"/admin/board", "/admin/rules", "/admin/sign-out", "/admin/members/MemberMemberMemberMe01/approve", "/admin/account/password"} {
		result := stranger.post(path, url.Values{"name": {"Hijacked"}, "csrf": {"x"}})
		if result.status != http.StatusForbidden && result.status != http.StatusUnauthorized && result.status != http.StatusNotFound && !(result.status == 200 && strings.Contains(result.body, "Sign in")) {
			t.Errorf("POST %s without a session answered %d", path, result.status)
		}
		t.Logf("POST %s without a session: %d", path, result.status)
	}
	if board, _ := site.service.Board(context.Background()); board.Name == "Hijacked" {
		t.Fatal("a post without a session changed the board")
	}
}

// TestWrongMethods answers 405 or 404, never 500, to unexpected methods.
func TestWrongMethods(t *testing.T) {
	site := newProbeSite(t, "", nil)
	visitor := site.newVisitor()
	for _, path := range []string{"/", "/language", "/admin/setup", "/admin/sign-in", "/static/style.css", "/join/x"} {
		for _, method := range []string{"PUT", "DELETE", "PATCH", "OPTIONS", "TRACE", "PROPFIND"} {
			result := visitor.do(method, path, "", nil)
			if result.status >= 500 {
				t.Errorf("%s %s: %d", method, path, result.status)
			}
			if method == "TRACE" && result.status == 200 {
				t.Errorf("TRACE %s is answered 200", path)
			}
		}
	}
	if result := visitor.do("POST", "/", "", nil); result.status >= 500 {
		t.Errorf("POST /: %d", result.status)
	}
}

// TestHostHeaderDoesNotLeakIntoLinks builds absolute links from the public URL, not the Host header.
func TestHostHeaderDoesNotLeakIntoLinks(t *testing.T) {
	site := newProbeSite(t, "", nil)
	page := site.newVisitor().do("GET", "/", "", http.Header{"Host": {"evil.example"}})
	if page.status >= 500 {
		t.Fatalf("a foreign Host header: %d", page.status)
	}
	if strings.Contains(page.body, "evil.example") {
		t.Error("the Host header appears in the page (the app link or invite would point at the attacker)")
	}
	t.Logf("a foreign Host header answers %d", page.status)
}

// TestBasePathIsHonoured serves under the public URL's path when it has one.
func TestBasePathIsHonoured(t *testing.T) {
	site := newProbeSite(t, "http://board.example/boards/crew", nil)
	visitor := site.newVisitor()
	root := visitor.get("/boards/crew/")
	t.Logf("GET /boards/crew/ answers %d; GET / answers %d; GET /boards/crew answers %d", root.status, visitor.get("/").status, visitor.get("/boards/crew").status)
	if root.status != 200 {
		t.Skip("the base path is not served at /boards/crew/; the rest of this probe does not apply")
	}
	if !strings.Contains(root.body, `href="/boards/crew/static/style.css`) {
		t.Error("the stylesheet is not linked under the base path")
	}
	if visitor.get("/boards/crew/static/style.css").status != 200 {
		t.Error("the stylesheet is not served under the base path")
	}
	result := visitor.post("/boards/crew/language", url.Values{"language": {"de"}, "return": {"/admin"}})
	t.Logf("language switch under a base path redirects to %q", result.header.Get("Location"))
	if location := result.header.Get("Location"); location != "" && !strings.HasPrefix(location, "/boards/crew") {
		t.Errorf("the language switch redirected outside the base path to %q", location)
	}
}
