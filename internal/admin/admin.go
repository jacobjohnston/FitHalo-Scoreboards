// Package admin keeps the board's one admin account: its password, passkeys, sessions and recovery.
package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/password"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/webauthn"
)

const (
	// accountCollection is the store collection that holds the account.
	accountCollection = "admin"

	// accountID is the account's document identifier.
	accountID = "admin"

	// sessionsCollection holds sessions, keyed by tokenKey.
	sessionsCollection = "sessions"

	// challengeCollection holds passkey ceremonies in progress.
	challengeCollection = "challenges"

	// SessionLifetime is how long a sign-in lasts.
	SessionLifetime = 12 * time.Hour

	// ChallengeLifetime is how long a passkey ceremony has to finish.
	ChallengeLifetime = 5 * time.Minute

	// RecoveryLifetime is how long a recovery code from reset-admin works.
	RecoveryLifetime = 15 * time.Minute

	// RecoveryFileName is the file reset-admin leaves for the running server, outside any collection.
	RecoveryFileName = ".recovery"

	// MinimumCodeLength is the shortest setup or recovery code the server accepts from configuration.
	MinimumCodeLength = 16

	// maximumUsernameLength is the most code points a username may have.
	maximumUsernameLength = 64

	// maximumPasskeyName is the most code points a passkey's name keeps.
	maximumPasskeyName = 60

	// maximumPasskeys is the most passkeys the account may register.
	maximumPasskeys = 20

	// identifierLength is the random bytes in a user handle or a challenge identifier.
	identifierLength = 16

	// recoveryCodeLength is the random bytes in a reset-admin recovery code, 24 base64url characters.
	recoveryCodeLength = 18

	// tokenLength is the random bytes in a session token, its CSRF token and a challenge.
	tokenLength = 32

	// tokenKeyLength is the bytes of a token's SHA-256 kept as its session's document identifier.
	tokenKeyLength = 16
)

// The errors the portal turns into messages.
var (
	// ErrAlreadySetUp is returned by Setup once the admin exists.
	ErrAlreadySetUp = errors.New("admin: the admin already exists")

	// ErrNotSetUp is returned before Setup has created the admin.
	ErrNotSetUp = errors.New("admin: there is no admin yet")

	// ErrWrongCode is returned for a setup or recovery code that does not match or was used.
	ErrWrongCode = errors.New("admin: the code is not right")

	// ErrWrongCredentials is returned for a wrong username or password, without saying which.
	ErrWrongCredentials = errors.New("admin: the username or password is not right")

	// ErrPasswordOff is returned for any password while password sign-in is turned off.
	ErrPasswordOff = errors.New("admin: the password is turned off")

	// ErrLastPasskey is returned for removing the only way in.
	ErrLastPasskey = errors.New("admin: the last passkey cannot go while the password is off")

	// ErrNoPasskey is returned for turning the password off with no passkey registered.
	ErrNoPasskey = errors.New("admin: the password cannot be turned off without a passkey")

	// ErrTooManyPasskeys is returned for adding a passkey beyond maximumPasskeys.
	ErrTooManyPasskeys = errors.New("admin: too many passkeys")

	// ErrInvalidUsername is returned for a username outside maximumUsernameLength.
	ErrInvalidUsername = errors.New("admin: the username must be 1 to 64 characters")

	// ErrInvalidPassword is returned for a password outside password's length limits.
	ErrInvalidPassword = errors.New("admin: the password must be 8 to 1024 characters")

	// ErrBreachedPassword is returned for a new password found in a data breach.
	ErrBreachedPassword = errors.New("admin: the password has appeared in a data breach")

	// ErrSessionExpired is returned for a session or challenge that is unknown, used or past its lifetime.
	ErrSessionExpired = errors.New("admin: the session has ended")

	// ErrUnknownPasskey is returned for using a passkey the account does not hold.
	ErrUnknownPasskey = errors.New("admin: the passkey is not registered")

	// ErrSignCountReused is returned for a signature count that did not advance past the stored one.
	ErrSignCountReused = errors.New("admin: the passkey's signature count did not advance")
)

