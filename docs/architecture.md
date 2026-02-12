# Architecture

```
┌──────────────────────────────────────────────────────────────────────────────────┐
│                               Developer Machine                                  │
│  ┌────────────────────────────────────────────────────────────────────────────┐  │
│  │                               Devcontainer                                 │  │
│  │                                                                            │  │
│  │  ┌─────────────┐    ┌──────────────────────────────────────────────────┐   │  │
│  │  │             │    │              bridge intercept                    │   │  │
│  │  │ Local Code  │    │                                                 │   │  │
│  │  │  Workspace  │    │  ┌──────────┐  ┌─────────────┐  ┌───────────┐  │   │  │
│  │  │             │    │  │   DNS    │  │ Transparent │  │    SSH    │  │   │  │
│  │  │             │    │  │  Server  │  │    Proxy    │  │   Proxy   │  │   │  │
│  │  │             │    │  │ :53      │  │ (iptables)  │  │          │  │   │  │
│  │  └──────┬──────┘    └──┴─────┬────┴──┴──────┬──────┴──┴─────┬────┴──┘   │  │
│  │         │                    │              │               │            │  │
│  │         │ file sync          │              │               │            │  │
│  │         │ (mutagen)          │ /etc/        │ TCP to        │            │  │
│  │         │                    │ resolv.conf  │ 10.128.0.0/16 │            │  │
│  └─────────┼────────────────────┼──────────────┼───────────────┼────────────┘  │
│            │                    │              │               │               │
└────────────┼────────────────────┼──────────────┼───────────────┼───────────────┘
             │                    │              │               │
             │                    │              │  WebSocket    │ WebSocket
             │                    │              │  (/tunnel)    │ (/ssh)
             │                    │              │               │
             ▼                    │              ▼               ▼
┌──────────────────────────────────────────────────────────────────────────────────┐
│                                Vercel Sandbox                                    │
│  ┌────────────────────────────────────────────────────────────────────────────┐  │
│  │                              bridge server                                 │  │
│  │                                                                            │  │
│  │  ┌───────────────┐    ┌──────────────────────────────────────────┐         │  │
│  │  │  SSH Server   │◀──▶│            WebSocket Server              │         │  │
│  │  │  (port 2222)  │    │                                          │         │  │
│  │  └───────────────┘    │  /health    /ssh    /tunnel              │         │  │
│  │                       │                     (relay between       │         │  │
│  │                       │                      client ◀▶ server)   │         │  │
│  │                       └──────────────────────┬───────────────────┘         │  │
│  └──────────────────────────────────────────────┼────────────────────────────┘  │
│          ▲                                      │                               │
│          │ file sync (mutagen)                  │ WebSocket                      │
│          │                                      ▼                               │
│  ┌───────┴───────┐                                                              │
│  │    Sandbox    │                                                              │
│  │   Workspace   │                                                              │
│  └───────────────┘                                                              │
└──────────────────────────────────────────────────────────────────────────────────┘
                                                  │
                                                  │ WebSocket
                                                  ▼
┌──────────────────────────────────────────────────────────────────────────────────┐
│                           Vercel Preview Deployment                               │
│  ┌────────────────────────────────────────────────────────────────────────────┐  │
│  │                            Dispatcher Service                              │  │
│  │                                                                            │  │
│  │  ┌─────────────────┐    ┌──────────────────┐    ┌──────────────────────┐  │  │
│  │  │  Incoming HTTP  │───▶│  Tunnel Client   │───▶│  Forward Response   │  │  │
│  │  │    Requests     │    │   (Singleton)    │    │   + Resolve DNS     │  │  │
│  │  └─────────────────┘    └──────────────────┘    └──────────────────────┘  │  │
│  └────────────────────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────────────────────┘
                                                  ▲
                                                  │
                                                  │ HTTPS
                                                  │
                                         ┌────────┴────────┐
                                         │   End Users /   │
                                         │   API Clients   │
                                         └─────────────────┘
```

### Component Overview

| Component              | Description                                                           |
|------------------------|-----------------------------------------------------------------------|
| **bridge CLI**         | Go CLI with `intercept` and `server` commands                         |
| **bridge intercept**   | Runs in devcontainer: DNS server, transparent proxy, SSH proxy        |
| **bridge server**      | Runs in sandbox: SSH server + WebSocket relay (`/health /ssh /tunnel`)|
| **Dispatcher**         | Service on Preview deployment that tunnels requests and resolves DNS  |
| **Mutagen**            | File synchronization between devcontainer and sandbox workspaces      |

### Data Flow

