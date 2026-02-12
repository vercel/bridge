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

# Write environment configuration to /etc/profile.d (sourced by entrypoint)
write_env_config() {
    local env_file="${ENVFILE:-.env.development.local}"

    cat > /etc/profile.d/bridge.sh << EOF
export SANDBOX_URL="${SANDBOXURL:-}"
export FUNCTION_URL="${FUNCTIONURL:-}"
export SANDBOX_NAME="${SANDBOXNAME:-}"
export SYNC_SOURCE="${SYNCSOURCE:-.}"
export SYNC_TARGET="${SYNCTARGET:-}"
export APP_PORT="${APPPORT:-3000}"
export BRIDGE_ENV_FILE="${env_file}"
export FORWARD_DOMAINS="${FORWARDDOMAINS:-$FORWARD_DOMAINS}"
EOF
}

# Helper to read a value from an env file
# Usage: read_env_var ENV_FILE VAR_NAME
read_env_var() {
    local file="$1" var="$2"
    if [ -f "$file" ]; then
        grep -m1 "^${var}=" "$file" | sed "s/^${var}=//" | sed 's/^"//;s/"$//'
    fi
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

# Function to read a value from an env file
read_env_file() {
    local var_name="$1"
    local env_file=""
    local env_filename="${BRIDGE_ENV_FILE:-.env.development.local}"

    # Try baked workspace path first, then fall back to find
    if [ -n "$BRIDGE_WORKSPACE_PATH" ] && [ -f "${BRIDGE_WORKSPACE_PATH}/${env_filename}" ]; then
        env_file="${BRIDGE_WORKSPACE_PATH}/${env_filename}"
    else
        env_file=$(find /workspaces -maxdepth 5 -name "$env_filename" -type f 2>/dev/null | head -1)
    fi

    if [ -n "$env_file" ] && [ -f "$env_file" ]; then
        grep -m1 "^${var_name}=" "$env_file" 2>/dev/null | sed "s/^${var_name}=//" | sed 's/^"//;s/"$//'
    fi
}

# Run bridge intercept as root (required for iptables)
if [ -n "$SANDBOX_URL" ]; then
    BYPASS_SECRET=$(read_env_file "VERCEL_AUTOMATION_BYPASS_SECRET")
    sudo SANDBOX_URL="$SANDBOX_URL" \
         FUNCTION_URL="$FUNCTION_URL" \
         VERCEL_AUTOMATION_BYPASS_SECRET="$BYPASS_SECRET" \
         FORWARD_DOMAINS="$FORWARD_DOMAINS" \
         /usr/local/bin/bridge intercept &
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
