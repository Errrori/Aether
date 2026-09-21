// Package ratelimit provides in-process token-bucket rate limiting for publish
// requests. Publisher (API key) and channel buckets are independent: both must
// allow a request for it to pass.
package ratelimit

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	// ScopePublisher identifies the per-API-key bucket.
	ScopePublisher = "publisher"
	// ScopeChannel identifies the per-channel bucket.
	ScopeChannel = "channel"
)

const (
	defaultIdleTTL       = 10 * time.Minute
	defaultSweepInterval = time.Minute
)

// RateConfig describes a single token bucket dimension.
type RateConfig struct {
	Rate  float64 // tokens added per second
	Burst int     // bucket capacity
}

// Config controls a Limiter. Zero IdleTTL and SweepInterval fall back to their
// defaults (10m and 1m).
type Config struct {
	Publisher     RateConfig
	Channel       RateConfig
	IdleTTL       time.Duration
	SweepInterval time.Duration
	OnRejected    func(scope string)
}

// Limiter checks publish requests against two independent token buckets.
// It is safe for concurrent use.
type Limiter struct {
	publisher     *keyedLimiter
	channel       *keyedLimiter
	sweepInterval time.Duration
	logger        *slog.Logger
	onRejected    func(scope string)

	startOnce sync.Once
	wg        sync.WaitGroup
}

// New creates a Limiter. The idle-bucket sweeper is not running until Start is
// called; Allow works without it.
func New(cfg Config, logger *slog.Logger) *Limiter {
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = defaultIdleTTL
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = defaultSweepInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Limiter{
		publisher:     newKeyedLimiter(cfg.Publisher, cfg.IdleTTL),
		channel:       newKeyedLimiter(cfg.Channel, cfg.IdleTTL),
		sweepInterval: cfg.SweepInterval,
		logger:        logger,
		onRejected:    cfg.OnRejected,
	}
}

// Start launches the idle-bucket sweeper. It returns immediately; the sweeper
// goroutine exits when ctx is cancelled (Wait blocks until then).
func (l *Limiter) Start(ctx context.Context) {
	l.startOnce.Do(func() {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			ticker := time.NewTicker(l.sweepInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					l.sweep(time.Now())
				}
			}
		}()
	})
}

// Wait blocks until the sweeper started by Start has exited. It returns
// immediately when Start was never called.
func (l *Limiter) Wait() {
	l.wg.Wait()
}

// Allow checks both buckets without blocking. A request is allowed only when a
// token is immediately available in both. On denial it returns the limiting
// scope and a suggested retry delay; the bucket that did allow the request has
// its token restored, so a request denied by one dimension does not consume
// quota from the other.
func (l *Limiter) Allow(keyID, channel string) (scope string, retryAfter time.Duration, ok bool) {
	return l.allowAt(keyID, channel, time.Now())
}

func (l *Limiter) allowAt(keyID, channel string, now time.Time) (scope string, retryAfter time.Duration, ok bool) {
	pubLim, pubRes := l.publisher.reserve(keyID, now)
	if delay := pubRes.DelayFrom(now); !pubRes.OK() || delay > 0 {
		pubRes.CancelAt(now)
		retry := denialDelay(pubLim, delay, now)
		l.reject(ScopePublisher, keyID, channel, retry)
		return ScopePublisher, retry, false
	}

	chLim, chRes := l.channel.reserve(channel, now)
	if delay := chRes.DelayFrom(now); !chRes.OK() || delay > 0 {
		chRes.CancelAt(now)
		pubRes.CancelAt(now)
		retry := denialDelay(chLim, delay, now)
		l.reject(ScopeChannel, keyID, channel, retry)
		return ScopeChannel, retry, false
	}

	return "", 0, true
}

func (l *Limiter) sweep(now time.Time) {
	l.publisher.sweep(now)
	l.channel.sweep(now)
}

func (l *Limiter) reject(scope, keyID, channel string, retry time.Duration) {
	if l.onRejected != nil {
		l.onRejected(scope)
	}
	l.logger.Debug("publish rate limited",
		"scope", scope, "key_id", keyID, "channel", channel, "retry_after", retry)
}

// denialDelay turns a failed reservation into a suggested wait time. A
// reservation that is OK but in the future carries the exact delay; one that is
// not OK carries InfDuration and falls back to the token shortfall.
func denialDelay(lim *rate.Limiter, delay time.Duration, now time.Time) time.Duration {
	if delay > 0 && delay < rate.InfDuration {
		return delay
	}
	missing := 1 - lim.Tokens()
	if missing <= 0 {
		return 0
	}
	limit := lim.Limit()
	if limit <= 0 || limit == rate.Inf {
		return 0
	}
	return time.Duration(float64(time.Second) * missing / float64(limit))
}

// keyedLimiter lazily creates one token bucket per key and drops buckets that
// have been idle for longer than idleTTL.
type keyedLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	limit   rate.Limit
	burst   int
	idleTTL time.Duration
}

type bucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newKeyedLimiter(cfg RateConfig, idleTTL time.Duration) *keyedLimiter {
	return &keyedLimiter{
		buckets: make(map[string]*bucket),
		limit:   rate.Limit(cfg.Rate),
		burst:   cfg.Burst,
		idleTTL: idleTTL,
	}
}

func (k *keyedLimiter) reserve(key string, now time.Time) (*rate.Limiter, *rate.Reservation) {
	k.mu.Lock()
	b, ok := k.buckets[key]
	if !ok {
		b = &bucket{limiter: rate.NewLimiter(k.limit, k.burst)}
		k.buckets[key] = b
	}
	b.lastSeen = now
	lim := b.limiter
	k.mu.Unlock()

	return lim, lim.ReserveN(now, 1)
}

func (k *keyedLimiter) sweep(now time.Time) int {
	k.mu.Lock()
	defer k.mu.Unlock()

	removed := 0
	for key, b := range k.buckets {
		if now.Sub(b.lastSeen) > k.idleTTL {
			delete(k.buckets, key)
			removed++
		}
	}
	return removed
}
