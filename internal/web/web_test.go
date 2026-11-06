package web

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"io/fs"
	"mime/multipart"
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
	"github.com/jacobjohnston/FitHalo-Scoreboards/spec"
)

// testSetupCode is the setup code every test board accepts.
const testSetupCode = "SetupCodeSetupCode"

// A browser is a visitor with cookies, posting from the board's own pages as a real browser does.
type browser struct {
	// t is the test the browser fails.
	t *testing.T

	// client keeps the browser's cookies.
	client *http.Client

	// server is the board the browser visits.
	server *httptest.Server
}

// A page is one answer a browser received.
type page struct {
	// status is the answer's status code.
	status int

	// body is the answer's whole body.
	body string

	// header is the answer's header.
	header http.Header
}

// A site is a test board, served and reachable through its parts.
type site struct {
	// t is the test the site belongs to.
	t *testing.T

	// server serves the site.
	server *httptest.Server

	// service is the board's service.
	service *scoreboard.Service

	// admin is the board's admin.
	admin *admin.Admin

	// web is the site under test.
	web *Site
}

// newSite starts a board in a temporary directory, served on a local test server.
func newSite(t *testing.T) *site {
	t.Helper()
	return newConfiguredSite(t, nil)
}

// newConfiguredSite starts a board like newSite, letting configure change the Site before it serves.
func newConfiguredSite(t *testing.T, configure func(*Site)) *site {
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
	board.WeekStartDay = "monday" // the tests' dates are Mondays
	if _, _, err := service.EnsureBoard(context.Background(), board); err != nil {
		t.Fatal(err)
	}
	administrator := &admin.Admin{Store: directory, Now: now, SetupCode: testSetupCode}
	result := &site{t: t, service: service, admin: administrator}
	result.server = httptest.NewUnstartedServer(nil)
	publicURL, _ := url.Parse("http://" + result.server.Listener.Addr().String())
	result.web = &Site{Service: service, Admin: administrator, PublicURL: publicURL}
	if configure != nil {
		configure(result.web)
	}
	handler, err := result.web.Handler()
	if err != nil {
		t.Fatal(err)
	}
	result.server.Config.Handler = handler
	result.server.Start()
	t.Cleanup(result.server.Close)
	return result
}

// newBrowser returns a visitor with an empty cookie jar.
func (s *site) newBrowser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: s.t, server: s.server, client: &http.Client{Jar: jar}}
}

// request sends a request from the board's own origin and reads the whole answer.
func (b *browser) request(method, path string, body io.Reader, header http.Header) page {
	b.t.Helper()
	request, err := http.NewRequest(method, b.server.URL+path, body)
	if err != nil {
		b.t.Fatal(err)
	}
	request.Header.Set("Origin", b.server.URL)
	for key, values := range header {
		request.Header[key] = values
	}
	response, err := b.client.Do(request)
	if err != nil {
		b.t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return page{status: response.StatusCode, body: string(data), header: response.Header}
}

// get fetches path.
func (b *browser) get(path string) page {
	b.t.Helper()
	return b.request("GET", path, nil, nil)
}

// post submits form to path.
func (b *browser) post(path string, form url.Values) page {
	b.t.Helper()
	return b.request("POST", path, strings.NewReader(form.Encode()), http.Header{"Content-Type": {"application/x-www-form-urlencoded"}})
}

// csrfPattern finds the CSRF token in a page's meta tag.
var csrfPattern = regexp.MustCompile(`<meta name="csrf" content="([^"]+)">`)

// csrf returns the CSRF token on the page at path.
func (b *browser) csrf(path string) string {
	b.t.Helper()
	match := csrfPattern.FindStringSubmatch(b.get(path).body)
	if match == nil {
		b.t.Fatalf("%s has no CSRF token", path)
	}
	return match[1]
}

// signedInBrowser sets the board up and returns the admin's signed-in browser.
func (s *site) signedInBrowser() *browser {
	s.t.Helper()
	visitor := s.newBrowser()
	result := visitor.post("/admin/setup", url.Values{
		"code": {testSetupCode}, "boardName": {"Morning Crew"}, "username": {"avery"},
		"password": {"a long password"}, "confirm": {"a long password"},
	})
	if result.status != http.StatusOK || !strings.Contains(result.body, "Morning Crew") {
		s.t.Fatalf("setup ended at %d: %s", result.status, result.body)
	}
	return visitor
}

// expect fails the test unless result has status and contains every text.
func expect(t *testing.T, result page, status int, contains ...string) {
	t.Helper()
	if result.status != status {
		t.Fatalf("got status %d, want %d", result.status, status)
	}
	for _, text := range contains {
		if !strings.Contains(result.body, text) {
			t.Fatalf("the page does not contain %q", text)
		}
	}
}

// TestEveryLanguageHasEveryString checks every key a template or a handler names has a string.
func TestEveryLanguageHasEveryString(t *testing.T) {
	files, _ := fs.Sub(assets, "assets")
	localizer, err := LoadLocalizer(files)
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, key := range localizer.Keys() {
		known[key] = true
	}
	used := map[string]bool{}
	templates, _ := fs.Glob(files, "templates/*.html")
	for _, name := range templates {
		data, _ := fs.ReadFile(files, name)
		for _, match := range regexp.MustCompile(`\.T "([A-Za-z_]+)"`).FindAllStringSubmatch(string(data), -1) {
			used[match[1]] = true
		}
	}
	for _, level := range []string{"progress", "score", "details"} {
		used["Sharing_Score_"+level] = true
	}
	for _, level := range []string{"none", "points", "summary", "full"} {
		used["Sharing_Activity_"+level] = true
	}
	for _, day := range []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"} {
		used["Weekday_"+day] = true
	}
	for _, key := range notices {
		used[key] = true
	}
	for key := range used {
		if !known[key] {
			t.Errorf("%s is used but has no string", key)
		}
	}
}

