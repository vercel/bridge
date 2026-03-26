FROM node:20-alpine

# Install pnpm
RUN corepack enable && corepack prepare pnpm@10.26.0 --activate

# Copy the local @vercel/bridge-api dependency first (for better layer caching)
COPY api/ts /app/api/ts

# Copy dispatcher source
COPY services/dispatcher /app/services/dispatcher

WORKDIR /app/services/dispatcher

# Install dependencies and build
RUN pnpm install --frozen-lockfile && pnpm run build

# Default port
EXPOSE 8080

# Run the dispatcher
CMD ["node", "dist/src/index.js"]
