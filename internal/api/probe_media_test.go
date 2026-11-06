package api_test

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/ratelimit"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
)

// examplePolyline is Google's documented example, three points.
const examplePolyline = "_p~iF~ps|U_ulLnnqC_mqNvxq`@"

// routePath is the caller's route path for an activity.
func routePath(activity string) string {
	return "/members/me/weekly-challenges/" + probeWeek + "/activities/" + activity + "/route"
}

// readRoutePath is another member's route path for an activity.
func readRoutePath(member, activity string) string {
	return "/members/" + member + "/weekly-challenges/" + probeWeek + "/activities/" + activity + "/route"
}

// encodePolyline encodes points as an Encoded Polyline at precision 5, independently of the server.
func encodePolyline(points [][2]float64) string {
	var out strings.Builder
	var lastLat, lastLng int64
	encode := func(value int64) {
		value <<= 1
		if value < 0 {
			value = ^value
		}
		for value >= 0x20 {
			out.WriteByte(byte((0x20 | (value & 0x1f)) + 63))
			value >>= 5
		}
		out.WriteByte(byte(value + 63))
	}
	for _, point := range points {
		lat := int64(point[0]*1e5 + 0.5)
		lng := int64(point[1]*1e5 + 0.5)
		if point[0] < 0 {
			lat = int64(point[0]*1e5 - 0.5)
		}
		if point[1] < 0 {
			lng = int64(point[1]*1e5 - 0.5)
		}
		encode(lat - lastLat)
		encode(lng - lastLng)
		lastLat, lastLng = lat, lng
	}
	return out.String()
}

// polylineOf returns a valid polyline of n points near Zurich.
func polylineOf(n int) string {
	points := make([][2]float64, n)
	for i := range points {
		points[i] = [2]float64{47.37 + float64(i)*0.0001, 8.54 + float64(i%7)*0.0002}
	}
	return encodePolyline(points)
}

// bulkyPolylineOf returns a valid polyline of n points whose deltas span the globe, so each costs 12 bytes.
func bulkyPolylineOf(n int) string {
	points := make([][2]float64, n)
	for i := range points {
		if i%2 == 0 {
			points[i] = [2]float64{-89.9 + float64(i%13)*0.001, -179.9}
		} else {
			points[i] = [2]float64{89.9, 179.9 - float64(i%11)*0.001}
		}
	}
	return encodePolyline(points)
}

// TestAvatarLifecycle checks PUT, GET, ETag, DELETE and hasAvatar across the API.
func TestAvatarLifecycle(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Face", sharing.Lowest)
	viewer := b.newPhone()
	viewer.mustJoin("Viewer", sharing.Lowest)

	expectProblem(t, viewer.get("/members/"+member.id()+"/avatar", nil), http.StatusNotFound, "notFound")
	expectStatus(t, member.send("DELETE", "/members/me/avatar", nil, ""), http.StatusNoContent)

	for name, upload := range map[string]struct {
		// data is the image.
		data []byte
		// contentType is the declared type.
		contentType string
	}{
		"png":  {pngBytes(t, 700, 300), "image/png"},
		"jpeg": {jpegBytes(t, 300, 700), "image/jpeg"},
	} {
		t.Run(name, func(t *testing.T) {
			expectStatus(t, member.send("PUT", "/members/me/avatar", upload.data, upload.contentType), http.StatusNoContent)
			me := member.get("/members/me", nil).object(t)
			if me["hasAvatar"] != true {
				t.Errorf("hasAvatar = %v after upload", me["hasAvatar"])
			}
			result := viewer.get("/members/"+member.id()+"/avatar", nil)
			expectStatus(t, result, http.StatusOK)
			if result.header.Get("Content-Type") != "image/jpeg" {
				t.Errorf("avatar served as %q", result.header.Get("Content-Type"))
			}
			picture, err := jpeg.Decode(bytes.NewReader(result.body))
			if err != nil {
				t.Fatalf("avatar is not a JPEG: %v", err)
			}
			bounds := picture.Bounds()
			if bounds.Dx() > 512 || bounds.Dy() > 512 {
				t.Errorf("avatar is %dx%d, over 512", bounds.Dx(), bounds.Dy())
			}
			if bounds.Dx() != 512 && bounds.Dy() != 512 {
				t.Logf("PROBE: a %s larger than 512 was scaled to %dx%d, not to fit 512", name, bounds.Dx(), bounds.Dy())
			}
			etag := result.header.Get("ETag")
			if etag == "" {
				t.Errorf("avatar has no ETag")
			}
			expectStatus(t, viewer.get("/members/"+member.id()+"/avatar", http.Header{"If-None-Match": {etag}}), http.StatusNotModified)
			if gz := viewer.get("/members/"+member.id()+"/avatar", http.Header{"Accept-Encoding": {"gzip"}}); gz.header.Get("Content-Encoding") == "gzip" {
				t.Logf("PROBE: a JPEG avatar is gzipped, which gains nothing")
			}
		})
	}
	// The scoreboard's summary says hasAvatar too.
	if board := viewer.get("/weekly-challenges/"+probeWeek, nil); !strings.Contains(string(board.body), `"hasAvatar":true`) {
		t.Errorf("scoreboard summary does not show the avatar: %s", board.body)
	}
	expectStatus(t, member.send("DELETE", "/members/me/avatar", nil, ""), http.StatusNoContent)
	expectProblem(t, viewer.get("/members/"+member.id()+"/avatar", nil), http.StatusNotFound, "notFound")
	if me := member.get("/members/me", nil).object(t); me["hasAvatar"] != false {
		t.Errorf("hasAvatar = %v after delete", me["hasAvatar"])
	}
	expectStatus(t, member.send("DELETE", "/members/me/avatar", nil, ""), http.StatusNoContent)
}

