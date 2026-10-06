# goRebind : Dynamic Reverse Proxy and DNS Resolver

This project provides a combined service: a high-performance HTTP reverse proxy and an optional local DNS server. It is designed to dynamically route traffic based on the requested host and includes advanced configuration options for SSL, outbound proxying, and HTTP/2 handling.

## Features

- **Dynamic Routing**: Maps source hostnames (e.g., `example.local`) to target URLs (e.g., `https://www.google.com`) via a simple JSON configuration file. The config is **hot-reloaded** when the file changes — no restart needed.
- **HTTPS SNI Passthrough (Optional)**: With `-https`, listens on 443 and routes TLS connections to the right target by their SNI hostname **without decrypting them**. Traffic stays end-to-end encrypted.
- **Local DNS Resolver (Optional)**: When enabled, it responds to `A` record queries for configured hosts with a specified local IP address, eliminating the need to modify your local host files. Other queries go to the system resolver, or to an `-upstream` DNS server.
- **Enhanced Proxy Stability**: Includes two crucial flags (`-http2=false` and `-no-keep-alive`) to resolve common proxy errors like `tls: user canceled` and `Unsolicited response`.
- **Flexible TLS Handling**: Allows skipping SSL certificate verification for local or development targets.
- **HTTP/2 Control**: Flag to force-enable or prevent HTTP/2 negotiation based on your backend or network requirements.

***

### 1. Prerequisites

You need Go installed (`go version 1.24+`).

### 2. How to Build & Run

**A. Clone and Build:**

```bash
# Clone or ensure you are in the project directory
go build -o goRebind .
```


**3. Run (Basic):**
```bash
./goRebind
# Output: HTTP Redirector listening on port 80...

```

**4. Run (With DNS):**
```bash
# Example for Linux/macOS
./goRebind -config config.json -port 8080 -dns -I wlan0
```


**4. Run (Custom):**
```bash
./goRebind -config config.json -port 8080 
```

**5. Run (With HTTPS SNI passthrough):**
```bash
# Serves HTTP on 80 and tunnels TLS on 443 by SNI, forwarding to each route's target
sudo ./goRebind -config config.json -dns -I wlan0 -https
```
> **Note:** Passthrough forwards the client's `ClientHello` unchanged, so the backend receives the **source** SNI. This is ideal for the DNS-rebind flow (the device looks up the real hostname, your DNS returns your IP, and the SNI already matches the real server). If the backend is virtual-hosted under a *different* name than the source, it may reject the connection — passthrough can't rewrite the SNI without decrypting.

### 3. Example Config File

Create a file named `config.json`:

```json
[
  {
    "source": "api.localhost",
    "target": "https://jsonplaceholder.typicode.com"
  },
  {
    "source": "local-app.com",
    "target": "http://127.0.0.1:9090"
  },
  {
    "source": "secure.internal",
    "target": "https://192.168.1.50"
  }
]
```

### Command Line Flags

| Flag | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `-port` | `int` | `80` | Port for the HTTP reverse proxy to listen on. |
| `-config` | `string` | (auto-detect) | Path to the JSON configuration file. |
| `-skip-ssl-verify` | `bool` | `true` | Skips TLS certificate verification for upstream targets. Useful for self-signed certificates. |
| `-proxy` | `string` | `""` | Outbound HTTP proxy URL (e.g., `http://user:pass@10.0.0.1:8080`). |
| `-http2` | `bool` | `false` | **Force-enable HTTP/2.** Set to `true` if your targets support H2 and you require it. *(Note: Setting this to `false` applies stability fixes to prevent the 'tls: user canceled' error.)* |
| **HTTPS Flags** | | | |
| `-https` | `bool` | `false` | Enable the HTTPS SNI passthrough listener. Routes TLS connections by SNI hostname without decrypting them. |
| `-https-addr` | `string` | `:443` | Listen address for the SNI passthrough listener. |
| **DNS Flags** | | | |
| `-dns` | `bool` | `false` | Enable the local DNS server (UDP and TCP). |
| `-dns-addr` | `string` | `:53` | Listen address for the DNS server. On Linux with systemd-resolved, use the interface IP (e.g., `192.168.1.5:53`) to avoid clashing with it. |
| `-upstream` | `string` | `""` | DNS server for names not in the config (e.g., `1.1.1.1` or `1.1.1.1:53`). Answers every record type with real TTLs. When empty, the system resolver is used, which answers only `A`/`AAAA`. |
| `-interface`, `-I` | `string` | `""` | Network interface name (e.g., `eth0` or `en0`). The IPv4 address of this interface will be returned for all matched hostnames. **Required if `-dns` is enabled.** |
| `-verbose` | `bool` | `false` | Enable verbose logging of DNS queries that aren't answered with the interface IP (passed upstream, or non-`A` queries for configured hosts). |
| `-no-keep-alive` | `bool` | `false` | Disable HTTP connection reuse (keep-alives). Use this flag if you encounter "Unsolicited response" or "readLoopPeekFailLocked" proxy errors. |


### FAQ

#### Troubleshooting: `httputil: ReverseProxy read error... tls: user canceled`

This error often occurs when an intermediate proxy (like Burp Suite or Zap) is used, and the underlying connection is closed prematurely by the backend.

**Workaround for Burp Suite:**

1. Navigate to **Settings** > **Proxy** > **HTTP**.

2. Go to the **Match and Replace** rules section.

3. Add a new rule:

   * **Type:** `Response header`

   * **Match:** `Connection: close`

   * **Replace:** `Connection: keep-alive`

#### Find interface name in Windows

```powershell
PS c:\>netsh interface show interface

Admin State    State          Type             Interface Name
-------------------------------------------------------------------------
Enabled        Disconnected   Dedicated        Local Area Connection
Enabled        Connected      Dedicated        VMware Network Adapter VMnet1
Enabled        Connected      Dedicated        WiFi
Enabled        Disconnected   Dedicated        Ethernet
```