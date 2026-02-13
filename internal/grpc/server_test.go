package grpc

import (
	"testing"
	"time"

	"github.com/persys/compute-agent/pkg/models"
)

func TestStatusToProto_MetadataPreserved(t *testing.T) {
	s := &Server{}
	status := &models.WorkloadStatus{
		ID:           "w1",
		Type:         models.WorkloadTypeVM,
		RevisionID:   "r1",
		DesiredState: models.DesiredStateRunning,
		ActualState:  models.ActualStateRunning,
		Message:      "ok",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
		Metadata: map[string]string{
			"vm.primary_ip": "10.0.0.4",
		},
	}
	pbStatus := s.statusToProto(status)
	if pbStatus.Metadata["vm.primary_ip"] != "10.0.0.4" {
		t.Fatal("expected metadata to be preserved")
	}
}