// TestAvatarRefusals checks media types, size, dimensions and the board's switch.
func TestAvatarRefusals(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Face", sharing.Lowest)
	png := pngBytes(t, 8, 8)
	cases := []struct {
		// name describes the upload.
		name string
		// data is the body.
		data []byte
		// contentType is the declared type.
		contentType string
		// status is the expected status.
		status int
		// code is the expected problem code.
		code string
	}{
		{"gif", append([]byte("GIF89a"), png...), "image/gif", http.StatusUnsupportedMediaType, "unsupportedMediaType"},
		{"no type", png, "", http.StatusUnsupportedMediaType, "unsupportedMediaType"},
		{"json", png, "application/json", http.StatusUnsupportedMediaType, "unsupportedMediaType"},
		{"png declared jpeg", png, "image/jpeg", http.StatusUnprocessableEntity, "imageInvalid"},
		{"jpeg declared png", jpegBytes(t, 8, 8), "image/png", http.StatusUnprocessableEntity, "imageInvalid"},
		{"truncated png", png[:len(png)/2], "image/png", http.StatusUnprocessableEntity, "imageInvalid"},
		{"empty", nil, "image/png", http.StatusUnprocessableEntity, "imageInvalid"},
		{"too many bytes", bytes.Repeat([]byte{0}, 256*1024+1), "image/png", http.StatusRequestEntityTooLarge, "payloadTooLarge"},
		{"2049 wide", pngBytes(t, 2049, 1), "image/png", http.StatusUnprocessableEntity, "imageInvalid"},
		{"2049 tall", pngBytes(t, 1, 2049), "image/png", http.StatusUnprocessableEntity, "imageInvalid"},
		{"too many pixels", pngBytes(t, 2048, 1537), "image/png", http.StatusUnprocessableEntity, "imageInvalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := member.send("PUT", "/members/me/avatar", c.data, c.contentType)
			if result.status == http.StatusNoContent {
				t.Fatalf("accepted")
			}
			if c.name == "too many pixels" && result.status == http.StatusRequestEntityTooLarge {
				t.Skipf("the 2048x1537 PNG is %d bytes, over the body limit before its pixels are counted", len(c.data))
			}
			expectProblem(t, result, c.status, c.code)
		})
	}
	// The largest allowed image, and a PNG with an alpha channel, both pass.
	expectStatus(t, member.send("PUT", "/members/me/avatar", pngBytes(t, 2048, 1536), "image/png"), http.StatusNoContent)
	// Parameters on the media type are allowed by HTTP.
	expectStatus(t, member.send("PUT", "/members/me/avatar", png, "image/png; charset=binary"), http.StatusNoContent)
	expectStatus(t, member.send("PUT", "/members/me/avatar", png, "IMAGE/PNG"), http.StatusNoContent)

	b.update(func(document *boardDocument) { document.AvatarsAllowed = false })
	expectProblem(t, member.send("PUT", "/members/me/avatar", png, "image/png"), http.StatusForbidden, "avatarsNotAllowed")
	viewer := b.newPhone()
	viewer.mustJoin("Viewer", sharing.Lowest)
	expectProblem(t, viewer.get("/members/"+member.id()+"/avatar", nil), http.StatusNotFound, "notFound")
	if me := member.get("/members/me", nil).object(t); me["hasAvatar"] != false {
		t.Errorf("hasAvatar = %v with avatars off", me["hasAvatar"])
	}
	if board := viewer.get("/board", nil).object(t); board["avatarsAllowed"] != false {
		t.Errorf("board still says avatarsAllowed")
	}
}

