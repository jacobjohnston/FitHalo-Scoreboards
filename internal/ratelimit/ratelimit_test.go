package ratelimit

import (
	"testing"
	"time"
)

// testTime is the fixed time the tests run at.
var testTime = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

// TestIPv6AddressesShareTheirSlash64 proves rotating addresses within one /64 does not escape the limit.
func TestIPv6AddressesShareTheirSlash64(t *testing.T) {
	limiter := New(2, 1)
	for _, key := range []string{"2001:db8:1:2::1", "2001:db8:1:2:ffff::9"} {
		if allowed, _ := limiter.Allow(key, testTime); !allowed {
			t.Fatalf("Allow(%q) refused within the burst", key)
		}
	}
	if allowed, _ := limiter.Allow("2001:db8:1:2:abcd::1", testTime); allowed {
		t.Fatal("Allow allowed a third address in the same /64")
	}
	if allowed, _ := limiter.Allow("2001:db8:1:3::1", testTime); !allowed {
		t.Fatal("Allow refused an address in another /64")
	}
}

// TestBucketKey checks IPv6 keys become their /64 while IPv4 and other keys stay as they are.
func TestBucketKey(t *testing.T) {
	for key, want := range map[string]string{
		"2001:db8:1:2:3:4:5:6":   "2001:db8:1:2::/64",
		"[2001:db8:1:2::1]:443":  "2001:db8:1:2::/64",
		"fe80::1%eth0":           "fe80::/64",
		"::ffff:203.0.113.9":     "203.0.113.9",
		"203.0.113.9":            "203.0.113.9",
		"203.0.113.10":           "203.0.113.10",
		"not an address":         "not an address",
		"":                       "",
		"member-key-hash-abcdef": "member-key-hash-abcdef",
	} {
		if got := bucketKey(key); got != want {
			t.Errorf("bucketKey(%q) = %q, want %q", key, got, want)
		}
	}
}

// TestIPv4AddressesKeepTheirOwnBuckets proves neighbouring IPv4 addresses are not grouped.
func TestIPv4AddressesKeepTheirOwnBuckets(t *testing.T) {
	limiter := New(1, 1)
	if allowed, _ := limiter.Allow("203.0.113.9", testTime); !allowed {
		t.Fatal("Allow refused the first request")
	}
	if allowed, _ := limiter.Allow("203.0.113.10", testTime); !allowed {
		t.Fatal("Allow refused a neighbouring IPv4 address")
	}
	if allowed, retryAfter := limiter.Allow("203.0.113.9", testTime); allowed || retryAfter != 60 {
		t.Fatalf("Allow = %v, %d, want false, 60", allowed, retryAfter)
	}
}

// TestNewRefusesNonPositiveLimits proves a zero or negative burst or rate panics instead of dividing by zero.
func TestNewRefusesNonPositiveLimits(t *testing.T) {
	for _, limits := range [][2]int{{0, 1}, {1, 0}, {-1, 1}, {1, -1}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("New(%d, %d) did not panic", limits[0], limits[1])
				}
			}()
			New(limits[0], limits[1])
		}()
	}
}
