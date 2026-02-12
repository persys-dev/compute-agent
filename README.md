# Persys Compute Agent

The Persys Compute Agent is a production-grade node-level execution engine for the Persys Cloud platform. It manages Docker containers, Docker Compose applications, and KVM virtual machines with idempotent operations, mTLS security, and automatic crash recovery.

## Features

- **Multi-Runtime Support**: Docker containers, Docker Compose, and KVM/libvirt VMs
- **Idempotent Operations**: Revision-based tracking prevents duplicate work
- **Secure Communication**: gRPC with mutual TLS authentication
- **Persistent State**: bbolt-backed state store for crash recovery
- **Auto Reconciliation**: Periodic state synchronization and self-healing
- **Production Ready**: Comprehensive error handling, logging, and monitoring

## Architecture

```
┌─────────────────┐
│   Scheduler     │
│   (Control)     │
└────────┬────────┘
         │ gRPC/mTLS
         ▼
┌─────────────────────────────────────┐
│      Persys Compute Agent           │
│                                     │
│  ┌──────────┐    ┌──────────────┐  │
│  │  gRPC    │◄──►│  Workload    │  │
│  │  Server  │    │  Manager     │  │
│  └──────────┘    └──────┬───────┘  │
│                          │          │
│  ┌───────────────────────┴────┐    │
│  │     Runtime Manager        │    │
│  ├────────────────────────────┤    │
│  │ Docker │ Compose │   VM    │    │
│  └────────┴─────────┴─────────┘    │
│                                     │
│  ┌──────────────┐ ┌─────────────┐  │
│  │ State Store  │ │ Reconciler  │  │
│  │   (bbolt)    │ │    Loop     │  │
│  └──────────────┘ └─────────────┘  │
└─────────────────────────────────────┘
         │            │          │
         ▼            ▼          ▼
    [Docker]    [Compose]   [libvirt]
```

## Quick Start

### Prerequisites

- Go 1.21 or later
- Docker Engine
- docker-compose (optional)
- KVM/libvirt (optional)
- protoc compiler

### Installation

```bash
# Clone the repository
git clone https://github.com/persys/compute-agent.git
cd compute-agent

# Install dependencies
make deps

# Build the agent (protobuf files are pre-generated and included)
make build

# The binary will be in bin/persys-agent
```

**Note:** Protobuf files are already generated and included in `pkg/api/v1/`. You only need to run `make proto` if you modify the `.proto` file.

### Running the Agent

```bash
# Generate development certificates
make dev-certs

# Run with development configuration
PERSYS_TLS_CERT=dev/certs/agent.crt \
PERSYS_TLS_KEY=dev/certs/agent.key \
PERSYS_TLS_CA=dev/certs/ca.crt \
./bin/persys-agent
```

## Configuration

The agent is configured via environment variables:

### Server Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PERSYS_GRPC_ADDR` | `0.0.0.0` | gRPC bind address |
| `PERSYS_GRPC_PORT` | `50051` | gRPC port |

### TLS/mTLS Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PERSYS_TLS_ENABLED` | `true` | Enable mTLS authentication |
| `PERSYS_TLS_CERT` | `/etc/persys/certs/agent.crt` | Server certificate path |
| `PERSYS_TLS_KEY` | `/etc/persys/certs/agent.key` | Server private key path |
| `PERSYS_TLS_CA` | `/etc/persys/certs/ca.crt` | CA certificate path |

### State Store Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PERSYS_STATE_PATH` | `/var/lib/persys/state.db` | bbolt database path |

### Runtime Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PERSYS_DOCKER_ENABLED` | `true` | Enable Docker runtime |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker socket |
| `PERSYS_COMPOSE_ENABLED` | `true` | Enable Compose runtime |
| `PERSYS_COMPOSE_BINARY` | `docker-compose` | Compose binary path |
| `PERSYS_VM_ENABLED` | `true` | Enable VM runtime |
| `PERSYS_LIBVIRT_URI` | `qemu:///system` | Libvirt connection URI |

### Reconciliation Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PERSYS_RECONCILE_ENABLED` | `true` | Enable reconciliation loop |
| `PERSYS_RECONCILE_INTERVAL` | `30s` | Reconciliation interval |

