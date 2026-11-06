package auth_test

import (
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/auth"
)

// TestConstantsMatchContract pins the documented scheme and windows.
func TestConstantsMatchContract(t *testing.T) {
	if auth.Scheme != "FitHalo-ECDSA" {
		t.Errorf("Scheme = %q", auth.Scheme)
	}
	if auth.MaximumSkew != 300*time.Second {
		t.Errorf("MaximumSkew = %v", auth.MaximumSkew)
	}
	if auth.NonceLifetime != 600*time.Second {
		t.Errorf("NonceLifetime = %v", auth.NonceLifetime)
	}
	if auth.NonceLifetime < 2*auth.MaximumSkew {
		t.Error("a nonce can be forgotten while its request is still within skew")
	}
}

// TestWithinSkewBoundaries checks the window is exactly ±300 seconds, inclusive at both edges.
func TestWithinSkewBoundaries(t *testing.T) {
	now := time.Unix(1792800000, 0)
	cases := []struct {
		// name describes the offset.
		name string

		// offset is added to now in seconds.
		offset int64

		// want is whether the time is within skew.
		want bool
	}{
		{name: "exact", offset: 0, want: true},
		{name: "299 behind", offset: -299, want: true},
		{name: "300 behind", offset: -300, want: true},
		{name: "301 behind", offset: -301, want: false},
		{name: "299 ahead", offset: 299, want: true},
		{name: "300 ahead", offset: 300, want: true},
		{name: "301 ahead", offset: 301, want: false},
		{name: "one day behind", offset: -86400, want: false},
		{name: "one day ahead", offset: 86400, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := auth.WithinSkew(now.Unix()+tc.offset, now); got != tc.want {
				t.Errorf("WithinSkew(now%+d) = %v, want %v", tc.offset, got, tc.want)
			}
		})
	}
}

// TestWithinSkewExtremeValues makes sure arithmetic at the ends of int64 cannot wrap into the window.
func TestWithinSkewExtremeValues(t *testing.T) {
	for _, now := range []time.Time{time.Unix(0, 0), time.Unix(1792800000, 0), time.Unix(math.MaxInt64/2, 0), time.Unix(-1792800000, 0)} {
		for _, seconds := range []int64{math.MinInt64, math.MinInt64 + 1, math.MinInt64 + 300, math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64 - 300} {
			if auth.WithinSkew(seconds, now) {
				t.Errorf("WithinSkew(%d, %d) = true", seconds, now.Unix())
			}
		}
	}
	if !auth.WithinSkew(0, time.Unix(0, 0)) || !auth.WithinSkew(-300, time.Unix(0, 0)) || auth.WithinSkew(-301, time.Unix(0, 0)) {
		t.Error("the window around the epoch is wrong")
	}
	if !auth.WithinSkew(math.MaxInt64, time.Unix(math.MaxInt64, 0)) {
		t.Log("PROBE: a time of MaxInt64 is refused even when now is MaxInt64")
	}
}

// TestWithinSkewSubSecondNow documents whether the fraction of a second in now counts.
func TestWithinSkewSubSecondNow(t *testing.T) {
	now := time.Unix(1792800000, 999_999_999)
	// PROBE: the contract says within 300 seconds of the server's clock; a request 300.999... seconds
	// behind sits on the edge depending on whether now is truncated to whole seconds first.
	t.Logf("PROBE: 300s behind a now of x.999s accepted=%v; 300s ahead accepted=%v", auth.WithinSkew(now.Unix()-300, now), auth.WithinSkew(now.Unix()+300, now))
	t.Logf("PROBE: 301s ahead a now of x.999s accepted=%v", auth.WithinSkew(now.Unix()+301, now))
}

// TestWithinSkewIgnoresLocation gives the same answer whatever the zone of now.
func TestWithinSkewIgnoresLocation(t *testing.T) {
	zone := time.FixedZone("far", 13*3600)
	now := time.Date(2026, 3, 29, 1, 30, 0, 0, zone)
	for offset := int64(-310); offset <= 310; offset++ {
		utc := auth.WithinSkew(now.Unix()+offset, now.UTC())
		local := auth.WithinSkew(now.Unix()+offset, now.In(zone))
		if utc != local {
			t.Fatalf("offset %d: UTC %v, zone %v", offset, utc, local)
		}
	}
}

