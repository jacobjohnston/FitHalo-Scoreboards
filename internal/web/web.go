// Package web serves a board's landing page and admin portal, in the FitHalo website's look and
// FitHalo's ten languages.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/admin"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/ratelimit"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/webauthn"
)

// assets holds the templates, string tables and static files.
//
//go:embed assets
var assets embed.FS

const (
	// sessionCookie holds the admin's session token.
	sessionCookie = "fithalo_admin"

	// ceremonyCookie holds the identifier of a passkey ceremony in progress.
	ceremonyCookie = "fithalo_ceremony"

	// languageCookie holds the visitor's chosen language.
	languageCookie = "fithalo_language"

	// languageCookieAge is how long, in seconds, a chosen language is remembered: a year.
	languageCookieAge = 365 * 24 * 60 * 60

	// formBodyLimit is the most bytes a posted form or JSON body may hold.
	formBodyLimit = 256 << 10

	// boardFormLimit is the most bytes the Board page's details may hold, an avatar's original included.
	boardFormLimit = 8 << 20

	// versionBytes is how many bytes of a static file's SHA-256 its version keeps.
	versionBytes = 5

	// globalSignInBurst is the most PBKDF2-backed attempts all addresses together may make at once.
	globalSignInBurst = 30

	// globalSignInPerMinute is the PBKDF2-backed attempts all addresses together may make each minute.
	globalSignInPerMinute = 60

	// globalSignInKey is GlobalSignInLimiter's bucket for password sign-in.
	globalSignInKey = "sign-in"

	// globalSetupKey is GlobalSignInLimiter's bucket for setup, so a sign-in flood cannot block it.
	globalSetupKey = "setup"

	// contentSecurity is every page's Content-Security-Policy; only the password check reaches out.
	contentSecurity = "default-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; script-src 'self'; " +
		"connect-src 'self' https://api.pwnedpasswords.com; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

	// permissionsPolicy turns off the browser features the portal never uses, and keeps passkeys to the board.
	permissionsPolicy = "camera=(), microphone=(), geolocation=(), payment=(), usb=(), " +
		"publickey-credentials-create=(self), publickey-credentials-get=(self)"
)

// fontTypes are the types Go's own table lacks; with nosniff, a font must arrive labelled as one.
var fontTypes = map[string]string{".ttf": "font/ttf", ".woff2": "font/woff2", ".txt": "text/plain; charset=utf-8"}

// pages are the templates parsed at start, each with the layout.
var pages = []string{"landing", "setup", "signin", "recover", "board", "members", "details", "rules", "account", "message"}

// Site serves the landing page and the admin portal.
type Site struct {
	// Service holds the board and its members.
	Service *scoreboard.Service

	// Admin holds the admin's account and sessions.
	Admin *admin.Admin

	// Logger receives page failures; nil logs nothing.
	Logger *slog.Logger

	// PublicURL is the board's address, with any path it is served under, and the passkeys' origin.
	PublicURL *url.URL

	// SignInLimiter limits attempts at the setup code, the password and recovery, per address.
	SignInLimiter *ratelimit.Limiter

	// GlobalSignInLimiter limits setup and password attempts from every address together, so PBKDF2's
	// total cost stays bounded; nil uses globalSignInBurst and globalSignInPerMinute.
	GlobalSignInLimiter *ratelimit.Limiter

	// ClientAddress returns the address a request came from; nil uses the connection's.
	ClientAddress func(*http.Request) string

	// localizer holds every language's strings.
	localizer *Localizer

	// templates holds each page's parsed template, by page name.
	templates map[string]*template.Template

	// static holds the files served under /static.
	static fs.FS

	// versions holds each static file's content hash, by name.
	versions map[string]string
}