// TestLanguageMatching maps browsers' preferences onto FitHalo's ten.
func TestLanguageMatching(t *testing.T) {
	cases := map[string]string{
		"":                        "en",
		"de-AT,de;q=0.9":          "de",
		"es-MX":                   "es-419",
		"es-ES":                   "es",
		"es":                      "es",
		"pt-PT":                   "pt-BR",
		"zh-CN":                   "zh-Hans",
		"zh-Hans-SG":              "zh-Hans",
		"zh-TW,ja;q=0.5":          "ja",
		"nl,fr;q=0.8,en;q=0.7":    "fr",
		"en-GB":                   "en",
		"ko-KR;q=0.1,it-CH;q=0.9": "it",
		"xx":                      "en",
	}
	for header, expected := range cases {
		if got := MatchLanguage(header); got != expected {
			t.Errorf("MatchLanguage(%q) = %s, want %s", header, got, expected)
		}
	}
}

// TestEveryPageInEveryLanguage draws each page in each language with no error or string key showing.
func TestEveryPageInEveryLanguage(t *testing.T) {
	board := newSite(t)
	visitor := board.signedInBrowser()
	for _, language := range Languages {
		for _, path := range []string{"/", "/admin/board", "/admin/members", "/admin/rules", "/admin/account", "/nothing-here"} {
			result := visitor.request("GET", path, nil, http.Header{"Accept-Language": {language}})
			if result.status >= 500 {
				t.Fatalf("%s in %s: %d", path, language, result.status)
			}
			if !strings.Contains(result.body, `<html lang="`+language+`">`) {
				t.Fatalf("%s in %s came back in another language", path, language)
			}
			if regexp.MustCompile(`>[A-Z][a-z]+_[A-Za-z_]+<`).MatchString(result.body) {
				t.Fatalf("%s in %s shows a string key", path, language)
			}
		}
	}
}

// TestLandingPage shows the invite on the address and its link, hides it on request, and flags old links.
func TestLandingPage(t *testing.T) {
	board := newSite(t)
	visitor := board.signedInBrowser()
	current, _ := board.service.Board(context.Background())

	landing := visitor.get("/")
	expect(t, landing, http.StatusOK, "Join the Morning Crew FitHalo Scoreboard!", "<svg", "fithalo://scoreboard?code="+current.InviteCode)
	if policy := landing.header.Get("Content-Security-Policy"); !strings.Contains(policy, "script-src 'self'") || strings.Contains(policy, "unsafe") {
		t.Fatalf("the content security policy is %q", policy)
	}
	if policy := landing.header.Get("Referrer-Policy"); policy != "same-origin" {
		// no-referrer makes browsers send Origin: null on the board's own forms, which the origin check refuses.
		t.Fatalf("the referrer policy is %q", policy)
	}
	if strings.Contains(landing.body, "<script") || strings.Contains(landing.body, `style="`) {
		t.Fatal("the landing page carries a script or an inline style")
	}

	board.service.UpdateBoard(context.Background(), func(stored *scoreboard.Board) error {
		stored.ShowInvite = false
		stored.LandingMessage = "Lift with us"
		return nil
	})
	hidden := visitor.get("/")
	expect(t, hidden, http.StatusOK, "Lift with us")
	if strings.Contains(hidden.body, current.InviteCode) {
		t.Fatal("the landing page showed an invite the admin hid")
	}
	expect(t, visitor.get("/join/"+current.InviteCode), http.StatusOK, "<svg")
	expect(t, visitor.get("/join/OldInviteOldInvite0000"), http.StatusNotFound, "no longer works")
}

// TestDefaultMessageNamesTheBoardOnce says FitHalo Scoreboard once however the board is named.
func TestDefaultMessageNamesTheBoardOnce(t *testing.T) {
	board := newSite(t)
	visitor := board.newBrowser()
	for _, want := range []struct{ name, language, message string }{
		{"", "en", "Join the FitHalo Scoreboard!"},
		{"FitHalo Scoreboard", "en", "Join the FitHalo Scoreboard!"},
		{"fithalo-scoreboard", "de", "Tritt dem FitHalo-Scoreboard bei!"},
		{"Morning Crew FitHalo Scoreboard", "en", "Join the Morning Crew FitHalo Scoreboard!"},
		{"Morning Crew FitHalo-Scoreboard", "de", "Tritt Morning Crew FitHalo-Scoreboard bei!"},
		{"Morning Crew", "en", "Join the Morning Crew FitHalo Scoreboard!"},
	} {
		board.service.UpdateBoard(context.Background(), func(stored *scoreboard.Board) error {
			stored.Name = want.name
			return nil
		})
		landing := visitor.request("GET", "/", nil, http.Header{"Accept-Language": {want.language}})
		if !strings.Contains(landing.body, "<h1 class=\"display\">"+want.message+"</h1>") {
			t.Errorf("a board named %q in %s does not say %q", want.name, want.language, want.message)
		}
	}
}

// TestTheFontIsServed serves Archivo labelled as a font, since with nosniff a browser needs that.
func TestTheFontIsServed(t *testing.T) {
	board := newSite(t)
	font := board.newBrowser().get("/static/fonts/Archivo.woff2")
	if font.status != http.StatusOK || font.header.Get("Content-Type") != "font/woff2" || len(font.body) < 100000 {
		t.Fatalf("the font came back %d as %q, %d bytes", font.status, font.header.Get("Content-Type"), len(font.body))
	}
	style := board.newBrowser().get("/static/style.css")
	if !strings.Contains(style.body, `url("fonts/Archivo.woff2")`) {
		t.Fatal("the stylesheet does not name the font the board serves")
	}
	if font.header.Get("Cache-Control") != "no-cache" || font.header.Get("ETag") == "" {
		t.Fatal("the font, which the stylesheet names without a version, is not revalidated")
	}
	unchanged := board.newBrowser().request("GET", "/static/fonts/Archivo.woff2", nil, http.Header{"If-None-Match": {font.header.Get("ETag")}})
	if unchanged.status != http.StatusNotModified {
		t.Fatalf("revalidating an unchanged font answered %d", unchanged.status)
	}
}

