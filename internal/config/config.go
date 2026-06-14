package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/persys-dev/persys-cloud/compute-agent/internal/node"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// Config holds all agent configuration
type Config struct {
	// Server configuration
	GRPCAddr    string `mapstructure:"grpc_addr"`
	GRPCPort    int    `mapstructure:"grpc_port"`
	MetricsPort int    `mapstructure:"metrics_port"`

	// TLS/mTLS configuration
	TLSEnabled  bool   `mapstructure:"tls_enabled"`
	TLSCertPath string `mapstructure:"tls_cert_path"`
	TLSKeyPath  string `mapstructure:"tls_key_path"`
	TLSCAPath   string `mapstructure:"tls_ca_path"`

	// Vault certificate manager configuration
	VaultEnabled       bool          `mapstructure:"vault_enabled"`
	VaultAddr          string        `mapstructure:"vault_addr"`
	VaultAuthMethod    string        `mapstructure:"vault_auth_method"`
	VaultToken         string        `mapstructure:"vault_token"`
	VaultAppRoleID     string        `mapstructure:"vault_approle_role_id"`
	VaultAppSecretID   string        `mapstructure:"vault_approle_secret_id"`
	VaultPKIMount      string        `mapstructure:"vault_pki_mount"`
	VaultPKIRole       string        `mapstructure:"vault_pki_role"`
	VaultCertTTL       time.Duration `mapstructure:"vault_cert_ttl"`
	VaultServiceName   string        `mapstructure:"vault_service_name"`
	VaultServiceDomain string        `mapstructure:"vault_service_domain"`
	VaultRetryInterval time.Duration `mapstructure:"vault_retry_interval"`

	// State store configuration
	StateStorePath string `mapstructure:"state_store_path"`

	// Runtime configuration
	DockerEnabled  bool   `mapstructure:"docker_enabled"`
	DockerEndpoint string `mapstructure:"docker_endpoint"`
	ComposeEnabled bool   `mapstructure:"compose_enabled"`
	ComposeBinary  string `mapstructure:"compose_binary"`
	VMEnabled      bool   `mapstructure:"vm_enabled"`
	LibvirtURI     string `mapstructure:"libvirt_uri"`

	// Managed storage provider configuration
	StorageLocalRoot    string `mapstructure:"storage_local_root"`
	StorageNFSStageDir  string `mapstructure:"storage_nfs_stage_dir"`
	StorageNFSServer    string `mapstructure:"storage_nfs_server"`
	StorageNFSExport    string `mapstructure:"storage_nfs_export"`
	StorageNFSOptions   string `mapstructure:"storage_nfs_options"`
	StorageCephStageDir string `mapstructure:"storage_ceph_stage_dir"`
	StorageCephCluster  string `mapstructure:"storage_ceph_cluster"`
	StorageCephPool     string `mapstructure:"storage_ceph_pool"`
	StorageCephUser     string `mapstructure:"storage_ceph_user"`
	StorageCephKeyring  string `mapstructure:"storage_ceph_keyring"`

	// Reconciliation configuration
	ReconcileInterval time.Duration `mapstructure:"reconcile_interval"`
	ReconcileEnabled  bool          `mapstructure:"reconcile_enabled"`

	// Logging
	LogLevel string `mapstructure:"log_level"`

	// Agent metadata
	NodeID     string            `mapstructure:"node_id"`
	Version    string            `mapstructure:"version"`
	NodeRegion string            `mapstructure:"node_region"`
	NodeEnv    string            `mapstructure:"node_env"`
	NodeLabels map[string]string `mapstructure:"node_labels"`

	// Scheduler control-plane configuration
	SchedulerAddr       string `mapstructure:"scheduler_addr"`
	SchedulerInsecure   bool   `mapstructure:"scheduler_insecure"`
	SchedulerTLSEnabled bool
	AgentGRPCEndpoint   string `mapstructure:"agent_grpc_endpoint"`

	// OpenTelemetry configuration
	OTELExporterEndpoint string `mapstructure:"otlp_endpoint"`
}

