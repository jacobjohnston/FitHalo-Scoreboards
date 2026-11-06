package ratelimit_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/ratelimit"
)

// start is the fixed instant every probe begins at.
var start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// drain takes every token from key at now and returns how many were allowed.
func drain(limiter *ratelimit.Limiter, key string, now time.Time, attempts int) int {
	allowed := 0
	for range attempts {
		if ok, _ := limiter.Allow(key, now); ok {
			allowed++
		}
	}
	return allowed
}

// TestBurstExactly allows exactly burst requests at one instant, then refuses with a retry of at least one second.
func TestBurstExactly(t *testing.T) {
	for _, burst := range []int{1, 3, 50} {
		limiter := ratelimit.New(burst, 60)
		for index := range burst {
			if ok, retry := limiter.Allow("k", start); !ok {
				t.Fatalf("burst %d: request %d refused with retry %d", burst, index+1, retry)
			}
		}
		ok, retry := limiter.Allow("k", start)
		if ok {
			t.Fatalf("burst %d: request %d allowed", burst, burst+1)
		}
		if retry < 1 {
			t.Fatalf("burst %d: retry %d, want at least 1", burst, retry)
		}
	}
}

// TestAllowedReportsZeroOrPositive records what the seconds value is while tokens remain.
func TestAllowedReportsZeroOrPositive(t *testing.T) {
	// PROBE: the doc only defines the seconds when none is left; record what an allowed call reports.
	limiter := ratelimit.New(3, 60)
	_, retry := limiter.Allow("k", start)
	t.Logf("allowed call reports %d seconds", retry)
	if retry < 0 {
		t.Fatalf("allowed call reported negative seconds %d", retry)
	}
}

// TestRetryIsExactAtOnePerSecond expects (false, 1) at the instant of exhaustion and success one second later.
func TestRetryIsExactAtOnePerSecond(t *testing.T) {
	limiter := ratelimit.New(1, 60)
	limiter.Allow("k", start)
	if ok, retry := limiter.Allow("k", start); ok || retry != 1 {
		t.Fatalf("at exhaustion: (%v, %d), want (false, 1)", ok, retry)
	}
	if ok, _ := limiter.Allow("k", start.Add(999*time.Millisecond)); ok {
		t.Fatal("allowed 1 ms before the token refilled")
	}
	if ok, _ := limiter.Allow("k", start.Add(time.Second)); !ok {
		t.Fatal("refused at exactly one second")
	}
}

// TestRetryAtOnePerMinute records whether the retry is 60 and that waiting it succeeds.
func TestRetryAtOnePerMinute(t *testing.T) {
	limiter := ratelimit.New(1, 1)
	limiter.Allow("k", start)
	ok, retry := limiter.Allow("k", start)
	if ok {
		t.Fatal("second request allowed")
	}
	t.Logf("one per minute: retry %d", retry)
	if retry != 60 {
		t.Errorf("retry %d, want 60", retry)
	}
	if ok, _ := limiter.Allow("k", start.Add(time.Duration(retry)*time.Second)); !ok {
		t.Fatalf("still refused after waiting the %d seconds it asked for", retry)
	}
}

// TestRetryAfterIsHonest checks, across rates and offsets, that waiting retry seconds succeeds and retry-1 does not.
func TestRetryAfterIsHonest(t *testing.T) {
	for _, test := range []struct {
		// name describes the rate.
		name string

		// burst and perMinute configure the limiter.
		burst, perMinute int

		// offset is how long after exhaustion the refused call is made.
		offset time.Duration
	}{
		{name: "60 per minute at exhaustion", burst: 1, perMinute: 60, offset: 0},
		{name: "60 per minute mid-second", burst: 1, perMinute: 60, offset: 400 * time.Millisecond},
		{name: "1 per minute at exhaustion", burst: 1, perMinute: 1, offset: 0},
		{name: "1 per minute after 59.5 s", burst: 1, perMinute: 1, offset: 59500 * time.Millisecond},
		{name: "1 per minute after 30 s", burst: 1, perMinute: 1, offset: 30 * time.Second},
		{name: "7 per minute at exhaustion", burst: 1, perMinute: 7, offset: 0},
		{name: "7 per minute after 3 s", burst: 1, perMinute: 7, offset: 3 * time.Second},
		{name: "13 per minute after 1.1 s", burst: 2, perMinute: 13, offset: 1100 * time.Millisecond},
		{name: "120 per minute at exhaustion", burst: 120, perMinute: 120, offset: 0},
		{name: "120 per minute after 250 ms", burst: 120, perMinute: 120, offset: 250 * time.Millisecond},
		{name: "1000 per minute at exhaustion", burst: 10, perMinute: 1000, offset: 0},
		{name: "30 per minute after 1 s", burst: 1, perMinute: 30, offset: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			limiter := ratelimit.New(test.burst, test.perMinute)
			drain(limiter, "k", start, test.burst)
			asked := start.Add(test.offset)
			ok, retry := limiter.Allow("k", asked)
			if ok {
				t.Skip("a token had already refilled by the offset")
			}
			if retry < 1 {
				t.Fatalf("retry %d, want at least 1", retry)
			}
			if ok, again := limiter.Allow("k", asked.Add(time.Duration(retry)*time.Second)); !ok {
				t.Errorf("refused after waiting the %d seconds it asked for (now says %d)", retry, again)
			}
			if retry > 1 {
				if ok, _ := limiter.Allow("k", asked.Add(time.Duration(retry-1)*time.Second)); ok {
					// PROBE: Retry-After may legitimately round up; an allow one second early is lenient, not wrong.
					t.Logf("allowed one second before the retry it asked for (%d); Retry-After rounds up", retry)
				}
			}
		})
	}
}

