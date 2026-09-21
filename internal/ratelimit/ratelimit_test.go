package ratelimit

import (
	"context"
	"testing"
	"time"
)

func newTestLimiter(pub, ch RateConfig) *Limiter {
	return New(Config{
		Publisher:     pub,
		Channel:       ch,
		IdleTTL:       time.Hour,
		SweepInterval: time.Hour,
	}, nil)
}

func TestAllow_PublicPath(t *testing.T) {
	l := newTestLimiter(
		RateConfig{Rate: 1000, Burst: 10},
		RateConfig{Rate: 1000, Burst: 10},
	)

	if _, _, ok := l.Allow("key-a", "ch-1"); !ok {
		t.Error("first request should be allowed")
	}
}

func TestAllow_PublisherIsolation(t *testing.T) {
	l := newTestLimiter(
		RateConfig{Rate: 1, Burst: 1},
		RateConfig{Rate: 1, Burst: 1000},
	)
	now := time.Now()

	if _, _, ok := l.allowAt("key-a", "ch-1", now); !ok {
		t.Fatal("key-a first request should be allowed")
	}

	scope, retry, ok := l.allowAt("key-a", "ch-2", now)
	if ok {
		t.Fatal("key-a second request should hit the publisher limit")
	}
	if scope != ScopePublisher {
		t.Errorf("scope = %q, want %q", scope, ScopePublisher)
	}
	if retry <= 0 {
		t.Errorf("retryAfter = %v, want positive", retry)
	}

	if _, _, ok := l.allowAt("key-b", "ch-1", now); !ok {
		t.Error("key-b should not be affected by key-a's exhaustion")
	}
}

func TestAllow_ChannelIsolationAndAggregation(t *testing.T) {
	l := newTestLimiter(
		RateConfig{Rate: 1000, Burst: 100},
		RateConfig{Rate: 1, Burst: 1},
	)
	now := time.Now()

	if _, _, ok := l.allowAt("key-a", "ch-1", now); !ok {
		t.Fatal("first request to ch-1 should be allowed")
	}

	if scope, _, ok := l.allowAt("key-a", "ch-1", now); ok || scope != ScopeChannel {
		t.Fatalf("same key on exhausted channel: scope=%q ok=%v, want channel denial", scope, ok)
	}

	// Different key, same channel: the channel bucket is shared across keys.
	if scope, _, ok := l.allowAt("key-b", "ch-1", now); ok || scope != ScopeChannel {
		t.Fatalf("different key on exhausted channel: scope=%q ok=%v, want channel denial", scope, ok)
	}

	// Different channel: isolated from ch-1's exhaustion.
	if _, _, ok := l.allowAt("key-b", "ch-2", now); !ok {
		t.Error("ch-2 should not be affected by ch-1's exhaustion")
	}
}

func TestAllow_BurstThenRefill(t *testing.T) {
	l := newTestLimiter(
		RateConfig{Rate: 20, Burst: 2},
		RateConfig{Rate: 1000, Burst: 1000},
	)
	base := time.Now()

	for i := range 2 {
		if _, _, ok := l.allowAt("key-a", "ch-1", base); !ok {
			t.Fatalf("request %d should be within burst", i)
		}
	}

	scope, retry, ok := l.allowAt("key-a", "ch-1", base)
	if ok {
		t.Fatal("third request should be denied after burst is exhausted")
	}
	if scope != ScopePublisher {
		t.Errorf("scope = %q, want %q", scope, ScopePublisher)
	}
	if retry <= 0 || retry > 100*time.Millisecond {
		t.Errorf("retryAfter = %v, want (0, 100ms]", retry)
	}

	// Partial refill (0.5 token) is not enough.
	if _, _, ok := l.allowAt("key-a", "ch-1", base.Add(25*time.Millisecond)); ok {
		t.Error("request should be denied before a full token is available")
	}

	// One full token (1.2 at 20/s after 60ms) allows one request.
	if _, _, ok := l.allowAt("key-a", "ch-1", base.Add(60*time.Millisecond)); !ok {
		t.Error("request should be allowed after refill")
	}
}

func TestAllow_RetryAfter(t *testing.T) {
	l := newTestLimiter(
		RateConfig{Rate: 100, Burst: 1},
		RateConfig{Rate: 1000, Burst: 100},
	)
	base := time.Now()

	if _, _, ok := l.allowAt("key-a", "ch-1", base); !ok {
		t.Fatal("first request should be allowed")
	}

	_, retry, ok := l.allowAt("key-a", "ch-1", base)
	if ok {
		t.Fatal("second request should be denied")
	}
	// One token at 100/s is 10ms.
	if retry < 9*time.Millisecond || retry > 11*time.Millisecond {
		t.Errorf("retryAfter = %v, want ~10ms", retry)
	}

	if _, _, ok := l.allowAt("key-a", "ch-1", base.Add(11*time.Millisecond)); !ok {
		t.Error("request should be allowed once a token refills")
	}
}

