package api_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/auth"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
)

// TestBoardCarriesEveryRequiredFieldAndNoSecret checks GET /board against the Board schema.
func TestBoardCarriesEveryRequiredFieldAndNoSecret(t *testing.T) {
	b := newBoard(t, boardOptions{configure: func(document *scoreboard_Board) {
		document.Description = "A board for probing."
		document.LandingMessage = "Welcome, probers."
	}})
	stranger := &phone{board: b}
	result := stranger.get("/board", nil)
	expectStatus(t, result, http.StatusOK)
	if contentType := result.header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("board served as %q", contentType)
	}
	document := result.object(t)
	for _, field := range []string{"id", "name", "hasAvatar", "weekStartDay", "retentionWeeks", "joinMode", "acceptingRequests",
		"passwordRequired", "avatarsAllowed", "maximumSharing", "ranking", "apiVersion", "serverVersion"} {
		if _, ok := document[field]; !ok {
			t.Errorf("board lacks required field %q", field)
		}
	}
	for _, field := range []string{"inviteCode", "passwordHash", "maximumPending", "landingMessage", "showInvite"} {
		if _, ok := document[field]; ok {
			t.Errorf("board exposes %q, which the Board schema does not list", field)
		}
	}
	if document["apiVersion"] != "1.0" {
		t.Errorf("apiVersion = %v, want 1.0", document["apiVersion"])
	}
	if id, _ := document["id"].(string); len(id) != 22 {
		t.Errorf("board id %q is not 22 characters", id)
	}
	if result.header.Get("ETag") == "" {
		t.Errorf("board has no ETag")
	}
}

// scoreboard_Board aliases the board document type for configure callbacks.
type scoreboard_Board = boardDocument

// TestBoardHonoursIfNoneMatch checks the ETag round trip on the unsigned read.
func TestBoardHonoursIfNoneMatch(t *testing.T) {
	b := newBoard(t, boardOptions{})
	stranger := &phone{board: b}
	first := stranger.get("/board", nil)
	expectStatus(t, first, http.StatusOK)
	etag := first.header.Get("ETag")
	again := stranger.get("/board", http.Header{"If-None-Match": {etag}})
	if again.status != http.StatusNotModified {
		t.Fatalf("same ETag answered %d, want 304", again.status)
	}
	if len(again.body) != 0 {
		t.Errorf("304 carried a body: %q", again.body)
	}
	// Either form is accepted, per openapi.yaml's conventions.
	other := "W/" + etag
	if strings.HasPrefix(etag, "W/") {
		other = strings.TrimPrefix(etag, "W/")
	}
	if weak := stranger.get("/board", http.Header{"If-None-Match": {other}}); weak.status != http.StatusNotModified {
		t.Errorf("other ETag form %q answered %d, want 304", other, weak.status)
	}
	if star := stranger.get("/board", http.Header{"If-None-Match": {"*"}}); star.status != http.StatusNotModified {
		t.Errorf("If-None-Match: * answered %d, want 304 (RFC 9110 §13.1.2)", star.status)
	}
	if list := stranger.get("/board", http.Header{"If-None-Match": {`"nope", ` + etag}}); list.status != http.StatusNotModified {
		t.Errorf("ETag inside a list answered %d, want 304", list.status)
	}
	b.update(func(document *boardDocument) { document.Name = "Renamed" })
	if changed := stranger.get("/board", http.Header{"If-None-Match": {etag}}); changed.status != http.StatusOK {
		t.Errorf("stale ETag after a change answered %d, want 200", changed.status)
	}
}

