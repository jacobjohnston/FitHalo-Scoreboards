package api_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/api"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/auth"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/ratelimit"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
	"github.com/jacobjohnston/FitHalo-Scoreboards/spec"
)

// probeInvite is every probe board's invite code.
const probeInvite = "ProbeInviteCode0000000"

// probeWeek is the Monday of the week the probe clock starts in.
const probeWeek = "2026-10-19"

// probeNow is the probe clock's starting instant, a Wednesday noon UTC.
var probeNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// board is a probe board served over HTTP from a directory store with a clock the test moves.
type board struct {
	// t is the test the board fails.
	t *testing.T

	// server serves the API.
	server *httptest.Server

	// service is the board behind the server, for setup the API cannot do.
	service *scoreboard.Service

	// basePath is the path the API is served under, before /api/v1.
	basePath string

	// mutex guards now.
	mutex sync.Mutex

	// now is the board's clock.
	now time.Time
}

// boardOptions configures a probe board before it is created.
type boardOptions struct {
	// configure edits the board document before it is stored.
	configure func(*scoreboard.Board)

	// basePath serves the API under a path, as a proxy might.
	basePath string

	// limiter replaces the generous default rate limiter.
	limiter *ratelimit.Limiter

	// joinLimiter replaces the generous default join limiter.
	joinLimiter *ratelimit.Limiter

	// noLimits serves without any rate limiter.
	noLimits bool
}

// newBoard serves an open board that starts its weeks on Monday.
func newBoard(t *testing.T, options boardOptions) *board {
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
	b := &board{t: t, now: probeNow, basePath: options.basePath}
	b.service = &scoreboard.Service{Store: directory, Allowlist: allowlist, Now: b.clock}
	document, err := scoreboard.DefaultBoard()
	if err != nil {
		t.Fatal(err)
	}
	document.JoinMode = scoreboard.JoinOpen
	document.InviteCode = probeInvite
	document.WeekStartDay = "monday"
	if options.configure != nil {
		options.configure(&document)
	}
	if _, _, err := b.service.EnsureBoard(context.Background(), document); err != nil {
		t.Fatal(err)
	}
	limiter := options.limiter
	if limiter == nil && !options.noLimits {
		limiter = ratelimit.New(100000, 100000)
	}
	joinLimiter := options.joinLimiter
	if joinLimiter == nil && !options.noLimits {
		joinLimiter = ratelimit.New(100000, 100000)
	}
	server := &api.Server{
		Service:       b.service,
		Nonces:        auth.NewNonceCache(),
		Limiter:       limiter,
		JoinLimiter:   joinLimiter,
		ServerVersion: "probe",
	}
	b.server = httptest.NewServer(server.Handler(options.basePath))
	t.Cleanup(b.server.Close)
	return b
}

// clock returns the board's time.
func (b *board) clock() time.Time {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.now
}

// advance moves the board's clock on by duration.
func (b *board) advance(duration time.Duration) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.now = b.now.Add(duration)
}

// update edits the board document through the service, as the admin portal would.
func (b *board) update(change func(*scoreboard.Board)) {
	b.t.Helper()
	_, err := b.service.UpdateBoard(context.Background(), func(document *scoreboard.Board) error {
		change(document)
		return nil
	})
	if err != nil {
		b.t.Fatal(err)
	}
}

// phone is a member's device: a key and the requests it signs.
type phone struct {
	// board is the board the phone talks to.
	board *board

	// key signs the phone's requests; nil sends them unsigned.
	key *ecdsa.PrivateKey

	// skew is added to the time the phone signs with.
	skew time.Duration
}

// newPhone returns a phone with a fresh key.
func (b *board) newPhone() *phone {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		b.t.Fatal(err)
	}
	return &phone{board: b, key: key}
}

// publicKey is the phone's key as the API carries it.
func (p *phone) publicKey() string {
	return auth.EncodePublicKey(&p.key.PublicKey)
}

