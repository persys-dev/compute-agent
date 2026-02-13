package state

import (
	"path/filepath"
	"testing"

	"github.com/persys/compute-agent/pkg/models"
)

func TestBoltStore_WorkloadAndStatusLifecycle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	s, err := NewBoltStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer s.Close()

	w := &models.Workload{ID: "w1", Type: models.WorkloadTypeContainer, RevisionID: "r1"}
	if err := s.SaveWorkload(w); err != nil {
		t.Fatalf("save workload failed: %v", err)
	}
	if _, err := s.GetWorkload("w1"); err != nil {
		t.Fatalf("get workload failed: %v", err)
	}

	st := &models.WorkloadStatus{ID: "w1", Type: models.WorkloadTypeContainer}
	if err := s.SaveStatus(st); err != nil {
		t.Fatalf("save status failed: %v", err)
	}
	if _, err := s.GetStatus("w1"); err != nil {
		t.Fatalf("get status failed: %v", err)
	}

	if err := s.DeleteWorkload("w1"); err != nil {
		t.Fatalf("delete workload failed: %v", err)
	}
	if _, err := s.GetWorkload("w1"); err == nil {
		t.Fatal("expected workload to be deleted")
	}
}