// TestBoardIsGzippedWithWeakETagWhenLarge checks the compression convention on a 1 KB body.
func TestBoardIsGzippedWithWeakETagWhenLarge(t *testing.T) {
	b := newBoard(t, boardOptions{configure: func(document *boardDocument) {
		document.Description = strings.Repeat("Probing boards and their descriptions. ", 13)
	}})
	stranger := &phone{board: b}
	plain := stranger.get("/board", nil)
	expectStatus(t, plain, http.StatusOK)
	if len(plain.body) < 1024 {
		t.Skipf("board body is only %d bytes; cannot reach the 1 KB threshold", len(plain.body))
	}
	if plain.header.Get("Content-Encoding") != "" {
		t.Errorf("gzip sent without Accept-Encoding")
	}
	zipped := stranger.get("/board", http.Header{"Accept-Encoding": {"gzip"}})
	expectStatus(t, zipped, http.StatusOK)
	if zipped.header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", zipped.header.Get("Content-Encoding"))
	}
	if !strings.HasPrefix(zipped.header.Get("ETag"), "W/") {
		t.Errorf("gzipped ETag %q is not weak", zipped.header.Get("ETag"))
	}
	if !strings.Contains(zipped.header.Get("Vary"), "Accept-Encoding") {
		t.Errorf("Vary = %q lacks Accept-Encoding", zipped.header.Get("Vary"))
	}
	strong := plain.header.Get("ETag")
	for _, form := range []string{strong, "W/" + strong} {
		if again := stranger.get("/board", http.Header{"Accept-Encoding": {"gzip"}, "If-None-Match": {form}}); again.status != http.StatusNotModified {
			t.Errorf("gzip client with If-None-Match %q answered %d, want 304", form, again.status)
		}
	}
}

// TestSignedRequestsNeedTheExactMessage checks each line of the signed message is covered.
func TestSignedRequestsNeedTheExactMessage(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Signer", sharing.Lowest)

	cases := []struct {
		// name describes the tampering.
		name string
		// tamper changes the request after it was signed.
		tamper func(r *request)
		// code is the problem code expected, or empty for success.
		code string
	}{
		{"untouched", func(r *request) {}, ""},
		{"method changed", func(r *request) { r.method = "DELETE" }, "unauthenticated"},
		{"query appended", func(r *request) { r.target += "?x=1" }, "unauthenticated"},
		{"trailing slash appended", func(r *request) { r.target += "/" }, "unknownOperation"},
		{"body changed", func(r *request) { r.body = []byte(`{"displayName":"Other"}`) }, "unauthenticated"},
		{"body emptied", func(r *request) { r.body = nil }, "unauthenticated"},
		// PROBE: RFC 9110 §11.1 makes the scheme case-insensitive; the README shows it in one case only.
		{"scheme lowercased", func(r *request) {
			r.header.Set("Authorization", strings.Replace(r.header.Get("Authorization"), "FitHalo-ECDSA", "fithalo-ecdsa", 1))
		}, "unauthenticated"},
		{"scheme replaced", func(r *request) {
			r.header.Set("Authorization", strings.Replace(r.header.Get("Authorization"), "FitHalo-ECDSA", "Bearer", 1))
		}, "unauthenticated"},
		{"header removed", func(r *request) { r.header.Del("Authorization") }, "unauthenticated"},
		{"signature payload bit flipped", func(r *request) {
			value := r.header.Get("Authorization")
			r.header.Set("Authorization", value[:len(value)-1]+flipPayloadBit(value[len(value)-1]))
		}, "unauthenticated"},
		{"signature truncated", func(r *request) {
			value := r.header.Get("Authorization")
			r.header.Set("Authorization", value[:len(value)-1])
		}, "unauthenticated"},
		{"signature padded", func(r *request) {
			r.header.Set("Authorization", r.header.Get("Authorization")+"==")
		}, "unauthenticated"},
		{"member swapped", func(r *request) {
			other := b.newPhone()
			other.mustJoin("Other", sharing.Lowest)
			r.header.Set("Authorization", strings.Replace(r.header.Get("Authorization"), "member="+member.id(), "member="+other.id(), 1))
		}, "unauthenticated"},
		{"nonce changed", func(r *request) {
			value := r.header.Get("Authorization")
			start := strings.Index(value, "nonce=") + len("nonce=")
			nonce := value[start : start+22]
			replacement := strings.Repeat("A", 22)
			if nonce == replacement {
				replacement = strings.Repeat("B", 22)
			}
			r.header.Set("Authorization", strings.Replace(value, "nonce="+nonce, "nonce="+replacement, 1))
		}, "unauthenticated"},
		{"time changed within skew", func(r *request) {
			value := r.header.Get("Authorization")
			now := fmt.Sprint(b.clock().Unix())
			r.header.Set("Authorization", strings.Replace(value, "time="+now, "time="+fmt.Sprint(b.clock().Unix()+1), 1))
		}, "unauthenticated"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := member.prepare("PATCH", "/members/me", []byte(`{"displayName":"Signer"}`), "application/json")
			c.tamper(r)
			result := member.dispatch(r)
			switch c.code {
			case "":
				expectStatus(t, result, http.StatusOK)
			case "unknownOperation":
				expectProblem(t, result, http.StatusNotFound, c.code)
			default:
				expectProblem(t, result, http.StatusUnauthorized, c.code)
			}
		})
	}
}

