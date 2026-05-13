package api

import (
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	rateLimitWindow     = time.Minute
	maxRateLimitBuckets = 50000
)

type rateLimiter struct {
	mu               sync.Mutex
	buckets          map[string]rateBucket
	trustedProxyCIDR []netip.Prefix
	nextPrune        time.Time
}

type rateBucket struct {
	windowStart time.Time
	count       int
}

func newRateLimiter(trustedProxyCIDRs string) *rateLimiter {
	return &rateLimiter{
		buckets:          make(map[string]rateBucket),
		trustedProxyCIDR: parseTrustedProxyCIDRs(trustedProxyCIDRs),
	}
}

func (l *rateLimiter) allow(clientID, path string, now time.Time) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	key := clientRateKey(clientID, path)
	limit := limitForPath(path)

	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, exists := l.buckets[key]
	if !exists && len(l.buckets) >= maxRateLimitBuckets {
		if l.nextPrune.IsZero() || !now.Before(l.nextPrune) {
			l.pruneLocked(now)
			l.nextPrune = now.Add(rateLimitWindow)
		}
		if len(l.buckets) >= maxRateLimitBuckets {
			l.evictOldestLocked()
		}
	}
	if bucket.windowStart.IsZero() || now.Sub(bucket.windowStart) >= rateLimitWindow {
		l.buckets[key] = rateBucket{windowStart: now, count: 1}
		if l.nextPrune.IsZero() || !now.Before(l.nextPrune) {
			l.pruneLocked(now)
			l.nextPrune = now.Add(rateLimitWindow)
		}
		return true, 0
	}
	if bucket.count >= limit {
		return false, rateLimitWindow - now.Sub(bucket.windowStart)
	}
	bucket.count++
	l.buckets[key] = bucket
	return true, 0
}

func (l *rateLimiter) pruneLocked(now time.Time) {
	for key, bucket := range l.buckets {
		if now.Sub(bucket.windowStart) >= 2*rateLimitWindow {
			delete(l.buckets, key)
		}
	}
}

func (l *rateLimiter) evictOldestLocked() {
	var oldestKey string
	var oldestStart time.Time
	for key, bucket := range l.buckets {
		if oldestKey == "" || bucket.windowStart.Before(oldestStart) {
			oldestKey = key
			oldestStart = bucket.windowStart
		}
	}
	if oldestKey != "" {
		delete(l.buckets, oldestKey)
	}
}

func clientRateKey(clientID, path string) string {
	return normalizeClientID(clientID) + " " + path
}

func (l *rateLimiter) clientIDFromRequest(r *http.Request) string {
	remote := normalizeClientID(r.RemoteAddr)
	if !l.trusts(remote) {
		return remote
	}
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		for _, part := range strings.Split(forwarded, ",") {
			if client := normalizeClientID(part); client != "unknown" {
				return client
			}
		}
	}
	if realIP := normalizeClientID(r.Header.Get("X-Real-IP")); realIP != "unknown" {
		return realIP
	}
	return remote
}

func (l *rateLimiter) trusts(clientID string) bool {
	addr, err := netip.ParseAddr(clientID)
	if err != nil {
		return false
	}
	for _, prefix := range l.trustedProxyCIDR {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func normalizeClientID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = strings.TrimSpace(host)
	}
	if addr, err := netip.ParseAddr(value); err == nil {
		return addr.String()
	}
	return "unknown"
}

func parseTrustedProxyCIDRs(raw string) []netip.Prefix {
	var out []netip.Prefix
	for _, token := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	}) {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(token); err == nil {
			if prefix.Bits() == 0 {
				log.Printf("WARNING: ignoring unsafe HOLO_TRUSTED_PROXY_CIDRS entry %q; configure specific proxy CIDRs instead", token)
				continue
			}
			out = append(out, prefix.Masked())
			continue
		}
		if addr, err := netip.ParseAddr(token); err == nil {
			bits := 128
			if addr.Is4() {
				bits = 32
			}
			out = append(out, netip.PrefixFrom(addr, bits))
		}
	}
	return out
}

func retryAfterSeconds(d time.Duration) string {
	seconds := int(d.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return strconv.Itoa(seconds)
}

func limitForPath(path string) int {
	switch path {
	case "/v1/support/bundle":
		return 3
	case "/v1/storage/disks/discovery":
		return 30
	default:
		return 300
	}
}
