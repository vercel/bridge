#!/bin/sh
set -e

REPO="vercel/bridge"
INSTALL_DIR="${BRIDGE_INSTALL_DIR:-${HOME}/.local/bin}"

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

# Print a one-liner the user can paste to add INSTALL_DIR to their PATH,
# tailored to whichever rc file their default shell would source.
print_path_hint() {
    case "${SHELL##*/}" in
        zsh)  rc="${ZDOTDIR:-$HOME}/.zshrc" ;;
        bash) rc="${HOME}/.bashrc" ;;
        fish) rc="${HOME}/.config/fish/config.fish" ;;
        *)    rc="your shell startup file" ;;
    esac

    echo ""
    echo "${INSTALL_DIR} is not on your PATH."
    if [ "${SHELL##*/}" = "fish" ]; then
        echo "Add it with:"
        echo ""
        echo "    fish_add_path ${INSTALL_DIR}"
    else
        echo "Add it by appending this line to ${rc}:"
        echo ""
        echo "    export PATH=\"${INSTALL_DIR}:\$PATH\""
    fi
    echo ""
    echo "Then open a new shell, or run: source ${rc}"
}

main() {
    os=$(get_os)
    arch=$(get_arch)

    mkdir -p "${INSTALL_DIR}"

    binary_name="bridge-${os}-${arch}"
    download_url="https://github.com/${REPO}/releases/download/edge/${binary_name}"

    echo "Downloading bridge edge (${os}/${arch})..."
    curl -fsSL -o "${INSTALL_DIR}/bridge" "${download_url}"
    chmod +x "${INSTALL_DIR}/bridge"

    echo "bridge (edge) installed to ${INSTALL_DIR}/bridge"

    install_linux_binary "$os" "$arch"

    # Hint about PATH only if INSTALL_DIR isn't already discoverable. We
    # don't try to edit rc files ourselves — that's the kind of magic
    # users rightly distrust in install scripts.
    case ":$PATH:" in
        *":${INSTALL_DIR}:"*) ;;
        *) print_path_hint ;;
    esac
}

# Also install the linux binary to ~/.bridge/bin/bridge-linux so that
# `bridge create` can bind-mount it into devcontainers regardless of the
# developer's host platform.
install_linux_binary() {
    os="$1"
    arch="$2"
    bridge_dir="${HOME}/.bridge/bin"
    mkdir -p "$bridge_dir"

    if [ "$os" = "linux" ]; then
        cp "${INSTALL_DIR}/bridge" "${bridge_dir}/bridge-linux"
    else
        linux_url="https://github.com/${REPO}/releases/download/edge/bridge-linux-${arch}"
        echo "Downloading linux bridge binary..."
        curl -fsSL -o "${bridge_dir}/bridge-linux" "${linux_url}"
    fi
    chmod +x "${bridge_dir}/bridge-linux"
    echo "Linux bridge binary installed to ${bridge_dir}/bridge-linux"
}

main
