# NAT Traversal System Design Document

> Version: 2.0.0  
> Last updated: 2025-07-XX

---

## Table of Contents

1. [Project Overview](#1-project-overview)
2. [System Architecture](#2-system-architecture)
3. [Module Design](#3-module-design)
4. [Communication Protocol](#4-communication-protocol)
5. [Business Flow](#5-business-flow)
6. [Error Handling & Reliability](#6-error-handling--reliability)
7. [Configuration Format](#7-configuration-format)
8. [Build & Deployment](#8-build--deployment)
9. [Appendix](#9-appendix)

---

## 1. Project Overview

### 1.1 Background

NAT traversal (also known as NAT piercing / reverse proxy) enables machines behind NAT or firewalls to be accessed from the public internet. Typical use cases include:

- Temporarily exposing a service developed at home to colleagues for testing
- Securely accessing internal databases or SSH services from the outside
- Remote access to IoT device management ports

### 1.2 Goals

- **Lightweight**: Single binary deployment, no external dependencies
- **Reliable**: Automatic reconnection on disconnect, heartbeat keepalive, automatic resource cleanup
- **Secure**: Token-based authentication, optional TLS encryption
- **Easy to use**: Simple configuration file, comprehensive command-line arguments

### 1.3 Glossary

| Term | Description |
|------|-------------|
| **Server** | The relay service running on a public machine; accepts client connections and opens proxy ports |
| **Client** | The proxy agent running on an internal machine; actively connects to the server and requests port mappings |
| **Control Connection** | A long-lived TCP connection from the client to the server, used only for control messages (register, proxy requests, heartbeats, tunnel notifications) |
| **Data Connection** | A new TCP connection initiated by the client upon receiving a `TunnelOpen` notification (connecting to the same server port), dedicated to data transport for a specific tunnel |
| **Tunnel** | A pairing of a data connection and an external user connection; the server bridges them for bidirectional data forwarding |
| **Proxy Mapping** | A port mapping rule: client local port ↔ server public port |
| **Reverse Proxy Mapping (RProxy)** | A reverse port mapping rule: client local port ↔ server remoteIP:remotePort, enabling the client to proactively access remote services |
| **Heartbeat** | A keepalive message sent periodically by the client to monitor the control connection status |

---

## 2. System Architecture

### 2.1 Overall Architecture Diagram

```
 Public Network                          Internal Network
┌──────────────────────────────┐    ┌──────────────────────────────┐
│          Server              │    │          Client              │
│                              │    │                              │
│  ┌────────────────────────┐  │    │  ┌────────────────────────┐  │
│  │  Control Connection     │◄─┼───┼──┤  Control Connection    │  │
│  │  Handler                │  │    │  │  Manager               │  │
│  │  (bindPort:7000)       │  │    │  │  (serverAddr:port)     │  │
│  └───────────┬────────────┘  │    │  └────────────────────────┘  │
│              │               │    │                              │
│  ┌───────────▼────────────┐  │    │  ┌────────────────────────┐  │
│  │  Client Registry        │  │    │  │  Heartbeat Timer       │  │
│  │  (clientID → controlConn)│  │    │  │  (45s interval)        │  │
│  └────────────────────────┘  │    │  └────────────────────────┘  │
│                              │    │                              │
│  ┌────────────────────────┐  │    │  ┌────────────────────────┐  │
│  │  Proxy Port Manager     │  │    │  │  Data Connection       │──┐ │
│  │  (remotePort → proxy)  │  │    │  │  Manager               │  │ │
│  └───────────┬────────────┘  │    │  │  (new TCP on notify)   │  │ │
│              │               │    │  └────────────────────────┘  │ │
│  ┌───────────▼────────────┐  │    │  ┌────────────────────────┐  │ │
│  │  Data Bridge            │  │    │  │  Local Connector       │  │ │
│  │  (pairs external conn   │  │    │  │  (localIP:localPort)  │──┘ │
│  │   ↔ data conn)         │  │    │  └────────────────────────┘  │ │
│  └────────────────────────┘  │    │                              │ │
│                              │    │  ┌────────────────────────┐  │ │
│  ┌────────────────────────┐  │    │  │  Reconnect Manager     │  │ │
│  │  Signal Handler         │  │    │  │  (exponential backoff) │  │ │
│  └────────────────────────┘  │    │  └────────────────────────┘  │ │
│                              │    │                              │ │
│                              │    │  ┌────────────────────────┐  │ │
│                              │    │  │  Signal Handler         │  │ │
│                              │    │  └────────────────────────┘  │ │
└──────────────────────────────┘    └──────────────────────────────┘
          ▲                                     │
          │    External user access              │ Data connection (new TCP)
          │    ssh -p 6000 user@server           │ (same port: bindPort)
          │                                     ▼
          └──────────────────────────────────────┘
                      Same TCP link
```

### 2.2 Data Flow Description

**Control Flow:**
1. Client connects to the server's `bindPort`, establishing a control connection
2. Client sends a `Register` message to complete identity registration
3. Client sends a `ProxyRequest` to request port mapping
4. Server replies with `ProxyResponse`, confirms the mapping, and starts listening on the corresponding port
5. Both sides send periodic `Heartbeat` messages for keepalive

**Data Flow — Proxy Mode (External → Internal):**
1. An external user connects to the server's `remotePort`
2. The server accepts the connection, assigns a unique `dataConnId`, and sends `TunnelOpen{dataConnId, proxyName}` to the client over the **control connection**
3. Upon receiving it, the client **initiates a new TCP connection** to the server's `bindPort` (same port as the control connection)
4. The client sends a `DataConnect{dataConnId, mode:"proxy"}` handshake message on this new connection
5. The server receives `DataConnect` and pairs this data connection with the previously waiting external user connection
6. The server's **data bridge** performs bidirectional byte copying (io.Copy) between the two TCP connections
7. The client simultaneously connects to the local service at `localIP:localPort` and performs bidirectional copying between the local connection and the data connection
8. When either side closes the connection, a `TunnelClose` message is sent (via the control connection), and the other side cleans up resources

**Data Flow — RProxy Mode (Internal → Remote):**
1. The client listens on `localPort`, waiting for local application connections
2. A local application connects to the client's `localPort`
3. The client **initiates a new TCP connection** to the server's `bindPort`, sending a `DataConnect{mode:"rproxy", rproxyName}` handshake message
4. The server looks up the mapped `remoteIP:remotePort` for the given `rproxyName`
5. The server actively connects to `remoteIP:remotePort`
6. The server's bridge pairs the remote connection with the data connection, performing bidirectional io.Copy
7. The client pairs the data connection with the local application connection, performing bidirectional io.Copy
8. When either side closes, resources are cleaned up

---

## 3. Module Design

### 3.1 Project Directory Structure

```
natt/
├── cmd/
│   └── natt/main.go            # Unified entry point
├── pkg/
│   ├── config/
│   │   ├── common.go          # Shared structs (ProxyRule, RProxyRule) + DecryptKey
│   │   ├── server.go          # Server config struct + loader
│   │   ├── client.go          # Client config struct + loader
│   │   └── config_test.go     # Config loading & validation tests
│   ├── crypto/
│   │   ├── cipher.go          # AES-256-GCM encrypted connection wrapper
│   │   ├── cipher_test.go     # Encryption/decryption unit tests
│   │   └── key.go             # GenerateKey key generation
│   ├── protocol/
│   │   ├── message.go         # Message types + frame format + encode/decode + read/write
│   │   └── message_test.go    # Protocol message tests
│   ├── server/
│   │   ├── server.go          # Server main loop (distinguishes control vs data connection by first message)
│   │   ├── registry.go        # Client registry
│   │   ├── registry_test.go   # Registry unit tests
│   │   ├── proxy_manager.go   # Proxy/rproxy port management + data bridging
│   │   └── proxy_manager_test.go
│   ├── client/
│   │   ├── client.go          # Client main loop (control connection + reconnect)
│   │   ├── local_connector.go # Local service connector
│   │   ├── data_conn.go       # Data connection management (new TCP upon TunnelOpen)
│   │   └── rproxy.go          # RProxy local port listener (new TCP upon local connection)
│   └── common/
│       └── constant.go        # Constants
├── integration_test.go        # End-to-end integration tests (including encrypted tunnel verification)
├── go.mod
├── go.sum
├── server.json                # Server config example
├── client.json                # Client config example
└── README.md
```

### 3.2 Core Module Responsibilities

#### pkg/config — Configuration Loading

```
type ServerConfig struct {
    BindAddr   string // Control connection listen address, default "0.0.0.0"
    BindPort   int    // Control connection listen port, default 7000
    Token      string // Authentication token, optional
    EncryptKey string // AES-256 encryption key (32-byte hex), optional
    LogLevel   string // Log level, default "info"
    LogFile    string // Log file path, optional
}

type ClientConfig struct {
    ServerAddr string       // Server address (required)
    ServerPort int          // Server port, default 7000
    Token      string       // Authentication token, must match server
    EncryptKey string       // AES-256 encryption key, must match server
    LogLevel   string       // Log level, default "info"
    LogFile    string       // Log file path, optional
    Proxies    []ProxyRule  // Proxy mapping rules list (proxy mode)
    RProxies   []RProxyRule // Reverse proxy mapping rules list (rproxy mode)

    // The following optional fields can be overridden via config file
    HeartbeatIntervalMs  int // Heartbeat interval (ms), default 45000
    ReconnectBaseDelayMs int // Reconnect base delay (ms), default 500
    ReconnectMaxDelayMs  int // Reconnect maximum delay (ms), default 60000
}

type ProxyRule struct {
    Name       string // Rule name, unique identifier
    LocalIP    string // Local service IP, default "127.0.0.1"
    LocalPort  int    // Local service port
    RemotePort int    // Server-exposed port (0 = random assignment)
}

type RProxyRule struct {
    Name       string // Rule name, unique identifier
    LocalPort  int    // Client listen port
    RemoteIP   string // Remote service IP (reachable from server)
    RemotePort int    // Remote service port
}
```

- Supports JSON configuration files
- Supports command-line argument overrides (e.g., `-c server.json`, `-s host -p 7000`)
- Configuration validation: required field checks, port range validation

#### pkg/protocol — Communication Protocol

Frame format and message definitions are described in [Chapter 4](#4-communication-protocol).

All functionality is concentrated in a single `message.go` file:
- `Encode(msg) → []byte` / `Decode([]byte) → Message`: Message serialization and deserialization (type + length + JSON payload)
- `ReadMessage(conn) → Message`: Reads a complete encrypted packet from a CipherConn-wrapped connection, decrypts it, then parses the inner protocol frame from the decrypted plaintext
- `WriteMessage(conn, msg)`: Serializes the message frame and writes it through CipherConn with encryption (CipherConn automatically handles frame wrapping and encryption)

#### pkg/crypto — Encrypted Transport

Provides an AES-256-GCM connection wrapper:
- `NewCipherConn(conn net.Conn, key []byte) (net.Conn, error)`: Wraps a regular TCP connection as an encrypted connection; `Write` automatically encrypts (random nonce + GCM ciphertext), `Read` automatically decrypts
- Key is fixed at 32 bytes (AES-256), provided as a hex-encoded string in the configuration
- **Wrapping timing**: Immediately after the TCP connection is established (Accept / Dial), `NewCipherConn` is called before any application data is read or written. All messages — including control connection `Register`, `RegisterAck`, and data connection `DataConnect` — are transmitted encrypted, with no plaintext phase

#### pkg/server — Server Core

| Component | Responsibility |
|-----------|---------------|
| **Server** | Main loop: listen on `bindPort` → accept connections → immediately wrap as CipherConn(encryptKey) → read first message, dispatch by message type to the control handler or data bridge |
| **Registry** | Manages online clients (clientID → controlConn mapping), supports lookup, registration, and removal |
| **ProxyManager** | Manages proxy and rproxy mappings + data bridging: handles `ProxyRequest` → listens on ports + waits for data connection pairing; handles `RProxyRequest` → stores `name → remoteIP:remotePort` mapping; upon accepting an external connection, assigns a `dataConnId` → sends `TunnelOpen` via the control connection → waits for the data connection; upon receiving a `DataConnect` handshake, dispatches based on the `mode` field — for `"proxy"` mode, matches the waiting external user connection by `dataConnId` and performs io.Copy; for `"rproxy"` mode, looks up the mapping by `rproxyName`, actively `Dial(remoteIP:remotePort)`, and performs io.Copy |

#### pkg/client — Client Core

| Component | Responsibility |
|-----------|---------------|
| **Client** | Main loop: control connection setup → register → proxy request → message loop (handles TunnelOpen/TunnelClose/HeartbeatAck) → reconnect loop upon disconnection |
| **LocalConnector** | Connects to local service (`localIP:localPort`) on demand; bridges data connection ↔ local connection |
| **DataConnManager** | Manages data connections: upon receiving `TunnelOpen`, creates a new TCP connection to the server, sends `DataConnect` handshake, then bridges with the local connector |
| **RProxy** | Listens on the local `localPort` for local application connections; upon receiving one, creates a new TCP connection to the server, sends `DataConnect{mode:"rproxy", rproxyName}`, bridges data connection ↔ local connection |

---

## 4. Communication Protocol

### 4.1 Frame Format

```
┌──────────────────────────────────────────────────────┐
│  Type (1 byte)    │  Length (2 bytes, big-endian)   │
├──────────────────────────────────────────────────────┤
│  Payload (JSON, variable length)                     │
│  ≤ 65535 bytes                                       │
└──────────────────────────────────────────────────────┘
```

- **Type**: message type identifier (1 byte, see section 4.2)
- **Length**: payload length (2 bytes, big-endian, excluding the 3-byte type+length header)
- **Payload**: UTF-8 JSON, maximum 65535 bytes

> **Note**: The wire-format frame described here operates **within** the CipherConn encryption layer. The actual bytes transmitted over TCP are the encrypted output of CipherConn, not the raw frame content.

### 4.2 Message Types

| Hex | Constant | Direction | Description |
|-----|----------|-----------|-------------|
| `0x01` | `TypeRegister` | Client → Server | Registration request, payload: `{clientId, token, version}` |
| `0x02` | `TypeRegisterAck` | Server → Client | Registration acknowledgment, payload: `{success, error?}` |
| `0x03` | `TypeHeartbeat` | Client → Server | Heartbeat request |
| `0x04` | `TypeHeartbeatAck` | Server → Client | Heartbeat response |
| `0x05` | `TypeProxyRequest` | Client → Server | Proxy request, payload: `{proxies: [{name, localIP, localPort, remotePort}]}` |
| `0x06` | `TypeProxyResponse` | Server → Client | Proxy response, payload: `{results: [{name, success, remotePort?, error?}]}` |
| `0x07` | `TypeTunnelOpen` | Server → Client | Tunnel open notification, payload: `{dataConnId, proxyName}` |
| `0x08` | `TypeDataConnect` | Client → Server | Data connection handshake, payload: `{dataConnId, mode, rproxyName?}` |
| `0x09` | `TypeTunnelClose` | Either → Peer | Tunnel close notification, payload: `{reason?}` |
| `0x0A` | `TypeRProxyRequest` | Client → Server | RProxy setup request, payload: `{rproxies: [{name, remoteIP, remotePort}]}` |
| `0x0B` | `TypeRProxyResponse` | Server → Client | RProxy setup response, payload: `{results: [{name, success, error?}]}` |

### 4.3 Key Protocol Design Decisions

**1. Why read the first message to distinguish control vs data connection?**
- Design principle: The server listens on a single `bindPort` and determines the connection type by reading the first message
- A **control connection** sends `Register` as the first message
- A **data connection** sends `DataConnect` as the first message
- This eliminates the need for separate ports

**2. Why does the client re-initiate a connection instead of reusing the control connection?**
- The control connection carries heartbeat and control messages; adding data traffic would increase complexity
- Data connections are short-lived (created per tunnel) and may disconnect independently without affecting the control plane
- Multiple concurrent data connections can be pipelined to improve throughput

**3. Why is the control connection persistent, while data connections are dynamically established?**
- The control connection must be persistent to push `TunnelOpen` notifications in real-time
- Data connections are created on demand, reducing resource consumption during idle periods

### 4.4 Message Flow Order Diagram

```
Control Connection:               Data Connection (per tunnel):
  Client → Server                   Client → Server
  ─────────────────────             ─────────────────────
  1. Register                       1. DataConnect
  2. RegisterAck                       {dataConnId, mode}
  3. ProxyRequest
  4. ProxyResponse
  5. Heartbeat (periodic)
  6. HeartbeatAck (periodic)
  7. TunnelOpen (pushed)
  ...
  N. TunnelClose
```

---

## 5. Business Flow

### 5.1 Connection Establishment Sequence

```
Client                                  Server
  │                                       │
  │── TCP Connect ──────────────────────►│
  │      (serverAddr:serverPort)          │
  │                                       │
  │◄── NewCipherConn(encryptKey) ───────│
  │                                       │
  │── Register ─────────────────────────►│
  │  {clientId, token, version}          │
  │                                       │
  │  ┌─ Verify token & clientId ─────┐   │
  │  │  • Check token matches config  │   │
  │  │  • Check clientId is unique    │   │
  │  │  • Assign to registry          │   │
  │  └────────────────────────────────┘   │
  │                                       │
  │◄─── RegisterAck ─────────────────────│
  │  {success:true}                      │
  │                                       │
  │── ProxyRequest ────────────────────►│
  │  {proxies:[...]}                    │
  │                                       │
  │  ┌─ Process mappings ────────────┐   │
  │  │  • For each proxy:            │   │
  │  │    - Listen on remotePort     │   │
  │  │    - Register in ProxyManager │   │
  │  └────────────────────────────────┘   │
  │                                       │
  │◄─── ProxyResponse ───────────────────│
  │  {results:[{success:true,...}]}      │
  │                                       │
  │══ Heartbeat (every 45s) ═══════════►│
  │◄══ HeartbeatAck ════════════════════│
  │                                       │
```

### 5.2 Proxy Tunnel Establishment Flow

```
External User          Server                          Client           Local Service
     │                    │                              │                    │
     │                    │  Listening on remotePort      │                    │
     │                    │  (e.g., 6000)                │                    │
     │                    │                              │                    │
     │── TCP Connect ───►│                              │                    │
     │  remotePort       │                              │                    │
     │                    │                              │                    │
     │                    │  Assign dataConnId           │                    │
     │                    │  (uuid)                     │                    │
     │                    │                              │                    │
     │                    │── TunnelOpen ──────────────►│                    │
     │                    │  {dataConnId, proxyName}    │                    │
     │                    │  (via control conn)         │                    │
     │                    │                              │                    │
     │                    │    New TCP Connect           │                    │
     │                    │◄────────────────────────────│                    │
     │                    │  (same bindPort)             │                    │
     │                    │                              │                    │
     │                    │◄── NewCipherConn ──────────│                    │
     │                    │                              │                    │
     │                    │◄── DataConnect ────────────│                    │
     │                    │  {dataConnId, mode:"proxy"} │                    │
     │                    │                              │                    │
     │                    │  Pair external conn          │── TCP Connect ──►│
     │                    │  ⇔ data conn                 │  localIP:localPort│
     │                    │                              │                    │
     │    io.Copy         │    io.Copy                   │    io.Copy        │
     │◄══════════════════►│◄════════════════════════════►│◄═════════════════►│
     │                    │                              │                    │
     │    Close conn      │                              │                    │
     │──────────────────►│                              │                    │
     │                    ├── TunnelClose ─────────────►│                    │
     │                    │   {reason:"client closed"}   │                    │
     │                    │                              ├── Close local ───►│
     │                    │                              │                    │
```

### 5.3 Client Reconnection Flow

```
Client reconnection is a core reliability mechanism — each step is detailed below:

Client.Start()
  │
  ├─ First connection (run()):
  │    ├─ TCP Dial with 5s timeout
  │    ├─ TCP keepalive (75s period)
  │    ├─ Wrap as CipherConn(encryptKey)
  │    ├─ Send Register → Receive RegisterAck
  │    ├─ Send ProxyRequest → Receive ProxyResponse
  │    ├─ Start heartbeat timer
  │    └─ Enter message loop (messageLoop)
  │
  ├─ messageLoop handles:
  │    ├─ TunnelOpen
  │    │    └─ initDataConn(dataConnId, proxyName)
  │    │         ├─ New TCP connection (separate goroutine)
  │    │         ├─ CipherConn wrapping
  │    │         ├─ Send DataConnect handshake
  │    │         └─ Bridge: local service ↔ data conn
  │    │
  │    ├─ TunnelClose
  │    │    └─ Close local & data connection pair
  │    │
  │    ├─ HeartbeatAck
  │    │    └─ Reset heartbeat timer
  │    │
  │    └─ Read error (disconnect detected)
  │         ├─ Close control connection
  │         ├─ Close all active tunnels (closeAllTunnels)
  │         │    ├─ Iterate tunnels map, call cancel() for each
  │         │    └─ cancel() triggers monitoring goroutine to close dataConn and localConn
  │         │
  │         └─ Run() catches error → log "connection error"
  │
  └─ Enter reconnection loop (reconnectLoop, retries indefinitely until client Stop):
       │
       ├─ Check c.active == false? (only when Stop() is called)
       │    └─ Yes → return false → Run() returns "client stopped" → process exits
       │
       ├─ delay = ReconnectBaseDelay (default 0.5s)
       │
       ├─ Loop:
       │    ├─ slog.Info("reconnecting", "delay", delay)
       │    ├─ time.Sleep(delay)
       │    ├─ net.DialTimeout(serverAddr, serverPort, 5s)
       │    │    ├─ Success → testConn.Close() → break out of reconnection loop
       │    │    └─ Failure → delay = min(delay × 1.5, ReconnectMaxDelay)
       │    │                  continue (no retry limit)
       │    │
       │    └─ Only exit condition: c.active == false (Stop() called)
       │
       ├─ Reconnection successful:
       │    ├─ New TCP connection
       │    ├─ TCP keepalive (75s period)
       │    ├─ Immediately wrap as CipherConn(encryptKey)
       │    ├─ c.control = conn, c.active = true
       │    ├─ Send Register → Receive RegisterAck
       │    ├─ Send ProxyRequest → Receive ProxyResponse
       │    ├─ Start heartbeat timer
       │    └─ Enter message loop (messageLoop)
       │
       └─ Disconnect again → back to loop start

Notes:
- Reconnection has **no retry limit**; it retries indefinitely until the server recovers or the client is stopped via Stop()
- Each reconnection goes through the full registration and proxy request flow
- Data connection disconnection does not affect the control connection. When the server detects a data connection drop, it sends a TunnelClose notification via the control connection to instruct the client to close the corresponding local connection
```

### 5.4 RProxy Tunnel Establishment Flow

RProxy mode is triggered locally by the client, allowing internal applications to access remote services through a tunnel:

```
Local App                  Client                          Server                   Remote Service
  │                          │                              │                        │
  │  connect:localPort       │                              │                        │
  │─────────────────────────►│                              │                        │
  │                          │  New TCP Connect              │                        │
  │                          │─────────────────────────────►│（same bindPort）        │
  │                          │                              │                        │
  │                          │◄── NewCipherConn ───────────│                        │
  │                          │                              │                        │
  │                          │── DataConnect ─────────────►│                        │
  │                          │  {mode:"rproxy",             │                        │
  │                          │   rproxyName:"db-tunnel"}    │                        │
  │                          │                              │  Look up mapping       │
  │                          │                              │  by rproxyName         │
  │                          │                              │                        │
  │                          │                              │── TCP Connect ────────►│
  │                          │                              │  remoteIP:remotePort   │
  │                          │                              │◄────── accept ─────────│
  │                          │                              │                        │
  │                          │    io.Copy bidirectional     │   io.Copy bidirectional │
  │◄────── raw data ────────│◄─────────────────────────────│◄────────────────────────│
  │                          │                              │                        │
  │────── raw data ────────►│──────────────────────────────►│────────────────────────►│
  │                          │                              │                        │
  │                          │   Encrypted via CipherConn   │   Raw TCP byte stream  │
  │                          │                              │                        │
  │  Close connection        │                              │                        │
  │─────────────────────────►│                              │                        │
  │                          ├── Close data conn ──────────►│                        │
  │                          │                              ├── Close remote conn ───►│
  │                          │                              │                        │
```

---

## 6. Error Handling & Reliability

### 6.1 Network Anomalies

| Scenario | Detection Method | Handling |
|----------|-----------------|----------|
| Server not started / crash | TCP connection timeout / EOF | Client retries indefinitely (0.5s → ×1.5 → 60s cap) until client is stopped |
| Network interruption | TCP keepalive / heartbeat write failure / read timeout | The earliest of the three detection methods triggers reconnection (as fast as 45s); on disconnect, close all active tunnels, re-register and re-request proxies after reconnection |
| Long-term network outage | Read timeout (HeartbeatInterval × 3) | Infinite reconnection at max backoff interval of 60s; auto-rebuilds proxy mappings on recovery |
| DNS resolution failure | net.Dial returns error | Log alert, retry with backoff strategy |
| DataConnect handshake timeout | Server waits 10s without DataConnect | Close external user connection, release dataConnId resources |
| Data connection mid-stream disconnection | TCP Read/Write returns error | Server closes paired external connection, sends TunnelClose via control connection |
| Encryption key mismatch | GCM authentication failure / decryption error | Close the current connection, log the error; control connection triggers client reconnection |

### 6.2 Heartbeat Mechanism

Heartbeats are sent **only on the control connection**; data connections do not send heartbeats:

```
Client (control connection)            Server
  │                                      │
  │ Every HeartbeatInterval (45s):      │
  │──── Heartbeat ──────────────────────►│
  │                                      │ Record lastHeartbeat
  │◄─── HeartbeatAck ───────────────────│
  │                                      │
  │ If no Ack received within            │ If no heartbeat received within
  │ 3 × HeartbeatInterval:               │ 3 × HeartbeatInterval:
  │ Control connection considered        │ Client considered offline →
  │ disconnected → trigger reconnection  │ close all associated data
  │                                      │ connections, release proxy
  │                                      │ ports and registration
```

- Both client and server maintain their own heartbeat timeout counters
- Receiving a heartbeat or control message from the peer resets the counter
- If the client fails to write a heartbeat (e.g., server already disconnected), it immediately calls `conn.Close()` to close the control connection, unblocking `messageLoop` blocked on `ReadMessage`, and triggers reconnection
- When the server detects a client timeout: close all associated data connections, release proxy ports, remove registration

### 6.3 Resource Leak Protection

| Resource | Protection |
|----------|-----------|
| Control connection TCP | `defer conn.Close()`; set read timeout; active close on heartbeat timeout |
| Data connection TCP | `io.Copy` returns error on either end → close both ends; set `KeepAlive` to prevent zombie connections |
| Goroutine | Each data bridge uses an independent goroutine managed by `errgroup`; on control connection exit, cancel all data goroutines via context |
| dataConnId | Released after use; clean up the waiting queue when DataConnect times out, preventing ID leaks |
| Encryption context | Each CipherConn holds its own GCM instance; automatically released when the connection closes; no extra cleanup needed |
| Proxy ports | Server records port ↔ client mapping; automatically releases associated ports and data connections when client control connection disconnects |

### 6.4 Graceful Shutdown

```
SIGINT / SIGTERM received
  │
  ├─ 1. Stop accepting new connections
  │     (Server stops Listener; Client stops reconnection loop)
  │
  ├─ 2. Close all active data connections
  │     ├─ Stop io.Copy forwarding
  │     ├─ Close data connection TCP (Client side)
  │     └─ Close paired external user connection (Server side)
  │
  ├─ 3. Notify peer of shutdown (via control connection)
  │     ├─ Client → Server sends close message
  │     └─ Wait for acknowledgment (3s timeout)
  │
  ├─ 4. Close control connection
  │
  └─ 5. Release all listening ports → exit (os.Exit(0))
     (Overall timeout 10s; force exit if not completed)
```

---

## 7. Configuration Format

### 7.1 Server Configuration

```json
{
  "bindAddr": "0.0.0.0",
  "bindPort": 7000,
  "token": "your-secret-token",
  "encryptKey": "7f3b8c1a2d5e9f064a7b8c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0",
  "logLevel": "info"
}
```

- `bindAddr`: Listen address, default `"0.0.0.0"`
- `bindPort`: Control connection port, default `7000`
- `token`: Authentication token, optional (leave empty to disable)
- `encryptKey`: AES-256 key (32-byte hex-encoded), optional (leave empty to disable encryption), can be generated via `natt keygen`
- `logLevel`: Log level `debug|info|warn|error`, default `"info"`
- `logFile`: Log file path, optional (leave empty for stdout)

### 7.2 Client Configuration

```json
{
  "serverAddr": "your-server.com",
  "serverPort": 7000,
  "token": "your-secret-token",
  "encryptKey": "7f3b8c1a2d5e9f064a7b8c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0",
  "logLevel": "info",
  "logFile": "",
  "heartbeatIntervalMs": 45000,
  "reconnectBaseDelayMs": 500,
  "reconnectMaxDelayMs": 60000,
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
    },
    {
      "name": "mysql",
      "localIP": "192.168.1.50",
      "localPort": 3306,
      "remotePort": 13306
    }
  ],
  "rproxies": [
    {
      "name": "db-tunnel",
      "localPort": 3307,
      "remoteIP": "10.0.0.50",
      "remotePort": 3306
    },
    {
      "name": "internal-api",
      "localPort": 8081,
      "remoteIP": "192.168.1.100",
      "remotePort": 80
    }
  ]
}
```

- `serverAddr`: Server address (required)
- `serverPort`: Server port, default `7000`
- `token` / `encryptKey`: Authentication and encryption keys, must match the server
- `heartbeatIntervalMs`: Heartbeat interval (ms), default `45000`
- `reconnectBaseDelayMs`: Reconnect base delay (ms), default `500`
- `reconnectMaxDelayMs`: Reconnect maximum delay (ms), default `60000`
- `proxies[]`: Proxy mapping rules list (proxy mode)
  - `name`: Rule name (unique identifier)
  - `localIP`: Local service IP, default `"127.0.0.1"`
  - `localPort`: Local service port
  - `remotePort`: Server-exposed port (`0` = random assignment)
- `rproxies[]`: Reverse proxy mapping rules list (rproxy mode)
  - `name`: Rule name (unique identifier)
  - `localPort`: Client listen port
  - `remoteIP`: Remote service IP (reachable from server)
  - `remotePort`: Remote service port

### 7.3 Command-Line Arguments

```
Server:
  natt server [-c server.json]

  -c string    Config file path (default "server.json")

Client:
  natt client [-c client.json] [-s host] [-p port]

  -c string    Config file path (default "client.json")
  -s string    Server address (overrides serverAddr in config)
  -p int       Server port (overrides serverPort in config)

Other:
  natt keygen                    # Generate AES-256 key
```

### 7.4 Usage Example

```bash
# 1. Build
./build.sh

# 2. Start the server on a public machine
./bin/natt server -c server.json

# 3. Start the client on an internal machine
./bin/natt client -c client.json

# 4. Access internal SSH through the public machine
ssh -oPort=6000 user@your-server.com
```

---

## 8. Build & Deployment

### 8.1 Build

Use `build.sh` for cross-platform compilation:

```bash
./build.sh          # Build for all platforms
```

Output binaries are placed in the `bin/` directory, named `natt-{os}-{arch}`.

Other auxiliary operations can be done with Go commands:

```bash
go test ./pkg/... -v -count=1 -timeout 60s          # Run unit tests
go test -tags=integration -v -count=1 -timeout 120s . # Run integration tests
go clean -cache                                      # Clean build cache
go fmt ./pkg/... ./cmd/...                           # Format code
go vet ./pkg/... ./cmd/...                           # Code check (static analysis)
```

### 8.2 Dependencies

- Go 1.26+ (as declared in go.mod)
- No external dependencies (standard library: `net`, `crypto/aes`, `crypto/cipher`, `crypto/rand`, `encoding/hex`)

### 8.3 Integration Test Plan

```
1. Start Server locally (bindAddr="127.0.0.1", bindPort=0 → random port)
2. Start Client to connect to the above Server
3. Client requests a proxy mapping (localIP="127.0.0.1", localPort=22, remotePort=0)
4. Server assigns remotePort (random port)
5. External connection to remotePort → verify data reaches client's local port 22
6. Disconnect Client → verify Server cleans up the proxy port
7. Client reconnects → verify re-registration and proxy restoration
```

---

## 9. Appendix

### 9.1 Pending Items

- TLS encryption support (V2 feature)
- UDP traversal support (V2 feature)
- HTTP/HTTPS reverse proxy support (V2 feature)
- Dashboard / Management API (V2 feature)
- P2P hole-punching mode (V2 feature)