// TestRefillDoesNotExceedBurst allows only burst requests after an hour idle.
func TestRefillDoesNotExceedBurst(t *testing.T) {
	limiter := ratelimit.New(3, 600)
	drain(limiter, "k", start, 3)
	if allowed := drain(limiter, "k", start.Add(time.Hour), 10); allowed != 3 {
		t.Fatalf("after an hour %d allowed, want 3", allowed)
	}
	if allowed := drain(limiter, "fresh", start.Add(24*365*time.Hour), 10); allowed != 3 {
		t.Fatalf("fresh key a year on: %d allowed, want 3", allowed)
	}
}

// TestFractionalRefill accumulates half a token a second at 30 per minute.
func TestFractionalRefill(t *testing.T) {
	limiter := ratelimit.New(1, 30)
	limiter.Allow("k", start)
	if ok, _ := limiter.Allow("k", start.Add(time.Second)); ok {
		t.Fatal("a token appeared after one second at half a token a second")
	}
	if ok, _ := limiter.Allow("k", start.Add(1999*time.Millisecond)); ok {
		t.Fatal("a token appeared 1 ms early")
	}
	if ok, _ := limiter.Allow("k", start.Add(2*time.Second)); !ok {
		t.Fatal("no token after two seconds at half a token a second")
	}
	if ok, _ := limiter.Allow("k", start.Add(2*time.Second)); ok {
		t.Fatal("two tokens after two seconds")
	}
}

// TestFractionIsKeptAcrossCalls checks a refused call does not discard the partial token accumulated so far.
func TestFractionIsKeptAcrossCalls(t *testing.T) {
	limiter := ratelimit.New(1, 30)
	limiter.Allow("k", start)
	for millis := 100; millis < 2000; millis += 100 {
		if ok, _ := limiter.Allow("k", start.Add(time.Duration(millis)*time.Millisecond)); ok {
			t.Fatalf("allowed at %d ms", millis)
		}
	}
	if ok, _ := limiter.Allow("k", start.Add(2*time.Second)); !ok {
		t.Fatal("polling every 100 ms lost the fractional refill; no token at two seconds")
	}
}

// TestSteadyRateOverAMinute checks perMinute requests spaced evenly after the burst are all allowed, and no extra.
func TestSteadyRateOverAMinute(t *testing.T) {
	limiter := ratelimit.New(5, 60)
	drain(limiter, "k", start, 5)
	allowed := 0
	for second := 1; second <= 60; second++ {
		if ok, _ := limiter.Allow("k", start.Add(time.Duration(second)*time.Second)); ok {
			allowed++
		}
	}
	if allowed != 60 {
		t.Fatalf("%d of 60 evenly spaced requests allowed", allowed)
	}
	if ok, _ := limiter.Allow("k", start.Add(60*time.Second)); ok {
		t.Fatal("a 61st request in the minute was allowed")
	}
}

// TestKeysAreIndependent exhausts one key without touching another.
func TestKeysAreIndependent(t *testing.T) {
	limiter := ratelimit.New(2, 60)
	drain(limiter, "10.0.0.1", start, 2)
	if ok, _ := limiter.Allow("10.0.0.1", start); ok {
		t.Fatal("exhausted key allowed")
	}
	if ok, _ := limiter.Allow("10.0.0.2", start); !ok {
		t.Fatal("other key refused")
	}
	if ok, _ := limiter.Allow("10.0.0.1 ", start); !ok {
		t.Fatal("key with trailing space refused; expected a separate bucket")
	}
}