var (
	// Global flag set (can be accessed from main if needed)
	fs = pflag.NewFlagSet("compute-agent", pflag.ContinueOnError)
)

// Load loads configuration with Viper + pflag
func Load() (*Config, error) {
	v := viper.New()

	// Setup Viper
	v.SetConfigName("agent_config")
	v.SetConfigType("yaml")
	v.SetEnvPrefix("PERSYS")
	v.AutomaticEnv()
	v.BindEnv("state_store_path", "PERSYS_STATE_PATH", "PERSYS_STATE_STORE_PATH")
	v.BindEnv("node_labels")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))

	// Handle PERSYS_NODE_LABELS environment variable (comma-separated)
	if labelsEnv := os.Getenv("PERSYS_NODE_LABELS"); labelsEnv != "" {
		v.Set("node_labels", parseLabelsEnv(labelsEnv))
	}

	// Bind CLI flags
	fs.String("config", "", "Path to config file")
	fs.Parse(os.Args[1:]) // Parse early (ContinueOnError)

	if cfgFile := fs.Lookup("config").Value.String(); cfgFile != "" {
		v.SetConfigFile(cfgFile)
	} else if envFile := os.Getenv("PERSYS_CONFIG_FILE"); envFile != "" {
		v.SetConfigFile(envFile)
	}

	// Add search paths
	for _, path := range getConfigSearchPaths() {
		v.AddConfigPath(path)
	}

	// Read config (graceful if file missing)
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("config file error: %w", err)
		}
		// fmt.Println("No config file found → using defaults + ENV + flags")
	}

	// Unmarshal
	cfg := defaultConfig()
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	// Post-processing
	cfg.SchedulerTLSEnabled = !cfg.SchedulerInsecure

	if cfg.NodeID == "" {
		cfg.NodeID = generateNodeID()
	}

	// Ensure NodeLabels map exists and apply defaults + region/env
	if cfg.NodeLabels == nil {
		cfg.NodeLabels = make(map[string]string)
	}
	cfg.NodeLabels = mergeWithDefaultLabels(cfg.NodeLabels)
	cfg.NodeLabels = parseNodeLabels(cfg.NodeRegion, cfg.NodeEnv, cfg.NodeLabels)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	fmt.Printf("✅ Config loaded from: %s | NodeID: %s\n", v.ConfigFileUsed(), cfg.NodeID)
	return cfg, nil
}

// getConfigSearchPaths returns possible locations for agent_config.yaml
func getConfigSearchPaths() []string {
	paths := []string{"/etc/persys"}

	if os.Geteuid() != 0 {
		if home, err := os.UserHomeDir(); err == nil {
			paths = append(paths, filepath.Join(home, ".persys"))
		}
	}
	paths = append(paths, ".")
	return paths
}

// defaultConfig returns a Config with sensible defaults
func defaultConfig() *Config {
	return &Config{
		GRPCAddr:    "0.0.0.0",
		GRPCPort:    50051,
		MetricsPort: 8089,

		TLSEnabled:  true,
		TLSCertPath: "/etc/persys/certs/agent/compute-agent.pem",
		TLSKeyPath:  "/etc/persys/certs/agent/compute-agent-key.pem",
		TLSCAPath:   "/etc/persys/certs/agent/ca.pem",

		VaultEnabled:       true,
		VaultAddr:          "http://localhost:8200",
		VaultAuthMethod:    "approle",
		VaultPKIMount:      "pki",
		VaultPKIRole:       "compute-agent",
		VaultCertTTL:       24 * time.Hour,
		VaultRetryInterval: 2 * time.Minute,

		StateStorePath: "/var/lib/persys/state.db",

		DockerEnabled:  true,
		DockerEndpoint: "unix:///var/run/docker.sock",
		ComposeEnabled: true,
		ComposeBinary:  "docker compose",
		VMEnabled:      true,
		LibvirtURI:     "qemu:///system",

		StorageLocalRoot:    "/var/lib/persys/volumes/local",
		StorageNFSStageDir:  "/var/lib/persys/volumes/nfs",
		StorageCephStageDir: "/var/lib/persys/volumes/ceph-rbd",

		ReconcileInterval: 30 * time.Second,
		ReconcileEnabled:  true,

		LogLevel: "info",

		SchedulerAddr:     "127.0.0.1:8085",
		SchedulerInsecure: false,
	}
}

