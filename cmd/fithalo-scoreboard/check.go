package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/auth"
)

// checker walks a running board through the API as a client would, as one throwaway member, and
// leaves the board as it found it.
type checker struct {
	// base is the board's address, without a trailing slash.
	base *url.URL

	// client sends every request.
	client *http.Client

	// key is the throwaway member's signing key.
	key *ecdsa.PrivateKey

	// output receives each check's outcome.
	output io.Writer

	// failures counts the checks that failed.
	failures int
}

// checkResponse is what a request returned.
type checkResponse struct {
	// status is the HTTP status code.
	status int

	// header is the response's headers.
	header http.Header

	// body is the response body, up to maximumResponseBytes.
	body []byte
}

const (
	// requestTimeout bounds each request the check makes.
	requestTimeout = 20 * time.Second

	// dialTimeout bounds each TLS handshake probe.
	dialTimeout = 10 * time.Second

	// maximumResponseBytes is the most of a response body the check reads.
	maximumResponseBytes = 1 << 20

	// boardIDLength is the length of a board's identifier: 16 random bytes in unpadded base64url.
	boardIDLength = 22
)

// check is the conformance check: fithalo-scoreboard check -url https://board.example.com -invite code.
// A board's password comes from SCOREBOARD_CHECK_PASSWORD, never a flag the process list shows.
func check(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	boardURL := flags.String("url", "", "the board's address")
	invite := flags.String("invite", "", "the board's current invite code")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	base, err := url.Parse(strings.TrimSuffix(*boardURL, "/"))
	if err != nil || base.Host == "" || *invite == "" {
		return errors.New("check needs -url and -invite")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	run := &checker{base: base, client: &http.Client{Timeout: requestTimeout}, key: key, output: output}

	if base.Scheme == "https" {
		run.checkTLS()
	}
	board := run.checkBoard()
	if board == nil {
		return errors.New("the board could not be read, so nothing else was checked")
	}
	status := run.checkJoin(*invite, os.Getenv("SCOREBOARD_CHECK_PASSWORD"))
	if status != "" {
		run.checkSignatures()
		if status == "active" {
			run.expectProblem("a member the admin has not allowed cannot read member requests", "GET", "/member-requests", nil, 403, "notPermitted")
			run.checkWeeklyChallenges(board)
		} else {
			fmt.Fprintln(run.output, "  note  the membership is waiting for approval, so weekly challenges were not checked; approve it and run again")
			run.expectProblem("a pending member cannot put a weekly challenge", "PUT", "/members/me/weekly-challenges/"+weekStart(board.WeekStartDay), []byte(`{"progress":0.5}`), 403, "memberPending")
		}
		run.checkLeaving()
	}
	if run.failures > 0 {
		return fmt.Errorf("%d checks failed", run.failures)
	}
	fmt.Fprintln(run.output, "Every check passed.")
	return nil
}

// checkedBoard is the part of GET /board the check reads.
type checkedBoard struct {
	// ID is the board's identifier.
	ID string `json:"id"`

	// WeekStartDay is the lower-case English weekday the board's weeks start on.
	WeekStartDay string `json:"weekStartDay"`

	// APIVersion is the API version the board serves.
	APIVersion string `json:"apiVersion"`
}

// result prints a check's outcome and counts a failure.
func (c *checker) result(passed bool, description, detail string) {
	if passed {
		fmt.Fprintf(c.output, "  pass  %s\n", description)
		return
	}
	c.failures++
	fmt.Fprintf(c.output, "  FAIL  %s: %s\n", description, detail)
}

// checkTLS checks the board refuses TLS 1.1 and accepts TLS 1.2 with a valid certificate.
func (c *checker) checkTLS() {
	host := c.base.Host
	if c.base.Port() == "" {
		host = net.JoinHostPort(c.base.Hostname(), "443")
	}
	for _, version := range []uint16{tls.VersionTLS11, tls.VersionTLS12} {
		connection, err := tls.DialWithDialer(&net.Dialer{Timeout: dialTimeout}, "tcp", host, &tls.Config{
			ServerName: c.base.Hostname(), MinVersion: version, MaxVersion: version,
		})
		if connection != nil {
			connection.Close()
		}
		if version == tls.VersionTLS11 {
			c.result(err != nil, "TLS 1.1 is refused", "the board accepted a TLS 1.1 handshake")
		} else {
			c.result(err == nil, "TLS 1.2 is accepted with a valid certificate", fmt.Sprint(err))
		}
	}
}

// checkBoard reads the board, or returns nil when it cannot, and checks unsigned requests are refused.
func (c *checker) checkBoard() *checkedBoard {
	response, err := c.send("GET", "/board", nil, false, time.Now(), "")
	if err != nil {
		c.result(false, "the board answers", err.Error())
		return nil
	}
	var board checkedBoard
	if response.status != 200 || json.Unmarshal(response.body, &board) != nil || len(board.ID) != boardIDLength || board.APIVersion == "" {
		c.result(false, "GET /board describes the board", fmt.Sprintf("status %d: %s", response.status, response.body))
		return nil
	}
	c.result(true, "GET /board describes the board, API "+board.APIVersion, "")
	c.expectProblem("an unsigned request is refused", "GET", "/members/me", nil, 401, "unauthenticated")
	return &board
}

// checkJoin joins as the throwaway member and returns the membership's status, or empty on failure.
func (c *checker) checkJoin(invite, boardPassword string) string {
	join := map[string]any{
		"inviteCode": invite, "publicKey": auth.EncodePublicKey(&c.key.PublicKey), "displayName": "Conformance check",
		"sharing": map[string]any{"score": "details", "activity": "points", "routes": false, "strengthSets": false, "strengthWeights": false},
	}
	if boardPassword != "" {
		join["password"] = boardPassword
	}
	body, _ := json.Marshal(join)
	response, err := c.send("POST", "/members", body, true, time.Now(), "")
	var me struct {
		Status string `json:"status"`
	}
	if err != nil || response.status != 201 || json.Unmarshal(response.body, &me) != nil {
		c.result(false, "joining creates a membership", describe(response, err))
		return ""
	}
	c.result(true, "joining creates a membership, "+me.Status, "")
	again, err := c.send("POST", "/members", body, true, time.Now(), "")
	c.result(err == nil && again.status == 200, "joining again returns the same membership", describe(again, err))
	return me.Status
}

// checkSignatures checks a signed request is accepted, and a skewed or replayed one refused.
func (c *checker) checkSignatures() {
	response, err := c.send("GET", "/members/me", nil, true, time.Now(), "")
	c.result(err == nil && response.status == 200, "a signed request is accepted", describe(response, err))
	c.expectProblemAt("a request signed ten minutes ago is refused as clock skew", time.Now().Add(-10*time.Minute), 401, "clockSkew")

	const replayCheck = "a replayed request is refused"
	authorization, err := auth.AuthorizationValue(c.key, "GET", c.target("/members/me"), time.Now(), nil)
	if err != nil {
		c.result(false, replayCheck, "signing the request: "+err.Error())
		return
	}
	first, err := c.send("GET", "/members/me", nil, false, time.Now(), authorization)
	if err != nil || first.status != 200 {
		c.result(false, replayCheck, "the first request: "+describe(first, err))
		return
	}
	replay, err := c.send("GET", "/members/me", nil, false, time.Now(), authorization)
	c.result(err == nil && replay.status == 401 && problemCode(replay) == "replayed", replayCheck, describe(replay, err))
}

// checkWeeklyChallenges puts a weekly challenge, and checks it is stripped to the member's sharing and appears on
// the board.
func (c *checker) checkWeeklyChallenges(board *checkedBoard) {
	start := weekStart(board.WeekStartDay)
	activityID := "CheckCheckCheckCheck00"
	challenge := map[string]any{
		"progress": 0.42, "score": 210, "goal": 500, "tier": "beginner",
		"breakdown": map[string]any{"exercise": 200, "dailyBonus": 10, "varietyBonus": 0},
		"activities": []any{map[string]any{
			"id": activityID, "type": "outdoorRucking", "day": start, "points": map[string]any{"base": 20, "bonus": 2},
			"durationSeconds": 1800, "details": map[string]any{"averageHeartRate": 120},
		}},
	}
	body, _ := json.Marshal(challenge)
	response, err := c.send("PUT", "/members/me/weekly-challenges/"+start, body, true, time.Now(), "")
	stripped := err == nil && response.status == 200 && bytes.Contains(response.body, []byte(activityID)) &&
		!bytes.Contains(response.body, []byte("averageHeartRate")) && !bytes.Contains(response.body, []byte("durationSeconds"))
	c.result(stripped, "a weekly challenge is stored at the member's sharing, with details above it stripped", describe(response, err))

	scoreboard, err := c.send("GET", "/weekly-challenges/"+start, nil, true, time.Now(), "")
	c.result(err == nil && scoreboard.status == 200 && bytes.Contains(scoreboard.body, []byte("Conformance check")), "the scoreboard lists the member", describe(scoreboard, err))
	if etag := scoreboard.header.Get("ETag"); etag != "" {
		unchanged, err := c.send("GET", "/weekly-challenges/"+start, nil, true, time.Now(), etag)
		c.result(err == nil && unchanged.status == 304, "an unchanged scoreboard answers 304", describe(unchanged, err))
	} else {
		c.result(false, "the scoreboard carries an ETag", "no ETag")
	}
	c.expectProblem("a weekly challenge starting on the wrong day is refused", "PUT", "/members/me/weekly-challenges/2000-01-01", body, 422, "weekOutOfRange")
}

// checkLeaving leaves the board and checks the member can no longer sign in.
func (c *checker) checkLeaving() {
	response, err := c.send("DELETE", "/members/me", nil, true, time.Now(), "")
	c.result(err == nil && response.status == 204, "leaving erases the membership", describe(response, err))
	c.expectProblem("a member who left can no longer sign in", "GET", "/members/me", nil, 401, "unauthenticated")
}

// expectProblem checks a signed request fails with status and the problem code.
func (c *checker) expectProblem(description, method, path string, body []byte, status int, code string) {
	response, err := c.send(method, path, body, true, time.Now(), "")
	c.result(err == nil && response.status == status && problemCode(response) == code, description, describe(response, err))
}

// expectProblemAt checks a GET of /members/me signed at signedAt fails with status and the problem code.
func (c *checker) expectProblemAt(description string, signedAt time.Time, status int, code string) {
	response, err := c.send("GET", "/members/me", nil, true, signedAt, "")
	c.result(err == nil && response.status == status && problemCode(response) == code, description, describe(response, err))
}

// target returns the request path for an API path.
func (c *checker) target(path string) string {
	return c.base.Path + "/api/v1" + path
}

// send makes a request, signed at signedAt when sign is set. For a GET, extra is an ETag to send as
// If-None-Match; for an unsigned request it is a ready-made Authorization header.
func (c *checker) send(method, path string, body []byte, sign bool, signedAt time.Time, extra string) (checkResponse, error) {
	target := c.target(path)
	request, err := http.NewRequest(method, c.base.Scheme+"://"+c.base.Host+target, bytes.NewReader(body))
	if err != nil {
		return checkResponse{}, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if sign {
		authorization, err := auth.AuthorizationValue(c.key, method, target, signedAt, body)
		if err != nil {
			return checkResponse{}, err
		}
		request.Header.Set("Authorization", authorization)
		if extra != "" {
			request.Header.Set("If-None-Match", extra)
		}
	} else if extra != "" {
		request.Header.Set("Authorization", extra)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return checkResponse{}, err
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes))
	return checkResponse{status: response.StatusCode, header: response.Header, body: data}, nil
}

// problemCode returns a problem response's code, or empty.
func problemCode(response checkResponse) string {
	var problem struct {
		Code string `json:"code"`
	}
	json.Unmarshal(response.body, &problem)
	return problem.Code
}

// describe summarises a response, or the error that stopped it, for a failed check.
func describe(response checkResponse, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("status %d: %s", response.status, bytes.TrimSpace(response.body))
}

// weekStart is the start of the current week on the board's start day, by UTC.
func weekStart(day string) string {
	weekdays := map[string]time.Weekday{"monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday,
		"thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday, "sunday": time.Sunday}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return today.AddDate(0, 0, -((int(today.Weekday()) - int(weekdays[day]) + 7) % 7)).Format("2006-01-02")
}
