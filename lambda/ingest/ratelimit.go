package main

import (
	"sync"
	"time"
)

// rateLimiter is an in-memory token bucket keyed by DSN public key. It is the
// SDK-facing abuse-prevention control (API Gateway usage plans would only throttle
// keyed clients, and stock Sentry SDKs never send an API key). In-memory state
// is per-Lambda-instance; acceptable for MVP, and 429 + Retry-After semantics
// are honored exactly per the response contract.
type rateLimiter struct {
	mu      sync.Mutex
	rate    float64 // tokens refilled per second
	burst   float64 // maximum tokens (bucket capacity)
	buckets map[string]*tokenBucket
	now     func() time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rate, burst float64) *rateLimiter {
	return &rateLimiter{
		rate:    rate,
		burst:   burst,
		buckets: make(map[string]*tokenBucket),
		now:     time.Now,
	}
}

// Allow reports whether key may proceed at time now; when false, retryAfter
// is the duration the client should wait (for the Retry-After header).
func (rl *rateLimiter) Allow(key string, now time.Time) (allowed bool, retryAfter time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[key]
	if !ok {
		b = &tokenBucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	}

	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * rl.rate
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}

	// Retry-After = seconds until at least one token is available.
	waitSec := (1 - b.tokens) / rl.rate
	if waitSec < 1 {
		waitSec = 1
	}
	return false, time.Duration(waitSec * float64(time.Second))
}
