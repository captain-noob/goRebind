# Development Guide

This guide is for working on goRebind itself. For what the tool does and how to use its flags, see [Readme.md](Readme.md).

## Prerequisites

- **Go 1.24+**. `go.mod` declares `go 1.24.2`.
- **Bash** for the cross-platform build script. Git Bash works on Windows.
- **curl**, and **nslookup** or **dig**, for manual testing.
- A **C compiler** (gcc/clang) only if you want to run the tests under `-race`. Everything else works without one.

## Project layout

| Path | Purpose |
| :--- | :--- |
| `main.go` | Flag parsing, startup orchestration, graceful shutdown. Holds the `verboseMode` and `shuttingDown` globals |
| `config.go` | Route config: loading, validation, hot-reload, and the `routeMap` / `lookupRoute` helpers |
| `proxy.go` | The HTTP reverse proxy: request rewriting, redirect/cookie rewriting, logging |
| `dns.go` | The DNS server: interface IP, matched-name answers, upstream forwarding, system lookups |
| `sni.go` | The HTTPS SNI passthrough listener |
| `main_test.go` | Tests for all of the above |
| `build.sh` | Cross-compiles release binaries into `build/` |
| `go.mod`, `go.sum` | Module `goRebind`. The only third-party dependency is `github.com/miekg/dns` |
| `Readme.md` | User-facing docs |
| `.gitattributes` | Keeps `*.go` and `*.sh` files at LF line endings (needed by `gofmt` and bash) |
| `.gitignore` | Ignores local configs (`config*.json`) and `build` |

Everything is `package main`; the file split is organizational, not an import boundary.

## Architecture

goRebind runs up to three listeners that share one routing table (`routeMap`, guarded by `mu`):

```
                         ┌─ config.go: watchConfig polls the file's mtime (2s) and swaps routeMap
                         │
client ──DNS query──▶ handleDNSRequest  (UDP + TCP on -dns-addr, only with -dns)
                        ├─ name in routeMap, type A → answer with interfaceIP (TTL 30 s)
                        ├─ name in routeMap, other  → empty answer (stops IPv6 bypass)
                        └─ name not in routeMap     → forwardDNS (with -upstream)
                                                       or systemDNSLookup (A/AAAA only)

client ──HTTP───────▶ handler (:port)
                        ├─ lookupRoute(Host) misses → 404
                        └─ hit → httputil.ReverseProxy
                                  ├─ Rewrite:        SetURL(target), keep client X-Forwarded-*, drop X-Forwarded-For
                                  ├─ ModifyResponse: rewrite Location + Set-Cookie back to the source host
                                  └─ ErrorHandler:   log, then 502

client ──TLS────────▶ SNI listener (-https-addr, only with -https)
                        ├─ peek ClientHello → SNI → lookupRoute(SNI)
                        └─ hit → raw TCP tunnel to the target (never decrypted)
```

The usual setup points a device's DNS at goRebind. The configured hostnames then resolve to this machine, the device sends its traffic here, and goRebind forwards each connection to the configured target.

### Startup sequence (`main`)

1. **Parse flags.** `-I` is an alias for `-interface`. If both are set, `-interface` wins.
2. **Choose a config file.** It uses `-config` if given. Otherwise it uses `./config.json` if that exists. Failing both, it writes `config-example.json` with two sample routes and uses that.
3. **Load routes (`loadConfig`)** and start the hot-reload watcher (`go watchConfig`).
4. **Install the signal context.** `signal.NotifyContext` makes a context that's cancelled on Ctrl-C or SIGTERM.
5. **Start DNS (with `-dns` only).** `setupDNS` stores the interface IP and normalizes the upstream address; `startDNSServer` starts a UDP and a TCP server, each in its own goroutine, and returns them for shutdown.
6. **Start the SNI listener (with `-https` only).** `startSNIServer` returns the `net.Listener` for shutdown.
7. **Start the HTTP server** in a goroutine.
8. **Block until the signal fires**, then shut everything down (see below).

### Config (`config.go`)

- `parseConfig` reads and validates a file into a **new** map without touching globals or exiting, so it's reused by both the initial load and hot-reload.
  - Each source is normalized with `normalizeHost`: lowercased, with any port and trailing dot removed.
  - A target that isn't an absolute URL (scheme plus host) is skipped with a warning.
- `loadConfig` is the initial load. A read or parse error here is **fatal** — there's nothing to fall back to.
- `watchConfig` polls the file's mtime every 2 s. On a change it re-parses; on success it swaps `routeMap` under the write lock, and on a parse error it logs and keeps the current routes. The watcher never exits the program.
- `lookupRoute` is the only way routes should be read. It normalizes the host and takes the read lock.

### HTTP proxy (`proxy.go`)