// TestAvatarPixelLimitIsReachableUnderTheByteLimit makes sure the dimension limits can be hit at all.
func TestAvatarPixelLimitIsReachableUnderTheByteLimit(t *testing.T) {
	// A solid PNG compresses extremely well, so even 2048x1537 fits in 256 KB.
	data := pngBytes(t, 2048, 1537)
	if len(data) > 256*1024 {
		t.Skipf("solid PNG is %d bytes", len(data))
	}
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Face", sharing.Lowest)
	expectProblem(t, member.send("PUT", "/members/me/avatar", data, "image/png"), http.StatusUnprocessableEntity, "imageInvalid")
}

// TestRouteLifecycle checks routes follow the weekly challenge, hasRoute, sharing and deletion.
func TestRouteLifecycle(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Runner", sharing.Highest)
	viewer := b.newPhone()
	viewer.mustJoin("Viewer", sharing.Lowest)
	route := map[string]any{"polyline": examplePolyline}

	// The weekly challenge goes first.
	expectProblem(t, member.json("PUT", routePath(activityID(1)), route), http.StatusNotFound, "notFound")
	challenge := everythingChallenge("2026-10-20", activityID(1))
	expectStatus(t, member.putWeeklyChallenge(probeWeek, challenge), http.StatusOK)
	// The activity must be in the weekly challenge.
	expectProblem(t, member.json("PUT", routePath(activityID(2)), route), http.StatusNotFound, "notFound")
	// PROBE: hasRoute is the client's claim, stored as sent: before any route is stored the weekly challenge already says
	// hasRoute true and the route answers 404. The schema calls it "Whether a route can be fetched".
	read := viewer.get("/members/"+member.id()+"/weekly-challenges/"+probeWeek, nil)
	expectStatus(t, read, http.StatusOK)
	if strings.Contains(string(read.body), `"hasRoute":true`) {
		t.Logf("PROBE: hasRoute true before any route was stored")
	}
	expectProblem(t, viewer.get(readRoutePath(member.id(), activityID(1)), nil), http.StatusNotFound, "notFound")

	expectStatus(t, member.json("PUT", routePath(activityID(1)), route), http.StatusNoContent)
	read = viewer.get("/members/"+member.id()+"/weekly-challenges/"+probeWeek, nil)
	if !strings.Contains(string(read.body), `"hasRoute":true`) {
		t.Errorf("hasRoute not true after storing a route: %s", read.body)
	}
	got := viewer.get(readRoutePath(member.id(), activityID(1)), nil)
	expectStatus(t, got, http.StatusOK)
	if got.object(t)["polyline"] != examplePolyline {
		t.Errorf("route = %s", got.body)
	}
	etag := got.header.Get("ETag")
	if etag == "" {
		t.Errorf("route has no ETag")
	}
	expectStatus(t, viewer.get(readRoutePath(member.id(), activityID(1)), http.Header{"If-None-Match": {etag}}), http.StatusNotModified)

	// Putting the weekly challenge again with the activity keeps the route; without it, deletes it.
	expectStatus(t, member.putWeeklyChallenge(probeWeek, challenge), http.StatusOK)
	expectStatus(t, viewer.get(readRoutePath(member.id(), activityID(1)), nil), http.StatusOK)
	expectStatus(t, member.putWeeklyChallenge(probeWeek, everythingChallenge("2026-10-20", activityID(2))), http.StatusOK)
	expectProblem(t, viewer.get(readRoutePath(member.id(), activityID(1)), nil), http.StatusNotFound, "notFound")
	// Putting the first activity back does not resurrect the route.
	expectStatus(t, member.putWeeklyChallenge(probeWeek, challenge), http.StatusOK)
	expectProblem(t, viewer.get(readRoutePath(member.id(), activityID(1)), nil), http.StatusNotFound, "notFound")
	read = viewer.get("/members/"+member.id()+"/weekly-challenges/"+probeWeek, nil)
	if strings.Contains(string(read.body), `"hasRoute":true`) {
		t.Logf("PROBE: hasRoute true for a route the server deleted, because the client said so again")
	}

	// Delete is idempotent.
	expectStatus(t, member.json("PUT", routePath(activityID(1)), route), http.StatusNoContent)
	expectStatus(t, member.send("DELETE", routePath(activityID(1)), nil, ""), http.StatusNoContent)
	expectStatus(t, member.send("DELETE", routePath(activityID(1)), nil, ""), http.StatusNoContent)
	expectProblem(t, viewer.get(readRoutePath(member.id(), activityID(1)), nil), http.StatusNotFound, "notFound")
}