// TestSignedTargetIncludesTheBasePath checks a board served under a path signs the whole target.
func TestSignedTargetIncludesTheBasePath(t *testing.T) {
	b := newBoard(t, boardOptions{basePath: "/scores"})
	member := b.newPhone()
	member.mustJoin("Pathed", sharing.Lowest)
	expectStatus(t, member.get("/members/me", nil), http.StatusOK)

	// A signature over the target without the base path must fail: the doc says the path is signed exactly as on
	// the request line, base path included.
	r := member.prepare("GET", "/members/me", nil, "")
	value, err := auth.AuthorizationValue(member.key, "GET", "/api/v1/members/me", b.clock(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.header.Set("Authorization", value)
	expectProblem(t, member.dispatch(r), http.StatusUnauthorized, "unauthenticated")
}

// TestQueryStringIsPartOfTheSignedTarget checks a request with a query verifies when the query was signed.
func TestQueryStringIsPartOfTheSignedTarget(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Query", sharing.Lowest)
	r := member.prepare("GET", "/members/me?cache=1&x=%20y", nil, "")
	expectStatus(t, member.dispatch(r), http.StatusOK)
}

// TestClockSkewBoundary checks 300 seconds either way is accepted and 301 is clockSkew.
func TestClockSkewBoundary(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Clock", sharing.Lowest)
	cases := []struct {
		// name describes the offset.
		name string
		// skew is the signing time's offset from the server's clock.
		skew time.Duration
		// ok is whether the request should verify.
		ok bool
	}{
		{"299s behind", -299 * time.Second, true},
		{"300s behind", -300 * time.Second, true},
		{"301s behind", -301 * time.Second, false},
		{"299s ahead", 299 * time.Second, true},
		{"300s ahead", 300 * time.Second, true},
		{"301s ahead", 301 * time.Second, false},
		{"a day ahead", 24 * time.Hour, false},
		{"epoch", -time.Duration(probeNow.Unix()) * time.Second, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			member.skew = c.skew
			result := member.get("/members/me", nil)
			if c.ok {
				expectStatus(t, result, http.StatusOK)
			} else {
				expectProblem(t, result, http.StatusUnauthorized, "clockSkew")
			}
		})
	}
}

// TestSubSecondClockShiftsTheWindowByUnderASecond records that the skew is compared with sub-second precision:
// a request signed exactly 300 whole seconds earlier is refused once the server's clock passes the second.
// PROBE: the README gives time in whole seconds; comparing whole seconds would accept this request.
func TestSubSecondClockShiftsTheWindowByUnderASecond(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Clock", sharing.Lowest)
	b.advance(999 * time.Millisecond)
	member.skew = -300 * time.Second
	expectProblem(t, member.get("/members/me", nil), http.StatusUnauthorized, "clockSkew")
	member.skew = -299 * time.Second
	expectStatus(t, member.get("/members/me", nil), http.StatusOK)
}

// TestReplayIsRefusedAndOutlivesTheSkewWindow checks a nonce is refused the second time, even at the edge.
func TestReplayIsRefusedAndOutlivesTheSkewWindow(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Replay", sharing.Lowest)
	r := member.prepare("GET", "/members/me", nil, "")
	expectStatus(t, member.dispatch(r), http.StatusOK)
	expectProblem(t, member.dispatch(r), http.StatusUnauthorized, "replayed")
	// 299 seconds later the signature's time is still inside the window, and the nonce must still be remembered.
	b.advance(299 * time.Second)
	expectProblem(t, member.dispatch(r), http.StatusUnauthorized, "replayed")
	// Past the window the time itself is refused, so the nonce need not be kept beyond 600 seconds.
	b.advance(2 * time.Second)
	expectProblem(t, member.dispatch(r), http.StatusUnauthorized, "clockSkew")
}

// TestReplayWithinWindowAfterServerClockMovesBack checks a nonce stays refused if the clock goes backwards.
func TestReplayWithinWindowAfterServerClockMovesBack(t *testing.T) {
	b := newBoard(t, boardOptions{noLimits: true})
	member := b.newPhone()
	member.mustJoin("Replay", sharing.Lowest)
	r := member.prepare("GET", "/members/me", nil, "")
	expectStatus(t, member.dispatch(r), http.StatusOK)
	b.advance(-200 * time.Second)
	expectProblem(t, member.dispatch(r), http.StatusUnauthorized, "replayed")
}

// TestNonceIsScopedToTheMember checks two members may use the same nonce.
func TestNonceIsScopedToTheMember(t *testing.T) {
	b := newBoard(t, boardOptions{})
	one, two := b.newPhone(), b.newPhone()
	one.mustJoin("One", sharing.Lowest)
	two.mustJoin("Two", sharing.Lowest)
	nonce := strings.Repeat("n", 22)
	for _, member := range []*phone{one, two} {
		seconds := b.clock().Unix()
		message := auth.Message("GET", "/api/v1/members/me", seconds, nonce, nil)
		signature, err := auth.Sign(member.key, message)
		if err != nil {
			t.Fatal(err)
		}
		r := member.prepare("GET", "/members/me", nil, "")
		r.header.Set("Authorization", fmt.Sprintf("FitHalo-ECDSA member=%s,time=%d,nonce=%s,signature=%s",
			member.id(), seconds, nonce, base64.RawURLEncoding.EncodeToString(signature)))
		expectStatus(t, member.dispatch(r), http.StatusOK)
	}
}

// TestConcurrentReplaysAdmitExactlyOne checks the nonce check is atomic under concurrency.
func TestConcurrentReplaysAdmitExactlyOne(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Racer", sharing.Lowest)
	r := member.prepare("GET", "/members/me", nil, "")
	const attempts = 32
	var wg sync.WaitGroup
	statuses := make([]int, attempts)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = member.dispatch(r).status
		}(i)
	}
	wg.Wait()
	accepted := 0
	for _, status := range statuses {
		if status == http.StatusOK {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("%d of %d identical requests were accepted, want exactly 1", accepted, attempts)
	}
}

