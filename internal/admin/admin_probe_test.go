package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/admin"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// probeSetupCode is the setup code every probe admin is configured with.
const probeSetupCode = "ProbeSetupCode-16+chars"

// probeUsername is the admin every probe creates.
const probeUsername = "avery"

// probePassword is the password every probe admin starts with.
const probePassword = "a long enough password"

// probeClock is a clock the test moves by hand.
type probeClock struct {
	// mu guards now, since sign-ins run from several goroutines.
	mu sync.Mutex

	// now is what Now returns.
	now time.Time
}

// Now returns the clock's time.
func (c *probeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *probeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// probeAdmin returns an admin over a fresh directory store, its clock and the store's directory.
func probeAdmin(t *testing.T) (*admin.Admin, *probeClock, string) {
	t.Helper()
	directory := t.TempDir()
	opened, err := store.OpenDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { opened.Close() })
	clock := &probeClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	administrator := &admin.Admin{
		Store: opened, Now: clock.Now, SetupCode: probeSetupCode, DataDirectory: directory, Pepper: []byte("probe pepper"),
	}
	return administrator, clock, directory
}

// setUpAdmin returns an admin already set up as probeUsername with probePassword.
func setUpAdmin(t *testing.T) (*admin.Admin, *probeClock, string) {
	t.Helper()
	administrator, clock, directory := probeAdmin(t)
	if err := administrator.Setup(context.Background(), probeSetupCode, probeUsername, probePassword); err != nil {
		t.Fatal(err)
	}
	return administrator, clock, directory
}

// TestProbeSetupCodeRules proves setup needs the configured code and happens once.
func TestProbeSetupCodeRules(t *testing.T) {
	ctx := context.Background()
	administrator, _, _ := probeAdmin(t)
	if ok, err := administrator.IsSetUp(ctx); ok || err != nil {
		t.Fatalf("IsSetUp before setup = %v, %v", ok, err)
	}
	if _, err := administrator.Account(ctx); !errors.Is(err, admin.ErrNotSetUp) {
		t.Fatalf("Account before setup = %v, want ErrNotSetUp", err)
	}
	for _, code := range []string{"", "probesetupcode-16+chars", probeSetupCode + " ", " " + probeSetupCode, probeSetupCode[:len(probeSetupCode)-1]} {
		if err := administrator.Setup(ctx, code, probeUsername, probePassword); !errors.Is(err, admin.ErrWrongCode) {
			t.Errorf("Setup with code %q = %v, want ErrWrongCode", code, err)
		}
	}
	if err := administrator.Setup(ctx, probeSetupCode, probeUsername, probePassword); err != nil {
		t.Fatal(err)
	}
	if ok, err := administrator.IsSetUp(ctx); !ok || err != nil {
		t.Fatalf("IsSetUp after setup = %v, %v", ok, err)
	}
	if err := administrator.Setup(ctx, probeSetupCode, "mallory", "another long password"); !errors.Is(err, admin.ErrAlreadySetUp) {
		t.Fatalf("second Setup with the right code = %v, want ErrAlreadySetUp", err)
	}
	if err := administrator.Setup(ctx, "wrong code entirely", "mallory", "another long password"); !errors.Is(err, admin.ErrAlreadySetUp) && !errors.Is(err, admin.ErrWrongCode) {
		t.Fatalf("second Setup with a wrong code = %v", err)
	}
	if err := administrator.SignIn(ctx, "mallory", "another long password"); !errors.Is(err, admin.ErrWrongCredentials) {
		t.Fatalf("mallory signed in after a refused setup: %v", err)
	}
}

// TestProbeShortConfiguredSetupCode records what the package does with a setup code under MinimumCodeLength.
func TestProbeShortConfiguredSetupCode(t *testing.T) {
	ctx := context.Background()
	administrator, _, _ := probeAdmin(t)
	administrator.SetupCode = ""
	if err := administrator.Setup(ctx, "", probeUsername, probePassword); !errors.Is(err, admin.ErrWrongCode) {
		t.Errorf("Setup with no configured code and an empty code = %v, want ErrWrongCode", err)
	}
	// PROBE: MinimumCodeLength says "the server accepts from configuration"; the package itself honours a
	// 5-character SetupCode and leaves the check to the command's configuration loading (which does refuse it).
	for _, code := range []string{"short", strings.Repeat("x", admin.MinimumCodeLength-1)} {
		administrator, _, _ := probeAdmin(t)
		administrator.SetupCode = code
		t.Logf("PROBE: Setup with a configured setup code of %d characters: %v", len(code), administrator.Setup(ctx, code, probeUsername, probePassword))
	}
	exact, _, _ := probeAdmin(t)
	exact.SetupCode = strings.Repeat("x", admin.MinimumCodeLength)
	if err := exact.Setup(ctx, exact.SetupCode, probeUsername, probePassword); err != nil {
		t.Errorf("Setup with a code of exactly MinimumCodeLength: %v", err)
	}
}

// TestProbeUsernameRules checks the 1 to 64 character rule and trimming.
func TestProbeUsernameRules(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		// name reads as the case.
		name string

		// username is given to Setup.
		username string

		// signInAs is tried after setup, when accepted.
		signInAs string

		// accept is whether Setup must succeed.
		accept bool
	}{
		{"one character", "a", "a", true},
		{"64 ASCII characters", strings.Repeat("u", 64), strings.Repeat("u", 64), true},
		{"65 ASCII characters", strings.Repeat("u", 65), "", false},
		{"empty", "", "", false},
		{"only spaces", "    ", "", false},
		{"padded with spaces", "  admin  ", "admin", true},
		{"padded at sign-in", "admin", "  admin  ", true},
		{"unicode", "Zoë ✓ 管理", "Zoë ✓ 管理", true},
		{"65 two-byte characters", strings.Repeat("é", 65), "", false},
		{"64 emoji", strings.Repeat("😀", 64), strings.Repeat("😀", 64), true},
		{"a tab around it", "\tadmin\t", "admin", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			administrator, _, _ := probeAdmin(t)
			err := administrator.Setup(ctx, probeSetupCode, test.username, probePassword)
			if !test.accept {
				if !errors.Is(err, admin.ErrInvalidUsername) {
					t.Errorf("Setup(%q) = %v, want ErrInvalidUsername", test.username, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Setup(%q) = %v", test.username, err)
			}
			if err := administrator.SignIn(ctx, test.signInAs, probePassword); err != nil {
				t.Errorf("SignIn(%q) after Setup(%q) = %v", test.signInAs, test.username, err)
			}
			account, _ := administrator.Account(ctx)
			t.Logf("PROBE: Setup(%q) stored Username %q", test.username, account.Username)
		})
	}
}

