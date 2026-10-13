package main

import (
	"sync"
	"time"
)

// maxLogBuckets bounds the bucket map. Buckets are keyed by format string,
// which at every current call site is a constant, so this is only a guard
// against a future caller passing a dynamic format.
const maxLogBuckets = 256

type logBucket struct {
	last       time.Time
	suppressed int
}

// rateLimitedLogger wraps a logger and limits Warnf per bucket (bucket =
// format string), so different kinds of warning never mask each other.
// Within a bucket, at most one message per interval (60s / perMin) is
// emitted; suppressed repeats are counted and appended to the next emitted
// message of the same bucket. Driven purely by calls (no goroutine): a
// tail of suppressed messages is reported only when the next one arrives.
// Infof is passed through unchanged. perMin <= 0 disables limiting.
type rateLimitedLogger struct {
	base     logger
	interval time.Duration
	now      func() time.Time

	mu      sync.Mutex
	buckets map[string]*logBucket
}

func newRateLimitedLogger(base logger, perMin int) *rateLimitedLogger {
	l := &rateLimitedLogger{
		base:    base,
		now:     time.Now,
		buckets: make(map[string]*logBucket),
	}
	if perMin > 0 {
		l.interval = time.Minute / time.Duration(perMin)
	}
	return l
}

func (l *rateLimitedLogger) Warnf(format string, args ...any) {
	if l.interval <= 0 {
		l.base.Warnf(format, args...)
		return
	}
	now := l.now()

	l.mu.Lock()
	b, ok := l.buckets[format]
	if ok && now.Sub(b.last) < l.interval {
		b.suppressed++
		l.mu.Unlock()
		return
	}
	if !ok {
		if len(l.buckets) >= maxLogBuckets {
			l.buckets = make(map[string]*logBucket)
		}
		b = &logBucket{}
		l.buckets[format] = b
	}
	n := b.suppressed
	b.suppressed = 0
	b.last = now
	l.mu.Unlock()

	if n > 0 {
		l.base.Warnf(format+" [+%d similar message(s) suppressed]", append(args[:len(args):len(args)], n)...)
		return
	}
	l.base.Warnf(format, args...)
}

func (l *rateLimitedLogger) Infof(format string, args ...any) {
	l.base.Infof(format, args...)
}