// Account is the admin, stored as admin/admin.
type Account struct {
	// Username is the name the admin signs in with, compared after trimming spaces.
	Username string `json:"username"`

	// UserHandle is the random WebAuthn user handle the admin's passkeys carry.
	UserHandle string `json:"userHandle"`

	// PasswordHash is the stored value from password.Hash.
	PasswordHash string `json:"passwordHash"`

	// PasswordEnabled reports whether the password signs in; it goes off only while a passkey exists.
	PasswordEnabled bool `json:"passwordEnabled"`

	// PasswordBreached flags a password found in a breach at sign-in, until it is replaced.
	PasswordBreached bool `json:"passwordBreached,omitempty"`

	// Passkeys lists the registered passkeys, never null in JSON.
	Passkeys []Passkey `json:"passkeys"`

	// UsedRecoveryCodes holds the SHA-256 of each configured recovery code already used.
	UsedRecoveryCodes []string `json:"usedRecoveryCodes,omitempty"`

	// CreatedAt is when setup ran, to the second.
	CreatedAt time.Time `json:"createdAt"`
}

// Passkey is one registered passkey.
type Passkey struct {
	// ID is the credential identifier in unpadded base64url.
	ID string `json:"id"`

	// Name is the admin's label for the passkey, trimmed by cleanName.
	Name string `json:"name"`

	// Algorithm is the passkey's COSE algorithm.
	Algorithm int `json:"algorithm"`

	// PublicKey is the passkey's public key as PKIX DER.
	PublicKey []byte `json:"publicKey"`

	// SignCount is the last signature count seen.
	SignCount uint32 `json:"signCount"`

	// CreatedAt is when the passkey was registered, to the second.
	CreatedAt time.Time `json:"createdAt"`

	// LastUsedAt is when the passkey last signed in, or zero if never.
	LastUsedAt time.Time `json:"lastUsedAt,omitzero"`
}

// Session is one sign-in, stored under the hash of its token so the store never holds a usable token.
type Session struct {
	// CSRF is the token every POST in the session must echo.
	CSRF string `json:"csrf"`

	// CreatedAt is when the admin signed in.
	CreatedAt time.Time `json:"createdAt"`

	// ExpiresAt is when the session ends, SessionLifetime after CreatedAt.
	ExpiresAt time.Time `json:"expiresAt"`
}

// challenge is a passkey ceremony in progress.
type challenge struct {
	// Challenge is the random value the passkey signs.
	Challenge []byte `json:"challenge"`

	// Purpose names the ceremony, so a challenge for one cannot answer another.
	Purpose string `json:"purpose"`

	// ExpiresAt is when the ceremony stops being answerable.
	ExpiresAt time.Time `json:"expiresAt"`
}

// recoveryFile is the contents of RecoveryFileName.
type recoveryFile struct {
	// Hash is the hex SHA-256 of the recovery code.
	Hash string `json:"hash"`

	// ExpiresAt is when the code stops working.
	ExpiresAt time.Time `json:"expiresAt"`
}

// Admin manages the account over the board's store.
type Admin struct {
	// Store holds the account, sessions and challenges.
	Store store.Store

	// Pepper is the server's password pepper; empty hashes without one.
	Pepper []byte

	// Now returns the current time.
	Now func() time.Time

	// SetupCode creates the admin on a new board. It does nothing once the admin exists.
	SetupCode string

	// RecoveryCode, from the configuration of a host with no shell, recovers the admin once.
	RecoveryCode string

	// DataDirectory is where reset-admin leaves a recovery file, on hosts that have one.
	DataDirectory string

	// Breached reports whether a password has appeared in a data breach. Nil or an error skips the
	// check, so a breach list that cannot be reached never stops the admin.
	Breached func(ctx context.Context, password string) (bool, error)
}