// TestProbePasswordLengthRules counts the password in graphemes at both limits.
func TestProbePasswordLengthRules(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		// name reads as the case.
		name string

		// password is given to Setup.
		password string

		// accept is whether Setup must succeed.
		accept bool
	}{
		{"7 ASCII", "1234567", false},
		{"8 ASCII", "12345678", true},
		{"8 emoji", strings.Repeat("😀", 8), true},
		{"7 emoji", strings.Repeat("😀", 7), false},
		{"7 letters with combining accents", strings.Repeat("é", 7), false},
		{"8 letters with combining accents", strings.Repeat("é", 8), true},
		{"1024 ASCII", strings.Repeat("p", 1024), true},
		{"1025 ASCII", strings.Repeat("p", 1025), false},
		{"empty", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			administrator, _, _ := probeAdmin(t)
			err := administrator.Setup(ctx, probeSetupCode, probeUsername, test.password)
			if test.accept && err != nil {
				t.Errorf("refused: %v", err)
			}
			if !test.accept && !errors.Is(err, admin.ErrInvalidPassword) {
				t.Errorf("got %v, want ErrInvalidPassword", err)
			}
			if test.accept && err == nil {
				if err := administrator.SignIn(ctx, probeUsername, test.password); err != nil {
					t.Errorf("sign-in with the accepted password: %v", err)
				}
			}
		})
	}
}

// TestProbeBreachHook checks the breach hook's three outcomes and that it never sees a wrong guess.
func TestProbeBreachHook(t *testing.T) {
	ctx := context.Background()
	administrator, _, _ := probeAdmin(t)
	var seen []string
	breached := map[string]bool{"breached password": true}
	failing := false
	administrator.Breached = func(_ context.Context, candidate string) (bool, error) {
		seen = append(seen, candidate)
		if failing {
			return true, errors.New("unreachable")
		}
		return breached[candidate], nil
	}
	if err := administrator.Setup(ctx, probeSetupCode, probeUsername, "breached password"); !errors.Is(err, admin.ErrBreachedPassword) {
		t.Fatalf("Setup with a breached password = %v", err)
	}
	if ok, _ := administrator.IsSetUp(ctx); ok {
		t.Fatal("a refused setup created the admin")
	}
	failing = true
	if err := administrator.Setup(ctx, probeSetupCode, probeUsername, probePassword); err != nil {
		t.Fatalf("Setup while the hook fails (returning true with an error) = %v, want nil", err)
	}
	failing = false

	seen = nil
	if err := administrator.SignIn(ctx, probeUsername, "not the password at all"); !errors.Is(err, admin.ErrWrongCredentials) {
		t.Fatalf("wrong password = %v", err)
	}
	if err := administrator.SignIn(ctx, "nobody", probePassword); !errors.Is(err, admin.ErrWrongCredentials) {
		t.Fatalf("wrong username = %v", err)
	}
	if len(seen) != 0 {
		t.Errorf("the breach hook saw wrong guesses %q; only successful sign-ins may be checked", seen)
	}
	breached[probePassword] = true
	if err := administrator.SignIn(ctx, probeUsername, probePassword); err != nil {
		t.Fatalf("a correct but breached password = %v, want nil with a flag", err)
	}
	if account, _ := administrator.Account(ctx); !account.PasswordBreached {
		t.Error("PasswordBreached not set after a breached sign-in")
	}
	if err := administrator.ChangePassword(ctx, "", probePassword, "breached password"); !errors.Is(err, admin.ErrBreachedPassword) {
		t.Fatalf("ChangePassword to a breached password = %v", err)
	}
	if account, _ := administrator.Account(ctx); !account.PasswordBreached {
		t.Error("a refused change cleared PasswordBreached")
	}
	if err := administrator.ChangePassword(ctx, "", probePassword, "a clean new password"); err != nil {
		t.Fatal(err)
	}
	if account, _ := administrator.Account(ctx); account.PasswordBreached {
		t.Error("PasswordBreached still set after the password changed")
	}

	administrator.Breached = nil
	if err := administrator.ChangePassword(ctx, "", "a clean new password", "breached password"); err != nil {
		t.Errorf("with no hook a listed password must pass: %v", err)
	}
}

// TestProbeSignInAnswers checks each sign-in answer, including the same one for right and wrong with the password off.
func TestProbeSignInAnswers(t *testing.T) {
	ctx := context.Background()
	administrator, _, _ := probeAdmin(t)
	err := administrator.SignIn(ctx, probeUsername, probePassword)
	if err == nil {
		t.Fatal("signed in before setup")
	}
	t.Logf("PROBE: SignIn before Setup: %v", err)
	if err := administrator.Setup(ctx, probeSetupCode, probeUsername, probePassword); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		// name reads as the case.
		name string

		// username and password are the guess.
		username, password string

		// want is the expected error.
		want error
	}{
		{"right", probeUsername, probePassword, nil},
		{"wrong password", probeUsername, probePassword + "x", admin.ErrWrongCredentials},
		{"password with trailing space", probeUsername, probePassword + " ", admin.ErrWrongCredentials},
		{"empty password", probeUsername, "", admin.ErrWrongCredentials},
		{"wrong username", "mallory", probePassword, admin.ErrWrongCredentials},
		{"username in another case", strings.ToUpper(probeUsername), probePassword, admin.ErrWrongCredentials},
		{"empty username", "", probePassword, admin.ErrWrongCredentials},
	} {
		if err := administrator.SignIn(ctx, test.username, test.password); !errors.Is(err, test.want) {
			t.Errorf("%s: SignIn = %v, want %v", test.name, err, test.want)
		}
	}
	if err := administrator.AddPasskey(ctx, admin.Passkey{ID: "phone", Name: "Phone"}); err != nil {
		t.Fatal(err)
	}
	if err := administrator.SetPasswordEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	for _, guess := range []string{probePassword, "wrong", ""} {
		if err := administrator.SignIn(ctx, probeUsername, guess); !errors.Is(err, admin.ErrPasswordOff) {
			t.Errorf("password off, SignIn(%q) = %v, want ErrPasswordOff", guess, err)
		}
	}
	if err := administrator.SignIn(ctx, "mallory", probePassword); !errors.Is(err, admin.ErrPasswordOff) {
		t.Errorf("password off, wrong username = %v, want ErrPasswordOff", err)
	}
	if err := administrator.SetPasswordEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := administrator.SignIn(ctx, probeUsername, probePassword); err != nil {
		t.Errorf("after turning the password back on: %v", err)
	}
}

