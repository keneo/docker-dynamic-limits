package model

import "time"

// LimitType identifies a resource limit category.
type LimitType string

const (
	LimitCPU        LimitType = "cpu"         // cumulative CPU time in seconds
	LimitRAM        LimitType = "ram"         // RAM in bytes
	LimitDisk       LimitType = "disk"        // disk space in bytes
	LimitNetwork    LimitType = "net"         // cumulative network bytes
	LimitDiskIOByte LimitType = "disk-io-bytes" // cumulative disk IO bytes
	LimitDiskIOOps  LimitType = "disk-io-ops"   // cumulative disk IO operations
	LimitSpending   LimitType = "spending"      // spending budget in cents (USD)
)

// AllLimitTypes lists every supported limit type.
var AllLimitTypes = []LimitType{
	LimitCPU, LimitRAM, LimitDisk, LimitNetwork,
	LimitDiskIOByte, LimitDiskIOOps, LimitSpending,
}

// Container represents a managed container.
type Container struct {
	ID          string    `json:"id"`
	DockerID    string    `json:"docker_id"`
	Name        string    `json:"name"`
	RegisteredAt time.Time `json:"registered_at"`
}

// LimitConfig holds the configured limit for one resource on one container.
type LimitConfig struct {
	ContainerID string    `json:"container_id"`
	Type        LimitType `json:"type"`
	Value       int64     `json:"value"` // meaning depends on type: seconds, bytes, cents, ops
}

// Usage holds the current usage for one resource on one container.
type Usage struct {
	ContainerID string    `json:"container_id"`
	Type        LimitType `json:"type"`
	Value       int64     `json:"value"`
}

// ContainerStatus is the full status for a managed container.
type ContainerStatus struct {
	Container Container              `json:"container"`
	Limits    map[LimitType]int64    `json:"limits"`
	Usage     map[LimitType]int64    `json:"usage"`
	Enforced  map[LimitType]bool     `json:"enforced"` // true if limit is currently being enforced (paused/blocked)
}

// EnforcementAction represents what to do when a limit is exceeded.
type EnforcementAction string

const (
	ActionNone       EnforcementAction = "none"
	ActionPause      EnforcementAction = "pause"
	ActionThrottle   EnforcementAction = "throttle"
	ActionBlock      EnforcementAction = "block"
	ActionKernelOOM  EnforcementAction = "kernel_oom"
)