// Account returns the admin, or ErrNotSetUp.
func (a *Admin) Account(ctx context.Context) (Account, error) {
	document, err := a.Store.Get(ctx, accountCollection, accountID)
	if errors.Is(err, store.ErrNotFound) {
		return Account{}, ErrNotSetUp
	}
	if err != nil {
		return Account{}, err
	}
	var account Account
	err = json.Unmarshal(document.Data, &account)
	return account, err
}

// IsSetUp reports whether the admin exists.
func (a *Admin) IsSetUp(ctx context.Context) (bool, error) {
	_, err := a.Account(ctx)
	if errors.Is(err, ErrNotSetUp) {
		return false, nil
	}
	return err == nil, err
}

// Setup creates the admin, given the setup code.
func (a *Admin) Setup(ctx context.Context, code, username, newPassword string) error {
	if a.SetupCode == "" || !equal(code, a.SetupCode) {
		return ErrWrongCode
	}
	username = strings.TrimSpace(username)
	if err := validate(username, newPassword); err != nil {
		return err
	}
	if err := a.checkBreach(ctx, newPassword); err != nil {
		return err
	}
	hash, err := password.Hash(newPassword, a.Pepper)
	if err != nil {
		return err
	}
	handle, err := randomString(identifierLength)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(Account{
		Username:        username,
		UserHandle:      handle,
		PasswordHash:    hash,
		PasswordEnabled: true,
		Passkeys:        []Passkey{},
		CreatedAt:       a.Now().UTC().Truncate(time.Second),
	})
	if err != nil {
		return err
	}
	_, err = a.Store.Put(ctx, accountCollection, accountID, encoded, 0)
	if errors.Is(err, store.ErrConflict) {
		return ErrAlreadySetUp
	}
	return err
}

// SignIn checks a username and password. It always runs PBKDF2, so a wrong username takes as long as a
// wrong password, and with the password off it answers ErrPasswordOff whether or not the guess was right.
func (a *Admin) SignIn(ctx context.Context, username, givenPassword string) error {
	account, err := a.Account(ctx)
	if err != nil {
		return err
	}
	matches, err := password.Verify(account.PasswordHash, givenPassword, a.Pepper)
	if !account.PasswordEnabled {
		return ErrPasswordOff
	}
	if err != nil {
		return err
	}
	if !matches || !equal(strings.TrimSpace(username), account.Username) {
		return ErrWrongCredentials
	}
	if !account.PasswordBreached && a.isBreached(ctx, givenPassword) {
		return a.update(ctx, func(account *Account) error {
			account.PasswordBreached = true
			return nil
		})
	}
	return nil
}

// ChangePassword replaces the password, given the current one, and ends every session but keepToken's.
func (a *Admin) ChangePassword(ctx context.Context, keepToken, current, newPassword string) error {
	if err := validate("admin", newPassword); err != nil {
		return err
	}
	if err := a.checkBreach(ctx, newPassword); err != nil {
		return err
	}
	err := a.update(ctx, func(account *Account) error {
		matches, err := password.Verify(account.PasswordHash, current, a.Pepper)
		if err != nil {
			return err
		}
		if !matches {
			return ErrWrongCredentials
		}
		account.PasswordHash, err = password.Hash(newPassword, a.Pepper)
		account.PasswordBreached = false
		return err
	})
	if err != nil {
		return err
	}
	return a.endOtherSessions(ctx, keepToken)
}

// SetPasswordEnabled turns the password on or off. It only goes off while a passkey exists.
func (a *Admin) SetPasswordEnabled(ctx context.Context, enabled bool) error {
	return a.update(ctx, func(account *Account) error {
		if !enabled && len(account.Passkeys) == 0 {
			return ErrNoPasskey
		}
		account.PasswordEnabled = enabled
		if !enabled {
			account.PasswordBreached = false
		}
		return nil
	})
}

// AddPasskey registers a passkey.
func (a *Admin) AddPasskey(ctx context.Context, passkey Passkey) error {
	passkey.Name = cleanName(passkey.Name)
	passkey.CreatedAt = a.Now().UTC().Truncate(time.Second)
	return a.update(ctx, func(account *Account) error {
		if len(account.Passkeys) >= maximumPasskeys {
			return ErrTooManyPasskeys
		}
		if slices.ContainsFunc(account.Passkeys, func(existing Passkey) bool { return existing.ID == passkey.ID }) {
			return nil
		}
		account.Passkeys = append(account.Passkeys, passkey)
		return nil
	})
}

