# Bridge Architecture

Bridge enables running Kubernetes services locally by creating an isolated namespace in a non-prod cluster, cloning a target deployment, and tunneling all traffic between the local devcontainer and the cluster over a single gRPC stream.

## High-Level Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│  Developer Machine                                                  │
│                                                                     │
│  ┌───────────────────────────────────────────────────────────────┐  │
│  │  Devcontainer (Docker)                                        │  │
│  │                                                               │  │
│  │  ┌──────────┐   ┌──────────┐   ┌──────────┐                  │  │
│  │  │ Local App│   │ DNS (53) │   │ iptables │                  │  │
│  │  │ (:3000)  │   │ intercept│   │ redirect │                  │  │
│  │  └────▲─────┘   └────┬─────┘   └────┬─────┘                  │  │
│  │       │              │              │                          │  │
│  │       │         ┌────▼──────────────▼────┐                    │  │
│  │       │         │  bridge intercept       │                    │  │
│  │       │         │  (transparent proxy +   │                    │  │
│  │       │         │   connection tracking)  │                    │  │
│  │       │         └────────────┬────────────┘                    │  │
│  │       │                     │                                  │  │
│  │       │          gRPC bidi stream (TunnelNetwork)              │  │
│  │       │                     │                                  │  │
│  └───────┼─────────────────────┼─────────────────────────────────┘  │
│          │                     │                                    │
│          │          kubectl port-forward (k8spf://)                 │
│          │                     │                                    │
└──────────┼─────────────────────┼────────────────────────────────────┘
           │                     │
┌──────────┼─────────────────────┼────────────────────────────────────┐
│  Kubernetes Cluster            │                                    │
│                                │                                    │
│  ┌─────────────────────────────▼──────────────────────┐             │
│  │  bridge-<device-id> namespace                       │             │
│  │                                                     │             │
│  │  ┌──────────────────────────────────────────┐       │             │
│  │  │  bridge-<deployment> pod                  │       │             │
│  │  │  ┌───────────────┐  ┌──────────────────┐ │       │             │
│  │  │  │ bridge server │  │ original sidecars│ │       │             │
│  │  │  │ (gRPC :9090)  │  │ (if any)         │ │       │             │
│  │  │  └───────┬───────┘  └──────────────────┘ │       │             │
│  │  │          │ dials real destinations         │       │             │
│  │  └──────────┼────────────────────────────────┘       │             │
│  │             │                                        │             │
│  └─────────────┼────────────────────────────────────────┘             │
│                │                                                      │
│  ┌─────────────▼──────────────────────┐                               │
│  │  Other namespaces / cluster DNS    │                               │
│  │  (redis.prod, api.staging, etc.)   │                               │
│  └────────────────────────────────────┘                               │
│                                                                       │
│  ┌────────────────────────────────────┐                               │
│  │  bridge namespace                  │                               │
│  │  ┌──────────────────────────────┐  │                               │
│  │  │  administrator (gRPC :9090)  │  │                               │
│  │  │  manages bridge lifecycle    │  │                               │
│  │  └──────────────────────────────┘  │                               │
│  └────────────────────────────────────┘                               │
└───────────────────────────────────────────────────────────────────────┘
```

## Components

### CLI Commands

| Command | Where it runs | Purpose |
|---------|---------------|---------|
| `bridge create` | Developer machine | Provisions a bridge namespace, clones the target deployment, generates devcontainer config |
| `bridge intercept` | Inside devcontainer | Runs DNS server, transparent proxy, and gRPC tunnel back to the cluster |
| `bridge server` | In-cluster (bridge pod) | gRPC proxy that accepts tunnel connections and dials real destinations |
| `bridge administrator` | In-cluster (bridge namespace) | Manages bridge lifecycle: create, list, delete |

### Administrator

The administrator runs as a long-lived deployment in the `bridge` namespace. It exposes a gRPC service (`BridgeAdministratorService`) with three RPCs:

- **CreateBridge** — Creates a device-scoped namespace (`bridge-<device-id-prefix>`), clones the target deployment with its ConfigMaps and Secrets, replaces the app container with the bridge proxy, creates a Service, and waits for the pod to become ready.
- **ListBridges** — Returns all bridges for a given device ID by querying namespace labels.
- **DeleteBridge** — Tears down a specific bridge deployment or the entire namespace.

The administrator sets up RBAC in each bridge namespace so it can manage resources there.

### Bridge Proxy (Server)

The bridge proxy runs as the main container in the cloned deployment pod. It:

1. Opens TCP listeners for the deployment's original ports (ingress)
2. Serves `BridgeProxyService` gRPC on port 9090
3. Accepts a `TunnelNetwork` bidirectional stream from the devcontainer
4. Handles `ResolveDNSQuery` RPCs — resolves hostnames using the cluster's DNS
5. Serves `BridgeFileSystemService` on the same port — a read-only view of the source container's volume mount paths, consumed by the devcontainer's FUSE client (see [Volume Mount Forwarding](#volume-mount-forwarding))

When an ingress connection arrives on a listen port, it's multiplexed through the tunnel to the devcontainer, which dials `localhost:3000` (or the configured app port) to reach the local app.

When the devcontainer sends egress traffic through the tunnel, the proxy dials the real destination within the cluster network.

### Intercept (Client)

`bridge intercept` runs inside the devcontainer and orchestrates:

1. **gRPC tunnel** — Opens a `TunnelNetwork` stream to the bridge proxy via `k8spf://` (kubectl port-forward)
2. **DNS server** — Listens on port 53, intercepts queries matching `--forward-domains` patterns
3. **resolv.conf** — Prepends `nameserver 127.0.0.1` so all DNS goes through the bridge
4. **Transparent proxy** — Listens on a random port, accepts redirected TCP connections
5. **iptables** — Redirects traffic destined for proxy CIDR IPs to the transparent proxy

---

## Traffic Flows

### Egress: Local App Reaches a Cluster Service

When the local app resolves a hostname like `redis.production.svc.cluster.local`:

```
App calls getaddrinfo("redis.production.svc.cluster.local")
  │
  ▼
DNS query → 127.0.0.1:53 (bridge DNS server)
  │
  │  Pattern matches --forward-domains (e.g., "*.production.svc.cluster.local" or "*")
  │
  ▼
gRPC ResolveDNSQuery → bridge proxy (in-cluster)
  │
  │  Cluster DNS resolves to real IP (e.g., 10.43.0.15)
  │
  ▼
DNS server allocates proxy IP from pool (e.g., 10.128.0.1)
  │
  │  Registers mapping: 10.128.0.1 → redis.production.svc.cluster.local / 10.43.0.15
  │
  ▼
DNS response returns 10.128.0.1 to the app
  │
  ▼
App connects to 10.128.0.1:6379
  │
  │  iptables REDIRECT → transparent proxy port
  │
  ▼
Transparent proxy reads SO_ORIGINAL_DST → 10.128.0.1:6379
  │
  │  Looks up 10.128.0.1 in connection registry → real dest: 10.43.0.15:6379
  │
  ▼
Tunnel multiplexes connection over gRPC stream
  │
  ▼
Bridge proxy dials 10.43.0.15:6379 inside cluster
  │
  ▼
Data flows bidirectionally through the tunnel
```

### Ingress: External Request Reaches Local App

When a request arrives at the bridge pod's listen port:

```
Cluster traffic → bridge pod port 8080
  │
  ▼
Bridge proxy ingress listener accepts connection
  │
  │  Assigns connection ID, sends through tunnel
  │
  ▼
Tunnel delivers to devcontainer
  │
  │  StaticPortDialer dials localhost:3000
  │
  ▼
Local app handles request, response flows back through tunnel
```

---

## DNS Interception

The DNS server intercepts queries that match configured domain patterns and tunnels them to the cluster for resolution. Non-matching queries fall through to the original system resolver.

**Pattern matching:**

| Pattern | Matches | Does not match |
|---------|---------|----------------|
| `*` | Everything | — |
| `*.example.com` | `foo.example.com` | `foo.bar.example.com` |
| `**.example.com` | `foo.bar.example.com` | — |

**AAAA handling:** For matched domains, AAAA queries return an empty `NOERROR` response. This prevents musl-based resolvers from discarding the A record result when the AAAA query returns `NXDOMAIN`.

**Cleanup:** A background goroutine releases unused proxy IP allocations after 10 seconds. This prevents the IP pool from being exhausted by DNS queries that never result in a TCP connection (e.g., `dig` or `nslookup`).

---

## Connection Tracking

The connection registry maps proxy IPs (from the `10.128.0.0/16` pool) to real destinations:

1. **DNS resolution** — Allocates a proxy IP and registers the mapping
2. **TCP connect** — iptables redirects the connection; the transparent proxy looks up the original destination via `SO_ORIGINAL_DST`, then finds the real target in the registry
3. **Cleanup** — Entries unused after 10 seconds are released back to the pool

---

## Volume Mount Forwarding

The proxy pod inherits the source deployment's `VolumeMounts` — ConfigMaps, Secrets, projected volumes (IRSA tokens, workload identity), and any other Kubernetes-managed volumes the application relied on. Those paths are made visible inside the devcontainer at the same absolute locations via FUSE.

```
Local app reads /var/run/secrets/.../token
  │
  ▼
Kernel routes the open/read to the bridge FUSE mount at /var/run/secrets/...
  │
  ▼
FUSE client issues Stat / ReadDir / ReadFile RPC over gRPC
  │
  ▼
BridgeFileSystemService on the proxy pod authorizes the path against the
allowlist of mount roots (the source container's VolumeMounts) and reads the
real file from the pod filesystem
  │
  ▼
Bytes flow back to the kernel, satisfying the app's read
```

**Properties:**

- **Read-only.** Most mount roots in the pod (ConfigMap/Secret/projected) are read-only at the kernel level anyway. Writes return `EROFS`.
- **Live.** Reads always hit the proxy pod's current filesystem, so rotated tokens and updated ConfigMap content appear within the FUSE attribute TTL (5 s) without restarting the bridge.
- **Allowlisted.** The server only serves paths under one of the `--mount-roots` it was configured with at startup. Requests for arbitrary pod paths return `PermissionDenied`.
- **Chunked.** Large files are split into 1 MiB reads on the wire — well below gRPC's default message limit.

The administrator derives `--mount-roots` from the original container's `VolumeMounts` and bakes it into the proxy `Command`. The same paths flow back to the CLI via `CreateBridgeResponse.volume_mount_paths` and are passed to the devcontainer feature as the `mountPaths` option. `bridge intercept` runs one FUSE mount per path; failures are logged but non-fatal so network interception still works on systems without `/dev/fuse`.

**Container requirements:** the devcontainer needs `--cap-add SYS_ADMIN`, `--device /dev/fuse`, and (on AppArmor systems) `--security-opt apparmor:unconfined`. The feature install script also installs `fuse3` when not already present.

---

## Tunnel Multiplexing

All traffic between the devcontainer and the cluster flows over a single gRPC bidirectional stream (`TunnelNetwork`). Connections are multiplexed using deterministic connection IDs of the form `<src-ip>:<src-port>-><dst-ip>:<dst-port>`.

The tunnel has:
- A **send pump** that drains a buffered channel onto the gRPC stream
- A **recv pump** that reads from the stream and routes messages by connection ID
- A concurrent map tracking active connections
- Automatic dial-on-first-message for new inbound connection IDs

---

## Kubernetes Port-Forward (k8spf)

Bridge uses a custom gRPC resolver scheme (`k8spf://`) to transparently connect to pods via kubectl port-forward without requiring direct network access to the cluster.

**Address format:**
```
k8spf:///name.namespace:port[?workload=deployment][&context=ctx]
```

When `workload=deployment`, the resolver looks up the deployment, finds a running pod via its label selector, and port-forwards to that pod. This handles pod restarts automatically since each gRPC dial re-resolves the deployment.

The resolver maintains a connection pool of SPDY port-forward sessions for efficiency.

---

## Bridge Creation Flow

```
$ bridge create my-api -n staging --connect

1. Read device ID from ~/.bridge/device-id.txt (auto-generated KSUID)

2. Connect to administrator via k8spf:///administrator.bridge:9090?workload=deployment

3. CreateBridge RPC:
   a. Create namespace: bridge-<device-id-prefix>
   b. Copy deployment "my-api" from "staging" namespace
   c. Extract referenced ConfigMaps and Secrets → consolidated copies
   d. Replace app container with bridge proxy image
   e. Preserve sidecar containers
   f. Create Service for cluster access
   g. Wait for pod ready

4. Generate .devcontainer/bridge-my-api/devcontainer.json:
   - Image: base devcontainer image
   - Feature: bridge with options (serverAddr, appPort, forwardDomains)
   - Capabilities: NET_ADMIN
   - Mounts: KUBECONFIG (localhost rewritten to host.docker.internal)

5. Start devcontainer (--connect flag)
   a. Docker builds image with bridge feature
   b. Feature install.sh: installs bridge binary, iptables, creates entrypoint
   c. Entrypoint launches "bridge intercept" as root (backgrounded)
   d. Intercept establishes tunnel, DNS, iptables
   e. Developer gets a shell with full cluster network access
```

---

## Devcontainer Feature

The bridge devcontainer feature (`features/bridge/`) is installed during container build. It:

1. Installs `curl` and `iptables`
2. Downloads the bridge binary (or uses a bind-mounted dev binary)
3. Writes environment config to `/etc/profile.d/bridge.sh`
4. Creates an entrypoint script that launches `bridge intercept` on container start

The entrypoint uses a split heredoc pattern: the unquoted section bakes in the workspace path at install time, while the quoted section contains runtime logic. This works around the limitation that `${containerWorkspaceFolder}` is not available as an environment variable at runtime.

**Feature options:**

| Option | Description | Default |
|--------|-------------|---------|
| `bridgeServerAddr` | k8spf address of the bridge proxy | — |
| `appPort` | Local application port | 3000 |
| `forwardDomains` | Domain patterns to intercept | — |
| `mountPaths` | Comma-separated paths to FUSE-mount from the proxy pod (defaults to whatever the server advertises) | — |
| `bridgeVersion` | Version to install (dev/latest/tag) | latest |
| `workspacePath` | Container workspace path | — |

---

## Device Identity

Each developer machine gets a unique identity stored at `~/.bridge/device-id.txt` (a KSUID). The first 6 characters are used to derive the bridge namespace name (`bridge-<prefix>`), keeping namespaces short while avoiding collisions across team members.

---

## Key Defaults

| Setting | Value |
|---------|-------|
| Proxy CIDR | `10.128.0.0/16` (~65K addresses) |
| Bridge proxy gRPC port | 9090 |
| Local app port | 3000 |
| DNS listen port | 53 |
| DNS cleanup interval | 5 seconds |
| Unused entry timeout | 10 seconds |
| Tunnel send buffer | 64 messages |
