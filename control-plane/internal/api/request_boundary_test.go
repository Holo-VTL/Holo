package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteOriginMiddlewareAllowsSameOriginAndCLIAndRejectsCrossSiteWrites(t *testing.T) {
	var handled int
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		handled++
		w.WriteHeader(http.StatusNoContent)
	})
	handler := (&Server{limiter: newRateLimiter("")}).writeOriginMiddleware(next)

	for _, tc := range []struct {
		name       string
		origin     []string
		fetchSite  string
		wantStatus int
	}{
		{name: "same origin", origin: []string{"http://example.com"}, fetchSite: "same-origin", wantStatus: http.StatusNoContent},
		{name: "default port normalization", origin: []string{"http://example.com:80"}, wantStatus: http.StatusNoContent},
		{name: "cli has no browser metadata", wantStatus: http.StatusNoContent},
		{name: "cross origin", origin: []string{"https://attacker.example"}, wantStatus: http.StatusForbidden},
		{name: "null origin", origin: []string{"null"}, wantStatus: http.StatusForbidden},
		{name: "origin path is invalid", origin: []string{"http://example.com/path"}, wantStatus: http.StatusForbidden},
		{name: "same site is still cross origin", fetchSite: "same-site", wantStatus: http.StatusForbidden},
		{name: "cross site without origin", fetchSite: "cross-site", wantStatus: http.StatusForbidden},
		{name: "unknown fetch metadata", fetchSite: "untrusted-value", wantStatus: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/action", strings.NewReader(`{}`))
			for _, origin := range tc.origin {
				req.Header.Add("Origin", origin)
			}
			if tc.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}
			before := handled
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			if resp.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d body=%q", tc.wantStatus, resp.Code, resp.Body.String())
			}
			if tc.wantStatus == http.StatusForbidden && handled != before {
				t.Fatal("rejected source reached the handler")
			}
		})
	}

	if handled != 3 {
		t.Fatalf("expected only three accepted requests to reach the handler, got %d", handled)
	}
}

func TestWriteOriginMiddlewareRejectsDuplicateAndConflictingBrowserHeaders(t *testing.T) {
	handler := (&Server{limiter: newRateLimiter("")}).writeOriginMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, tc := range []struct {
		name   string
		origin []string
		site   string
	}{
		{name: "duplicate origin", origin: []string{"http://example.com", "http://example.com"}},
		{name: "origin and cross-site metadata conflict", origin: []string{"http://example.com"}, site: "cross-site"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodDelete, "http://example.com/v1/action", nil)
			for _, origin := range tc.origin {
				req.Header.Add("Origin", origin)
			}
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			if resp.Code != http.StatusForbidden {
				t.Fatalf("expected duplicate or conflicting browser metadata to be rejected, got %d", resp.Code)
			}
		})
	}
}

func TestUntrustedForwardedOriginHeadersAreIgnored(t *testing.T) {
	handler := (&Server{limiter: newRateLimiter("")}).writeOriginMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/action", nil)
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("untrusted forwarded headers must not alter the direct same-origin request, got %d", resp.Code)
	}
}