// A view is everything a page's template sees.
type view struct {
	// site is the site drawing the page.
	site *Site

	// Language is the page's language code.
	Language string

	// Languages are the footer menu's choices.
	Languages []languageOption

	// Title is the document's title.
	Title string

	// BasePath is the path the board is served under, without a trailing slash.
	BasePath string

	// BoardName is the board's name, or a placeholder when it has none.
	BoardName string

	// Board is the board as stored.
	Board scoreboard.Board

	// Page is the page's template name, or empty for a message.
	Page string

	// Path is the request's path.
	Path string

	// SignedIn reports whether the admin portal's navigation shows.
	SignedIn bool

	// PageScripts are the page's own scripts, from pageScripts.
	PageScripts []string

	// CSRF is the session's token for the page's forms.
	CSRF string

	// Notice is a confirmation shown above the page.
	Notice string

	// Error is a failure shown above the page.
	Error string

	// MemberLimitReached reports whether the board is full, which every admin page warns of first.
	MemberLimitReached bool

	// Script reports whether the page loads the passkey script.
	Script bool

	// Data holds the page's own values, by name.
	Data map[string]any
}

// A languageOption is one entry in the footer's language menu.
type languageOption struct {
	// Code is the language's code.
	Code string

	// Name is the language's name in itself.
	Name string

	// Selected reports whether the page is in this language.
	Selected bool
}

// An option is one choice in a form's radio group or menu.
type option struct {
	// Value is what the form posts.
	Value string

	// Label is what the page shows.
	Label string

	// Selected reports whether the choice is the stored one.
	Selected bool
}

// T returns the page's language's text for key, with placeholders filled from name and value pairs.
func (v *view) T(key string, arguments ...any) string {
	return v.site.localizer.Text(v.Language, key, arguments...)
}

// Asset returns a static file's address with its content's hash, so a browser never uses a stale copy.
func (v *view) Asset(name string) string {
	return v.BasePath + "/static/" + name + "?v=" + v.site.versions[name]
}

// Handler returns the site's handler. The API is served separately, at basePath/api.
func (s *Site) Handler() (http.Handler, error) {
	if s.ClientAddress == nil {
		s.ClientAddress = func(request *http.Request) string {
			host, _, err := net.SplitHostPort(request.RemoteAddr)
			if err != nil {
				return request.RemoteAddr
			}
			return host
		}
	}
	if s.GlobalSignInLimiter == nil {
		s.GlobalSignInLimiter = ratelimit.New(globalSignInBurst, globalSignInPerMinute)
	}
	files, err := fs.Sub(assets, "assets")
	if err != nil {
		return nil, err
	}
	if s.localizer, err = LoadLocalizer(files); err != nil {
		return nil, err
	}
	if s.static, err = fs.Sub(files, "static"); err != nil {
		return nil, err
	}
	s.versions = map[string]string{}
	err = fs.WalkDir(s.static, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := fs.ReadFile(s.static, name)
		digest := sha256.Sum256(data)
		s.versions[name] = hex.EncodeToString(digest[:versionBytes])
		return err
	})
	if err != nil {
		return nil, err
	}
	s.templates = map[string]*template.Template{}
	for _, page := range pages {
		parsed, err := template.ParseFS(files, "templates/layout.html", "templates/password.html", "templates/"+page+".html")
		if err != nil {
			return nil, err
		}
		s.templates[page] = parsed
	}

	base := s.basePath()
	mux := http.NewServeMux()
	handle := func(pattern string, handler http.HandlerFunc) {
		method, route, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" "+base+route, s.current(handler))
	}
	handle("GET /{$}", s.landing)
	handle("GET /join/{code}", s.join)
	mux.HandleFunc("GET "+base+"/static/{file...}", s.serveStatic)
	handle("POST /language", s.setLanguage)

	handle("GET /admin", s.adminHome)
	handle("GET /admin/{$}", s.adminHome)
	handle("GET /admin/setup", s.setupPage)
	handle("POST /admin/setup", s.limited(s.globallyLimited(globalSetupKey, s.setup)))
	handle("GET /admin/sign-in", s.signInPage)
	handle("POST /admin/sign-in", s.limited(s.globallyLimited(globalSignInKey, s.signIn)))
	handle("POST /admin/sign-in/passkey/begin", s.limited(s.beginPasskeySignIn))
	handle("POST /admin/sign-in/passkey/finish", s.limited(s.finishPasskeySignIn))
	handle("GET /admin/recover", s.recoverPage)
	// Recovery checks its code before hashing, so a flood of sign-ins cannot lock the admin out of it.
	handle("POST /admin/recover", s.limited(s.recover))

	handle("POST /admin/sign-out", s.signedIn(s.signOut))
	handle("GET /admin/board", s.signedIn(s.boardPage))
	handle("POST /admin/board", s.signedInUpTo(boardFormLimit, s.saveBoard))
	handle("POST /admin/board/invite", s.signedIn(s.replaceInvite))
	handle("GET /admin/invite.svg", s.signedIn(s.inviteSVG))
	handle("GET /admin/members", s.signedIn(s.membersPage))
	handle("GET /admin/members/{id}/details", s.signedIn(s.detailsPage))
	handle("POST /admin/members/{id}/can-approve", s.signedIn(s.setCanApprove))
	handle("POST /admin/members/{id}/{action}", s.signedIn(s.memberAction))
	handle("POST /admin/blocked/{id}/unblock", s.signedIn(s.unblock))
	handle("GET /admin/rules", s.signedIn(s.rulesPage))
	handle("POST /admin/rules", s.signedIn(s.saveRules))
	handle("POST /admin/rules/password", s.signedIn(s.saveBoardPassword))
	handle("GET /admin/account", s.signedIn(s.accountPage))
	handle("POST /admin/account/password", s.signedIn(s.changePassword))
	handle("POST /admin/account/password-enabled", s.signedIn(s.setPasswordEnabled))
	handle("POST /admin/account/passkeys/begin", s.signedIn(s.beginPasskeyRegistration))
	handle("POST /admin/account/passkeys/finish", s.signedIn(s.finishPasskeyRegistration))
	handle("POST /admin/account/passkeys/{id}/rename", s.signedIn(s.renamePasskey))
	handle("POST /admin/account/passkeys/{id}/delete", s.signedIn(s.deletePasskey))
	mux.HandleFunc(base+"/", func(writer http.ResponseWriter, request *http.Request) {
		s.message(writer, request, http.StatusNotFound, "Error_NotFoundTitle", "Error_NotFound")
	})
	return s.secured(mux), nil
}