### Logging Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PERSYS_LOG_LEVEL` | `info` | Log level (debug, info, warn, error) |

### Agent Metadata

| Variable | Default | Description |
|----------|---------|-------------|
| `PERSYS_NODE_ID` | hostname | Unique node identifier |
| `PERSYS_VERSION` | `dev` | Agent version |

## API Reference

### gRPC Service

The agent exposes a gRPC service with the following methods:

#### ApplyWorkload

Creates or updates a workload with idempotent revision tracking.

```protobuf
rpc ApplyWorkload(ApplyWorkloadRequest) returns (ApplyWorkloadResponse)
```

**Request:**
- `id`: Unique workload identifier
- `type`: Workload type (container, compose, vm)
- `revision_id`: Revision for idempotency
- `desired_state`: Running or Stopped
- `spec`: Runtime-specific configuration

**Response:**
- `applied`: Whether workload was applied
- `skipped`: True if revision already applied
- `status`: Current workload status

#### DeleteWorkload

Removes a workload and cleans up resources.

```protobuf
rpc DeleteWorkload(DeleteWorkloadRequest) returns (DeleteWorkloadResponse)
```

#### GetWorkloadStatus

Retrieves current status of a workload.

```protobuf
rpc GetWorkloadStatus(GetWorkloadStatusRequest) returns (GetWorkloadStatusResponse)
```

#### ListWorkloads

Lists all managed workloads, optionally filtered by type.

```protobuf
rpc ListWorkloads(ListWorkloadsRequest) returns (ListWorkloadsResponse)
```

#### HealthCheck

Returns agent health and runtime status.

```protobuf
rpc HealthCheck(HealthCheckRequest) returns (HealthCheckResponse)
```

## Workload Specifications

### Container Spec

```json
{
  "image": "nginx:latest",
  "command": ["/bin/sh"],
  "args": ["-c", "nginx -g 'daemon off;'"],
  "env": {
    "ENV_VAR": "value"
  },
  "volumes": [
    {
      "host_path": "/data",
      "container_path": "/app/data",
      "read_only": false
    }
  ],
  "ports": [
    {
      "host_port": 8080,
      "container_port": 80,
      "protocol": "tcp"
    }
  ],
  "resources": {
    "cpu_shares": 1024,
    "memory_bytes": 536870912
  },
  "restart_policy": {
    "policy": "unless-stopped",
    "max_retry_count": 3
  }
}
```

### Compose Spec

```json
{
  "project_name": "myapp",
  "compose_yaml": "<base64-encoded-docker-compose.yml>",
  "env": {
    "DATABASE_URL": "postgres://..."
  }
}
```

### VM Spec

```json
{
  "name": "web-vm",
  "vcpus": 4,
  "memory_mb": 8192,
  "disks": [
    {
      "path": "/var/lib/libvirt/images/web-vm.qcow2",
      "device": "vda",
      "format": "qcow2",
      "size_gb": 50
    }
  ],
  "networks": [
    {
      "network": "default",
      "mac_address": "52:54:00:12:34:56",
      "ip_address": "192.168.122.100"
    }
  ],
  "cloud_init": "<base64-encoded-cloud-init-user-data>"
}
```

## Development

### Project Structure

```
persys-compute-agent/
├── cmd/
│   └── agent/           # Main entry point
├── internal/
│   ├── config/          # Configuration management
│   ├── grpc/            # gRPC server implementation
│   ├── reconcile/       # Reconciliation loop
│   ├── runtime/         # Runtime implementations
│   │   ├── docker.go    # Docker runtime
│   │   ├── compose.go   # Compose runtime
│   │   └── vm.go        # VM runtime
│   ├── state/           # State store (bbolt)
│   └── workload/        # Workload manager
├── pkg/
│   ├── api/             # Generated protobuf code
│   └── models/          # Data models
├── api/
│   └── proto/           # Protobuf definitions
├── Dockerfile
├── Makefile
└── README.md
```

### Building

```bash
# Build for current platform
make build

# Build for Linux (useful for macOS/Windows dev)
make build-linux

# Run tests
make test

# Format code
make fmt

# Run linters
make lint
```

