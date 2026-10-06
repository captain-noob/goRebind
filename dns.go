package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"strings"

	"github.com/miekg/dns"
)

// answerTTL is the TTL (in seconds) of DNS answers goRebind makes itself. It is kept short so
// clients stop sending traffic here soon after goRebind exits.
const answerTTL = 30

var (
	// Interface IP for DNS responses
	interfaceIP net.IP

	// Upstream DNS server (host:port) for unmatched queries; empty means the system resolver
	upstreamDNS string
)

func getInterfaceIP(name string) (net.IP, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}

	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.To4(), nil
			}
		}
	}
	return nil, fmt.Errorf("no IPv4 address found on interface %s", name)
}

// setupDNS resolves the interface IP and normalizes the upstream address into the package globals.
func setupDNS(iface, upstream string) error {
	ip, err := getInterfaceIP(iface)
	if err != nil {
		return fmt.Errorf("getting IP for interface %s: %w", iface, err)
	}
	interfaceIP = ip
	log.Printf("DNS Server enabled. Responding with IP %s for matched hosts.", interfaceIP.String())

	if upstream != "" {
		if _, _, err := net.SplitHostPort(upstream); err != nil {
			upstream = net.JoinHostPort(upstream, "53")
		}
		upstreamDNS = upstream
		log.Printf("Forwarding other DNS queries to %s", upstreamDNS)
	} else {
		log.Println("Resolving other DNS queries with the system resolver (A/AAAA only)")
	}
	return nil
}

// startDNSServer starts a UDP and a TCP DNS server on addr and returns them for shutdown.
func startDNSServer(addr string) []*dns.Server {
	dns.HandleFunc(".", handleDNSRequest)

	var servers []*dns.Server
	for _, network := range []string{"udp", "tcp"} {
		server := &dns.Server{Addr: addr, Net: network}
		servers = append(servers, server)
		go func(s *dns.Server, netw string) {
			log.Printf("DNS Server listening on %s %s...", strings.ToUpper(netw), addr)
			if err := s.ListenAndServe(); err != nil && !shuttingDown.Load() {
				log.Fatalf("Failed to start DNS server (%s): %v", netw, err)
			}
		}(server, network)
	}
	return servers
}

func handleDNSRequest(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Compress = false

	if r.Opcode == dns.OpcodeQuery && len(r.Question) > 0 {
		q := r.Question[0]
		name := normalizeHost(q.Name)
		qtype := dns.TypeToString[q.Qtype]

		if _, exists := lookupRoute(name); exists {
			// A matched name only ever resolves to us: other types (notably AAAA) get an empty
			// answer so IPv6-capable clients can't go around the proxy.
			if q.Qtype == dns.TypeA {
				log.Printf("[DNS] Match: %s -> Returning Interface IP", name)
				m.Answer = append(m.Answer, &dns.A{
					Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: answerTTL},
					A:   interfaceIP,
				})
			} else if verboseMode {
				log.Printf("[DNS] Match: %s %s -> Empty Answer", name, qtype)
			}
		} else if upstreamDNS != "" {
			if verboseMode {
				log.Printf("[DNS] No Match: %s %s -> Upstream %s", name, qtype, upstreamDNS)
			}
			m = forwardDNS(w, r)
		} else {
			if verboseMode {
				log.Printf("[DNS] No Match: %s %s -> System Lookup", name, qtype)
			}
			systemDNSLookup(q, m)
		}
	}

	w.WriteMsg(m)
}

// forwardDNS relays a query to upstreamDNS over the same protocol the client used, so the
// upstream's answer (record types, TTLs, NXDOMAIN, truncation) reaches the client unchanged.
func forwardDNS(w dns.ResponseWriter, r *dns.Msg) *dns.Msg {
	c := &dns.Client{Net: "udp"}
	if _, ok := w.RemoteAddr().(*net.TCPAddr); ok {
		c.Net = "tcp"
	}

	resp, _, err := c.Exchange(r, upstreamDNS)
	if err != nil {
		log.Printf("[ERROR] DNS upstream %s: %v", upstreamDNS, err)
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeServerFailure)
		return m
	}
	resp.Compress = true
	return resp
}

// systemDNSLookup answers A and AAAA queries from the host's own resolver. Other types get an
// empty answer, because the system resolver can only return addresses.
func systemDNSLookup(q dns.Question, m *dns.Msg) {
	if q.Qtype != dns.TypeA && q.Qtype != dns.TypeAAAA {
		return
	}

	// Look up both families and filter, so a name with only A records gets an empty AAAA answer
	// rather than NXDOMAIN, which would tell clients the name doesn't exist at all.
	ips, err := net.LookupIP(strings.TrimSuffix(q.Name, "."))
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			m.Rcode = dns.RcodeNameError
		} else {
			m.Rcode = dns.RcodeServerFailure
		}
		return
	}

	hdr := dns.RR_Header{Name: q.Name, Rrtype: q.Qtype, Class: dns.ClassINET, Ttl: answerTTL}
	for _, ip := range ips {
		if q.Qtype == dns.TypeA && ip.To4() != nil {
			m.Answer = append(m.Answer, &dns.A{Hdr: hdr, A: ip.To4()})
		} else if q.Qtype == dns.TypeAAAA && ip.To4() == nil {
			m.Answer = append(m.Answer, &dns.AAAA{Hdr: hdr, AAAA: ip})
		}
	}
}