// TestSetupExplainsItsFields offers help for the setup code and the board name, and the password checks.
func TestSetupExplainsItsFields(t *testing.T) {
	board := newSite(t)
	expect(t, board.newBrowser().get("/admin/setup"), http.StatusOK,
		"popovertarget=\"setup-code-help\"", "popovertarget=\"board-name-help\"", "data-password-check",
		"class=\"callout\"", "https://haveibeenpwned.com/Passwords")
}

// TestBreachedPasswordKeepsToAccount keeps an admin with a breached password to Account until it changes.
func TestBreachedPasswordKeepsToAccount(t *testing.T) {
	board := newSite(t)
	board.signedInBrowser()
	board.admin.Breached = func(_ context.Context, candidate string) (bool, error) {
		return candidate == "a long password", nil
	}
	visitor := board.newBrowser()
	signedIn := visitor.post("/admin/sign-in", url.Values{"username": {"avery"}, "password": {"a long password"}})
	expect(t, signedIn, http.StatusOK, "Your password has appeared in a data breach.")
	expect(t, visitor.get("/admin/members"), http.StatusOK, "Your password has appeared in a data breach.")

	changed := visitor.post("/admin/account/password", url.Values{
		"csrf": {visitor.csrf("/admin/account")}, "current": {"a long password"},
		"password": {"a fresh password"}, "confirm": {"a fresh password"},
	})
	if strings.Contains(changed.body, "Your password has appeared in a data breach.") {
		t.Fatal("the warning outlived the new password")
	}
	if members := visitor.get("/admin/members"); strings.Contains(members.body, "Change the password") {
		t.Fatal("the members page is still out of reach")
	}
	if strings.Contains(board.newBrowser().get("/admin/sign-in").body, "maxlength=\"256\"") {
		t.Fatal("the sign-in field still stops at 256 characters")
	}
}

// TestAssetsAreVersioned links static files by content hash and caches only that version for good.
func TestAssetsAreVersioned(t *testing.T) {
	board := newSite(t)
	landing := board.newBrowser().get("/")
	match := regexp.MustCompile(`href="(/static/style\.css\?v=[0-9a-f]{10})"`).FindStringSubmatch(landing.body)
	if match == nil {
		t.Fatal("the page links the stylesheet without a version")
	}
	style := board.newBrowser().get(match[1])
	if style.status != http.StatusOK || !strings.Contains(style.header.Get("Cache-Control"), "immutable") {
		t.Fatalf("the versioned stylesheet is cached as %q", style.header.Get("Cache-Control"))
	}
	if stale := board.newBrowser().get("/static/style.css?v=0000000000"); strings.Contains(stale.header.Get("Cache-Control"), "immutable") {
		t.Fatal("an old version's address was cached for good")
	}
}

// TestFormsNeedTheBoardsOrigin refuses a post from another site, and one without the session's token.
func TestFormsNeedTheBoardsOrigin(t *testing.T) {
	board := newSite(t)
	visitor := board.signedInBrowser()
	token := visitor.csrf("/admin/board")
	form := url.Values{"csrf": {token}, "name": {"Hijacked"}}

	foreign := visitor.request("POST", "/admin/board", strings.NewReader(form.Encode()), http.Header{
		"Content-Type": {"application/x-www-form-urlencoded"}, "Origin": {"https://evil.example"},
	})
	expect(t, foreign, http.StatusForbidden)
	form.Set("csrf", "not the token")
	expect(t, visitor.post("/admin/board", form), http.StatusForbidden)
	if stored, _ := board.service.Board(context.Background()); stored.Name == "Hijacked" {
		t.Fatal("a forged form changed the board")
	}
	form.Set("csrf", token)
	expect(t, visitor.post("/admin/board", form), http.StatusOK, "Saved.")
}

// TestSignInAndOut covers the password, a wrong one, and signing out.
func TestSignInAndOut(t *testing.T) {
	board := newSite(t)
	first := board.signedInBrowser()
	token := first.csrf("/admin/board")
	expect(t, first.post("/admin/sign-out", url.Values{"csrf": {token}}), http.StatusOK, "Sign in")
	if result := first.get("/admin/board"); !strings.Contains(result.body, "Sign in with a passkey") {
		t.Fatal("the board page opened after signing out")
	}

	second := board.newBrowser()
	expect(t, second.post("/admin/sign-in", url.Values{"username": {"avery"}, "password": {"wrong password"}}), http.StatusUnauthorized, "That username or password")
	expect(t, second.post("/admin/sign-in", url.Values{"username": {"avery"}, "password": {"a long password"}}), http.StatusOK, "Morning Crew")
	expect(t, second.post("/admin/setup", url.Values{"code": {testSetupCode}, "boardName": {"Again"}, "username": {"mallory"}, "password": {"another password"}, "confirm": {"another password"}}),
		http.StatusOK)
	if account, _ := board.admin.Account(context.Background()); account.Username != "avery" {
		t.Fatal("a second setup replaced the admin")
	}
}

// TestMemberLimitBanner warns on every admin page once the board holds MaximumMembers, and only then.
func TestMemberLimitBanner(t *testing.T) {
	board := newSite(t)
	visitor := board.signedInBrowser()
	banner := "reached its limit of 1,000 members"
	if result := visitor.get("/admin/board"); strings.Contains(result.body, banner) {
		t.Fatal("an empty board showed the member limit banner")
	}
	ctx := context.Background()
	for index := range scoreboard.MaximumMembers {
		id := fmt.Sprintf("MemberMemberMember%04d", index)
		encoded, _ := json.Marshal(scoreboard.Member{ID: id, DisplayName: "Member", Status: scoreboard.StatusActive})
		board.service.Store.Put(ctx, "members", id, encoded, 0)
	}
	for _, path := range []string{"/admin/board", "/admin/members", "/admin/rules", "/admin/account"} {
		expect(t, visitor.get(path), http.StatusOK, banner)
	}
}

