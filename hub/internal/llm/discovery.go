package llm

import (
	"context"
	"log/slog"
	"time"
)

const defaultDiscoveryTTL = 60 * time.Second

type discoverer interface {
	discoverable() bool
	discoverModels(ctx context.Context) ([]ModelSpec, error)
}

// discoveryState is guarded by Registry.mu.
type discoveryState struct {
	models   []ModelSpec
	last     time.Time     // completion of the last attempt (ok or not)
	ttl      time.Duration // 0 = default
	inflight chan struct{}
	warned   bool
}

// SetDiscoveryTTL changes how long a discovery result is considered fresh.
func (r *Registry) SetDiscoveryTTL(d time.Duration) {
	r.mu.Lock()
	r.disc.ttl = d
	r.mu.Unlock()
}

func (r *Registry) discoverer() discoverer {
	if r.discoverOff {
		return nil
	}
	p, ok := r.providers["openai-compatible"]
	if !ok {
		return nil
	}
	if d, ok := p.(discoverer); ok && d.discoverable() {
		return d
	}
	return nil
}

// Discover refreshes the discovered model list from the openai-compatible
// provider now (L8). On failure the last good list is kept and the error is
// returned (logged at warn once, then debug, until it succeeds again).
func (r *Registry) Discover(ctx context.Context) error {
	r.mu.RLock()
	d := r.discoverer()
	r.mu.RUnlock()
	if d == nil {
		return nil
	}
	models, err := d.discoverModels(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disc.last = time.Now()
	if err != nil {
		if !r.disc.warned {
			slog.Warn("llm: model discovery failed; keeping last known list", "err", err)
			r.disc.warned = true
		} else {
			slog.Debug("llm: model discovery failed", "err", err)
		}
		return err
	}
	r.disc.warned = false
	r.disc.models = models
	return nil
}

// refreshAsync starts a background discovery unless one is running; the
// returned channel closes when it finishes (nil when discovery does not apply).
func (r *Registry) refreshAsync() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.discoverer() == nil {
		return nil
	}
	if r.disc.inflight != nil {
		return r.disc.inflight
	}
	done := make(chan struct{})
	r.disc.inflight = done
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = r.Discover(ctx)
		r.mu.Lock()
		r.disc.inflight = nil
		r.mu.Unlock()
		close(done)
	}()
	return done
}

// RefreshModels triggers the lazy TTL refresh and waits at most wait for it, so
// callers can serve the cached list when the provider is slow.
func (r *Registry) RefreshModels(ctx context.Context, wait time.Duration) {
	r.mu.RLock()
	ttl := r.disc.ttl
	if ttl <= 0 {
		ttl = defaultDiscoveryTTL
	}
	stale := r.disc.inflight == nil && time.Since(r.disc.last) >= ttl
	busy := r.disc.inflight != nil
	r.mu.RUnlock()
	if !stale && !busy {
		return
	}
	done := r.refreshAsync()
	if done == nil {
		return
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	case <-ctx.Done():
	}
}