// TestNonceCacheFirstUseOnly accepts a nonce once and refuses it afterwards within the lifetime.
func TestNonceCacheFirstUseOnly(t *testing.T) {
	cache := auth.NewNonceCache()
	now := time.Unix(1792800000, 0)
	if !cache.Use("m", "n", now) {
		t.Fatal("first use refused")
	}
	for _, delay := range []time.Duration{0, time.Nanosecond, time.Second, 299 * time.Second, 300 * time.Second, 301 * time.Second, 599 * time.Second, 599*time.Second + 999*time.Millisecond} {
		if cache.Use("m", "n", now.Add(delay)) {
			t.Errorf("nonce accepted again after %v", delay)
		}
	}
}

// TestNonceCacheMembersAreIndependent keeps one member's nonces apart from another's.
func TestNonceCacheMembersAreIndependent(t *testing.T) {
	cache := auth.NewNonceCache()
	now := time.Unix(1792800000, 0)
	if !cache.Use("alice", "n", now) || !cache.Use("bob", "n", now) {
		t.Fatal("the same nonce was refused for a different member")
	}
	if cache.Use("alice", "n", now) || cache.Use("bob", "n", now) {
		t.Fatal("a used nonce was accepted again")
	}
	if !cache.Use("alice", "m", now) {
		t.Fatal("a fresh nonce was refused")
	}
	// Member and nonce must not be joined into one key that another pair can collide with.
	if !cache.Use("a", "bn", now) || !cache.Use("ab", "n", now) || !cache.Use("", "abn", now) || !cache.Use("abn", "", now) {
		t.Fatal("member and nonce collide when concatenated")
	}
	if !cache.Use("a\x00", "n", now) || !cache.Use("a", "\x00n", now) {
		t.Fatal("member and nonce collide around a NUL")
	}
}

// TestNonceCacheExpiry documents the exact second at which a nonce is forgotten.
func TestNonceCacheExpiry(t *testing.T) {
	now := time.Unix(1792800000, 0)
	for _, delay := range []time.Duration{auth.NonceLifetime - time.Second, auth.NonceLifetime, auth.NonceLifetime + time.Nanosecond, auth.NonceLifetime + time.Second} {
		cache := auth.NewNonceCache()
		cache.Use("m", "n", now)
		again := cache.Use("m", "n", now.Add(delay))
		switch {
		case delay < auth.NonceLifetime && again:
			t.Errorf("nonce accepted again after %v, inside the lifetime", delay)
		case delay > auth.NonceLifetime && !again:
			t.Errorf("nonce still refused after %v, past the lifetime", delay)
		case delay == auth.NonceLifetime:
			// PROBE: the contract says "in the last 600 seconds"; the boundary second is unspecified.
			t.Logf("PROBE: nonce reusable at exactly NonceLifetime=%v", again)
		}
	}
}

// TestNonceOutlivesSkewWindow checks that no request still within skew can have its nonce forgotten.
func TestNonceOutlivesSkewWindow(t *testing.T) {
	for _, nanos := range []int{0, 500_000_000, 999_999_999} {
		accepted := time.Unix(1792800000, int64(nanos))
		for offset := int64(-300); offset <= 300; offset++ {
			requestTime := accepted.Unix() + offset
			if !auth.WithinSkew(requestTime, accepted) {
				// A fraction of a second in now pushes the -300 edge out of the window; such a request is refused.
				continue
			}
			cache := auth.NewNonceCache()
			if !cache.Use("m", "n", accepted) {
				t.Fatal("first use refused")
			}
			for delay := int64(0); delay <= 1000; delay++ {
				replayAt := accepted.Add(time.Duration(delay) * time.Second)
				if !auth.WithinSkew(requestTime, replayAt) {
					break
				}
				if cache.Use("m", "n", replayAt) {
					t.Fatalf("request signed at %+ds, accepted at .%09d, replays %ds later while still within skew", offset, nanos, delay)
				}
			}
		}
	}
}

// TestNonceCacheRefusedUseDoesNotExtendLifetime documents whether a refused replay refreshes the record.
func TestNonceCacheRefusedUseDoesNotExtendLifetime(t *testing.T) {
	cache := auth.NewNonceCache()
	now := time.Unix(1792800000, 0)
	cache.Use("m", "n", now)
	if cache.Use("m", "n", now.Add(500*time.Second)) {
		t.Fatal("replay accepted")
	}
	// PROBE: the contract counts from acceptance; the nonce was not accepted at +500s, so by +601s it has
	// left the window. A cache that refreshes on refusal still refuses it, which is safe but undocumented.
	t.Logf("PROBE: nonce reusable at +601s after a refused replay at +500s: %v", cache.Use("m", "n", now.Add(601*time.Second)))
}