// TestIPv6SharesSlash64 puts every address in one /64 into one bucket and the next /64 into another.
func TestIPv6SharesSlash64(t *testing.T) {
	limiter := ratelimit.New(2, 1)
	for _, key := range []string{"2001:db8::1", "2001:db8::2"} {
		if ok, _ := limiter.Allow(key, start); !ok {
			t.Fatalf("%s refused within the burst", key)
		}
	}
	for _, key := range []string{"2001:db8:0:0:ffff::1", "2001:DB8::3", "2001:0db8:0000:0000:0000:0000:0000:0004", "2001:db8::ffff:ffff:ffff:ffff"} {
		if ok, _ := limiter.Allow(key, start); ok {
			t.Errorf("%s escaped its /64's exhausted bucket", key)
		}
	}
	if ok, _ := limiter.Allow("2001:db8:0:1::1", start); !ok {
		t.Fatal("the next /64 was refused")
	}
}

// TestAddressForms records whether ports, brackets, zones and IPv4-mapped forms share a bucket with the bare address.
func TestAddressForms(t *testing.T) {
	for _, test := range []struct {
		// name describes the pair.
		name string

		// first and second are the two key spellings.
		first, second string

		// wantShared says whether the contract implies one bucket; nil records only.
		wantShared *bool
	}{
		{name: "IPv6 with port and brackets", first: "2001:db8::1", second: "[2001:db8::1]:443"},
		{name: "IPv6 with zone", first: "fe80::1", second: "fe80::2%en0"},
		{name: "IPv4 with port", first: "10.0.0.1", second: "10.0.0.1:1234"},
		{name: "IPv4-mapped IPv6", first: "10.0.0.1", second: "::ffff:10.0.0.1"},
		{name: "IPv4-mapped IPv6 hex form", first: "10.0.0.1", second: "::ffff:a00:1"},
		{name: "IPv6 loopback and IPv4 loopback", first: "::1", second: "127.0.0.1", wantShared: new(bool)},
	} {
		t.Run(test.name, func(t *testing.T) {
			limiter := ratelimit.New(1, 1)
			limiter.Allow(test.first, start)
			ok, _ := limiter.Allow(test.second, start)
			shared := !ok
			// PROBE: the doc only promises /64 sharing for IPv6; everything else here is recorded, not required.
			t.Logf("%q and %q share a bucket: %v", test.first, test.second, shared)
			if test.wantShared != nil && shared != *test.wantShared {
				t.Fatalf("shared %v, want %v", shared, *test.wantShared)
			}
		})
	}
}

// TestIPv4MappedDoesNotCollapseAllIPv4 checks ::ffff:10.0.0.1 and ::ffff:10.0.0.2 are not one /64 bucket.
func TestIPv4MappedDoesNotCollapseAllIPv4(t *testing.T) {
	limiter := ratelimit.New(1, 1)
	limiter.Allow("::ffff:10.0.0.1", start)
	if ok, _ := limiter.Allow("::ffff:10.0.0.2", start); !ok {
		t.Fatal("two different IPv4-mapped addresses share one bucket; every IPv4 client behind the /64 rule would be one bucket")
	}
}

// TestNonAddressKeys treats arbitrary strings as independent plain keys.
func TestNonAddressKeys(t *testing.T) {
	limiter := ratelimit.New(1, 1)
	keys := []string{"", "garbage", "garbage2", ":", "::", ":::", "[", "]", "[]:80", "%", "2001:db8::/64", "1.2.3.4.5", "::ffff:", "\x00", "ü"}
	for _, key := range keys {
		if ok, _ := limiter.Allow(key, start); !ok {
			t.Errorf("first request for %q refused", key)
		}
	}
	for _, key := range keys {
		if ok, _ := limiter.Allow(key, start); ok {
			t.Errorf("second request for %q allowed; its bucket was not its own", key)
		}
	}
}

// TestNewPanicsOnNonPositive expects a panic for zero and negative configuration.
func TestNewPanicsOnNonPositive(t *testing.T) {
	for _, test := range []struct {
		// name describes the configuration.
		name string

		// burst and perMinute are the arguments.
		burst, perMinute int
	}{
		{name: "zero burst", burst: 0, perMinute: 1},
		{name: "zero rate", burst: 1, perMinute: 0},
		{name: "negative burst", burst: -1, perMinute: 1},
		{name: "negative rate", burst: 1, perMinute: -1},
		{name: "both zero", burst: 0, perMinute: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("New did not panic")
				}
			}()
			ratelimit.New(test.burst, test.perMinute)
		})
	}
}

