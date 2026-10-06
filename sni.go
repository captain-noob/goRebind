package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/url"
	"time"
)

// startSNIServer listens for TLS connections and tunnels each to the backend chosen by its SNI
// server name. It never decrypts: only the plaintext ClientHello is inspected, to read the SNI.
func startSNIServer(addr string) net.Listener {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to start HTTPS SNI listener: %v", err)
	}
	log.Printf("HTTPS SNI passthrough listening on %s...", addr)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				if shuttingDown.Load() || errors.Is(err, net.ErrClosed) {
					return
				}
				log.Printf("[ERROR] SNI accept: %v", err)
				return
			}
			go handleSNIConn(conn)
		}
	}()
	return ln
}

func handleSNIConn(client net.Conn) {
	defer client.Close()

	// Bound the time spent reading the ClientHello, then clear the deadline for the tunnel.
	if err := client.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return
	}
	hello, clientReader, err := peekClientHello(client)
	if err != nil {
		log.Printf("[SNI] ClientHello peek failed from %s: %v", client.RemoteAddr(), err)
		return
	}
	if err := client.SetReadDeadline(time.Time{}); err != nil {
		return
	}

	if hello.ServerName == "" {
		log.Printf("[SNI] connection from %s has no SNI, dropping", client.RemoteAddr())
		return
	}
	target, ok := lookupRoute(hello.ServerName)
	if !ok {
		log.Printf("[SNI] no route for %s, dropping", hello.ServerName)
		return
	}

	addr := sniTargetAddr(target)
	log.Printf("[SNI] %s -> %s", hello.ServerName, addr)

	backend, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		log.Printf("[ERROR] SNI dial %s: %v", addr, err)
		return
	}
	defer backend.Close()

	// Tunnel both directions. When either side finishes, the deferred Close on both conns
	// unblocks the other copy.
	done := make(chan struct{}, 2)
	go func() { io.Copy(backend, clientReader); done <- struct{}{} }()
	go func() { io.Copy(client, backend); done <- struct{}{} }()
	<-done
}

// sniTargetAddr is the host:port to tunnel a TLS connection to. TLS defaults to port 443 when the
// target URL doesn't specify one.
func sniTargetAddr(target *url.URL) string {
	port := target.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(target.Hostname(), port)
}

// peekClientHello reads the TLS ClientHello to extract the SNI, then returns a reader that
// replays the exact bytes read so the connection can be tunneled unchanged. Only the plaintext
// ClientHello is read here; application data is never decrypted.
func peekClientHello(reader io.Reader) (*tls.ClientHelloInfo, io.Reader, error) {
	peeked := new(bytes.Buffer)
	hello, err := readClientHello(io.TeeReader(reader, peeked))
	if hello == nil {
		return nil, nil, err
	}
	return hello, io.MultiReader(peeked, reader), nil
}

func readClientHello(reader io.Reader) (*tls.ClientHelloInfo, error) {
	var hello *tls.ClientHelloInfo
	err := tls.Server(readOnlyConn{reader: reader}, &tls.Config{
		GetConfigForClient: func(argHello *tls.ClientHelloInfo) (*tls.Config, error) {
			hello = new(tls.ClientHelloInfo)
			*hello = *argHello
			return nil, nil
		},
	}).Handshake()
	// The handshake always fails (no certificate is configured), but by then GetConfigForClient
	// has captured the SNI, which is all that is needed.
	if hello == nil {
		return nil, err
	}
	return hello, nil
}

// readOnlyConn adapts an io.Reader to net.Conn for tls.Server, exposing only reads. Writes fail,
// so the aborted handshake can't send an alert, and no addresses or deadlines are needed.
type readOnlyConn struct {
	reader io.Reader
}

func (c readOnlyConn) Read(p []byte) (int, error)         { return c.reader.Read(p) }
func (c readOnlyConn) Write(p []byte) (int, error)        { return 0, io.ErrClosedPipe }
func (c readOnlyConn) Close() error                       { return nil }
func (c readOnlyConn) LocalAddr() net.Addr                { return nil }
func (c readOnlyConn) RemoteAddr() net.Addr               { return nil }
func (c readOnlyConn) SetDeadline(t time.Time) error      { return nil }
func (c readOnlyConn) SetReadDeadline(t time.Time) error  { return nil }
func (c readOnlyConn) SetWriteDeadline(t time.Time) error { return nil }
