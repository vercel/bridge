#!/bin/sh
set -e

REPO="vercel/bridge"
INSTALL_DIR="/usr/local/bin"

# Detect OS
get_os() {
    case "$(uname -s)" in
        Linux)  echo "linux" ;;
        Darwin) echo "darwin" ;;
        *)      echo "Unsupported OS: $(uname -s)" >&2; exit 1 ;;
    esac
}

# Detect architecture
get_arch() {
    case "$(uname -m)" in
        x86_64)        echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *)             echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
    esac
}

main() {
    local os arch binary_name download_url

    os=$(get_os)
    arch=$(get_arch)

    # Cache sudo credentials up front so the prompt happens before the
    # download — avoids issues when the script is piped via curl | sh.
    if [ ! -w "$INSTALL_DIR" ]; then
        echo "Installation to ${INSTALL_DIR} requires sudo access."
        sudo -v
    fi

    binary_name="bridge-${os}-${arch}"
    download_url="https://github.com/${REPO}/releases/download/edge/${binary_name}"

    echo "Downloading bridge edge (${os}/${arch})..."

    curl -fsSL -o bridge "${download_url}"
    chmod +x bridge

    if [ -w "$INSTALL_DIR" ]; then
        mv bridge "${INSTALL_DIR}/bridge"
    else
        sudo mv bridge "${INSTALL_DIR}/bridge"
    fi

    if [ "$os" = "darwin" ]; then
        # Remove macOS quarantine attribute to prevent Gatekeeper killing the binary
        sudo xattr -d com.apple.quarantine "${INSTALL_DIR}/bridge" 2>/dev/null || true
    fi

    echo "bridge (edge) installed to ${INSTALL_DIR}/bridge"

    install_linux_binary "$os" "$arch"
}

# Also install the linux binary to ~/.bridge/bin/bridge-linux so that
# `bridge create` can bind-mount it into devcontainers.
install_linux_binary() {
    local os="$1" arch="$2"
    local bridge_dir="${HOME}/.bridge/bin"
    mkdir -p "$bridge_dir"

    if [ "$os" = "linux" ]; then
        cp "${INSTALL_DIR}/bridge" "${bridge_dir}/bridge-linux"
    else
        local linux_url="https://github.com/${REPO}/releases/download/edge/bridge-linux-${arch}"
        echo "Downloading linux bridge binary..."
        curl -fsSL -o "${bridge_dir}/bridge-linux" "${linux_url}"
    fi
    chmod +x "${bridge_dir}/bridge-linux"
    echo "Linux bridge binary installed to ${bridge_dir}/bridge-linux"
}

main