// RenamePasskey changes a passkey's name.
func (a *Admin) RenamePasskey(ctx context.Context, id, name string) error {
	return a.update(ctx, func(account *Account) error {
		for index := range account.Passkeys {
			if account.Passkeys[index].ID == id {
				account.Passkeys[index].Name = cleanName(name)
			}
		}
		return nil
	})
}

// RemovePasskey removes a passkey, unless it is the last way in, and ends every session but keepToken's.
func (a *Admin) RemovePasskey(ctx context.Context, keepToken, id string) error {
	removed := false
	err := a.update(ctx, func(account *Account) error {
		remaining := slices.DeleteFunc(slices.Clone(account.Passkeys), func(passkey Passkey) bool {
			return passkey.ID == id
		})
		if len(remaining) == 0 && !account.PasswordEnabled {
			return ErrLastPasskey
		}
		removed = len(remaining) != len(account.Passkeys)
		account.Passkeys = remaining
		return nil
	})
	if err != nil || !removed {
		return err
	}
	return a.endOtherSessions(ctx, keepToken)
}

// UsePasskey records a passkey's new signature count after a sign-in, checking the count in the same
// write so two assertions carrying one count cannot both pass.
func (a *Admin) UsePasskey(ctx context.Context, id string, signCount uint32) error {
	return a.update(ctx, func(account *Account) error {
		index := slices.IndexFunc(account.Passkeys, func(passkey Passkey) bool { return passkey.ID == id })
		if index < 0 {
			return ErrUnknownPasskey
		}
		if !webauthn.SignCountAdvances(account.Passkeys[index].SignCount, signCount) {
			return ErrSignCountReused
		}
		account.Passkeys[index].SignCount = signCount
		account.Passkeys[index].LastUsedAt = a.Now().UTC().Truncate(time.Second)
		return nil
	})
}

