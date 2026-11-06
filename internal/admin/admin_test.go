package admin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// setupCode is the setup code every test's admin is configured with.
const setupCode = "SetupCodeSetupCode"

// clock is a time the test moves by hand.
type clock struct {
	// now is the time Now returns.
	now time.Time
}

// Now returns the clock's time.
func (c *clock) Now() time.Time { return c.now }

// newAdmin returns an admin over a fresh directory store, with its clock.
func newAdmin(t *testing.T) (*Admin, *clock) {
	t.Helper()
	directory := t.TempDir()
	opened, err := store.OpenDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { opened.Close() })
	moment := &clock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	pepper := []byte("pepper for the tests, long enough")
	return &Admin{Store: opened, Now: moment.Now, SetupCode: setupCode, DataDirectory: directory, Pepper: pepper}, moment
}

// TestSetupOnlyOnce proves setup needs the right code and works only once.
func TestSetupOnlyOnce(t *testing.T) {
	administrator, _ := newAdmin(t)
	ctx := context.Background()
	if err := administrator.Setup(ctx, "WrongCodeWrongCode", "avery", "a long password"); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("a wrong code: %v", err)
	}
	if err := administrator.Setup(ctx, setupCode, "avery", "short"); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("a short password: %v", err)
	}
	if err := administrator.Setup(ctx, setupCode, "avery", "a long password"); err != nil {
		t.Fatal(err)
	}
	if err := administrator.Setup(ctx, setupCode, "mallory", "another password"); !errors.Is(err, ErrAlreadySetUp) {
		t.Fatalf("a second setup: %v", err)
	}
	if err := administrator.SignIn(ctx, "avery", "a long password"); err != nil {
		t.Fatalf("signing in: %v", err)
	}
	if err := administrator.SignIn(ctx, "mallory", "a long password"); !errors.Is(err, ErrWrongCredentials) {
		t.Fatalf("the wrong username: %v", err)
	}
}

// TestPasswordAndPasskeys proves the account keeps at least one way in at all times.
func TestPasswordAndPasskeys(t *testing.T) {
	administrator, _ := newAdmin(t)
	ctx := context.Background()
	if err := administrator.Setup(ctx, setupCode, "avery", "a long password"); err != nil {
		t.Fatal(err)
	}
	if err := administrator.SetPasswordEnabled(ctx, false); !errors.Is(err, ErrNoPasskey) {
		t.Fatalf("turning the password off with no passkey: %v", err)
	}
	for _, id := range []string{"first", "second"} {
		if err := administrator.AddPasskey(ctx, Passkey{ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := administrator.SetPasswordEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := administrator.SignIn(ctx, "avery", "a long password"); !errors.Is(err, ErrPasswordOff) {
		t.Fatalf("signing in with the password off: %v", err)
	}
	if err := administrator.RemovePasskey(ctx, "", "first"); err != nil {
		t.Fatal(err)
	}
	if err := administrator.RemovePasskey(ctx, "", "second"); !errors.Is(err, ErrLastPasskey) {
		t.Fatalf("removing the last passkey with the password off: %v", err)
	}
	if err := administrator.ChangePassword(ctx, "", "the wrong password", "a new long password"); !errors.Is(err, ErrWrongCredentials) {
		t.Fatalf("changing the password without the current one: %v", err)
	}
}

// TestRecovery proves both kinds of recovery code work as limited and clear every passkey and session.
func TestRecovery(t *testing.T) {
	administrator, moment := newAdmin(t)
	ctx := context.Background()
	if err := administrator.Setup(ctx, setupCode, "avery", "a long password"); err != nil {
		t.Fatal(err)
	}
	administrator.AddPasskey(ctx, Passkey{ID: "lost", Name: "Lost phone"})
	token, _, _ := administrator.NewSession(ctx)

	administrator.RecoveryCode = "RecoveryCodeRecovery"
	if err := administrator.Recover(ctx, "not the code at all", "recovered password"); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("a wrong code: %v", err)
	}
	if err := administrator.Recover(ctx, administrator.RecoveryCode, "recovered password"); err != nil {
		t.Fatal(err)
	}
	account, _ := administrator.Account(ctx)
	if len(account.Passkeys) != 0 || !account.PasswordEnabled {
		t.Fatalf("after recovery the account is %+v", account)
	}
	if _, err := administrator.Session(ctx, token); !errors.Is(err, ErrSessionExpired) {
		t.Fatal("a session outlived recovery")
	}
	if err := administrator.Recover(ctx, administrator.RecoveryCode, "again and again"); !errors.Is(err, ErrWrongCode) {
		t.Fatal("a configured recovery code worked twice")
	}

	code, err := WriteRecoveryFile(administrator.DataDirectory, moment.now)
	if err != nil {
		t.Fatal(err)
	}
	moment.now = moment.now.Add(RecoveryLifetime + time.Second)
	if err := administrator.Recover(ctx, code, "too late password"); !errors.Is(err, ErrWrongCode) {
		t.Fatal("a reset-admin code worked after it expired")
	}
	code, _ = WriteRecoveryFile(administrator.DataDirectory, moment.now)
	if err := administrator.Recover(ctx, code, "in time password"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(administrator.DataDirectory, RecoveryFileName)); !os.IsNotExist(err) {
		t.Fatal("the recovery file outlived its use")
	}
	if err := administrator.SignIn(ctx, "avery", "in time password"); err != nil {
		t.Fatalf("signing in after recovery: %v", err)
	}
}

// TestRecoveryFileMustBeRemoved proves a reset-admin code is refused when its file cannot be removed.
func TestRecoveryFileMustBeRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes files from a read-only directory")
	}
	administrator, moment := newAdmin(t)
	ctx := context.Background()
	if err := administrator.Setup(ctx, setupCode, "avery", "a long enough password"); err != nil {
		t.Fatal(err)
	}
	recovery := t.TempDir()
	administrator.DataDirectory = recovery
	code, err := WriteRecoveryFile(recovery, moment.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(recovery, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(recovery, 0o700) })

	if err := administrator.Recover(ctx, code, "recovered password"); err == nil {
		t.Fatal("Recover() = nil, want an error when the recovery file cannot be removed")
	}
	if err := administrator.SignIn(ctx, "avery", "a long enough password"); err != nil {
		t.Fatalf("the old password stopped working after a refused recovery: %v", err)
	}
}