func TestRequestTargetLimitRejectsBeforeRateLimitAllocation(t *testing.T) {
	limiter := newRateLimiter("")
	handler := (&Server{limiter: limiter}).requestTargetMiddleware((&Server{limiter: limiter}).rateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	req := httptest.NewRequest(http.MethodGet, "/healthz?x="+strings.Repeat("a", maxRequestTargetBytes), nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusRequestURITooLong {
		t.Fatalf("expected overlong request target to return 414, got %d", resp.Code)
	}
	if len(limiter.buckets) != 0 {
		t.Fatalf("overlong request allocated rate-limit state: %d buckets", len(limiter.buckets))
	}
}

func TestJSONBodyContentTypeMiddlewarePreservesEmptyActionsAndRejectsWrongMediaType(t *testing.T) {
	srv := newTestServerWithAPIKey(t, "")

	wrongType := httptest.NewRequest(http.MethodPost, "/v1/storage/pools", strings.NewReader(`{"poolId":"pool-bad-media","name":"Bad Media"}`))
	wrongType.Header.Set("Content-Type", "text/plain")
	wrongTypeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(wrongTypeResp, wrongType)
	if wrongTypeResp.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected non-JSON body to return 415, got %d body=%s", wrongTypeResp.Code, wrongTypeResp.Body.String())
	}
	if pools := srv.storage.svc.ListPools(httptest.NewRequest(http.MethodGet, "/", nil).Context()); len(pools) != 0 {
		t.Fatalf("wrong media type changed storage state: %+v", pools)
	}

	missingType := httptest.NewRequest(http.MethodPost, "/v1/storage/pools", strings.NewReader(`{"poolId":"pool-missing-media","name":"Missing Media"}`))
	missingTypeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(missingTypeResp, missingType)
	if missingTypeResp.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected missing content type to return 415, got %d", missingTypeResp.Code)
	}
	duplicateType := httptest.NewRequest(http.MethodPost, "/v1/storage/pools", strings.NewReader(`{"poolId":"pool-duplicate-media","name":"Duplicate Media"}`))
	duplicateType.Header.Set("Content-Type", "application/json")
	duplicateType.Header.Add("Content-Type", "text/plain")
	duplicateTypeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(duplicateTypeResp, duplicateType)
	if duplicateTypeResp.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected duplicate content type to return 415, got %d", duplicateTypeResp.Code)
	}

	emptyRequired := httptest.NewRequest(http.MethodPost, "/v1/storage/pools", nil)
	emptyRequiredResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(emptyRequiredResp, emptyRequired)
	if emptyRequiredResp.Code != http.StatusBadRequest {
		t.Fatalf("expected empty required JSON to return 400, got %d", emptyRequiredResp.Code)
	}

	createLibrary := newAuthedRequest(http.MethodPost, "/v1/libraries", strings.NewReader(`{"libraryId":"lib-empty-body","name":"Empty Body"}`))
	createLibrary.Header.Set("Content-Type", "application/json; charset=utf-8")
	createLibraryResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createLibraryResp, createLibrary)
	if createLibraryResp.Code != http.StatusCreated {
		t.Fatalf("create library: expected 201, got %d body=%s", createLibraryResp.Code, createLibraryResp.Body.String())
	}

	optionalEmpty := httptest.NewRequest(http.MethodPost, "/v1/libraries/lib-empty-body/slots", nil)
	optionalEmptyResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(optionalEmptyResp, optionalEmpty)
	if optionalEmptyResp.Code != http.StatusOK {
		t.Fatalf("expected optional empty body to retain existing action behavior, got %d body=%s", optionalEmptyResp.Code, optionalEmptyResp.Body.String())
	}
}

func TestWriteOriginRejectionHappensBeforeJSONMediaValidation(t *testing.T) {
	srv := newTestServerWithAPIKey(t, "")
	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/storage/pools", strings.NewReader(`{"poolId":"pool-origin","name":"Origin"}`))
	req.Header.Set("Origin", "https://attacker.example")
	req.Header.Set("Content-Type", "text/plain")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("expected origin rejection before body validation, got %d", resp.Code)
	}
	if pools := srv.storage.svc.ListPools(req.Context()); len(pools) != 0 {
		t.Fatalf("rejected origin changed storage state: %+v", pools)
	}
}

func TestTrustedProxyOriginRequiresSingleValidForwardedHostAndProto(t *testing.T) {
	server := &Server{limiter: newRateLimiter("192.0.2.10/32")}
	handler := server.writeOriginMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requestIsHTTPS(r, server.limiter) {
			t.Error("trusted HTTPS proxy should receive HSTS")
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	valid := httptest.NewRequest(http.MethodPost, "http://internal.example/v1/action", nil)
	valid.RemoteAddr = "192.0.2.10:54321"
	valid.Header.Set("X-Forwarded-Host", "console.example:443")
	valid.Header.Set("X-Forwarded-Proto", "https")
	valid.Header.Set("Origin", "https://console.example")
	validResp := httptest.NewRecorder()
	handler.ServeHTTP(validResp, valid)
	if validResp.Code != http.StatusNoContent {
		t.Fatalf("expected valid trusted proxy origin, got %d body=%s", validResp.Code, validResp.Body.String())
	}

	invalidChain := httptest.NewRequest(http.MethodPost, "http://internal.example/v1/action", nil)
	invalidChain.RemoteAddr = "192.0.2.10:54321"
	invalidChain.Header.Set("X-Forwarded-Host", "console.example:443")
	invalidChain.Header.Set("X-Forwarded-Proto", "https")
	invalidChain.Header.Set("X-Forwarded-For", "not-an-ip")
	invalidChain.Header.Set("Origin", "https://console.example")
	invalidChainResp := httptest.NewRecorder()
	handler.ServeHTTP(invalidChainResp, invalidChain)
	if invalidChainResp.Code != http.StatusForbidden {
		t.Fatalf("expected malformed trusted client chain to be rejected, got %d", invalidChainResp.Code)
	}

	for _, name := range []string{"X-Forwarded-Host", "X-Forwarded-Proto"} {
		req := httptest.NewRequest(http.MethodPost, "http://internal.example/v1/action", nil)
		req.RemoteAddr = "192.0.2.10:54321"
		req.Header.Set("X-Forwarded-Host", "console.example")
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Add(name, "duplicate")
		req.Header.Set("Origin", "https://console.example")
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusForbidden {
			t.Fatalf("expected duplicate %s to be rejected, got %d", name, resp.Code)
		}
	}
}
