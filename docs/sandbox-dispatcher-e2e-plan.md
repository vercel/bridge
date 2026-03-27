# Sandbox Dispatcher E2E Plan

## Goal

Prove this end-to-end flow:

1. A Vercel preview deployment runs `flow/dispatcher`.
2. A sandbox runs `bridge intercept`.
3. The interceptor wakes the dispatcher with `POST /__tunnel/connect`.
4. The dispatcher connects back to the interceptor over WebSocket at `GET /__tunnel/connect`.
5. Preview ingress is forwarded through the dispatcher to the sandbox app.
6. Sandbox egress flows through the interceptor SOCKS proxy and back out through the dispatcher.

## Current Implementation State

### Transport shape

The old “dispatcher exposes BridgeProxyService over ConnectRPC” direction was abandoned.

Current model:

- the dispatcher is a plain HTTP service
- the interceptor uses plain HTTP to wake it
- the actual tunnel is a WebSocket initiated by the dispatcher back to the interceptor

The relevant endpoints are now:

- dispatcher:
  - `POST /__tunnel/connect`
  - `GET /__tunnel/status`
  - normal forwarded HTTP requests
- interceptor:
  - `GET /__tunnel/connect` as a WebSocket upgrade endpoint

### What was changed in Go

Implemented:

- `pkg/proxy/client.go`
  - the HTTP proxy client no longer uses ConnectRPC for the dispatcher path
  - `GetMetadata` returns an empty response
  - `CopyFiles` returns an empty response
  - `ResolveDNS` returns an empty result
  - `OpenTunnel()` now creates a stream that wakes the dispatcher and then waits for an inbound WebSocket from the interceptor server
- `pkg/tunnel/tunnel.go`
  - `Stream` now has `Refresh(context.Context) error`
  - tunnel send/recv paths retry by calling `Refresh()` when the underlying stream dies
- `pkg/tunnel/stream_ws.go`
  - added reconnect support for WebSocket streams
  - added `NewWakeableWSStream(...)` so the stream itself owns the wake-and-wait protocol
- `pkg/commands/intercept.go`
  - interceptor starts its local tunnel acceptor before opening the proxy client
  - the HTTP proxy client receives the accepted-connection channel from the interceptor server
- `pkg/commands/intercept_health.go`
  - despite the filename, this is now just the interceptor-side HTTP/WebSocket acceptor for `GET /__tunnel/connect`
  - it upgrades inbound dispatcher WebSockets and passes accepted sockets to the stream through a channel

Removed / intentionally avoided:

- no `Connector` interface
- no dispatcher-side unary BridgeProxyService implementation
- no ConnectRPC dependency in the dispatcher HTTP path

### What was changed in dispatcher

Implemented:

- `flow/dispatcher/src/common-handler.ts`
  - `POST /__tunnel/connect` wakes the singleton tunnel client
  - `GET /__tunnel/status` reports whether the tunnel is connected
  - all other HTTP requests are forwarded through the tunnel client
- `flow/dispatcher/src/tunnel.ts`
  - the dispatcher unmarshals tunnel frames with protobuf via `fromBinary(TunnelNetworkMessageSchema, ...)`
  - outbound tunnel frames are encoded with protobuf via `toBinary(...)`
  - the dispatcher WebSocket now connects to `.../__tunnel/connect` on the interceptor, not `/bridge.v1.BridgeProxyService/TunnelNetwork`
  - incoming tunnel frames are handled in 3 buckets:
    - connection error / close
    - response chunks for pending forwarded HTTP requests
    - outbound traffic from the sandbox app, which the dispatcher handles by opening a real TCP socket to the requested destination and proxying bytes back over the tunnel

## E2E Test Status

### Added

A first-pass test suite was added in:

- `e2esandbox/dispatcher_test.go`

The draft test currently does this:

