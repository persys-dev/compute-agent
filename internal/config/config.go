package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/persys/compute-agent/internal/node"
)

// Config holds all agent configuration
type Config struct {
	// Server configuration
	GRPCAddr string
	GRPCPort int

	// TLS/mTLS configuration
	TLSEnabled  bool
	TLSCertPath string
	TLSKeyPath  string
	TLSCAPath   string

	// Vault certificate manager configuration
	VaultEnabled       bool
	VaultAddr          string
	VaultAuthMethod    string
	VaultToken         string
	VaultAppRoleID     string
	VaultAppSecretID   string
	VaultPKIMount      string
	VaultPKIRole       string
	VaultCertTTL       time.Duration
	VaultServiceName   string
	VaultServiceDomain string
	VaultRetryInterval time.Duration

	// State store configuration
	StateStorePath string

	// Runtime configuration
	DockerEnabled  bool
	DockerEndpoint string
	ComposeEnabled bool
	ComposeBinary  string
	VMEnabled      bool
	LibvirtURI     string

	// Reconciliation configuration
	ReconcileInterval time.Duration
	ReconcileEnabled  bool

	// Logging
	LogLevel string

	// Agent metadata
	NodeID     string
	Version    string
	NodeRegion string
	NodeEnv    string
	NodeLabels map[string]string

	// Scheduler control-plane configuration
	SchedulerAddr       string
	SchedulerInsecure   bool
	SchedulerTLSEnabled bool
	AgentGRPCEndpoint   string
}

