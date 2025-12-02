```

### 2. Implementation details

#### How the Config Works
The program uses a strict JSON format. It defines a `routingTable` in memory.
1.  It checks the `-config` flag.
2.  If the flag is missing, it generates a random filename (e.g., `redirector-config-12345.json`) and sets that as the target path.
3.  It checks if that file exists on disk.
    * **If No:** It creates the file with a sample JSON array.
    * **If Yes:** It reads the JSON.
4.  It iterates over the JSON array, parsing the `source` (incoming Host header) and `target` (upstream URL) into a map for fast $O(1)$ lookups.

#### How Routing is Handled
Inside the `proxyHandler` function:
1.  **Host Extraction:** It grabs `r.Host` from the incoming request. It attempts to match it exactly against the loaded config. If that fails, it strips the port (e.g., `:8080`) and tries again.
2.  **Request Construction:** A new HTTP request is created.
    * **URL:** Combines the `Target` (scheme + host) with the *original* Path and Query strings.
    * **Headers:** Loops through every single header in the original request and copies them to the new request.
    * **Host Header:** Explicitly overwrites the `Host` header to match the `target` domain (required for virtual hosting/SNI).
3.  **Transport:** The custom `http.Client` sends the request.
4.  **Response:** The response headers and status code are copied back to the client, followed by streaming the body via `io.Copy`.

#### Flags Explanation

* `-config <file>`: Specifies the JSON file. If omitted, the program acts intelligently to generate a random one and use it.
* `-skip-ssl-verify <true/false>`: Controls the `InsecureSkipVerify` setting in the TLS config. Defaults to `true` (skips verification) as requested.
* `-port <num>`: Integers only. Sets the TCP port the Go server listens on.
* `-proxy <url>`: If provided, the internal Transport sets its `Proxy` field to `http.ProxyURL`, routing outbound traffic through that gateway.

#### How to Build & Run

**1. Initialize Module (Optional but recommended):**
```bash
go mod init redirector
```

**2. Build:**
```bash
go build -o redirector main.go
```

**3. Run (Basic):**
```bash
./redirector
# Output: No config specified. Using generated filename: redirector-config-85721.json ...
```

**4. Run (Custom):**
```bash
./redirector -config myroutes.json -port 8080 -skip-ssl-verify=false
```

### 3. Example Config File

Create a file named `routes.json`:

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
]# GoRebind
