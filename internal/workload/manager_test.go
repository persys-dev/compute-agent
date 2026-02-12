package workload

import (
	"context"
	"errors"
	"testing"
	"time"

	errors2 "github.com/persys/compute-agent/internal/errors"
	"github.com/persys/compute-agent/internal/resources"
	"github.com/persys/compute-agent/internal/retry"
	"github.com/persys/compute-agent/internal/runtime"
	"github.com/persys/compute-agent/pkg/models"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// MockStore implements the state.Store interface for testing
type MockStore struct {
	mock.Mock
}

func (m *MockStore) SaveWorkload(workload *models.Workload) error {
	args := m.Called(workload)
	return args.Error(0)
}

func (m *MockStore) GetWorkload(id string) (*models.Workload, error) {
	args := m.Called(id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.Workload), args.Error(1)
}

func (m *MockStore) DeleteWorkload(id string) error {
	args := m.Called(id)
	return args.Error(0)
}

func (m *MockStore) ListWorkloads() ([]*models.Workload, error) {
	args := m.Called()
	return args.Get(0).([]*models.Workload), args.Error(1)
}

func (m *MockStore) SaveStatus(status *models.WorkloadStatus) error {
	args := m.Called(status)
	return args.Error(0)
}

func (m *MockStore) GetStatus(id string) (*models.WorkloadStatus, error) {
	args := m.Called(id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.WorkloadStatus), args.Error(1)
}

func (m *MockStore) ListStatuses() ([]*models.WorkloadStatus, error) {
	args := m.Called()
	return args.Get(0).([]*models.WorkloadStatus), args.Error(1)
}

func (m *MockStore) Close() error {
	args := m.Called()
	return args.Error(0)
}

// MockRuntime implements the runtime.Runtime interface for testing
type MockRuntime struct {
	mock.Mock
}

type runtimeWithMetadata struct {
	*MockRuntime
	metadata map[string]string
	err      error
}

func (r *runtimeWithMetadata) StatusMetadata(ctx context.Context, id string) (map[string]string, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.metadata, nil
}

func (m *MockRuntime) Create(ctx context.Context, workload *models.Workload) error {
	args := m.Called(ctx, workload)
	return args.Error(0)
}

func (m *MockRuntime) Start(ctx context.Context, id string) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *MockRuntime) Stop(ctx context.Context, id string) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *MockRuntime) Delete(ctx context.Context, id string) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *MockRuntime) Status(ctx context.Context, id string) (models.ActualState, string, error) {
	args := m.Called(ctx, id)
	return args.Get(0).(models.ActualState), args.String(1), args.Error(2)
}

func (m *MockRuntime) List(ctx context.Context) ([]string, error) {
	args := m.Called(ctx)
	return args.Get(0).([]string), args.Error(1)
}

func (m *MockRuntime) Type() models.WorkloadType {
	args := m.Called()
	return args.Get(0).(models.WorkloadType)
}

