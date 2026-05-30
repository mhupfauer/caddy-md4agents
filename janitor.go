package md4agents

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

// janitor periodically removes sidecar files whose source HTML no longer
// exists under Root. Disabled unless JanitorInterval > 0. Orphan cleanup
// is not required for correctness (the per-request mtime check ensures
// freshness even with orphans present); it's purely a disk-hygiene knob
// for long-lived deployments where pages get deleted.
type janitor struct {
	m        *MarkdownForAgents
	interval time.Duration
	cancel   context.CancelFunc
	done     chan struct{}
}

func newJanitor(m *MarkdownForAgents, interval time.Duration) *janitor {
	return &janitor{m: m, interval: interval, done: make(chan struct{})}
}

func (j *janitor) start(ctx caddy.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	j.cancel = cancel
	go j.run(runCtx)
}

func (j *janitor) stop() error {
	if j.cancel != nil {
		j.cancel()
	}
	<-j.done
	return nil
}

func (j *janitor) run(ctx context.Context) {
	defer close(j.done)
	t := time.NewTicker(j.interval)
	defer t.Stop()
	// First sweep immediately so config reloads pick up changes promptly.
	j.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.sweep(ctx)
		}
	}
}

func (j *janitor) sweep(ctx context.Context) {
	var removed int
	err := filepath.WalkDir(j.m.cacheResolved, func(path string, d os.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Sidecars end in ".html.md" — leave anything else alone.
		if !strings.HasSuffix(path, ".html.md") {
			return nil
		}
		rel, err := filepath.Rel(j.m.cacheResolved, path)
		if err != nil {
			return nil
		}
		sourceRel := strings.TrimSuffix(rel, ".md") // back to "...html"
		source := filepath.Join(j.m.rootResolved, sourceRel)
		// Only treat a confirmed not-exist as "orphan". A transient
		// EIO/ESTALE during an NFS flap would otherwise cause us to
		// delete the entire sidecar cache.
		_, statErr := os.Stat(source)
		if statErr == nil {
			return nil
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			j.m.log.Debug("janitor: skip uncertain source",
				zap.String("source", source), zap.Error(statErr))
			return nil
		}
		if err := os.Remove(path); err == nil {
			removed++
		}
		return nil
	})
	if err != nil {
		j.m.log.Debug("janitor walk error", zap.Error(err))
	}
	if removed > 0 {
		j.m.log.Info("janitor removed orphan sidecars", zap.Int("count", removed))
	}
}