- **Routing:** the handler calls `lookupRoute(r.Host)`. Because the host is normalized the same way config sources are, `API.local:8080` and `api.local.` both match `api.local`. A miss returns `404 goRebind: no route for host …`, and the request never reaches the proxy.
- **Passing the route:** the matched `proxyRoute{source, target}` goes to `Rewrite` and `ModifyResponse` through the request context (`routeKey`). `source` is the inbound Host as the client sent it; `target` is the route's URL.
- **Rewrite:**
  - `pr.SetURL(target)` sets the scheme and host and joins the target's base path with the request path (`http://host/base` + `/x?q=1` → `/base/x?q=1`). It also sends the target's host as `Host`.
  - Rewrite mode strips client forwarding headers. The handler copies `Forwarded`, `X-Forwarded-Host` and `X-Forwarded-Proto` back; `X-Forwarded-For` stays dropped, so the target never sees the client IP.
- **ModifyResponse (`rewriteRedirectAndCookies`):** when the source host differs from the target (e.g. `example.local` → `google.com`), a bare redirect or cookie would send the client straight to the target and off the proxy. So:
  - A `Location` whose host equals the target host is rewritten to `http://<source>/…`, keeping the client on our HTTP listener.
  - Any `Set-Cookie` `Domain` attribute is forced to the source host. Other attributes (`Path`, `Secure`, `HttpOnly`, …) are left alone. Note `Secure` cookies won't be stored by a client talking to us over plain HTTP — strip it in `rewriteCookieDomain` if that becomes a problem.
- **HTTP/2 is off by default.** That needs two settings on the transport: `NextProtos: ["http/1.1"]` and an *empty, non-nil* `TLSNextProto` map. The empty map is what actually disables Go's built-in HTTP/2. With `-http2`, both stay nil and `ForceAttemptHTTP2` is set to true.
- **`loggingResponseWriter`** records the status for the `[HTTP-OUT]` line. Its `Unwrap` method is required — without it, `http.ResponseController` can't reach the real writer and WebSocket upgrades fail. On an upgrade, ReverseProxy hijacks the connection and never calls `WriteHeader`, so a status of 0 is logged as 101 when the tunnel closes.

### DNS server (`dns.go`)

- One handler is registered for `.` (every name). Only the first question in each query is handled.
- **Configured names:** an `A` query is answered with `interfaceIP` (TTL `answerTTL`, 30 s). Every other type, AAAA included, gets an empty NOERROR reply, so IPv6-capable clients can't get the real address and go around the proxy.
- **Other names, with `-upstream`:** `forwardDNS` relays the query over the protocol the client used and returns the reply unchanged — record types, TTLs, NXDOMAIN and truncation all pass through. An unreachable upstream yields SERVFAIL.
- **Other names, without `-upstream`:** `systemDNSLookup` uses `net.LookupIP`, answers only A/AAAA, returns NXDOMAIN for missing names and SERVFAIL for other errors.

### HTTPS SNI passthrough (`sni.go`)

- `startSNIServer` accepts TCP connections and hands each to `handleSNIConn`.
- `peekClientHello` reads the TLS **ClientHello** (which is plaintext) to extract the SNI, using a throwaway `tls.Server` whose `GetConfigForClient` captures the name and then fails the handshake. An `io.TeeReader` records the exact bytes read, and `peekClientHello` returns an `io.MultiReader` that replays them — so the real backend receives a byte-identical ClientHello. **Application data is never read or decrypted here.**
- The SNI name is routed with `lookupRoute`. On a hit, `handleSNIConn` dials the target's host and port (`sniTargetAddr`, default 443) and copies bytes both ways until either side closes.
- **Limitation:** the ClientHello is forwarded unchanged, so the backend sees the *source* SNI. That's perfect for the DNS-rebind flow (the source name *is* the real name), but a backend virtual-hosted under a different name may reject it. Passthrough can't rewrite SNI without decrypting.

### Graceful shutdown

`main` blocks on the signal context. When it fires it calls `stop()` (so a second Ctrl-C force-quits), sets `shuttingDown`, then `httpServer.Shutdown` with a 5 s deadline, `Shutdown()` on each DNS server, and `Close()` on the SNI listener. `shuttingDown` tells the DNS and SNI serve loops that a listener error is expected, not fatal.

> On Windows only `os.Interrupt` (Ctrl-C) is actually delivered; `SIGTERM` is accepted by the code but never raised. The shutdown path is straightforward Go and isn't unit-tested — the servers' individual `Shutdown`/`Close` calls are exercised by the tests, but `main`'s orchestration isn't.

### Shared state

