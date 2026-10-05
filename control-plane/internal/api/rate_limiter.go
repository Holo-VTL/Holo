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
	rateLimitWindow      = time.Minute
	maxRateLimitBuckets  = 4096
	maxOverallRequests   = 300
	maxSupportRequests   = 3
	maxDiscoveryRequests = 30
)

type rateLimiter struct {
	mu               sync.Mutex
	buckets          map[string]rateBucket
	trustedProxyCIDR []netip.Prefix
	nextPrune        time.Time
}

type rateBucket struct {
	windowStart    time.Time
	totalCount     uint16
	supportCount   uint8
	discoveryCount uint8
}

func newRateLimiter(trustedProxyCIDRs string) *rateLimiter {
	return &rateLimiter{
		buckets:          make(map[string]rateBucket, maxRateLimitBuckets),
		trustedProxyCIDR: parseTrustedProxyCIDRs(trustedProxyCIDRs),
	}
}

func (l *rateLimiter) allow(clientID, path string, now time.Time) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	key := normalizeClientID(clientID)

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.nextPrune.IsZero() || !now.Before(l.nextPrune) {
		l.pruneLocked(now)
		l.nextPrune = now.Add(rateLimitWindow)
	}

	bucket, exists := l.buckets[key]
	if !exists {
		if len(l.buckets) >= maxRateLimitBuckets {
			return false, rateLimitWindow
		}
		bucket.windowStart = now
	} else if !now.Before(bucket.windowStart) && now.Sub(bucket.windowStart) >= rateLimitWindow {
		bucket = rateBucket{windowStart: now}
	}

	if bucket.totalCount <= maxOverallRequests {
		bucket.totalCount++
	}
	allowed := bucket.totalCount <= maxOverallRequests
	switch requestClass(path) {
	case requestClassSupport:
		if bucket.supportCount <= maxSupportRequests {
			bucket.supportCount++
		}
		allowed = allowed && bucket.supportCount <= maxSupportRequests
	case requestClassDiscovery:
		if bucket.discoveryCount <= maxDiscoveryRequests {
			bucket.discoveryCount++
		}
		allowed = allowed && bucket.discoveryCount <= maxDiscoveryRequests
	}
	l.buckets[key] = bucket
	if !allowed {
		return false, rateLimitWindow - now.Sub(bucket.windowStart)
	}
	return true, 0
}

type rateRequestClass uint8

const (
	requestClassGeneral rateRequestClass = iota
	requestClassSupport
	requestClassDiscovery
)

func requestClass(path string) rateRequestClass {
	switch path {
	case "/v1/support/bundle":
		return requestClassSupport
	case "/v1/storage/disks/discovery":
		return requestClassDiscovery
	default:
		return requestClassGeneral
	}
}

func (l *rateLimiter) pruneLocked(now time.Time) {
	for key, bucket := range l.buckets {
		if !now.Before(bucket.windowStart) && now.Sub(bucket.windowStart) >= 2*rateLimitWindow {
			delete(l.buckets, key)
		}
	}
}

func (l *rateLimiter) clientIDFromRequest(r *http.Request) string {
	remote := normalizeClientID(r.RemoteAddr)
	client, _, err := forwardedClientIP(r, l)
	if err != nil {
		return remote
	}
	return client
}

func (l *rateLimiter) trusts(clientID string) bool {
	addr, err := netip.ParseAddr(clientID)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
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
		return addr.Unmap().String()
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
			addr = addr.Unmap()
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
