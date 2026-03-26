FROM mcr.microsoft.com/devcontainers/python:3.12

# Install dig and other network debugging tools
USER root
RUN rm -f /etc/apt/sources.list.d/yarn.list \
    && apt-get update && apt-get install -y --no-install-recommends dnsutils \
    && rm -rf /var/lib/apt/lists/*
USER vscode

# Install uv from official image (recommended by Astral)
COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx /usr/local/bin/

# Install debugpy for VS Code Python debugging
RUN pip install --no-cache-dir debugpy

# Set working directory
WORKDIR /workspace
