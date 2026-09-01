// Package health holds the cached reachability status of each target.
//
// The status is a cache on purpose: SPEC.md §8 asks the target list to show
// health, but actively probing on every list call would mount every
// configured share on every page load (PROGRESS.md D-6). Only an explicit
// test, or a job that resolves the target, refreshes it.
package health

import (
	"sync"
	"time"
)

// State is a target's last known reachability.
type State string

const (
	StateUnknown   State = "unknown"   // never checked since startup
	StateHealthy   State = "healthy"   // last check succeeded
	StateUnhealthy State = "unhealthy" // last check failed; Message says why
)

// Status is one cached observation.
type Status struct {
	State     State     `json:"state"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
	Message   string    `json:"message,omitempty"`
}

// Cache is a concurrency-safe map of target ID to Status.
type Cache struct {
	mu sync.RWMutex
	m  map[string]Status
}

// NewCache returns an empty cache.
func NewCache() *Cache { return &Cache{m: map[string]Status{}} }

// Get returns the cached status, or StateUnknown if there is none.
func (c *Cache) Get(id string) Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s, ok := c.m[id]; ok {
		return s
	}
	return Status{State: StateUnknown}
}

// SetHealthy records a successful check.
func (c *Cache) SetHealthy(id string) {
	c.set(id, Status{State: StateHealthy, CheckedAt: time.Now().UTC()})
}

// SetUnhealthy records a failed check together with the user-facing reason.
func (c *Cache) SetUnhealthy(id, message string) {
	c.set(id, Status{State: StateUnhealthy, CheckedAt: time.Now().UTC(), Message: message})
}

func (c *Cache) set(id string, s Status) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[id] = s
}

// Delete drops a target's status, for when the target itself is deleted.
func (c *Cache) Delete(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
}
