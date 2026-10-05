package api

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRateLimiterAppliesSupportBundleLimitPerClient(t *testing.T) {
	limiter := newRateLimiter("")
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		allowed, _ := limiter.allow("10.0.0.1:12345", "/v1/support/bundle", now)
		if !allowed {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	allowed, retryAfter := limiter.allow("10.0.0.1:12345", "/v1/support/bundle", now)
	if allowed {
		t.Fatal("fourth support bundle request in the same window should be rejected")
	}
	if retryAfter != rateLimitWindow {
		t.Fatalf("expected retry-after duration %s, got %s", rateLimitWindow, retryAfter)
	}
	allowed, _ = limiter.allow("10.0.0.1:12345", "/v1/support/bundle", now.Add(rateLimitWindow))
	if !allowed {
		t.Fatal("request after window reset should be allowed")
	}
}

func TestClientIDFromRequestUsesForwardedHeadersOnlyFromTrustedProxy(t *testing.T) {
	limiter := newRateLimiter("192.0.2.10/32")
	req := httptest.NewRequest(http.MethodGet, "/v1/support/bundle", nil)
	req.RemoteAddr = "192.0.2.10:54321"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 198.51.100.8")
	if got := limiter.clientIDFromRequest(req); got != "198.51.100.8" {
		t.Fatalf("expected the nearest untrusted forwarded client IP, got %q", got)
	}

	req.Header.Del("X-Forwarded-For")
	req.Header.Set("X-Real-IP", "198.51.100.9")
	if got := limiter.clientIDFromRequest(req); got != "192.0.2.10" {
		t.Fatalf("X-Real-IP is not trusted; expected proxy RemoteAddr, got %q", got)
	}

	untrusted := newRateLimiter("")
	if got := untrusted.clientIDFromRequest(req); got != "192.0.2.10" {
		t.Fatalf("expected untrusted proxy headers to be ignored, got %q", got)
	}
}

func TestClientIDFromRequestWalksTrustedProxyChainFromRight(t *testing.T) {
	limiter := newRateLimiter("10.0.0.0/8")
	req := httptest.NewRequest(http.MethodGet, "/v1/support/bundle", nil)
	req.RemoteAddr = "10.0.0.2:443"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 198.51.100.8, 10.0.0.1")
	if got := limiter.clientIDFromRequest(req); got != "198.51.100.8" {
		t.Fatalf("expected the nearest untrusted hop after trusted proxies, got %q", got)
	}
}

func TestClientIDFromRequestIgnoresGlobalTrustedProxyCIDRs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cidr       string
		remoteAddr string
		header     string
		want       string
	}{
		{
			name:       "ipv4 global",
			cidr:       "0.0.0.0/0",
			remoteAddr: "198.51.100.50:12345",
			header:     "203.0.113.7",
			want:       "198.51.100.50",
		},
		{
			name:       "ipv6 global",
			cidr:       "::/0",
			remoteAddr: "[2001:db8::50]:12345",
			header:     "2001:db8:1::7",
			want:       "2001:db8::50",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limiter := newRateLimiter(tc.cidr)
			req := httptest.NewRequest(http.MethodGet, "/v1/support/bundle", nil)
			req.RemoteAddr = tc.remoteAddr
			req.Header.Set("X-Forwarded-For", tc.header)
			if got := limiter.clientIDFromRequest(req); got != tc.want {
				t.Fatalf("expected global trusted proxy CIDR to be ignored, got %q want %q", got, tc.want)
			}
		})
	}
}

func TestParseTrustedProxyCIDRsWarnsForGlobalCIDR(t *testing.T) {
	var logs bytes.Buffer
	original := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(original) })

	prefixes := parseTrustedProxyCIDRs("0.0.0.0/0,127.0.0.1/32,::/0")
	if len(prefixes) != 1 || prefixes[0].String() != "127.0.0.1/32" {
		t.Fatalf("expected only specific loopback prefix to remain, got %v", prefixes)
	}
	if got := logs.String(); !strings.Contains(got, "0.0.0.0/0") || !strings.Contains(got, "::/0") {
		t.Fatalf("expected warnings for global CIDRs, got %q", got)
	}
}