### Docker Build

```bash
# Build Docker image
make docker-build

# Push to registry
make docker-push
```

## Deployment

### Systemd Service

Create `/etc/systemd/system/persys-agent.service`:

```ini
[Unit]
Description=Persys Compute Agent
After=network.target docker.service libvirtd.service

[Service]
Type=simple
User=root
Environment="PERSYS_NODE_ID=%H"
Environment="PERSYS_STATE_PATH=/var/lib/persys/state.db"
Environment="PERSYS_TLS_CERT=/etc/persys/certs/agent.crt"
Environment="PERSYS_TLS_KEY=/etc/persys/certs/agent.key"
Environment="PERSYS_TLS_CA=/etc/persys/certs/ca.crt"
ExecStart=/usr/local/bin/persys-agent
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

Enable and start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable persys-agent
sudo systemctl start persys-agent
```

### Docker Deployment

```bash
docker run -d \
  --name persys-agent \
  --privileged \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /var/run/libvirt:/var/run/libvirt \
  -v /var/lib/persys:/var/lib/persys \
  -v /etc/persys/certs:/etc/persys/certs:ro \
  -p 50051:50051 \
  -e PERSYS_NODE_ID=$(hostname) \
  persys/compute-agent:latest
```

### Kubernetes Deployment

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: persys-agent
  namespace: persys-system
spec:
  selector:
    matchLabels:
      app: persys-agent
  template:
    metadata:
      labels:
        app: persys-agent
    spec:
      hostNetwork: true
      hostPID: true
      containers:
      - name: agent
        image: persys/compute-agent:latest
        securityContext:
          privileged: true
        env:
        - name: PERSYS_NODE_ID
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        volumeMounts:
        - name: docker-socket
          mountPath: /var/run/docker.sock
        - name: libvirt-socket
          mountPath: /var/run/libvirt
        - name: state
          mountPath: /var/lib/persys
        - name: certs
          mountPath: /etc/persys/certs
          readOnly: true
      volumes:
      - name: docker-socket
        hostPath:
          path: /var/run/docker.sock
      - name: libvirt-socket
        hostPath:
          path: /var/run/libvirt
      - name: state
        hostPath:
          path: /var/lib/persys
      - name: certs
        secret:
          secretName: persys-agent-certs
```

## Security

### mTLS Authentication

The agent uses mutual TLS for secure communication:

1. **Certificate Generation**: Use CFSSL or OpenSSL to generate certificates
2. **Certificate Distribution**: Deploy certificates via secret management (Vault, etc.)
3. **Certificate Rotation**: Implement automated rotation for production

### Least Privilege

- Run agent as non-root where possible
- Use Docker socket with appropriate permissions
- Configure libvirt access control
- Implement RBAC for scheduler authentication

### Secret Management

Secrets can be injected via:
- Environment variables (from scheduler)
- Vault integration (optional)
- Kubernetes secrets
- Encrypted at rest in state store (future)

## Monitoring

### Health Check

```bash
grpcurl -plaintext localhost:50051 \
  persys.agent.v1.AgentService/HealthCheck
```

### Metrics (Future)

- Workload count by state
- Runtime operation latency
- Reconciliation cycle duration
- State store operations
- gRPC request rate/latency

### Logging

The agent uses structured logging with configurable levels:

```bash
PERSYS_LOG_LEVEL=debug ./bin/persys-agent
```

## Troubleshooting

### Agent won't start

Check logs for:
- TLS certificate issues
- State store permissions
- Runtime availability (Docker/libvirt)

### Workloads not starting

1. Check workload status: `GetWorkloadStatus`
2. Verify runtime is healthy
3. Check reconciliation logs
4. Inspect state store

### High CPU usage

- Reduce reconciliation frequency
- Check for stuck workloads
- Review runtime performance

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests
5. Run `make test` and `make lint`
6. Submit a pull request

## License

Copyright © 2024 Persys Cloud

## Support

For issues and questions:
- GitHub Issues: https://github.com/persys/compute-agent/issues
- Documentation: https://docs.persys.cloud
- Email: support@persys.cloud
