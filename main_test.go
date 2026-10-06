package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// Tests replace package globals (routeMap, interfaceIP, upstreamDNS), so none of them run in parallel.

// setRoutes replaces the routing table for the duration of a test.
func setRoutes(t *testing.T, routes map[string]string) {
	t.Helper()
	m := make(map[string]*url.URL)
	for src, target := range routes {
		u, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}
		m[src] = u
	}
	mu.Lock()
	old := routeMap
	routeMap = m
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		routeMap = old
		mu.Unlock()
	})
}

// --- Config ---

func TestNormalizeHost(t *testing.T) {
	tests := map[string]string{
		"api.local":      "api.local",
		"API.Local:8080": "api.local",
		"api.local.":     "api.local",
		"api.local.:80":  "api.local",
		"[::1]:80":       "::1",
	}
	for in, want := range tests {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	setRoutes(t, nil)
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := `[
		{"source": "API.Local.", "target": "http://127.0.0.1:9090"},
		{"source": "bad.local", "target": "api.example.com"}
	]`
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	loadConfig(path)

	if _, ok := lookupRoute("api.local"); !ok {
		t.Error("source should be stored lowercased without the trailing dot")
	}
	if _, ok := lookupRoute("bad.local"); ok {
		t.Error("a target without a scheme should be skipped")
	}
}

// --- HTTP proxy ---

// startProxy serves the goRebind handler on a loopback port and returns its base URL.
func startProxy(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(newProxyHandler(false, "", false, false))
	t.Cleanup(srv.Close)
	return srv.URL
}

// get sends a GET through the proxy with the given Host header and returns the drained response.
func get(t *testing.T, proxyURL, host, path string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, proxyURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func TestProxyRouting(t *testing.T) {
	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(backend.Close)
	setRoutes(t, map[string]string{"api.local": backend.URL})
	proxy := startProxy(t)

	tests := []struct {
		host     string
		want     int
		wantHits int32
	}{
		{"api.local", http.StatusOK, 1},
		{"API.Local:18080", http.StatusOK, 1}, // what a browser sends on a non-80 port
		{"api.local.", http.StatusOK, 1},
		{"other.local", http.StatusNotFound, 0},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			before := hits.Load()
			resp := get(t, proxy, tt.host, "/", nil)
			if resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
			if got := hits.Load() - before; got != tt.wantHits {
				t.Errorf("backend hits = %d, want %d", got, tt.wantHits)
			}
		})
	}
}

func TestProxyRewritesRequest(t *testing.T) {
	type seenRequest struct {
		host, uri string
		header    http.Header
	}
	seen := make(chan seenRequest, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- seenRequest{r.Host, r.RequestURI, r.Header.Clone()}
	}))
	t.Cleanup(backend.Close)
	setRoutes(t, map[string]string{"api.local": backend.URL + "/base"})
	proxy := startProxy(t)

	get(t, proxy, "api.local", "/x?q=1", http.Header{
		"X-Forwarded-For":   {"203.0.113.7"},
		"X-Forwarded-Proto": {"https"},
	})
	got := <-seen

	if want := strings.TrimPrefix(backend.URL, "http://"); got.host != want {
		t.Errorf("Host = %q, want the target's host %q", got.host, want)
	}
	if got.uri != "/base/x?q=1" {
		t.Errorf("request URI = %q, want the target path joined with the request path", got.uri)
	}
	if xff := got.header.Get("X-Forwarded-For"); xff != "" {
		t.Errorf("X-Forwarded-For = %q, want it dropped", xff)
	}
	if proto := got.header.Get("X-Forwarded-Proto"); proto != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want the client's value passed through", proto)
	}
}

