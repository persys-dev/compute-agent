package storage

import (
	"fmt"
	"sync"
	"time"
)

// PoolType represents the type of storage pool
type PoolType string

const (
	PoolTypeLocal   PoolType = "local"   // Local filesystem
	PoolTypeNFS     PoolType = "nfs"     // NFS mount
	PoolTypeISCSI   PoolType = "iscsi"   // iSCSI target
	PoolTypeLVM     PoolType = "lvm"     // LVM logical volumes
	PoolTypeGluster PoolType = "gluster" // Gluster distributed storage
)

// Pool represents a storage pool configuration
type Pool struct {
	// Identification
	Name        string   // Unique pool name
	Type        PoolType // Pool backend type
	UUID        string   // Unique identifier
	CreatedAt   time.Time
	LastUpdated time.Time

	// Capacity
	TotalSizeGB     int64 // Total pool size in GB
	AvailableSizeGB int64 // Available space in GB
	AllocatedSizeGB int64 // Currently allocated in GB

	// Configuration
	Config map[string]string // Backend-specific configuration
	// For local: "path" -> mount point
	// For NFS: "server", "export", "mount_options"
	// For iSCSI: "target", "portal", "username", "password"
	// For LVM: "vg_name" -> volume group name

	// Settings
	Quota            int64 // Hard limit on pool (0 = unlimited)
	WarningThreshold int   // Percentage (e.g., 80 for 80% full warning)

	// Status
	Active  bool
	Healthy bool
	Status  string // "healthy", "degraded", "offline", "error"

	// Metadata
	Labels map[string]string
}

// DiskAllocation represents a disk allocated from a pool
type DiskAllocation struct {
	ID         string // Unique ID
	PoolName   string // Associated pool
	Name       string // Disk/volume name
	SizeGB     int64
	Path       string // Mount path or device path
	Format     string // qcow2, raw, ext4, etc.
	CreatedAt  time.Time
	AttachedTo string // Workload ID if attached
	ReadOnly   bool
}

// Manager handles storage pool lifecycle
type Manager struct {
	pools       map[string]*Pool
	allocations map[string]*DiskAllocation
	mu          sync.RWMutex
}

// NewManager creates a new storage manager
func NewManager() *Manager {
	return &Manager{
		pools:       make(map[string]*Pool),
		allocations: make(map[string]*DiskAllocation),
	}
}

// CreatePool creates a new storage pool
func (m *Manager) CreatePool(pool *Pool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if pool.Name == "" {
		return fmt.Errorf("pool name cannot be empty")
	}

	if _, exists := m.pools[pool.Name]; exists {
		return fmt.Errorf("pool %s already exists", pool.Name)
	}

	if pool.TotalSizeGB <= 0 {
		return fmt.Errorf("pool size must be positive")
	}

	if pool.AvailableSizeGB == 0 {
		pool.AvailableSizeGB = pool.TotalSizeGB
	}

	if pool.CreatedAt.IsZero() {
		pool.CreatedAt = time.Now()
	}

	if pool.Config == nil {
		pool.Config = make(map[string]string)
	}

	if pool.Labels == nil {
		pool.Labels = make(map[string]string)
	}

	m.pools[pool.Name] = pool
	return nil
}

// GetPool retrieves a pool by name
func (m *Manager) GetPool(name string) (*Pool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	pool, exists := m.pools[name]
	if !exists {
		return nil, fmt.Errorf("pool %s not found", name)
	}

	return pool, nil
}

// ListPools returns all pools
func (m *Manager) ListPools() []*Pool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	pools := make([]*Pool, 0, len(m.pools))
	for _, pool := range m.pools {
		pools = append(pools, pool)
	}
	return pools
}

// ListPoolsByType returns pools of a specific type
func (m *Manager) ListPoolsByType(poolType PoolType) []*Pool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var pools []*Pool
	for _, pool := range m.pools {
		if pool.Type == poolType {
			pools = append(pools, pool)
		}
	}
	return pools
}

// DeletePool removes a storage pool
func (m *Manager) DeletePool(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if pool has allocations
	for _, alloc := range m.allocations {
		if alloc.PoolName == name {
			return fmt.Errorf("pool %s has active allocations", name)
		}
	}

	delete(m.pools, name)
	return nil
}