// TestNonceCacheClockGoesBackwards keeps refusing a nonce when now is earlier than its record.
func TestNonceCacheClockGoesBackwards(t *testing.T) {
	cache := auth.NewNonceCache()
	now := time.Unix(1792800000, 0)
	if !cache.Use("m", "n", now) {
		t.Fatal("first use refused")
	}
	for _, back := range []time.Duration{time.Nanosecond, time.Second, 299 * time.Second, 300 * time.Second, 599 * time.Second, 600 * time.Second, 601 * time.Second, time.Hour, 24 * time.Hour} {
		if cache.Use("m", "n", now.Add(-back)) {
			t.Errorf("nonce accepted again with the clock %v behind its record", back)
		}
	}
}

// TestNonceCacheClockStepsForwardThenBack documents what a forward purge followed by a backward clock step allows.
func TestNonceCacheClockStepsForwardThenBack(t *testing.T) {
	cache := auth.NewNonceCache()
	now := time.Unix(1792800000, 0)
	cache.Use("alice", "n1", now)
	if !cache.Use("bob", "n2", now.Add(2*time.Hour)) {
		t.Fatal("a fresh nonce was refused")
	}
	// PROBE: a server clock that steps two hours ahead and then back lets alice's nonce through again while
	// her request's time is still within skew of the restored clock. The contract assumes a sane clock.
	t.Logf("PROBE: nonce accepted again after the clock stepped forward and back: %v", cache.Use("alice", "n1", now.Add(time.Second)))
}

// TestNonceCacheForwardJumpPurgesOnlyExpired checks a long gap frees old nonces without touching fresh ones.
func TestNonceCacheForwardJumpPurgesOnlyExpired(t *testing.T) {
	cache := auth.NewNonceCache()
	now := time.Unix(1792800000, 0)
	cache.Use("m", "old", now)
	cache.Use("m", "fresh", now.Add(500*time.Second))
	later := now.Add(700 * time.Second)
	if !cache.Use("m", "old", later) {
		t.Error("an expired nonce was still refused after a long gap")
	}
	if cache.Use("m", "fresh", later) {
		t.Error("a live nonce was forgotten when an older one expired")
	}
}

// TestNonceCacheManyNoncesPerMember remembers thousands of nonces for one member.
func TestNonceCacheManyNoncesPerMember(t *testing.T) {
	cache := auth.NewNonceCache()
	now := time.Unix(1792800000, 0)
	nonces := make([]string, 5000)
	for i := range nonces {
		nonces[i] = strings.Repeat("A", 20) + string(rune('A'+i%26)) + string(rune('A'+(i/26)%26))
	}
	seen := map[string]bool{}
	for i, nonce := range nonces {
		if seen[nonce] {
			continue
		}
		seen[nonce] = true
		if !cache.Use("m", nonce, now.Add(time.Duration(i)*time.Millisecond)) {
			t.Fatalf("distinct nonce %d refused", i)
		}
	}
	for nonce := range seen {
		if cache.Use("m", nonce, now.Add(10*time.Second)) {
			t.Fatalf("nonce %q forgotten while live", nonce)
		}
	}
}

// TestNonceCacheOddStrings documents that Use places no constraints on its strings.
func TestNonceCacheOddStrings(t *testing.T) {
	cache := auth.NewNonceCache()
	now := time.Unix(1792800000, 0)
	long := strings.Repeat("n", 1<<20)
	t.Logf("PROBE: empty member and nonce first use=%v second=%v", cache.Use("", "", now), cache.Use("", "", now))
	t.Logf("PROBE: 1 MiB nonce first use=%v second=%v", cache.Use("m", long, now), cache.Use("m", long, now))
	t.Logf("PROBE: invalid UTF-8 nonce first use=%v second=%v", cache.Use("m", "\xff\xfe", now), cache.Use("m", "\xff\xfe", now))
}

