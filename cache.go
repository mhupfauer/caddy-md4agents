package md4agents

import (
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

// entry is what we hand back to a request. ETag is the hex SHA-256 of the
// markdown body — clients use it for If-None-Match revalidation, and the
// pre-generator uses it to skip re-converting unchanged sources.
type entry struct {
	Markdown []byte
	ETag     string
	Created  time.Time
}

func (e *entry) expired(ttl time.Duration) bool {
	return ttl > 0 && time.Since(e.Created) > ttl
}

// markdownCache is a thin TTL+LRU wrapper. Size 0 means "unbounded" — we still
// allocate a large LRU because golang-lru requires a positive size.
type markdownCache struct {
	lru *lru.Cache[string, *entry]
	ttl time.Duration

	// inflight collapses concurrent identical conversions into one. Without
	// this, a thundering herd on the same uncached URL would convert N times.
	mu       sync.Mutex
	inflight map[string]*inflightCall
}

type inflightCall struct {
	done chan struct{}
	e    *entry
	err  error
}

func newCache(size int, ttl time.Duration) (*markdownCache, error) {
	if size <= 0 {
		size = 4096
	}
	c, err := lru.New[string, *entry](size)
	if err != nil {
		return nil, err
	}
	return &markdownCache{lru: c, ttl: ttl, inflight: make(map[string]*inflightCall)}, nil
}

func (c *markdownCache) get(key string) (*entry, bool) {
	e, ok := c.lru.Get(key)
	if !ok {
		return nil, false
	}
	if e.expired(c.ttl) {
		c.lru.Remove(key)
		return nil, false
	}
	return e, true
}

func (c *markdownCache) put(key string, e *entry) { c.lru.Add(key, e) }

func (c *markdownCache) purge(key string) { c.lru.Remove(key) }

// do collapses concurrent calls for the same key. The fn is only executed
// once; other callers wait and receive the same result. The pattern is the
// same as singleflight, inlined to avoid another dependency.
func (c *markdownCache) do(key string, fn func() (*entry, error)) (*entry, error) {
	if e, ok := c.get(key); ok {
		return e, nil
	}
	c.mu.Lock()
	// Re-check under lock: another goroutine may have completed the
	// conversion in the gap between the cheap get above and acquiring mu.
	if e, ok := c.get(key); ok {
		c.mu.Unlock()
		return e, nil
	}
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-call.done
		return call.e, call.err
	}
	call := &inflightCall{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	call.e, call.err = fn()
	if call.err == nil && call.e != nil {
		c.put(key, call.e)
	}
	close(call.done)

	c.mu.Lock()
	delete(c.inflight, key)
	c.mu.Unlock()

	return call.e, call.err
}