// TestManagingMembers approves, deletes, blocks and unblocks through the portal.
func TestManagingMembers(t *testing.T) {
	board := newSite(t)
	visitor := board.signedInBrowser()
	ctx := context.Background()
	ids := []string{"MemberMemberMemberMe01", "MemberMemberMemberMe02"}
	for index, id := range ids {
		member := scoreboard.Member{ID: id, DisplayName: []string{"Blake", "Casey"}[index], Status: scoreboard.StatusPending, Sharing: sharing.Lowest}
		encoded, _ := json.Marshal(member)
		board.service.Store.Put(ctx, "members", id, encoded, 0)
	}
	expect(t, visitor.get("/admin/members"), http.StatusOK, "Waiting for approval", "Blake", "Casey")
	token := visitor.csrf("/admin/members")

	expect(t, visitor.post("/admin/members/"+ids[0]+"/approve", url.Values{"csrf": {token}}), http.StatusOK, "Approved.")
	if member, _ := board.service.Member(ctx, ids[0]); member.Status != scoreboard.StatusActive {
		t.Fatal("approving did not activate the member")
	}
	expect(t, visitor.post("/admin/members/"+ids[1]+"/block", url.Values{"csrf": {token}}), http.StatusOK, "Blocked.", "Member "+ids[1][:6])
	if blocked, _ := board.service.Blocked(ctx); len(blocked) != 1 {
		t.Fatal("blocking did not block")
	}
	expect(t, visitor.post("/admin/blocked/"+ids[1]+"/unblock", url.Values{"csrf": {token}}), http.StatusOK, "Unblocked.")
}

// TestAllowingAMemberToApprove gives and takes back the permission with the Details page's checkbox, which only an active
// member has.
func TestAllowingAMemberToApprove(t *testing.T) {
	board := newSite(t)
	visitor := board.signedInBrowser()
	ctx := context.Background()
	id := "MemberMemberMemberMe01"
	encoded, _ := json.Marshal(scoreboard.Member{ID: id, DisplayName: "Blake", Status: scoreboard.StatusActive, Sharing: sharing.Lowest})
	board.service.Store.Put(ctx, "members", id, encoded, 0)
	path := "/admin/members/" + id + "/details"
	unchecked := `name="canApproveMembers" value="on">`
	checked := `name="canApproveMembers" value="on" checked>`
	expect(t, visitor.get(path), http.StatusOK, "Can Approve Members", unchecked)
	token := visitor.csrf(path)

	expect(t, visitor.post("/admin/members/"+id+"/can-approve", url.Values{"csrf": {token}, "canApproveMembers": {"on"}}), http.StatusOK, "Saved.", checked)
	if member, _ := board.service.Member(ctx, id); !member.CanApproveMembers {
		t.Fatal("checking the box did not give the permission")
	}
	expect(t, visitor.post("/admin/members/"+id+"/can-approve", url.Values{"csrf": {token}}), http.StatusOK, "Saved.", unchecked)
	if member, _ := board.service.Member(ctx, id); member.CanApproveMembers {
		t.Fatal("clearing the box did not take the permission back")
	}
	expect(t, visitor.post("/admin/members/MemberMemberMemberMe02/can-approve", url.Values{"csrf": {token}}), http.StatusNotFound)

	const pendingID = "MemberMemberMemberMe03"
	encoded, _ = json.Marshal(scoreboard.Member{ID: pendingID, DisplayName: "Casey", Status: scoreboard.StatusPending, Sharing: sharing.Lowest})
	board.service.Store.Put(ctx, "members", pendingID, encoded, 0)
	if result := visitor.get("/admin/members/" + pendingID + "/details"); result.status != http.StatusOK || strings.Contains(result.body, "canApproveMembers") {
		t.Fatalf("the details page of a waiting member: %d, or it offers the permission", result.status)
	}
}

// TestLoweringTheMaximumEnforcesIt strips stored weekly challenges when the admin lowers what members may share.
func TestLoweringTheMaximumEnforcesIt(t *testing.T) {
	board := newSite(t)
	visitor := board.signedInBrowser()
	ctx := context.Background()
	member := scoreboard.Member{ID: "MemberMemberMemberMe01", DisplayName: "Drew", Status: scoreboard.StatusActive, Sharing: sharing.Highest}
	encoded, _ := json.Marshal(member)
	board.service.Store.Put(ctx, "members", member.ID, encoded, 0)
	challenge := `{"progress":0.5,"score":250,"goal":500,"breakdown":{"exercise":250,"dailyBonus":0,"varietyBonus":0}}`
	if _, err := board.service.PutWeeklyChallenge(ctx, member, "2026-10-19", []byte(challenge)); err != nil {
		t.Fatal(err)
	}

	token := visitor.csrf("/admin/rules")
	expect(t, visitor.post("/admin/rules", url.Values{
		"csrf": {token}, "joinMode": {"approval"}, "score": {"progress"}, "activity": {"none"},
		"ranking": {"name"}, "retentionWeeks": {"12"}, "maximumPending": {"10"}, "weekStartDay": {"monday"},
	}), http.StatusOK, "Saved.")
	stored, _ := board.service.Store.Get(ctx, "weeks/2026-10-19", member.ID)
	if strings.Contains(string(stored.Data), "score") || !strings.Contains(string(stored.Data), "progress") {
		t.Fatalf("after lowering the maximum the weekly challenge is %s", stored.Data)
	}
}

