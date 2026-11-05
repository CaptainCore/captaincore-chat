package api

import (
	"sync"
	"time"
)

// limiter is a per-key token bucket. Capacity n refills fully over per.
type limiter struct {
	mu      sync.Mutex
	n       float64
	per     time.Duration
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(n int, per time.Duration) *limiter {
	return &limiter{n: float64(n), per: per, buckets: make(map[string]*bucket), now: time.Now}
}

// Allow takes one token for key, reporting whether one was available.
func (l *limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) > 10000 { // bound memory; full buckets are disposable
			for k, v := range l.buckets {
				if now.Sub(v.last) > l.per {
					delete(l.buckets, k)
				}
			}
		}
		b = &bucket{tokens: l.n, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.n / l.per.Seconds()
	if b.tokens > l.n {
		b.tokens = l.n
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