// TestProbeSessionLifetime checks the 12 hour boundary, unknown tokens, ended sessions and no renewal.
func TestProbeSessionLifetime(t *testing.T) {
	ctx := context.Background()
	administrator, clock, _ := setUpAdmin(t)
	token, session, err := administrator.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || session.CSRF == "" {
		t.Fatalf("NewSession returned token %q, CSRF %q", token, session.CSRF)
	}
	if !session.CreatedAt.Equal(clock.Now()) {
		t.Errorf("CreatedAt %v, clock %v", session.CreatedAt, clock.Now())
	}
	if !session.ExpiresAt.Equal(session.CreatedAt.Add(admin.SessionLifetime)) {
		t.Errorf("ExpiresAt %v, want CreatedAt + 12h", session.ExpiresAt)
	}
	for _, bad := range []string{"", token[:len(token)-1], token + "x", strings.ToUpper(token), swapFirstCharacter(token), "not a token"} {
		if _, err := administrator.Session(ctx, bad); !errors.Is(err, admin.ErrSessionExpired) {
			t.Errorf("Session(%q) = %v, want ErrSessionExpired", bad, err)
		}
	}

	clock.Advance(6 * time.Hour)
	if found, err := administrator.Session(ctx, token); err != nil || found.CSRF != session.CSRF {
		t.Fatalf("at 6h: %v", err)
	}
	clock.Advance(6*time.Hour - time.Second)
	if _, err := administrator.Session(ctx, token); err != nil {
		t.Errorf("at 11h59m59s: %v, want alive", err)
	}
	clock.Advance(time.Second)
	_, atBoundary := administrator.Session(ctx, token)
	t.Logf("PROBE: Session at exactly 12h after creation: %v", atBoundary)
	clock.Advance(time.Second)
	if _, err := administrator.Session(ctx, token); !errors.Is(err, admin.ErrSessionExpired) {
		t.Errorf("at 12h1s: %v, want ErrSessionExpired (use at 6h must not renew)", err)
	}

	token, _, _ = administrator.NewSession(ctx)
	if err := administrator.EndSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := administrator.Session(ctx, token); !errors.Is(err, admin.ErrSessionExpired) {
		t.Errorf("an ended session: %v", err)
	}
	t.Logf("PROBE: EndSession twice: %v", administrator.EndSession(ctx, token))
	t.Logf("PROBE: EndSession of an unknown token: %v", administrator.EndSession(ctx, "unknown"))
}

// swapFirstCharacter changes the first character of s to a different URL-safe one.
func swapFirstCharacter(s string) string {
	replacement := byte('A')
	if s[0] == 'A' {
		replacement = 'B'
	}
	return string(replacement) + s[1:]
}

// TestProbeSessionTokensAreDistinct starts many sessions and expects unique tokens and CSRF values.
func TestProbeSessionTokensAreDistinct(t *testing.T) {
	ctx := context.Background()
	administrator, _, directory := setUpAdmin(t)
	tokens, csrfs := map[string]bool{}, map[string]bool{}
	var first string
	for i := 0; i < 50; i++ {
		token, session, err := administrator.NewSession(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = token
		}
		if tokens[token] || csrfs[session.CSRF] {
			t.Fatalf("repeated token or CSRF at session %d", i)
		}
		tokens[token], csrfs[session.CSRF] = true, true
		if len(token) < 22 {
			t.Errorf("token %q is only %d characters", token, len(token))
		}
	}
	if found := filesContaining(t, directory, first); len(found) != 0 {
		t.Errorf("the token is stored in plain form in %v", found)
	}
	if found := filesContaining(t, directory, probePassword); len(found) != 0 {
		t.Errorf("the password is stored in plain form in %v", found)
	}
}