// TestChangingTheWeekStartErasesHistory erases weekly challenges and routes only when the start day changes.
func TestChangingTheWeekStartErasesHistory(t *testing.T) {
	if board, _ := scoreboard.DefaultBoard(); board.WeekStartDay != "sunday" {
		t.Fatalf("a new board starts on %s", board.WeekStartDay)
	}
	board := newSite(t)
	visitor := board.signedInBrowser()
	ctx := context.Background()
	member := scoreboard.Member{ID: "MemberMemberMemberMe01", DisplayName: "Drew", Status: scoreboard.StatusActive, Sharing: sharing.Highest}
	encoded, _ := json.Marshal(member)
	board.service.Store.Put(ctx, "members", member.ID, encoded, 0)
	challenge := `{"progress":0.5,"activities":[{"id":"ActivityActivityActi01","type":"outdoorRunning","day":"2026-10-20","points":{"base":10,"bonus":0},"hasRoute":true}]}`
	if _, err := board.service.PutWeeklyChallenge(ctx, member, "2026-10-19", []byte(challenge)); err != nil {
		t.Fatal(err)
	}
	if err := board.service.PutRoute(ctx, member, "2026-10-19", "ActivityActivityActi01", []byte(`{"polyline":"_p~iF~ps|U_ulLnnqC"}`)); err != nil {
		t.Fatal(err)
	}
	save := func(day string) {
		expect(t, visitor.post("/admin/rules", url.Values{
			"csrf": {visitor.csrf("/admin/rules")}, "joinMode": {"approval"}, "score": {"details"}, "activity": {"full"},
			"ranking": {"name"}, "retentionWeeks": {"52"}, "maximumPending": {"10"}, "weekStartDay": {day},
		}), http.StatusOK, "Saved.")
	}

	save("monday")
	if collections, _ := board.service.Store.Collections(ctx, "weeks/"); len(collections) != 1 {
		t.Fatalf("saving the same start day erased the weekly challenges: %v", collections)
	}
	save("sunday")
	challenges, _ := board.service.Store.Collections(ctx, "weeks/")
	routes, _ := board.service.Store.Collections(ctx, "routes/"+member.ID+"/")
	if len(challenges) != 0 || len(routes) != 0 {
		t.Fatalf("after changing the start day the store still holds weekly challenges %v and routes %v", challenges, routes)
	}
}

// TestRulesWithoutActivities saves rules lacking the disabled activity choice, and checks the page.
func TestRulesWithoutActivities(t *testing.T) {
	board := newSite(t)
	visitor := board.signedInBrowser()
	page := visitor.get("/admin/rules")
	expect(t, page, http.StatusOK, "sharing.js", "warnings.js", "data-sharing-options", `value="name" checked`)
	expect(t, visitor.post("/admin/rules", url.Values{
		"csrf": {visitor.csrf("/admin/rules")}, "joinMode": {"approval"}, "score": {"score"},
		"ranking": {"name"}, "retentionWeeks": {"52"}, "maximumPending": {"10"}, "weekStartDay": {"monday"},
	}), http.StatusOK, "Saved.")
	saved, _ := board.service.Board(context.Background())
	if saved.MaximumSharing.Activity != sharing.ActivityNone || saved.MaximumSharing.Score != sharing.ScoreScore {
		t.Fatalf("the maximum was saved as %+v", saved.MaximumSharing)
	}
}

// TestPendingLimitOnThePortal holds the limit to 1 through 999, and the landing page says when it is met.
func TestPendingLimitOnThePortal(t *testing.T) {
	board := newSite(t)
	visitor := board.signedInBrowser()
	token := visitor.csrf("/admin/rules")
	rules := url.Values{"csrf": {token}, "joinMode": {"approval"}, "score": {"details"}, "activity": {"full"},
		"ranking": {"progress"}, "retentionWeeks": {"12"}, "weekStartDay": {"monday"}}
	for _, refused := range []string{"0", "1000", "-1", "many"} {
		rules.Set("maximumPending", refused)
		expect(t, visitor.post("/admin/rules", rules), http.StatusBadRequest, "Allow between 1 and 999")
	}
	rules.Set("maximumPending", "999")
	expect(t, visitor.post("/admin/rules", rules), http.StatusOK, "Saved.")
	rules.Set("maximumPending", "1")
	expect(t, visitor.post("/admin/rules", rules), http.StatusOK, "Saved.")
	if stored, _ := board.service.Board(context.Background()); stored.PendingLimit() != 1 {
		t.Fatalf("the limit was stored as %d", stored.PendingLimit())
	}

	member := scoreboard.Member{ID: "MemberMemberMemberMe01", DisplayName: "Waiting", Status: scoreboard.StatusPending, Sharing: sharing.Lowest}
	encoded, _ := json.Marshal(member)
	board.service.Store.Put(context.Background(), "members", member.ID, encoded, 0)
	full := visitor.get("/")
	expect(t, full, http.StatusOK, "taking new requests")
	if strings.Contains(full.body, "class=\"qr\"") {
		t.Fatal("a full board still showed its invite")
	}
	expect(t, visitor.get("/admin/members"), http.StatusOK, "Waiting: 1 of 1")
}

// TestPasskeys registers a passkey and signs in with it, using a software authenticator.
func TestPasskeys(t *testing.T) {
	board := newSite(t)
	visitor := board.signedInBrowser()
	token := visitor.csrf("/admin/account")
	jsonHeader := http.Header{"Content-Type": {"application/json"}, "X-CSRF-Token": {token}}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	party := board.web.relyingParty()
	credentialID := []byte("software passkey credential")

	var options struct {
		Challenge string `json:"challenge"`
		RP        struct {
			ID string `json:"id"`
		} `json:"rp"`
	}
	begin := visitor.request("POST", "/admin/account/passkeys/begin", strings.NewReader("{}"), jsonHeader)
	if err := json.Unmarshal([]byte(begin.body), &options); err != nil || options.RP.ID != party.ID {
		t.Fatalf("registration options %s", begin.body)
	}
	challenge, _ := base64.RawURLEncoding.DecodeString(options.Challenge)
	clientData := clientDataJSON("webauthn.create", challenge, party.Origin)
	attestation := attestationObject(party.ID, credentialID, key)
	finish := visitor.request("POST", "/admin/account/passkeys/finish", jsonBody(map[string]string{
		"clientDataJSON": encode(clientData), "attestationObject": encode(attestation), "name": "Test key",
	}), jsonHeader)
	if finish.status != http.StatusOK || !strings.Contains(finish.body, "passkeyAdded") {
		t.Fatalf("registration finished with %d: %s", finish.status, finish.body)
	}

	stranger := board.newBrowser()
	begin = stranger.request("POST", "/admin/sign-in/passkey/begin", strings.NewReader("{}"), http.Header{"Content-Type": {"application/json"}})
	json.Unmarshal([]byte(begin.body), &options)
	challenge, _ = base64.RawURLEncoding.DecodeString(options.Challenge)
	clientData = clientDataJSON("webauthn.get", challenge, party.Origin)
	rpIDHash := sha256.Sum256([]byte(party.ID))
	authenticatorData := binary.BigEndian.AppendUint32(append(rpIDHash[:], 0x05), 1)
	clientDigest := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte(nil), authenticatorData...), clientDigest[:]...))
	signature, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
	signedIn := stranger.request("POST", "/admin/sign-in/passkey/finish", jsonBody(map[string]string{
		"id": encode(credentialID), "clientDataJSON": encode(clientData),
		"authenticatorData": encode(authenticatorData), "signature": encode(signature),
	}), http.Header{"Content-Type": {"application/json"}})
	if signedIn.status != http.StatusOK || !strings.Contains(signedIn.body, "/admin/board") {
		t.Fatalf("passkey sign-in finished with %d: %s", signedIn.status, signedIn.body)
	}
	expect(t, stranger.get("/admin/account"), http.StatusOK, "Test key")

	replay := board.newBrowser().request("POST", "/admin/sign-in/passkey/finish", jsonBody(map[string]string{
		"id": encode(credentialID), "clientDataJSON": encode(clientData),
		"authenticatorData": encode(authenticatorData), "signature": encode(signature),
	}), http.Header{"Content-Type": {"application/json"}})
	if replay.status == http.StatusOK {
		t.Fatal("a replayed passkey sign-in worked")
	}
}