1. **File Sync**: Local workspace ←→ Sandbox workspace via SSH + Mutagen
2. **SSH Access**: Developer SSH → SSH Proxy → WebSocket `/ssh` → Sandbox SSH Server
3. **Inbound Requests**: End User → Dispatcher → Tunnel → Sandbox → Devcontainer app
4. **Outbound DNS**: App DNS query → Bridge DNS → matched: tunnel to dispatcher / unmatched: Docker DNS
5. **Outbound TCP**: App connects to proxy IP → iptables redirect → Transparent Proxy → Tunnel → Dispatcher

## Order of operations

1. User runs `vc connect --dev`
2. `vc` spins up a Preview deployment using the same env vars as your current project but is actually running
   the [dispatcher service](services/dispatcher).
3. `vc` spins up a Sandbox for the development session.
4. `vc` spins up a Devcontainer locally but with the `bridge` CLI injected as a Devcontainer feature.
5. `bridge intercept` is run in the Devcontainer feature which:

Connects to the `bridge` server on the Sandbox:

* SSH: Connects to the `bridge` server on `/ssh` and starts a local TCP proxy to allow for SSH
  connections within the container
* Tunnel: Forwards all intercepted egress L4 traffic down the websocket created on the `/tunnel` endpoint of the bridge
  server
  * `bridge` server then forwards all traffic to the Dispatcher preview deployment who then makes the call.

