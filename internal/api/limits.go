package api

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// maxTrackedClients mirrors app/limits.py MAX_TRACKED_CLIENTS.
const maxTrackedClients = 10_000

// bodyLimitRejections counts every request rejected for exceeding the body
// cap, on the Content-Length gate and on the read-side MaxBytesReader cap.
var bodyLimitRejections atomic.Int64

// rateLimitRejections counts every request rejected by the sliding-window
// rate limiter.
var rateLimitRejections atomic.Int64

// BodyLimitRejections returns the process-wide count of rejected oversized
// request bodies (Go-only resource-limit metric).
func BodyLimitRejections() int64 { return bodyLimitRejections.Load() }

// RateLimitRejections returns the process-wide count of rate-limited requests.
func RateLimitRejections() int64 { return rateLimitRejections.Load() }

// rateLimiter is the Go port of RateLimitMiddleware: a token-free sliding
// window limiter per client IP plus a Content-Length body gate. Like the
// Python middleware it trusts the Content-Length header only (chunked bodies
// without one are not measured).
type rateLimiter struct {
	limit   int
	window  time.Duration
	maxBody int64

	mu   sync.Mutex
	hits map[string][]time.Time
}

// newRateLimiter applies the same coercion as the Python constructor:
// limit >= 1, window >= 0.1s, max_body >= 1.
func newRateLimiter(limit int, windowSeconds float64, maxBody int) *rateLimiter {
	if limit < 1 {
		limit = 1
	}
	if windowSeconds < 0.1 {
		windowSeconds = 0.1
	}
	if maxBody < 1 {
		maxBody = 1
	}
	return &rateLimiter{
		limit:   limit,
		window:  time.Duration(windowSeconds * float64(time.Second)),
		maxBody: int64(maxBody),
		hits:    map[string][]time.Time{},
	}
}

// clientKey mirrors RateLimitMiddleware._client_key.
func clientKey(r *http.Request) string {
	if r.RemoteAddr == "" {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (l *rateLimiter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentLength := r.Header.Get("Content-Length")
		if contentLength != "" {
			parsed, err := strconv.Atoi(contentLength)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid content-length")
				return
			}
			if int64(parsed) > l.maxBody {
				bodyLimitRejections.Add(1)
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
		}

		// The Content-Length header can lie or be absent (chunked), so the
		// read side is capped as well; without this a single request could
		// exhaust memory.
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, l.maxBody)
		}

		client := clientKey(r)
		now := time.Now()

		l.mu.Lock()
		bucket := l.hits[client]
		cutoff := now.Add(-l.window)
		start := 0
		for start < len(bucket) && !bucket[start].After(cutoff) {
			start++
		}
		if start > 0 {
			bucket = append([]time.Time{}, bucket[start:]...)
		}
		if len(bucket) >= l.limit {
			l.hits[client] = bucket
			l.mu.Unlock()
			rateLimitRejections.Add(1)
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		bucket = append(bucket, now)
		l.hits[client] = bucket
		if len(l.hits) > maxTrackedClients {
			rebuilt := make(map[string][]time.Time, len(l.hits))
			for key, value := range l.hits {
				if len(value) > 0 {
					rebuilt[key] = value
				}
			}
			l.hits = rebuilt
		}
		l.mu.Unlock()

		next.ServeHTTP(w, r)
	})
}
