package scoreboard_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/auth"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/problem"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
	"github.com/jacobjohnston/FitHalo-Scoreboards/spec"
)

const (
	// probeInvite is every probe board's invite code.
	probeInvite = "InviteCodeInviteCode00"

	// probeWeek is the Monday the probe clock's week starts on.
	probeWeek = "2026-10-19"

	// probeDay is a day inside probeWeek.
	probeDay = "2026-10-20"

	// probeActivity is the identifier of the base weekly challenge's only activity.
	probeActivity = "AAAAAAAAAAAAAAAAAAAAAA"
)

// probeNow is the probe clock's starting time, a Wednesday in probeWeek.
var probeNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// weekStartDays is every weekday a board may start on, in Go's weekday order.
var weekStartDays = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}

// probe is a service over a real directory store with a clock the test controls.
type probe struct {
	// t is the test the probe fails.
	t testing.TB

	// service is the board under test.
	service *scoreboard.Service

	// directory is the store beneath service, for looking at what was kept.
	directory *store.Directory

	// allowlist is spec/sharing.json.
	allowlist *sharing.Allowlist

	// mutex guards now.
	mutex sync.Mutex

	// now is the board's clock.
	now time.Time
}

// newProbe returns a service over an open Monday board, letting configure change the board first.
func newProbe(t testing.TB, configure func(*scoreboard.Board)) *probe {
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
	p := &probe{t: t, directory: directory, allowlist: allowlist, now: probeNow}
	p.service = &scoreboard.Service{Store: directory, Allowlist: allowlist, Now: p.clock, Logger: slog.New(slog.DiscardHandler)}
	board, err := scoreboard.DefaultBoard()
	if err != nil {
		t.Fatal(err)
	}
	board.JoinMode = scoreboard.JoinOpen
	board.InviteCode = probeInvite
	board.WeekStartDay = "monday"
	if configure != nil {
		configure(&board)
	}
	if _, _, err := p.service.EnsureBoard(context.Background(), board); err != nil {
		t.Fatal(err)
	}
	return p
}

// clock returns the board's time.
func (p *probe) clock() time.Time {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	return p.now
}

// advance moves the board's clock on by duration.
func (p *probe) advance(duration time.Duration) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.now = p.now.Add(duration)
}

// board returns the stored board.
func (p *probe) board() scoreboard.Board {
	p.t.Helper()
	board, err := p.service.Board(context.Background())
	if err != nil {
		p.t.Fatal(err)
	}
	return board
}

// identity is a member's key pair with the public key and identifier the API derives from it.
type identity struct {
	// key is the member's private key.
	key *ecdsa.PrivateKey

	// publicKey is the key as JoinRequest.PublicKey carries it.
	publicKey string

	// id is auth.MemberID of the raw public key.
	id string
}

// newIdentity returns a fresh P-256 identity.
func newIdentity(t testing.TB) identity {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded := auth.EncodePublicKey(&key.PublicKey)
	_, raw, err := auth.ParsePublicKey(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return identity{key: key, publicKey: encoded, id: auth.MemberID(raw)}
}

// joinRequest returns a join request for the identity with the board's invite.
func (p *probe) joinRequest(who identity, name string, chosen sharing.Sharing) scoreboard.JoinRequest {
	return scoreboard.JoinRequest{InviteCode: probeInvite, PublicKey: who.publicKey, DisplayName: name, Sharing: chosen}
}

// joinAs joins the identity, failing the test when the join fails.
func (p *probe) joinAs(who identity, name string, chosen sharing.Sharing) scoreboard.Member {
	p.t.Helper()
	member, _, err := p.service.Join(context.Background(), p.joinRequest(who, name, chosen), who.id)
	if err != nil {
		p.t.Fatalf("joining %q: %v", name, err)
	}
	return member
}

// join adds a member with a new identity.
func (p *probe) join(name string, chosen sharing.Sharing) scoreboard.Member {
	p.t.Helper()
	return p.joinAs(newIdentity(p.t), name, chosen)
}

// member re-reads a membership.
func (p *probe) member(id string) scoreboard.Member {
	p.t.Helper()
	member, err := p.service.Member(context.Background(), id)
	if err != nil {
		p.t.Fatal(err)
	}
	return member
}

// putWeeklyChallenge puts a weekly challenge for member, starting on weekStart.
func (p *probe) putWeeklyChallenge(member scoreboard.Member, weekStart string, body []byte) (scoreboard.WeeklyChallenge, error) {
	return p.service.PutWeeklyChallenge(context.Background(), member, weekStart, body)
}

// mustPutWeeklyChallenge puts a weekly challenge, failing the test when it is refused.
func (p *probe) mustPutWeeklyChallenge(member scoreboard.Member, weekStart string, body []byte) scoreboard.WeeklyChallenge {
	p.t.Helper()
	challenge, err := p.putWeeklyChallenge(member, weekStart, body)
	if err != nil {
		p.t.Fatalf("putting the weekly challenge of %s for %s: %v", weekStart, member.DisplayName, err)
	}
	return challenge
}

// storedChallenge reads a member's stored weekly challenge as a JSON object, or nil when there is none.
func (p *probe) storedChallenge(memberID, weekStart string) map[string]any {
	p.t.Helper()
	document, err := p.directory.Get(context.Background(), "weeks/"+weekStart, memberID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		p.t.Fatal(err)
	}
	return decodeObject(p.t, document.Data)
}

// storedRoutes counts the routes stored for a member's weekly challenge.
func (p *probe) storedRoutes(memberID, weekStart string) int {
	p.t.Helper()
	items, err := p.directory.List(context.Background(), "routes/"+memberID+"/"+weekStart)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		p.t.Fatal(err)
	}
	return len(items)
}