Starts [mutagen](https://mutagen.io/) to sync the Devcontainer workspace to the Sandbox workspace and vice versa via the
above SSH connection to the Sandbox.

## Tunnel Protocol

The `/tunnel` WebSocket endpoint on the bridge server enables bidirectional communication between the local client
(in the Devcontainer) and the dispatcher (on the Preview deployment). The bridge server acts as a relay, pairing
client and server connections.

### Connection Flow

```
┌─────────────┐                    ┌───────────────┐                    ┌────────────┐
│   Client    │                    │ Bridge Server │                    │ Dispatcher │
│ (Devcontainer)                   │   (Sandbox)   │                    │ (Preview)  │
└──────┬──────┘                    └───────┬───────┘                    └─────┬──────┘
       │                                   │                                  │
       │ 1. WebSocket connect /tunnel      │                                  │
       │──────────────────────────────────▶│                                  │
       │                                   │                                  │
       │ 2. Send registration              │                                  │
       │    {type: "client",               │                                  │
       │     function_url: "..."}          │                                  │
       │──────────────────────────────────▶│                                  │
       │                                   │                                  │
       │                                   │ 3. POST /__tunnel/connect        │
       │                                   │    Body: ServerConnection (JSON) │
       │                                   │─────────────────────────────────▶│
       │                                   │                                  │
       │                                   │        4. Validate function_url  │
       │                                   │           (must match regexp)    │
       │                                   │                                  │
       │                                   │ 5. WebSocket connect /tunnel     │
       │                                   │◀─────────────────────────────────│
       │                                   │                                  │
       │                                   │ 6. Send registration             │
       │                                   │    {type: "server"}              │
       │                                   │◀─────────────────────────────────│
       │                                   │                                  │
       │ 7. Bridge pipes connections       │                                  │
       │◀─────────────────────────────────▶│◀────────────────────────────────▶│
       │                                   │                                  │
```

### Bridge Server Implementation

1. Accept WebSocket connection on `/tunnel`
2. Wait for registration message (30s timeout)
3. If registration is `type: "client"`:

- Extract `function_url` from registration
- POST to `{function_url}/__tunnel/connect` with `ServerConnection` protobuf as JSON payload
- Hold connection waiting for matching server connection

4. If registration is `type: "server"`:

- Match with waiting client connection
- Begin piping data bidirectionally between client and server

5. On timeout or error, clean up connection

## DNS Interception

When `--forward-domains` is configured, `bridge intercept` runs a local DNS server that intercepts matching queries and routes them through the tunnel. This allows the devcontainer to reach services that only resolve from the dispatcher's network.

DNS interception is done by prepending the bridge DNS server to `/etc/resolv.conf`. No iptables rules are needed for DNS — iptables are only used for TCP redirect of proxy CIDR traffic.

### Components

```
┌─────────────────────────────────────────────────────────────────────────┐
│                            Devcontainer                                 │
│                                                                         │
│  /etc/resolv.conf                                                       │
│  ┌──────────────────────────┐                                           │
│  │ nameserver 127.0.0.1     │ ◀── bridge DNS (added at startup)        │
│  │ nameserver 127.0.0.11    │ ◀── original Docker DNS (kept)           │
│  └──────────────────────────┘                                           │
│                                                                         │
│  ┌───────────────────────────────────────────────────────────────────┐  │
│  │                      Bridge DNS Server (127.0.0.1:53)             │  │
│  │                                                                    │  │
│  │  ┌────────────────────────┐     ┌───────────────────────────────┐ │  │
│  │  │  TunnelExchangeClient  │     │    SystemExchangeClient       │ │  │
│  │  │                        │     │                               │ │  │
│  │  │  Matched patterns:     │     │  Everything else:             │ │  │
│  │  │  resolve via tunnel,   │     │  forward to original NS,     │ │  │
│  │  │  return intercepted=   │     │  return intercepted=false     │ │  │
│  │  │  true                  │     │                               │ │  │
│  │  └───────────┬────────────┘     └──────────────┬────────────────┘ │  │
│  └──────────────┼─────────────────────────────────┼──────────────────┘  │
│                 │                                  │                     │
│                 │ WebSocket tunnel                 │ UDP to 127.0.0.11   │
│                 ▼                                  ▼                     │
│  ┌──────────────────────┐             ┌──────────────────────────────┐  │
│  │    Tunnel Client     │             │  Docker Embedded DNS         │  │
│  └──────────┬───────────┘             │  (resolves container names   │  │
│             │                         │   and external domains)      │  │
│             │                         └──────────────────────────────┘  │
└─────────────┼──────────────────────────────────────────────────────────┘
              │ WebSocket
              ▼
     ┌─────────────────┐
     │  Bridge Server   │
     │   (Sandbox)      │──▶ Dispatcher resolves DNS
     └─────────────────┘
```

### Matched Domain Query

An app queries a hostname that matches a `--forward-domains` pattern (e.g., `*.example.com`):

```
App                Bridge DNS           Tunnel             Dispatcher
 │                 (127.0.0.1:53)       Exchange           (resolves DNS)
 │                  │                    │                   │
 │ A? api.example.com                   │                   │
 │─────────────────▶│                    │                   │
 │                  │                    │                   │
 │                  │ ExchangeContext()   │                   │
 │                  │───────────────────▶│                   │
 │                  │                    │                   │
 │                  │                    │ pattern match     │
 │                  │                    │                   │
 │                  │                    │ ResolveDNS("api.example.com")
 │                  │                    │──── WebSocket ───▶│
 │                  │                    │                   │ resolve
 │                  │                    │◀── 93.184.1.1 ───│
 │                  │                    │                   │
 │                  │ intercepted=true    │                   │
 │                  │ A: 93.184.1.1      │                   │
 │                  │◀───────────────────│                   │
 │                  │                    │                   │
 │                  │ Registry.Register("api.example.com", 93.184.1.1)
 │                  │ → proxy IP 10.128.0.1
 │                  │
 │ A: 10.128.0.1    │
 │◀─────────────────│
```

The app connects to `10.128.0.1`. Iptables redirects TCP destined for `10.128.0.0/16` to the transparent proxy:

```
App              iptables           Proxy              Registry           Tunnel
 │                (nat)              │                   │                  │
 │ connect 10.128.0.1:443           │                   │                  │
 │───────────────▶│                  │                   │                  │
 │                │ REDIRECT         │                   │                  │
 │                │─────────────────▶│                   │                  │
 │                │                  │                   │                  │
 │                │                  │ getOriginalDst()  │                  │
 │                │                  │ → 10.128.0.1:443  │                  │
 │                │                  │                   │                  │
 │                │                  │ Lookup(10.128.0.1) │                  │
 │                │                  │──────────────────▶│                  │
 │                │                  │                   │                  │
 │                │                  │ api.example.com   │                  │
 │                │                  │◀──────────────────│                  │
 │                │                  │                   │                  │
 │                │                  │ DialThroughTunnel("api.example.com:443")
 │                │                  │─────────────────────────────────────▶│
 │                │                  │                   │                  │
 │◀═════════════════════════════════▶│◀═══════════════════════════════════▶│
 │          bidirectional copy       │            WebSocket tunnel          │
```

### Unmatched Domain Query

An app queries a hostname that does not match any `--forward-domains` pattern:

```
App                Bridge DNS           System             Docker DNS
 │                 (127.0.0.1:53)       Exchange           (127.0.0.11)
 │                  │                    │                   │
 │ A? google.com    │                    │                   │
 │─────────────────▶│                    │                   │
 │                  │                    │                   │
 │                  │ ExchangeContext()   │                   │
 │                  │───────────────────▶│                   │
 │                  │                    │                   │
 │                  │                    │ no match          │
 │                  │                    │ → fallback        │
 │                  │                    │                   │
 │                  │                    │ UDP query         │
 │                  │                    │──────────────────▶│
 │                  │                    │                   │
 │                  │                    │ A: 142.250.x.x    │
 │                  │                    │◀──────────────────│
 │                  │                    │                   │
 │                  │ intercepted=false   │                   │
 │                  │◀───────────────────│                   │
 │                  │
 │                  │ pass through as-is (no proxy IP)
 │                  │
 │ A: 142.250.x.x  │
 │◀─────────────────│
 │
 │ connect directly (not in proxy CIDR, no iptables intercept)
```
