package md4agents

import (
	"bytes"
	"net/http"
	"strings"
)

// captureWriter buffers the upstream response so we can convert HTML → MD
// before sending bytes back to the real client. If the body exceeds the
// configured cap, we stop buffering and `flush()` writes through using only
// the bytes we already have plus any subsequent writes that come in directly
// — at that point we've forfeited conversion for this request.
type captureWriter struct {
	http.ResponseWriter
	status      int
	body        *bytes.Buffer
	max         int64
	overflow    bool
	wroteHeader bool
}

func newCaptureWriter(w http.ResponseWriter, max int64) *captureWriter {
	return &captureWriter{ResponseWriter: w, body: new(bytes.Buffer), max: max, status: 200}
}

func (c *captureWriter) WriteHeader(code int) {
	c.status = code
	c.wroteHeader = true
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if c.overflow {
		// Already gave up — pass through.
		return c.ResponseWriter.Write(p)
	}
	if int64(c.body.Len()+len(p)) > c.max {
		c.overflow = true
		// Replay what we buffered, then write the new chunk.
		c.writeCapturedHeaders()
		if _, err := c.ResponseWriter.Write(c.body.Bytes()); err != nil {
			return 0, err
		}
		c.body.Reset()
		return c.ResponseWriter.Write(p)
	}
	return c.body.Write(p)
}

func (c *captureWriter) writeCapturedHeaders() {
	if !c.wroteHeader {
		return
	}
	c.ResponseWriter.WriteHeader(c.status)
	c.wroteHeader = false
}

// flush writes the buffered upstream response through unchanged. Use this
// when we decide not to convert (non-HTML response, conversion error, etc.).
func (c *captureWriter) flush() error {
	if c.overflow {
		// Headers and body already sent in Write.
		return nil
	}
	c.writeCapturedHeaders()
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
	ct := c.Header().Get("Content-Type")
	return strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml")
}