// TestSessionsAndChallenges proves sessions and challenges end on time and a challenge answers once.
func TestSessionsAndChallenges(t *testing.T) {
	administrator, moment := newAdmin(t)
	ctx := context.Background()
	token, session, err := administrator.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if found, err := administrator.Session(ctx, token); err != nil || found.CSRF != session.CSRF {
		t.Fatalf("a new session: %v", err)
	}
	stored, _ := administrator.Store.List(ctx, sessionsCollection)
	if len(stored) != 1 || stored[0].ID == token {
		t.Fatal("the store holds the session's token rather than its hash")
	}

	id, challenge, err := administrator.NewChallenge(ctx, "signIn")
	if err != nil || len(challenge) != 32 {
		t.Fatalf("NewChallenge() = %d bytes, %v; want 32 bytes, nil", len(challenge), err)
	}
	if _, err := administrator.TakeChallenge(ctx, id, "register"); err == nil {
		t.Fatal("a challenge answered for another purpose")
	}
	id, _, _ = administrator.NewChallenge(ctx, "signIn")
	if _, err := administrator.TakeChallenge(ctx, id, "signIn"); err != nil {
		t.Fatal(err)
	}
	if _, err := administrator.TakeChallenge(ctx, id, "signIn"); err == nil {
		t.Fatal("a challenge answered twice")
	}

	moment.now = moment.now.Add(SessionLifetime)
	if _, err := administrator.Session(ctx, token); !errors.Is(err, ErrSessionExpired) {
		t.Fatal("a session outlived its lifetime")
	}
	if err := administrator.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if left, _ := administrator.Store.List(ctx, sessionsCollection); len(left) != 0 {
		t.Fatal("the sweep left an ended session")
	}
}

// TestBreachedPasswords refuses a breached new password, ignores a failed check, and flags one at sign-in.
func TestBreachedPasswords(t *testing.T) {
	administrator, _ := newAdmin(t)
	ctx := context.Background()
	breachedPasswords := map[string]bool{"breached password": true}
	checkFails := false
	administrator.Breached = func(_ context.Context, candidate string) (bool, error) {
		if checkFails {
			return false, errors.New("unreachable")
		}
		return breachedPasswords[candidate], nil
	}

	if err := administrator.Setup(ctx, setupCode, "avery", "breached password"); !errors.Is(err, ErrBreachedPassword) {
		t.Fatalf("a breached password at setup: %v", err)
	}
	checkFails = true
	if err := administrator.Setup(ctx, setupCode, "avery", "a long password"); err != nil {
		t.Fatalf("setup while the check fails: %v", err)
	}
	checkFails = false

	breachedPasswords["a long password"] = true
	if err := administrator.SignIn(ctx, "avery", "a long password"); err != nil {
		t.Fatalf("signing in with a password breached since: %v", err)
	}
	if account, _ := administrator.Account(ctx); !account.PasswordBreached {
		t.Fatal("a breached password at sign-in was not flagged")
	}
	if err := administrator.ChangePassword(ctx, "", "a long password", "breached password"); !errors.Is(err, ErrBreachedPassword) {
		t.Fatalf("changing to a breached password: %v", err)
	}
	if err := administrator.ChangePassword(ctx, "", "a long password", "a fresh 😀 password"); err != nil {
		t.Fatal(err)
	}
	if account, _ := administrator.Account(ctx); account.PasswordBreached {
		t.Fatal("changing the password did not clear the flag")
	}
}

