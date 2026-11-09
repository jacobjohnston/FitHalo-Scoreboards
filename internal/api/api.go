// Package api serves openapi.yaml: routing, request signatures, body limits, ETags and problem details.
package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/auth"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/problem"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/ratelimit"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
)

// APIVersion is the version of openapi.yaml this server implements.
const APIVersion = "1.1"

// Body limits, from openapi.yaml's Limits table where it gives one.
const (
	// smallBodyLimit is for every operation the table does not list.
	smallBodyLimit = 16 << 10

	// weeklyChallengeBodyLimit is a weekly challenge body's 384 KB.
	weeklyChallengeBodyLimit = 384 << 10

	// routeBodyLimit is a route's 256 KB.
	routeBodyLimit = 256 << 10

	// avatarBodyLimit is an avatar upload's 256 KB.
	avatarBodyLimit = 256 << 10

	// boardAvatarCacheControl lets shared caches keep the public board avatar for five minutes.
	boardAvatarCacheControl = "public, max-age=300"
)

// etagLength is how many bytes of a body's SHA-256 digest its ETag carries.
const etagLength = 12

// signatureFailure is the detail for a bad signature and for an unknown member alike, so neither tells
// whether a key is a member.
const signatureFailure = "the signature does not verify"

// Server handles the client API.
type Server struct {
	// Service holds the board's rules and its store.
	Service *scoreboard.Service

	// Nonces refuses a signature's nonce the second time it is used.
	Nonces Nonces

	// Limiter rate-limits every operation but joining, by client address; nil means no limit.
	Limiter *ratelimit.Limiter

	// JoinLimiter rate-limits joining, by client address; nil means no limit.
	JoinLimiter *ratelimit.Limiter

	// Logger records each request; nil logs nothing.
	Logger *slog.Logger

	// ServerVersion is the build's version, reported on the board.
	ServerVersion string

	// ClientAddress returns the address a request came from, for rate limits. It defaults to the
	// connection's address; behind a proxy it reads the proxy's header.
	ClientAddress func(*http.Request) string
}

// Nonces remembers which nonces each member has used. auth.NonceCache does it in memory for a single
// instance; nonces.Store does it in the store for many.
type Nonces interface {
	// Use records member's nonce and reports whether it was new.
	Use(member, nonce string, now time.Time) bool
}

// memberKey is the context key for a request's signedRequest.
type memberKey struct{}

// Handler returns the API's handler, serving under basePath, which is empty or starts with a slash.
func (s *Server) Handler(basePath string) http.Handler {
	if s.ClientAddress == nil {
		s.ClientAddress = connectionAddress
	}
	prefix := strings.TrimSuffix(basePath, "/") + "/api/v1"
	mux := http.NewServeMux()
	route := func(pattern string, handler http.HandlerFunc) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.Handle(method+" "+prefix+path, s.limited(s.Limiter, handler))
	}

	route("GET /board", s.getBoard)
	route("GET /board/avatar", s.getBoardAvatar)
	mux.Handle("POST "+prefix+"/members", s.limited(s.JoinLimiter, s.join))
	route("GET /members/me", s.signed(smallBodyLimit, s.getMe))
	route("PATCH /members/me", s.signed(smallBodyLimit, s.updateMe))
	route("DELETE /members/me", s.signed(smallBodyLimit, s.leave))
	route("PUT /members/me/avatar", s.signed(avatarBodyLimit, s.active(s.putAvatar)))
	route("DELETE /members/me/avatar", s.signed(smallBodyLimit, s.deleteAvatar))
	route("PUT /members/me/weekly-challenges/{weekStart}", s.signed(weeklyChallengeBodyLimit, s.active(s.putWeeklyChallenge)))
	route("PUT /members/me/weekly-challenges/{weekStart}/activities/{activityID}/route", s.signed(routeBodyLimit, s.active(s.putRoute)))
	route("DELETE /members/me/weekly-challenges/{weekStart}/activities/{activityID}/route", s.signed(smallBodyLimit, s.deleteRoute))
	route("GET /member-requests", s.signed(smallBodyLimit, s.approver(s.getMemberRequests)))
	route("POST /member-requests/{memberID}/approve", s.signed(smallBodyLimit, s.approver(s.approveMemberRequest)))
	route("DELETE /member-requests/{memberID}", s.signed(smallBodyLimit, s.approver(s.rejectMemberRequest)))
	route("GET /weekly-challenges/{weekStart}", s.signed(smallBodyLimit, s.active(s.getScoreboard)))
	route("GET /members/{memberID}/weekly-challenges/{weekStart}", s.signed(smallBodyLimit, s.active(s.getMemberWeeklyChallenge)))
	route("GET /members/{memberID}/avatar", s.signed(smallBodyLimit, s.active(s.getAvatar)))
	route("GET /members/{memberID}/weekly-challenges/{weekStart}/activities/{activityID}/route", s.signed(smallBodyLimit, s.active(s.getRoute)))
	mux.Handle(prefix+"/", s.limited(s.Limiter, func(writer http.ResponseWriter, _ *http.Request) {
		problem.Write(writer, problem.New(problem.UnknownOperation, ""))
	}))
	return s.logged(mux)
}

