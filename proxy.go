package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// routeKey is the request-context key under which the handler passes the matched route to the
// proxy's Rewrite and ModifyResponse hooks.
type routeKey struct{}

type proxyRoute struct {
	source string // inbound Host, as the client sent it (may include a port)
	target *url.URL
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer, which ReverseProxy needs
// to flush streamed responses and to hijack the connection for WebSocket upgrades.
func (lrw *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return lrw.ResponseWriter
}

func newProxyHandler(skipSSL bool, proxyAddr string, enableH2 bool, disableKeepAlive bool) http.Handler {

	// --- H2 Negotiation Fix ---

	// Determine TLS ALPN protocols
	var nextProtos []string
	// Determine TLSNextProto map
	var tlsNextProto map[string]func(authority string, c *tls.Conn) http.RoundTripper

	if !enableH2 {
		// Aggressively force HTTP/1.1 to bypass proxy/firewall H2 inspection issues
		nextProtos = []string{"http/1.1"}
		// Explicitly setting an EMPTY MAP disables HTTP/2 support in the transport
		tlsNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)
	}
	// If enableH2 is true, nextProtos and tlsNextProto remain nil, using Go's default H2 support.

	// Configure Transport
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: skipSSL,
			NextProtos:         nextProtos, // Forces http/1.1 if H2 is disabled
		},
		TLSNextProto:      tlsNextProto, // Explicitly disables H2 if enableH2 is false
		ForceAttemptHTTP2: enableH2,
		Proxy:             http.ProxyFromEnvironment,
		DisableKeepAlives: disableKeepAlive, // New option to fix 'unsolicited response'
	}

	if proxyAddr != "" {
		pURL, err := url.Parse(proxyAddr)
		if err != nil {
			log.Fatalf("Invalid proxy URL: %v", err)
		}
		transport.Proxy = http.ProxyURL(pURL)
		log.Printf("Using outbound proxy: %s", proxyAddr)
	}

	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			route := pr.In.Context().Value(routeKey{}).(proxyRoute)

			// SetURL joins the target's base path with the request path and sends the target's host as Host
			pr.SetURL(route.target)

			// Rewrite strips client-sent forwarding headers; put them back so the proxy stays
			// transparent. X-Forwarded-For stays dropped so the target never sees the client IP.
			for _, h := range []string{"Forwarded", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if v, ok := pr.In.Header[h]; ok {
					pr.Out.Header[h] = v
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if route, ok := resp.Request.Context().Value(routeKey{}).(proxyRoute); ok {
				rewriteRedirectAndCookies(resp, route)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if !errors.Is(err, context.Canceled) {
				log.Printf("[ERROR] Proxy Error for %s: %v", r.URL.Host, err)
			}
			w.WriteHeader(http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[HTTP-IN] %s %s %s", r.Method, r.Host, r.URL.Path)
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w}

		if target, ok := lookupRoute(r.Host); ok {
			route := proxyRoute{source: r.Host, target: target}
			proxy.ServeHTTP(lrw, r.WithContext(context.WithValue(r.Context(), routeKey{}, route)))
		} else {
			http.Error(lrw, "goRebind: no route for host "+r.Host, http.StatusNotFound)
		}

		status := lrw.statusCode
		if status == 0 {
			// ReverseProxy hijacks upgraded (e.g. WebSocket) connections without calling WriteHeader
			status = http.StatusSwitchingProtocols
		}
		log.Printf("[HTTP-OUT] %s %s %s -> %d (%s)", r.Method, r.Host, r.URL.Path, status, time.Since(start).Round(time.Millisecond))
	})
}

// rewriteRedirectAndCookies makes the target's redirects and cookies point back at the proxy, so
// a client whose source host differs from the target host stays on the proxy for the next request.
func rewriteRedirectAndCookies(resp *http.Response, route proxyRoute) {
	targetHost := route.target.Hostname()
	sourceHost := route.source
	if h, _, err := net.SplitHostPort(sourceHost); err == nil {
		sourceHost = h
	}

	// Redirects back to the target host are rewritten to the source host, over plain HTTP, since
	// that is how clients reach the proxy.
	if loc := resp.Header.Get("Location"); loc != "" {
		if u, err := url.Parse(loc); err == nil && strings.EqualFold(u.Hostname(), targetHost) {
			u.Scheme = "http"
			u.Host = route.source // preserve the authority (and any port) the client used
			resp.Header.Set("Location", u.String())
		}
	}

	// Scope any Domain-bearing cookie to the source host so the client sends it back through us.
	for i, c := range resp.Header["Set-Cookie"] {
		resp.Header["Set-Cookie"][i] = rewriteCookieDomain(c, sourceHost)
	}
}

// rewriteCookieDomain forces a cookie's Domain attribute to host, leaving the rest of the cookie
// (name, value, Path, Secure, HttpOnly, …) untouched. A cookie with no Domain is left as-is.
func rewriteCookieDomain(setCookie, host string) string {
	parts := strings.Split(setCookie, ";")
	for i, p := range parts {
		if attr := strings.TrimSpace(p); len(attr) > 7 && strings.EqualFold(attr[:7], "domain=") {
			parts[i] = " Domain=" + host
		}
	}
	return strings.Join(parts, ";")
}