1. creates a sandbox with port `8081` exposed for the interceptor
2. builds and uploads the Linux `bridge` binary
3. creates a Vercel preview deployment for the linked `flow/dispatcher` project
4. sets `BRIDGE_SERVER_ADDR` for the dispatcher deployment to the sandbox interceptor route
5. starts `bridge intercept` in SOCKS mode inside the sandbox
6. clones `vercel/bridge` inside the sandbox
7. installs `flow/userservice`
8. starts `vercel dev` for `userservice` on port `3000`
9. tries preview ingress with `GET /api/health`
10. tries sandbox-side `curl` through `socks5h://127.0.0.1:1080`

### Validation done

Only a compile-level check was run:

```sh
env GOCACHE=/tmp/bridge-gocache GOMODCACHE=/tmp/bridge-gomodcache \
  go test ./e2esandbox -run TestDispatcherSuite -count=0
```

No live sandbox/Vercel end-to-end run has been completed yet.

## Known Gaps

### Preview protection bypass

The test currently looks for:

- `VERCEL_AUTOMATION_BYPASS_SECRET`

Current state:

- this secret is not checked into the repo
- it was not present in the current shell environment when the test was drafted

Impact:

- if the preview deployment is protected, ingress verification may skip or fail until this secret is supplied at runtime

### Userservice runtime assumptions

The test currently assumes the sandbox image can run all of:

- `git`
- `node`
- `npm`
- `npx`
- `vercel dev`

If that turns out false, the likely fix is to replace `vercel dev` with a simpler local server process.

### Dispatcher deployment assumptions

The draft test currently uses:

- the linked local Vercel project file at `flow/dispatcher/.vercel/project.json`
- the current git remote / branch / SHA

That means it will skip or fail if:

- the local project is not linked
- the repo remote is not `vercel/bridge`
- the linked Vercel project or org is inaccessible to the current token

### HTTP no-op unary methods

The HTTP proxy client intentionally returns empty results for:

- `GetMetadata`
- `CopyFiles`
- `ResolveDNS`

This matches the current simplified dispatcher path, but it means the HTTP dispatcher path does not currently provide real metadata, file copy, or DNS behavior.

## Remaining TODOs

1. Run the new `DispatcherSuite` against real Sandbox and Vercel infrastructure.
2. Confirm the preview deployment can reach the sandbox interceptor route set in `BRIDGE_SERVER_ADDR`.
3. Confirm `bridge intercept --server-addr <preview-url>` successfully wakes the dispatcher and receives the inbound dispatcher WebSocket on `GET /__tunnel/connect`.
4. Verify that forwarded preview ingress actually reaches the sandbox app on port `3000`.
5. Verify that sandbox-side egress through `socks5h://127.0.0.1:1080` works end to end through the dispatcher.
6. Provide the runtime preview-protection bypass secret through `VERCEL_AUTOMATION_BYPASS_SECRET`, or decide to hard-fail on protected previews instead of skipping.
7. Decide whether `userservice` should keep using `vercel dev` in the sandbox or be replaced with a smaller purpose-built local HTTP server.
8. Decide whether the HTTP path should keep empty no-op unary methods or whether the dispatcher needs real metadata / file-copy / DNS behavior later.
9. Clean up naming: `pkg/commands/intercept_health.go` no longer primarily represents gRPC health behavior.

## Likely First Runtime Failure Points

Most likely places this test breaks first:

1. preview deployment protection blocks `/api/health`
2. the dispatcher cannot dial back to the sandbox interceptor route
3. `vercel dev` is too heavy or unavailable in the sandbox
4. ingress forwarding does not target the expected sandbox app port
5. SOCKS egress works differently than the current test assumes

## Current Conclusion

The architecture is now aligned with the intended flow:

- interceptor wakes dispatcher via HTTP
- dispatcher connects back to interceptor via WebSocket
- stream refresh owns reconnect behavior
- dispatcher forwards ingress and outbound traffic using protobuf-framed tunnel messages

What remains is runtime validation and cleanup, not another transport rewrite.