// basePath returns the path the board is served under, without a trailing slash.
func (s *Site) basePath() string {
	return strings.TrimSuffix(s.PublicURL.Path, "/")
}

// relyingParty returns the passkey relying party for the board's host and origin.
func (s *Site) relyingParty() webauthn.RelyingParty {
	return webauthn.RelyingParty{ID: s.PublicURL.Hostname(), Origin: s.PublicURL.Scheme + "://" + s.PublicURL.Host}
}

// secured sets the headers every page carries, and refuses a form posted from anywhere but the board.
func (s *Site) secured(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		header := writer.Header()
		header.Set("Content-Security-Policy", contentSecurity)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "same-origin")
		header.Set("Permissions-Policy", permissionsPolicy)
		header.Set("Cross-Origin-Opener-Policy", "same-origin")
		if s.PublicURL.Scheme == "https" {
			// No includeSubDomains: a board on a bare domain would force HTTPS on the owner's other sites.
			header.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if request.Method == http.MethodPost && !s.sameOrigin(request) {
			s.message(writer, request, http.StatusForbidden, "Error_ForbiddenTitle", "Error_Forbidden")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// sameOrigin reports whether a post came from the board's own pages, by Origin or else Referer.
func (s *Site) sameOrigin(request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		if referer, err := url.Parse(request.Header.Get("Referer")); err == nil && referer.Host != "" {
			origin = referer.Scheme + "://" + referer.Host
		}
	}
	return origin == s.relyingParty().Origin
}

// limited applies the sign-in limit, when the site has one.
func (s *Site) limited(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if s.SignInLimiter != nil {
			if allowed, _ := s.SignInLimiter.Allow(s.ClientAddress(request), s.Service.Now()); !allowed {
				s.message(writer, request, http.StatusTooManyRequests, "Error_TooManyTitle", "Error_TooMany")
				return
			}
		}
		next(writer, request)
	}
}

// globallyLimited applies GlobalSignInLimiter's bucket key, after limited has spent the address's own token.
func (s *Site) globallyLimited(key string, next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if s.GlobalSignInLimiter != nil {
			if allowed, _ := s.GlobalSignInLimiter.Allow(key, s.Service.Now()); !allowed {
				s.message(writer, request, http.StatusTooManyRequests, "Error_TooManyTitle", "Error_TooMany")
				return
			}
		}
		next(writer, request)
	}
}

