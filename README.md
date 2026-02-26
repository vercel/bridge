# Bridge

A CLI that enables developers to run their code locally within the context of a Dev/Staging K8s environment.

<img src="docs/assets/create.gif" alt="bridge create demo" width="900" />

## Installation

```sh
curl -fsSL https://raw.githubusercontent.com/vercel/bridge/main/install-edge.sh | sh
```

This downloads the latest edge binary for your platform (macOS/Linux, amd64/arm64) and installs it to `/usr/local/bin`.

### Update

Either rerun the [installation](#installation) or you can run

```
bridge update
```

## Usage

To create a bridge to the `userservice` you can simply run

```
bridge create -c userservice
```

This will

1. Copy the `userservice` configuration
2. Swap the application container with a proxy
3. Generate a Devcontainer from your nearest (or specify which with `-f`)
4. Put you inside the container (`-c`)

## API

The API is defined using protocol buffers. Currently, all messages are sent/received via websocket/HTTP but the payloads
themselves are housed within the [protos directory](./proto).

### Generate

To generate, install [buf](https://buf.build/docs/cli/installation/) and run:

```
make
```

## Local Development (k3d)

### Prerequisites

- [Docker](https://docs.docker.com/get-docker/)
- [k3d](https://k3d.io/) (`brew install k3d`)
- Go 1.25+

### 1. Create a k3d cluster with a registry

```bash
k3d cluster create bridge --registry-create bridge-registry:0.0.0.0:5111
```

This creates a lightweight k3s cluster with a local container registry at `k3d-bridge-registry.localhost:5111`. Your
kubeconfig context is automatically switched to `k3d-bridge`.

### 2. Seed the cluster

Build images, push to the registry, and apply the Kubernetes manifests:

```bash
go run deploy/main.go
```

This deploys the bridge administrator (namespace `bridge`) and a test HTTP server (namespace `test-workloads`). Re-run
to pick up code changes — images are pushed and deployments are restarted automatically.

### 3. Build the CLI

```bash
go build -o bridge ./cmd/bridge
```

To test the devcontainer feature locally, also build a Linux binary:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o dist/bridge-linux ./cmd/bridge
```

### 4. Run commands

Create a bridge to the test server:

```bash
./bridge create test-api-server -n test-workloads --connect
```

To test local changes to the devcontainer feature (intercept, DNS, etc.), first set up hardlinks to the feature files:

```bash
mkdir -p .devcontainer/local-features/bridge
ln features/bridge/install.sh .devcontainer/local-features/bridge/install.sh
ln features/bridge/devcontainer-feature.json .devcontainer/local-features/bridge/devcontainer-feature.json
```

Then run with the local feature ref:

```bash
./bridge create test-api-server -n test-workloads --feature-ref ../local-features/bridge -f .devcontainer/devcontainer.json --force --connect
```

Or start just the administrator port-forward to verify connectivity:

```bash
kubectl port-forward -n bridge svc/administrator 9090:9090
```

### Teardown

```bash
k3d cluster delete bridge --registry-delete k3d-bridge-registry
```

## Architecture

See [here](./docs/architecture.md) for more info.
