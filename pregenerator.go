package md4agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

// pregenerator walks Root at startup and primes the sidecar cache. The
// resolver's lazy path is fast enough for most sites; this exists for
// deployments that want the very first request to be hot. It exits after
// the walk — runtime invalidation is handled by stat-checks in the resolver.
type pregenerator struct {
	m      *MarkdownForAgents
	cancel context.CancelFunc
	done   chan struct{}
}

func newPregenerator(m *MarkdownForAgents) *pregenerator {
	return &pregenerator{m: m, done: make(chan struct{})}
}

func (p *pregenerator) start(ctx caddy.Context) error {
	// Inherit the Caddy context so a config reload cancels an in-flight walk.
	runCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	go p.run(runCtx)
	return nil
}

func (p *pregenerator) stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	<-p.done
	return nil
}

func (p *pregenerator) run(ctx context.Context) {
	defer close(p.done)

	const workers = 4
	jobs := make(chan string, workers*2)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				p.process(path)
			}
		}()
	}

	walkErr := filepath.WalkDir(p.m.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			return err
		}
		// Skip the cache dir so we don't recurse into our own output.
		if d.IsDir() {
			if p.m.CacheDir != "" && path == p.m.CacheDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".html") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case jobs <- path:
		}
		return nil
	})
	close(jobs)
	wg.Wait()

	if walkErr != nil && walkErr != context.Canceled {
		p.m.log.Warn("pregeneration walk error", zap.Error(walkErr))
	} else {
		p.m.log.Info("pregeneration complete", zap.String("root", p.m.Root))
	}
}

func (p *pregenerator) process(htmlPath string) {
	// Walk reports a file's path as-listed; resolve and re-check it under
	// rootResolved so a malicious symlink in the tree can't get its
	// target pregenerated and cached.
	real, err := resolveAndCheck(p.m.rootResolved, htmlPath)
	if err != nil {
		p.m.log.Debug("pregeneration: skipping unsafe path",
			zap.String("file", htmlPath), zap.Error(err))
		return
	}
	st, err := os.Stat(real)
	if err != nil || st.IsDir() {
		return
	}
	if _, err := p.m.loadOrGenerate(context.Background(), real, st); err != nil {
		p.m.log.Warn("pregeneration failed",
			zap.String("file", real), zap.Error(err))
	}
}