// getBoard serves the board's public description, which needs no signature.
func (s *Server) getBoard(writer http.ResponseWriter, request *http.Request) {
	board, err := s.Service.Board(request.Context())
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	accepting, err := s.Service.AcceptingRequests(request.Context(), board)
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	s.writeJSON(writer, request, http.StatusOK, boardResponse{
		AcceptingRequests: accepting,
		ID:                board.ID,
		Name:              board.Name,
		Description:       board.Description,
		HasAvatar:         board.HasAvatar,
		WeekStartDay:      board.WeekStartDay,
		RetentionWeeks:    board.RetentionWeeks,
		JoinMode:          board.JoinMode,
		PasswordRequired:  board.PasswordHash != "",
		AvatarsAllowed:    board.AvatarsAllowed,
		MaximumSharing:    board.MaximumSharing,
		Ranking:           board.Ranking,
		APIVersion:        APIVersion,
		ServerVersion:     s.ServerVersion,
	}, true)
}

// getBoardAvatar serves the board's own avatar as JPEG, which needs no signature.
func (s *Server) getBoardAvatar(writer http.ResponseWriter, request *http.Request) {
	image, err := s.Service.BoardAvatar(request.Context())
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	// The image is public, so a proxy or CDN in front may answer repeats for a few minutes.
	s.writeCached(writer, request, http.StatusOK, "image/jpeg", image, boardAvatarCacheControl)
}

// join makes a key's holder a member, or returns their membership. It is signed with the key in its
// own body, so it verifies the signature itself.
func (s *Server) join(writer http.ResponseWriter, request *http.Request) {
	body, failure := readBody(writer, request, smallBodyLimit, "application/json")
	if failure != nil {
		problem.Write(writer, failure)
		return
	}
	var joinRequest scoreboard.JoinRequest
	if err := decodeJSON(body, &joinRequest); err != nil {
		problem.Write(writer, problem.New(problem.BadRequest, err.Error()))
		return
	}
	key, rawKey, err := auth.ParsePublicKey(joinRequest.PublicKey)
	if err != nil {
		problem.Write(writer, problem.New(problem.BadRequest, err.Error()))
		return
	}
	memberID := auth.MemberID(rawKey)
	if failure := s.verify(request, body, memberID, key); failure != nil {
		problem.Write(writer, failure)
		return
	}
	member, created, err := s.Service.Join(request.Context(), joinRequest, memberID)
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeMe(writer, request, status, member)
}

// getMe serves the signing member.
func (s *Server) getMe(writer http.ResponseWriter, request *http.Request) {
	s.writeMe(writer, request, http.StatusOK, memberFrom(request))
}

// updateMe changes the signing member's display name or sharing.
func (s *Server) updateMe(writer http.ResponseWriter, request *http.Request) {
	if failure := requireContentType(request, "application/json"); failure != nil {
		problem.Write(writer, failure)
		return
	}
	var update scoreboard.MemberUpdate
	if err := decodeJSON(bodyFrom(request), &update); err != nil {
		problem.Write(writer, problem.New(problem.BadRequest, err.Error()))
		return
	}
	member, err := s.Service.UpdateMember(request.Context(), memberFrom(request).ID, update)
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	s.writeMe(writer, request, http.StatusOK, member)
}

