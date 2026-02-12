# Build stage
FROM golang:1.21-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git make protobuf protobuf-dev

WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN make build-linux

# Runtime stage
FROM ubuntu:22.04

# Install runtime dependencies
RUN apt-get update && apt-get install -y \
    docker.io \
    docker-compose \
    qemu-kvm \
    libvirt-daemon-system \
    libvirt-clients \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Create persys user
RUN useradd -r -u 1000 -g 0 -m -s /bin/bash persys && \
    mkdir -p /var/lib/persys /etc/persys/certs && \
    chown -R persys:root /var/lib/persys /etc/persys

# Copy binary from builder
COPY --from=builder /build/bin/persys-agent-linux-amd64 /usr/local/bin/persys-agent
RUN chmod +x /usr/local/bin/persys-agent

# Set up volumes
VOLUME ["/var/lib/persys", "/etc/persys"]

# Expose gRPC port
EXPOSE 50051

# Run as persys user
USER persys

# Set default environment variables
ENV PERSYS_STATE_PATH=/var/lib/persys/state.db \
    PERSYS_TLS_CERT=/etc/persys/certs/agent.crt \
    PERSYS_TLS_KEY=/etc/persys/certs/agent.key \
    PERSYS_TLS_CA=/etc/persys/certs/ca.crt \
    PERSYS_LOG_LEVEL=info

ENTRYPOINT ["/usr/local/bin/persys-agent"]
