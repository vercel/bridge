#!/bin/bash
set -e

# GitHub repository for bridge releases
REPO="vercel-eddie/bridge"

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
    local version="${BRIDGEVERSION:-edge}"
    local arch=$(get_arch)
    local os="linux"

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

# Write environment configuration
write_env_config() {
    # Write to /etc/environment for system-wide availability (read by PAM)
    # Feature options are available as uppercase versions without special chars
    {
        echo "SANDBOX_URL=${SANDBOXURL:-}"
        echo "FUNCTION_URL=${FUNCTIONURL:-}"
        echo "SANDBOX_NAME=${SANDBOXNAME:-}"
        echo "SYNC_SOURCE=${SYNCSOURCE:-.}"
        echo "SYNC_TARGET=${SYNCTARGET:-}"
    } >> /etc/environment

    # Also write to /etc/profile.d for shell login sessions
    cat > /etc/profile.d/bridge.sh << EOF
export SANDBOX_URL="${SANDBOXURL:-}"
export FUNCTION_URL="${FUNCTIONURL:-}"
export SANDBOX_NAME="${SANDBOXNAME:-}"
export SYNC_SOURCE="${SYNCSOURCE:-.}"
export SYNC_TARGET="${SYNCTARGET:-}"
EOF
}

# Create entrypoint script
create_entrypoint() {
    cat > /usr/local/bin/bridge-entrypoint.sh << 'EOF'
#!/bin/bash
# Source profile in case env vars aren't inherited
[ -f /etc/profile.d/bridge.sh ] && source /etc/profile.d/bridge.sh

# Run bridge intercept as root (required for iptables), passing env vars explicitly
if [ -n "$SANDBOX_URL" ]; then
    sudo -E /usr/local/bin/bridge intercept &
fi

exec "$@"
EOF
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
