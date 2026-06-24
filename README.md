# natt — NAT Traversal Tool

[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8)](https://go.dev/)
[![License](https://img.shields.io/badge/License-MIT-green)](LICENSE)

natt is a lightweight NAT traversal tool written in Go, with **zero external dependencies**. It supports two modes of operation:

- **Proxy mode**: Expose internal services (SSH/HTTP/DB) behind NAT to external users through a public server
- **RProxy mode**: Allow internal applications to access remote services through a tunnel (reverse proxy)

## Architecture

```
┌──────────────────────┐    TCP Encrypted Channel    ┌──────────────────────┐
│   Public Server      │◄──────────────────────────┤   Internal Client     │
│                      │   Control Connection        │                      │
│  natt server         │                             │  natt client         │
│                      │                             │                      │
│  Listens: bindPort:7000│                           │  Proxy mode:         │
│                      │                             │  Expose local        │
│  Proxy mode:         │                             │  services to         │
│  Exposes ports       │                             │  the internet        │
│  6000 (SSH)         │                             │                      │
│  8080 (Web)         │                             │  RProxy mode:        │
│                      │                             │  Listen on           │
│  RProxy mode:        │                             │  localPort, forward  │
│  Connects to remote  │                             │  to remote service   │
└──────┬───────────────┘                             └──────────────────────┘
       │                                            
       │  External user access (Proxy)                       
       │  ssh -p 6000 user@server
       │  http://server:8080
       ▼
```

## Quick Start

### 1. Build

```bash
# Build for all platforms (output: a single natt binary)
./build.sh

# Binaries are in the bin/ directory, named natt-{os}-{arch}
# Select mode via subcommand:
#   natt server -c server.json    # Start the server
#   natt client -c client.json    # Start the client
#   natt keygen                   # Generate an AES-256 key
```

### 2. Generate an Encryption Key (optional)

```bash
./bin/natt keygen
# Output: 192f6ba4dd790aff6ece202b26ebcdc4215f6b1992858f2c31af01fcd6abd1e6
```

### 3. Configure the Server

Create `server.json`:

```json
{
  "bindAddr": "0.0.0.0",
  "bindPort": 7000,
  "token": "your-secret-token",
  "encryptKey": "214f1afda32e8b712a87cbec119147c9cbdf5c9f463183aeee102c3fc23a0d48",
  "logLevel": "info"
}
```

Start on the public machine:

```bash
./bin/natt server -c server.json
```

### 4. Configure the Client

Create `client.json`:

```json
{
  "serverAddr": "your-server.com",
  "serverPort": 7000,
  "token": "your-secret-token",
  "encryptKey": "214f1afda32e8b712a87cbec119147c9cbdf5c9f463183aeee102c3fc23a0d48",
  "logLevel": "info",
  "proxies": [
    {
      "name": "ssh",
      "localIP": "127.0.0.1",
      "localPort": 22,
      "remotePort": 6000
    },
    {
      "name": "web",
      "localIP": "127.0.0.1",
      "localPort": 8080,
      "remotePort": 8080
    }
  ],
  "rproxies": [
    {
      "name": "db-tunnel",
      "localPort": 3307,
      "remoteIP": "10.0.0.50",
      "remotePort": 3306
    }
  ]
}
```

Start on the internal machine:

```bash
./bin/natt client -c client.json
```

### 5. Access Internal Services

```bash
# Access internal SSH through the public server
ssh -oPort=6000 user@your-server.com

# Access internal web service
curl http://your-server.com:8080
```

## How It Works

### Control Flow

1. The client actively connects to the server's `bindPort`, establishing a **control connection** (long-lived)
2. The client sends a `Register` message to complete authentication (with Token verification)
3. The client sends a `ProxyRequest` to request port mapping
4. The server replies with `ProxyResponse` and starts listening on the corresponding `remotePort`

### Proxy Data Flow (External → Internal)

1. An external user connects to the server's `remotePort`
2. The server sends a `TunnelOpen` notification to the client over the control connection
3. The client **establishes a new TCP connection** to the server
4. The client sends a `DataConnect{mode:"proxy"}` handshake on this connection
5. The server pairs the external user's connection with the client's data connection
6. Both sides perform `io.Copy` for bidirectional data forwarding

### RProxy Data Flow (Internal → Remote)

1. The client listens on `localPort`, waiting for local application connections
2. A local application connects to the client's `localPort`
3. The client **establishes a new TCP connection** to the server, sending a `DataConnect{mode:"rproxy", rproxyName}` handshake
4. The server looks up the mapped `remoteIP:remotePort` for the given `rproxyName`
5. The server actively connects to `remoteIP:remotePort`
6. The server pairs the remote connection with the data connection and performs io.Copy
7. The client pairs the data connection with the local application connection and performs io.Copy

### Encrypted Transport

- All TCP connections are wrapped with **AES-256-GCM** immediately after establishment
- Every byte is ciphertext from the start — **no plaintext phase**
- The encryption key is pre-shared through the config file (hex-encoded 32-byte key)
- GCM authentication tags ensure data integrity; mismatched keys cause automatic disconnection

## Command-Line Arguments

Use the unified `natt` binary, selecting the mode via subcommand:

```
natt server -c server.json    # Start the server
natt client -c client.json    # Start the client
natt keygen                   # Generate an AES-256 key
```

### Server Subcommand

```
natt server [-c server.json]

Options:
  -c string  Config file path (default "server.json")
```

### Client Subcommand

```
natt client [-c client.json] [-s host] [-p port]

Options:
  -c string  Config file path (default "client.json")
  -s string  Server address (overrides config)
  -p int     Server port (overrides config)
```

## Configuration Reference

### Server Configuration (`server.json`)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `bindAddr` | string | `"0.0.0.0"` | Listen address |
| `bindPort` | int | `7000` | Control connection port |
| `token` | string | `""` | Authentication token (leave empty to disable) |
| `encryptKey` | string | `""` | AES-256 key (leave empty to disable encryption) |
| `logLevel` | string | `"info"` | Log level: debug/info/warn/error |

### Client Configuration (`client.json`)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `serverAddr` | string | — | Server address (required) |
| `serverPort` | int | `7000` | Server port |
| `token` | string | `""` | Authentication token |
| `encryptKey` | string | `""` | AES-256 key |
| `logLevel` | string | `"info"` | Log level |
| `heartbeatIntervalMs` | int | `45000` | Heartbeat interval (milliseconds) |
| `reconnectBaseDelayMs` | int | `500` | Reconnect base delay (milliseconds) |
| `reconnectMaxDelayMs` | int | `60000` | Reconnect maximum delay (milliseconds) |
| `proxies` | array | `[]` | Proxy mapping rules (proxy mode) |
| `rproxies` | array | `[]` | Reverse proxy mapping rules (rproxy mode) |

#### Proxy Mapping Rules (`proxies[]`)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | — | Rule name (unique identifier) |
| `localIP` | string | `"127.0.0.1"` | Local service IP |
| `localPort` | int | — | Local service port |
| `remotePort` | int | — | Server-exposed port (0 = random assignment) |

#### Reverse Proxy Mapping Rules (`rproxies[]`)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | — | Rule name (unique identifier) |
| `localPort` | int | — | Client listen port |
| `remoteIP` | string | — | Remote service IP (reachable from the server) |
| `remotePort` | int | — | Remote service port |

## Error Handling

| Scenario | Handling |
|----------|----------|
| Server not started | Client exponential backoff reconnect (0.5s → 1.5s → 2.25s → ... → 60s cap) |
| Network interruption | Heartbeat timeout detection (3× interval without reply = disconnect) |
| Key mismatch | GCM authentication failure, immediate connection close + alert |
| Data connection drop | Does not affect the control connection; only the corresponding tunnel is closed |
| Graceful shutdown | SIGINT/SIGTERM → close data connections → notify peer → close control connection |

## Testing

```bash
# Run unit tests
go test ./pkg/... -v -count=1 -timeout 60s

# Run integration tests (including encrypted tunnel verification)
go test -tags=integration -v -count=1 -timeout 120s .

# Format code
go fmt ./pkg/... ./cmd/...

# Lint (static analysis)
go vet ./pkg/... ./cmd/...
```

## Build Requirements

- Go 1.22+
- Zero external dependencies (standard library only)

## Project Structure

```
natt/
├── cmd/
│   └── natt/main.go      # Unified entry point (subcommands: server/client/keygen)
├── pkg/
│   ├── crypto/           # AES-256-GCM encryption
│   ├── protocol/         # Communication protocol frames
│   ├── config/           # Configuration loading
│   ├── server/           # Server logic
│   ├── client/           # Client logic
│   │   ├── client.go
│   │   ├── data_conn.go
│   │   ├── local_connector.go
│   │   └── rproxy.go     # RProxy local port listener
│   └── common/           # Constants
├── server.json           # Example config
└── client.json           # Example config
```

## License

This project is licensed under the MIT License.
