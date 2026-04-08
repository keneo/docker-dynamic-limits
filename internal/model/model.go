package model

import (
	"strings"
	"time"
)

// Scope identifies an enforcement scope: host ("") or a named segment.
type Scope string

const ScopeHost Scope = "" // Host scope = all containers on this machine

// SegmentScope returns the scope for a named segment.
func SegmentScope(id string) Scope { return Scope("segment:" + id) }

// IsSegment returns true if this scope refers to a segment.
func (s Scope) IsSegment() bool { return strings.HasPrefix(string(s), "segment:") }

// SegmentID extracts the segment ID from a segment scope. Returns "" for host scope.
func (s Scope) SegmentID() string {
	if s.IsSegment() {
		return strings.TrimPrefix(string(s), "segment:")
	}
	return ""
}

// LimitType identifies a resource limit category.
type LimitType string

const (
	LimitCPU        LimitType = "cpu"         // cumulative CPU time in seconds
	LimitRAM        LimitType = "ram"         // RAM in bytes
	LimitDisk       LimitType = "disk"        // disk space in bytes
	LimitNetwork    LimitType = "net"         // cumulative network bytes
	LimitDiskIOByte LimitType = "disk-io-bytes" // cumulative disk IO bytes
	LimitDiskIOOps  LimitType = "disk-io-ops"   // cumulative disk IO operations
	LimitSpending        LimitType = "spending"          // spending budget in milli-cents (1 cent = 1000 milli-cents)
	LimitRAMUsageBSec    LimitType = "ram-usage-bsec"    // cumulative RAM usage × time (byte-seconds)
	LimitDiskUsageBSec   LimitType = "disk-usage-bsec"   // cumulative disk usage × time (byte-seconds)
	LimitRAMRequestBSec  LimitType = "ram-request-bsec"  // cumulative RAM request × time (byte-seconds)
	LimitDiskRequestBSec LimitType = "disk-request-bsec" // cumulative disk request × time (byte-seconds)
)

// AllLimitTypes lists every supported limit type.
var AllLimitTypes = []LimitType{
	LimitCPU, LimitRAM, LimitDisk, LimitNetwork,
	LimitDiskIOByte, LimitDiskIOOps, LimitSpending,
	LimitRAMUsageBSec, LimitDiskUsageBSec,
	LimitRAMRequestBSec, LimitDiskRequestBSec,
}

// CumulativeLimitTypes are limit types that represent consumed resources.
// Their usage is accumulated into the global total when a container is removed,
// so that removing a container doesn't reduce global usage for irreversible resources.
// Only ram and disk (current-state metrics) are excluded.
var CumulativeLimitTypes = []LimitType{
	LimitCPU, LimitNetwork, LimitDiskIOByte, LimitDiskIOOps, LimitSpending,
	LimitRAMUsageBSec, LimitDiskUsageBSec,
	LimitRAMRequestBSec, LimitDiskRequestBSec,
}

// Container represents a managed container.
type Container struct {
	ID           string    `json:"id"`
	DockerID     string    `json:"docker_id"`
	Name         string    `json:"name"`
	RegisteredAt time.Time `json:"registered_at"`
	SegmentID    string    `json:"segment_id,omitempty"` // empty = no segment (host-only)
}

// Segment represents a named group of containers with shared limits.
type Segment struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
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
	Enforced  map[LimitType]bool     `json:"enforced"`    // true if limit is currently being enforced (paused/blocked)
	State     string                 `json:"state"`        // "running", "paused", "exited", "deleted"
	ProxyAddr string                 `json:"proxy_addr,omitempty"` // spending proxy address for this container
	Frozen    bool                   `json:"frozen"`       // true if container is user-frozen
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
