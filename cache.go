package md4agents

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

// entry is what we hand back to a request. ETag is the hex SHA-256 of the
// markdown body. Headers carries the safe-allowlist subset of the upstream
// response that produced this entry so cache hits and 304s replay the same
// downstream-cache hints (Cache-Control, Vary, etc.) the first request saw.
type entry struct {
	Markdown []byte
	ETag     string
	Headers  http.Header
	Created  time.Time
}

func (e *entry) expired(ttl time.Duration) bool {
	return ttl > 0 && time.Since(e.Created) > ttl
}

// markdownCache wraps an LRU with a byte budget. Byte accounting uses an
// atomic counter so the eviction callback (which runs under the LRU's
// own mutex) can update it without risking a lock-order inversion with
// the caller of Put/Get.
type markdownCache struct {
	lru      *lru.Cache[string, *entry]
	ttl      time.Duration
	maxBytes int64
	maxEntry int64
	bytes    atomic.Int64

	mu       sync.Mutex
	inflight map[string]*inflightCall
}

type inflightCall struct {
	done chan struct{}
	e    *entry
	err  error
}

func newCache(entries int, ttl time.Duration, maxBytes, maxEntry int64) (*markdownCache, error) {
	if entries <= 0 {
		entries = 4096
	}
	if maxBytes <= 0 {
		maxBytes = 256 << 20
	}
	if maxEntry <= 0 {
		maxEntry = 1 << 20
	}
	c := &markdownCache{
		ttl: ttl, maxBytes: maxBytes, maxEntry: maxEntry,
		inflight: make(map[string]*inflightCall),
	}
	l, err := lru.NewWithEvict[string, *entry](entries, func(_ string, e *entry) {
		c.bytes.Add(-int64(len(e.Markdown)))
	})
	if err != nil {
		return nil, err
	}
	c.lru = l
	return c, nil
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

// put rejects oversized entries and pre-evicts oldest until the byte
// budget admits the new one. Returns false if the entry was too large.
func (c *markdownCache) put(key string, e *entry) bool {
	size := int64(len(e.Markdown))
	if size > c.maxEntry {
		return false
	}
	for c.bytes.Load()+size > c.maxBytes {
		if _, _, ok := c.lru.RemoveOldest(); !ok {
			// Empty cache and still over budget — give up rather than spin.
			break
		}
	}
	c.bytes.Add(size)
	c.lru.Add(key, e)
	return true
}

func (c *markdownCache) purge(key string) { c.lru.Remove(key) }

// do collapses concurrent calls for the same key.
func (c *markdownCache) do(key string, fn func() (*entry, error)) (*entry, error) {
	if e, ok := c.get(key); ok {
		return e, nil
	}
	c.mu.Lock()
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

func (c *markdownCache) stats() (entries int, bytes int64) {
	return c.lru.Len(), c.bytes.Load()
}
