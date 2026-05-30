package md4agents

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
)

// errPathEscape is returned by the path resolvers when a candidate path
// resolves outside Root via symlinks. We treat this as a security signal
// and short-circuit the request to 404 — without it, file_server would
// happily follow the same symlink and the dynamic capture path would
// convert and serve the result, defeating the static-path guard.
var errPathEscape = errors.New("path escapes root")

// serveStatic implements the static-first lookup. It returns (served=true)
// when the request was fully handled (success, 304, or refused-as-escape);
// served=false means the caller should fall back to the dynamic path.
//
// Resolution order, given a logical URL path P:
//  1. ROOT/P stripped of suffix + ".md" (author file) → serve verbatim
//  2. ROOT/P stripped of suffix + ".html"             → convert / cache / serve
//  3. ROOT/P stripped of suffix + "/index.html"       → convert / cache / serve
//  4. miss → served=false
//
// Every disk read goes through resolveAndCheck so a malicious symlink under
// Root can't escape (read), and the sidecar path uses resolveParentAndCheck
// so write-through can't escape CacheDir either.
//
// Caches are keyed by absolute source path + mtime + size, so a source edit
// implicitly invalidates without an explicit purge.
func (m *MarkdownForAgents) serveStatic(w http.ResponseWriter, r *http.Request, logicalPath string) (bool, error) {
	base := logicalPath
	base = strings.TrimSuffix(base, ".html")
	if m.URLSuffix != "" {
		base = strings.TrimSuffix(base, m.URLSuffix)
	}
	base = strings.TrimSuffix(base, "/")
	if base == "" {
		base = "/"
	}

	author, err := m.resolveAuthor(base)
	if err == errPathEscape {
		return true, m.refuseEscape(w, r, base)
	}
	if author != "" {
		return true, m.serveFile(w, r, author, "author")
	}

	htmlPath, err := m.resolveHTML(base)
	if err == errPathEscape {
		return true, m.refuseEscape(w, r, base)
	}
	if err != nil {
		return false, nil
	}

	return true, m.serveGenerated(w, r, htmlPath)
}

// refuseEscape writes a 404 (deliberately indistinguishable from a true
// miss, no need to advertise that we caught an escape attempt) and logs
// the attempt for the operator.
func (m *MarkdownForAgents) refuseEscape(w http.ResponseWriter, r *http.Request, base string) error {
	m.log.Warn("refused symlink escape",
		zap.String("path", r.URL.Path),
		zap.String("resolved_base", base))
	http.NotFound(w, r)
	return nil
}

// resolveAuthor returns the canonical path of an author-written `.md`
// file under Root, or "" if none exists. Returns errPathEscape if a
// candidate exists but resolves outside Root via symlink — the caller
// must NOT fall through to a downstream handler in that case.
func (m *MarkdownForAgents) resolveAuthor(base string) (string, error) {
	for _, candidate := range []string{
		filepath.Join(m.Root, filepath.FromSlash(base)+".md"),
		filepath.Join(m.Root, filepath.FromSlash(base), "index.md"),
	} {
		real, err := resolveAndCheck(m.rootResolved, candidate)
		if err != nil {
			if isEscapeErr(err) {
				return "", errPathEscape
			}
			continue
		}
		if st, err := os.Stat(real); err == nil && !st.IsDir() {
			return real, nil
		}
	}
	return "", nil
}

func (m *MarkdownForAgents) resolveHTML(base string) (string, error) {
	for _, candidate := range []string{
		filepath.Join(m.Root, filepath.FromSlash(base)+".html"),
		filepath.Join(m.Root, filepath.FromSlash(base), "index.html"),
	} {
		real, err := resolveAndCheck(m.rootResolved, candidate)
		if err != nil {
			if isEscapeErr(err) {
				return "", errPathEscape
			}
			continue
		}
		if st, err := os.Stat(real); err == nil && !st.IsDir() {
			return real, nil
		}
	}
	return "", errors.New("no html source")
}

// isEscapeErr distinguishes "this candidate escapes root" from
// "candidate doesn't exist". EvalSymlinks returns NotExist for missing
// paths; anything else from our resolveAndCheck is a security signal.
func isEscapeErr(err error) bool {
	if err == nil {
		return false
	}
	if os.IsNotExist(err) {
		return false
	}
	return strings.Contains(err.Error(), "escapes root")
}

func (m *MarkdownForAgents) serveFile(w http.ResponseWriter, r *http.Request, abs, kind string) error {
	data, err := readBoundedFile(abs, m.MaxBodyBytes)
	if err != nil {
		return err
	}
	e := newEntry(data)
	m.log.Debug("static markdown served",
		zap.String("file", abs), zap.String("kind", kind))
	return m.writeMarkdown(w, r, e, nil)
}

