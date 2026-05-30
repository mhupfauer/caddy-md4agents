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

// markdownCache wraps an LRU with a byte budget. Both an entry-count cap and a
// total-byte cap apply; an Add evicts oldest entries until both budgets fit.
//
// Bounding by count alone is unsafe: 4096 × 4 MiB ≈ 16 GiB in the worst case.
// MaxEntryBytes additionally rejects oversized single entries so one giant
// page can't dominate the cache or DOS the byte accounting.
type markdownCache struct {
	mu       sync.Mutex
	lru      *lru.Cache[string, *entry]
	ttl      time.Duration
	maxBytes int64
	maxEntry int64
	bytes    int64

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
		maxBytes = 256 << 20 // 256 MiB
	}
	if maxEntry <= 0 {
		maxEntry = 1 << 20 // 1 MiB per entry
	}
	c := &markdownCache{
		ttl: ttl, maxBytes: maxBytes, maxEntry: maxEntry,
		inflight: make(map[string]*inflightCall),
	}
	l, err := lru.NewWithEvict[string, *entry](entries, func(_ string, e *entry) {
		c.mu.Lock()
		c.bytes -= int64(len(e.Markdown))
		if c.bytes < 0 {
			c.bytes = 0
		}
		c.mu.Unlock()
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

// put rejects oversized entries and evicts oldest until the byte budget fits.
// Returns false if the entry was too large to admit.
func (c *markdownCache) put(key string, e *entry) bool {
	size := int64(len(e.Markdown))
	if size > c.maxEntry {
		return false
	}
	c.mu.Lock()
	for c.bytes+size > c.maxBytes && c.lru.Len() > 0 {
		// RemoveOldest triggers our evict callback, which decrements bytes.
		c.mu.Unlock()
		c.lru.RemoveOldest()
		c.mu.Lock()
	}
	c.bytes += size
	c.mu.Unlock()
	c.lru.Add(key, e)
	return true
}

func (c *markdownCache) purge(key string) { c.lru.Remove(key) }

// do collapses concurrent calls for the same key. The fn is only executed
// once; other callers wait and receive the same result.
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

// stats returns current totals; used by tests.
func (c *markdownCache) stats() (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len(), c.bytes
}
