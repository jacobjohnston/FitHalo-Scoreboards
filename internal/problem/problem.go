// Package problem writes RFC 9457 problem details with the stable codes the API defines.
package problem

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// Code is the machine-readable part of a problem, listed in openapi.yaml under Problem.
type Code string

// The codes the API defines.
const (
	// BadRequest is a request that is malformed or fails validation.
	BadRequest Code = "badRequest"

	// Unauthenticated is a missing or bad signature, or a signer who is not a member.
	Unauthenticated Code = "unauthenticated"

	// ClockSkew is a signed time too far from the server's clock.
	ClockSkew Code = "clockSkew"

	// Replayed is a nonce the member has already used.
	Replayed Code = "replayed"

	// InviteInvalid is a join whose invite code is not the board's current one.
	InviteInvalid Code = "inviteInvalid"

	// PasswordIncorrect is a join with a missing or wrong board password.
	PasswordIncorrect Code = "passwordIncorrect"

	// Blocked is a join from a member the admin blocked from joining again.
	Blocked Code = "blocked"

	// PendingLimitReached is a join refused because the approval queue is full.
	PendingLimitReached Code = "pendingLimitReached"

	// MemberLimitReached is a join refused because the board has as many members as it keeps.
	MemberLimitReached Code = "memberLimitReached"

	// MemberPending is a request from a member still waiting for approval.
	MemberPending Code = "memberPending"

	// NotPermitted is a request from a member the admin has not allowed to approve requests.
	NotPermitted Code = "notPermitted"

	// AvatarsNotAllowed is an avatar upload to a board that takes none.
	AvatarsNotAllowed Code = "avatarsNotAllowed"

	// RoutesNotShared is a route upload from a member whose sharing excludes routes.
	RoutesNotShared Code = "routesNotShared"

	// NotFound is no such member, weekly challenge, activity, route or avatar.
	NotFound Code = "notFound"

	// UnknownOperation is a method and path this server does not serve, such as one a newer server added.
	UnknownOperation Code = "unknownOperation"

	// UpgradeRequired is a client too old for the board to serve; the 1.0 server never sends it.
	UpgradeRequired Code = "upgradeRequired"

	// PayloadTooLarge is a body over its operation's limit.
	PayloadTooLarge Code = "payloadTooLarge"

	// UnsupportedMediaType is a body of a type the operation does not take.
	UnsupportedMediaType Code = "unsupportedMediaType"

	// ImageInvalid is an avatar that does not decode as an image.
	ImageInvalid Code = "imageInvalid"

	// WeekOutOfRange is a week not on the board's start day, too old to keep, or after next week.
	WeekOutOfRange Code = "weekOutOfRange"

	// RateLimited is a request over the client address's rate limit.
	RateLimited Code = "rateLimited"

	// Internal is an unexpected server failure, with no detail for the client.
	Internal Code = "internal"

	// Unavailable is a server or store that cannot be reached for now.
	Unavailable Code = "unavailable"
)

// definitions gives each code its HTTP status and title.
var definitions = map[Code]struct {
	status int
	title  string
}{
	BadRequest:           {http.StatusBadRequest, "Bad request"},
	Unauthenticated:      {http.StatusUnauthorized, "Not signed in"},
	ClockSkew:            {http.StatusUnauthorized, "Clock skew"},
	Replayed:             {http.StatusUnauthorized, "Replayed request"},
	InviteInvalid:        {http.StatusForbidden, "Invite not valid"},
	PasswordIncorrect:    {http.StatusForbidden, "Password incorrect"},
	Blocked:              {http.StatusForbidden, "Blocked"},
	PendingLimitReached:  {http.StatusForbidden, "Not taking requests"},
	MemberLimitReached:   {http.StatusForbidden, "Board full"},
	MemberPending:        {http.StatusForbidden, "Waiting for approval"},
	NotPermitted:         {http.StatusForbidden, "Not permitted"},
	AvatarsNotAllowed:    {http.StatusForbidden, "Avatars not allowed"},
	RoutesNotShared:      {http.StatusForbidden, "Routes not shared"},
	NotFound:             {http.StatusNotFound, "Not found"},
	UnknownOperation:     {http.StatusNotFound, "Unknown operation"},
	UpgradeRequired:      {http.StatusForbidden, "Upgrade required"},
	PayloadTooLarge:      {http.StatusRequestEntityTooLarge, "Too large"},
	UnsupportedMediaType: {http.StatusUnsupportedMediaType, "Unsupported media type"},
	ImageInvalid:         {http.StatusUnprocessableEntity, "Image not valid"},
	WeekOutOfRange:       {http.StatusUnprocessableEntity, "Week out of range"},
	RateLimited:          {http.StatusTooManyRequests, "Too many requests"},
	Internal:             {http.StatusInternalServerError, "Server error"},
	Unavailable:          {http.StatusServiceUnavailable, "Unavailable"},
}

// Problem is an RFC 9457 problem detail. It is also an error, so services can return one.
type Problem struct {
	// Code is the stable code clients match on.
	Code Code `json:"code"`

	// Status is the HTTP status the problem is sent with.
	Status int `json:"status"`

	// Title is the code's short summary for people.
	Title string `json:"title"`

	// Detail explains this occurrence for people, and is omitted when empty.
	Detail string `json:"detail,omitempty"`

	// RetryAfter is the seconds to send in a Retry-After header, or 0 for none; it is not in the body.
	RetryAfter int `json:"-"`
}

// New returns the problem for code, with an optional detail for people. An unknown code becomes Internal.
func New(code Code, detail string) *Problem {
	definition, known := definitions[code]
	if !known {
		definition = definitions[Internal]
		code = Internal
	}
	return &Problem{Code: code, Status: definition.status, Title: definition.title, Detail: detail}
}

// Error returns the problem's code and detail.
func (p *Problem) Error() string {
	if p.Detail == "" {
		return string(p.Code)
	}
	return string(p.Code) + ": " + p.Detail
}

// Write sends the problem as application/problem+json.
func Write(writer http.ResponseWriter, problem *Problem) {
	header := writer.Header()
	header.Set("Content-Type", "application/problem+json")
	header.Set("Cache-Control", "no-store")
	if problem.RetryAfter > 0 {
		header.Set("Retry-After", strconv.Itoa(problem.RetryAfter))
	}
	writer.WriteHeader(problem.Status)
	json.NewEncoder(writer).Encode(problem)
}
