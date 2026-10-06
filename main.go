package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/miekg/dns"
)

var (
	// Global verbose flag, read by the DNS handler
	verboseMode bool

	// Set once shutdown begins, so serve loops don't treat the closed listener as a fatal error
	shuttingDown atomic.Bool
)

func main() {
	// 1. Parse Flags
	configPath := flag.String("config", "", "Path to config file")
	skipSSL := flag.Bool("skip-ssl-verify", true, "Skip TLS verification")
	port := flag.Int("port", 80, "Port for HTTP server")
	proxyURL := flag.String("proxy", "", "Optional outbound HTTP proxy URL")
	enableDNS := flag.Bool("dns", false, "Enable DNS server functionality")
	dnsAddr := flag.String("dns-addr", ":53", "Listen address for the DNS server (UDP and TCP)")
	upstream := flag.String("upstream", "", "Upstream DNS server for names not in the config, e.g. 1.1.1.1 (default: system resolver)")
	ifaceName := flag.String("interface", "", "Network interface name (required for DNS)")
	ifaceNameShort := flag.String("I", "", "Alias for -interface")
	enableHTTPS := flag.Bool("https", false, "Enable HTTPS SNI passthrough (routes TLS by SNI without decrypting it)")
	httpsAddr := flag.String("https-addr", ":443", "Listen address for the HTTPS SNI passthrough listener")
	verbose := flag.Bool("verbose", false, "Enable verbose logging for DNS misses")
	forceH2 := flag.Bool("http2", false, "Force enable HTTP/2 (may cause 'tls: user canceled' errors on some proxies)")
	disableKeepAlive := flag.Bool("no-keep-alive", false, "Disable HTTP connection reuse (fixes 'unsolicited response' in some proxies)")
	flag.Parse()

	// Set global verbose state
	verboseMode = *verbose

	// Handle interface alias
	finalIface := *ifaceName
	if finalIface == "" {
		finalIface = *ifaceNameShort
	}

	// 2. Config Loading / Generation
	targetConfig := *configPath
	if targetConfig == "" {
		if _, err := os.Stat("config.json"); err == nil {
			targetConfig = "config.json"
			log.Println("No config flag provided, using existing 'config.json'")
		} else {
			targetConfig = "config-example.json"
			createDummyConfig(targetConfig)
			log.Printf("Created example config file: %s\n", targetConfig)
		}
	}

	loadConfig(targetConfig)
	go watchConfig(targetConfig)

	// Shut down cleanly on Ctrl-C / SIGTERM
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 3. DNS Server Setup (Optional)
	var dnsServers []*dns.Server
	if *enableDNS {
		if finalIface == "" {
			log.Fatal("Error: -interface or -I is required when -dns is enabled")
		}
		if err := setupDNS(finalIface, *upstream); err != nil {
			log.Fatalf("Error: %v", err)
		}
		dnsServers = startDNSServer(*dnsAddr)
	}

	// 4. HTTPS SNI passthrough (Optional)
	var sniListener net.Listener
	if *enableHTTPS {
		sniListener = startSNIServer(*httpsAddr)
	}

	// 5. HTTP Redirector
	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: newProxyHandler(*skipSSL, *proxyURL, *forceH2, *disableKeepAlive),
	}
	go func() {
		log.Printf("HTTP Redirector listening on port %d...", *port)
		log.Printf("HTTP/2 Enabled: %v", *forceH2)
		log.Printf("Keep-Alives Enabled: %v", !*disableKeepAlive)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// 6. Wait for a signal, then stop everything
	<-ctx.Done()
	stop() // restore default handling so a second Ctrl-C force-quits
	shuttingDown.Store(true)
	log.Println("Shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP shutdown: %v", err)
	}
	for _, s := range dnsServers {
		_ = s.Shutdown()
	}
	if sniListener != nil {
		_ = sniListener.Close()
	}
	log.Println("Stopped.")
}