// Recover sets a new password and removes every passkey, given a recovery code: one from the host's
// configuration, used once, or one reset-admin left in the data directory, used within its lifetime.
func (a *Admin) Recover(ctx context.Context, code, newPassword string) error {
	if err := validate("admin", newPassword); err != nil {
		return err
	}
	fromFile, err := a.matchesRecoveryFile(code)
	if err != nil {
		return err
	}
	// The breach check calls out to the internet, so only a code that could be right reaches it.
	if !fromFile && (a.RecoveryCode == "" || !equal(code, a.RecoveryCode)) {
		return ErrWrongCode
	}
	if err := a.checkBreach(ctx, newPassword); err != nil {
		return err
	}
	// The file is removed before it is used, so its code works once, even for two requests at a time.
	if fromFile {
		err := os.Remove(filepath.Join(a.DataDirectory, RecoveryFileName))
		if errors.Is(err, os.ErrNotExist) {
			return ErrWrongCode
		}
		if err != nil {
			return fmt.Errorf("admin: removing the recovery file: %w", err)
		}
	}
	codeHash := hashCode(code)
	err = a.update(ctx, func(account *Account) error {
		fromConfiguration := a.RecoveryCode != "" && equal(code, a.RecoveryCode) &&
			!slices.Contains(account.UsedRecoveryCodes, codeHash)
		if !fromFile && !fromConfiguration {
			return ErrWrongCode
		}
		hash, err := password.Hash(newPassword, a.Pepper)
		if err != nil {
			return err
		}
		account.PasswordHash = hash
		account.PasswordEnabled = true
		account.PasswordBreached = false
		account.Passkeys = []Passkey{}
		if fromConfiguration {
			account.UsedRecoveryCodes = append(account.UsedRecoveryCodes, codeHash)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return a.endAllSessions(ctx)
}

// WriteRecoveryFile makes a recovery code for reset-admin, leaves its hash in dataDirectory for the
// running server, and returns the code.
func WriteRecoveryFile(dataDirectory string, now time.Time) (string, error) {
	code, err := randomString(recoveryCodeLength)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(recoveryFile{Hash: hashCode(code), ExpiresAt: now.Add(RecoveryLifetime).UTC()})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dataDirectory, RecoveryFileName), encoded, 0o600); err != nil {
		return "", fmt.Errorf("admin: writing the recovery file: %w", err)
	}
	return code, nil
}

// matchesRecoveryFile reports whether code matches an unexpired recovery file from reset-admin.
func (a *Admin) matchesRecoveryFile(code string) (bool, error) {
	if a.DataDirectory == "" {
		return false, nil
	}
	data, err := os.ReadFile(filepath.Join(a.DataDirectory, RecoveryFileName))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var file recoveryFile
	if err := json.Unmarshal(data, &file); err != nil {
		return false, nil
	}
	return a.Now().Before(file.ExpiresAt) && equal(hashCode(code), file.Hash), nil
}

// NewSession starts a sign-in and returns its token, which only the browser's cookie holds.
func (a *Admin) NewSession(ctx context.Context) (string, Session, error) {
	token, err := randomString(tokenLength)
	if err != nil {
		return "", Session{}, err
	}
	csrf, err := randomString(tokenLength)
	if err != nil {
		return "", Session{}, err
	}
	now := a.Now().UTC()
	session := Session{CSRF: csrf, CreatedAt: now, ExpiresAt: now.Add(SessionLifetime)}
	encoded, err := json.Marshal(session)
	if err != nil {
		return "", Session{}, err
	}
	if _, err := a.Store.Put(ctx, sessionsCollection, tokenKey(token), encoded, 0); err != nil {
		return "", Session{}, err
	}
	return token, session, nil
}

// Session returns a live session for a token, or ErrSessionExpired.
func (a *Admin) Session(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrSessionExpired
	}
	document, err := a.Store.Get(ctx, sessionsCollection, tokenKey(token))
	if errors.Is(err, store.ErrNotFound) {
		return Session{}, ErrSessionExpired
	}
	if err != nil {
		return Session{}, err
	}
	var session Session
	if err := json.Unmarshal(document.Data, &session); err != nil {
		return Session{}, err
	}
	if !a.Now().Before(session.ExpiresAt) {
		return Session{}, ErrSessionExpired
	}
	return session, nil
}

// EndSession signs out.
func (a *Admin) EndSession(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return a.Store.Delete(ctx, sessionsCollection, tokenKey(token), store.AnyVersion)
}

// NewChallenge starts a passkey ceremony and returns the identifier its cookie holds and the challenge.
func (a *Admin) NewChallenge(ctx context.Context, purpose string) (string, []byte, error) {
	id, err := randomString(identifierLength)
	if err != nil {
		return "", nil, err
	}
	value := make([]byte, tokenLength)
	if _, err := rand.Read(value); err != nil {
		return "", nil, err
	}
	expiresAt := a.Now().Add(ChallengeLifetime).UTC()
	encoded, err := json.Marshal(challenge{Challenge: value, Purpose: purpose, ExpiresAt: expiresAt})
	if err != nil {
		return "", nil, err
	}
	if _, err := a.Store.Put(ctx, challengeCollection, id, encoded, 0); err != nil {
		return "", nil, err
	}
	return id, value, nil
}

// TakeChallenge returns a ceremony's challenge and deletes it, so it can be answered only once.
func (a *Admin) TakeChallenge(ctx context.Context, id, purpose string) ([]byte, error) {
	if store.ValidateID(id) != nil {
		return nil, ErrSessionExpired
	}
	document, err := a.Store.Get(ctx, challengeCollection, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrSessionExpired
	}
	if err != nil {
		return nil, err
	}
	if err := a.Store.Delete(ctx, challengeCollection, id, document.Version); err != nil {
		return nil, ErrSessionExpired
	}
	var stored challenge
	if err := json.Unmarshal(document.Data, &stored); err != nil {
		return nil, err
	}
	if stored.Purpose != purpose || !a.Now().Before(stored.ExpiresAt) {
		return nil, ErrSessionExpired
	}
	return stored.Challenge, nil
}

