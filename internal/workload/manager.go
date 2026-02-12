package workload

import (
	"context"
	"fmt"
	"time"

	"github.com/persys/compute-agent/internal/errors"
	"github.com/persys/compute-agent/internal/metrics"
	"github.com/persys/compute-agent/internal/resources"
	"github.com/persys/compute-agent/internal/runtime"
	"github.com/persys/compute-agent/internal/state"
	"github.com/persys/compute-agent/pkg/models"
	"github.com/sirupsen/logrus"
)

// Manager handles workload lifecycle with idempotency
type Manager struct {
	store           state.Store
	runtimeMgr      *runtime.Manager
	logger          *logrus.Entry
	metrics         *metrics.Metrics
	resourceMonitor *resources.Monitor
}

// NewManager creates a new workload manager
func NewManager(store state.Store, runtimeMgr *runtime.Manager, logger *logrus.Logger) *Manager {
	return &Manager{
		store:      store,
		runtimeMgr: runtimeMgr,
		logger:     logger.WithField("component", "workload-manager"),
	}
}

// SetMetrics sets the metrics instance for recording operational metrics
func (m *Manager) SetMetrics(metricsInst *metrics.Metrics) {
	m.metrics = metricsInst
}

// SetResourceMonitor sets the resource monitor for availability checks
func (m *Manager) SetResourceMonitor(monitor *resources.Monitor) {
	m.resourceMonitor = monitor
}

