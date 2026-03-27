FROM mcr.microsoft.com/devcontainers/javascript-node:22

# Install dig and other network debugging tools
USER root
RUN apt-get update && apt-get install -y --no-install-recommends dnsutils \
    && rm -rf /var/lib/apt/lists/*
USER node

# Set working directory
WORKDIR /workspace