// AllocateDisk allocates disk space from a pool
func (m *Manager) AllocateDisk(poolName string, sizeGB int64, format string) (*DiskAllocation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pool, exists := m.pools[poolName]
	if !exists {
		return nil, fmt.Errorf("pool %s not found", poolName)
	}

	if sizeGB <= 0 {
		return nil, fmt.Errorf("disk size must be positive")
	}

	if sizeGB > pool.AvailableSizeGB {
		return nil, fmt.Errorf("insufficient space in pool %s: needed %dGB, available %dGB",
			poolName, sizeGB, pool.AvailableSizeGB)
	}

	// Check quota if set
	newAllocated := pool.AllocatedSizeGB + sizeGB
	if pool.Quota > 0 && newAllocated > pool.Quota {
		return nil, fmt.Errorf("allocation exceeds quota for pool %s", poolName)
	}

	// Create allocation
	diskID := fmt.Sprintf("disk-%d", time.Now().UnixNano())
	allocation := &DiskAllocation{
		ID:        diskID,
		PoolName:  poolName,
		Name:      diskID,
		SizeGB:    sizeGB,
		Format:    format,
		CreatedAt: time.Now(),
	}

	// Update pool metrics
	pool.AvailableSizeGB -= sizeGB
	pool.AllocatedSizeGB = newAllocated
	pool.LastUpdated = time.Now()

	m.allocations[allocation.ID] = allocation

	// Check warning threshold
	utilizationPercent := float64(pool.AllocatedSizeGB) / float64(pool.TotalSizeGB) * 100
	if int(utilizationPercent) >= pool.WarningThreshold {
		pool.Status = "warning: utilization above threshold"
	}

	return allocation, nil
}

// FreeDisk releases allocated disk space back to the pool
func (m *Manager) FreeDisk(diskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	allocation, exists := m.allocations[diskID]
	if !exists {
		return fmt.Errorf("disk allocation %s not found", diskID)
	}

	if allocation.AttachedTo != "" {
		return fmt.Errorf("disk %s is currently attached to %s", diskID, allocation.AttachedTo)
	}

	pool, exists := m.pools[allocation.PoolName]
	if !exists {
		return fmt.Errorf("pool %s not found", allocation.PoolName)
	}

	// Update pool metrics
	pool.AvailableSizeGB += allocation.SizeGB
	pool.AllocatedSizeGB -= allocation.SizeGB
	pool.LastUpdated = time.Now()

	delete(m.allocations, diskID)
	return nil
}

// GetDiskAllocation retrieves an allocation by ID
func (m *Manager) GetDiskAllocation(diskID string) (*DiskAllocation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	alloc, exists := m.allocations[diskID]
	if !exists {
		return nil, fmt.Errorf("disk allocation %s not found", diskID)
	}

	return alloc, nil
}

// ListAllocations returns all allocations for a pool
func (m *Manager) ListAllocations(poolName string) []*DiskAllocation {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var allocations []*DiskAllocation
	for _, alloc := range m.allocations {
		if alloc.PoolName == poolName {
			allocations = append(allocations, alloc)
		}
	}
	return allocations
}

// AttachDisk marks a disk as attached to a workload
func (m *Manager) AttachDisk(diskID, workloadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	alloc, exists := m.allocations[diskID]
	if !exists {
		return fmt.Errorf("disk allocation %s not found", diskID)
	}

	if alloc.AttachedTo != "" {
		return fmt.Errorf("disk %s is already attached to %s", diskID, alloc.AttachedTo)
	}

	alloc.AttachedTo = workloadID
	return nil
}

// DetachDisk marks a disk as detached from a workload
func (m *Manager) DetachDisk(diskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	alloc, exists := m.allocations[diskID]
	if !exists {
		return fmt.Errorf("disk allocation %s not found", diskID)
	}

	alloc.AttachedTo = ""
	return nil
}

// GetPoolStats returns utilization statistics for a pool
type PoolStats struct {
	Name               string
	TotalSizeGB        int64
	AllocatedSizeGB    int64
	AvailableSizeGB    int64
	UtilizationPercent float64
	AllocationCount    int
}

// GetPoolStats returns statistics for a pool
func (m *Manager) GetPoolStats(poolName string) (*PoolStats, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	pool, exists := m.pools[poolName]
	if !exists {
		return nil, fmt.Errorf("pool %s not found", poolName)
	}

	allocCount := 0
	for _, alloc := range m.allocations {
		if alloc.PoolName == poolName {
			allocCount++
		}
	}

	utilizationPercent := 0.0
	if pool.TotalSizeGB > 0 {
		utilizationPercent = float64(pool.AllocatedSizeGB) / float64(pool.TotalSizeGB) * 100
	}

	return &PoolStats{
		Name:               pool.Name,
		TotalSizeGB:        pool.TotalSizeGB,
		AllocatedSizeGB:    pool.AllocatedSizeGB,
		AvailableSizeGB:    pool.AvailableSizeGB,
		UtilizationPercent: utilizationPercent,
		AllocationCount:    allocCount,
	}, nil
}

// ResizePool resizes a storage pool
func (m *Manager) ResizePool(poolName string, newSizeGB int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	pool, exists := m.pools[poolName]
	if !exists {
		return fmt.Errorf("pool %s not found", poolName)
	}

	if newSizeGB <= pool.AllocatedSizeGB {
		return fmt.Errorf("new size (%dGB) cannot be less than allocated space (%dGB)",
			newSizeGB, pool.AllocatedSizeGB)
	}

	oldSize := pool.TotalSizeGB
	pool.TotalSizeGB = newSizeGB
	pool.AvailableSizeGB = newSizeGB - pool.AllocatedSizeGB
	pool.LastUpdated = time.Now()

	fmt.Printf("Pool %s resized from %dGB to %dGB\n", poolName, oldSize, newSizeGB)
	return nil
}