// encode returns data in unpadded base64url.
func encode(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// jsonBody returns value encoded as a JSON body.
func jsonBody(value any) io.Reader {
	encoded, _ := json.Marshal(value)
	return bytes.NewReader(encoded)
}

// clientDataJSON returns the client data a browser signs for a ceremony of kind.
func clientDataJSON(kind string, challenge []byte, origin string) []byte {
	encoded, _ := json.Marshal(map[string]any{"type": kind, "challenge": encode(challenge), "origin": origin})
	return encoded
}

// attestationObject hand-builds the CBOR a new passkey sends: {"fmt": "none", "attStmt": {}, "authData": ...}.
func attestationObject(rpID string, credentialID []byte, key *ecdsa.PrivateKey) []byte {
	x, y := make([]byte, 32), make([]byte, 32)
	key.X.FillBytes(x)
	key.Y.FillBytes(y)
	coseKey := []byte{0xA5, 0x01, 0x02, 0x03, 0x26, 0x20, 0x01, 0x21, 0x58, 0x20}
	coseKey = append(coseKey, x...)
	coseKey = append(coseKey, 0x22, 0x58, 0x20)
	coseKey = append(coseKey, y...)

	rpIDHash := sha256.Sum256([]byte(rpID))
	authenticatorData := append(rpIDHash[:], 0x45)
	authenticatorData = binary.BigEndian.AppendUint32(authenticatorData, 0)
	authenticatorData = append(authenticatorData, make([]byte, 16)...)
	authenticatorData = binary.BigEndian.AppendUint16(authenticatorData, uint16(len(credentialID)))
	authenticatorData = append(authenticatorData, credentialID...)
	authenticatorData = append(authenticatorData, coseKey...)

	object := []byte{0xA3, 0x63, 'f', 'm', 't', 0x64, 'n', 'o', 'n', 'e', 0x67, 'a', 't', 't', 'S', 't', 'm', 't', 0xA0, 0x68, 'a', 'u', 't', 'h', 'D', 'a', 't', 'a', 0x59}
	object = binary.BigEndian.AppendUint16(object, uint16(len(authenticatorData)))
	return append(object, authenticatorData...)
}

// TestPasswordOffAnswersAlike shows a wrong and a right password the same page while the password is off.
func TestPasswordOffAnswersAlike(t *testing.T) {
	board := newSite(t)
	ctx := context.Background()
	if err := board.admin.Setup(ctx, testSetupCode, "avery", "a long password"); err != nil {
		t.Fatal(err)
	}
	board.admin.AddPasskey(ctx, admin.Passkey{ID: "phone", Name: "Phone"})
	if err := board.admin.SetPasswordEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	right := board.newBrowser().post("/admin/sign-in", url.Values{"username": {"avery"}, "password": {"a long password"}})
	wrong := board.newBrowser().post("/admin/sign-in", url.Values{"username": {"avery"}, "password": {"wrong password"}})
	if right.status != wrong.status || right.body != wrong.body {
		t.Fatalf("a right password answered %d and a wrong one %d, with different pages", right.status, wrong.status)
	}
}

// TestPasswordChangeKeepsOnlyThisBrowser signs out every other browser when the password changes.
func TestPasswordChangeKeepsOnlyThisBrowser(t *testing.T) {
	board := newSite(t)
	first := board.signedInBrowser()
	second := board.newBrowser()
	expect(t, second.post("/admin/sign-in", url.Values{"username": {"avery"}, "password": {"a long password"}}), http.StatusOK, "Morning Crew")

	expect(t, first.post("/admin/account/password", url.Values{
		"csrf": {first.csrf("/admin/account")}, "current": {"a long password"},
		"password": {"a fresh password"}, "confirm": {"a fresh password"},
	}), http.StatusOK)
	expect(t, first.get("/admin/board"), http.StatusOK, "Morning Crew")
	if result := second.get("/admin/board"); !strings.Contains(result.body, "Sign in with a passkey") {
		t.Fatal("another browser stayed signed in after the password changed")
	}
}

// TestGlobalSignInLimit refuses password attempts beyond the global limit from any address, and leaves recovery open.
func TestGlobalSignInLimit(t *testing.T) {
	board := newConfiguredSite(t, func(configured *Site) {
		configured.SignInLimiter = ratelimit.New(10, 10)
		configured.GlobalSignInLimiter = ratelimit.New(2, 1)
		configured.ClientAddress = func(request *http.Request) string { return request.Header.Get("X-Test-Address") }
	})
	if err := board.admin.Setup(context.Background(), testSetupCode, "avery", "a long password"); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"username": {"avery"}, "password": {"wrong password"}}
	for index, want := range []int{http.StatusUnauthorized, http.StatusUnauthorized, http.StatusTooManyRequests} {
		header := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "X-Test-Address": {"198.51.100." + strconv.Itoa(index+1)}}
		if got := board.newBrowser().request("POST", "/admin/sign-in", strings.NewReader(form.Encode()), header); got.status != want {
			t.Fatalf("attempt %d answered %d, want %d", index+1, got.status, want)
		}
	}
	recovery := url.Values{"code": {"not the code"}, "password": {"a new long password"}, "confirm": {"a new long password"}}
	header := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "X-Test-Address": {"198.51.100.9"}}
	if got := board.newBrowser().request("POST", "/admin/recover", strings.NewReader(recovery.Encode()), header); got.status != http.StatusBadRequest {
		t.Fatalf("recovery during a sign-in flood answered %d, want %d", got.status, http.StatusBadRequest)
	}
	if newSite(t).web.GlobalSignInLimiter == nil {
		t.Fatal("a site without a global limit has none")
	}
}