func TestProxyRewritesLocationAndCookies(t *testing.T) {
	// The backend replies with a redirect and a cookie scoped to its own (target) host.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostOnly := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			hostOnly = h
		}
		w.Header().Set("Location", "https://"+r.Host+"/next")
		w.Header().Add("Set-Cookie", "sid=abc; Domain="+hostOnly+"; Path=/; HttpOnly")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(backend.Close)
	setRoutes(t, map[string]string{"secure.internal": backend.URL})
	proxy := startProxy(t)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest(http.MethodGet, proxy+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "secure.internal"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if got := resp.Header.Get("Location"); got != "http://secure.internal/next" {
		t.Errorf("Location = %q, want it rewritten back to the source host over http", got)
	}
	cookie := resp.Header.Get("Set-Cookie")
	if !strings.Contains(cookie, "Domain=secure.internal") {
		t.Errorf("Set-Cookie = %q, want Domain rewritten to the source host", cookie)
	}
	if !strings.Contains(cookie, "Path=/") || !strings.Contains(cookie, "HttpOnly") {
		t.Errorf("Set-Cookie = %q, want other attributes preserved", cookie)
	}
}

func TestProxyUnreachableTarget(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := l.Addr().String()
	l.Close()
	setRoutes(t, map[string]string{"api.local": "http://" + closedAddr})

	if resp := get(t, startProxy(t), "api.local", "/", nil); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestProxyUpgrade(t *testing.T) {
	// The backend switches to a line-echo protocol, standing in for a WebSocket server.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "echo" {
			http.Error(w, "upgrade required", http.StatusBadRequest)
			return
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		brw.Flush()
		line, _ := brw.ReadString('\n')
		brw.WriteString(line)
		brw.Flush()
	}))
	t.Cleanup(backend.Close)
	setRoutes(t, map[string]string{"ws.local": backend.URL})
	proxy := startProxy(t)

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: ws.local\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}

	fmt.Fprint(conn, "ping\n")
	if line, err := br.ReadString('\n'); err != nil || line != "ping\n" {
		t.Fatalf("echo = %q, %v; want %q", line, err, "ping\n")
	}
}

// --- HTTPS SNI passthrough ---

func TestSNIPassthrough(t *testing.T) {
	// A TLS backend with its own cert. We tunnel to it by SNI without decrypting.
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello-sni")
	}))
	t.Cleanup(backend.Close)
	setRoutes(t, map[string]string{"secure.internal": backend.URL})

	ln := startSNIServer("127.0.0.1:0")
	t.Cleanup(func() { ln.Close() })

	// The client completes a real TLS handshake with the backend THROUGH the tunnel. The name
	// won't match the backend's cert, so skip verification — we are testing transport, not trust.
	// A deadline keeps a broken tunnel from hanging the test instead of failing it.
	dialer := &tls.Dialer{Config: &tls.Config{
		ServerName:         "secure.internal",
		InsecureSkipVerify: true,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	nc, err := dialer.DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("TLS handshake through the SNI tunnel failed: %v", err)
	}
	conn := nc.(*tls.Conn)
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: secure.internal\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello-sni" {
		t.Errorf("body = %q, want %q delivered end-to-end through the tunnel", body, "hello-sni")
	}
}

// --- DNS ---

// fakeDNSWriter captures the reply. The embedded interface is nil; only the methods
// handleDNSRequest uses are implemented.
type fakeDNSWriter struct {
	dns.ResponseWriter
	msg *dns.Msg
}

func (f *fakeDNSWriter) WriteMsg(m *dns.Msg) error { f.msg = m; return nil }

func (f *fakeDNSWriter) RemoteAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5353}
}

// setDNS sets the interface IP and upstream server for the duration of a test.
func setDNS(t *testing.T, ip, upstream string) {
	t.Helper()
	oldIP, oldUpstream := interfaceIP, upstreamDNS
	interfaceIP, upstreamDNS = net.ParseIP(ip).To4(), upstream
	t.Cleanup(func() { interfaceIP, upstreamDNS = oldIP, oldUpstream })
}

// query runs one question through handleDNSRequest and returns the reply.
func query(t *testing.T, name string, qtype uint16) *dns.Msg {
	t.Helper()
	req := new(dns.Msg)
	req.SetQuestion(dns.Fqdn(name), qtype)
	w := &fakeDNSWriter{}
	handleDNSRequest(w, req)
	if w.msg == nil {
		t.Fatal("no reply written")
	}
	return w.msg
}

