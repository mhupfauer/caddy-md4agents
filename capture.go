package md4agents

import (
	"bytes"
	"net/http"
	"strings"
)

// captureWriter buffers the upstream response so we can convert HTML → MD
// before sending bytes back to the real client.
//
// CRITICAL: it must NOT share the underlying ResponseWriter's header map.
// If it did (which is the default for embedded ResponseWriter via
// Header()), upstream setting Set-Cookie / Server / X-Powered-By would
// land directly on the wire, defeating the snapshotSafeHeaders allowlist.
// We keep a private http.Header instead.
//
// If the body exceeds the configured cap, we stop buffering, flush our
// state to the real writer, and pass through subsequent writes.
type captureWriter struct {
	http.ResponseWriter
	hdr         http.Header
	status      int
	body        *bytes.Buffer
	max         int64
	overflow    bool
	wroteHeader bool
}

func newCaptureWriter(w http.ResponseWriter, max int64) *captureWriter {
	return &captureWriter{
		ResponseWriter: w,
		hdr:            make(http.Header),
		body:           new(bytes.Buffer),
		max:            max,
		status:         200,
	}
}

// Header returns our isolated header map. Upstream writes never reach the
// real ResponseWriter unless we explicitly copy them.
func (c *captureWriter) Header() http.Header { return c.hdr }

func (c *captureWriter) WriteHeader(code int) {
	c.status = code
	c.wroteHeader = true
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if c.overflow {
		return c.ResponseWriter.Write(p)
	}
	if int64(c.body.Len()+len(p)) > c.max {
		c.overflow = true
		c.flushHeadersToReal()
		if _, err := c.ResponseWriter.Write(c.body.Bytes()); err != nil {
			return 0, err
		}
		c.body.Reset()
		return c.ResponseWriter.Write(p)
	}
	return c.body.Write(p)
}

// flushHeadersToReal copies our isolated headers to the real writer and
// commits the status. Used when we've decided NOT to rewrite the body
// (overflow, non-HTML response, conversion failure) and just want to
// pass the captured response through unchanged.
func (c *captureWriter) flushHeadersToReal() {
	real := c.ResponseWriter.Header()
	for k, vs := range c.hdr {
		real[k] = vs
	}
	if c.wroteHeader {
		c.ResponseWriter.WriteHeader(c.status)
		c.wroteHeader = false
	}
}

// flush writes the buffered upstream response through unchanged.
func (c *captureWriter) flush() error {
	if c.overflow {
		return nil
	}
	c.flushHeadersToReal()
	if c.body.Len() == 0 {
		return nil
	}
	_, err := c.ResponseWriter.Write(c.body.Bytes())
	return err
}

func (c *captureWriter) isConvertibleHTML() bool {
	if c.overflow {
		return false
	}
	if c.status < 200 || c.status >= 300 {
		return false
	}
	ct := c.hdr.Get("Content-Type")
	return strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml")
}