// Load reads configuration from environment variables with sensible defaults
func Load() (*Config, error) {
	cfg := &Config{
		// Server defaults
		GRPCAddr: getEnv("PERSYS_GRPC_ADDR", "0.0.0.0"),
		GRPCPort: getEnvAsInt("PERSYS_GRPC_PORT", 50051),

		// TLS defaults
		TLSEnabled:         getEnvAsBool("PERSYS_TLS_ENABLED", true),
		TLSCertPath:        getEnv("PERSYS_TLS_CERT", "/etc/persys/certs/agent/compute-agent.pem"),
		TLSKeyPath:         getEnv("PERSYS_TLS_KEY", "/etc/persys/certs/agent/compute-agent-key.pem"),
		TLSCAPath:          getEnv("PERSYS_TLS_CA", "/etc/persys/certs/agent/ca.pem"),
		VaultEnabled:       getEnvAsBool("PERSYS_VAULT_ENABLED", true),
		VaultAddr:          getEnv("PERSYS_VAULT_ADDR", "http://localhost:8200"),
		VaultAuthMethod:    strings.ToLower(getEnv("PERSYS_VAULT_AUTH_METHOD", "token")),
		VaultToken:         getEnv("PERSYS_VAULT_TOKEN", ""),
		VaultAppRoleID:     getEnv("PERSYS_VAULT_APPROLE_ROLE_ID", ""),
		VaultAppSecretID:   getEnv("PERSYS_VAULT_APPROLE_SECRET_ID", ""),
		VaultPKIMount:      getEnv("PERSYS_VAULT_PKI_MOUNT", "pki"),
		VaultPKIRole:       getEnv("PERSYS_VAULT_PKI_ROLE", "compute-agent"),
		VaultCertTTL:       getEnvAsDuration("PERSYS_VAULT_CERT_TTL", 24*time.Hour),
		VaultServiceName:   getEnv("PERSYS_VAULT_SERVICE_NAME", "compute-agent"),
		VaultServiceDomain: getEnv("PERSYS_VAULT_SERVICE_DOMAIN", ""),
		VaultRetryInterval: getEnvAsDuration("PERSYS_VAULT_RETRY_INTERVAL", 2*time.Minute),

		// State store defaults
		StateStorePath: getEnv("PERSYS_STATE_PATH", "/var/lib/persys/state.db"),

		// Runtime defaults
		DockerEnabled:  getEnvAsBool("PERSYS_DOCKER_ENABLED", true),
		DockerEndpoint: getEnv("DOCKER_HOST", "unix:///var/run/docker.sock"),
		ComposeEnabled: getEnvAsBool("PERSYS_COMPOSE_ENABLED", true),
		ComposeBinary:  getEnv("PERSYS_COMPOSE_BINARY", "docker compose"),
		VMEnabled:      getEnvAsBool("PERSYS_VM_ENABLED", true),
		LibvirtURI:     getEnv("PERSYS_LIBVIRT_URI", "qemu:///system"),

		// Reconciliation defaults
		ReconcileInterval: getEnvAsDuration("PERSYS_RECONCILE_INTERVAL", 30*time.Second),
		ReconcileEnabled:  getEnvAsBool("PERSYS_RECONCILE_ENABLED", true),

		// Logging
		LogLevel: getEnv("PERSYS_LOG_LEVEL", "info"),

		// Metadata
		NodeID:     generateNodeID(),
		Version:    getEnv("PERSYS_VERSION", "dev"),
		NodeRegion: getEnv("PERSYS_NODE_REGION", ""),
		NodeEnv:    getEnv("PERSYS_NODE_ENV", ""),

		// Scheduler defaults
		SchedulerAddr:       getEnv("PERSYS_SCHEDULER_ADDR", "127.0.0.1:8085"),
		SchedulerInsecure:   getEnvAsBool("PERSYS_SCHEDULER_INSECURE", false),
		SchedulerTLSEnabled: !getEnvAsBool("PERSYS_SCHEDULER_INSECURE", false),
		AgentGRPCEndpoint:   getEnv("PERSYS_AGENT_GRPC_ENDPOINT", ""),
	}

	cfg.NodeLabels = parseNodeLabels(
		cfg.NodeRegion,
		cfg.NodeEnv,
		getEnv("PERSYS_NODE_LABELS", ""),
	)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

// Validate checks that the configuration is valid
func (c *Config) Validate() error {
	if c.GRPCPort < 1 || c.GRPCPort > 65535 {
		return fmt.Errorf("invalid GRPC port: %d", c.GRPCPort)
	}

	if c.TLSEnabled {
		if c.TLSCertPath == "" || c.TLSKeyPath == "" || c.TLSCAPath == "" {
			return fmt.Errorf("TLS enabled but certificate paths not configured")
		}
	}

	if c.VaultEnabled {
		if !c.TLSEnabled {
			return fmt.Errorf("vault cert manager requires TLS to be enabled")
		}
		if c.VaultAddr == "" {
			return fmt.Errorf("vault is enabled but PERSYS_VAULT_ADDR is empty")
		}
		if c.VaultPKIMount == "" || c.VaultPKIRole == "" {
			return fmt.Errorf("vault is enabled but PKI mount/role is not configured")
		}
		switch c.VaultAuthMethod {
		case "token":
			if c.VaultToken == "" {
				return fmt.Errorf("vault token auth selected but PERSYS_VAULT_TOKEN is empty")
			}
		case "approle":
			if c.VaultAppRoleID == "" || c.VaultAppSecretID == "" {
				return fmt.Errorf("vault approle auth selected but role_id/secret_id is missing")
			}
		default:
			return fmt.Errorf("unsupported vault auth method %q (expected token|approle)", c.VaultAuthMethod)
		}
		if c.VaultCertTTL <= 0 {
			return fmt.Errorf("vault cert TTL must be positive")
		}
		if c.VaultRetryInterval <= 0 {
			return fmt.Errorf("vault retry interval must be positive")
		}
	}

	if c.StateStorePath == "" {
		return fmt.Errorf("state store path cannot be empty")
	}

	if !c.DockerEnabled && !c.ComposeEnabled && !c.VMEnabled {
		return fmt.Errorf("at least one runtime must be enabled")
	}

	if c.SchedulerAddr == "" {
		return fmt.Errorf("scheduler address cannot be empty")
	}

	return nil
}

// Helper functions
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvAsInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
	}
	return defaultValue
}

func getEnvAsBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolVal, err := strconv.ParseBool(value); err == nil {
			return boolVal
		}
	}
	return defaultValue
}

func getEnvAsDuration(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if duration, err := time.ParseDuration(value); err == nil {
			return duration
		}
	}
	return defaultValue
}

func parseNodeLabels(region, env, labelsRaw string) map[string]string {
	labels := make(map[string]string)

	if region != "" {
		labels["region"] = region
	}
	if env != "" {
		labels["env"] = env
	}

	for _, pair := range strings.Split(labelsRaw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		value := strings.TrimSpace(kv[1])
		if key == "" || value == "" {
			continue
		}
		labels[key] = value
	}

	return labels
}

func getHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}

// generateNodeID creates a unique node identifier
func generateNodeID() string {
	nodeID, err := node.GenerateUniqueNodeID()
	if err != nil {
		// Log warning and return fallback
		nodeID = getHostname()
	}
	return nodeID
}
