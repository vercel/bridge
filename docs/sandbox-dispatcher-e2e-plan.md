# Sandbox Dispatcher E2E Plan

## Goal

Create an end-to-end sandbox test that proves this flow:

1. A bridge proxy server runs inside a Vercel Sandbox.
2. A Vercel preview deployment runs the dispatcher code from this repo.
3. The sandbox interceptor connects to that preview deployment.
4. A `userservice` process runs inside the sandbox behind the bridge/interceptor.
5. Ingress to the preview deployment reaches the sandbox app.
6. Outbound traffic from the sandbox app can flow through the interceptor SOCKS proxy.

## Current Building Blocks

- `pkg/flow/sandbox` already creates sandboxes, runs commands, streams logs, and exposes sandbox routes.
- `pkg/flow/vercel` now resolves the Vercel token and can create / poll deployments.
- `flow/dispatcher` already has a Vercel function entrypoint and tunnel logic.
- `flow/userservice` is a minimal Vercel API app suitable for ingress validation.
- `bridge intercept` now has a path to support HTTP proxy targets by reusing `pkg/proxy.NewHTTPProxyClient`.
- the Go bridge proxy server already exposes BridgeProxyService over Connect RPC for unary methods.

## Planned Test Shape

### 1. Add a new e2e suite

Create a new sandbox e2e suite, likely in `e2esandbox/dispatcher_test.go`, with one main test that owns the full flow.

Suggested suite state:

- sandbox client
- vercel client
- sandbox ID
- sandbox public routes
- preview deployment metadata
- built linux bridge binary path

### 2. Resolve local project metadata

Read the local Vercel project link for `flow/userservice/.vercel/project.json` to get:

- target Vercel project ID
- owning team ID
- project name

Resolve Git source metadata from the local repo:

- remote origin repo: `vercel/bridge`
- current branch/ref
- current commit SHA

Use `gitSource` in the deployment body rather than inlined files.

### 3. Provision the sandbox

Create a sandbox with:

- runtime: Node 24
- timeout: at least 10 minutes
- exposed port for the bridge HTTP server

Initial assumption:

- expose port `9090` for the bridge HTTP server, because the dispatcher needs a reachable `BRIDGE_SERVER_ADDR`

### 4. Build and upload bridge

Reuse the existing linux build pattern from the sandbox e2e tests:

- `GOOS=linux`
- `GOARCH=amd64`
- `CGO_ENABLED=0`

Upload the binary into the sandbox and `chmod +x` it.

### 5. Start the sandbox bridge proxy server

Start `bridge server` inside the sandbox in HTTP mode on the exposed port.

Current expected command shape:

```sh
/vercel/sandbox/bridge server --protocol http --addr :9090
```

This server is the target for the dispatcher’s `BRIDGE_SERVER_ADDR`.

### 6. Create the preview deployment

Create a preview deployment against the `userservice` Vercel project using:

- `gitSource` pointing at `vercel/bridge`
- `projectSettings.rootDirectory = "flow/dispatcher"`
- dispatcher-compatible project settings (`framework`, `buildCommand`, `installCommand`, node version)
- `env.BRIDGE_SERVER_ADDR = <sandbox public route for 9090>`

Wait for the deployment to reach a terminal ready state and fail fast with the Vercel error fields if the build fails.

### 7. Start the sandbox interceptor

Start `bridge intercept` inside the sandbox in SOCKS mode.

Target:

- `--server-addr <preview deployment URL>`

Other expected flags:

- `--intercept-mode socks`
- `--socks-addr :1080`
- `--app-port 3000`
- `--addr :8081`

This assumes the interceptor can talk to the preview deployment over the HTTP proxy client path.

Current constraint:

- Vercel Functions can do WebSocket egress
- Vercel Functions cannot act as the inbound WebSocket server endpoint for the interceptor-facing tunnel path

So the dispatcher work should start with the unary BridgeProxyService API surface first.

### 8. Clone the repo inside the sandbox

Clone `https://github.com/vercel/bridge.git` into the sandbox and start the local `userservice`.

Expected flow:

1. clone repo into a writable sandbox directory
2. `cd flow/userservice`
3. install deps
4. run `vercel dev` on port `3000`

### 9. Start userservice with SOCKS env

Run `userservice` with outbound proxy env configured to the local interceptor:

- `ALL_PROXY=socks5://127.0.0.1:1080`
- `HTTP_PROXY=socks5://127.0.0.1:1080`
- `HTTPS_PROXY=socks5://127.0.0.1:1080`
- `NO_PROXY=127.0.0.1,localhost,::1`