// Sweep deletes ended sessions and abandoned ceremonies.
func (a *Admin) Sweep(ctx context.Context) error {
	now := a.Now()
	for _, collection := range []string{sessionsCollection, challengeCollection} {
		items, err := a.Store.List(ctx, collection)
		if err != nil {
			return err
		}
		for _, item := range items {
			var expiry struct {
				ExpiresAt time.Time `json:"expiresAt"`
			}
			if json.Unmarshal(item.Data, &expiry) != nil || now.Before(expiry.ExpiresAt) {
				continue
			}
			err := a.Store.Delete(ctx, collection, item.ID, item.Version)
			if err != nil && !errors.Is(err, store.ErrConflict) {
				return err
			}
		}
	}
	return nil
}

// endAllSessions signs out every browser.
func (a *Admin) endAllSessions(ctx context.Context) error {
	return a.Store.DeleteCollection(ctx, sessionsCollection)
}

// endOtherSessions signs out every browser but the one holding keepToken.
func (a *Admin) endOtherSessions(ctx context.Context, keepToken string) error {
	items, err := a.Store.List(ctx, sessionsCollection)
	if err != nil {
		return err
	}
	kept := tokenKey(keepToken)
	for _, item := range items {
		if item.ID == kept {
			continue
		}
		err := a.Store.Delete(ctx, sessionsCollection, item.ID, store.AnyVersion)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

// update applies change to the stored account, retrying when another write got there first.
func (a *Admin) update(ctx context.Context, change func(*Account) error) error {
	return store.Update(ctx, a.Store, accountCollection, accountID, func(data []byte) ([]byte, error) {
		if data == nil {
			return nil, ErrNotSetUp
		}
		var account Account
		if err := json.Unmarshal(data, &account); err != nil {
			return nil, err
		}
		if err := change(&account); err != nil {
			return nil, err
		}
		return json.Marshal(account)
	})
}

// validate checks a username's and a new password's lengths.
func validate(username, newPassword string) error {
	if length := utf8.RuneCountInString(username); length == 0 || length > maximumUsernameLength {
		return ErrInvalidUsername
	}
	if length := password.Length(newPassword); length < password.MinimumLength || length > password.MaximumLength {
		return ErrInvalidPassword
	}
	return nil
}

// checkBreach refuses a new password that has appeared in a breach.
func (a *Admin) checkBreach(ctx context.Context, newPassword string) error {
	if a.isBreached(ctx, newPassword) {
		return ErrBreachedPassword
	}
	return nil
}

// isBreached reports whether a password is known to be breached, reading a failed check as not.
func (a *Admin) isBreached(ctx context.Context, candidate string) bool {
	if a.Breached == nil {
		return false
	}
	breached, err := a.Breached(ctx, candidate)
	return err == nil && breached
}

// cleanName trims a passkey name to maximumPasskeyName code points, defaulting to "Passkey".
func cleanName(name string) string {
	name = strings.TrimSpace(name)
	if runes := []rune(name); len(runes) > maximumPasskeyName {
		name = string(runes[:maximumPasskeyName])
	}
	if name == "" {
		name = "Passkey"
	}
	return name
}

// equal compares two strings in constant time.
func equal(first, second string) bool {
	return subtle.ConstantTimeCompare([]byte(first), []byte(second)) == 1
}

// hashCode returns a recovery code's SHA-256 in hex.
func hashCode(code string) string {
	digest := sha256.Sum256([]byte(code))
	return hex.EncodeToString(digest[:])
}

// tokenKey returns the document identifier of a session token, so the store never holds the token.
func tokenKey(token string) string {
	digest := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(digest[:tokenKeyLength])
}

// randomString returns byteCount random bytes in unpadded base64url.
func randomString(byteCount int) (string, error) {
	random := make([]byte, byteCount)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}