// ApplyWorkload applies a workload with revision-based idempotency
func (m *Manager) ApplyWorkload(ctx context.Context, workload *models.Workload) (*models.WorkloadStatus, bool, error) {
	m.logger.Infof("Applying workload: %s (type: %s, revision: %s)", workload.ID, workload.Type, workload.RevisionID)

	// Check if workload already exists with same revision (idempotency)
	existing, err := m.store.GetWorkload(workload.ID)
	if err == nil && existing.RevisionID == workload.RevisionID {
		m.logger.Infof("Workload %s already at revision %s, skipping", workload.ID, workload.RevisionID)

		// Return current status
		status, err := m.store.GetStatus(workload.ID)
		if err != nil {
			status = &models.WorkloadStatus{
				ID:           workload.ID,
				Type:         workload.Type,
				RevisionID:   workload.RevisionID,
				DesiredState: workload.DesiredState,
				ActualState:  models.ActualStateUnknown,
				Message:      "status not found",
			}
		}
		return status, true, nil // skipped=true
	}

	// Get runtime for this workload type
	rt, err := m.runtimeMgr.GetRuntime(workload.Type)
	if err != nil {
		return nil, false, fmt.Errorf("runtime not available: %w", err)
	}

	// If revision changed, we need to recreate
	if existing != nil && existing.RevisionID != workload.RevisionID {
		m.logger.Infof("Workload %s revision changed from %s to %s, recreating",
			workload.ID, existing.RevisionID, workload.RevisionID)

		// Delete old workload
		if err := rt.Delete(ctx, workload.ID); err != nil {
			m.logger.Warnf("Failed to delete old workload: %v", err)
		}
	}

	// Save workload to state store
	if err := m.store.SaveWorkload(workload); err != nil {
		return nil, false, fmt.Errorf("failed to save workload: %w", err)
	}

	// Save initial pending status to indicate creation is in progress
	initialStatus := &models.WorkloadStatus{
		ID:           workload.ID,
		Type:         workload.Type,
		RevisionID:   workload.RevisionID,
		DesiredState: workload.DesiredState,
		ActualState:  models.ActualStatePending,
		Message:      "creating workload",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	if err := m.store.SaveStatus(initialStatus); err != nil {
		m.logger.Warnf("Failed to save initial status: %v", err)
	}

	// Create the workload in the runtime
	startTime := time.Now()
	if err := rt.Create(ctx, workload); err != nil {
		// On creation failure, remove the workload from state to prevent ghost workloads
		m.logger.Errorf("Failed to create workload %s: %v, removing from state", workload.ID, err)

		// Record failed workload metric
		if m.metrics != nil {
			m.metrics.RecordWorkloadFailed()
		}

		// Best effort deletion from state
		if delErr := m.store.DeleteWorkload(workload.ID); delErr != nil {
			m.logger.Warnf("Failed to clean up failed workload %s from state: %v", workload.ID, delErr)
		}

		// Create detailed error
		workloadErr := errors.NewWorkloadError(
			errors.ErrCodeCreateFailed,
			"Failed to create workload in runtime",
			workload.ID,
			string(workload.Type),
			err,
		)
		workloadErr.WithDetail("type", workload.Type)
		workloadErr.WithDetail("revision", workload.RevisionID)

		return nil, false, workloadErr
	}

	// Record apply duration
	if m.metrics != nil {
		m.metrics.RecordApplyWorkloadDuration(time.Since(startTime))
	}

	// Start if desired state is running
	if workload.DesiredState == models.DesiredStateRunning {
		if err := rt.Start(ctx, workload.ID); err != nil {
			// On start failure, also remove the workload (delete what we created)
			m.logger.Errorf("Failed to start workload %s: %v, cleaning up and removing from state", workload.ID, err)

			if delErr := rt.Delete(ctx, workload.ID); delErr != nil {
				m.logger.Warnf("Failed to delete workload during cleanup: %v", delErr)
			}

			if delErr := m.store.DeleteWorkload(workload.ID); delErr != nil {
				m.logger.Warnf("Failed to clean up failed workload %s from state: %v", workload.ID, delErr)
			}

			workloadErr := errors.NewWorkloadError(
				errors.ErrCodeStartFailed,
				"Failed to start workload",
				workload.ID,
				string(workload.Type),
				err,
			)
			workloadErr.WithDetail("type", workload.Type)
			workloadErr.WithDetail("revision", workload.RevisionID)

			return nil, false, workloadErr
		}
	}

	// Get final status
	actualState, message, err := rt.Status(ctx, workload.ID)
	if err != nil {
		m.logger.Warnf("Failed to get status for %s: %v", workload.ID, err)
		actualState = models.ActualStateUnknown
		message = fmt.Sprintf("status check failed: %v", err)
	}

	status := &models.WorkloadStatus{
		ID:           workload.ID,
		Type:         workload.Type,
		RevisionID:   workload.RevisionID,
		DesiredState: workload.DesiredState,
		ActualState:  actualState,
		Message:      message,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	if err := m.store.SaveStatus(status); err != nil {
		return status, false, fmt.Errorf("failed to save status: %w", err)
	}

	// Record successful workload creation
	if m.metrics != nil {
		m.metrics.RecordWorkloadCreated()
	}

	m.logger.Infof("Successfully applied workload %s", workload.ID)
	return status, false, nil
}

// DeleteWorkload removes a workload
func (m *Manager) DeleteWorkload(ctx context.Context, id string) error {
	m.logger.Infof("Deleting workload: %s", id)

	startTime := time.Now()

	// Get workload from store
	workload, err := m.store.GetWorkload(id)
	if err != nil {
		return fmt.Errorf("workload not found: %w", err)
	}

	// Get runtime
	rt, err := m.runtimeMgr.GetRuntime(workload.Type)
	if err != nil {
		return fmt.Errorf("runtime not available: %w", err)
	}

	// Delete from runtime
	if err := rt.Delete(ctx, id); err != nil {
		m.logger.Warnf("Failed to delete workload from runtime: %v", err)
	}

	// Remove from state store
	if err := m.store.DeleteWorkload(id); err != nil {
		return fmt.Errorf("failed to delete from state: %w", err)
	}

	// Record deletion metrics
	if m.metrics != nil {
		m.metrics.RecordWorkloadDeleted()
		m.metrics.RecordDeleteWorkloadDuration(time.Since(startTime))
	}

	m.logger.Infof("Successfully deleted workload %s", id)
	return nil
}

// GetStatus returns the current status of a workload
func (m *Manager) GetStatus(ctx context.Context, id string) (*models.WorkloadStatus, error) {
	// Get from store first
	status, err := m.store.GetStatus(id)
	if err != nil {
		return nil, fmt.Errorf("status not found: %w", err)
	}

	// Optionally refresh from runtime
	workload, err := m.store.GetWorkload(id)
	if err == nil {
		rt, err := m.runtimeMgr.GetRuntime(workload.Type)
		if err == nil {
			actualState, message, err := rt.Status(ctx, id)
			if err == nil {
				status.ActualState = actualState
				status.Message = message
				status.UpdatedAt = time.Now()
				m.store.SaveStatus(status)
			}
		}
	}

	return status, nil
}

// ListWorkloads returns all workloads
func (m *Manager) ListWorkloads(ctx context.Context, workloadType *models.WorkloadType) ([]*models.WorkloadStatus, error) {
	statuses, err := m.store.ListStatuses()
	if err != nil {
		return nil, fmt.Errorf("failed to list statuses: %w", err)
	}

	// Filter by type if specified
	if workloadType != nil {
		var filtered []*models.WorkloadStatus
		for _, status := range statuses {
			if status.Type == *workloadType {
				filtered = append(filtered, status)
			}
		}
		return filtered, nil
	}

	return statuses, nil
}

// ReconcileWorkload ensures workload state matches desired state
func (m *Manager) ReconcileWorkload(ctx context.Context, id string) error {
	workload, err := m.store.GetWorkload(id)
	if err != nil {
		return fmt.Errorf("workload not found: %w", err)
	}

	status, err := m.store.GetStatus(id)
	if err != nil {
		return fmt.Errorf("status not found: %w", err)
	}

	rt, err := m.runtimeMgr.GetRuntime(workload.Type)
	if err != nil {
		return fmt.Errorf("runtime not available: %w", err)
	}

	// Get current actual state
	actualState, message, err := rt.Status(ctx, id)
	if err != nil {
		m.logger.Warnf("Failed to get status for %s: %v", id, err)
		return err
	}

	// Update status
	status.ActualState = actualState
	status.Message = message
	status.UpdatedAt = time.Now()

	// Reconcile state
	needsAction := false

	// Don't reconcile if workload is in a transient state (unknown or pending indicates creation/deletion in progress)
	if actualState == models.ActualStateUnknown || actualState == models.ActualStatePending {
		m.logger.Debugf("Skipping reconciliation for %s: workload is in transient state (%s)", id, actualState)
		needsAction = false
	} else if actualState == models.ActualStateFailed {
		// Retry failed workloads - they may have failed due to transient issues like network timeouts
		m.logger.Infof("Reconciling %s: status=failed, attempting to recreate...", id)

		// Delete the failed workload first
		if err := rt.Delete(ctx, id); err != nil {
			m.logger.Warnf("Failed to delete failed workload %s: %v", id, err)
		}

		// Try to recreate
		if err := rt.Create(ctx, workload); err != nil {
			m.logger.Errorf("Failed to recreate %s during reconciliation: %v", id, err)
			status.ActualState = models.ActualStateFailed
			status.Message = fmt.Sprintf("recreate failed: %v", err)
		} else {
			m.logger.Infof("Successfully recreated %s during reconciliation", id)
			// Start if desired state is running
			if workload.DesiredState == models.DesiredStateRunning {
				if err := rt.Start(ctx, id); err != nil {
					m.logger.Errorf("Failed to start recreated %s: %v", id, err)
					status.ActualState = models.ActualStateFailed
					status.Message = fmt.Sprintf("start after recreate failed: %v", err)
				} else {
					status.ActualState = models.ActualStateRunning
					status.Message = "running"
				}
			}
		}
		needsAction = true
	} else if workload.DesiredState == models.DesiredStateRunning && actualState != models.ActualStateRunning {
		m.logger.Infof("Reconciling %s: desired=running, actual=%s, starting...", id, actualState)
		if err := rt.Start(ctx, id); err != nil {
			m.logger.Errorf("Failed to start %s during reconciliation: %v", id, err)
			status.ActualState = models.ActualStateFailed
			status.Message = fmt.Sprintf("reconcile start failed: %v", err)
		}
		needsAction = true
	} else if workload.DesiredState == models.DesiredStateStopped && actualState == models.ActualStateRunning {
		m.logger.Infof("Reconciling %s: desired=stopped, actual=running, stopping...", id)
		if err := rt.Stop(ctx, id); err != nil {
			m.logger.Errorf("Failed to stop %s during reconciliation: %v", id, err)
			status.ActualState = models.ActualStateFailed
			status.Message = fmt.Sprintf("reconcile stop failed: %v", err)
		}
		needsAction = true
	}

	if needsAction {
		// Recheck status after action
		actualState, message, err = rt.Status(ctx, id)
		if err == nil {
			status.ActualState = actualState
			status.Message = message
		}
	}

	// Save updated status
	return m.store.SaveStatus(status)
}

// UpdateWorkloadMetrics updates gauge metrics with current workload counts
func (m *Manager) UpdateWorkloadMetrics() error {
	if m.metrics == nil {
		return nil
	}

	statuses, err := m.store.ListStatuses()
	if err != nil {
		return fmt.Errorf("failed to list statuses: %w", err)
	}

	// Count by state and type
	counts := make(map[string]map[string]int) // [state][type] = count

	for _, status := range statuses {
		state := string(status.ActualState)
		wType := string(status.Type)

		if counts[state] == nil {
			counts[state] = make(map[string]int)
		}
		counts[state][wType]++
	}

	// Record metrics for each state and type combination
	for state, types := range counts {
		for wType, count := range types {
			m.metrics.RecordWorkloadCount(state, wType, count)
		}
	}

	return nil
}
