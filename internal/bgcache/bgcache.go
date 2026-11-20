// Package bgcache is a read-now, refresh-later cache for slow lookups
// (subprocesses like lsof/git/sh) that are reachable from a hot loop.
//
// Get never blocks: it returns the last known value (empty if none yet) and,
// if that value is older than the TTL and no refresh is in flight, starts one
// in a goroutine. The loop pays nanoseconds; the subprocess runs elsewhere and
// its result shows up on a later Get. Any exec.Command reachable from the
// session loop or the client receive loop goes through here.
package bgcache

import (
	"sync"
	"time"
)

type entry struct {
	val     string
	at      time.Time
	running bool
}

type Cache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]*entry
}

func New(ttl time.Duration) *Cache {
	return &Cache{ttl: ttl, m: map[string]*entry{}}
}

// SetTTL changes how long a value is served before a refresh is started.
func (c *Cache) SetTTL(ttl time.Duration) {
	c.mu.Lock()
	c.ttl = ttl
	c.mu.Unlock()
}

// Get returns the cached value for key ("" until the first fetch lands) and
// kicks off fetch in the background when the value is stale. One fetch per
// key at a time.
func (c *Cache) Get(key string, fetch func() string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[key]
	if e == nil {
		e = &entry{}
		c.m[key] = e
	}
	if !e.running && time.Since(e.at) >= c.ttl {
		e.running = true
		go func() {
			v := fetch()
			c.mu.Lock()
			e.val, e.at, e.running = v, time.Now(), false
			c.mu.Unlock()
		}()
	}
	return e.val
}
