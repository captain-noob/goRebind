# Development Guide

This guide is for working on goRebind itself. For what the tool does and how to use its flags, see [Readme.md](Readme.md).

## Prerequisites

- **Go 1.24+**. `go.mod` declares `go 1.24.2`.
- **Bash** for the cross-platform build script. Git Bash works on Windows.
- **curl**, and **nslookup** or **dig**, for manual testing.

## Project layout

| Path | Purpose |
| :--- | :--- |
| `main.go` | All of the code: flag parsing, config loading, the HTTP reverse proxy and the DNS server |
| `buid.sh` | Cross-compiles release binaries into `build/` |
| `go.mod`, `go.sum` | Module `goRebind`. The only third-party dependency is `github.com/miekg/dns` |
| `Readme.md` | User-facing docs |
| `.gitignore` | Ignores `config*` and `build` (see [Gotchas](#gotchas)) |

## Architecture

goRebind runs two servers that share one routing table:

```
client ──DNS query──▶ handleDNSRequest  (UDP :53, only with -dns)
                        ├─ name in routeMap and type A → answer with interfaceIP
                        └─ anything else              → systemDNSLookup (net.LookupIP)

client ──HTTP───────▶ handler (:port) → httputil.ReverseProxy
                        ├─ Director:     routeMap[Host] → set scheme, host and Host header; drop X-Forwarded-For
                        ├─ Transport:    TLS-verify toggle, HTTP/1.1 pinning, keep-alives, outbound proxy
                        └─ ErrorHandler: log, then 502
                                 │
                                 ▼
                       target (directly, or through -proxy, e.g. Burp)
```

The usual setup points a device's DNS at goRebind. The configured hostnames then resolve to this machine, the device sends its HTTP requests here, and the proxy forwards each one to the configured target.

### Startup sequence (`main`)

1. **Parse flags.** `-I` is an alias for `-interface`. If both are set, `-interface` wins.
2. **Choose a config file.** It uses `-config` if given. Otherwise it uses `./config.json` if that exists. Failing both, it writes `config-example.json` with two sample routes and uses that.
3. **Load routes (`loadConfig`).** The config is a JSON array of `{source, target}` objects, and each one goes into `routeMap` (key: lowercased source, value: parsed target URL). An invalid target URL is skipped with a warning. An unreadable file or invalid JSON is fatal.
4. **Start DNS (with `-dns` only).** `getInterfaceIP` finds the first non-loopback IPv4 address on the interface and stores it in `interfaceIP`. Then `startDNSServer` runs in a goroutine.
5. **Serve HTTP.** `startHTTPServer` builds the transport and proxy, then blocks in `ListenAndServe`.

### Shared state

| Global | Written | Read |
| :--- | :--- | :--- |
| `routeMap` | Once, by `loadConfig` at startup | On every HTTP request (`Director`) and every DNS query |
| `mu` (`sync.RWMutex`) | Guards `routeMap` | Not strictly needed yet, since nothing writes after startup, but it makes a future hot-reload cheap |
| `interfaceIP` | Once, at startup | DNS answers for matched names |
| `verboseMode` | Once, at startup | Logging of DNS queries that pass through to the system resolver |

### HTTP proxy details

- **Routing** looks up `req.Host`, lowercased, exactly as received. That includes any port, which causes [known issue 1](#known-issues).
- **The Host header is rewritten** to the target's host, so virtual-hosted targets and TLS SNI work.
- **`X-Forwarded-For` is set to `nil`.** In `ReverseProxy`, a nil value stops the proxy from appending the client IP, so the target never sees it.
- **HTTP/2 is off by default.** That needs two settings on the transport: `NextProtos: ["http/1.1"]` and an *empty, non-nil* `TLSNextProto` map. The empty map is what actually disables Go's built-in HTTP/2. With `-http2`, both stay nil and `ForceAttemptHTTP2` is set to true.
- **Outbound proxy:** by default it uses `http.ProxyFromEnvironment` (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`). `-proxy` overrides that.
- **Errors:** `ErrorHandler` logs everything except `context canceled` (the client went away) and returns an empty 502. The request it receives is the *outbound* one, so the host in the log is the target's, not the source's.

### DNS server details

- It uses `miekg/dns` on **UDP only**, on all interfaces, port 53. One handler is registered for `.`, which covers every name.
- Only the first question in each query is handled.
- **Configured names:** only A queries are answered, with `interfaceIP`. Records are built with `dns.NewRR` from a string with no TTL, so they get the library default of **3600 s**.
- **Everything else** goes to `net.LookupIP` on the host's own resolver, and the results become A or AAAA records. Other record types get an empty NOERROR reply.

## Building

```bash
go build -o goRebind .
```

On Windows, name the output `goRebind.exe`. To build all six release targets (linux, windows and darwin, each on amd64 and arm64):

```bash
./buid.sh
```

The binaries go to `build/goRebind-<os>-<arch>[.exe]`, which git ignores.

## Running locally

### Config

Create a `config.json` in the repo root (git ignores it) that points at something local:

```json
[
  { "source": "api.local", "target": "http://127.0.0.1:9090" }
]
```

Any local HTTP server can be the target, for example `python -m http.server 9090`.

### HTTP proxy

Use a high port so you don't need elevated rights, and set the `Host` header yourself:

```bash
go run . -config config.json -port 18080
```

```bash
curl -H "Host: api.local" http://127.0.0.1:18080/
```

Don't test with `curl http://api.local:18080/` or a browser. Both send `Host: api.local:18080`, which won't match a route until [known issue 1](#known-issues) is fixed.

How to read the log:

- `[HTTP-IN] GET api.local /` is logged for every request.
- A `[ERROR] Proxy Error for 127.0.0.1:9090: dial tcp ...` means the route **matched** but the target couldn't be reached.
- A `[ERROR] Proxy Error for <host>: unsupported protocol scheme ""` means **no route matched**.

### DNS server

The DNS server always binds UDP port 53, because there's no flag for the port yet:

```bash
go run . -config config.json -port 18080 -dns -I WiFi
```

To query it on Windows:

```powershell
nslookup api.local 127.0.0.1
```

On Linux or macOS:

```bash
dig @127.0.0.1 api.local A
```

The answer should be the interface's IP, and the server logs `[DNS] Match: api.local -> Returning Interface IP`. Add `-verbose` to also log queries that pass through to the system resolver.

On Windows, nslookup may also print `DNS request timed out`. nslookup sends an AAAA query too, and for a configured name that query falls through to a slow system lookup ([known issue 3](#known-issues)).

**Interface names:** Go uses the adapter's friendly name.

- Windows: `netsh interface show interface` or `Get-NetAdapter`
- Linux: `ip -br addr`
- macOS: `ifconfig`

`getInterfaceIP` takes the first non-loopback IPv4 address it finds. On a disconnected adapter, that can be a `169.254.x.x` link-local address, which other devices can't reach.

### Pointing a real device at it

1. Run `goRebind -port 80 -dns -I <iface>`.
2. Set the device's DNS server to this machine's IP on that interface.
3. Requests to configured hostnames now arrive on port 80 and are forwarded. To inspect them, chain through Burp with `-proxy http://127.0.0.1:8080`.
4. When you're done, remember that the device can keep cached answers for **up to an hour** (TTL 3600). Flush DNS on the device, or toggle its network off and on.

### Ports and permissions

- **Linux:** ports below 1024 need root or `sudo setcap 'cap_net_bind_service=+ep' ./goRebind`. If systemd-resolved is running, binding `:53` usually fails with `address already in use`, because systemd-resolved holds `127.0.0.53:53`.
- **Windows:** low ports don't need elevation, but something may already be using them. IIS or HTTP.sys can hold 80, and Internet Connection Sharing can hold 53. The first run shows a Windows Firewall prompt. Allow private networks if other devices need to reach the proxy.

## Before committing

```bash
gofmt -l .
go vet ./...
go mod tidy
go test ./...
```

What to expect today:

- `gofmt -l .` lists `main.go` because of its line endings, not its formatting (see [Gotchas](#gotchas)).
- `go mod tidy` moves `miekg/dns` from an indirect to a direct requirement.
- `go test` reports `no test files`.

## Testing

There are no tests yet. Here's where to start:

- **DNS:** `handleDNSRequest` takes a `dns.ResponseWriter`. Pass in a fake that captures the reply, set the globals, and check the answer:

  ```go
  // fakeDNSWriter captures the reply; the embedded interface is nil because
  // handleDNSRequest only calls WriteMsg.
  type fakeDNSWriter struct {
  	dns.ResponseWriter
  	msg *dns.Msg
  }

  func (f *fakeDNSWriter) WriteMsg(m *dns.Msg) error { f.msg = m; return nil }

  func TestDNSMatchReturnsInterfaceIP(t *testing.T) {
  	target, _ := url.Parse("http://127.0.0.1:9090")
  	routeMap = map[string]*url.URL{"api.local": target}
  	interfaceIP = net.ParseIP("10.0.0.5").To4()

  	req := new(dns.Msg)
  	req.SetQuestion("api.local.", dns.TypeA)
  	w := &fakeDNSWriter{}
  	handleDNSRequest(w, req)

  	if len(w.msg.Answer) != 1 {
  		t.Fatalf("want 1 answer, got %d", len(w.msg.Answer))
  	}
  	if a := w.msg.Answer[0].(*dns.A); !a.A.Equal(interfaceIP) {
  		t.Errorf("want %s, got %s", interfaceIP, a.A)
  	}
  }
  ```

- **HTTP routing:** `startHTTPServer` builds the proxy and serves it in the same function, so tests can't get at the handler without binding a port. First pull the construction out into something like `newHandler(...) http.Handler`. Then use an `httptest.Server` as the target, send requests through `httptest.NewRecorder` with different `Host` headers, and check which ones reach it.

Tests that set `routeMap` or `interfaceIP` change package globals, so don't mark them `t.Parallel()`.

## Code conventions

- **One `package main`.** Prefer the standard library. `miekg/dns` is the only dependency, so keep it that way unless there's a clear reason.
- **Log format:** request-time log lines start with a bracketed tag (`[HTTP-IN]`, `[DNS]`, `[ERROR]`). Startup messages have no tag.
- **Failures:** misconfiguration at startup is fatal (`log.Fatal`). Never exit while handling a request.
- **Adding a flag:** declare it in the flag block in `main`, pass it into `startHTTPServer` or the DNS setup instead of reading a global, and add it to the flag table in `Readme.md`.

## Gotchas

- **`.gitignore`'s `config*` also matches Go source files.** A new `config.go` would be silently left out of commits (`git check-ignore config.go` confirms it). Change the pattern to `config*.json` before splitting the code into several files.
- **`main.go` and `Readme.md` are committed with CRLF line endings**, which is why `gofmt -l` flags `main.go`. To fix it once, add a `.gitattributes` containing `*.go text eol=lf`, then run `git add --renormalize .` and `gofmt -w .`.
- **TLS verification of targets is off by default** (`-skip-ssl-verify=true`).
- **A bare run doesn't stop when there's no config.** It writes `config-example.json`, uses it, and so proxies `example.local` to `https://www.google.com`.

## Known issues

These have been confirmed against the current code. Remove each one when it's fixed.

1. **A `Host` header that includes a port never matches a route.** On any port other than 80, browsers and curl send `api.local:18080`, the lookup fails, and the request gets a 502.
2. **The target URL's path is dropped.** `Director` copies only the scheme and host, so `https://host/api` loses `/api`.
3. **AAAA queries for configured names fall through to a system lookup.** IPv6-capable clients can get the real address and go around the proxy, and the lookup is slow.
4. **DNS answers have a 3600 s TTL**, so devices keep sending traffic here for up to an hour after goRebind stops.
5. **An unknown host gets a 502 and a misleading `unsupported protocol scheme` log**, instead of a 404.
6. **Pass-through DNS is limited.** It answers only A and AAAA, returns NOERROR instead of NXDOMAIN for names that don't exist, and doesn't listen on TCP.
7. **Only a plain HTTP listener exists**, so clients that connect with HTTPS on port 443 can't be served.
8. **`Location` and `Set-Cookie` domains aren't rewritten**, so redirects can take a client off the proxy when the source host differs from the target.
9. **`loggingResponseWriter` captures the status code but never logs it.**
10. **Docs and module metadata are out of date.** `go.mod` marks `miekg/dns` as `// indirect`, and `Readme.md` says Go 1.18+ while `go.mod` requires 1.24.2.

## Roadmap

In order of priority:

1. **Routing fixes with tests.**
   - Replace `Director` with `Rewrite` and `pr.SetURL(target)`. That joins the target path, sets Host, and drops `X-Forwarded-*` by default.
   - Strip the port from `Host` with `net.SplitHostPort`.
   - Return a 404 for unknown hosts before the request reaches the proxy.
   - This fixes issues 1, 2 and 5.
2. **DNS correctness.**
   - Build records as structs (`&dns.A{...}`) with a short TTL.
   - Return an empty answer for AAAA queries on configured names.
   - Forward everything else to an upstream server (`-upstream 1.1.1.1:53`, via `dns.Client.Exchange`).
   - Listen on TCP too, and bind to `interfaceIP:53` instead of `:53`.
   - This fixes issues 3, 4 and 6.
3. **HTTPS listener.** The first step is SNI passthrough on port 443, which routes without decrypting. Full interception would then mint certificates from a local root CA.
4. **Quality of life:**
   - Wildcard sources (`*.example.com`).
   - A repeatable `-route src=target` flag.
   - Config hot-reload.
   - Auto-detecting the outbound IP when `-I` is omitted.
   - Logging status and latency.
   - A `-dump` flag that prints full requests and responses.
5. **Hygiene:**
   - Split `main.go` into `config.go`, `proxy.go` and `dns.go`, after fixing `.gitignore`.
   - Shut down cleanly with `signal.NotifyContext`.
   - Use `errors.Is(err, context.Canceled)`.
   - Rename `buid.sh`.
   - Build with `CGO_ENABLED=0 -ldflags "-s -w"`.
   - Add a release workflow.
