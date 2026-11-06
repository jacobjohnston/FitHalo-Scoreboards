// Package ratelimit limits requests per address with a token bucket held in memory.
package ratelimit

import (
	"fmt"
	"math"
	"net/netip"
	"sync"
	"time"
)

// sweepInterval is the least time between sweeps of idle buckets.
const sweepInterval = time.Minute

// ipv6PrefixBits is the prefix an IPv6 address is keyed by, since one subscriber usually holds a /64.
const ipv6PrefixBits = 64

// Limiter allows each key a burst of requests, refilled at a steady rate.
type Limiter struct {
	// mutex guards every field below it.
	mutex sync.Mutex

	// burst is the most tokens a bucket holds.
	burst float64

	// perSecond is the tokens a bucket regains each second.
	perSecond float64

	// buckets holds each key's bucket.
	buckets map[string]*bucket

	// lastSwept is when idle buckets were last forgotten.
	lastSwept time.Time
}

// bucket is one key's tokens.
type bucket struct {
	// tokens is the tokens left, as of updated.
	tokens float64

	// updated is when tokens was last refilled.
	updated time.Time
}

// New returns a limiter allowing burst requests at once and perMinute requests a minute after that.
// It panics unless both are positive, since callers pass constants.
func New(burst int, perMinute int) *Limiter {
	if burst < 1 || perMinute < 1 {
		panic(fmt.Sprintf("ratelimit: New(%d, %d) needs a positive burst and rate", burst, perMinute))
	}
	perSecond := float64(perMinute) / time.Minute.Seconds()
	return &Limiter{burst: float64(burst), perSecond: perSecond, buckets: map[string]*bucket{}}
}

// Allow takes a token for key, and when none is left reports how many seconds until one is. An IPv6
// address shares a bucket with the rest of its /64.
func (l *Limiter) Allow(key string, now time.Time) (bool, int) {
	key = bucketKey(key)
	l.mutex.Lock()
	defer l.mutex.Unlock()

	l.sweep(now)
	current, known := l.buckets[key]
	if !known {
		current = &bucket{tokens: l.burst, updated: now}
		l.buckets[key] = current
	}
	current.tokens = math.Min(l.burst, current.tokens+now.Sub(current.updated).Seconds()*l.perSecond)
	current.updated = now
	if current.tokens >= 1 {
		current.tokens--
		return true, 0
	}
	return false, int(math.Ceil((1 - current.tokens) / l.perSecond))
}

// sweep forgets buckets that have refilled completely, so idle addresses cost nothing.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastSwept) < sweepInterval {
		return
	}
	refill := time.Duration(l.burst / l.perSecond * float64(time.Second))
	for key, current := range l.buckets {
		if now.Sub(current.updated) > refill {
			delete(l.buckets, key)
		}
	}
	l.lastSwept = now
}

// bucketKey returns the /64 prefix of an IPv6 address, the plain form of an IPv4-mapped one, and any
// other key unchanged.
func bucketKey(key string) string {
	address, err := netip.ParseAddr(key)
	if err != nil {
		addressPort, err := netip.ParseAddrPort(key)
		if err != nil {
			return key
		}
		address = addressPort.Addr()
	}
	address = address.Unmap()
	if address.Is4() {
		return address.String()
	}
	prefix, err := address.WithZone("").Prefix(ipv6PrefixBits)
	if err != nil {
		return key
	}
	return prefix.String()
}
