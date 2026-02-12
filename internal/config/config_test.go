package config

import "testing"

func TestValidate_InvalidPort(t *testing.T) {
	cfg := &Config{GRPCPort: 70000, StateStorePath: "/tmp/state.db", DockerEnabled: true}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid port error")
	}
}

func TestValidate_TLSRequiresPaths(t *testing.T) {
	cfg := &Config{GRPCPort: 50051, TLSEnabled: true, StateStorePath: "/tmp/state.db", DockerEnabled: true}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected TLS path validation error")
	}
}

func TestValidate_AtLeastOneRuntime(t *testing.T) {
	cfg := &Config{GRPCPort: 50051, StateStorePath: "/tmp/state.db"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected runtime validation error")
	}
}