// TestTimeGoingBackwards records whether a clock step backwards grants or loses tokens, and that it never panics.
func TestTimeGoingBackwards(t *testing.T) {
	limiter := ratelimit.New(2, 60)
	drain(limiter, "k", start, 2)
	ok, retry := limiter.Allow("k", start.Add(-time.Hour))
	// PROBE: the doc says nothing about a clock that steps back; a grant here would let a skewed clock mint tokens.
	t.Logf("an hour earlier: allowed %v, retry %d", ok, retry)
	if ok {
		t.Fatal("a clock step backwards granted a token")
	}
	if retry < 1 {
		t.Fatalf("retry %d after a step back, want at least 1", retry)
	}
	if ok, _ := limiter.Allow("k", start.Add(time.Second)); !ok {
		t.Fatal("after the step back, the token due one second after exhaustion was lost")
	}
}

// TestZeroTime accepts the zero time.Time as now without panicking.
func TestZeroTime(t *testing.T) {
	limiter := ratelimit.New(1, 60)
	if ok, _ := limiter.Allow("k", time.Time{}); !ok {
		t.Fatal("first request at the zero time refused")
	}
	if ok, _ := limiter.Allow("k", time.Time{}); ok {
		t.Fatal("second request at the zero time allowed")
	}
	if ok, _ := limiter.Allow("k", time.Time{}.Add(time.Second)); !ok {
		t.Fatal("refused one second after the zero time")
	}
}

// TestConcurrentBurstIsExact hammers one key from 100 goroutines and expects exactly burst successes.
func TestConcurrentBurstIsExact(t *testing.T) {
	limiter := ratelimit.New(50, 60)
	var allowed atomic.Int32
	var group sync.WaitGroup
	for range 100 {
		group.Add(1)
		go func() {
			defer group.Done()
			if ok, _ := limiter.Allow("shared", start); ok {
				allowed.Add(1)
			}
		}()
	}
	group.Wait()
	if allowed.Load() != 50 {
		t.Fatalf("%d allowed under contention, want 50", allowed.Load())
	}
}

// TestConcurrentKeysDoNotRace creates and drains many keys at once under the race detector.
func TestConcurrentKeysDoNotRace(t *testing.T) {
	limiter := ratelimit.New(2, 60)
	var group sync.WaitGroup
	for worker := range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range 200 {
				key := "2001:db8:" + string(rune('a'+worker)) + "::" + string(rune('a'+index%26))
				limiter.Allow(key, start.Add(time.Duration(index)*time.Millisecond))
			}
		}()
	}
	group.Wait()
}

// TestLargeValuesDoNotOverflow checks huge rates and bursts, and long idle gaps, behave.
func TestLargeValuesDoNotOverflow(t *testing.T) {
	huge := ratelimit.New(1<<31, 1_000_000_000)
	if ok, _ := huge.Allow("k", start); !ok {
		t.Fatal("huge limiter refused its first request")
	}
	if ok, _ := huge.Allow("k", start.Add(1000*time.Hour)); !ok {
		t.Fatal("huge limiter refused after a long gap")
	}
	fast := ratelimit.New(1, 1_000_000_000)
	fast.Allow("k", start)
	if ok, retry := fast.Allow("k", start); ok {
		t.Log("a billion a minute: second request at the same instant allowed")
	} else if retry < 1 {
		t.Fatalf("a billion a minute: retry %d, want at least 1", retry)
	}
	if ok, _ := fast.Allow("k", start.Add(time.Millisecond)); !ok {
		t.Fatal("a billion a minute: no token after a millisecond")
	}
	slow := ratelimit.New(1, 1)
	slow.Allow("k", start)
	if ok, _ := slow.Allow("k", start.Add(200*365*24*time.Hour)); !ok {
		t.Fatal("two centuries idle: refused")
	}
	bigBurst := ratelimit.New(1<<31, 1)
	if allowed := drain(bigBurst, "k", start, 1000); allowed != 1000 {
		t.Fatalf("burst of 2^31: only %d of 1000 allowed", allowed)
	}
}

// TestRetrySecondsNeverZeroWhenRefused sweeps sub-second offsets and checks a refusal never says wait 0.
func TestRetrySecondsNeverZeroWhenRefused(t *testing.T) {
	for _, perMinute := range []int{1, 7, 30, 60, 61, 119, 120, 1000} {
		limiter := ratelimit.New(1, perMinute)
		limiter.Allow("k", start)
		for millis := 0; millis < 60000; millis += 37 {
			now := start.Add(time.Duration(millis) * time.Millisecond)
			ok, retry := limiter.Allow("k", now)
			if ok {
				break
			}
			if retry < 1 {
				t.Fatalf("%d per minute at %d ms: refused with retry %d", perMinute, millis, retry)
			}
		}
	}
}
