package httpapi

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestFixedWindowLimiterConcurrentBoundAndReset(t *testing.T) {
	clock := &fakeHTTPClock{now: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)}
	limiter := newFixedWindowLimiter(clock, 10, time.Minute, 8)
	key := limitKey(loginKeyDomain, "person@example.com")
	start := make(chan struct{})
	results := make(chan bool, 40)
	var group sync.WaitGroup
	for range 40 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			allowed, _ := limiter.allow(key)
			results <- allowed
		}()
	}
	close(start)
	group.Wait()
	close(results)
	allowed := 0
	for result := range results {
		if result {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("concurrent allowed=%d want=10", allowed)
	}
	clock.advance(time.Minute)
	if ok, retry := limiter.allow(key); !ok || retry != 0 {
		t.Fatalf("window did not reset: ok=%t retry=%s", ok, retry)
	}
}

func TestFixedWindowLimiterBoundsKeyMapWithoutEvictionBypass(t *testing.T) {
	clock := &fakeHTTPClock{now: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)}
	limiter := newFixedWindowLimiter(clock, 2, time.Minute, 2)
	for index := 0; index < 2; index++ {
		if allowed, _ := limiter.allow(limitKey(loginKeyDomain, fmt.Sprintf("key-%d", index))); !allowed {
			t.Fatal("initial bounded key rejected")
		}
	}
	if allowed, retry := limiter.allow(limitKey(loginKeyDomain, "overflow")); allowed || retry <= 0 {
		t.Fatalf("overflow key allowed=%t retry=%s", allowed, retry)
	}
	if len(limiter.entries) != 2 {
		t.Fatalf("limiter map size=%d", len(limiter.entries))
	}
	clock.advance(time.Minute)
	if allowed, _ := limiter.allow(limitKey(loginKeyDomain, "overflow")); !allowed {
		t.Fatal("expired entries were not cleaned")
	}
	if len(limiter.entries) > 2 {
		t.Fatalf("limiter map exceeded cap: %d", len(limiter.entries))
	}
}

func TestGlobalLoginLimiterCapsDistinctKeys(t *testing.T) {
	clock := &fakeHTTPClock{now: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)}
	limits := newAbuseLimiters(clock)
	for index := 0; index < loginGlobalLimit; index++ {
		if allowed, _ := limits.allowLogin(fmt.Sprintf("person-%d@example.com", index)); !allowed {
			t.Fatalf("global login rejected request %d early", index)
		}
	}
	if allowed, retry := limits.allowLogin("overflow@example.com"); allowed || retry <= 0 {
		t.Fatalf("global login overflow allowed=%t retry=%s", allowed, retry)
	}
}
