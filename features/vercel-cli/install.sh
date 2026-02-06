#!/bin/bash
set -e

VERSION="${VERSION:-latest}"
ENVFILE="${ENVFILE:-.env.development.local}"

# Remove yarn repository if it exists (often has expired GPG keys causing apt-get update to fail)
rm -f /etc/apt/sources.list.d/yarn.list 2>/dev/null || true

echo "Installing Vercel CLI (version: $VERSION)..."

# Check if Node.js is installed
if ! command -v node &> /dev/null; then
    echo "Node.js is required but not installed. Installing Node.js..."
    if command -v apt-get &> /dev/null; then
        rm -f /etc/apt/sources.list.d/yarn.list 2>/dev/null || true
        apt-get update
        apt-get install -y curl
        curl -fsSL https://deb.nodesource.com/setup_20.x | bash -
        apt-get install -y nodejs
        rm -rf /var/lib/apt/lists/*
    elif command -v apk &> /dev/null; then
        apk add --no-cache nodejs npm
    elif command -v yum &> /dev/null; then
        curl -fsSL https://rpm.nodesource.com/setup_20.x | bash -
        yum install -y nodejs
    else
        echo "ERROR: Could not install Node.js. Please install it manually."
        exit 1
    fi
fi

# Install Vercel CLI
if [ "$VERSION" = "latest" ]; then
    npm install -g vercel
else
    npm install -g "vercel@$VERSION"
fi

# Find where npm installed vercel
VERCEL_BIN=$(which vercel)
echo "Vercel CLI installed at: $VERCEL_BIN"

# Move the original binary to a different name
mv "$VERCEL_BIN" "${VERCEL_BIN}-bin"

# Create wrapper script for vercel that reads VERCEL_TOKEN from env file every time
cat > "$VERCEL_BIN" << 'WRAPPER'
#!/bin/bash
# Vercel CLI wrapper - reads VERCEL_TOKEN from env file on every invocation

# If VERCEL_TOKEN not already set, read it from the env file
if [ -z "$VERCEL_TOKEN" ]; then
  # Search for env file in common locations
  # Devcontainers typically mount to /workspaces/<repo-name>/
  for candidate in "/workspaces"/*/"${ENVFILE}" "/workspaces/${ENVFILE}" "${ENVFILE}"; do
    if [ -f "$candidate" ]; then
      VERCEL_TOKEN=$(grep -m1 '^VERCEL_TOKEN=' "$candidate" | sed 's/^VERCEL_TOKEN=//' | sed 's/^"//;s/"$//')
      break
    fi
  done
fi

# Use token if available
if [ -n "$VERCEL_TOKEN" ]; then
  exec "$(dirname "$0")/vercel-bin" --token "$VERCEL_TOKEN" "$@"
else
  exec "$(dirname "$0")/vercel-bin" "$@"
fi
WRAPPER

# Inject the env file path into the wrapper
sed -i "s|\${ENVFILE}|${ENVFILE}|g" "$VERCEL_BIN"
chmod +x "$VERCEL_BIN"

# Create wrapper script for vc (symlink to vercel wrapper)
VC_BIN=$(dirname "$VERCEL_BIN")/vc
if [ -f "$VC_BIN" ]; then
    rm "$VC_BIN"
fi
ln -s "$VERCEL_BIN" "$VC_BIN"

echo "Vercel CLI installed successfully!"
echo ""
echo "Usage:"
echo "  vercel whoami"
echo "  vc dev"
echo ""
echo "Token will be read from ${ENVFILE} on every invocation."