func TestAllow_ChannelDenialDoesNotConsumePublisherToken(t *testing.T) {
	l := newTestLimiter(
		RateConfig{Rate: 10, Burst: 2},
		RateConfig{Rate: 0.001, Burst: 1},
	)
	now := time.Now()

	if _, _, ok := l.allowAt("key-a", "ch-1", now); !ok {
		t.Fatal("first request should be allowed")
	}

	if scope, _, ok := l.allowAt("key-a", "ch-1", now); ok || scope != ScopeChannel {
		t.Fatalf("second request: scope=%q ok=%v, want channel denial", scope, ok)
	}

	// The channel-denied request must not have consumed the publisher token;
	// if it did, the publisher bucket would now be the limiting scope.
	if scope, _, ok := l.allowAt("key-a", "ch-1", now); ok || scope != ScopeChannel {
		t.Fatalf("third request: scope=%q ok=%v, want channel denial (publisher token restored)", scope, ok)
	}
}

func TestOnRejected(t *testing.T) {
	counts := map[string]int{}
	l := New(Config{
		Publisher:     RateConfig{Rate: 1, Burst: 1},
		Channel:       RateConfig{Rate: 1, Burst: 1},
		IdleTTL:       time.Hour,
		SweepInterval: time.Hour,
		OnRejected:    func(scope string) { counts[scope]++ },
	}, nil)
	now := time.Now()

	if _, _, ok := l.allowAt("key-a", "ch-1", now); !ok {
		t.Fatal("first request should be allowed")
	}
	l.allowAt("key-a", "ch-1", now) // publisher denial

	if counts[ScopePublisher] != 1 {
		t.Errorf("publisher rejections = %d, want 1", counts[ScopePublisher])
	}
	if counts[ScopeChannel] != 0 {
		t.Errorf("channel rejections = %d, want 0", counts[ScopeChannel])
	}
}

func TestSweep_RemovesIdleBuckets(t *testing.T) {
	l := New(Config{
		Publisher:     RateConfig{Rate: 1, Burst: 1},
		Channel:       RateConfig{Rate: 1, Burst: 1},
		IdleTTL:       time.Minute,
		SweepInterval: time.Hour,
	}, nil)
	base := time.Now()

	if _, _, ok := l.allowAt("key-a", "ch-1", base); !ok {
		t.Fatal("request should be allowed")
	}
	if got := len(l.publisher.buckets); got != 1 {
		t.Fatalf("publisher buckets = %d, want 1", got)
	}
	if got := len(l.channel.buckets); got != 1 {
		t.Fatalf("channel buckets = %d, want 1", got)
	}

	l.sweep(base.Add(2 * time.Minute))
	if got := len(l.publisher.buckets); got != 0 {
		t.Errorf("publisher buckets after sweep = %d, want 0", got)
	}
	if got := len(l.channel.buckets); got != 0 {
		t.Errorf("channel buckets after sweep = %d, want 0", got)
	}
}

func TestSweep_KeepsActiveBuckets(t *testing.T) {
	l := New(Config{
		Publisher:     RateConfig{Rate: 1, Burst: 1},
		Channel:       RateConfig{Rate: 1, Burst: 1},
		IdleTTL:       time.Minute,
		SweepInterval: time.Hour,
	}, nil)
	base := time.Now()

	if _, _, ok := l.allowAt("key-a", "ch-1", base); !ok {
		t.Fatal("request should be allowed")
	}

	l.sweep(base)
	if got := len(l.publisher.buckets); got != 1 {
		t.Errorf("publisher buckets = %d, want 1", got)
	}
	if got := len(l.channel.buckets); got != 1 {
		t.Errorf("channel buckets = %d, want 1", got)
	}
}

func TestSweep_ResetBurst(t *testing.T) {
	l := New(Config{
		Publisher:     RateConfig{Rate: 0.001, Burst: 1},
		Channel:       RateConfig{Rate: 0.001, Burst: 1},
		IdleTTL:       time.Minute,
		SweepInterval: time.Hour,
	}, nil)
	base := time.Now()

	if _, _, ok := l.allowAt("key-a", "ch-1", base); !ok {
		t.Fatal("first request should be allowed")
	}
	if _, _, ok := l.allowAt("key-a", "ch-1", base); ok {
		t.Fatal("second request should be denied while the bucket is exhausted")
	}

	after := base.Add(2 * time.Minute)
	l.sweep(after)
	if _, _, ok := l.allowAt("key-a", "ch-1", after); !ok {
		t.Error("request after bucket eviction should use a fresh burst")
	}
}

func TestNew_Defaults(t *testing.T) {
	l := New(Config{
		Publisher: RateConfig{Rate: 1, Burst: 1},
		Channel:   RateConfig{Rate: 1, Burst: 1},
	}, nil)

	if l.sweepInterval != time.Minute {
		t.Errorf("sweepInterval = %v, want 1m", l.sweepInterval)
	}
	if l.publisher.idleTTL != 10*time.Minute {
		t.Errorf("publisher idleTTL = %v, want 10m", l.publisher.idleTTL)
	}
	if l.channel.idleTTL != 10*time.Minute {
		t.Errorf("channel idleTTL = %v, want 10m", l.channel.idleTTL)
	}
}

func TestStartWait_StopsOnCancel(t *testing.T) {
	l := newTestLimiter(
		RateConfig{Rate: 1, Burst: 1},
		RateConfig{Rate: 1, Burst: 1},
	)

	l.Wait() // no Start: must return immediately

	ctx, cancel := context.WithCancel(context.Background())
	l.Start(ctx)
	l.Start(ctx) // idempotent
	cancel()
	l.Wait()
}
