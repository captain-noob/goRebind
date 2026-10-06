package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ConfigRoute represents a single mapping rule
type ConfigRoute struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

var (
	// Global map for O(1) lookups during high traffic
	routeMap = make(map[string]*url.URL)
	mu       sync.RWMutex
)

func createDummyConfig(filename string) {
	dummy := []ConfigRoute{
		{Source: "example.local", Target: "https://www.google.com"},
		{Source: "api.local", Target: "http://127.0.0.1:8080"},
	}
	file, _ := json.MarshalIndent(dummy, "", "  ")
	_ = os.WriteFile(filename, file, 0644)
}

// parseConfig reads and validates a config file into a fresh route map. It never mutates globals
// or exits, so it is safe to call both at startup and on a hot reload.
func parseConfig(path string) (map[string]*url.URL, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var routes []ConfigRoute
	if err := json.Unmarshal(data, &routes); err != nil {
		return nil, fmt.Errorf("invalid JSON config: %w", err)
	}

	m := make(map[string]*url.URL)
	for _, r := range routes {
		src := normalizeHost(r.Source)
		if src == "" {
			log.Printf("Warning: Skipping route with empty source (target %s)", r.Target)
			continue
		}
		targetURL, err := url.Parse(r.Target)
		if err != nil {
			log.Printf("Warning: Skipping invalid target URL %s: %v", r.Target, err)
			continue
		}
		if targetURL.Scheme == "" || targetURL.Host == "" {
			log.Printf("Warning: Skipping target %s: must be an absolute URL like https://host", r.Target)
			continue
		}
		if _, dup := m[src]; dup {
			log.Printf("Warning: Duplicate source %s; later target %s overrides the earlier one", r.Source, r.Target)
		}
		m[src] = targetURL
		log.Printf("Loaded Route: %s -> %s", r.Source, r.Target)
	}
	return m, nil
}

// loadConfig performs the initial load. A bad config here is fatal, because there is nothing to
// fall back to.
func loadConfig(path string) {
	routes, err := parseConfig(path)
	if err != nil {
		log.Fatalf("Failed to read config: %v", err)
	}
	mu.Lock()
	routeMap = routes
	mu.Unlock()
}

// watchConfig polls the config file's modification time and swaps in new routes when it changes.
// A reload that fails to parse is logged and the current routes are kept.
func watchConfig(path string) {
	var lastMod time.Time
	if info, err := os.Stat(path); err == nil {
		lastMod = info.ModTime()
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		info, err := os.Stat(path)
		if err != nil || info.ModTime().Equal(lastMod) {
			continue
		}
		lastMod = info.ModTime()

		routes, err := parseConfig(path)
		if err != nil {
			log.Printf("[CONFIG] Reload failed, keeping current routes: %v", err)
			continue
		}
		mu.Lock()
		routeMap = routes
		mu.Unlock()
		log.Printf("[CONFIG] Reloaded %d route(s) from %s", len(routes), path)
	}
}

// normalizeHost lowercases a host and strips any port and trailing dot, so "API.local:8080"
// and "api.local." both match the route for "api.local".
func normalizeHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		// A bracketed IPv6 literal without a port, e.g. "[::1]"; strip the brackets so it matches
		// the same address seen with a port ("[::1]:80" -> "::1").
		host = host[1 : len(host)-1]
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

func lookupRoute(host string) (*url.URL, bool) {
	mu.RLock()
	defer mu.RUnlock()
	target, ok := routeMap[normalizeHost(host)]
	return target, ok
}