// TestUnsignedRequestsNeedNoNonce checks an unsigned bad request does not burn a nonce the member can use.
func TestUnsignedRequestsNeedNoNonce(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Burn", sharing.Lowest)
	r := member.prepare("GET", "/members/me", nil, "")
	value := r.header.Get("Authorization")
	// A forged header with the right member, time and nonce but a garbage signature must not record the nonce.
	forged := value[:strings.Index(value, "signature=")] + "signature=" + strings.Repeat("A", 86)
	bad := member.prepare("GET", "/members/me", nil, "")
	bad.header.Set("Authorization", forged)
	expectProblem(t, member.dispatch(bad), http.StatusUnauthorized, "unauthenticated")
	expectStatus(t, member.dispatch(r), http.StatusOK)
}

// TestHeaderParameterOrderAndSpacesAreFree checks the header's parameters may come in any order with spaces.
func TestHeaderParameterOrderAndSpacesAreFree(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Order", sharing.Lowest)
	sign := func(t *testing.T) (int64, string, string) {
		seconds := b.clock().Unix()
		nonceBytes := make([]byte, 16)
		rand.Read(nonceBytes)
		nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
		message := auth.Message("GET", "/api/v1/members/me", seconds, nonce, nil)
		signature, err := auth.Sign(member.key, message)
		if err != nil {
			t.Fatal(err)
		}
		return seconds, nonce, base64.RawURLEncoding.EncodeToString(signature)
	}
	formats := []struct {
		// name describes the layout.
		name string
		// format lays the parameters out, with verbs for member, time, nonce, signature.
		format string
		// ok is whether the header must parse.
		ok bool
	}{
		{"documented order", "FitHalo-ECDSA member=%[1]s,time=%[2]d,nonce=%[3]s,signature=%[4]s", true},
		{"spaces after commas", "FitHalo-ECDSA member=%[1]s, time=%[2]d, nonce=%[3]s, signature=%[4]s", true},
		{"reversed", "FitHalo-ECDSA signature=%[4]s,nonce=%[3]s,time=%[2]d,member=%[1]s", true},
		{"many spaces", "FitHalo-ECDSA member=%[1]s,     time=%[2]d,nonce=%[3]s,signature=%[4]s", true},
		{"duplicate time", "FitHalo-ECDSA member=%[1]s,time=%[2]d,time=%[2]d,nonce=%[3]s,signature=%[4]s", false},
		{"missing nonce", "FitHalo-ECDSA member=%[1]s,time=%[2]d,signature=%[4]s", false},
		{"unknown parameter", "FitHalo-ECDSA member=%[1]s,time=%[2]d,nonce=%[3]s,signature=%[4]s,extra=1", false},
		{"two spaces after scheme", "FitHalo-ECDSA  member=%[1]s,time=%[2]d,nonce=%[3]s,signature=%[4]s", true},
		{"uppercase parameter name", "FitHalo-ECDSA Member=%[1]s,time=%[2]d,nonce=%[3]s,signature=%[4]s", false},
		{"quoted value", "FitHalo-ECDSA member=\"%[1]s\",time=%[2]d,nonce=%[3]s,signature=%[4]s", false},
		{"time with plus", "FitHalo-ECDSA member=%[1]s,time=+%[2]d,nonce=%[3]s,signature=%[4]s", false},
		{"time with leading zero", "FitHalo-ECDSA member=%[1]s,time=0%[2]d,nonce=%[3]s,signature=%[4]s", false},
		{"nonce padded", "FitHalo-ECDSA member=%[1]s,time=%[2]d,nonce=%[3]s==,signature=%[4]s", false},
		{"trailing comma", "FitHalo-ECDSA member=%[1]s,time=%[2]d,nonce=%[3]s,signature=%[4]s,", false},
		{"space before comma", "FitHalo-ECDSA member=%[1]s ,time=%[2]d,nonce=%[3]s,signature=%[4]s", false},
		{"tab after comma", "FitHalo-ECDSA member=%[1]s,\ttime=%[2]d,nonce=%[3]s,signature=%[4]s", false},
	}
	for _, f := range formats {
		t.Run(f.name, func(t *testing.T) {
			seconds, nonce, signature := sign(t)
			r := member.prepare("GET", "/members/me", nil, "")
			r.header.Set("Authorization", fmt.Sprintf(f.format, member.id(), seconds, nonce, signature))
			result := member.dispatch(r)
			if f.ok {
				expectStatus(t, result, http.StatusOK)
			} else {
				expectProblem(t, result, http.StatusUnauthorized, "unauthenticated")
			}
		})
	}
}

