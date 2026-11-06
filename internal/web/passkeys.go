package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/admin"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/webauthn"
)

const (
	// purposeSignIn marks a challenge issued for signing in.
	purposeSignIn = "signIn"

	// purposeRegister marks a challenge issued for adding a passkey.
	purposeRegister = "register"

	// ceremonyTimeout is the browser's time for a ceremony, in milliseconds.
	ceremonyTimeout = int64(admin.ChallengeLifetime / time.Millisecond)
)

// encoding is WebAuthn's base64url without padding.
var encoding = base64.RawURLEncoding

// A descriptor names a credential the browser should not register again.
type descriptor struct {
	// Type is always "public-key".
	Type string `json:"type"`

	// ID is the credential's identifier, in unpadded base64url.
	ID string `json:"id"`
}

// beginPasskeySignIn starts a passkey sign-in and returns the browser's request options.
func (s *Site) beginPasskeySignIn(writer http.ResponseWriter, request *http.Request) {
	id, challenge, err := s.Admin.NewChallenge(request.Context(), purposeSignIn)
	if err != nil {
		s.failJSON(writer, err)
		return
	}
	s.setCookie(writer, ceremonyCookie, id, admin.ChallengeLifetime)
	writeJSONStatus(writer, http.StatusOK, map[string]any{
		"challenge": encoding.EncodeToString(challenge),
		"rpId":      s.relyingParty().ID,
		"timeout":   ceremonyTimeout,
	})
}

// finishPasskeySignIn verifies a passkey's assertion and starts a session.
func (s *Site) finishPasskeySignIn(writer http.ResponseWriter, request *http.Request) {
	var answer struct {
		ID                string `json:"id"`
		ClientDataJSON    string `json:"clientDataJSON"`
		AuthenticatorData string `json:"authenticatorData"`
		Signature         string `json:"signature"`
	}
	if !readJSON(writer, request, &answer) {
		return
	}
	challenge, ok := s.takeChallenge(writer, request, purposeSignIn)
	if !ok {
		return
	}
	account, err := s.Admin.Account(request.Context())
	if err != nil {
		s.failJSON(writer, err)
		return
	}
	var passkey *admin.Passkey
	for index := range account.Passkeys {
		if account.Passkeys[index].ID == answer.ID {
			passkey = &account.Passkeys[index]
		}
	}
	clientData, clientErr := encoding.DecodeString(answer.ClientDataJSON)
	authenticatorData, dataErr := encoding.DecodeString(answer.AuthenticatorData)
	signature, signatureErr := encoding.DecodeString(answer.Signature)
	credentialID, idErr := encoding.DecodeString(answer.ID)
	if passkey == nil || errors.Join(clientErr, dataErr, signatureErr, idErr) != nil {
		writeJSONStatus(writer, http.StatusUnauthorized, map[string]string{"error": "unknown passkey"})
		return
	}
	credential := webauthn.Credential{ID: credentialID, Algorithm: passkey.Algorithm, PublicKey: passkey.PublicKey, SignCount: passkey.SignCount}
	signCount, err := s.relyingParty().VerifyAssertion(challenge, credential, clientData, authenticatorData, signature)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("a passkey sign-in failed", "error", err)
		}
		writeJSONStatus(writer, http.StatusUnauthorized, map[string]string{"error": "the passkey did not verify"})
		return
	}
	// The count is checked again inside the write, so a cloned passkey racing this one cannot also pass.
	switch err := s.Admin.UsePasskey(request.Context(), passkey.ID, signCount); {
	case errors.Is(err, admin.ErrSignCountReused), errors.Is(err, admin.ErrUnknownPasskey):
		if s.Logger != nil {
			s.Logger.Warn("a passkey sign-in failed", "error", err)
		}
		writeJSONStatus(writer, http.StatusUnauthorized, map[string]string{"error": "the passkey did not verify"})
		return
	case err != nil:
		s.failJSON(writer, err)
		return
	}
	token, _, err := s.Admin.NewSession(request.Context())
	if err != nil {
		s.failJSON(writer, err)
		return
	}
	s.setCookie(writer, sessionCookie, token, admin.SessionLifetime)
	writeJSONStatus(writer, http.StatusOK, map[string]string{"redirect": s.basePath() + "/admin/board"})
}

