package main

import (
	"sync"
	"time"
)

// cache memoizes built reports for cacheTTL, keyed by the options that shape
// them. The Lunch Money API is rate limited, so repeated hits on this endpoint
// should not each fan out into three upstream requests.
//
// The zero value is ready to use.
type cache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	report *Report
	stored time.Time
}

// get returns a cached report if one is present and still fresh as of now.
func (c *cache) get(key string, now time.Time) (*Report, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if now.Sub(e.stored) >= cacheTTL {
		delete(c.entries, key)
		return nil, false
	}

	return e.report, true
}

// set stores a report under key.
func (c *cache) set(key string, rep *Report, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil {
		c.entries = map[string]cacheEntry{}
	}
	c.entries[key] = cacheEntry{report: rep, stored: now}
}