// filesContaining returns the files beneath root whose contents include needle.
func filesContaining(t *testing.T, root, needle string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), needle) {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestProbeChangePasswordKeepsOneSession changes the password from the middle of three sessions.
func TestProbeChangePasswordKeepsOneSession(t *testing.T) {
	ctx := context.Background()
	administrator, _, _ := setUpAdmin(t)
	var tokens [3]string
	for i := range tokens {
		tokens[i], _, _ = administrator.NewSession(ctx)
	}
	if err := administrator.ChangePassword(ctx, tokens[1], "not the current password", "a brand new password"); !errors.Is(err, admin.ErrWrongCredentials) {
		t.Fatalf("ChangePassword with the wrong current password = %v", err)
	}
	for i, token := range tokens {
		if _, err := administrator.Session(ctx, token); err != nil {
			t.Errorf("a refused change ended session %d: %v", i, err)
		}
	}
	if err := administrator.SignIn(ctx, probeUsername, probePassword); err != nil {
		t.Errorf("a refused change altered the password: %v", err)
	}

	if err := administrator.ChangePassword(ctx, tokens[1], probePassword, "a brand new password"); err != nil {
		t.Fatal(err)
	}
	for i, token := range tokens {
		_, err := administrator.Session(ctx, token)
		if i == 1 && err != nil {
			t.Errorf("the kept session ended: %v", err)
		}
		if i != 1 && !errors.Is(err, admin.ErrSessionExpired) {
			t.Errorf("session %d survived the change: %v", i, err)
		}
	}
	if err := administrator.SignIn(ctx, probeUsername, probePassword); !errors.Is(err, admin.ErrWrongCredentials) {
		t.Errorf("the old password still works: %v", err)
	}
	if err := administrator.SignIn(ctx, probeUsername, "a brand new password"); err != nil {
		t.Errorf("the new password: %v", err)
	}
	if err := administrator.ChangePassword(ctx, tokens[1], "a brand new password", "a brand new password"); err != nil {
		t.Logf("PROBE: changing to the same password: %v", err)
	}
	if err := administrator.ChangePassword(ctx, tokens[1], "a brand new password", "short"); !errors.Is(err, admin.ErrInvalidPassword) {
		t.Errorf("ChangePassword to a short password = %v", err)
	}
	if _, err := administrator.Session(ctx, tokens[1]); err != nil {
		t.Errorf("a refused change ended the kept session: %v", err)
	}
	other, _, _ := administrator.NewSession(ctx)
	if err := administrator.ChangePassword(ctx, "no such token", "a brand new password", "yet another password"); err != nil {
		t.Fatal(err)
	}
	if _, err := administrator.Session(ctx, other); !errors.Is(err, admin.ErrSessionExpired) {
		t.Errorf("with an unknown keep token every session must end: %v", err)
	}
}

// TestProbeRemovePasskeyKeepsOneSession removes a passkey from the middle of three sessions.
func TestProbeRemovePasskeyKeepsOneSession(t *testing.T) {
	ctx := context.Background()
	administrator, _, _ := setUpAdmin(t)
	if err := administrator.AddPasskey(ctx, admin.Passkey{ID: "stolen", Name: "Stolen"}); err != nil {
		t.Fatal(err)
	}
	var tokens [3]string
	for i := range tokens {
		tokens[i], _, _ = administrator.NewSession(ctx)
	}
	// PROBE: RemovePasskey of an unknown identifier answers nil rather than ErrUnknownPasskey; the doc only says the
	// error is for "using" a passkey. It must at least not end other sessions.
	t.Logf("PROBE: RemovePasskey of an unknown passkey: %v", administrator.RemovePasskey(ctx, tokens[1], "unknown"))
	for i, token := range tokens {
		if _, err := administrator.Session(ctx, token); err != nil {
			t.Errorf("removing an unknown passkey ended session %d: %v", i, err)
		}
	}
	if account, _ := administrator.Account(ctx); len(account.Passkeys) != 1 {
		t.Errorf("removing an unknown passkey changed the passkeys: %+v", account.Passkeys)
	}
	if err := administrator.RemovePasskey(ctx, tokens[1], "stolen"); err != nil {
		t.Fatal(err)
	}
	for i, token := range tokens {
		_, err := administrator.Session(ctx, token)
		if i == 1 && err != nil {
			t.Errorf("the kept session ended: %v", err)
		}
		if i != 1 && !errors.Is(err, admin.ErrSessionExpired) {
			t.Errorf("session %d survived the removal: %v", i, err)
		}
	}
	if account, _ := administrator.Account(ctx); len(account.Passkeys) != 0 {
		t.Errorf("passkeys after removal: %+v", account.Passkeys)
	}
}

// TestProbePasskeyAndPasswordRules walks the one-way-in invariant through every transition.
func TestProbePasskeyAndPasswordRules(t *testing.T) {
	ctx := context.Background()
	administrator, clock, _ := probeAdmin(t)
	if err := administrator.AddPasskey(ctx, admin.Passkey{ID: "early", Name: "Early"}); !errors.Is(err, admin.ErrNotSetUp) {
		t.Errorf("AddPasskey before setup = %v, want ErrNotSetUp", err)
	}
	if err := administrator.Setup(ctx, probeSetupCode, probeUsername, probePassword); err != nil {
		t.Fatal(err)
	}
	if err := administrator.SetPasswordEnabled(ctx, false); !errors.Is(err, admin.ErrNoPasskey) {
		t.Fatalf("SetPasswordEnabled(false) with no passkey = %v", err)
	}
	if account, _ := administrator.Account(ctx); !account.PasswordEnabled {
		t.Error("a refused SetPasswordEnabled(false) turned the password off")
	}
	if err := administrator.AddPasskey(ctx, admin.Passkey{ID: "one", Name: "  Phone  "}); err != nil {
		t.Fatal(err)
	}
	account, _ := administrator.Account(ctx)
	if len(account.Passkeys) != 1 || account.Passkeys[0].ID != "one" {
		t.Fatalf("passkeys %+v", account.Passkeys)
	}
	t.Logf("PROBE: AddPasskey stored Name %q for %q, CreatedAt %v (clock %v)", account.Passkeys[0].Name, "  Phone  ", account.Passkeys[0].CreatedAt, clock.Now())
	if err := administrator.SetPasswordEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := administrator.RemovePasskey(ctx, "", "one"); !errors.Is(err, admin.ErrLastPasskey) {
		t.Fatalf("removing the last passkey with the password off = %v", err)
	}
	if account, _ := administrator.Account(ctx); len(account.Passkeys) != 1 {
		t.Error("the refused removal removed the passkey")
	}
	if err := administrator.SetPasswordEnabled(ctx, false); err != nil {
		t.Errorf("SetPasswordEnabled(false) twice = %v", err)
	}
	if err := administrator.SetPasswordEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := administrator.RemovePasskey(ctx, "", "one"); err != nil {
		t.Fatalf("removing the last passkey with the password on = %v", err)
	}
	// PROBE: removing or renaming a passkey that is gone answers nil, not ErrUnknownPasskey; only UsePasskey refuses.
	t.Logf("PROBE: removing a removed passkey: %v", administrator.RemovePasskey(ctx, "", "one"))
	t.Logf("PROBE: renaming a removed passkey: %v", administrator.RenamePasskey(ctx, "one", "Gone"))
	if account, _ := administrator.Account(ctx); len(account.Passkeys) != 0 {
		t.Errorf("renaming a removed passkey brought it back: %+v", account.Passkeys)
	}
	if err := administrator.UsePasskey(ctx, "one", 1); !errors.Is(err, admin.ErrUnknownPasskey) {
		t.Errorf("using a removed passkey = %v, want ErrUnknownPasskey", err)
	}
}

// TestProbePasskeyLimitAndDuplicates adds passkeys until refused and records the limit and duplicate handling.
func TestProbePasskeyLimitAndDuplicates(t *testing.T) {
	ctx := context.Background()
	administrator, _, _ := setUpAdmin(t)
	added := 0
	for added < 1000 {
		err := administrator.AddPasskey(ctx, admin.Passkey{ID: fmt.Sprintf("key-%03d", added), Name: "Key"})
		if errors.Is(err, admin.ErrTooManyPasskeys) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		added++
	}
	if added == 1000 {
		t.Fatal("1000 passkeys accepted; no limit found")
	}
	t.Logf("PROBE: the passkey limit is %d", added)
	account, _ := administrator.Account(ctx)
	if len(account.Passkeys) != added {
		t.Errorf("Account holds %d passkeys after %d accepted", len(account.Passkeys), added)
	}
	if err := administrator.RemovePasskey(ctx, "", "key-000"); err != nil {
		t.Fatal(err)
	}
	if err := administrator.AddPasskey(ctx, admin.Passkey{ID: "replacement", Name: "Key"}); err != nil {
		t.Errorf("adding after a removal at the limit: %v", err)
	}

	fresh, _, _ := setUpAdmin(t)
	if err := fresh.AddPasskey(ctx, admin.Passkey{ID: "same", Name: "First"}); err != nil {
		t.Fatal(err)
	}
	err := fresh.AddPasskey(ctx, admin.Passkey{ID: "same", Name: "Second"})
	account, _ = fresh.Account(ctx)
	t.Logf("PROBE: AddPasskey with a duplicate ID: %v; account now holds %d passkeys", err, len(account.Passkeys))
	if err == nil && len(account.Passkeys) != 1 {
		t.Errorf("a duplicate passkey ID was stored twice")
	}
	t.Logf("PROBE: AddPasskey with an empty ID: %v", fresh.AddPasskey(ctx, admin.Passkey{ID: "", Name: "Empty"}))
	t.Logf("PROBE: AddPasskey with a non-base64url ID: %v", fresh.AddPasskey(ctx, admin.Passkey{ID: "not base64url!", Name: "Odd"}))
}

// TestProbePasskeyNames records the name limits of AddPasskey and RenamePasskey.
func TestProbePasskeyNames(t *testing.T) {
	ctx := context.Background()
	administrator, _, _ := setUpAdmin(t)
	if err := administrator.AddPasskey(ctx, admin.Passkey{ID: "named", Name: "Phone"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "   ", "  Laptop  ", strings.Repeat("n", 64), strings.Repeat("n", 65), strings.Repeat("n", 1000), strings.Repeat("😀", 64), "line\nbreak", "tab\tbed", "nul\x00byte"} {
		err := administrator.RenamePasskey(ctx, "named", name)
		account, _ := administrator.Account(ctx)
		t.Logf("PROBE: RenamePasskey(%q) (%d bytes): %v; stored %q (%d bytes)", truncate(name), len(name), err, truncate(account.Passkeys[0].Name), len(account.Passkeys[0].Name))
		if err == nil && strings.Repeat("n", 1000) == account.Passkeys[0].Name {
			t.Error("a 1000-character name was stored as given")
		}
	}
}

// truncate shortens s for a log line.
func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// TestProbeUsePasskeyCounts checks the count rule and the last-used time through UsePasskey.
func TestProbeUsePasskeyCounts(t *testing.T) {
	ctx := context.Background()
	administrator, clock, _ := setUpAdmin(t)
	if err := administrator.AddPasskey(ctx, admin.Passkey{ID: "counted", Name: "Counted", SignCount: 5}); err != nil {
		t.Fatal(err)
	}
	if err := administrator.AddPasskey(ctx, admin.Passkey{ID: "uncounted", Name: "Uncounted"}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		// name reads as the case.
		name string

		// id is the passkey used.
		id string

		// count is the count received.
		count uint32

		// want is the expected error.
		want error
	}{
		{"same count", "counted", 5, admin.ErrSignCountReused},
		{"lower count", "counted", 4, admin.ErrSignCountReused},
		{"zero after five", "counted", 0, admin.ErrSignCountReused},
		{"higher count", "counted", 6, nil},
		{"same again", "counted", 6, admin.ErrSignCountReused},
		{"zero after zero", "uncounted", 0, nil},
		{"one after zero", "uncounted", 1, nil},
		{"zero after one", "uncounted", 0, admin.ErrSignCountReused},
	} {
		clock.Advance(time.Minute)
		if err := administrator.UsePasskey(ctx, test.id, test.count); !errors.Is(err, test.want) {
			t.Errorf("%s: UsePasskey = %v, want %v", test.name, err, test.want)
		}
	}
	account, _ := administrator.Account(ctx)
	for _, passkey := range account.Passkeys {
		switch passkey.ID {
		case "counted":
			if passkey.SignCount != 6 {
				t.Errorf("counted SignCount %d, want 6", passkey.SignCount)
			}
		case "uncounted":
			if passkey.SignCount != 1 {
				t.Errorf("uncounted SignCount %d, want 1", passkey.SignCount)
			}
		}
		if passkey.LastUsedAt.IsZero() {
			t.Errorf("%s: LastUsedAt still zero after use", passkey.ID)
		}
		if passkey.LastUsedAt.After(clock.Now()) {
			t.Errorf("%s: LastUsedAt %v is after the clock %v", passkey.ID, passkey.LastUsedAt, clock.Now())
		}
	}
}

// TestProbeChallengeRules checks a ceremony's one use, purpose, and five minute lifetime.
func TestProbeChallengeRules(t *testing.T) {
	ctx := context.Background()
	administrator, clock, _ := setUpAdmin(t)
	ids, challenges := map[string]bool{}, map[string]bool{}
	for i := 0; i < 20; i++ {
		id, challenge, err := administrator.NewChallenge(ctx, "registration")
		if err != nil {
			t.Fatal(err)
		}
		if len(challenge) != 32 {
			t.Fatalf("challenge of %d bytes", len(challenge))
		}
		if id == "" || ids[id] || challenges[string(challenge)] {
			t.Fatalf("repeated challenge id or bytes at %d", i)
		}
		ids[id], challenges[string(challenge)] = true, true
	}

	id, challenge, _ := administrator.NewChallenge(ctx, "registration")
	taken, err := administrator.TakeChallenge(ctx, id, "registration")
	if err != nil || string(taken) != string(challenge) {
		t.Fatalf("TakeChallenge = %x, %v; want the challenge", taken, err)
	}
	if _, err := administrator.TakeChallenge(ctx, id, "registration"); !errors.Is(err, admin.ErrSessionExpired) {
		t.Errorf("second TakeChallenge = %v, want ErrSessionExpired", err)
	}

	id, _, _ = administrator.NewChallenge(ctx, "registration")
	if _, err := administrator.TakeChallenge(ctx, id, "signin"); !errors.Is(err, admin.ErrSessionExpired) {
		t.Errorf("TakeChallenge with another purpose = %v, want ErrSessionExpired", err)
	}
	_, afterWrongPurpose := administrator.TakeChallenge(ctx, id, "registration")
	t.Logf("PROBE: taking a challenge for its own purpose after a wrong-purpose attempt: %v", afterWrongPurpose)

	for _, bad := range []string{"", "unknown", id + "x", strings.ToUpper(id)} {
		if _, err := administrator.TakeChallenge(ctx, bad, "registration"); !errors.Is(err, admin.ErrSessionExpired) {
			t.Errorf("TakeChallenge(%q) = %v, want ErrSessionExpired", bad, err)
		}
	}

	id, _, _ = administrator.NewChallenge(ctx, "signin")
	clock.Advance(admin.ChallengeLifetime - time.Second)
	if _, err := administrator.TakeChallenge(ctx, id, "signin"); err != nil {
		t.Errorf("at 4m59s: %v, want alive", err)
	}
	id, _, _ = administrator.NewChallenge(ctx, "signin")
	clock.Advance(admin.ChallengeLifetime)
	_, atBoundary := administrator.TakeChallenge(ctx, id, "signin")
	t.Logf("PROBE: TakeChallenge at exactly 5m: %v", atBoundary)
	id, _, _ = administrator.NewChallenge(ctx, "signin")
	clock.Advance(admin.ChallengeLifetime + time.Second)
	if _, err := administrator.TakeChallenge(ctx, id, "signin"); !errors.Is(err, admin.ErrSessionExpired) {
		t.Errorf("at 5m1s: %v, want ErrSessionExpired", err)
	}
	t.Logf("PROBE: NewChallenge with an empty purpose: %v", func() error { _, _, err := administrator.NewChallenge(ctx, ""); return err }())
}

// TestProbeSweepKeepsLiveSessions runs Sweep with live and dead sessions and challenges mixed.
func TestProbeSweepKeepsLiveSessions(t *testing.T) {
	ctx := context.Background()
	administrator, clock, _ := setUpAdmin(t)
	old, _, _ := administrator.NewSession(ctx)
	oldChallenge, _, _ := administrator.NewChallenge(ctx, "signin")
	clock.Advance(admin.SessionLifetime + time.Second)
	live, _, _ := administrator.NewSession(ctx)
	liveChallenge, _, _ := administrator.NewChallenge(ctx, "signin")
	if err := administrator.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := administrator.Session(ctx, live); err != nil {
		t.Errorf("Sweep ended a live session: %v", err)
	}
	if _, err := administrator.Session(ctx, old); !errors.Is(err, admin.ErrSessionExpired) {
		t.Errorf("an old session after Sweep: %v", err)
	}
	if _, err := administrator.TakeChallenge(ctx, liveChallenge, "signin"); err != nil {
		t.Errorf("Sweep took a live challenge: %v", err)
	}
	if _, err := administrator.TakeChallenge(ctx, oldChallenge, "signin"); !errors.Is(err, admin.ErrSessionExpired) {
		t.Errorf("an old challenge after Sweep: %v", err)
	}
	if err := administrator.Sweep(ctx); err != nil {
		t.Errorf("Sweep with nothing to do: %v", err)
	}
}

// TestProbeRecoveryFile checks what reset-admin leaves behind and how the code is spent.
func TestProbeRecoveryFile(t *testing.T) {
	ctx := context.Background()
	administrator, clock, directory := setUpAdmin(t)
	code, err := admin.WriteRecoveryFile(directory, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(code) < admin.MinimumCodeLength {
		t.Errorf("recovery code %q is shorter than MinimumCodeLength", code)
	}
	path := filepath.Join(directory, admin.RecoveryFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no recovery file: %v", err)
	}
	t.Logf("PROBE: recovery file mode %v, %d bytes", info.Mode().Perm(), info.Size())
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("the recovery file is readable by others: %v", info.Mode().Perm())
	}
	if found := filesContaining(t, directory, code); len(found) != 0 {
		t.Errorf("the recovery code itself is written in %v", found)
	}
	second, _ := admin.WriteRecoveryFile(directory, clock.Now())
	if second == code {
		t.Error("two recovery codes are the same")
	}
	if err := administrator.Recover(ctx, code, "after reset password"); !errors.Is(err, admin.ErrWrongCode) {
		t.Errorf("the first code after a second WriteRecoveryFile = %v, want ErrWrongCode", err)
	}
	code = second

	if err := administrator.AddPasskey(ctx, admin.Passkey{ID: "lost", Name: "Lost"}); err != nil {
		t.Fatal(err)
	}
	token, _, _ := administrator.NewSession(ctx)
	if err := administrator.Recover(ctx, "not the code", "after reset password"); !errors.Is(err, admin.ErrWrongCode) {
		t.Fatalf("a wrong code = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a wrong code removed the recovery file: %v", err)
	}
	if err := administrator.Recover(ctx, code, "short"); !errors.Is(err, admin.ErrInvalidPassword) {
		t.Fatalf("Recover with a short password = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a refused recovery consumed the file: %v", err)
	}
	if err := administrator.SignIn(ctx, probeUsername, probePassword); err != nil {
		t.Errorf("the old password after a refused recovery: %v", err)
	}
	if _, err := administrator.Session(ctx, token); err != nil {
		t.Errorf("a refused recovery ended the session: %v", err)
	}
	administrator.Breached = func(context.Context, string) (bool, error) { return true, nil }
	if err := administrator.Recover(ctx, code, "a breached password"); !errors.Is(err, admin.ErrBreachedPassword) {
		t.Errorf("Recover with a breached password = %v", err)
	}
	administrator.Breached = nil

	clock.Advance(admin.RecoveryLifetime - time.Second)
	if err := administrator.Recover(ctx, code, "after reset password"); err != nil {
		t.Fatalf("Recover at 14m59s = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the recovery file survived its use")
	}
	account, err := administrator.Account(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(account.Passkeys) != 0 || !account.PasswordEnabled || account.Username != probeUsername {
		t.Errorf("after recovery: %+v", account)
	}
	if _, err := administrator.Session(ctx, token); !errors.Is(err, admin.ErrSessionExpired) {
		t.Error("a session outlived recovery")
	}
	if err := administrator.SignIn(ctx, probeUsername, probePassword); !errors.Is(err, admin.ErrWrongCredentials) {
		t.Error("the old password works after recovery")
	}
	if err := administrator.SignIn(ctx, probeUsername, "after reset password"); err != nil {
		t.Errorf("the recovered password: %v", err)
	}
	if err := administrator.Recover(ctx, code, "yet another password"); !errors.Is(err, admin.ErrWrongCode) {
		t.Errorf("the same file code twice = %v, want ErrWrongCode", err)
	}
}

// TestProbeRecoveryFileExpiry checks the 15 minute boundary of a reset-admin code.
func TestProbeRecoveryFileExpiry(t *testing.T) {
	ctx := context.Background()
	administrator, clock, directory := setUpAdmin(t)
	code, _ := admin.WriteRecoveryFile(directory, clock.Now())
	clock.Advance(admin.RecoveryLifetime)
	err := administrator.Recover(ctx, code, "after reset password")
	t.Logf("PROBE: Recover at exactly 15m: %v", err)
	if err == nil {
		return
	}
	clock.Advance(time.Second)
	if err := administrator.Recover(ctx, code, "after reset password"); !errors.Is(err, admin.ErrWrongCode) {
		t.Errorf("Recover at 15m1s = %v, want ErrWrongCode", err)
	}
	_, statErr := os.Stat(filepath.Join(directory, admin.RecoveryFileName))
	t.Logf("PROBE: the recovery file after an expired attempt: %v", statErr)
	backdated, _ := admin.WriteRecoveryFile(directory, clock.Now().Add(-admin.RecoveryLifetime-time.Minute))
	if err := administrator.Recover(ctx, backdated, "after reset password"); !errors.Is(err, admin.ErrWrongCode) {
		t.Errorf("a file written in the past = %v, want ErrWrongCode", err)
	}
	future, _ := admin.WriteRecoveryFile(directory, clock.Now().Add(time.Hour))
	t.Logf("PROBE: a recovery file dated an hour ahead: %v", administrator.Recover(ctx, future, "after reset password"))
}

// TestProbeConfiguredRecoveryCode checks the configured code works once and short ones never.
func TestProbeConfiguredRecoveryCode(t *testing.T) {
	ctx := context.Background()
	administrator, clock, _ := setUpAdmin(t)
	administrator.RecoveryCode = "ConfiguredRecoveryCode"
	if err := administrator.Recover(ctx, "configuredrecoverycode", "recovered password"); !errors.Is(err, admin.ErrWrongCode) {
		t.Errorf("the code in another case = %v", err)
	}
	if err := administrator.Recover(ctx, administrator.RecoveryCode, "short"); !errors.Is(err, admin.ErrInvalidPassword) {
		t.Errorf("Recover with a short password = %v", err)
	}
	if err := administrator.Recover(ctx, administrator.RecoveryCode, "recovered password"); err != nil {
		t.Fatalf("a refused recovery consumed the configured code: %v", err)
	}
	account, _ := administrator.Account(ctx)
	if len(account.UsedRecoveryCodes) != 1 {
		t.Errorf("UsedRecoveryCodes %v, want one entry", account.UsedRecoveryCodes)
	}
	for _, used := range account.UsedRecoveryCodes {
		if strings.Contains(used, administrator.RecoveryCode) {
			t.Error("UsedRecoveryCodes holds the code in plain form")
		}
	}
	clock.Advance(24 * time.Hour)
	if err := administrator.Recover(ctx, administrator.RecoveryCode, "recovered again"); !errors.Is(err, admin.ErrWrongCode) {
		t.Errorf("the configured code a second time = %v, want ErrWrongCode", err)
	}
	if err := administrator.SignIn(ctx, probeUsername, "recovered password"); err != nil {
		t.Errorf("the recovered password: %v", err)
	}
	administrator.RecoveryCode = "AnotherConfiguredCode"
	if err := administrator.Recover(ctx, administrator.RecoveryCode, "recovered again"); err != nil {
		t.Errorf("a new configured code = %v", err)
	}

	administrator.RecoveryCode = ""
	if err := administrator.Recover(ctx, "", "recovered thrice"); !errors.Is(err, admin.ErrWrongCode) {
		t.Errorf("no configured recovery code and an empty code = %v, want ErrWrongCode", err)
	}
	// PROBE: the README says the board refuses a value under 16 characters; the package itself honours a
	// 5-character RecoveryCode and leaves the check to the command's configuration loading (which does refuse it).
	for _, short := range []string{"short", strings.Repeat("c", admin.MinimumCodeLength-1)} {
		administrator.RecoveryCode = short
		t.Logf("PROBE: Recover with a configured recovery code of %d characters: %v", len(short), administrator.Recover(ctx, short, "recovered thrice"))
	}
	administrator.RecoveryCode = strings.Repeat("c", admin.MinimumCodeLength)
	if err := administrator.Recover(ctx, administrator.RecoveryCode, "recovered thrice"); err != nil {
		t.Errorf("a configured code of exactly MinimumCodeLength = %v", err)
	}
}

// TestProbeRecoverBeforeSetup proves recovery needs an admin to recover.
func TestProbeRecoverBeforeSetup(t *testing.T) {
	ctx := context.Background()
	administrator, clock, directory := probeAdmin(t)
	code, _ := admin.WriteRecoveryFile(directory, clock.Now())
	if err := administrator.Recover(ctx, code, "recovered password"); !errors.Is(err, admin.ErrNotSetUp) {
		t.Errorf("Recover before setup = %v, want ErrNotSetUp", err)
	}
	administrator.RecoveryCode = "ConfiguredRecoveryCode"
	if err := administrator.Recover(ctx, administrator.RecoveryCode, "recovered password"); !errors.Is(err, admin.ErrNotSetUp) {
		t.Errorf("Recover with a configured code before setup = %v, want ErrNotSetUp", err)
	}
	if ok, _ := administrator.IsSetUp(ctx); ok {
		t.Error("Recover created an admin")
	}
	_, err := os.Stat(filepath.Join(directory, admin.RecoveryFileName))
	t.Logf("PROBE: the recovery file after Recover before setup: %v", err)
}

// TestProbeAccountJSONHidesThePassword marshals the account and looks for the password.
func TestProbeAccountJSONHidesThePassword(t *testing.T) {
	ctx := context.Background()
	administrator, _, _ := setUpAdmin(t)
	account, err := administrator.Account(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(account)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), probePassword) {
		t.Error("the account JSON carries the password")
	}
	if account.PasswordHash == "" || account.PasswordHash == probePassword {
		t.Errorf("PasswordHash %q", account.PasswordHash)
	}
	if !strings.Contains(string(encoded), `"passkeys":[]`) {
		t.Errorf("passkeys must be [] not null: %s", encoded)
	}
	if account.UserHandle == "" {
		t.Error("UserHandle is empty")
	}
	if !strings.Contains(account.PasswordHash, "$") {
		t.Errorf("PasswordHash %q does not look like password.Hash's dollar-joined form", account.PasswordHash)
	}
}

// TestProbeConcurrentSessions hammers sign-in and sessions from 20 goroutines under the race detector.
func TestProbeConcurrentSessions(t *testing.T) {
	ctx := context.Background()
	administrator, _, _ := setUpAdmin(t)
	var group sync.WaitGroup
	failures := make(chan error, 200)
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := administrator.SignIn(ctx, probeUsername, probePassword); err != nil {
				failures <- fmt.Errorf("SignIn: %w", err)
				return
			}
			token, session, err := administrator.NewSession(ctx)
			if err != nil {
				failures <- fmt.Errorf("NewSession: %w", err)
				return
			}
			found, err := administrator.Session(ctx, token)
			if err != nil || found.CSRF != session.CSRF {
				failures <- fmt.Errorf("Session: %v", err)
				return
			}
			if _, _, err := administrator.NewChallenge(ctx, "signin"); err != nil {
				failures <- fmt.Errorf("NewChallenge: %w", err)
			}
			if err := administrator.EndSession(ctx, token); err != nil {
				failures <- fmt.Errorf("EndSession: %w", err)
				return
			}
			if _, err := administrator.Session(ctx, token); !errors.Is(err, admin.ErrSessionExpired) {
				failures <- fmt.Errorf("after EndSession: %v", err)
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}

// TestProbeConcurrentChangePassword races two changes from the same current password and expects one truth.
func TestProbeConcurrentChangePassword(t *testing.T) {
	ctx := context.Background()
	administrator, _, _ := setUpAdmin(t)
	candidates := []string{"first racer password", "second racer password"}
	results := make([]error, len(candidates))
	var group sync.WaitGroup
	for i, candidate := range candidates {
		group.Add(1)
		go func() {
			defer group.Done()
			results[i] = administrator.ChangePassword(ctx, "", probePassword, candidate)
		}()
	}
	group.Wait()
	working := -1
	for i, candidate := range candidates {
		if err := administrator.SignIn(ctx, probeUsername, candidate); err == nil {
			if working != -1 {
				t.Fatal("two passwords sign in")
			}
			working = i
		}
	}
	if working == -1 {
		t.Fatal("neither new password signs in")
	}
	if results[working] != nil {
		t.Errorf("the password that works was reported as failed: %v", results[working])
	}
	t.Logf("PROBE: concurrent ChangePassword results %v; %q won", results, candidates[working])
	if err := administrator.SignIn(ctx, probeUsername, probePassword); !errors.Is(err, admin.ErrWrongCredentials) {
		t.Errorf("the old password still works after the race: %v", err)
	}
}

// TestProbeConcurrentPasskeyAddsNeverConflict adds passkeys from several goroutines and expects every add to
// succeed or be refused for a reason the admin can act on, never a store conflict.
func TestProbeConcurrentPasskeyAddsNeverConflict(t *testing.T) {
	ctx := context.Background()
	administrator, _, _ := setUpAdmin(t)
	const adds = 16
	var group sync.WaitGroup
	errs := make([]error, adds)
	for i := 0; i < adds; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			errs[i] = administrator.AddPasskey(ctx, admin.Passkey{ID: fmt.Sprintf("racer-%d", i), Name: "Racer"})
		}()
	}
	group.Wait()
	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, admin.ErrTooManyPasskeys) {
			t.Errorf("AddPasskey: %v (errors.Is store.ErrConflict: %v)", err, errors.Is(err, store.ErrConflict))
		}
	}
	account, _ := administrator.Account(ctx)
	if len(account.Passkeys) != succeeded {
		t.Errorf("%d adds succeeded but the account holds %d passkeys", succeeded, len(account.Passkeys))
	}
}

// TestProbeConcurrentPasskeyRenamesNeverConflict renames different passkeys from several goroutines at once.
func TestProbeConcurrentPasskeyRenamesNeverConflict(t *testing.T) {
	ctx := context.Background()
	administrator, _, _ := setUpAdmin(t)
	const passkeys = 8
	for i := 0; i < passkeys; i++ {
		if err := administrator.AddPasskey(ctx, admin.Passkey{ID: fmt.Sprintf("renamed-%d", i), Name: "Old"}); err != nil {
			t.Fatal(err)
		}
	}
	var group sync.WaitGroup
	errs := make([]error, passkeys)
	for i := 0; i < passkeys; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			errs[i] = administrator.RenamePasskey(ctx, fmt.Sprintf("renamed-%d", i), fmt.Sprintf("New %d", i))
		}()
	}
	group.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("RenamePasskey %d: %v (errors.Is store.ErrConflict: %v)", i, err, errors.Is(err, store.ErrConflict))
		}
	}
	account, _ := administrator.Account(ctx)
	for _, passkey := range account.Passkeys {
		if passkey.Name == "Old" {
			t.Errorf("%s kept its old name", passkey.ID)
		}
	}
}