// sessionKey is the context key for the signed-in request's session token.
type sessionKey struct{}

// signedIn requires a live session, and on a post its CSRF token as well.
func (s *Site) signedIn(next func(http.ResponseWriter, *http.Request, admin.Session)) http.HandlerFunc {
	return s.signedInUpTo(formBodyLimit, next)
}

// signedInUpTo is signedIn for a post of up to limit bytes, which may be a multipart form.
func (s *Site) signedInUpTo(limit int64, next func(http.ResponseWriter, *http.Request, admin.Session)) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		token := ""
		if cookie, err := request.Cookie(sessionCookie); err == nil {
			token = cookie.Value
		}
		session, err := s.Admin.Session(request.Context(), token)
		if errors.Is(err, admin.ErrSessionExpired) {
			if request.Method == http.MethodGet {
				http.Redirect(writer, request, s.basePath()+"/admin/sign-in", http.StatusSeeOther)
				return
			}
			s.message(writer, request, http.StatusForbidden, "Error_ForbiddenTitle", "Error_SignedOut")
			return
		}
		if err != nil {
			s.fail(writer, request, err)
			return
		}
		if request.Method == http.MethodPost {
			request.Body = http.MaxBytesReader(writer, request.Body, limit)
			// The form is parsed whichever way the token arrives, so an oversize upload always answers 413.
			var tooLarge *http.MaxBytesError
			if err := request.ParseMultipartForm(limit); errors.As(err, &tooLarge) {
				s.message(writer, request, http.StatusRequestEntityTooLarge, "Error_TooLargeTitle", "Error_TooLarge")
				return
			}
			given := request.Header.Get("X-CSRF-Token")
			if given == "" {
				given = request.PostFormValue("csrf")
			}
			if subtle.ConstantTimeCompare([]byte(given), []byte(session.CSRF)) != 1 {
				s.message(writer, request, http.StatusForbidden, "Error_ForbiddenTitle", "Error_Forbidden")
				return
			}
		}
		writer.Header().Set("Cache-Control", "no-store")
		if s.mustReplacePassword(request) {
			if request.Method == http.MethodGet {
				http.Redirect(writer, request, s.basePath()+"/admin/account", http.StatusSeeOther)
				return
			}
			s.message(writer, request, http.StatusForbidden, "Error_ForbiddenTitle", "Account_PasswordBreached")
			return
		}
		next(writer, request.WithContext(context.WithValue(request.Context(), sessionKey{}, token)), session)
	}
}

// sessionToken returns the signed-in request's session token, which signedIn put in its context.
func sessionToken(request *http.Request) string {
	token, _ := request.Context().Value(sessionKey{}).(string)
	return token
}

// mustReplacePassword reports whether a breached password keeps the admin to the Account page.
func (s *Site) mustReplacePassword(request *http.Request) bool {
	path := strings.TrimPrefix(request.URL.Path, s.basePath())
	if strings.HasPrefix(path, "/admin/account") || path == "/admin/sign-out" {
		return false
	}
	account, err := s.Admin.Account(request.Context())
	return err == nil && account.PasswordEnabled && account.PasswordBreached
}

// pageScripts are the static scripts each page loads beside the layout's, by page name.
var pageScripts = map[string][]string{
	"account": {"password.js"},
	"board":   {"avatar.js"},
	"recover": {"password.js"},
	"rules":   {"sharing.js", "warnings.js"},
	"setup":   {"password.js"},
}

// newView returns a page's view in the request's language, titled by titleKey and the board's name.
func (s *Site) newView(request *http.Request, page, titleKey string) *view {
	language := chooseLanguage(request)
	board, _ := s.Service.Board(request.Context())
	result := &view{
		site:     s,
		Language: language,
		BasePath: s.basePath(),
		Board:    board,
		Page:     page,
		Path:     request.URL.Path,
		Data:     map[string]any{},

		PageScripts: pageScripts[page],
	}
	for _, code := range Languages {
		result.Languages = append(result.Languages, languageOption{Code: code, Name: languageNames[code], Selected: code == language})
	}
	result.BoardName = board.Name
	if result.BoardName == "" {
		result.BoardName = result.T("Common_UnnamedBoard")
	}
	result.Title = result.T(titleKey) + " · " + result.BoardName
	return result
}