// id is the member identifier the phone's key derives, computed independently of the server.
func (p *phone) id() string {
	raw := elliptic.Marshal(elliptic.P256(), p.key.PublicKey.X, p.key.PublicKey.Y)
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

// reply is an answer read in full.
type reply struct {
	// status is the HTTP status.
	status int

	// header is the response's headers.
	header http.Header

	// body is the whole body.
	body []byte
}

// decode unmarshals the body into value, failing the test when it cannot.
func (r reply) decode(t *testing.T, value any) {
	t.Helper()
	if err := json.Unmarshal(r.body, value); err != nil {
		t.Fatalf("decoding %q: %v", r.body, err)
	}
}

// object decodes the body as a generic JSON object.
func (r reply) object(t *testing.T) map[string]any {
	t.Helper()
	var value map[string]any
	r.decode(t, &value)
	return value
}

// code returns the body's problem code, or empty when the body is not a problem.
func (r reply) code() string {
	var failure struct {
		Code string `json:"code"`
	}
	json.Unmarshal(r.body, &failure)
	return failure.Code
}

// request is a request about to be sent, so a test can tamper with it after signing.
type request struct {
	// method is the HTTP method.
	method string

	// target is the path and query after the base path, starting at /api/v1.
	target string

	// body is the request body.
	body []byte

	// header is every header to send, Authorization included once signed.
	header http.Header
}

// prepare builds and signs a request without sending it.
func (p *phone) prepare(method, path string, body []byte, contentType string) *request {
	p.board.t.Helper()
	target := p.board.basePath + "/api/v1" + path
	r := &request{method: method, target: target, body: body, header: http.Header{}}
	if contentType != "" {
		r.header.Set("Content-Type", contentType)
	}
	if p.key != nil {
		value, err := auth.AuthorizationValue(p.key, method, target, p.board.clock().Add(p.skew), body)
		if err != nil {
			p.board.t.Fatal(err)
		}
		r.header.Set("Authorization", value)
	}
	return r
}

// dispatch sends a prepared request to the board.
func (p *phone) dispatch(r *request) reply {
	p.board.t.Helper()
	httpRequest, err := http.NewRequest(r.method, p.board.server.URL+r.target, bytes.NewReader(r.body))
	if err != nil {
		p.board.t.Fatal(err)
	}
	for key, values := range r.header {
		httpRequest.Header[key] = values
	}
	return perform(p.board.t, httpRequest)
}

// perform sends an HTTP request and reads the whole response.
func perform(t *testing.T, httpRequest *http.Request) reply {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	result, err := client.Do(httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	body, _ := io.ReadAll(result.Body)
	return reply{status: result.StatusCode, header: result.Header, body: body}
}

// send signs and sends a request with a raw body.
func (p *phone) send(method, path string, body []byte, contentType string) reply {
	p.board.t.Helper()
	return p.dispatch(p.prepare(method, path, body, contentType))
}

// get signs and sends a GET with optional extra headers.
func (p *phone) get(path string, header http.Header) reply {
	p.board.t.Helper()
	r := p.prepare("GET", path, nil, "")
	for key, values := range header {
		r.header[key] = values
	}
	return p.dispatch(r)
}

// json signs and sends value as a JSON body.
func (p *phone) json(method, path string, value any) reply {
	p.board.t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		p.board.t.Fatal(err)
	}
	return p.send(method, path, encoded, "application/json")
}

// joinBody is a join request with the probe invite and the phone's key.
func (p *phone) joinBody(name string, chosen sharing.Sharing) map[string]any {
	return map[string]any{
		"inviteCode":  probeInvite,
		"publicKey":   p.publicKey(),
		"displayName": name,
		"sharing":     chosen,
	}
}

// join asks to join the board.
func (p *phone) join(name string, chosen sharing.Sharing) reply {
	p.board.t.Helper()
	return p.json("POST", "/members", p.joinBody(name, chosen))
}

// mustJoin joins and fails the test unless the board answers 201.
func (p *phone) mustJoin(name string, chosen sharing.Sharing) {
	p.board.t.Helper()
	expectStatus(p.board.t, p.join(name, chosen), http.StatusCreated)
}

// putWeeklyChallenge puts a weekly challenge and returns the reply.
func (p *phone) putWeeklyChallenge(weekStart string, challenge any) reply {
	p.board.t.Helper()
	return p.json("PUT", "/members/me/weekly-challenges/"+weekStart, challenge)
}

// expectStatus fails the test unless result has status.
func expectStatus(t *testing.T, result reply, status int) {
	t.Helper()
	if result.status != status {
		t.Fatalf("got status %d, want %d: %s", result.status, status, result.body)
	}
}

// expectProblem fails the test unless result is a problem with status and code in the RFC 9457 shape.
func expectProblem(t *testing.T, result reply, status int, code string) {
	t.Helper()
	if result.status != status {
		t.Fatalf("got status %d, want %d: %s", result.status, status, result.body)
	}
	if contentType := result.header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/problem+json") {
		t.Errorf("problem served as %q", contentType)
	}
	var problem struct {
		Code   string `json:"code"`
		Status int    `json:"status"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(result.body, &problem); err != nil {
		t.Fatalf("problem body %q: %v", result.body, err)
	}
	if problem.Code != code {
		t.Errorf("got code %q, want %q", problem.Code, code)
	}
	if problem.Status != status {
		t.Errorf("problem status %d does not match HTTP status %d", problem.Status, status)
	}
	if problem.Title == "" {
		t.Errorf("problem has no title")
	}
}

// activityID returns a deterministic 22-character identifier for test activities.
func activityID(n int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("activity-%d", n)))
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

// everythingChallenge is a weekly challenge whose one activity carries every field spec/sharing.json lists.
func everythingChallenge(day string, id string) map[string]any {
	return map[string]any{
		"progress": 0.8, "score": 400, "goal": 500, "tier": "monster",
		"breakdown": map[string]any{"exercise": 350, "dailyBonus": 30, "varietyBonus": 20},
		"activities": []any{map[string]any{
			"id": id, "type": "outdoorRunning", "day": day,
			"points":          map[string]any{"base": 100, "bonus": 10},
			"durationSeconds": 1800, "distanceMeters": 5000,
			"start": day + "T07:00:00+02:00",
			"details": map[string]any{
				"activeEnergyKilocalories": 400, "averageHeartRate": 150, "maximumHeartRate": 180, "minimumHeartRate": 95,
				"averageSpeedMetersPerSecond": 2.8, "elevationGainMeters": 50, "elevationLossMeters": 45,
				"steps": 6000, "averageCadence": 170, "averagePowerWatts": 250, "averageMETs": 9.5,
				"carriedLoadKilograms": 10, "inclinePercent": 1.5, "strollerLoadKilograms": 12,
				"exposureTemperatureCelsius": 80,
				"weather":                    map[string]any{"temperatureCelsius": 18, "humidityPercent": 60},
			},
			"hasRoute": true,
			"strength": map[string]any{
				"deload":  true,
				"muscles": []any{map[string]any{"group": "chest", "intensity": "primary"}},
				"exercises": []any{map[string]any{
					"catalogID": "8d9d1a3e-2f4b-4c7a-9e1f-0a1b2c3d4e5f", "name": "Custom", "loadIsAssistance": false,
					"personalRecord": true, "injured": true, "angleDegrees": 30, "volumeKilograms": 960,
					"modifiers": []any{
						map[string]any{"kind": "assist", "assist": "instant"},
						map[string]any{"kind": "partialRange", "partialRange": "shortened"},
						map[string]any{"kind": "chains", "percent": 20},
						map[string]any{"kind": "tempo", "tempo": map[string]any{"eccentricSeconds": 3, "bottomPauseSeconds": 1, "concentricSeconds": 1, "topPauseSeconds": 0}},
						map[string]any{"kind": "isokinetic", "velocityMetersPerSecond": 0.5},
						map[string]any{"kind": "bands", "weightKilograms": 10},
						map[string]any{"kind": "progression", "weightPerRepKilograms": 1},
					},
					"sets": []any{map[string]any{
						"type": "normal", "reps": 8, "durationSeconds": 30, "distanceMeters": 20, "weightKilograms": 60, "repsInReserve": 2,
						"right":    map[string]any{"reps": 8, "durationSeconds": 30, "weightKilograms": 60, "repsInReserve": 1},
						"clusters": []any{map[string]any{"reps": 3, "weightKilograms": 50}},
					}},
				}},
			},
		}},
	}
}

// minimalChallenge is a weekly challenge with only the field every level carries.
func minimalChallenge(progress float64) map[string]any {
	return map[string]any{"progress": progress}
}

// leafPaths lists the path of every scalar in a decoded JSON document in spec/sharing.json's notation.
func leafPaths(value any, prefix string, out *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			leafPaths(child, path, out)
		}
	case []any:
		for _, child := range typed {
			leafPaths(child, prefix+"[]", out)
		}
	default:
		*out = append(*out, prefix)
	}
}