// TestNonceCacheConcurrentSameNonce lets 100 goroutines race for one nonce and expects exactly one winner.
func TestNonceCacheConcurrentSameNonce(t *testing.T) {
	for round := 0; round < 20; round++ {
		cache := auth.NewNonceCache()
		now := time.Unix(1792800000, 0)
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		start := make(chan struct{})
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if cache.Use("m", "n", now.Add(time.Duration(i)*time.Millisecond)) {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
		if wins != 1 {
			t.Fatalf("round %d: %d goroutines won the same nonce", round, wins)
		}
	}
}

// TestNonceCacheConcurrentDistinct hammers the cache with distinct members and nonces under the race detector.
func TestNonceCacheConcurrentDistinct(t *testing.T) {
	cache := auth.NewNonceCache()
	now := time.Unix(1792800000, 0)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			member := strings.Repeat(string(rune('a'+g)), 22)
			for i := 0; i < 500; i++ {
				nonce := strings.Repeat("A", 18) + string(rune('A'+i%26)) + string(rune('A'+(i/26)%26)) + "AA"
				at := now.Add(time.Duration(i) * time.Second)
				if !cache.Use(member, nonce, at) {
					t.Errorf("goroutine %d: fresh nonce %d refused", g, i)
					return
				}
				if cache.Use(member, nonce, at) {
					t.Errorf("goroutine %d: nonce %d accepted twice", g, i)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestAuthorizationValueRoundTrip parses and verifies the header a client builds.
func TestAuthorizationValueRoundTrip(t *testing.T) {
	key := probeKey(t)
	publicKey, raw, err := auth.ParsePublicKey(auth.EncodePublicKey(&key.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		// name describes the request.
		name string

		// method is the HTTP method.
		method string

		// target is the request target.
		target string

		// body is the request body.
		body []byte
	}{
		{name: "GET no body", method: "GET", target: "/api/v1/members/me"},
		{name: "PUT body", method: "PUT", target: "/api/v1/members/me/weekly-challenges/2026-10-19", body: []byte(`{"progress":0.5}`)},
		{name: "POST empty body", method: "POST", target: "/api/v1/members", body: []byte{}},
		{name: "query", method: "GET", target: "/api/v1/board?week=2026-10-19&x=%2F"},
		{name: "non-ASCII", method: "DELETE", target: "/api/v1/ü"},
	}
	now := time.Unix(1792800000, 123_456_789)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, err := auth.AuthorizationValue(key, tc.method, tc.target, now, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(value, auth.Scheme+" ") {
				t.Fatalf("header %q does not start with the scheme", value)
			}
			if strings.ContainsAny(value, "\r\n\"") {
				t.Fatalf("header %q contains a quote or line break", value)
			}
			header, err := auth.ParseHeader(value)
			if err != nil {
				t.Fatal(err)
			}
			if header.Member != auth.MemberID(raw) {
				t.Errorf("member %q, want %q", header.Member, auth.MemberID(raw))
			}
			if header.Time != now.Unix() {
				t.Errorf("time %d, want %d", header.Time, now.Unix())
			}
			if len(header.Nonce) != 22 || strings.ContainsAny(header.Nonce, "+/=") {
				t.Errorf("nonce %q is not 22 base64url characters", header.Nonce)
			}
			if len(header.Signature) != 64 {
				t.Errorf("signature is %d bytes", len(header.Signature))
			}
			message := auth.Message(tc.method, tc.target, header.Time, header.Nonce, tc.body)
			if !auth.Verify(publicKey, message, header.Signature) {
				t.Error("the header's signature does not verify over the request")
			}
			if !auth.WithinSkew(header.Time, now) {
				t.Error("the header's time is not within skew of the time it was built")
			}
		})
	}
}

// TestAuthorizationValueNonceIsFresh expects a different nonce on every call, retries included.
func TestAuthorizationValueNonceIsFresh(t *testing.T) {
	key := probeKey(t)
	now := time.Unix(1792800000, 0)
	cache := auth.NewNonceCache()
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		value, err := auth.AuthorizationValue(key, "GET", "/api/v1/members/me", now, nil)
		if err != nil {
			t.Fatal(err)
		}
		header, err := auth.ParseHeader(value)
		if err != nil {
			t.Fatal(err)
		}
		if seen[header.Nonce] {
			t.Fatalf("nonce %q repeated on call %d", header.Nonce, i)
		}
		seen[header.Nonce] = true
		if !cache.Use(header.Member, header.Nonce, now) {
			t.Fatalf("nonce %q refused by a cache that had not seen it", header.Nonce)
		}
	}
}

// TestAuthorizationValueLowerCaseMethod documents what a client gets when it passes a lower-case method.
func TestAuthorizationValueLowerCaseMethod(t *testing.T) {
	key := probeKey(t)
	now := time.Unix(1792800000, 0)
	value, err := auth.AuthorizationValue(key, "put", "/api/v1/members/me", now, nil)
	if err != nil {
		t.Fatal(err)
	}
	header, err := auth.ParseHeader(value)
	if err != nil {
		t.Fatal(err)
	}
	upper := auth.Verify(&key.PublicKey, auth.Message("PUT", "/api/v1/members/me", header.Time, header.Nonce, nil), header.Signature)
	t.Logf("PROBE: header built with method \"put\" verifies against the upper-case message: %v", upper)
}