// TestNonCanonicalBase64IsNotAnAlias checks a signature or nonce with different trailing bits is not accepted as
// the same value, which would let a replay slip past the nonce check.
func TestNonCanonicalBase64IsNotAnAlias(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Canon", sharing.Lowest)
	seconds := b.clock().Unix()
	// 16 bytes encode to 22 characters whose last character carries 2 payload bits and 4 padding bits.
	nonceBytes := []byte("0123456789abcdef")
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	message := auth.Message("GET", "/api/v1/members/me", seconds, nonce, nil)
	signature, err := auth.Sign(member.key, message)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(signature)
	send := func(nonceValue string) reply {
		r := member.prepare("GET", "/members/me", nil, "")
		r.header.Set("Authorization", fmt.Sprintf("FitHalo-ECDSA member=%s,time=%d,nonce=%s,signature=%s",
			member.id(), seconds, nonceValue, encoded))
		return member.dispatch(r)
	}
	expectStatus(t, send(nonce), http.StatusOK)
	// The same 16 bytes with a different final character (padding bits set) decode the same under lenient
	// decoders; the server must treat it as a replay or refuse it, never accept it as new.
	alias := nonce[:21] + flipPaddingBits(nonce[21])
	result := send(alias)
	if result.status == http.StatusOK {
		t.Fatalf("nonce alias %q accepted after %q: a replay with non-canonical base64", alias, nonce)
	}
}