// TestPasswordLengthCountsCharacters allows 1024 characters, counting an emoji once.
func TestPasswordLengthCountsCharacters(t *testing.T) {
	administrator, _ := newAdmin(t)
	ctx := context.Background()
	if err := administrator.Setup(ctx, setupCode, "avery", strings.Repeat("😀", 7)); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("seven emoji, which are 28 bytes: %v", err)
	}
	if err := administrator.Setup(ctx, setupCode, "avery", strings.Repeat("👨‍👩‍👧", 1025)); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("1025 characters: %v", err)
	}
	if err := administrator.Setup(ctx, setupCode, "avery", strings.Repeat("👨‍👩‍👧", 1024)); err != nil {
		t.Fatalf("1024 characters: %v", err)
	}
}

// TestPasswordOffAnswersAlike gives a wrong and a right password the same answer while the password is off.
func TestPasswordOffAnswersAlike(t *testing.T) {
	administrator, _ := newAdmin(t)
	ctx := context.Background()
	if err := administrator.Setup(ctx, setupCode, "avery", "a long password"); err != nil {
		t.Fatal(err)
	}
	administrator.AddPasskey(ctx, Passkey{ID: "only", Name: "Phone"})
	if err := administrator.SetPasswordEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	for _, guess := range []string{"a long password", "the wrong password"} {
		if err := administrator.SignIn(ctx, "avery", guess); !errors.Is(err, ErrPasswordOff) {
			t.Errorf("SignIn(%q) = %v, want ErrPasswordOff", guess, err)
		}
	}
}

// TestChangesEndOtherSessions proves a password change or a passkey removal signs out every other browser.
func TestChangesEndOtherSessions(t *testing.T) {
	administrator, _ := newAdmin(t)
	ctx := context.Background()
	if err := administrator.Setup(ctx, setupCode, "avery", "a long password"); err != nil {
		t.Fatal(err)
	}
	administrator.AddPasskey(ctx, Passkey{ID: "stolen", Name: "Stolen phone"})
	changes := map[string]func(keep string) error{
		"changing the password": func(keep string) error {
			return administrator.ChangePassword(ctx, keep, "a long password", "a long password")
		},
		"removing a passkey": func(keep string) error { return administrator.RemovePasskey(ctx, keep, "stolen") },
	}
	for name, change := range changes {
		current, _, _ := administrator.NewSession(ctx)
		other, _, _ := administrator.NewSession(ctx)
		if err := change(current); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := administrator.Session(ctx, current); err != nil {
			t.Errorf("%s ended the current session: %v", name, err)
		}
		if _, err := administrator.Session(ctx, other); !errors.Is(err, ErrSessionExpired) {
			t.Errorf("%s left another session alive: %v", name, err)
		}
	}
}

// TestSignCountIsCheckedInTheWrite lets only one of several uses carrying the same count through.
func TestSignCountIsCheckedInTheWrite(t *testing.T) {
	administrator, _ := newAdmin(t)
	ctx := context.Background()
	if err := administrator.Setup(ctx, setupCode, "avery", "a long password"); err != nil {
		t.Fatal(err)
	}
	administrator.AddPasskey(ctx, Passkey{ID: "cloned", Name: "Cloned key", SignCount: 4})

	const attempts = 8
	var group sync.WaitGroup
	results := make(chan error, attempts)
	for range attempts {
		group.Add(1)
		go func() {
			defer group.Done()
			results <- administrator.UsePasskey(ctx, "cloned", 5)
		}()
	}
	group.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d uses with the same count succeeded, want 1", succeeded)
	}
	if err := administrator.UsePasskey(ctx, "cloned", 3); !errors.Is(err, ErrSignCountReused) {
		t.Fatalf("UsePasskey with a lower count = %v, want ErrSignCountReused", err)
	}
	if err := administrator.UsePasskey(ctx, "missing", 9); !errors.Is(err, ErrUnknownPasskey) {
		t.Fatalf("UsePasskey of an unknown passkey = %v, want ErrUnknownPasskey", err)
	}
}
