package workload

import (
	"context"
	stdErrors "errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/persys/compute-agent/internal/errors"
	"github.com/persys/compute-agent/internal/metrics"
	"github.com/persys/compute-agent/internal/resources"
	"github.com/persys/compute-agent/internal/retry"
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
	workloadOpMu    sync.Mutex
	workloadOpLocks map[string]*sync.Mutex

	retryPolicy   *retry.RetryPolicy
	retryTrackers map[string]*retry.RetryTracker
	retryMu       sync.Mutex
}

// NewManager creates a new workload manager
func NewManager(store state.Store, runtimeMgr *runtime.Manager, logger *logrus.Logger) *Manager {
	return &Manager{
		store:           store,
		runtimeMgr:      runtimeMgr,
		logger:          logger.WithField("component", "workload-manager"),
		workloadOpLocks: make(map[string]*sync.Mutex),
		retryPolicy:     retry.DefaultRetryPolicy(),
		retryTrackers:   make(map[string]*retry.RetryTracker),
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

// SetRetryPolicy sets retry policy used by reconciliation when handling failed workloads
func (m *Manager) SetRetryPolicy(policy *retry.RetryPolicy) {
	if policy == nil {
		policy = retry.DefaultRetryPolicy()
	}
	m.retryMu.Lock()
	m.retryPolicy = policy
	m.retryMu.Unlock()
}

func (m *Manager) getRetryTracker(workloadID string) *retry.RetryTracker {
	m.retryMu.Lock()
	defer m.retryMu.Unlock()
	tracker, ok := m.retryTrackers[workloadID]
	if !ok {
		tracker = retry.NewRetryTracker(m.retryPolicy)
		m.retryTrackers[workloadID] = tracker
	}
	return tracker
}

func (m *Manager) resetRetryTracker(workloadID string) {
	m.retryMu.Lock()
	delete(m.retryTrackers, workloadID)
	m.retryMu.Unlock()
}

func (m *Manager) lockWorkloadOp(workloadID string) func() {
	m.workloadOpMu.Lock()
	opLock, ok := m.workloadOpLocks[workloadID]
	if !ok {
		opLock = &sync.Mutex{}
		m.workloadOpLocks[workloadID] = opLock
	}
	m.workloadOpMu.Unlock()

	opLock.Lock()
	return opLock.Unlock
}

func ensureMetadata(status *models.WorkloadStatus) {
	if status.Metadata == nil {
		status.Metadata = make(map[string]string)
	}
}

func isRuntimeMissing(message string) bool {
	msg := strings.ToLower(message)
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no such") ||
		strings.Contains(msg, "does not exist")
}

func (m *Manager) persistFailedStatus(workload *models.Workload, message string, err error) {
	failedStatus, statusErr := m.store.GetStatus(workload.ID)
	if statusErr != nil || failedStatus == nil {
		failedStatus = &models.WorkloadStatus{
			ID:           workload.ID,
			Type:         workload.Type,
			RevisionID:   workload.RevisionID,
			DesiredState: workload.DesiredState,
			CreatedAt:    time.Now(),
		}
	}

	failedStatus.Type = workload.Type
	failedStatus.RevisionID = workload.RevisionID
	failedStatus.DesiredState = workload.DesiredState
	failedStatus.ActualState = models.ActualStateFailed
	failedStatus.Message = message
	failedStatus.UpdatedAt = time.Now()
	ensureMetadata(failedStatus)
	failedStatus.Metadata["last_failure_at"] = time.Now().Format(time.RFC3339)
	if err != nil {
		failedStatus.Metadata["last_error"] = err.Error()
	}

	if saveErr := m.store.SaveStatus(failedStatus); saveErr != nil {
		m.logger.Warnf("Failed to persist failed status for %s: %v", workload.ID, saveErr)
	}
}

func (m *Manager) enrichStatusWithRuntimeMetadata(ctx context.Context, rt runtime.Runtime, workloadID string, status *models.WorkloadStatus) {
	provider, ok := rt.(runtime.StatusMetadataProvider)
	if !ok || status == nil {
		return
	}

	metadata, err := provider.StatusMetadata(ctx, workloadID)
	if err != nil {
		m.logger.Debugf("No runtime metadata available for %s: %v", workloadID, err)
		return
	}

	if len(metadata) == 0 {
		return
	}

	ensureMetadata(status)
	for k, v := range metadata {
		status.Metadata[k] = v
	}
}

// ApplyWorkload applies a workload with revision-based idempotency
func (m *Manager) ApplyWorkload(ctx context.Context, workload *models.Workload) (*models.WorkloadStatus, bool, error) {
	unlock := m.lockWorkloadOp(workload.ID)
	defer unlock()

	m.logger.Infof("Applying workload: %s (type: %s, revision: %s)", workload.ID, workload.Type, workload.RevisionID)

	// Check if workload already exists with same revision (idempotency)
	existing, err := m.store.GetWorkload(workload.ID)
	if err == nil && existing.RevisionID == workload.RevisionID {
		status, err := m.store.GetStatus(workload.ID)
		if err == nil && status != nil {
			missingFromRuntime := status.ActualState == models.ActualStateUnknown && isRuntimeMissing(status.Message)
			if status.ActualState != models.ActualStateFailed && !missingFromRuntime {
				m.logger.Infof("Workload %s already at revision %s, skipping", workload.ID, workload.RevisionID)
				return status, true, nil // skipped=true
			}

			m.logger.Infof(
				"Workload %s at revision %s has non-healthy state (%s), retrying apply",
				workload.ID,
				workload.RevisionID,
				status.ActualState,
			)
		} else {
			m.logger.Warnf(
				"Workload %s revision matches but status unavailable, preserving idempotent skip: %v",
				workload.ID,
				err,
			)
			status = &models.WorkloadStatus{
				ID:           workload.ID,
				Type:         workload.Type,
				RevisionID:   workload.RevisionID,
				DesiredState: workload.DesiredState,
				ActualState:  models.ActualStateUnknown,
				Message:      "status not found",
			}
			return status, true, nil // skipped=true
		}
	}

	// Get runtime for this workload type
	rt, err := m.runtimeMgr.GetRuntime(workload.Type)
	if err != nil {
		return nil, false, fmt.Errorf("runtime not available: %w", err)
	}

	// Check resource availability before persisting workload state or creating runtime resources
	if m.resourceMonitor != nil {
		available, issues, err := m.resourceMonitor.IsResourceAvailable()
		if err != nil {
			return nil, false, fmt.Errorf("failed to validate resource availability: %w", err)
		}

		if !available {
			if m.metrics != nil {
				m.metrics.RecordWorkloadFailed()
				m.metrics.RecordError(string(errors.ErrCodeResourceQuotaExceeded))
			}

			if err := m.store.SaveWorkload(workload); err != nil {
				m.logger.Warnf("Failed to persist resource-constrained workload %s: %v", workload.ID, err)
			}

			m.persistFailedStatus(
				workload,
				fmt.Sprintf("admission rejected due to resource constraints: %v", issues),
				nil,
			)

			workloadErr := errors.NewWorkloadError(
				errors.ErrCodeResourceQuotaExceeded,
				"Insufficient system resources to admit workload",
				workload.ID,
				string(workload.Type),
				nil,
			)
			workloadErr.WithDetail("issues", issues)
			workloadErr.WithDetail("revision", workload.RevisionID)

			return nil, false, workloadErr
		}
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
		m.logger.Errorf("Failed to create workload %s: %v", workload.ID, err)

		// Record failed workload metric
		if m.metrics != nil {
			m.metrics.RecordWorkloadFailed()
		}

		m.persistFailedStatus(workload, fmt.Sprintf("create failed: %v", err), err)

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
			m.logger.Errorf("Failed to start workload %s: %v, leaving desired state for reconciliation", workload.ID, err)

			if delErr := rt.Delete(ctx, workload.ID); delErr != nil {
				m.logger.Warnf("Failed to delete workload during cleanup: %v", delErr)
			}

			m.persistFailedStatus(workload, fmt.Sprintf("start failed: %v", err), err)

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

	m.enrichStatusWithRuntimeMetadata(ctx, rt, workload.ID, status)

	if err := m.store.SaveStatus(status); err != nil {
		return status, false, fmt.Errorf("failed to save status: %w", err)
	}

	m.resetRetryTracker(workload.ID)

	// Record successful workload creation
	if m.metrics != nil {
		m.metrics.RecordWorkloadCreated()
	}

	m.logger.Infof("Successfully applied workload %s", workload.ID)
	return status, false, nil
}

// DeleteWorkload removes a workload
func (m *Manager) DeleteWorkload(ctx context.Context, id string) error {
	unlock := m.lockWorkloadOp(id)
	defer unlock()

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
				m.enrichStatusWithRuntimeMetadata(ctx, rt, id, status)
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
	unlock := m.lockWorkloadOp(id)
	defer unlock()

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
		status.ActualState = models.ActualStateFailed
		status.Message = fmt.Sprintf("runtime status failed: %v", err)
		status.UpdatedAt = time.Now()
		return m.store.SaveStatus(status)
	}

	// Update status
	status.ActualState = actualState
	status.Message = message
	status.UpdatedAt = time.Now()

	// Reconcile state
	needsAction := false

	// Don't reconcile if workload is in a transient state (unknown or pending indicates creation/deletion in progress)
	missingFromRuntime := actualState == models.ActualStateUnknown && isRuntimeMissing(message)
	if actualState == models.ActualStatePending {
		m.logger.Debugf("Skipping reconciliation for %s: workload is in transient state (%s)", id, actualState)
		needsAction = false
	} else if actualState == models.ActualStateUnknown && !missingFromRuntime {
		m.logger.Debugf("Skipping reconciliation for %s: workload state unknown (%s)", id, message)
		needsAction = false
	} else if actualState == models.ActualStateFailed || (missingFromRuntime && workload.DesiredState == models.DesiredStateRunning) {
		if missingFromRuntime {
			m.logger.Infof("Reconciling %s: workload missing from runtime, attempting recreate...", id)
			status.ActualState = models.ActualStateFailed
			if status.Message == "" {
				status.Message = message
			}
			ensureMetadata(status)
			status.Metadata["failure_reason"] = "RUNTIME_RESOURCE_MISSING"
			status.Metadata["last_error"] = status.Message
		} else {
			tracker := m.getRetryTracker(id)
			if tracker.GetAttemptCount() > 0 && !tracker.CanRetryNow() {
				ensureMetadata(status)
				status.Metadata["retry_attempts"] = fmt.Sprintf("%d", tracker.GetAttemptCount())
				status.Metadata["next_retry_time"] = tracker.GetNextRetryTime().Format(time.RFC3339)
				m.logger.Debugf("Skipping retry for %s until %s", id, tracker.GetNextRetryTime().Format(time.RFC3339))
				return m.store.SaveStatus(status)
			}

			reason := retry.ClassifyError(stdErrors.New(status.Message))
			retryResult, recordErr := tracker.RecordFailure(reason, status.Message)
			if recordErr != nil {
				m.logger.Warnf("Failed to record retry metadata for %s: %v", id, recordErr)
			}

			ensureMetadata(status)
			status.Metadata["retry_attempts"] = fmt.Sprintf("%d", tracker.GetAttemptCount())
			status.Metadata["failure_reason"] = string(reason)
			status.Metadata["last_error"] = status.Message

			if retryResult != nil && retryResult.Retryable {
				status.Metadata["next_retry_time"] = retryResult.NextRetryTime.Format(time.RFC3339)
				if !tracker.CanRetryNow() {
					m.logger.Debugf("Skipping retry for %s until %s", id, retryResult.NextRetryTime.Format(time.RFC3339))
					return m.store.SaveStatus(status)
				}
			} else {
				m.logger.Infof("Not retrying failed workload %s (reason: %s)", id, reason)
				return m.store.SaveStatus(status)
			}
		}

		// Retry failed workloads when policy allows
		m.logger.Infof("Reconciling %s: status=failed, attempting retry recreate...", id)
		if m.resourceMonitor != nil {
			available, issues, checkErr := m.resourceMonitor.IsResourceAvailable()
			if checkErr != nil {
				status.ActualState = models.ActualStateFailed
				status.Message = fmt.Sprintf("recreate admission check failed: %v", checkErr)
				status.UpdatedAt = time.Now()
				ensureMetadata(status)
				status.Metadata["last_error"] = status.Message
				return m.store.SaveStatus(status)
			}
			if !available {
				status.ActualState = models.ActualStateFailed
				status.Message = fmt.Sprintf("recreate deferred due to resource constraints: %v", issues)
				status.UpdatedAt = time.Now()
				ensureMetadata(status)
				status.Metadata["failure_reason"] = string(retry.FailureReasonResourceQuotaExceeded)
				status.Metadata["last_error"] = status.Message
				status.Metadata["resource_issues"] = strings.Join(issues, "; ")
				m.logger.Infof("Reconciling %s: retry blocked by resource constraints: %v", id, issues)
				return m.store.SaveStatus(status)
			}
		}

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
					m.resetRetryTracker(id)
					delete(status.Metadata, "retry_attempts")
					delete(status.Metadata, "next_retry_time")
					delete(status.Metadata, "failure_reason")
					delete(status.Metadata, "last_error")
				}
			} else {
				status.ActualState = models.ActualStateStopped
				status.Message = "recreated and left stopped"
				m.resetRetryTracker(id)
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

	// Enrich with runtime metadata when available
	m.enrichStatusWithRuntimeMetadata(ctx, rt, id, status)

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