// TestRoutesNeedRouteSharing checks routesNotShared on upload and notFound on read after lowering.
func TestRoutesNeedRouteSharing(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Runner", sharing.Highest)
	viewer := b.newPhone()
	viewer.mustJoin("Viewer", sharing.Lowest)
	expectStatus(t, member.putWeeklyChallenge(probeWeek, everythingChallenge("2026-10-20", activityID(1))), http.StatusOK)
	expectStatus(t, member.json("PUT", routePath(activityID(1)), map[string]any{"polyline": examplePolyline}), http.StatusNoContent)

	noRoutes := sharing.Highest
	noRoutes.Routes = false
	expectStatus(t, member.json("PATCH", "/members/me", map[string]any{"sharing": noRoutes}), http.StatusOK)
	expectProblem(t, viewer.get(readRoutePath(member.id(), activityID(1)), nil), http.StatusNotFound, "notFound")
	expectProblem(t, member.json("PUT", routePath(activityID(1)), map[string]any{"polyline": examplePolyline}), http.StatusForbidden, "routesNotShared")
	read := viewer.get("/members/"+member.id()+"/weekly-challenges/"+probeWeek, nil)
	if strings.Contains(string(read.body), "hasRoute") {
		t.Errorf("hasRoute served without route sharing: %s", read.body)
	}
	// Raising sharing again does not bring the route back.
	expectStatus(t, member.json("PATCH", "/members/me", map[string]any{"sharing": sharing.Highest}), http.StatusOK)
	expectProblem(t, viewer.get(readRoutePath(member.id(), activityID(1)), nil), http.StatusNotFound, "notFound")

	// The board's maximum works the same way.
	expectStatus(t, member.json("PUT", routePath(activityID(1)), map[string]any{"polyline": examplePolyline}), http.StatusNoContent)
	b.update(func(document *boardDocument) { document.MaximumSharing = noRoutes })
	expectProblem(t, viewer.get(readRoutePath(member.id(), activityID(1)), nil), http.StatusNotFound, "notFound")
	expectProblem(t, member.json("PUT", routePath(activityID(1)), map[string]any{"polyline": examplePolyline}), http.StatusForbidden, "routesNotShared")
	// A member at points level without routes: routesNotShared beats notFound for a missing activity? Record.
	result := member.json("PUT", routePath(activityID(9)), map[string]any{"polyline": examplePolyline})
	t.Logf("PROBE: route for a missing activity without route sharing answers %d %s", result.status, result.code())
}