Wait until `http://127.0.0.1:3000/api/health` is healthy inside the sandbox.

### 10. Verify ingress

Send a request to the preview deployment and assert it reaches `userservice`.

Primary check:

- `GET /api/health`

Possible implementation options:

- plain HTTP request if the preview URL is publicly reachable
- Vercel-authenticated request if preview protection is enabled

### 11. Verify sandbox-side curl

From inside the sandbox, run a `curl` that exercises the configured flow and assert success.

Candidate checks:

- `curl` to the preview deployment URL
- `curl` to a public URL with SOCKS env set, to prove outbound proxying

The first option is preferable if preview protection can be bypassed cleanly from test code.

## Known Risks / Open Questions

### Preview protection

Preview deployments may be behind Vercel Authentication.

Current state:

- a preview protection bypass secret is available for the test run
- do not commit the raw secret into the repository
- inject it at runtime through env or test-only configuration

Use the bypass secret for:

- local ingress verification against the preview deployment
- sandbox-side `curl` when the sandbox needs to hit the protected preview URL

Without this, ingress verification can fail even when deployment setup is correct.

### Dispatcher upstream destination

The dispatcher currently knows how to connect to the bridge server via `BRIDGE_SERVER_ADDR`, but ingress forwarding behavior needs to be validated against the actual bridge server / interceptor topology in this test.

If ingress does not reach the sandbox app with the current runtime behavior, likely fixes will be in one of:

- dispatcher request destination selection
- bridge server listen-port / ingress listener configuration
- interceptor upstream selection

### Protocol mismatch between intercept and dispatcher

This is the main architectural blocker discovered during planning.

`bridge intercept --server-addr ...` expects a BridgeProxyService endpoint:

- gRPC for native bridge targets
- Connect RPC for unary methods on HTTP targets

The Go bridge proxy server already implements the Connect RPC API. The problem is specifically that `flow/dispatcher` does **not** expose BridgeProxyService today. It exposes:

- `POST /__tunnel/connect`
- `GET /__tunnel/status`
- regular HTTP request forwarding through its own tunnel client

That means the exact requested flow still fails because the interceptor cannot call the BridgeProxyService methods it needs on the dispatcher preview deployment.

The first concrete dispatcher API gap is:

- `GetMetadata`
- `CopyFiles`
- `ResolveDNSQuery`

### Bridge retry / reconnect behavior

The bridge-side reconnect path also needs a targeted update.

`bridge intercept` currently fetches metadata once during startup, then opens the tunnel. If the bridge-side connection dies and the retry path re-establishes it later, the client should refresh state by calling `GetMetadata` again before continuing.

Current conclusion:

- do not start with a broad Go transport rewrite
- do start with a focused retry change so reconnect re-runs `GetMetadata`

This is the smallest Go-side change currently believed necessary.

### Narrowed implementation direction

The working assumption is now:

1. `flow/dispatcher` implements the unary BridgeProxyService API surface expected by `bridge intercept`
2. bridge retry/reconnect refreshes metadata with `GetMetadata`
3. only after that do we revisit tunnel transport shape if the e2e still fails

### Previous broader options retained for context

Earlier planning considered either:

- changing the e2e topology so interceptor skips the dispatcher, or
- immediately replacing the HTTP tunnel transport

That is no longer the default plan.

The narrower path above should be attempted first.

### Current failure mode

Before the new dispatcher API work, the exact requested flow fails because:

- sandbox interceptor
- `--server-addr <preview deployment url>`

cannot yet call `GetMetadata`, `CopyFiles`, or `ResolveDNSQuery` on the dispatcher.

### Userservice runtime in sandbox

The test assumes the sandbox image has what is needed to run:

- `git`
- `node`
- `npm` / `npx`
- `vercel dev`

If `vercel dev` is not viable in the sandbox, switch to a simpler local server process for validation.

## Execution Order

1. Finish `pkg/flow/vercel` support needed for `gitSource` deployments and preview-protection helpers that consume the runtime bypass secret.
2. Implement the unary BridgeProxyService endpoints in `flow/dispatcher`.
3. Update bridge retry/reconnect so reconnect re-fetches metadata with `GetMetadata`.
4. Re-run the preview-deployment flow and see whether the remaining tunnel transport is sufficient.
5. Add the new `e2esandbox` suite and helpers.
6. Run the test manually and fix the first failing runtime assumption.
7. Revisit tunnel transport only if the narrower dispatcher API + retry changes are insufficient.
8. Tighten assertions and cleanup once the end-to-end path is stable.