// serveGenerated is the lazy-convert + write-through core. The cache key
// embeds source mtime + size so the in-memory cache is auto-invalidated on
// source change. The disk sidecar is keyed by source mtime alone — if the
// source is newer than the sidecar, we re-convert and overwrite.
//
// We stat the source first (cheap, hits the FS cache after the first
// request) to derive the cache key, *then* open the file only if we'll
// actually do work. Opening before the cache lookup would leak the fd on
// every cache hit because `cache.do` skips the callback (and its defer
// close) on hit.
func (m *MarkdownForAgents) serveGenerated(w http.ResponseWriter, r *http.Request, htmlPath string) error {
	st, err := os.Stat(htmlPath)
	if err != nil {
		return err
	}
	if st.Size() > m.MaxBodyBytes {
		return fmt.Errorf("source %s exceeds max_body_bytes (%d > %d)",
			htmlPath, st.Size(), m.MaxBodyBytes)
	}
	key := fmt.Sprintf("%s|%d|%d", htmlPath, st.ModTime().UnixNano(), st.Size())

	e, err := m.cache.do(key, func() (*entry, error) {
		f, err := os.Open(htmlPath)
		if err != nil {
			return nil, err
		}
		// Re-stat from fd to close the TOCTOU window. If the source raced
		// out from under us between Stat and Open, refuse rather than
		// cache stale content under a now-wrong key.
		st2, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		if st2.ModTime() != st.ModTime() || st2.Size() != st.Size() {
			f.Close()
			return nil, fmt.Errorf("source %s changed during open", htmlPath)
		}
		return m.loadOrGenerateFromFile(r.Context(), htmlPath, f, st2)
	})
	if err != nil {
		return err
	}
	return m.writeMarkdown(w, r, e, nil)
}

// loadOrGenerateFromFile reuses the open file handle to avoid the
// stat/open TOCTOU. The handle is consumed regardless (read or closed).
func (m *MarkdownForAgents) loadOrGenerateFromFile(ctx context.Context, htmlPath string, f *os.File, st os.FileInfo) (*entry, error) {
	defer f.Close()

	sidecar := m.sidecarPath(htmlPath)
	if sidecar != "" {
		if sst, err := os.Stat(sidecar); err == nil && !sst.ModTime().Before(st.ModTime()) {
			if data, err := readBoundedFile(sidecar, m.MaxBodyBytes*2); err == nil {
				return newEntry(data), nil
			}
		}
	}

	htmlBytes, err := io.ReadAll(io.LimitReader(f, m.MaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(htmlBytes)) > m.MaxBodyBytes {
		return nil, fmt.Errorf("source %s exceeds max_body_bytes during read", htmlPath)
	}

	md, err := m.convertWithTimeout(ctx, htmlBytes)
	if err != nil {
		return nil, fmt.Errorf("convert %s: %w", htmlPath, err)
	}
	mdBytes := []byte(md)

	if sidecar != "" {
		if err := writeAtomic(sidecar, mdBytes, st.ModTime()); err != nil {
			m.log.Warn("sidecar write failed",
				zap.String("sidecar", sidecar), zap.Error(err))
		}
	}
	return newEntry(mdBytes), nil
}

// loadOrGenerate is the legacy stat-path entry used by the pregenerator,
// which doesn't already hold an open handle.
func (m *MarkdownForAgents) loadOrGenerate(ctx context.Context, htmlPath string, st os.FileInfo) (*entry, error) {
	f, err := os.Open(htmlPath)
	if err != nil {
		return nil, err
	}
	// Re-stat from fd in case anything raced.
	st2, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st2.Size() > m.MaxBodyBytes {
		f.Close()
		return nil, fmt.Errorf("source %s exceeds max_body_bytes (%d > %d)",
			htmlPath, st2.Size(), m.MaxBodyBytes)
	}
	return m.loadOrGenerateFromFile(ctx, htmlPath, f, st2)
}

// sidecarPath maps a source HTML path to its on-disk markdown cache file.
// Returns "" if no cache dir is configured, or if the would-be sidecar
// path escapes CacheDir via symlinks.
//
//	ROOT/docs/about.html  →  CACHE/docs/about.html.md
//
// We append ".md" to the full filename (rather than swapping the extension)
// so the generated file can never collide with an author-written `.md`.
func (m *MarkdownForAgents) sidecarPath(htmlPath string) string {
	if m.CacheDir == "" {
		return ""
	}
	rel, err := filepath.Rel(m.rootResolved, htmlPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	candidate := filepath.Join(m.cacheResolved, rel+".md")
	final, err := resolveParentAndCheck(m.cacheResolved, candidate)
	if err != nil {
		return ""
	}
	return final
}

// readBoundedFile reads at most cap+1 bytes via LimitReader so a runaway
// file can't OOM the process. Returns an error if the file exceeds cap.
func readBoundedFile(path string, cap int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if cap <= 0 {
		cap = 4 << 20
	}
	data, err := io.ReadAll(io.LimitReader(f, cap+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > cap {
		return nil, fmt.Errorf("file %s exceeds limit %d", path, cap)
	}
	return data, nil
}

func writeAtomic(dst string, data []byte, mtime time.Time) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".md4agents-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	_ = os.Chtimes(tmpName, mtime, mtime)
	return os.Rename(tmpName, dst)
}