// codeOf returns the problem code in err, "" for nil, or the error text when it is not a problem.
func codeOf(err error) problem.Code {
	if err == nil {
		return ""
	}
	var failure *problem.Problem
	if errors.As(err, &failure) {
		return failure.Code
	}
	return problem.Code("not a problem: " + err.Error())
}

// expectCode fails the test unless err carries want, where "" means no error.
func expectCode(t testing.TB, err error, want problem.Code) {
	t.Helper()
	if got := codeOf(err); got != want {
		t.Errorf("got code %q (%v), want %q", got, err, want)
	}
}

// decodeObject decodes JSON into a generic object.
func decodeObject(t testing.TB, data []byte) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatalf("decoding %.80q: %v", data, err)
	}
	return object
}

// asObject encodes value and decodes it back as a generic object.
func asObject(t testing.TB, value any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return decodeObject(t, encoded)
}

// encode marshals value as a request body.
func encode(t testing.TB, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// baseActivity is one activity with every field filled in.
func baseActivity(id string) map[string]any {
	return map[string]any{
		"id":              id,
		"type":            "traditionalStrengthTraining",
		"day":             probeDay,
		"points":          map[string]any{"base": 30.0, "bonus": 5.0},
		"durationSeconds": 3600.0,
		"distanceMeters":  1200.0,
		"start":           "2026-10-20T07:30:00Z",
		"details": map[string]any{
			"activeEnergyKilocalories":    400.0,
			"averageHeartRate":            130.0,
			"maximumHeartRate":            170.0,
			"averageSpeedMetersPerSecond": 1.4,
			"elevationGainMeters":         30.0,
			"elevationLossMeters":         28.0,
			"steps":                       5000,
			"averageCadence":              110.0,
			"averagePowerWatts":           180.0,
			"averageMETs":                 6.0,
			"carriedLoadKilograms":        13.6,
			"inclinePercent":              2.0,
			"strollerLoadKilograms":       15.0,
			"exposureTemperatureCelsius":  80.0,
			"weather":                     map[string]any{"temperatureCelsius": 12.0, "humidityPercent": 60.0},
		},
		"hasRoute": true,
		"strength": map[string]any{"exercises": []any{baseExercise()}},
	}
}

// baseExercise is one exercise with every field filled in.
func baseExercise() map[string]any {
	return map[string]any{
		"catalogID":        "6f1c2e3a-1b2c-4d5e-8f90-123456789abc",
		"name":             "Landmine Press",
		"loadIsAssistance": false,
		"sets":             []any{baseSet()},
	}
}

// baseSet is one set with every field filled in.
func baseSet() map[string]any {
	return map[string]any{
		"type":            "dropset",
		"reps":            10,
		"durationSeconds": 40.0,
		"distanceMeters":  20.0,
		"weightKilograms": 25.0,
		"right":           map[string]any{"reps": 8, "durationSeconds": 30.0, "weightKilograms": 20.0},
		"clusters":        []any{map[string]any{"reps": 4, "weightKilograms": 15.0}},
	}
}

// baseChallenge is a valid weekly challenge with every field filled in, as a JSON object.
func baseChallenge() map[string]any {
	return map[string]any{
		"progress":   0.8,
		"score":      400.0,
		"goal":       500.0,
		"tier":       "monster",
		"breakdown":  map[string]any{"exercise": 350.0, "dailyBonus": 30.0, "varietyBonus": 20.0},
		"activities": []any{baseActivity(probeActivity)},
	}
}

// setPath replaces the value at a dotted path such as activities.0.points.base, creating missing objects.
func setPath(t testing.TB, object map[string]any, path string, value any) {
	t.Helper()
	parts := strings.Split(path, ".")
	var current any = object
	for index, part := range parts {
		last := index == len(parts)-1
		switch container := current.(type) {
		case map[string]any:
			if last {
				container[part] = value
				return
			}
			next, ok := container[part]
			if !ok {
				next = map[string]any{}
				container[part] = next
			}
			current = next
		case []any:
			position, err := strconv.Atoi(part)
			if err != nil || position >= len(container) {
				t.Fatalf("bad index %q in %s", part, path)
			}
			if last {
				container[position] = value
				return
			}
			current = container[position]
		default:
			t.Fatalf("cannot descend into %T at %q in %s", current, part, path)
		}
	}
}

// deletePath removes the value at a dotted path.
func deletePath(t testing.TB, object map[string]any, path string) {
	t.Helper()
	parts := strings.Split(path, ".")
	var current any = object
	for _, part := range parts[:len(parts)-1] {
		switch container := current.(type) {
		case map[string]any:
			current = container[part]
		case []any:
			position, err := strconv.Atoi(part)
			if err != nil {
				t.Fatalf("bad index %q in %s", part, path)
			}
			current = container[position]
		}
	}
	container, ok := current.(map[string]any)
	if !ok {
		t.Fatalf("cannot delete %s from %T", path, current)
	}
	delete(container, parts[len(parts)-1])
}

// identifiers returns count fresh activity identifiers.
func identifiers(t testing.TB, count int) []string {
	t.Helper()
	ids := make([]string, count)
	for index := range ids {
		id, err := scoreboard.NewIdentifier()
		if err != nil {
			t.Fatal(err)
		}
		ids[index] = id
	}
	return ids
}

// smallActivity is an activity with only its required fields.
func smallActivity(id string) map[string]any {
	return map[string]any{"id": id, "type": "outdoorRunning", "day": probeDay, "points": map[string]any{"base": 1.0, "bonus": 0.0}}
}

// challengeWithActivities is a valid weekly challenge with count small activities.
func challengeWithActivities(t testing.TB, count int) map[string]any {
	t.Helper()
	activities := make([]any, 0, count)
	for _, id := range identifiers(t, count) {
		activities = append(activities, smallActivity(id))
	}
	return map[string]any{"progress": 0.5, "score": 50.0, "goal": 100.0, "activities": activities}
}

// encodeSigned encodes one polyline coordinate delta in the Encoded Polyline Algorithm Format.
func encodeSigned(value int64) string {
	value <<= 1
	if value < 0 {
		value = ^value
	}
	var text strings.Builder
	for value >= 0x20 {
		text.WriteByte(byte((0x20 | (value & 0x1f)) + 63))
		value >>= 5
	}
	text.WriteByte(byte(value + 63))
	return text.String()
}

// encodePolyline encodes latitude and longitude pairs at precision 5.
func encodePolyline(points [][2]float64) string {
	var text strings.Builder
	var lastLatitude, lastLongitude int64
	for _, point := range points {
		latitude := int64(math.Round(point[0] * 1e5))
		longitude := int64(math.Round(point[1] * 1e5))
		text.WriteString(encodeSigned(latitude - lastLatitude))
		text.WriteString(encodeSigned(longitude - lastLongitude))
		lastLatitude, lastLongitude = latitude, longitude
	}
	return text.String()
}

// shortRoute is a route body of count points a metre apart.
func shortRoute(t testing.TB, count int) []byte {
	t.Helper()
	points := make([][2]float64, count)
	for index := range points {
		points[index] = [2]float64{51.5 + float64(index)*0.00001, -0.12}
	}
	return encode(t, map[string]any{"polyline": encodePolyline(points)})
}

// wideRoute is a route body of count points zig-zagging across the globe, the widest a point can encode.
func wideRoute(t testing.TB, count int) []byte {
	t.Helper()
	points := make([][2]float64, count)
	for index := range points {
		sign := float64(1 - 2*(index%2))
		points[index] = [2]float64{sign * 89.99, sign * 179.99}
	}
	return encode(t, map[string]any{"polyline": encodePolyline(points)})
}

// encodedPNG returns a side by side gradient as PNG.
func encodedPNG(t testing.TB, side int) []byte {
	t.Helper()
	source := image.NewNRGBA(image.Rect(0, 0, side, side))
	for y := range side {
		for x := range side {
			source.Set(x, y, color.NRGBA{R: 200, G: uint8(x), B: uint8(y), A: 255})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, source); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

// leafSet returns the leaf paths of a decoded object as a set.
func leafSet(object map[string]any) map[string]bool {
	set := map[string]bool{}
	for _, path := range sharing.LeafPaths(object) {
		set[path] = true
	}
	return set
}

// describeSharing names a sharing compactly for test names and messages.
func describeSharing(s sharing.Sharing) string {
	return fmt.Sprintf("%s/%s/r%t/s%t/w%t", s.Score, s.Activity, s.Routes, s.StrengthSets, s.StrengthWeights)
}