// leave deletes the signing member and everything they stored.
func (s *Server) leave(writer http.ResponseWriter, request *http.Request) {
	if err := s.Service.Leave(request.Context(), memberFrom(request).ID); err != nil {
		s.fail(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// putAvatar stores the signing member's avatar from a JPEG or PNG body.
func (s *Server) putAvatar(writer http.ResponseWriter, request *http.Request) {
	contentType, _, _ := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if contentType != "image/jpeg" && contentType != "image/png" {
		problem.Write(writer, problem.New(problem.UnsupportedMediaType, "an avatar is image/jpeg or image/png"))
		return
	}
	if err := s.Service.SetAvatar(request.Context(), memberFrom(request), bodyFrom(request), contentType); err != nil {
		s.fail(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// deleteAvatar removes the signing member's avatar.
func (s *Server) deleteAvatar(writer http.ResponseWriter, request *http.Request) {
	if err := s.Service.DeleteAvatar(request.Context(), memberFrom(request).ID); err != nil {
		s.fail(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// putWeeklyChallenge stores the signing member's weekly challenge and answers with what was kept after stripping.
func (s *Server) putWeeklyChallenge(writer http.ResponseWriter, request *http.Request) {
	if failure := requireContentType(request, "application/json"); failure != nil {
		problem.Write(writer, failure)
		return
	}
	challenge, err := s.Service.PutWeeklyChallenge(request.Context(), memberFrom(request), request.PathValue("weekStart"), bodyFrom(request))
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	s.writeJSON(writer, request, http.StatusOK, challenge, false)
}

// putRoute stores the route of one of the signing member's activities.
func (s *Server) putRoute(writer http.ResponseWriter, request *http.Request) {
	if failure := requireContentType(request, "application/json"); failure != nil {
		problem.Write(writer, failure)
		return
	}
	activityID := request.PathValue("activityID")
	if !scoreboard.IsIdentifier(activityID) {
		problem.Write(writer, problem.New(problem.BadRequest, "the activity is not an identifier"))
		return
	}
	err := s.Service.PutRoute(request.Context(), memberFrom(request), request.PathValue("weekStart"), activityID, bodyFrom(request))
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// deleteRoute removes the route of one of the signing member's activities.
func (s *Server) deleteRoute(writer http.ResponseWriter, request *http.Request) {
	activityID := request.PathValue("activityID")
	if !scoreboard.IsIdentifier(activityID) {
		problem.Write(writer, problem.New(problem.BadRequest, "the activity is not an identifier"))
		return
	}
	if err := s.Service.DeleteRoute(request.Context(), memberFrom(request), request.PathValue("weekStart"), activityID); err != nil {
		s.fail(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// getMemberRequests serves the members waiting for approval.
func (s *Server) getMemberRequests(writer http.ResponseWriter, request *http.Request) {
	result, err := s.Service.PendingRequests(request.Context())
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	s.writeJSON(writer, request, http.StatusOK, result, true)
}

// approveMemberRequest makes a waiting member active.
func (s *Server) approveMemberRequest(writer http.ResponseWriter, request *http.Request) {
	memberID := request.PathValue("memberID")
	if !scoreboard.IsIdentifier(memberID) {
		problem.Write(writer, problem.New(problem.NotFound, "no such request"))
		return
	}
	if err := s.Service.ApproveRequest(request.Context(), memberID); err != nil {
		s.fail(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// rejectMemberRequest deletes a waiting member.
func (s *Server) rejectMemberRequest(writer http.ResponseWriter, request *http.Request) {
	memberID := request.PathValue("memberID")
	if !scoreboard.IsIdentifier(memberID) {
		problem.Write(writer, problem.New(problem.NotFound, "no such request"))
		return
	}
	if err := s.Service.RejectRequest(request.Context(), memberID); err != nil {
		s.fail(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// getScoreboard serves every member's summary for a weekly challenge.
func (s *Server) getScoreboard(writer http.ResponseWriter, request *http.Request) {
	result, err := s.Service.Scoreboard(request.Context(), request.PathValue("weekStart"))
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	s.writeJSON(writer, request, http.StatusOK, result, true)
}

// getMemberWeeklyChallenge serves one member's weekly challenge, as far as their sharing allows.
func (s *Server) getMemberWeeklyChallenge(writer http.ResponseWriter, request *http.Request) {
	memberID := request.PathValue("memberID")
	if !scoreboard.IsIdentifier(memberID) {
		problem.Write(writer, problem.New(problem.NotFound, "no such member"))
		return
	}
	result, err := s.Service.MemberWeeklyChallenge(request.Context(), memberID, request.PathValue("weekStart"))
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	s.writeJSON(writer, request, http.StatusOK, result, true)
}

// getAvatar serves a member's avatar as JPEG.
func (s *Server) getAvatar(writer http.ResponseWriter, request *http.Request) {
	memberID := request.PathValue("memberID")
	if !scoreboard.IsIdentifier(memberID) {
		problem.Write(writer, problem.New(problem.NotFound, "no such member"))
		return
	}
	image, err := s.Service.Avatar(request.Context(), memberID)
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	s.writeBody(writer, request, http.StatusOK, "image/jpeg", image, true)
}

// getRoute serves the route of one of a member's activities.
func (s *Server) getRoute(writer http.ResponseWriter, request *http.Request) {
	memberID, activityID := request.PathValue("memberID"), request.PathValue("activityID")
	if !scoreboard.IsIdentifier(memberID) || !scoreboard.IsIdentifier(activityID) {
		problem.Write(writer, problem.New(problem.NotFound, "no route"))
		return
	}
	route, err := s.Service.Route(request.Context(), memberID, request.PathValue("weekStart"), activityID)
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	s.writeJSON(writer, request, http.StatusOK, route, true)
}

// signed verifies a request's signature, loads its member, and hands the body on with it.
func (s *Server) signed(limit int64, next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, failure := readBody(writer, request, limit, "")
		if failure != nil {
			problem.Write(writer, failure)
			return
		}
		// Not every operation reads the board, so the storage guard runs here for them all.
		if _, err := s.Service.Board(request.Context()); errors.Is(err, scoreboard.ErrStorageNewer) {
			s.fail(writer, request, err)
			return
		}
		header, err := auth.ParseHeader(request.Header.Get("Authorization"))
		if err != nil {
			problem.Write(writer, problem.New(problem.Unauthenticated, err.Error()))
			return
		}
		// The time is checked before the member is looked up, so clockSkew never tells a member from a stranger.
		if !auth.WithinSkew(header.Time, s.Service.Now()) {
			problem.Write(writer, clockSkew())
			return
		}
		member, err := s.Service.Member(request.Context(), header.Member)
		if err != nil {
			if isProblem(err, problem.NotFound) {
				problem.Write(writer, problem.New(problem.Unauthenticated, signatureFailure))
				return
			}
			s.fail(writer, request, err)
			return
		}
		key, _, err := auth.ParsePublicKey(member.PublicKey)
		if err != nil {
			s.fail(writer, request, err)
			return
		}
		if failure := s.verify(request, body, member.ID, key); failure != nil {
			problem.Write(writer, failure)
			return
		}
		ctx := context.WithValue(request.Context(), memberKey{}, signedRequest{member: member, body: body})
		next(writer, request.WithContext(ctx))
	}
}

// verify checks a request's Authorization header against key, in the order the README gives.
func (s *Server) verify(request *http.Request, body []byte, memberID string, key *ecdsa.PublicKey) *problem.Problem {
	header, err := auth.ParseHeader(request.Header.Get("Authorization"))
	if err != nil {
		return problem.New(problem.Unauthenticated, err.Error())
	}
	now := s.Service.Now()
	if !auth.WithinSkew(header.Time, now) {
		return clockSkew()
	}
	if header.Member != memberID {
		return problem.New(problem.Unauthenticated, "the member does not match the key")
	}
	message := auth.Message(request.Method, request.RequestURI, header.Time, header.Nonce, body)
	if !auth.Verify(key, message, header.Signature) {
		return problem.New(problem.Unauthenticated, signatureFailure)
	}
	if !s.Nonces.Use(memberID, header.Nonce, now) {
		return problem.New(problem.Replayed, "")
	}
	return nil
}

// clockSkew is the answer to a request whose time is outside the window.
func clockSkew() *problem.Problem {
	return problem.New(problem.ClockSkew, "the request's time is more than 300 seconds from the server's")
}

// active refuses a pending member.
func (s *Server) active(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if memberFrom(request).Status != scoreboard.StatusActive {
			problem.Write(writer, problem.New(problem.MemberPending, ""))
			return
		}
		next(writer, request)
	}
}

// approver refuses a member who is pending or whom the admin has not allowed to approve requests.
func (s *Server) approver(next http.HandlerFunc) http.HandlerFunc {
	return s.active(func(writer http.ResponseWriter, request *http.Request) {
		if !memberFrom(request).CanApproveMembers {
			problem.Write(writer, problem.New(problem.NotPermitted, ""))
			return
		}
		next(writer, request)
	})
}

// limited applies a rate limit by the client's address; a nil limiter allows everything.
func (s *Server) limited(limiter *ratelimit.Limiter, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if limiter != nil {
			if allowed, retryAfter := limiter.Allow(s.ClientAddress(request), s.Service.Now()); !allowed {
				failure := problem.New(problem.RateLimited, "")
				failure.RetryAfter = retryAfter
				problem.Write(writer, failure)
				return
			}
		}
		next(writer, request)
	})
}

// logged sets nosniff on every response and logs each request's method, route, status and duration,
// and nothing that identifies a member.
func (s *Server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}
		recorder.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(recorder, request)
		if s.Logger != nil {
			s.Logger.Info("request", "method", request.Method, "route", request.Pattern, "status", recorder.status, "duration", time.Since(started).Round(time.Millisecond))
		}
	})
}

// fail sends err as its problem, or logs it and sends unavailable for newer storage and internal otherwise.
func (s *Server) fail(writer http.ResponseWriter, request *http.Request, err error) {
	var failure *problem.Problem
	if errors.As(err, &failure) {
		problem.Write(writer, failure)
		return
	}
	if s.Logger != nil {
		s.Logger.Error("request failed", "route", request.Pattern, "error", err)
	}
	if errors.Is(err, scoreboard.ErrStorageNewer) {
		problem.Write(writer, problem.New(problem.Unavailable, ""))
		return
	}
	problem.Write(writer, problem.New(problem.Internal, ""))
}

// writeMe sends member as the member's own view, with their effective sharing on the board.
func (s *Server) writeMe(writer http.ResponseWriter, request *http.Request, status int, member scoreboard.Member) {
	board, err := s.Service.Board(request.Context())
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	s.writeJSON(writer, request, status, meResponse{
		ID:                member.ID,
		DisplayName:       member.DisplayName,
		Status:            member.Status,
		CanApproveMembers: member.CanApproveMembers,
		Sharing:           member.Sharing,
		EffectiveSharing:  scoreboard.Effective(member, board),
		HasAvatar:         member.HasAvatar,
		JoinedAt:          member.JoinedAt,
	}, false)
}

// writeJSON sends value as a JSON body ending in a newline.
func (s *Server) writeJSON(writer http.ResponseWriter, request *http.Request, status int, value any, cacheable bool) {
	encoded, err := json.Marshal(value)
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	s.writeBody(writer, request, status, "application/json", append(encoded, '\n'), cacheable)
}

// writeBody sends a body, and for a cacheable read an ETag, answering 304 when the client already has
// it.
func (s *Server) writeBody(writer http.ResponseWriter, request *http.Request, status int, contentType string, body []byte, cacheable bool) {
	if cacheable {
		s.writeCached(writer, request, status, contentType, body, "private, no-cache")
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", contentType)
	writer.WriteHeader(status)
	writer.Write(body)
}

// writeCached writes a body with an ETag and cacheControl, answering 304 when the request already has it.
func (s *Server) writeCached(writer http.ResponseWriter, request *http.Request, status int, contentType string, body []byte, cacheControl string) {
	header := writer.Header()
	digest := sha256.Sum256(body)
	etag := `"` + base64.RawURLEncoding.EncodeToString(digest[:etagLength]) + `"`
	header.Set("ETag", etag)
	header.Set("Cache-Control", cacheControl)
	if matchesETag(request.Header.Get("If-None-Match"), etag) {
		writer.WriteHeader(http.StatusNotModified)
		return
	}
	header.Set("Content-Type", contentType)
	writer.WriteHeader(status)
	writer.Write(body)
}

// matchesETag reports whether an If-None-Match header names etag, weakly or by "*".
func matchesETag(ifNoneMatch, etag string) bool {
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == etag || candidate == "*" {
			return true
		}
	}
	return false
}

// readBody reads a request's body up to limit, checking its type when one is required.
func readBody(writer http.ResponseWriter, request *http.Request, limit int64, contentType string) ([]byte, *problem.Problem) {
	if contentType != "" {
		if failure := requireContentType(request, contentType); failure != nil {
			return nil, failure
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, limit))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return nil, problem.New(problem.PayloadTooLarge, "")
	}
	if err != nil {
		return nil, problem.New(problem.BadRequest, "the body could not be read")
	}
	return body, nil
}

// requireContentType refuses a request whose Content-Type is not expected, ignoring parameters.
func requireContentType(request *http.Request, expected string) *problem.Problem {
	contentType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || contentType != expected {
		return problem.New(problem.UnsupportedMediaType, "the body must be "+expected)
	}
	return nil
}

// decodeJSON decodes body into value, with an error that does not echo the body.
func decodeJSON(body []byte, value any) error {
	if err := json.Unmarshal(body, value); err != nil {
		return errors.New("the body is not valid JSON of the expected shape")
	}
	return nil
}

// connectionAddress is the host of the connection a request came on.
func connectionAddress(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return request.RemoteAddr
	}
	return host
}

// isProblem reports whether err is a problem with code.
func isProblem(err error, code problem.Code) bool {
	var failure *problem.Problem
	return errors.As(err, &failure) && failure.Code == code
}

// signedRequest is what signed hands on in a request's context.
type signedRequest struct {
	// member is the member who signed the request.
	member scoreboard.Member

	// body is the request's body, already read and verified.
	body []byte
}

// memberFrom returns the member who signed request, for handlers behind signed.
func memberFrom(request *http.Request) scoreboard.Member {
	return request.Context().Value(memberKey{}).(signedRequest).member
}

// bodyFrom returns the verified body of request, for handlers behind signed.
func bodyFrom(request *http.Request) []byte {
	return request.Context().Value(memberKey{}).(signedRequest).body
}

// statusRecorder remembers the status a handler sent, for the log.
type statusRecorder struct {
	// ResponseWriter is the writer the response goes to.
	http.ResponseWriter

	// status is the status sent, or 200 when the handler never set one.
	status int
}

// WriteHeader records status and sends it.
func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// boardResponse is GET /board's body: the board's public description, without its secrets.
type boardResponse struct {
	// AcceptingRequests reports whether a new member could join now.
	AcceptingRequests bool `json:"acceptingRequests"`

	// ID is the board's permanent identifier.
	ID string `json:"id"`

	// Name is the board's title.
	Name string `json:"name"`

	// Description is the admin's optional text about the board.
	Description string `json:"description,omitempty"`

	// HasAvatar reports whether GET /board/avatar has the board's own avatar.
	HasAvatar bool `json:"hasAvatar"`

	// WeekStartDay is the lowercase English weekday every week starts on.
	WeekStartDay string `json:"weekStartDay"`

	// RetentionWeeks is how many weeks the board keeps, counting the current one.
	RetentionWeeks int `json:"retentionWeeks"`

	// JoinMode is scoreboard.JoinOpen or scoreboard.JoinApproval.
	JoinMode string `json:"joinMode"`

	// PasswordRequired reports whether joining needs the board's password.
	PasswordRequired bool `json:"passwordRequired"`

	// AvatarsAllowed reports whether members may upload and show avatars.
	AvatarsAllowed bool `json:"avatarsAllowed"`

	// MaximumSharing caps every member's sharing.
	MaximumSharing sharing.Sharing `json:"maximumSharing"`

	// Ranking is scoreboard.RankByProgress or scoreboard.RankByName.
	Ranking string `json:"ranking"`

	// APIVersion is the openapi.yaml version the server implements.
	APIVersion string `json:"apiVersion"`

	// ServerVersion is the server build's version.
	ServerVersion string `json:"serverVersion"`
}

// meResponse is the member's own view of their membership.
type meResponse struct {
	// ID is the member's identifier, derived from their public key.
	ID string `json:"id"`

	// DisplayName is the name the board shows for the member.
	DisplayName string `json:"displayName"`

	// Status is scoreboard.StatusPending or scoreboard.StatusActive.
	Status string `json:"status"`

	// CanApproveMembers reports whether the admin lets the member approve and reject join requests.
	CanApproveMembers bool `json:"canApproveMembers"`

	// Sharing is what the member chose to share.
	Sharing sharing.Sharing `json:"sharing"`

	// EffectiveSharing is Sharing capped by the board's maximum.
	EffectiveSharing sharing.Sharing `json:"effectiveSharing"`

	// HasAvatar reports whether the member has uploaded an avatar.
	HasAvatar bool `json:"hasAvatar"`

	// JoinedAt is when the member joined.
	JoinedAt time.Time `json:"joinedAt"`
}
