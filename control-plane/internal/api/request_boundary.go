package api

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

const maxRequestTargetBytes = 8 << 10

type parsedOrigin struct {
	scheme string
	host   string
	port   string
}

func (o parsedOrigin) equal(other parsedOrigin) bool {
	return o.scheme == other.scheme && o.host == other.host && o.port == other.port
}

func (s *Server) requestTargetMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := r.RequestURI
		if target == "" && r.URL != nil {
			target = r.URL.EscapedPath()
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
		}
		if len(target) > maxRequestTargetBytes {
			s.recordManagementRejection("request_target")
			respondError(w, http.StatusRequestURITooLong, "request target too long", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) writeOriginMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWriteMethod(r.Method) || validateWriteOrigin(r, s.limiter) != nil {
			if isWriteMethod(r.Method) {
				s.recordManagementRejection("request_origin")
				respondError(w, http.StatusForbidden, "request origin is not allowed", nil)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) jsonBodyContentTypeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") || !isWriteMethod(r.Method) || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}

		var prefix [1]byte
		n, err := io.ReadFull(r.Body, prefix[:])
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			respondError(w, http.StatusBadRequest, "invalid request body", nil)
			return
		}
		if n == 0 {
			next.ServeHTTP(w, r)
			return
		}
		if validateJSONContentType(r.Header) != nil {
			s.recordManagementRejection("json_content_type")
			respondError(w, http.StatusUnsupportedMediaType, "content type must be application/json", nil)
			return
		}
		r.Body = &prefixedReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix[:n]), r.Body), closer: r.Body}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recordManagementRejection(reason string) {
	if s != nil && s.metricsHD != nil && s.metricsHD.registry != nil {
		s.metricsHD.registry.RecordManagementRejection(reason)
	}
}

type prefixedReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *prefixedReadCloser) Close() error { return r.closer.Close() }

func validateWriteOrigin(r *http.Request, limiter *rateLimiter) error {
	if r == nil || r.URL == nil {
		return errors.New("invalid request")
	}
	requestOrigin, err := effectiveRequestOrigin(r, limiter)
	if err != nil {
		return err
	}

	origins := r.Header.Values("Origin")
	if len(origins) > 1 {
		return errors.New("duplicate origin")
	}
	if len(origins) == 1 {
		origin, err := parseOrigin(origins[0])
		if err != nil || !origin.equal(requestOrigin) {
			return errors.New("origin mismatch")
		}
	}

	fetchSites := r.Header.Values("Sec-Fetch-Site")
	if len(fetchSites) > 1 {
		return errors.New("duplicate fetch metadata")
	}
	if len(fetchSites) == 1 {
		value := strings.ToLower(strings.TrimSpace(fetchSites[0]))
		if strings.Contains(value, ",") {
			return errors.New("invalid fetch metadata")
		}
		switch value {
		case "same-origin", "none":
		case "same-site", "cross-site":
			return errors.New("cross-origin write")
		default:
			return errors.New("invalid fetch metadata")
		}
	}
	return nil
}

func effectiveRequestOrigin(r *http.Request, limiter *rateLimiter) (parsedOrigin, error) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	clientIP := normalizeClientID(r.RemoteAddr)
	trustedProxy := limiter != nil && limiter.trusts(clientIP)
	if trustedProxy {
		forwardedHost, hostPresent, err := singleForwardedHeader(r, "X-Forwarded-Host")
		if err != nil {
			return parsedOrigin{}, err
		}
		if hostPresent {
			host = forwardedHost
		}
		forwardedProto, protoPresent, err := singleForwardedHeader(r, "X-Forwarded-Proto")
		if err != nil {
			return parsedOrigin{}, err
		}
		if protoPresent {
			scheme = strings.ToLower(forwardedProto)
		}
		if _, _, err := forwardedClientIP(r, limiter); err != nil {
			return parsedOrigin{}, err
		}
	}
	return parseAuthorityOrigin(scheme, host)
}

func singleForwardedHeader(r *http.Request, name string) (string, bool, error) {
	values := r.Header.Values(name)
	if len(values) == 0 {
		return "", false, nil
	}
	if len(values) != 1 {
		return "", true, errors.New("duplicate forwarded header")
	}
	value := strings.TrimSpace(values[0])
	if value == "" || strings.Contains(value, ",") {
		return "", true, errors.New("invalid forwarded header")
	}
	return value, true, nil
}

func forwardedClientIP(r *http.Request, limiter *rateLimiter) (string, bool, error) {
	remote := normalizeClientID(r.RemoteAddr)
	if limiter == nil || !limiter.trusts(remote) {
		return remote, false, nil
	}
	values := r.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return remote, true, nil
	}
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return remote, true, errors.New("invalid forwarded client chain")
	}
	parts := strings.Split(values[0], ",")
	for i := len(parts) - 1; i >= 0; i-- {
		value := strings.TrimSpace(parts[i])
		addr, err := netip.ParseAddr(value)
		if err != nil {
			return remote, true, errors.New("invalid forwarded client chain")
		}
		addr = addr.Unmap()
		if !limiter.trusts(addr.String()) {
			return addr.String(), true, nil
		}
	}
	return remote, true, errors.New("forwarded chain has no client")
}

func parseOrigin(value string) (parsedOrigin, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "null") || strings.ContainsAny(value, ",\r\n\t ") {
		return parsedOrigin{}, errors.New("invalid origin")
	}
	u, err := url.Parse(value)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return parsedOrigin{}, errors.New("invalid origin")
	}
	return parseAuthorityOrigin(u.Scheme, u.Host)
}

func parseAuthorityOrigin(scheme, authority string) (parsedOrigin, error) {
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if scheme != "http" && scheme != "https" {
		return parsedOrigin{}, errors.New("invalid origin scheme")
	}
	if authority == "" || strings.ContainsAny(authority, "/?#@,\r\n\t ") || strings.HasSuffix(authority, ":") {
		return parsedOrigin{}, errors.New("invalid origin authority")
	}
	u, err := url.Parse(scheme + "://" + authority)
	if err != nil || u.Host == "" || u.User != nil || u.Hostname() == "" {
		return parsedOrigin{}, errors.New("invalid origin authority")
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, "%") {
		return parsedOrigin{}, errors.New("invalid origin host")
	}
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	} else {
		parsedPort, err := strconv.Atoi(port)
		if err != nil || parsedPort < 1 || parsedPort > 65535 {
			return parsedOrigin{}, errors.New("invalid origin port")
		}
		port = strconv.Itoa(parsedPort)
	}
	return parsedOrigin{scheme: scheme, host: host, port: port}, nil
}

func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func requestIsHTTPS(r *http.Request, limiter *rateLimiter) bool {
	if r.TLS != nil {
		return true
	}
	clientIP := normalizeClientID(r.RemoteAddr)
	if limiter == nil || !limiter.trusts(clientIP) {
		return false
	}
	proto, present, err := singleForwardedHeader(r, "X-Forwarded-Proto")
	return err == nil && present && strings.EqualFold(proto, "https")
}

func normalizeForwardedRemote(value string) string {
	remote := normalizeClientID(value)
	if ip, err := netip.ParseAddr(remote); err == nil {
		return ip.Unmap().String()
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		if ip, parseErr := netip.ParseAddr(host); parseErr == nil {
			return ip.Unmap().String()
		}
	}
	return remote
}