// startUpstream runs a fake upstream DNS server: "missing.example." is NXDOMAIN and every other
// name has one MX record with a 300 s TTL.
func startUpstream(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	srv := &dns.Server{
		PacketConn:        pc,
		NotifyStartedFunc: func() { close(started) },
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
			m := new(dns.Msg)
			m.SetReply(r)
			if r.Question[0].Name == "missing.example." {
				m.Rcode = dns.RcodeNameError
			} else {
				m.Answer = append(m.Answer, &dns.MX{
					Hdr:        dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeMX, Class: dns.ClassINET, Ttl: 300},
					Preference: 10,
					Mx:         "mail.example.com.",
				})
			}
			w.WriteMsg(m)
		}),
	}
	go srv.ActivateAndServe()
	<-started
	t.Cleanup(func() { srv.Shutdown() })
	return pc.LocalAddr().String()
}

func TestDNSMatchedA(t *testing.T) {
	setRoutes(t, map[string]string{"api.local": "http://127.0.0.1:9090"})
	setDNS(t, "10.0.0.5", "")

	reply := query(t, "API.local", dns.TypeA)
	if len(reply.Answer) != 1 {
		t.Fatalf("want 1 answer, got %d", len(reply.Answer))
	}
	a, ok := reply.Answer[0].(*dns.A)
	if !ok || !a.A.Equal(interfaceIP) {
		t.Fatalf("answer = %v, want an A record for %s", reply.Answer[0], interfaceIP)
	}
	if a.Hdr.Ttl != answerTTL {
		t.Errorf("TTL = %d, want %d", a.Hdr.Ttl, answerTTL)
	}
}

func TestDNSMatchedOtherTypesAreEmpty(t *testing.T) {
	setRoutes(t, map[string]string{"api.local": "http://127.0.0.1:9090"})
	setDNS(t, "10.0.0.5", "")

	for _, qtype := range []uint16{dns.TypeAAAA, dns.TypeHTTPS, dns.TypeMX} {
		reply := query(t, "api.local", qtype)
		if reply.Rcode != dns.RcodeSuccess || len(reply.Answer) != 0 {
			t.Errorf("%s: rcode %s with %d answers, want NOERROR with none",
				dns.TypeToString[qtype], dns.RcodeToString[reply.Rcode], len(reply.Answer))
		}
	}
}

func TestDNSSystemResolverSkipsNonAddressTypes(t *testing.T) {
	setRoutes(t, nil)
	setDNS(t, "10.0.0.5", "")

	reply := query(t, "example.com", dns.TypeMX)
	if reply.Rcode != dns.RcodeSuccess || len(reply.Answer) != 0 {
		t.Errorf("rcode %s with %d answers, want NOERROR with none",
			dns.RcodeToString[reply.Rcode], len(reply.Answer))
	}
}

func TestDNSForwardsToUpstream(t *testing.T) {
	setRoutes(t, map[string]string{"api.local": "http://127.0.0.1:9090"})
	setDNS(t, "10.0.0.5", startUpstream(t))

	reply := query(t, "example.com", dns.TypeMX)
	if len(reply.Answer) != 1 {
		t.Fatalf("want 1 answer, got %d", len(reply.Answer))
	}
	if mx, ok := reply.Answer[0].(*dns.MX); !ok || mx.Hdr.Ttl != 300 {
		t.Errorf("answer = %v, want the upstream's MX record with its 300 s TTL", reply.Answer[0])
	}

	if reply := query(t, "missing.example", dns.TypeA); reply.Rcode != dns.RcodeNameError {
		t.Errorf("rcode = %s, want the upstream's NXDOMAIN", dns.RcodeToString[reply.Rcode])
	}

	// Matched names are still answered locally, not forwarded.
	if reply := query(t, "api.local", dns.TypeA); len(reply.Answer) != 1 {
		t.Errorf("matched name: want 1 local answer, got %d", len(reply.Answer))
	}
}
