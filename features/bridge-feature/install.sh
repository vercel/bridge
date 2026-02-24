#!/bin/bash
set -e

# GitHub repository for bridge releases
REPO="vercel/bridge"

# Install required tools
install_dependencies() {
    if ! command -v curl &> /dev/null; then
        if command -v apt-get &> /dev/null; then
            apt-get update && apt-get install -y curl ca-certificates
        elif command -v apk &> /dev/null; then
            apk add --no-cache curl ca-certificates
        elif command -v yum &> /dev/null; then
            yum install -y curl ca-certificates
        fi
    fi

    # Install iptables if not present (needed for traffic interception)
    if ! command -v iptables &> /dev/null; then
        if command -v apt-get &> /dev/null; then
            apt-get update && apt-get install -y iptables
        elif command -v apk &> /dev/null; then
            apk add --no-cache iptables
        elif command -v yum &> /dev/null; then
            yum install -y iptables
        fi
    fi
}

# Detect architecture
get_arch() {
    local arch=$(uname -m)
    case "$arch" in
        x86_64) echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *) echo "Unsupported architecture: $arch" >&2; exit 1 ;;
    esac
}

# Get the latest release version from GitHub
get_latest_version() {
    curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | \
        grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/'
}

# Download and install bridge binary
install_bridge() {
    if [ -x /usr/local/bin/bridge ]; then
        echo "Bridge binary already installed, skipping download"
        return 0
    fi

    local version="${BRIDGEVERSION:-edge}"

    # In dev mode the binary is expected to be provided via bind mount at runtime.
    if [ "$version" = "dev" ]; then
        echo "Dev mode: skipping binary download (expecting bind mount at runtime)"
        return 0
    fi

    local arch=$(get_arch)
    local os="linux"

    # Versions like "edge-abc1234" map to the "edge" release tag.
    if [[ "$version" == edge-* ]]; then
        version="edge"
    fi

    # Resolve 'latest' to actual version
    if [ "$version" = "latest" ]; then
        version=$(get_latest_version)
        if [ -z "$version" ]; then
            echo "Failed to fetch latest version" >&2
            exit 1
        fi
    fi

    echo "Installing bridge ${version} for ${os}-${arch}..."

    local binary_name="bridge-${os}-${arch}"
    local download_url="https://github.com/${REPO}/releases/download/${version}/${binary_name}"

    echo "Downloading from: ${download_url}"

    if ! curl -fsSL -o /usr/local/bin/bridge "${download_url}"; then
        echo "Failed to download bridge binary" >&2
        exit 1
    fi

    chmod +x /usr/local/bin/bridge
    echo "Bridge ${version} installed successfully"
}

# Write environment configuration to /etc/profile.d (sourced by entrypoint)
write_env_config() {
    cat > /etc/profile.d/bridge.sh << EOF
export BRIDGE_SERVER_ADDR="${BRIDGESERVERADDR:-}"
export APP_PORT="${APPPORT:-3000}"
export FORWARD_DOMAINS="${FORWARDDOMAINS:-$FORWARD_DOMAINS}"
EOF
}

# Create entrypoint script
create_entrypoint() {
    # First part: bake the workspace path at install time (unquoted heredoc
    # so ${WORKSPACEPATH} is expanded now, not at runtime)
    cat > /usr/local/bin/bridge-entrypoint.sh << EOF
#!/bin/bash
# Baked at install time from feature option "workspacePath"
BRIDGE_WORKSPACE_PATH="${WORKSPACEPATH:-}"
EOF

    # Second part: runtime logic (quoted heredoc — no expansion at install time)
    cat >> /usr/local/bin/bridge-entrypoint.sh << 'RUNTIME'

# Source profile in case env vars aren't inherited
[ -f /etc/profile.d/bridge.sh ] && source /etc/profile.d/bridge.sh

# Run bridge intercept as root (required for iptables)
if [ -n "$BRIDGE_SERVER_ADDR" ]; then
    sudo BRIDGE_SERVER_ADDR="$BRIDGE_SERVER_ADDR" \
         FORWARD_DOMAINS="$FORWARD_DOMAINS" \
         KUBECONFIG="${KUBECONFIG:-}" \
         /usr/local/bin/bridge --log-path stderr intercept > /tmp/bridge-intercept.log 2>&1 &
fi

exec "$@"
RUNTIME
    chmod +x /usr/local/bin/bridge-entrypoint.sh
}

# Main installation
main() {
    echo "Installing Bridge Tunnel Client..."

    install_dependencies
    install_bridge
    write_env_config
    create_entrypoint

    echo "Bridge Tunnel Client installation complete"
}

main
