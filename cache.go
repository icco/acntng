package main

import (
	"sync"
	"time"
)

// cache memoizes a report for cacheTTL, keyed by whatever shapes it, so
// repeated hits do not each fan out into upstream calls. Zero value ready.
type cache[T any] struct {
	mu      sync.Mutex
	entries map[string]cacheEntry[T]
}

type cacheEntry[T any] struct {
	value  T
	stored time.Time
}

// get returns a cached value if present and still fresh.
func (c *cache[T]) get(key string, now time.Time) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var zero T

	e, ok := c.entries[key]
	if !ok {
		return zero, false
	}
	if now.Sub(e.stored) >= cacheTTL {
		delete(c.entries, key)
		return zero, false
	}

	return e.value, true
}

// set stores a value under key.
func (c *cache[T]) set(key string, v T, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil {
		c.entries = map[string]cacheEntry[T]{}
	}
	c.entries[key] = cacheEntry[T]{value: v, stored: now}
}