// flipPayloadBit returns a base64url character whose highest bit, a payload bit in a final character, is flipped.
func flipPayloadBit(last byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	index := strings.IndexByte(alphabet, last)
	return string(alphabet[index^0x20])
}

// flipPaddingBits returns a base64url character with the same payload bits but padding bits set.
func flipPaddingBits(last byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	index := strings.IndexByte(alphabet, last)
	return string(alphabet[index|0x0f])
}

// TestJoinMustBeSignedByTheKeyItRegisters checks the header's member equals the body key's identifier.
func TestJoinMustBeSignedByTheKeyItRegisters(t *testing.T) {
	b := newBoard(t, boardOptions{})
	signer, other := b.newPhone(), b.newPhone()
	body := signer.joinBody("Impostor", sharing.Lowest)
	body["publicKey"] = other.publicKey()
	expectProblem(t, signer.json("POST", "/members", body), http.StatusUnauthorized, "unauthenticated")
	if result := other.get("/members/me", nil); result.status != http.StatusUnauthorized {
		t.Errorf("the other key became a member through a request it did not sign: %d", result.status)
	}
}

// TestMemberIDIsDerivedFromTheKey checks the identifier the server assigns matches the README.
func TestMemberIDIsDerivedFromTheKey(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	result := member.join("Derived", sharing.Lowest)
	expectStatus(t, result, http.StatusCreated)
	raw, err := base64.RawURLEncoding.DecodeString(member.publicKey())
	if err != nil || len(raw) != 65 || raw[0] != 4 {
		t.Fatalf("public key is not a 65-byte uncompressed point: %v %d", err, len(raw))
	}
	sum := sha256.Sum256(raw)
	want := base64.RawURLEncoding.EncodeToString(sum[:16])
	if got := result.object(t)["id"]; got != want {
		t.Errorf("id = %v, want %s", got, want)
	}
}

// TestEmptyBodyHashIsTheSHA256OfNothing pins the documented constant into the signed message.
func TestEmptyBodyHashIsTheSHA256OfNothing(t *testing.T) {
	message := string(auth.Message("GET", "/x", 1, "n", nil))
	lines := strings.Split(message, "\n")
	if len(lines) != 5 {
		t.Fatalf("message has %d lines, want 5: %q", len(lines), message)
	}
	empty := sha256.Sum256(nil)
	if lines[4] != hex.EncodeToString(empty[:]) || lines[4] != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("body hash line = %q", lines[4])
	}
	if lines[0] != "FitHalo-ECDSA-1" || lines[1] != "GET" || lines[2] != "/x" || lines[3] != "1:n" {
		t.Errorf("message lines = %q", lines)
	}
}

// TestStrangerAndWrongSignatureGetTheSameAnswer checks membership cannot be probed through the detail text.
func TestStrangerAndWrongSignatureGetTheSameAnswer(t *testing.T) {
	b := newBoard(t, boardOptions{})
	member := b.newPhone()
	member.mustJoin("Known", sharing.Lowest)
	stranger := b.newPhone()
	strangerReply := stranger.get("/members/me", nil)
	r := member.prepare("GET", "/members/me", nil, "")
	value := r.header.Get("Authorization")
	r.header.Set("Authorization", value[:strings.Index(value, "signature=")]+"signature="+strings.Repeat("A", 86))
	forgedReply := member.dispatch(r)
	expectProblem(t, strangerReply, http.StatusUnauthorized, "unauthenticated")
	expectProblem(t, forgedReply, http.StatusUnauthorized, "unauthenticated")
	if string(strangerReply.body) != string(forgedReply.body) {
		t.Errorf("stranger got %s but a forged signature got %s", strangerReply.body, forgedReply.body)
	}
}