// beginPasskeyRegistration starts adding a passkey and returns the browser's creation options.
func (s *Site) beginPasskeyRegistration(writer http.ResponseWriter, request *http.Request, _ admin.Session) {
	account, err := s.Admin.Account(request.Context())
	if err != nil {
		s.failJSON(writer, err)
		return
	}
	id, challenge, err := s.Admin.NewChallenge(request.Context(), purposeRegister)
	if err != nil {
		s.failJSON(writer, err)
		return
	}
	s.setCookie(writer, ceremonyCookie, id, admin.ChallengeLifetime)
	excluded := []descriptor{}
	for _, passkey := range account.Passkeys {
		excluded = append(excluded, descriptor{Type: "public-key", ID: passkey.ID})
	}
	parameters := []map[string]any{}
	for _, algorithm := range webauthn.SupportedAlgorithms {
		parameters = append(parameters, map[string]any{"type": "public-key", "alg": algorithm})
	}
	board, _ := s.Service.Board(request.Context())
	writeJSONStatus(writer, http.StatusOK, map[string]any{
		"challenge":          encoding.EncodeToString(challenge),
		"rp":                 map[string]string{"id": s.relyingParty().ID, "name": board.Name},
		"user":               map[string]string{"id": account.UserHandle, "name": account.Username, "displayName": account.Username},
		"pubKeyCredParams":   parameters,
		"excludeCredentials": excluded,
		"authenticatorSelection": map[string]string{
			"residentKey":      "required",
			"userVerification": "required",
		},
		"timeout": ceremonyTimeout,
	})
}

// finishPasskeyRegistration verifies a new passkey and adds it to the admin's account.
func (s *Site) finishPasskeyRegistration(writer http.ResponseWriter, request *http.Request, _ admin.Session) {
	var answer struct {
		ClientDataJSON    string `json:"clientDataJSON"`
		AttestationObject string `json:"attestationObject"`
		Name              string `json:"name"`
	}
	if !readJSON(writer, request, &answer) {
		return
	}
	challenge, ok := s.takeChallenge(writer, request, purposeRegister)
	if !ok {
		return
	}
	clientData, clientErr := encoding.DecodeString(answer.ClientDataJSON)
	attestation, attestationErr := encoding.DecodeString(answer.AttestationObject)
	if clientErr != nil || attestationErr != nil {
		writeJSONStatus(writer, http.StatusBadRequest, map[string]string{"error": "malformed response"})
		return
	}
	credential, err := s.relyingParty().VerifyRegistration(challenge, clientData, attestation)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("a passkey registration failed", "error", err)
		}
		writeJSONStatus(writer, http.StatusBadRequest, map[string]string{"error": "the passkey did not verify"})
		return
	}
	err = s.Admin.AddPasskey(request.Context(), admin.Passkey{
		ID:        encoding.EncodeToString(credential.ID),
		Name:      answer.Name,
		Algorithm: credential.Algorithm,
		PublicKey: credential.PublicKey,
		SignCount: credential.SignCount,
	})
	if err != nil {
		s.failJSON(writer, err)
		return
	}
	writeJSONStatus(writer, http.StatusOK, map[string]string{"redirect": s.basePath() + "/admin/account?done=passkeyAdded"})
}

// takeChallenge spends the ceremony cookie's challenge, which must be for purpose, and clears the cookie.
func (s *Site) takeChallenge(writer http.ResponseWriter, request *http.Request, purpose string) ([]byte, bool) {
	cookie, err := request.Cookie(ceremonyCookie)
	if err != nil {
		writeJSONStatus(writer, http.StatusBadRequest, map[string]string{"error": "no ceremony"})
		return nil, false
	}
	s.setCookie(writer, ceremonyCookie, "", 0)
	challenge, err := s.Admin.TakeChallenge(request.Context(), cookie.Value, purpose)
	if err != nil {
		writeJSONStatus(writer, http.StatusBadRequest, map[string]string{"error": "the ceremony has ended"})
		return nil, false
	}
	return challenge, true
}

// readJSON decodes a JSON body into value, answering 400 itself when it cannot.
func readJSON(writer http.ResponseWriter, request *http.Request, value any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, formBodyLimit)
	if err := json.NewDecoder(request.Body).Decode(value); err != nil {
		writeJSONStatus(writer, http.StatusBadRequest, map[string]string{"error": "malformed request"})
		return false
	}
	return true
}

// failJSON logs err and answers a JSON server error.
func (s *Site) failJSON(writer http.ResponseWriter, err error) {
	if s.Logger != nil {
		s.Logger.Error("passkey request failed", "error", err)
	}
	writeJSONStatus(writer, http.StatusInternalServerError, map[string]string{"error": "server error"})
}

// writeJSONStatus answers status with value as uncached JSON.
func writeJSONStatus(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	json.NewEncoder(writer).Encode(value)
}
