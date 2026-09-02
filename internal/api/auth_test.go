package api

import (
	"testing"
	"time"
)

// A failed login from an address that never comes back must not leave a
// permanent map entry. clear() only fires on a *successful* login, so before
// this was fixed an unauthenticated caller rotating source addresses grew
// loginLimiter.attempts without bound.
func TestLoginLimiterForgetsExpiredAddresses(t *testing.T) {
	l := newLoginLimiter()

	l.mu.Lock()
	l.attempts["10.0.0.1"] = []time.Time{time.Now().Add(-2 * loginAttemptWindow)}
	l.mu.Unlock()

	if l.blocked("10.0.0.1") {
		t.Fatal("an address whose only failure has expired must not be blocked")
	}
	l.mu.Lock()
	_, still := l.attempts["10.0.0.1"]
	l.mu.Unlock()
	if still {
		t.Fatal("blocked() kept an empty entry for an expired address; the map grows without bound")
	}
}

// The limiter still has to do its job: recent failures accumulate, block at
// the threshold, and a successful login clears them.
func TestLoginLimiterBlocksAndClears(t *testing.T) {
	l := newLoginLimiter()
	const addr = "10.0.0.2"

	for i := range maxLoginAttempts - 1 {
		l.record(addr)
		if l.blocked(addr) {
			t.Fatalf("blocked after %d failures, want at least %d", i+1, maxLoginAttempts)
		}
	}
	l.record(addr)
	if !l.blocked(addr) {
		t.Fatalf("not blocked after %d failures", maxLoginAttempts)
	}

	l.clear(addr)
	if l.blocked(addr) {
		t.Fatal("a successful login must clear the address")
	}
	l.mu.Lock()
	_, still := l.attempts[addr]
	l.mu.Unlock()
	if still {
		t.Fatal("clear() left an entry behind")
	}
}

// Expired failures must not count toward the threshold, or an address that
// guessed slowly a day ago would still be locked out.
func TestLoginLimiterIgnoresFailuresOutsideTheWindow(t *testing.T) {
	l := newLoginLimiter()
	const addr = "10.0.0.3"

	old := time.Now().Add(-loginAttemptWindow - time.Minute)
	stale := make([]time.Time, maxLoginAttempts)
	for i := range stale {
		stale[i] = old
	}
	l.mu.Lock()
	l.attempts[addr] = stale
	l.mu.Unlock()

	if l.blocked(addr) {
		t.Fatal("stale failures blocked an address that has not guessed recently")
	}
}
