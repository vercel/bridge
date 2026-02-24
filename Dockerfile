FROM golang:1.25-alpine AS builder

WORKDIR /src

# Copy go.mod files first for layer caching
COPY go.mod go.sum ./
COPY api/go/go.mod api/go/go.sum ./api/go/
RUN go mod download

# Copy source code
COPY . .

ARG VERSION=dev

RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /bridge \
    ./cmd/bridge

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

COPY --from=builder /bridge /usr/local/bin/bridge

ENTRYPOINT ["bridge"]