// Validate ensures config correctness
func (c *Config) Validate() error {
	if c.GRPCPort < 1 || c.GRPCPort > 65535 {
		return fmt.Errorf("invalid GRPC port: %d", c.GRPCPort)
	}
	if c.MetricsPort < 1 || c.MetricsPort > 65535 {
		return fmt.Errorf("invalid metrics port: %d", c.MetricsPort)
	}
	if c.TLSEnabled {
		if c.TLSCertPath == "" || c.TLSKeyPath == "" || c.TLSCAPath == "" {
			return fmt.Errorf("TLS enabled but certificate paths not configured")
		}
	}
	if c.VaultEnabled {
		if !c.TLSEnabled {
			return fmt.Errorf("vault requires TLS enabled")
		}
		if c.VaultAddr == "" {
			return fmt.Errorf("vault enabled but addr is empty")
		}
		switch strings.ToLower(c.VaultAuthMethod) {
		case "token":
			if c.VaultToken == "" {
				return fmt.Errorf("vault token auth selected but token is empty")
			}
		case "approle":
			if c.VaultAppRoleID == "" || c.VaultAppSecretID == "" {
				return fmt.Errorf("vault approle auth selected but role_id/secret_id missing")
			}
		default:
			return fmt.Errorf("unsupported vault auth method %q", c.VaultAuthMethod)
		}
		if c.VaultCertTTL <= 0 {
			return fmt.Errorf("vault cert TTL must be positive")
		}
		if c.VaultRetryInterval <= 0 {
			return fmt.Errorf("vault retry interval must be positive")
		}
	}
	if !c.DockerEnabled && !c.ComposeEnabled && !c.VMEnabled {
		return fmt.Errorf("at least one runtime must be enabled")
	}
	if c.SchedulerAddr == "" {
		return fmt.Errorf("scheduler address cannot be empty")
	}
	return nil
}

// mergeWithDefaultLabels adds sensible default labels if they are not already set
func mergeWithDefaultLabels(labels map[string]string) map[string]string {
	defaults := map[string]string{
		"os":   runtime.GOOS,
		"arch": runtime.GOARCH,
		// Add more useful defaults here in the future (e.g. hostname, etc.)
	}

	for k, v := range defaults {
		if _, exists := labels[k]; !exists {
			labels[k] = v
		}
	}
	return labels
}

// parseNodeLabels merges region/env with user-provided labels (region/env take precedence)
func parseNodeLabels(region, env string, raw map[string]string) map[string]string {
	if raw == nil {
		raw = make(map[string]string)
	}

	if region != "" {
		raw["region"] = region
	}
	if env != "" {
		raw["env"] = env
	}
	return raw
}

// generateNodeID creates unique node identifier
func generateNodeID() string {
	id, err := node.GenerateUniqueNodeID()
	if err != nil {
		id = getHostname()
	}
	return id
}

func getHostname() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "unknown"
}

// parseLabelsEnv parses comma-separated key=value pairs for PERSYS_NODE_LABELS
func parseLabelsEnv(s string) map[string]string {
	labels := make(map[string]string)
	if s == "" {
		return labels
	}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		if idx := strings.Index(pair, "="); idx > 0 {
			k := strings.TrimSpace(pair[:idx])
			v := strings.TrimSpace(pair[idx+1:])
			if k != "" && v != "" {
				labels[k] = v
			}
		}
	}
	return labels
}