func TestRateLimitMiddlewareSetsRetryAfter(t *testing.T) {
	limiter := newRateLimiter("198.51.100.0/24")
	handler := (&Server{limiter: limiter}).rateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/support/bundle", nil)
		req.RemoteAddr = "198.51.100.1:443"
		req.Header.Set("X-Forwarded-For", "203.0.113.10")
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusNoContent {
			t.Fatalf("request %d expected 204, got %d", i+1, resp.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/support/bundle", nil)
	req.RemoteAddr = "198.51.100.1:443"
	req.Header.Set("X-Forwarded-For", "203.0.113.10")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", resp.Code)
	}
	if got := resp.Header().Get("Retry-After"); got == "" {
		t.Fatal("expected Retry-After header on 429")
	}

	otherClientReq := httptest.NewRequest(http.MethodGet, "/v1/support/bundle", nil)
	otherClientReq.RemoteAddr = "198.51.100.1:443"
	otherClientReq.Header.Set("X-Forwarded-For", "203.0.113.11")
	otherClientResp := httptest.NewRecorder()
	handler.ServeHTTP(otherClientResp, otherClientReq)
	if otherClientResp.Code != http.StatusNoContent {
		t.Fatalf("different forwarded client should have a separate bucket, got %d", otherClientResp.Code)
	}
}

func TestRateLimitMiddlewareIgnoresSpoofedForwardedForFromUntrustedClient(t *testing.T) {
	limiter := newRateLimiter("")
	handler := (&Server{limiter: limiter}).rateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/support/bundle", nil)
		req.RemoteAddr = "198.51.100.50:12345"
		req.Header.Set("X-Forwarded-For", "203.0.113."+strconv.Itoa(i+1))
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusNoContent {
			t.Fatalf("request %d expected 204, got %d", i+1, resp.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/support/bundle", nil)
	req.RemoteAddr = "198.51.100.50:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.99")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusTooManyRequests {
		t.Fatalf("expected spoofed forwarded headers to share RemoteAddr bucket and return 429, got %d", resp.Code)
	}
}

func TestRateLimiterPruneIsTimeGated(t *testing.T) {
	limiter := newRateLimiter("")
	now := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	limiter.buckets["198.51.100.1"] = rateBucket{windowStart: now.Add(-3 * rateLimitWindow), totalCount: 1}
	limiter.buckets["198.51.100.2"] = rateBucket{windowStart: now, totalCount: 1}

	allowed, _ := limiter.allow("client-a", "/v1/a", now.Add(rateLimitWindow))
	if !allowed {
		t.Fatal("expected request to be allowed")
	}
	if _, ok := limiter.buckets["198.51.100.1"]; ok {
		t.Fatal("expected stale bucket to be pruned")
	}
	firstNextPrune := limiter.nextPrune

	limiter.buckets["198.51.100.3"] = rateBucket{windowStart: now.Add(-3 * rateLimitWindow), totalCount: 1}
	allowed, _ = limiter.allow("client-b", "/v1/a", now.Add(rateLimitWindow+time.Second))
	if !allowed {
		t.Fatal("expected second request to be allowed")
	}
	if _, ok := limiter.buckets["198.51.100.3"]; !ok {
		t.Fatal("expected second stale bucket to remain until next prune interval")
	}
	if !limiter.nextPrune.Equal(firstNextPrune) {
		t.Fatalf("expected nextPrune unchanged, got %s want %s", limiter.nextPrune, firstNextPrune)
	}
}

func TestRateLimiterRejectsNewClientWhenFullWithoutEviction(t *testing.T) {
	limiter := newRateLimiter("")
	now := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	for i := 0; i < maxRateLimitBuckets; i++ {
		limiter.buckets["2001:db8::"+strconv.FormatInt(int64(i+1), 16)] = rateBucket{windowStart: now, totalCount: 1}
	}
	limiter.nextPrune = now.Add(rateLimitWindow)

	allowed, retryAfter := limiter.allow("2001:db8::ffff", "/v1/a", now)
	if allowed {
		t.Fatal("expected a new client to be rejected while all client slots are occupied")
	}
	if len(limiter.buckets) != maxRateLimitBuckets {
		t.Fatalf("expected bucket map to remain capped at %d, got %d", maxRateLimitBuckets, len(limiter.buckets))
	}
	if _, ok := limiter.buckets["2001:db8::1"]; !ok {
		t.Fatal("existing client bucket must not be evicted")
	}
	if retryAfter <= 0 {
		t.Fatal("full client table must include a positive retry interval")
	}
}

func TestRateLimiterUsesOneEntryPerIPAndCapsAllRequestClasses(t *testing.T) {
	limiter := newRateLimiter("")
	now := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10_000; i++ {
		limiter.allow("203.0.113.8:1234", "/v1/variable/"+strconv.Itoa(i), now)
	}
	if len(limiter.buckets) != 1 {
		t.Fatalf("10,000 path variants allocated %d limiter entries", len(limiter.buckets))
	}
	if maxRateLimitBuckets != 4096 {
		t.Fatalf("expected a fixed client capacity of 4096, got %d", maxRateLimitBuckets)
	}

	support := newRateLimiter("")
	for i := 0; i < 3; i++ {
		if allowed, _ := support.allow("203.0.113.9", "/v1/support/bundle", now); !allowed {
			t.Fatalf("support request %d should be allowed", i+1)
		}
	}
	if allowed, _ := support.allow("203.0.113.9", "/v1/support/bundle", now); allowed {
		t.Fatal("support bundle cap should be three per minute")
	}
	for i := 4; i < 300; i++ {
		if allowed, _ := support.allow("203.0.113.9", "/v1/ordinary", now); !allowed {
			t.Fatalf("overall request %d should remain allowed", i)
		}
	}
	if allowed, _ := support.allow("203.0.113.9", "/v1/ordinary", now); allowed {
		t.Fatal("overall cap should be 300 per minute")
	}

	discovery := newRateLimiter("")
	for i := 0; i < 30; i++ {
		if allowed, _ := discovery.allow("203.0.113.10", "/v1/storage/disks/discovery", now); !allowed {
			t.Fatalf("discovery request %d should be allowed", i+1)
		}
	}
	if allowed, _ := discovery.allow("203.0.113.10", "/v1/storage/disks/discovery", now); allowed {
		t.Fatal("disk discovery cap should be thirty per minute")
	}
}
