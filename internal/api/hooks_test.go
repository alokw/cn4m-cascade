package api

import (
	"fmt"
	"testing"
	"time"
)

// The limiter must not accumulate an entry per address it ever sees.
//
// Pruning only on an address's *next* request never reclaims one that never
// returns — and this is the endpoint designed to be reachable anonymously, so
// walking source addresses is cheap for anyone on the network. An earlier
// version of this code claimed the leak had been avoided; it had not.
func TestHookLimiterDoesNotGrowWithoutBound(t *testing.T) {
	l := newHookLimiter()

	// One failure each from far more addresses than the sweep threshold.
	for i := range maxTrackedAddrs * 2 {
		l.record(fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}

	l.mu.Lock()
	tracked := len(l.failures)
	l.mu.Unlock()

	if tracked > maxTrackedAddrs {
		t.Fatalf("tracking %d addresses with a %d ceiling; nothing is reclaiming them",
			tracked, maxTrackedAddrs)
	}
}

// Expired entries are dropped even for an address that never comes back.
func TestHookLimiterForgetsExpiredAddresses(t *testing.T) {
	l := newHookLimiter()

	l.mu.Lock()
	l.failures["192.0.2.1"] = []time.Time{time.Now().Add(-2 * hookAttemptWindow)}
	l.mu.Unlock()

	// A record from *another* address triggers the sweep path once the map is
	// large enough; force it directly here, which is what the sweep does.
	l.mu.Lock()
	l.sweep()
	_, still := l.failures["192.0.2.1"]
	l.mu.Unlock()

	if still {
		t.Fatal("an address whose only failure expired is still tracked")
	}
}

// Blocking is by failures alone, and only after the threshold.
func TestHookLimiterBlocksOnlyAfterRepeatedFailures(t *testing.T) {
	l := newHookLimiter()
	const addr = "198.51.100.7"

	for i := range maxHookFailures - 1 {
		l.record(addr)
		if l.blocked(addr) {
			t.Fatalf("blocked after %d failures; the threshold is %d", i+1, maxHookFailures)
		}
	}
	l.record(addr)
	if !l.blocked(addr) {
		t.Fatalf("not blocked after %d failures", maxHookFailures)
	}

	// An address that never failed is never blocked, however busy the map is.
	if l.blocked("203.0.113.9") {
		t.Fatal("an address with no failures is blocked")
	}
}