func (m *MockRuntime) Healthy(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

func TestApplyWorkload_NewWorkload(t *testing.T) {
	// Setup
	mockStore := new(MockStore)
	mockRuntime := new(MockRuntime)

	runtimeMgr := runtime.NewManager()
	mockRuntime.On("Type").Return(models.WorkloadTypeContainer)
	runtimeMgr.Register(mockRuntime)

	logger := logrus.New()
	logger.SetLevel(logrus.FatalLevel) // Suppress logs in tests

	manager := NewManager(mockStore, runtimeMgr, logger)

	// Create test workload
	workload := &models.Workload{
		ID:           "test-workload",
		Type:         models.WorkloadTypeContainer,
		RevisionID:   "rev-1",
		DesiredState: models.DesiredStateRunning,
		Spec: map[string]interface{}{
			"image": "nginx:latest",
		},
	}

	// Setup expectations
	mockStore.On("GetWorkload", "test-workload").Return(nil, assert.AnError)
	mockStore.On("SaveWorkload", mock.AnythingOfType("*models.Workload")).Return(nil)
	mockRuntime.On("Create", mock.Anything, workload).Return(nil)
	mockRuntime.On("Start", mock.Anything, "test-workload").Return(nil)
	mockRuntime.On("Status", mock.Anything, "test-workload").Return(
		models.ActualStateRunning, "running", nil,
	)
	mockStore.On("SaveStatus", mock.AnythingOfType("*models.WorkloadStatus")).Return(nil)

	// Execute
	ctx := context.Background()
	status, skipped, err := manager.ApplyWorkload(ctx, workload)

	// Assert
	assert.NoError(t, err)
	assert.False(t, skipped)
	assert.NotNil(t, status)
	assert.Equal(t, "test-workload", status.ID)
	assert.Equal(t, models.ActualStateRunning, status.ActualState)

	// Verify all expectations were met
	mockStore.AssertExpectations(t)
	mockRuntime.AssertExpectations(t)
}

func TestApplyWorkload_SameRevision_Skipped(t *testing.T) {
	// Setup
	mockStore := new(MockStore)
	mockRuntime := new(MockRuntime)

	runtimeMgr := runtime.NewManager()
	mockRuntime.On("Type").Return(models.WorkloadTypeContainer)
	runtimeMgr.Register(mockRuntime)

	logger := logrus.New()
	logger.SetLevel(logrus.FatalLevel)

	manager := NewManager(mockStore, runtimeMgr, logger)

	// Existing workload with same revision
	existing := &models.Workload{
		ID:           "test-workload",
		Type:         models.WorkloadTypeContainer,
		RevisionID:   "rev-1",
		DesiredState: models.DesiredStateRunning,
		Spec: map[string]interface{}{
			"image": "nginx:latest",
		},
	}

	existingStatus := &models.WorkloadStatus{
		ID:           "test-workload",
		Type:         models.WorkloadTypeContainer,
		RevisionID:   "rev-1",
		DesiredState: models.DesiredStateRunning,
		ActualState:  models.ActualStateRunning,
		Message:      "running",
	}

	// Setup expectations
	mockStore.On("GetWorkload", "test-workload").Return(existing, nil)
	mockStore.On("GetStatus", "test-workload").Return(existingStatus, nil)

	// Execute - apply same workload again
	ctx := context.Background()
	status, skipped, err := manager.ApplyWorkload(ctx, existing)

	// Assert
	assert.NoError(t, err)
	assert.True(t, skipped)
	assert.NotNil(t, status)
	assert.Equal(t, "rev-1", status.RevisionID)

	// Verify no runtime operations were called (skipped)
	mockRuntime.AssertNotCalled(t, "Create", mock.Anything, mock.Anything)
	mockRuntime.AssertNotCalled(t, "Start", mock.Anything, mock.Anything)

	mockStore.AssertExpectations(t)
}

func TestApplyWorkload_DifferentRevision_Recreated(t *testing.T) {
	// Setup
	mockStore := new(MockStore)
	mockRuntime := new(MockRuntime)

	runtimeMgr := runtime.NewManager()
	mockRuntime.On("Type").Return(models.WorkloadTypeContainer)
	runtimeMgr.Register(mockRuntime)

	logger := logrus.New()
	logger.SetLevel(logrus.FatalLevel)

	manager := NewManager(mockStore, runtimeMgr, logger)

	// Existing workload with different revision
	existing := &models.Workload{
		ID:           "test-workload",
		Type:         models.WorkloadTypeContainer,
		RevisionID:   "rev-1",
		DesiredState: models.DesiredStateRunning,
		Spec: map[string]interface{}{
			"image": "nginx:1.20",
		},
	}

	// New workload with updated revision
	updated := &models.Workload{
		ID:           "test-workload",
		Type:         models.WorkloadTypeContainer,
		RevisionID:   "rev-2",
		DesiredState: models.DesiredStateRunning,
		Spec: map[string]interface{}{
			"image": "nginx:1.21",
		},
	}

	// Setup expectations
	mockStore.On("GetWorkload", "test-workload").Return(existing, nil)
	mockRuntime.On("Delete", mock.Anything, "test-workload").Return(nil)
	mockStore.On("SaveWorkload", mock.AnythingOfType("*models.Workload")).Return(nil)
	mockRuntime.On("Create", mock.Anything, updated).Return(nil)
	mockRuntime.On("Start", mock.Anything, "test-workload").Return(nil)
	mockRuntime.On("Status", mock.Anything, "test-workload").Return(
		models.ActualStateRunning, "running", nil,
	)
	mockStore.On("SaveStatus", mock.AnythingOfType("*models.WorkloadStatus")).Return(nil)

	// Execute
	ctx := context.Background()
	status, skipped, err := manager.ApplyWorkload(ctx, updated)

	// Assert
	assert.NoError(t, err)
	assert.False(t, skipped)
	assert.NotNil(t, status)
	assert.Equal(t, "rev-2", status.RevisionID)

	// Verify delete was called (recreation)
	mockRuntime.AssertCalled(t, "Delete", mock.Anything, "test-workload")

	mockStore.AssertExpectations(t)
	mockRuntime.AssertExpectations(t)
}

func TestDeleteWorkload(t *testing.T) {
	// Setup
	mockStore := new(MockStore)
	mockRuntime := new(MockRuntime)

	runtimeMgr := runtime.NewManager()
	mockRuntime.On("Type").Return(models.WorkloadTypeContainer)
	runtimeMgr.Register(mockRuntime)

	logger := logrus.New()
	logger.SetLevel(logrus.FatalLevel)

	manager := NewManager(mockStore, runtimeMgr, logger)

	workload := &models.Workload{
		ID:   "test-workload",
		Type: models.WorkloadTypeContainer,
	}

	// Setup expectations
	mockStore.On("GetWorkload", "test-workload").Return(workload, nil)
	mockRuntime.On("Delete", mock.Anything, "test-workload").Return(nil)
	mockStore.On("DeleteWorkload", "test-workload").Return(nil)

	// Execute
	ctx := context.Background()
	err := manager.DeleteWorkload(ctx, "test-workload")

	// Assert
	assert.NoError(t, err)

	mockStore.AssertExpectations(t)
	mockRuntime.AssertExpectations(t)
}

func TestReconcileWorkload_StartStopped(t *testing.T) {
	// Setup
	mockStore := new(MockStore)
	mockRuntime := new(MockRuntime)

	runtimeMgr := runtime.NewManager()
	mockRuntime.On("Type").Return(models.WorkloadTypeContainer)
	runtimeMgr.Register(mockRuntime)

	logger := logrus.New()
	logger.SetLevel(logrus.FatalLevel)

	manager := NewManager(mockStore, runtimeMgr, logger)

	workload := &models.Workload{
		ID:           "test-workload",
		Type:         models.WorkloadTypeContainer,
		DesiredState: models.DesiredStateRunning,
	}

	status := &models.WorkloadStatus{
		ID:           "test-workload",
		Type:         models.WorkloadTypeContainer,
		DesiredState: models.DesiredStateRunning,
		ActualState:  models.ActualStateStopped,
	}

	// Setup expectations
	mockStore.On("GetWorkload", "test-workload").Return(workload, nil)
	mockStore.On("GetStatus", "test-workload").Return(status, nil)
	mockRuntime.On("Status", mock.Anything, "test-workload").Return(
		models.ActualStateStopped, "stopped", nil,
	).Once()
	mockRuntime.On("Start", mock.Anything, "test-workload").Return(nil)
	mockRuntime.On("Status", mock.Anything, "test-workload").Return(
		models.ActualStateRunning, "running", nil,
	).Once()
	mockStore.On("SaveStatus", mock.AnythingOfType("*models.WorkloadStatus")).Return(nil)

	// Execute
	ctx := context.Background()
	err := manager.ReconcileWorkload(ctx, "test-workload")

	// Assert
	assert.NoError(t, err)

	// Verify Start was called
	mockRuntime.AssertCalled(t, "Start", mock.Anything, "test-workload")

	mockStore.AssertExpectations(t)
	mockRuntime.AssertExpectations(t)
}

func TestApplyWorkload_ResourceUnavailable_FailsBeforeCreate(t *testing.T) {
	// Setup
	mockStore := new(MockStore)
	mockRuntime := new(MockRuntime)

	runtimeMgr := runtime.NewManager()
	mockRuntime.On("Type").Return(models.WorkloadTypeContainer)
	runtimeMgr.Register(mockRuntime)

	logger := logrus.New()
	logger.SetLevel(logrus.FatalLevel)

	manager := NewManager(mockStore, runtimeMgr, logger)
	manager.resourceMonitor = nil // ensure default path first

	workload := &models.Workload{
		ID:           "test-workload",
		Type:         models.WorkloadTypeContainer,
		RevisionID:   "rev-1",
		DesiredState: models.DesiredStateRunning,
		Spec: map[string]interface{}{
			"image": "nginx:latest",
		},
	}

	// Configure existing lookup
	mockStore.On("GetWorkload", "test-workload").Return(nil, errors.New("not found"))

	// Attach a real monitor with impossible thresholds so it fails deterministically
	manager.SetResourceMonitor(resources.NewMonitor(&resources.Thresholds{
		MemoryThreshold: -1,
		CPUThreshold:    -1,
		DiskThreshold:   -1,
	}, logger))

	// Execute
	ctx := context.Background()
	status, skipped, err := manager.ApplyWorkload(ctx, workload)

	// Assert
	assert.Error(t, err)
	assert.Nil(t, status)
	assert.False(t, skipped)

	var workloadErr *errors2.WorkloadError
	assert.ErrorAs(t, err, &workloadErr)
	assert.Equal(t, errors2.ErrCodeResourceQuotaExceeded, workloadErr.Code)

	// Verify no create/start or save operations were called
	mockStore.AssertNotCalled(t, "SaveWorkload", mock.Anything)
	mockRuntime.AssertNotCalled(t, "Create", mock.Anything, mock.Anything)
	mockRuntime.AssertNotCalled(t, "Start", mock.Anything, mock.Anything)

	mockStore.AssertExpectations(t)
}

func TestReconcileWorkload_FailedWorkload_RespectsRetryBackoff(t *testing.T) {
	mockStore := new(MockStore)
	mockRuntime := new(MockRuntime)

	runtimeMgr := runtime.NewManager()
	mockRuntime.On("Type").Return(models.WorkloadTypeContainer)
	runtimeMgr.Register(mockRuntime)

	logger := logrus.New()
	logger.SetLevel(logrus.FatalLevel)

	manager := NewManager(mockStore, runtimeMgr, logger)
	manager.SetRetryPolicy(&retry.RetryPolicy{
		MaxAttempts:        3,
		InitialDelay:       1 * time.Hour,
		MaxDelay:           1 * time.Hour,
		BackoffMultiplier:  2,
		OnlyRetryTransient: true,
	})

	workload := &models.Workload{
		ID:           "failed-workload",
		Type:         models.WorkloadTypeContainer,
		DesiredState: models.DesiredStateRunning,
	}

	status := &models.WorkloadStatus{
		ID:           "failed-workload",
		Type:         models.WorkloadTypeContainer,
		DesiredState: models.DesiredStateRunning,
		ActualState:  models.ActualStateFailed,
		Message:      "network timeout while pulling image",
	}

	mockStore.On("GetWorkload", "failed-workload").Return(workload, nil)
	mockStore.On("GetStatus", "failed-workload").Return(status, nil)
	mockRuntime.On("Status", mock.Anything, "failed-workload").Return(
		models.ActualStateFailed, "network timeout while pulling image", nil,
	).Once()

	mockStore.On("SaveStatus", mock.MatchedBy(func(s *models.WorkloadStatus) bool {
		if s.Metadata == nil {
			return false
		}
		return s.Metadata["retry_attempts"] == "1" && s.Metadata["failure_reason"] == "IMAGE_PULL_TIMEOUT"
	})).Return(nil).Once()

	err := manager.ReconcileWorkload(context.Background(), "failed-workload")
	assert.NoError(t, err)

	mockRuntime.AssertNotCalled(t, "Delete", mock.Anything, "failed-workload")
	mockRuntime.AssertNotCalled(t, "Create", mock.Anything, mock.Anything)
	mockRuntime.AssertNotCalled(t, "Start", mock.Anything, "failed-workload")
	mockStore.AssertExpectations(t)
}

func TestGetStatus_SurfacesRuntimeMetadata(t *testing.T) {
	mockStore := new(MockStore)
	baseRuntime := new(MockRuntime)
	wrappedRuntime := &runtimeWithMetadata{
		MockRuntime: baseRuntime,
		metadata: map[string]string{
			"vm.primary_ip":   "10.0.0.5",
			"vm.ip_addresses": "10.0.0.5,fd00::5",
		},
	}

	runtimeMgr := runtime.NewManager()
	baseRuntime.On("Type").Return(models.WorkloadTypeVM)
	runtimeMgr.Register(wrappedRuntime)

	logger := logrus.New()
	logger.SetLevel(logrus.FatalLevel)
	manager := NewManager(mockStore, runtimeMgr, logger)

	workload := &models.Workload{ID: "vm-1", Type: models.WorkloadTypeVM}
	status := &models.WorkloadStatus{ID: "vm-1", Type: models.WorkloadTypeVM, Metadata: map[string]string{}}

	mockStore.On("GetStatus", "vm-1").Return(status, nil)
	mockStore.On("GetWorkload", "vm-1").Return(workload, nil)
	baseRuntime.On("Status", mock.Anything, "vm-1").Return(models.ActualStateRunning, "running", nil)
	mockStore.On("SaveStatus", mock.MatchedBy(func(s *models.WorkloadStatus) bool {
		return s.Metadata["vm.primary_ip"] == "10.0.0.5"
	})).Return(nil)

	got, err := manager.GetStatus(context.Background(), "vm-1")
	assert.NoError(t, err)
	assert.Equal(t, "10.0.0.5", got.Metadata["vm.primary_ip"])
	assert.Equal(t, "10.0.0.5,fd00::5", got.Metadata["vm.ip_addresses"])

	mockStore.AssertExpectations(t)
	baseRuntime.AssertExpectations(t)
}