// TestHeaderTokenKeepsTheBodyLimit refuses an oversize form even when the CSRF token arrives as a header.
func TestHeaderTokenKeepsTheBodyLimit(t *testing.T) {
	board := newSite(t)
	visitor := board.signedInBrowser()
	form := url.Values{"name": {"Oversize"}, "padding": {strings.Repeat("a", boardFormLimit)}}
	result := visitor.request("POST", "/admin/board", strings.NewReader(form.Encode()), http.Header{
		"Content-Type": {"application/x-www-form-urlencoded"}, "X-CSRF-Token": {visitor.csrf("/admin/board")},
	})
	if stored, _ := board.service.Board(context.Background()); stored.Name == "Oversize" || result.status == http.StatusOK {
		t.Fatalf("an oversize form answered %d and saved the name %q", result.status, stored.Name)
	}
}

// TestHeaderTokenOversizeUploadIsTooLarge answers an oversize multipart post with its token in the header as 413.
func TestHeaderTokenOversizeUploadIsTooLarge(t *testing.T) {
	board := newSite(t)
	visitor := board.signedInBrowser()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	writer.WriteField("name", "Oversize")
	part, _ := writer.CreateFormFile("avatar", "avatar.png")
	part.Write(make([]byte, boardFormLimit))
	writer.Close()
	result := visitor.request("POST", "/admin/board", &body, http.Header{
		"Content-Type": {writer.FormDataContentType()}, "X-CSRF-Token": {visitor.csrf("/admin/board")},
	})
	if result.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversize upload with a header token answered %d, want %d", result.status, http.StatusRequestEntityTooLarge)
	}
}

// postDetails submits the Board page's details as a multipart form, with image as the avatar when set.
func (b *browser) postDetails(fields map[string]string, image []byte) page {
	b.t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, value := range fields {
		writer.WriteField(name, value)
	}
	part, _ := writer.CreateFormFile("avatar", "avatar.png")
	part.Write(image)
	writer.Close()
	return b.request("POST", "/admin/board", &body, http.Header{"Content-Type": {writer.FormDataContentType()}})
}

// TestBoardAvatarInDetails saves, keeps and removes the board's avatar with the rest of its details.
func TestBoardAvatarInDetails(t *testing.T) {
	board := newSite(t)
	visitor := board.signedInBrowser()
	hasAvatar := func() bool {
		stored, _ := board.service.Board(context.Background())
		return stored.HasAvatar
	}
	fields := map[string]string{"csrf": visitor.csrf("/admin/board"), "name": "Avatar Crew"}

	expect(t, visitor.postDetails(fields, []byte("not an image")), http.StatusBadRequest, "Use a JPEG or PNG of up to 2048 pixels")
	if stored, _ := board.service.Board(context.Background()); stored.HasAvatar || stored.Name == "Avatar Crew" {
		t.Fatal("a bad image saved the details")
	}
	var encoded bytes.Buffer
	png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 8, 8)))
	expect(t, visitor.postDetails(fields, encoded.Bytes()), http.StatusOK, "Saved.", "/api/v1/board/avatar", `name="removeAvatar"`)
	if stored, _ := board.service.Board(context.Background()); !stored.HasAvatar || stored.Name != "Avatar Crew" {
		t.Fatal("the avatar and name were not saved together")
	}
	expect(t, visitor.postDetails(fields, nil), http.StatusOK, "Saved.")
	if !hasAvatar() {
		t.Fatal("saving without a new image removed the avatar")
	}
	fields["removeAvatar"] = "on"
	expect(t, visitor.postDetails(fields, nil), http.StatusOK, "Saved.")
	if hasAvatar() {
		t.Fatal("removing the avatar kept it")
	}
	expect(t, visitor.postDetails(fields, make([]byte, boardFormLimit)), http.StatusRequestEntityTooLarge, "Choose an image under 8 MB")
}

// TestPasskeyEndpointsNeedTheToken refuses the JSON passkey registration calls without X-CSRF-Token.
func TestPasskeyEndpointsNeedTheToken(t *testing.T) {
	board := newSite(t)
	visitor := board.signedInBrowser()
	for _, path := range []string{"/admin/account/passkeys/begin", "/admin/account/passkeys/finish"} {
		result := visitor.request("POST", path, strings.NewReader("{}"), http.Header{"Content-Type": {"application/json"}})
		if result.status != http.StatusForbidden {
			t.Errorf("%s without a token answered %d, want %d", path, result.status, http.StatusForbidden)
		}
	}
}