| Global | Where | Written | Read |
| :--- | :--- | :--- | :--- |
| `routeMap`, `mu` | config.go | `loadConfig`, and `watchConfig` on every reload | `lookupRoute`, on every request and query |
| `interfaceIP` | dns.go | `setupDNS` at startup | DNS answers for matched names |
| `upstreamDNS` | dns.go | `setupDNS` at startup | `handleDNSRequest`, `forwardDNS`. Empty means system resolver |
| `verboseMode` | main.go | Once, at startup | DNS logging |
| `shuttingDown` | main.go | Once, when shutdown begins | DNS and SNI serve loops |

`mu` is a real requirement now: `watchConfig` writes `routeMap` while requests read it.

## Building

```bash
go build -o goRebind .
```

On Windows, name the output `goRebind.exe`. To build all six release targets (linux, windows and darwin, each on amd64 and arm64):

```bash
./build.sh
```

The binaries go to `build/goRebind-<os>-<arch>[.exe]`, which git ignores.

## Releasing

Releases are cut by `.github/workflows/release.yml`. Push a `v*` tag (or run the workflow manually from the Actions tab and type the tag):

```bash
git tag v1.0.0 && git push origin v1.0.0
```

The workflow vets and tests, cross-compiles all six targets with `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.version=<tag>"`, packages each as a `.tar.gz` (`.zip` for Windows) containing the binary and `Readme.md`, writes `SHA256SUMS.txt`, and publishes a GitHub Release with auto-generated notes. The injected version is what `goRebind -version` prints (`dev` for local builds).

## Running locally

### Config

Create a `config.json` in the repo root (git ignores it) that points at something local:

```json
[
  { "source": "api.local", "target": "http://127.0.0.1:9090" }
]
```

Any local HTTP server can be the target, for example `python -m http.server 9090`. Editing this file while goRebind runs triggers a hot reload within ~2 s; watch for a `[CONFIG] Reloaded N route(s)` log line.

### HTTP proxy

Use a high port so you don't need elevated rights:

```bash
go run . -config config.json -port 18080
```

```bash
curl -H "Host: api.local" http://127.0.0.1:18080/
```

`curl -H "Host: api.local:18080" …` works too, which is what a browser would send. Each request produces an `[HTTP-IN]` and an `[HTTP-OUT]` line; `-> 404` means no route matched, and `-> 502` comes with an `[ERROR] Proxy Error …` line saying why the target couldn't be reached.

### HTTPS SNI passthrough

```bash
go run . -config config.json -port 18080 -https -https-addr 127.0.0.1:18443
```

Point a route's source at an HTTPS target, then connect with TLS using that source as the SNI:

```bash
curl -sk --resolve api.local:18443:127.0.0.1 https://api.local:18443/
```

