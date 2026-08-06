package main

import (
	"testing"
	"time"
)

func TestRateLimiterAllowsBurst(t *testing.T) {
	rl := newRateLimiter(10, 10)
	now := time.Now()
	for i := 0; i < 10; i++ {
		allowed, _ := rl.Allow("k", now)
		if !allowed {
			t.Fatalf("request %d should be allowed within burst", i)
		}
	}
	allowed, retry := rl.Allow("k", now)
	if allowed {
		t.Fatal("request beyond burst should be denied")
	}
	if retry <= 0 {
		t.Errorf("retry-after = %v", retry)
	}
}

func TestRateLimiterRefill(t *testing.T) {
	rl := newRateLimiter(1, 1)
	now := time.Now()
	if ok, _ := rl.Allow("k", now); !ok {
		t.Fatal("first request denied")
	}
	if ok, _ := rl.Allow("k", now); ok {
		t.Fatal("second request should be denied")
	}
	// After 1 second, a token is refilled.
	ok, _ := rl.Allow("k", now.Add(2*time.Second))
	if !ok {
		t.Fatal("request after refill should be allowed")
	}
}

func TestRateLimiterIsolatedPerKey(t *testing.T) {
	rl := newRateLimiter(1, 1)
	now := time.Now()
	if ok, _ := rl.Allow("a", now); !ok {
		t.Fatal("key a first request should be allowed")
	}
	if ok, _ := rl.Allow("a", now); ok {
		t.Fatal("key a second request should be denied")
	}
	if ok, _ := rl.Allow("b", now); !ok {
		t.Fatal("key b should have its own bucket")
	}
}