// TestRouteValidation checks the Route schema and limits.
func TestRouteValidation(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Runner", sharing.Highest)
	expectStatus(t, member.putWeeklyChallenge(probeWeek, everythingChallenge("2026-10-20", activityID(1))), http.StatusOK)
	cases := []struct {
		// name describes the body.
		name string
		// body is the route body.
		body any
		// status is the expected status.
		status int
		// code is the expected problem code, or empty for success.
		code string
	}{
		{"example", map[string]any{"polyline": examplePolyline}, http.StatusNoContent, ""},
		{"two points", map[string]any{"polyline": polylineOf(2)}, http.StatusNoContent, ""},
		{"one point", map[string]any{"polyline": polylineOf(1)}, http.StatusBadRequest, "badRequest"},
		{"empty polyline", map[string]any{"polyline": ""}, http.StatusBadRequest, "badRequest"},
		{"missing polyline", map[string]any{}, http.StatusBadRequest, "badRequest"},
		{"polyline not a string", map[string]any{"polyline": 5}, http.StatusBadRequest, "badRequest"},
		{"malformed", map[string]any{"polyline": "_p~iF~ps|U_ulLnnqC_mqNvxq"}, http.StatusBadRequest, "badRequest"},
		{"character out of range", map[string]any{"polyline": "_p~iF~ps|U _ulLnnqC"}, http.StatusBadRequest, "badRequest"},
		{"latitude out of range", map[string]any{"polyline": encodePolyline([][2]float64{{91, 0}, {90, 0}})}, http.StatusBadRequest, "badRequest"},
		{"longitude out of range", map[string]any{"polyline": encodePolyline([][2]float64{{0, 180.5}, {0, 180}})}, http.StatusBadRequest, "badRequest"},
		{"5000 points", map[string]any{"polyline": polylineOf(5000)}, http.StatusNoContent, ""},
		{"5001 points", map[string]any{"polyline": polylineOf(5001)}, http.StatusBadRequest, "badRequest"},
		{"body array", []any{}, http.StatusBadRequest, "badRequest"},
		{"unknown field", map[string]any{"polyline": examplePolyline, "color": "red"}, http.StatusNoContent, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := member.json("PUT", routePath(activityID(1)), c.body)
			if c.code == "" {
				expectStatus(t, result, c.status)
				return
			}
			if result.status == http.StatusNoContent {
				t.Fatalf("accepted")
			}
			expectProblem(t, result, c.status, c.code)
		})
	}
	// 256 KB is the route body limit.
	big := []byte(`{"polyline":"` + examplePolyline + `","pad":"` + strings.Repeat("x", 256*1024) + `"}`)
	expectProblem(t, member.send("PUT", routePath(activityID(1)), big, "application/json"), http.StatusRequestEntityTooLarge, "payloadTooLarge")
}

// TestWeeklyChallengeStorageCap checks the 1 MB cap on a weekly challenge and its routes together.
func TestWeeklyChallengeStorageCap(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Hoarder", sharing.Highest)
	const routes = 24
	var activities []any
	for i := 0; i < routes; i++ {
		activities = append(activities, map[string]any{"id": activityID(i), "type": "outdoorRunning", "day": "2026-10-20", "points": map[string]any{"base": 1, "bonus": 0}})
	}
	expectStatus(t, member.putWeeklyChallenge(probeWeek, map[string]any{"progress": 0, "activities": activities}), http.StatusOK)
	// 5,000 points at 12 bytes each is about 60 KB, the largest route the point limit allows.
	route := map[string]any{"polyline": bulkyPolylineOf(5000)}
	encoded, _ := jsonBytes(route)
	if len(encoded) < 50*1024 || len(encoded) > 256*1024 {
		t.Fatalf("route body is %d bytes; want between 50 KB and 256 KB", len(encoded))
	}
	stored := 0
	for i := 0; i < routes; i++ {
		result := member.json("PUT", routePath(activityID(i)), route)
		if result.status == http.StatusNoContent {
			stored++
			continue
		}
		expectProblem(t, result, http.StatusRequestEntityTooLarge, "payloadTooLarge")
		break
	}
	if stored*len(encoded) > 1<<20 {
		t.Errorf("%d routes of %d bytes stored, over 1 MB", stored, len(encoded))
	}
	if stored == routes {
		t.Errorf("all %d routes stored: %d bytes, cap not enforced", routes, routes*len(encoded))
	}
	// Replacing a stored route counts only the new one, so re-putting the first route is fine.
	expectStatus(t, member.json("PUT", routePath(activityID(0)), route), http.StatusNoContent)
	// The admin's usage view agrees with what is stored.
	usage, err := b.service.Usage(t.Context(), member.id())
	if err != nil {
		t.Fatal(err)
	}
	if usage.Total() > 1<<20 {
		t.Errorf("usage reports %d bytes, over 1 MB", usage.Total())
	}
	// A weekly challenge upload that would push the total past the cap is refused with payloadTooLarge too.
	padded := map[string]any{"progress": 0, "activities": activities, "pad": strings.Repeat("p", 300*1024)}
	if result := member.putWeeklyChallenge(probeWeek, padded); result.status == http.StatusOK {
		after, _ := b.service.Usage(t.Context(), member.id())
		if after.Total() > 1<<20 {
			t.Errorf("weekly challenge accepted and total is %d bytes, over 1 MB", after.Total())
		}
	} else {
		expectProblem(t, result, http.StatusRequestEntityTooLarge, "payloadTooLarge")
	}
}

