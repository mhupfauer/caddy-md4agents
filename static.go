package md4agents

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
)

// serveStatic implements the static-first lookup. It returns (served=true)
// when the request was fully handled (success or 304); served=false means
// the caller should fall back to the dynamic path.
//
// Resolution order, given a logical URL path P:
//  1. ROOT/P stripped of suffix + ".md" (author file) → serve verbatim
//  2. ROOT/P stripped of suffix + ".html"             → convert / cache / serve
//  3. ROOT/P stripped of suffix + "/index.html"       → convert / cache / serve
//  4. miss → served=false
//
// Caches are keyed by absolute source path + mtime + size, so a source edit
// implicitly invalidates without an explicit purge.
func (m *MarkdownForAgents) serveStatic(w http.ResponseWriter, r *http.Request, logicalPath string) (bool, error) {
	base := logicalPath
	// logicalPath may already be ".html" (suffix-rewrite path) or carry the
	// markdown suffix (Accept-header / query path on a .md URL). Strip
	// either so the candidate lookup below is symmetric.
	base = strings.TrimSuffix(base, ".html")
	if m.URLSuffix != "" {
		base = strings.TrimSuffix(base, m.URLSuffix)
	}
	base = strings.TrimSuffix(base, "/")
	if base == "" {
		base = "/"
	}

	// 1. Author-written .md takes precedence.
	if author := m.resolveAuthor(base); author != "" {
		return true, m.serveFile(w, r, author, "author")
	}

	// 2/3. Find HTML source.
	htmlPath, err := m.resolveHTML(base)
	if err != nil {
		return false, nil
	}

	return true, m.serveGenerated(w, r, htmlPath)
}

func (m *MarkdownForAgents) resolveAuthor(base string) string {
	candidate := filepath.Join(m.Root, filepath.FromSlash(base)+".md")
	if !isUnderRoot(m.Root, candidate) {
		return ""
	}
	if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
		return candidate
	}
	// /docs/  → /docs/index.md
	candidate = filepath.Join(m.Root, filepath.FromSlash(base), "index.md")
	if !isUnderRoot(m.Root, candidate) {
		return ""
	}
	if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
		return candidate
	}
	return ""
}

func (m *MarkdownForAgents) resolveHTML(base string) (string, error) {
	candidates := []string{
		filepath.Join(m.Root, filepath.FromSlash(base)+".html"),
		filepath.Join(m.Root, filepath.FromSlash(base), "index.html"),
	}
	for _, c := range candidates {
		if !isUnderRoot(m.Root, c) {
			continue
		}
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", errors.New("no html source")
}

func (m *MarkdownForAgents) serveFile(w http.ResponseWriter, r *http.Request, abs, kind string) error {
	data, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	e := newEntry(data)
	m.log.Debug("static markdown served",
		zap.String("file", abs), zap.String("kind", kind))
	return m.writeMarkdown(w, r, e)
}

// serveGenerated is the lazy-convert + write-through core. The cache key
// embeds source mtime + size so the in-memory cache is auto-invalidated on
// source change. The disk sidecar is keyed by source mtime alone — if the
// source is newer than the sidecar, we re-convert and overwrite.
func (m *MarkdownForAgents) serveGenerated(w http.ResponseWriter, r *http.Request, htmlPath string) error {
	st, err := os.Stat(htmlPath)
	if err != nil {
		return err
	}
	key := fmt.Sprintf("%s|%d|%d", htmlPath, st.ModTime().UnixNano(), st.Size())

	e, err := m.cache.do(key, func() (*entry, error) {
		return m.loadOrGenerate(r.Context(), htmlPath, st)
	})
	if err != nil {
		return err
	}
	return m.writeMarkdown(w, r, e)
}

// loadOrGenerate is called once per unique (path, mtime, size) tuple, so
// the disk-read / conversion / disk-write happens at most once per source
// version even under load.
func (m *MarkdownForAgents) loadOrGenerate(ctx context.Context, htmlPath string, st os.FileInfo) (*entry, error) {
	sidecar := m.sidecarPath(htmlPath)
	if sidecar != "" {
		if sst, err := os.Stat(sidecar); err == nil && !sst.ModTime().Before(st.ModTime()) {
			if data, err := os.ReadFile(sidecar); err == nil {
				return newEntry(data), nil
			}
		}
	}

	htmlBytes, err := os.ReadFile(htmlPath)
	if err != nil {
		return nil, err
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

// sidecarPath maps a source HTML path to its on-disk markdown cache file.
// Returns "" if no cache dir is configured.
//
//	ROOT/docs/about.html  →  CACHE/docs/about.html.md
//
// We append ".md" to the full filename (rather than swapping the extension)
// so the generated file can never collide with an author-written `.md`.
func (m *MarkdownForAgents) sidecarPath(htmlPath string) string {
	if m.CacheDir == "" {
		return ""
	}
	rel, err := filepath.Rel(m.Root, htmlPath)
	if err != nil {
		return ""
	}
	return filepath.Join(m.CacheDir, rel+".md")
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
	// Match source mtime so the freshness check (sidecar >= source) holds.
	_ = os.Chtimes(tmpName, mtime, mtime)
	return os.Rename(tmpName, dst)
}

// isUnderRoot rejects path-traversal attempts. Caddy normalizes URLs but a
// custom rewrite or matcher could still construct something exotic, so we
// belt-and-braces it.
func isUnderRoot(root, candidate string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absCandidate)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}