(`--resolve` makes curl send `api.local` as the SNI while connecting to the local listener.) The target must speak TLS, and must accept the source name as its SNI — see the limitation under [Architecture](#https-sni-passthrough-snigo).

### DNS server

On Linux or macOS, put DNS on a high port with `-dns-addr` and query it with `dig`:

```bash
go run . -config config.json -port 18080 -dns -dns-addr 127.0.0.1:15353 -I en0
```

```bash
dig @127.0.0.1 -p 15353 api.local A
```

Windows `nslookup` can't query other ports: its `-port=` option didn't reach the server when tested. On Windows, keep DNS on port 53, which doesn't need elevation there:

```bash
go run . -config config.json -port 18080 -dns -dns-addr 127.0.0.1:53 -I WiFi
```

```powershell
nslookup api.local 127.0.0.1
```

The answer should be the interface's IP, and the server logs `[DNS] Match: api.local -> Returning Interface IP`. Add `-verbose` to also log the queries that aren't answered with the interface IP.

Don't use port 5353 on Windows. The OS's mDNS service holds it, and binding fails with `forbidden by its access permissions`.

**Interface names:** Go uses the adapter's friendly name.

- Windows: `netsh interface show interface` or `Get-NetAdapter`
- Linux: `ip -br addr`
- macOS: `ifconfig`

`getInterfaceIP` takes the first non-loopback IPv4 address it finds. On a disconnected adapter, that can be a `169.254.x.x` link-local address, which other devices can't reach.

### Pointing a real device at it

1. Run `goRebind -port 80 -dns -I <iface> -upstream <your usual DNS server>`, adding `-https` if the device uses HTTPS.
2. Set the device's DNS server to this machine's IP on that interface.
3. Requests to configured hostnames arrive here and are forwarded. To inspect the HTTP ones, chain through Burp with `-proxy http://127.0.0.1:8080`. (SNI passthrough is opaque — Burp can't see inside it.)
4. After goRebind stops, the device can keep cached DNS answers for up to 30 s.

### Ports and permissions

- **Linux:** ports below 1024 need root or `sudo setcap 'cap_net_bind_service=+ep' ./goRebind`. If systemd-resolved is running, binding `:53` usually fails with `address already in use`, because it holds `127.0.0.53:53`. Bind to the interface IP instead, e.g. `-dns-addr 192.168.1.5:53`.
- **Windows:** low ports don't need elevation, but something may already be using them. IIS or HTTP.sys can hold 80, and Internet Connection Sharing can hold 53. The first run shows a Windows Firewall prompt; allow private networks if other devices need to reach goRebind.

## Before committing

```bash
gofmt -l .
go vet ./...
go mod tidy
go test ./...
```

All four should be clean: `gofmt` and `go mod tidy` should change nothing, and every test should pass.

## Testing

All tests are in `main_test.go` and run fully on loopback, with no outside network access. They replace package globals, so **don't add `t.Parallel()`**. Use the helpers, which restore the globals when the test ends:

| Helper | Use |
| :--- | :--- |
| `setRoutes(t, map[string]string{...})` | Replace the routing table. Keys must already be normalized |
| `setDNS(t, ip, upstream)` | Set `interfaceIP` and `upstreamDNS` |
| `startProxy(t)` / `get(t, …)` | Serve `newProxyHandler` on a loopback port and send requests with any `Host` header |
| `query(t, name, qtype)` | Run one question through `handleDNSRequest` with a fake writer and return the reply |
| `startUpstream(t)` | A fake upstream DNS server (`missing.example.` → NXDOMAIN, everything else → one MX record, TTL 300) |

Notable tests: `TestProxyRouting` (port/dot normalization, 404 on miss), `TestProxyRewritesRequest` (path join, forwarding headers), `TestProxyRewritesLocationAndCookies`, `TestProxyUpgrade` (WebSocket), `TestSNIPassthrough` (end-to-end TLS tunnel), and the `TestDNS*` set.

- The backend handler in a proxy/SNI test runs in another goroutine — pass what you capture over a channel or an atomic, not a plain variable.
- Network tests should carry their own deadline (see the `tls.Dialer` in `TestSNIPassthrough`) so a regression fails fast instead of hanging.
- To run under the race detector you need a C compiler: `CGO_ENABLED=1 go test -race ./...`.

## Code conventions

- **One `package main`.** Prefer the standard library. `miekg/dns` is the only dependency; keep it that way unless there's a clear reason.
- **Route lookups** always go through `lookupRoute`. Never index `routeMap` directly.
- **Log format:** request-time lines start with a bracketed tag (`[HTTP-IN]`, `[HTTP-OUT]`, `[DNS]`, `[SNI]`, `[CONFIG]`, `[ERROR]`). Startup messages have no tag.
- **Failures:** misconfiguration at startup is fatal (`log.Fatal`). Never exit while handling a request or a reload.
- **Adding a flag:** declare it in the flag block in `main`, pass it into the relevant constructor instead of reading a global where possible, and add it to the flag table in `Readme.md`.

## Gotchas

- **TLS verification of targets is off by default** (`-skip-ssl-verify=true`).
- **A bare run doesn't stop when there's no config.** It writes `config-example.json`, uses it, and so proxies `example.local` to `https://www.google.com`.
- **Without `-upstream`, other names go through the host's own resolver.** If this machine uses goRebind as its own DNS server, those lookups loop back in. Names the OS resolves by multicast (e.g. `.local`) can also take several seconds.
- **SNI passthrough doesn't rewrite the SNI**, so a backend virtual-hosted under a name other than the source may reject the connection.
- **Line endings:** `.gitattributes` keeps `.go` and `.sh` files at LF. If `gofmt -l` flags a file that looks correctly formatted, check its line endings.

## Known issues

Remove each one when it's fixed.

1. **No TLS *interception*.** `-https` only tunnels by SNI; goRebind can't see or modify HTTPS traffic. Decrypting would need a local CA that mints per-host certs (and the client trusting it).
2. **The system-resolver DNS path (no `-upstream`) is limited.** It answers only A/AAAA with a made-up 30 s TTL, and it can loop as described in [Gotchas](#gotchas). Prefer `-upstream` for real use.
3. **`main`'s shutdown orchestration isn't unit-tested**, and on Windows only Ctrl-C triggers it.

## Roadmap

1. **TLS interception (opt-in).** A local CA + per-host leaf certs, so HTTPS can be inspected and `ModifyResponse` applied to it. This is a bigger, trust-sensitive feature — keep it behind an explicit flag and off by default.
2. **Wildcard sources** (`*.example.com`).
3. **A repeatable `-route src=target` flag** for quick runs without a config file.
4. **Auto-detect the outbound IP** when `-I` is omitted.
5. **A `-dump` flag** that prints full requests and responses.
6. **A Docker image and a systemd unit** for easy deployment onto a test box. (The release workflow in [Releasing](#releasing) already ships cross-platform binaries.)
