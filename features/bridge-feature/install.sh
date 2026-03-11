#!/bin/bash
set -e
trap 'echo "ERROR: install.sh failed at line $LINENO (exit code $?)" >&2' ERR

# Install required tools
install_dependencies() {
    if ! command -v curl &> /dev/null; then
        echo "Installing curl..."
        if command -v apt-get &> /dev/null; then
            apt-get update && apt-get install -y curl ca-certificates
        elif command -v apk &> /dev/null; then
            apk add --no-cache curl ca-certificates
        elif command -v yum &> /dev/null; then
            yum install -y curl ca-certificates
        fi
    fi

    # Install iptables if not present (needed for traffic interception).
    # Non-fatal: the base image may already include it or the package
    # repository may be unavailable during the build.
    if ! command -v iptables &> /dev/null; then
        echo "Installing iptables..."
        if command -v apt-get &> /dev/null; then
            apt-get update && apt-get install -y iptables || echo "WARNING: failed to install iptables (will retry at runtime)" >&2
        elif command -v apk &> /dev/null; then
            apk add --no-cache iptables || echo "WARNING: failed to install iptables (will retry at runtime)" >&2
        elif command -v yum &> /dev/null; then
            yum install -y iptables || echo "WARNING: failed to install iptables (will retry at runtime)" >&2
        fi
    fi

    # Install sg (switch group) if not present. Required by the entrypoint
    # to run bridge intercept under the _bridge group for iptables GID exclusion.
    if ! command -v sg &> /dev/null; then
        echo "Installing sg..."
        if command -v apt-get &> /dev/null; then
            apt-get update && apt-get install -y login || true
        elif command -v apk &> /dev/null; then
            apk add --no-cache shadow-login || true
        elif command -v yum &> /dev/null; then
            yum install -y shadow-utils || true
        fi
    fi
}

# Create a dedicated group for running bridge intercept.
# Running under a separate GID allows iptables --gid-owner rules to exclude
# bridge's own DNS traffic from the UDP redirect, preventing a circular
# dependency when gRPC reconnects and the aws CLI needs real DNS to reach
# the STS endpoint for EKS authentication.
create_bridge_group() {
    if getent group _bridge &> /dev/null; then
        echo "Group _bridge already exists"
        return
    fi

    echo "Creating _bridge group..."
    if command -v addgroup &> /dev/null && command -v apk &> /dev/null; then
        # Alpine
        addgroup -S _bridge
    elif command -v groupadd &> /dev/null; then
        # Debian / RHEL
        groupadd --system _bridge
    else
        echo "ERROR: could not create _bridge group (no addgroup/groupadd found)" >&2
        exit 1
    fi
}

# Write environment configuration to /etc/profile.d (sourced by entrypoint)
write_env_config() {
    cat > /etc/profile.d/bridge.sh << EOF
export BRIDGE_SERVER_ADDR="${BRIDGESERVERADDR:-}"
export BRIDGE_APP_PORT="${APPPORT:-3000}"
export BRIDGE_FORWARD_DOMAINS="${FORWARDDOMAINS:-$BRIDGE_FORWARD_DOMAINS}"
export BRIDGE_COPY_FILES="${COPYFILES:-$BRIDGE_COPY_FILES}"
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

# Run bridge intercept as root under the _bridge group (required for iptables).
# The dedicated GID lets iptables exclude bridge's own DNS traffic from the
# UDP redirect via --gid-owner, breaking the circular dependency when gRPC
# reconnects and the aws CLI needs real DNS.
if [ -n "$BRIDGE_SERVER_ADDR" ]; then
    sudo -E sg _bridge -c '
        exec /usr/local/bin/bridge --log-paths stderr intercept
    ' > /tmp/bridge-intercept.log 2>&1 &
fi

exec "$@"
RUNTIME
    chmod +x /usr/local/bin/bridge-entrypoint.sh
}

# Main installation
main() {
    echo "Installing Bridge Tunnel Client..."

    install_dependencies
    create_bridge_group
    write_env_config
    create_entrypoint

    echo "Bridge Tunnel Client installation complete"
}

main
