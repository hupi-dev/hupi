package demo

import (
	"sync"
	"time"
)

// IPRateLimiter is a simple in-memory fixed-window limiter on how many
// demo sessions a single client IP may create per window. It is
// deliberately not the real cost guard — it resets on process restart
// and does nothing against an attacker willing to rotate source IPs —
// that job belongs to Store.CreateSession's database-backed daily cap,
// which survives both. This just stops the casual, single-IP case
// cheaply, without a database round-trip on every attempt.
type IPRateLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	seen   map[string][]time.Time
}

func NewIPRateLimiter(max int, window time.Duration) *IPRateLimiter {
	return &IPRateLimiter{max: max, window: window, seen: make(map[string][]time.Time)}
}

// Allow reports whether ip may create another session right now, and
// records this attempt either way — a denied attempt still counts
// toward the window, so retrying immediately can't reset it.
func (l *IPRateLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-l.window)

	// Filters in place: kept always trails the read index, so this never
	// overwrites an entry before it's been read.
	kept := l.seen[ip][:0]
	for _, t := range l.seen[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	l.seen[ip] = kept

	return len(kept) <= l.max
}