// TestLoweringSharingDeletesAboveTheNewLevelInEveryChallenge checks the PATCH's promise across several weeks.
func TestLoweringSharingDeletesAboveTheNewLevelInEveryChallenge(t *testing.T) {
	b := newBoard(t, boardOptions{configure: func(document *boardDocument) { document.RetentionWeeks = 8 }})
	member := b.newPhone()
	member.mustJoin("Lower", sharing.Highest)
	viewer := b.newPhone()
	viewer.mustJoin("Viewer", sharing.Lowest)
	weeks := []string{"2026-09-28", "2026-10-12", "2026-10-19", "2026-10-26"}
	for i, week := range weeks {
		day := week[:8] + fmt.Sprintf("%02d", atoi(week[8:])+1)
		expectStatus(t, member.putWeeklyChallenge(week, everythingChallenge(day, activityID(i))), http.StatusOK)
		expectStatus(t, member.json("PUT", "/members/me/weekly-challenges/"+week+"/activities/"+activityID(i)+"/route", map[string]any{"polyline": examplePolyline}), http.StatusNoContent)
	}
	lowered := sharing.Sharing{Score: sharing.ScoreDetails, Activity: sharing.ActivityPoints, StrengthSets: true}
	expectStatus(t, member.json("PATCH", "/members/me", map[string]any{"sharing": lowered}), http.StatusOK)
	list := loadSpecAllowlist(t)
	for i, week := range weeks {
		read := viewer.get("/members/"+member.id()+"/weekly-challenges/"+week, nil)
		expectStatus(t, read, http.StatusOK)
		var memberWeeklyChallenge struct {
			Challenge map[string]any `json:"challenge"`
		}
		read.decode(t, &memberWeeklyChallenge)
		var leaves []string
		leafPaths(memberWeeklyChallenge.Challenge, "", &leaves)
		for _, leaf := range leaves {
			if !list.allows(leaf, lowered) {
				t.Errorf("week %s still serves %q", week, leaf)
			}
		}
		expectProblem(t, viewer.get("/members/"+member.id()+"/weekly-challenges/"+week+"/activities/"+activityID(i)+"/route", nil), http.StatusNotFound, "notFound")
		if strings.Contains(string(read.body), "weightKilograms") {
			t.Errorf("week %s still serves weights", week)
		}
	}
	// What the store holds is also stripped, not only what is served: raising sharing must not bring it back.
	expectStatus(t, member.json("PATCH", "/members/me", map[string]any{"sharing": sharing.Highest}), http.StatusOK)
	read := viewer.get("/members/"+member.id()+"/weekly-challenges/"+weeks[0], nil)
	if strings.Contains(string(read.body), "weightKilograms") || strings.Contains(string(read.body), "averageHeartRate") {
		t.Errorf("raising sharing restored stripped data: %s", read.body)
	}
}

// atoi parses a small decimal.
func atoi(value string) int {
	n := 0
	for _, r := range value {
		n = n*10 + int(r-'0')
	}
	return n
}