// render draws a page into a buffer first, so a template error never sends half a page.
func (s *Site) render(writer http.ResponseWriter, status int, page string, data *view) {
	var output bytes.Buffer
	if err := s.templates[page].ExecuteTemplate(&output, "layout", data); err != nil {
		if s.Logger != nil {
			s.Logger.Error("rendering a page", "page", page, "error", err)
		}
		http.Error(writer, "The page could not be drawn.", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(status)
	writer.Write(output.Bytes())
}

// message draws the message page with a heading and a message, both string keys.
func (s *Site) message(writer http.ResponseWriter, request *http.Request, status int, headingKey, messageKey string) {
	page := s.newView(request, "", headingKey)
	page.Data["Heading"] = page.T(headingKey)
	page.Data["Message"] = page.T(messageKey)
	s.render(writer, status, "message", page)
}

// fail logs err and draws the generic failure page.
func (s *Site) fail(writer http.ResponseWriter, request *http.Request, err error) {
	if s.Logger != nil {
		s.Logger.Error("page failed", "path", request.URL.Path, "error", err)
	}
	s.message(writer, request, http.StatusInternalServerError, "Error_FailedTitle", "Error_Failed")
}

// current refuses every page but static files while a newer server has laid out the stored data.
func (s *Site) current(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if _, err := s.Service.Board(request.Context()); errors.Is(err, scoreboard.ErrStorageNewer) {
			s.fail(writer, request, err)
			return
		}
		next(writer, request)
	}
}

// redirect goes back to an admin page with a notice key, which the page shows in its language.
func (s *Site) redirect(writer http.ResponseWriter, request *http.Request, page, notice string) {
	target := s.basePath() + "/admin/" + page
	if notice != "" {
		target += "?done=" + url.QueryEscape(notice)
	}
	http.Redirect(writer, request, target, http.StatusSeeOther)
}

// serveStatic serves a static file, but never a Markdown file.
func (s *Site) serveStatic(writer http.ResponseWriter, request *http.Request) {
	name := path.Clean(request.PathValue("file"))
	data, err := fs.ReadFile(s.static, name)
	if err != nil || strings.HasSuffix(name, ".md") {
		http.NotFound(writer, request)
		return
	}
	header := writer.Header()
	if contentType, known := fontTypes[path.Ext(name)]; known {
		header.Set("Content-Type", contentType)
	}
	// Only the exact version is cached for good; others, like the font the stylesheet names, revalidate.
	header.Set("ETag", `"`+s.versions[name]+`"`)
	if request.URL.Query().Get("v") == s.versions[name] {
		header.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		header.Set("Cache-Control", "no-cache")
	}
	http.ServeContent(writer, request, name, time.Time{}, bytes.NewReader(data))
}

// setLanguage remembers a chosen language and returns to the page, if that page is the board's own.
func (s *Site) setLanguage(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, formBodyLimit)
	language := request.PostFormValue("language")
	for _, code := range Languages {
		if code == language {
			http.SetCookie(writer, &http.Cookie{
				Name: languageCookie, Value: code, Path: s.basePath() + "/", MaxAge: languageCookieAge,
				HttpOnly: true, Secure: s.PublicURL.Scheme == "https", SameSite: http.SameSiteLaxMode,
			})
		}
	}
	back := request.PostFormValue("return")
	if !strings.HasPrefix(back, s.basePath()+"/") || strings.HasPrefix(back, "//") || strings.ContainsAny(back, "\\\r\n") {
		back = s.basePath() + "/"
	}
	http.Redirect(writer, request, back, http.StatusSeeOther)
}

// setCookie sets a strict cookie on the admin paths; a lifetime of zero or less deletes it.
func (s *Site) setCookie(writer http.ResponseWriter, name, value string, lifetime time.Duration) {
	cookie := &http.Cookie{
		Name: name, Value: value, Path: s.basePath() + "/admin", HttpOnly: true,
		Secure: s.PublicURL.Scheme == "https", SameSite: http.SameSiteStrictMode,
	}
	if lifetime > 0 {
		cookie.MaxAge = int(lifetime.Seconds())
	} else {
		cookie.MaxAge = -1
	}
	http.SetCookie(writer, cookie)
}
