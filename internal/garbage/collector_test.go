package garbage

import "testing"

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Enabled {
		t.Fatal("expected gc enabled by default")
	}
	if cfg.Interval <= 0 || cfg.FailedWorkloadTTL <= 0 {
		t.Fatal("expected positive gc intervals")
	}
}