// TestRateLimitAnswersWithRetryAfter checks 429, its code and the Retry-After header.
func TestRateLimitAnswersWithRetryAfter(t *testing.T) {
	b := newBoard(t, boardOptions{limiter: ratelimit.New(3, 60), joinLimiter: ratelimit.New(2, 60)})
	stranger := &phone{board: b}
	for i := 0; i < 3; i++ {
		expectStatus(t, stranger.get("/board", nil), http.StatusOK)
	}
	limited := stranger.get("/board", nil)
	expectProblem(t, limited, http.StatusTooManyRequests, "rateLimited")
	retry := limited.header.Get("Retry-After")
	if retry == "" || atoi(retry) < 1 {
		t.Errorf("Retry-After = %q, want a positive integer", retry)
	}
	// Waiting as long as told, the next request passes.
	b.advance(timeSeconds(atoi(retry)))
	expectStatus(t, stranger.get("/board", nil), http.StatusOK)
	b.advance(time.Minute)

	// Joining has its own limiter, so a join storm does not lock readers out and vice versa.
	first, second, third := b.newPhone(), b.newPhone(), b.newPhone()
	expectStatus(t, first.join("One", sharing.Lowest), http.StatusCreated)
	expectStatus(t, second.join("Two", sharing.Lowest), http.StatusCreated)
	expectProblem(t, third.join("Three", sharing.Lowest), http.StatusTooManyRequests, "rateLimited")
	// The member limiter was not drained by the joins.
	expectStatus(t, first.get("/members/me", nil), http.StatusOK)
}

// TestUnknownPathsAndMethods checks the fallback shape outside the defined operations.
func TestUnknownPathsAndMethods(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Lost", sharing.Lowest)
	for _, path := range []string{"/", "/nothing", "/board/", "/members", "/members/", "/weekly-challenges", "/weekly-challenges/" + probeWeek + "/extra", "/members/me/weekly-challenges", "/members/me/avatar/x"} {
		result := member.get(path, nil)
		if result.status != http.StatusNotFound && result.status != http.StatusMethodNotAllowed {
			t.Errorf("GET %s answered %d: %s", path, result.status, result.body)
			continue
		}
		if !strings.HasPrefix(result.header.Get("Content-Type"), "application/problem+json") {
			t.Errorf("GET %s answered %d as %q, not a problem", path, result.status, result.header.Get("Content-Type"))
		}
	}
	// Outside /api/v1 entirely, and a wrong version.
	for _, target := range []string{"/", "/api", "/api/v2/board", "/API/V1/board"} {
		request, _ := http.NewRequest("GET", b.server.URL+target, nil)
		result := perform(t, request)
		if result.status != http.StatusNotFound {
			t.Errorf("GET %s answered %d: %s", target, result.status, result.body)
		}
	}
}

// TestProblemBodiesAreNotSniffable checks error responses carry nosniff so a browser never renders them.
func TestProblemBodiesAreNotSniffable(t *testing.T) {
	b := newBoard(t, boardOptions{})
	result := (&phone{board: b}).get("/members/me", nil)
	expectProblem(t, result, http.StatusUnauthorized, "unauthenticated")
	if result.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", result.header.Get("X-Content-Type-Options"))
	}
	if auth := result.header.Get("WWW-Authenticate"); auth == "" {
		t.Logf("PROBE: 401 without WWW-Authenticate, which RFC 9110 §15.5.2 requires")
	}
}

// TestConcurrentPutWeeklyChallengeKeepsAConsistentChallenge hammers one weekly challenge and checks the result is one of the uploads.
func TestConcurrentPutWeeklyChallengeKeepsAConsistentChallenge(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Racer", sharing.Highest)
	const writers = 16
	done := make(chan int, writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			challenge := everythingChallenge("2026-10-20", activityID(i))
			challenge["progress"] = float64(i)
			done <- member.putWeeklyChallenge(probeWeek, challenge).status
		}(i)
	}
	for i := 0; i < writers; i++ {
		if status := <-done; status != http.StatusOK {
			t.Errorf("a concurrent PUT answered %d", status)
		}
	}
	read := member.get("/members/"+member.id()+"/weekly-challenges/"+probeWeek, nil)
	expectStatus(t, read, http.StatusOK)
	var memberWeeklyChallenge struct {
		Challenge struct {
			Progress   float64 `json:"progress"`
			Activities []struct {
				ID string `json:"id"`
			} `json:"activities"`
		} `json:"challenge"`
	}
	read.decode(t, &memberWeeklyChallenge)
	if len(memberWeeklyChallenge.Challenge.Activities) != 1 || memberWeeklyChallenge.Challenge.Activities[0].ID != activityID(int(memberWeeklyChallenge.Challenge.Progress)) {
		t.Errorf("stored weekly challenge mixes uploads: %s", read.body)
	}
}