// httpsSite returns a handler for board as if served at https://board.example, for headers and cookies.
func httpsSite(t *testing.T, board *site) http.Handler {
	t.Helper()
	publicURL, _ := url.Parse("https://board.example/")
	secure := &Site{Service: board.service, Admin: board.admin, PublicURL: publicURL}
	handler, err := secure.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

// serve sends a request from https://board.example's own pages to handler and returns the answer.
func serve(handler http.Handler, method, path string, form url.Values) *http.Response {
	request := httptest.NewRequest(method, "https://board.example"+path, strings.NewReader(form.Encode()))
	request.Header.Set("Origin", "https://board.example")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

// TestSecurityHeaders sets every page's security headers, with HSTS over https only.
func TestSecurityHeaders(t *testing.T) {
	board := newSite(t)
	plain := board.newBrowser().get("/").header
	secure := serve(httpsSite(t, board), "GET", "/", nil).Header
	for name, want := range map[string]string{
		"X-Content-Type-Options":     "nosniff",
		"X-Frame-Options":            "DENY",
		"Referrer-Policy":            "same-origin",
		"Cross-Origin-Opener-Policy": "same-origin",
		"Permissions-Policy":         permissionsPolicy,
		"Content-Security-Policy":    contentSecurity,
	} {
		if plain.Get(name) != want || secure.Get(name) != want {
			t.Errorf("%s is %q over http and %q over https, want %q", name, plain.Get(name), secure.Get(name), want)
		}
	}
	if !strings.Contains(plain.Get("Permissions-Policy"), "publickey-credentials-get=(self)") {
		t.Error("the permissions policy does not leave passkeys to the board")
	}
	if got := secure.Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Errorf("Strict-Transport-Security is %q over https", got)
	}
	if got := plain.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security is %q over http", got)
	}
}

// TestCookieAttributes keeps the session cookie HttpOnly, Secure, strict and on the admin paths.
func TestCookieAttributes(t *testing.T) {
	board := newSite(t)
	if err := board.admin.Setup(context.Background(), testSetupCode, "avery", "a long password"); err != nil {
		t.Fatal(err)
	}
	handler := httpsSite(t, board)
	cookies := map[string]*http.Cookie{}
	for _, response := range []*http.Response{
		serve(handler, "POST", "/admin/sign-in", url.Values{"username": {"avery"}, "password": {"a long password"}}),
		serve(handler, "POST", "/language", url.Values{"language": {"de"}, "return": {"/"}}),
		serve(handler, "POST", "/admin/sign-in/passkey/begin", nil),
	} {
		for _, cookie := range response.Cookies() {
			cookies[cookie.Name] = cookie
		}
	}
	for name, want := range map[string]struct {
		path     string
		sameSite http.SameSite
		maxAge   int
	}{
		sessionCookie:  {"/admin", http.SameSiteStrictMode, int(admin.SessionLifetime.Seconds())},
		ceremonyCookie: {"/admin", http.SameSiteStrictMode, int(admin.ChallengeLifetime.Seconds())},
		languageCookie: {"/", http.SameSiteLaxMode, languageCookieAge},
	} {
		cookie := cookies[name]
		if cookie == nil {
			t.Errorf("%s was not set", name)
			continue
		}
		if !cookie.HttpOnly || !cookie.Secure || cookie.Path != want.path || cookie.SameSite != want.sameSite ||
			cookie.MaxAge != want.maxAge {
			t.Errorf("%s is %+v, want HttpOnly, Secure, path %s, SameSite %d, MaxAge %d",
				name, cookie, want.path, want.sameSite, want.maxAge)
		}
	}
}

// TestMemberDetails shows a member's stored weekly challenges and routes against the limit for one, in every language,
// and answers 404 for anyone else.
func TestMemberDetails(t *testing.T) {
	board := newSite(t)
	visitor := board.signedInBrowser()
	ctx := context.Background()
	const id = "MemberMemberMemberMe01"
	member := scoreboard.Member{ID: id, DisplayName: "Blake", Status: scoreboard.StatusActive, Sharing: sharing.Highest}
	encoded, _ := json.Marshal(member)
	board.service.Store.Put(ctx, "members", id, encoded, 0)
	board.service.Store.Put(ctx, "weeks/2026-10-19", id, bytes.Repeat([]byte("w"), 3000), store.AnyVersion)
	board.service.Store.Put(ctx, "routes/"+id+"/2026-10-19", "RouteRouteRouteRoute01", bytes.Repeat([]byte("r"), 2000), store.AnyVersion)

	expect(t, visitor.get("/admin/members"), http.StatusOK, "/admin/members/"+id+"/details")
	expect(t, visitor.get("/admin/members/"+id+"/details"), http.StatusOK, "Data stored for Blake", "Week of 2026-10-19",
		"Challenge data: 3 KB · Routes: 1 (2 KB)", "1% of the limit for one weekly challenge", "Total: 5 KB · Weekly challenges stored: 1", "No avatar")
	for _, language := range Languages {
		result := visitor.request("GET", "/admin/members/"+id+"/details", nil, http.Header{"Accept-Language": {language}})
		if result.status != http.StatusOK || regexp.MustCompile(`>[A-Z][a-z]+_[A-Za-z_]+<`).MatchString(result.body) {
			t.Fatalf("the details page in %s: %d, or it shows a string key", language, result.status)
		}
	}
	expect(t, visitor.get("/admin/members/MemberMemberMemberMe02/details"), http.StatusNotFound)
	expect(t, visitor.get("/admin/members/not-an-id/details"), http.StatusNotFound)
}

// TestNewerStorageRefusesPages fails every page but static files once a newer server has laid out the store.
func TestNewerStorageRefusesPages(t *testing.T) {
	board := newSite(t)
	ctx := context.Background()
	stored, err := board.service.Board(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stored.StorageVersion = scoreboard.StorageVersion + 1
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.service.Store.Put(ctx, "board", "board", encoded, store.AnyVersion); err != nil {
		t.Fatal(err)
	}
	browser := board.newBrowser()
	for _, path := range []string{"/", "/join/" + stored.InviteCode, "/admin/setup", "/admin/sign-in"} {
		if result := browser.request("GET", path, nil, nil); result.status != http.StatusInternalServerError {
			t.Errorf("GET %s: status %d, want %d", path, result.status, http.StatusInternalServerError)
		}
	}
	if result := browser.request("GET", "/static/style.css", nil, nil); result.status != http.StatusOK {
		t.Errorf("GET /static/style.css: status %d, want %d", result.status, http.StatusOK)
	}
}
