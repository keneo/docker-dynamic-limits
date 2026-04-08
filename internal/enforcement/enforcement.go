package enforcement

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/keneo/docker-dynamic-limits/internal/cgroup"
	"github.com/keneo/docker-dynamic-limits/internal/docker"
	"github.com/keneo/docker-dynamic-limits/internal/events"
	"github.com/keneo/docker-dynamic-limits/internal/model"
	"github.com/keneo/docker-dynamic-limits/internal/proxy"
	"github.com/keneo/docker-dynamic-limits/internal/store"
)

// EnforcementController defines the interface for enforcement operations.
type EnforcementController interface {
	StartContainer(containerID string, dockerID string)
	StopContainer(containerID string, reason string)
	IsEnforced(containerID string, lt model.LimitType) bool
	GetEnforced(containerID string) map[model.LimitType]bool
	NotifyLimitChanged(containerID string)
	StartScopeEnforcement()
	IsScopeEnforced(scope model.Scope, lt model.LimitType) bool
	GetScopeEnforced(scope model.Scope) map[model.LimitType]bool
	Freeze(containerID string) error
	Unfreeze(containerID string) (enforcementActive bool, err error)
	IsFrozen(containerID string) bool
}

// Manager manages enforcement goroutines for all registered containers.
type Manager struct {
	store    store.DataStore
	docker   docker.DockerClient
	cgroup   cgroup.CgroupReader
	proxy    proxy.SpendingProxy
	bus      *events.Bus
	interval time.Duration

	mu             sync.Mutex
	workers        map[string]context.CancelFunc // containerID -> cancel func
	enforced       map[string]map[model.LimitType]bool
	frozen         map[string]bool // containerID -> frozen state
	dockerIDs      map[string]string // containerID -> dockerID (for freeze/unfreeze)
	scopeEnforced map[model.Scope]map[model.LimitType]bool
	scopeCancel   context.CancelFunc
	lastSleepEmit  time.Time // dedup sleep events across containers
}

// NewManager creates an enforcement manager.
func NewManager(st store.DataStore, dc docker.DockerClient, cg cgroup.CgroupReader, px proxy.SpendingProxy, bus *events.Bus) *Manager {
	return &Manager{
		store:          st,
		docker:         dc,
		cgroup:         cg,
		proxy:          px,
		bus:            bus,
		interval:       time.Second,
		workers:        make(map[string]context.CancelFunc),
		enforced:       make(map[string]map[model.LimitType]bool),
		frozen:         make(map[string]bool),
		dockerIDs:      make(map[string]string),
		scopeEnforced: make(map[model.Scope]map[model.LimitType]bool),
	}
}

// StartContainer begins enforcement for a container.
func (m *Manager) StartContainer(containerID string, dockerID string) {
	m.mu.Lock()
	if _, exists := m.workers[containerID]; exists {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.workers[containerID] = cancel
	m.enforced[containerID] = make(map[model.LimitType]bool)
	m.dockerIDs[containerID] = dockerID
	// Load frozen state from store
	if isFrozen, err := m.store.IsFrozen(containerID); err == nil {
		m.frozen[containerID] = isFrozen
	}
	m.mu.Unlock()

	go m.enforcementLoop(ctx, containerID, dockerID)
	log.Printf("[enforcement] started monitoring container %s", containerID)
}

// StopContainer stops enforcement for a container.
func (m *Manager) StopContainer(containerID string, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cancel, ok := m.workers[containerID]; ok {
		cancel()
		delete(m.workers, containerID)
		delete(m.enforced, containerID)
		delete(m.frozen, containerID)
		delete(m.dockerIDs, containerID)
		log.Printf("[enforcement] stopped monitoring container %s: %s", containerID, reason)
	}
}

// IsEnforced returns whether a specific limit is currently being enforced.
func (m *Manager) IsEnforced(containerID string, lt model.LimitType) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.enforced[containerID]; ok {
		return e[lt]
	}
	return false
}

// GetEnforced returns all enforcement states for a container.
func (m *Manager) GetEnforced(containerID string) map[model.LimitType]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[model.LimitType]bool)
	if e, ok := m.enforced[containerID]; ok {
		for k, v := range e {
			result[k] = v
		}
	}
	return result
}

// NotifyLimitChanged should be called when a limit is changed so enforcement
// can immediately re-evaluate.
func (m *Manager) NotifyLimitChanged(containerID string) {
	// The enforcement loop polls every second, so limit changes
	// are picked up quickly. This method exists for future optimization
	// (e.g., using channels to wake the loop immediately).
}

func (m *Manager) enforcementLoop(ctx context.Context, containerID, dockerID string) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	lastTick := time.Now().Round(0) // strip monotonic reading for wall-clock comparison

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().Round(0) // strip monotonic reading
			elapsed := now.Sub(lastTick)
			lastTick = now
			slept := elapsed > m.interval*5
			if slept {
				log.Printf("[enforcement] sleep detected for %s: gap=%v", containerID, elapsed)
				m.emitSleepEvent(containerID, elapsed)
				// Reset the ticker — after VM suspend/resume the internal
				// monotonic-based timer can get stuck.
				ticker.Reset(m.interval)
			}
			m.checkAndEnforce(ctx, containerID, dockerID, slept)
		}
	}
}

func (m *Manager) checkAndEnforce(ctx context.Context, containerID, dockerID string, slept bool) {
	// Fast path: frozen containers are Docker-paused with byte-sec
	// accumulators suspended — no usage changes, so skip all expensive
	// Docker API calls, cgroup reads, and SQLite writes.
	m.mu.Lock()
	isFrozen := m.frozen[containerID]
	m.mu.Unlock()
	if isFrozen {
		return
	}

	// Check if container is still running
	running, err := m.docker.IsContainerRunning(ctx, dockerID)
	if err != nil || !running {
		return
	}

	limits, err := m.store.GetAllLimits(containerID)
	if err != nil {
		log.Printf("[enforcement] error getting limits for %s: %v", containerID, err)
		return
	}

	// Find cgroup path
	cgroupPath, cgroupErr := m.cgroup.FindCgroupPath(dockerID)

	// Check each limit type
	for _, lt := range model.AllLimitTypes {
		limit, hasLimit := limits[lt]
		if !hasLimit || limit == 0 {
			continue
		}

		var usage int64
		if isByteSecondType(lt) {
			// Don't accumulate if already enforced (container killed)
			m.mu.Lock()
			wasEnforced := m.enforced[containerID][lt]
			isFrozen := m.frozen[containerID]
			m.mu.Unlock()

			if !wasEnforced && !slept && !isFrozen {
				source := m.getByteSecondSource(ctx, containerID, dockerID, lt, limits, cgroupPath, cgroupErr)
				m.store.AddUsage(containerID, lt, source)
			}
			usage, _ = m.store.GetUsage(containerID, lt)
		} else {
			var err error
			usage, err = m.getCurrentUsage(ctx, containerID, dockerID, lt, cgroupPath, cgroupErr)
			if err != nil {
				continue
			}
			// Persist usage to store
			m.store.SetUsage(containerID, lt, usage)
		}

		exceeded := usage >= limit
		m.mu.Lock()
		wasEnforced := m.enforced[containerID][lt]
		m.mu.Unlock()

		if exceeded && !wasEnforced {
			m.enforce(ctx, containerID, dockerID, lt, cgroupPath)
		} else if !exceeded && wasEnforced {
			m.release(ctx, containerID, dockerID, lt, cgroupPath)
		} else if !exceeded && !wasEnforced {
			// Reconcile: after daemon restart, in-memory state is lost but the
			// container may still be in an enforced state from the previous run.
			if m.isActuallyEnforced(ctx, dockerID, lt) {
				m.release(ctx, containerID, dockerID, lt, cgroupPath)
			}
		}
	}

	// Always sync spending from proxy to store, even when no per-container
	// spending limit is set. This ensures global spending totals are correct
	// and spending survives daemon restarts.
	if m.proxy != nil {
		spending := m.proxy.GetSpending(containerID)
		if spending > 0 {
			m.store.SetUsage(containerID, model.LimitSpending, spending)
		}
	}

	// Emit usage_update event
	if m.bus != nil {
		allUsage, _ := m.store.GetAllUsage(containerID)
		enforcedMap := m.GetEnforced(containerID)
		uMap := make(map[string]int64, len(allUsage))
		for k, v := range allUsage {
			uMap[string(k)] = v
		}
		lMap := make(map[string]int64, len(limits))
		for k, v := range limits {
			lMap[string(k)] = v
		}
		eMap := make(map[string]bool, len(enforcedMap))
		for k, v := range enforcedMap {
			eMap[string(k)] = v
		}
		m.bus.PublishData(events.UsageUpdate, containerID, events.UsageUpdateData{
			Usage:    uMap,
			Limits:   lMap,
			Enforced: eMap,
		})
	}
}

func (m *Manager) getCurrentUsage(ctx context.Context, containerID, dockerID string, lt model.LimitType, cgroupPath string, cgroupErr error) (int64, error) {
	switch lt {
	case model.LimitCPU:
		if cgroupErr != nil {
			return 0, cgroupErr
		}
		usec, err := m.cgroup.CPUUsageMicroseconds(cgroupPath)
		if err != nil {
			return 0, err
		}
		return usec / 1_000_000, nil // convert to seconds

	case model.LimitRAM:
		if cgroupErr != nil {
			return 0, cgroupErr
		}
		return m.cgroup.MemoryCurrent(cgroupPath)

	case model.LimitDisk:
		return m.docker.GetContainerDiskUsage(ctx, dockerID)

	case model.LimitNetwork:
		if cgroupErr != nil {
			return 0, cgroupErr
		}
		// Try to get network stats from stored veth name or default
		// For simplicity, read from stored usage + incremental
		usage, _ := m.store.GetUsage(containerID, lt)
		return usage, nil

	case model.LimitDiskIOByte:
		if cgroupErr != nil {
			return 0, cgroupErr
		}
		stats, err := m.cgroup.ReadIOStat(cgroupPath)
		if err != nil {
			return 0, err
		}
		return stats.TotalBytes, nil

	case model.LimitDiskIOOps:
		if cgroupErr != nil {
			return 0, cgroupErr
		}
		stats, err := m.cgroup.ReadIOStat(cgroupPath)
		if err != nil {
			return 0, err
		}
		return stats.TotalOps, nil

	case model.LimitSpending:
		if m.proxy != nil {
			return m.proxy.GetSpending(containerID), nil
		}
		return 0, nil

	default:
		return 0, fmt.Errorf("unknown limit type: %s", lt)
	}
}

// isByteSecondType returns true for cumulative byte-second limit types.
func isByteSecondType(lt model.LimitType) bool {
	switch lt {
	case model.LimitRAMUsageBSec, model.LimitDiskUsageBSec,
		model.LimitRAMRequestBSec, model.LimitDiskRequestBSec:
		return true
	}
	return false
}

// getByteSecondSource returns the current source value to accumulate for a byte-second type.
func (m *Manager) getByteSecondSource(ctx context.Context, containerID, dockerID string, lt model.LimitType, limits map[model.LimitType]int64, cgroupPath string, cgroupErr error) int64 {
	switch lt {
	case model.LimitRAMUsageBSec:
		if cgroupErr != nil {
			return 0
		}
		v, err := m.cgroup.MemoryCurrent(cgroupPath)
		if err != nil {
			return 0
		}
		return v
	case model.LimitDiskUsageBSec:
		v, err := m.docker.GetContainerDiskUsage(ctx, dockerID)
		if err != nil {
			return 0
		}
		return v
	case model.LimitRAMRequestBSec:
		v, _ := m.store.GetLimit(containerID, model.LimitRAM)
		return v
	case model.LimitDiskRequestBSec:
		v, _ := m.store.GetLimit(containerID, model.LimitDisk)
		return v
	}
	return 0
}

func (m *Manager) enforce(ctx context.Context, containerID, dockerID string, lt model.LimitType, cgroupPath string) {
	var err error
	switch lt {
	case model.LimitCPU:
		err = m.docker.PauseContainer(ctx, dockerID)
		if err == nil {
			log.Printf("[enforcement] paused container %s: CPU limit exceeded", containerID)
		} else if strings.Contains(err.Error(), "already paused") {
			err = nil // already in desired state
		}

	case model.LimitRAM:
		// RAM is enforced by the kernel via memory.max — we just update it
		limit, _ := m.store.GetLimit(containerID, lt)
		err = m.docker.UpdateMemoryLimit(ctx, dockerID, limit)
		if err == nil {
			log.Printf("[enforcement] set memory limit for %s to %d bytes", containerID, limit)
		}

	case model.LimitDisk:
		err = m.docker.PauseContainer(ctx, dockerID)
		if err == nil {
			log.Printf("[enforcement] paused container %s: disk limit exceeded", containerID)
		} else if strings.Contains(err.Error(), "already paused") {
			err = nil // already in desired state
		}

	case model.LimitNetwork:
		err = m.docker.DisconnectNetwork(ctx, dockerID)
		if err == nil {
			log.Printf("[enforcement] disconnected network for %s: network limit exceeded", containerID)
		}

	case model.LimitDiskIOByte, model.LimitDiskIOOps:
		// Throttle IO to near-zero
		if cgroupPath != "" {
			err = m.cgroup.SetIOMax(cgroupPath, "8:0", 1, 1)
			if err == nil {
				log.Printf("[enforcement] throttled IO for %s: disk IO limit exceeded", containerID)
			}
		}

	case model.LimitSpending:
		// Spending enforcement is handled by the proxy itself
		log.Printf("[enforcement] spending limit exceeded for %s", containerID)

	case model.LimitRAMUsageBSec, model.LimitDiskUsageBSec,
		model.LimitRAMRequestBSec, model.LimitDiskRequestBSec:
		err = m.docker.StopContainer(ctx, dockerID)
		if err == nil {
			usage, _ := m.store.GetUsage(containerID, lt)
			limit, _ := m.store.GetLimit(containerID, lt)
			log.Printf("[enforcement] killed container %s: %s limit exceeded (usage=%d, limit=%d)", containerID, lt, usage, limit)
			if m.bus != nil {
				m.bus.PublishData(events.ContainerKilled, containerID, events.ContainerKilledData{
					LimitType:   string(lt),
					UsageAtKill: usage,
					LimitAtKill: limit,
				})
			}
		}
	}

	if err != nil {
		log.Printf("[enforcement] error enforcing %s for %s: %v", lt, containerID, err)
		return
	}

	m.mu.Lock()
	if m.enforced[containerID] == nil {
		m.enforced[containerID] = make(map[model.LimitType]bool)
	}
	m.enforced[containerID][lt] = true
	m.mu.Unlock()

	if m.bus != nil {
		m.bus.PublishData(events.EnforcementChange, containerID, events.EnforcementChangeData{
			LimitType: string(lt),
			Enforced:  true,
		})
	}
}

func (m *Manager) release(ctx context.Context, containerID, dockerID string, lt model.LimitType, cgroupPath string) {
	var err error
	switch lt {
	case model.LimitCPU:
		paused, _ := m.docker.IsContainerPaused(ctx, dockerID)
		if paused {
			// Only unpause if no other limit type is also enforcing pause
			if !m.isOtherPauseActive(containerID, lt) {
				err = m.docker.UnpauseContainer(ctx, dockerID)
				if err == nil {
					log.Printf("[enforcement] resumed container %s: CPU limit increased", containerID)
				}
			}
		}

	case model.LimitRAM:
		limit, _ := m.store.GetLimit(containerID, lt)
		err = m.docker.UpdateMemoryLimit(ctx, dockerID, limit)
		if err == nil {
			log.Printf("[enforcement] updated memory limit for %s to %d bytes", containerID, limit)
		}

	case model.LimitDisk:
		paused, _ := m.docker.IsContainerPaused(ctx, dockerID)
		if paused && !m.isOtherPauseActive(containerID, lt) {
			err = m.docker.UnpauseContainer(ctx, dockerID)
			if err == nil {
				log.Printf("[enforcement] resumed container %s: disk limit increased", containerID)
			}
		}

	case model.LimitNetwork:
		err = m.docker.ReconnectNetwork(ctx, dockerID)
		if err == nil {
			log.Printf("[enforcement] reconnected network for %s: network limit increased", containerID)
		}

	case model.LimitDiskIOByte, model.LimitDiskIOOps:
		if cgroupPath != "" {
			err = m.cgroup.RemoveIOMax(cgroupPath, "8:0")
			if err == nil {
				log.Printf("[enforcement] unthrottled IO for %s: IO limit increased", containerID)
			}
		}

	case model.LimitSpending:
		log.Printf("[enforcement] spending limit released for %s", containerID)

	case model.LimitRAMUsageBSec, model.LimitDiskUsageBSec,
		model.LimitRAMRequestBSec, model.LimitDiskRequestBSec:
		// Container was killed; if user increased the limit and restarted
		// the container, just clear the enforced flag (no Docker action needed).
		log.Printf("[enforcement] %s limit released for %s (container may be restarted)", lt, containerID)
	}

	if err != nil {
		log.Printf("[enforcement] error releasing %s for %s: %v", lt, containerID, err)
		return
	}

	m.mu.Lock()
	if m.enforced[containerID] != nil {
		m.enforced[containerID][lt] = false
	}
	m.mu.Unlock()

	if m.bus != nil {
		m.bus.PublishData(events.EnforcementChange, containerID, events.EnforcementChangeData{
			LimitType: string(lt),
			Enforced:  false,
		})
	}
}

// emitSleepEvent publishes a system_sleep event, deduplicating across containers.
func (m *Manager) emitSleepEvent(containerID string, elapsed time.Duration) {
	if m.bus == nil {
		return
	}
	m.mu.Lock()
	if time.Since(m.lastSleepEmit) < m.interval*5 {
		m.mu.Unlock()
		return
	}
	m.lastSleepEmit = time.Now()
	m.mu.Unlock()

	m.bus.PublishData(events.SystemSleep, containerID, events.SystemSleepData{
		DurationSeconds: elapsed.Seconds(),
	})
}

// isActuallyEnforced checks the real container state to detect enforcement
// from a previous daemon instance (whose in-memory state was lost on restart).
func (m *Manager) isActuallyEnforced(ctx context.Context, dockerID string, lt model.LimitType) bool {
	switch lt {
	case model.LimitCPU, model.LimitDisk:
		paused, err := m.docker.IsContainerPaused(ctx, dockerID)
		return err == nil && paused
	default:
		return false
	}
}

// isOtherPauseActive checks if another limit type that uses pause is also enforced,
// including scope enforcement (host + container's segment) and user freeze.
func (m *Manager) isOtherPauseActive(containerID string, except model.LimitType) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Frozen containers must stay Docker-paused
	if m.frozen[containerID] {
		return true
	}
	pauseTypes := []model.LimitType{model.LimitCPU, model.LimitDisk}
	for _, lt := range pauseTypes {
		if lt != except && m.enforced[containerID][lt] {
			return true
		}
	}
	// Check host scope enforcement
	if hostEnf := m.scopeEnforced[model.ScopeHost]; hostEnf != nil {
		for _, lt := range pauseTypes {
			if lt != except && hostEnf[lt] {
				return true
			}
		}
	}
	// Check container's segment scope enforcement
	if c, err := m.store.GetContainer(containerID); err == nil && c.SegmentID != "" {
		segScope := model.SegmentScope(c.SegmentID)
		if segEnf := m.scopeEnforced[segScope]; segEnf != nil {
			for _, lt := range pauseTypes {
				if lt != except && segEnf[lt] {
					return true
				}
			}
		}
	}
	return false
}

// StartScopeEnforcement spawns a goroutine that enforces scope-level limits
// (host scope and all segment scopes).
func (m *Manager) StartScopeEnforcement() {
	m.mu.Lock()
	if m.scopeCancel != nil {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.scopeCancel = cancel
	m.mu.Unlock()

	go m.scopeEnforcementLoop(ctx)
	log.Println("[enforcement] started scope enforcement")
}

// IsScopeEnforced returns whether a specific limit type is enforced for a scope.
func (m *Manager) IsScopeEnforced(scope model.Scope, lt model.LimitType) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.scopeEnforced[scope]; e != nil {
		return e[lt]
	}
	return false
}

// GetScopeEnforced returns all enforcement states for a scope.
func (m *Manager) GetScopeEnforced(scope model.Scope) map[model.LimitType]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[model.LimitType]bool)
	if e := m.scopeEnforced[scope]; e != nil {
		for k, v := range e {
			result[k] = v
		}
	}
	return result
}

func (m *Manager) scopeEnforcementLoop(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Check host scope
			m.checkScopeEnforcement(ctx, model.ScopeHost)
			// Check each segment scope
			segments, err := m.store.ListSegments()
			if err == nil {
				for _, seg := range segments {
					m.checkScopeEnforcement(ctx, model.SegmentScope(seg.ID))
				}
			}
		}
	}
}

func (m *Manager) checkScopeEnforcement(ctx context.Context, scope model.Scope) {
	scopeLimits, err := m.store.GetAllScopeLimits(scope)
	if err != nil {
		log.Printf("[enforcement] error getting scope %q limits: %v", scope, err)
		return
	}
	if len(scopeLimits) == 0 {
		return
	}

	containers, err := m.store.ListContainersByScope(scope)
	if err != nil {
		log.Printf("[enforcement] error listing containers for scope %q enforcement: %v", scope, err)
		return
	}

	// Sum usage across all containers for each limit type
	totalUsage := make(map[model.LimitType]int64)
	for _, c := range containers {
		allUsage, err := m.store.GetAllUsage(c.ID)
		if err != nil {
			continue
		}
		for lt, v := range allUsage {
			totalUsage[lt] += v
		}
	}
	// Add accumulated usage from removed containers
	if accum, err := m.store.GetScopeUsageAccum(scope); err == nil {
		for lt, v := range accum {
			totalUsage[lt] += v
		}
	}

	for lt, limit := range scopeLimits {
		if limit == 0 {
			continue
		}

		usage := totalUsage[lt]
		exceeded := usage >= limit

		m.mu.Lock()
		if m.scopeEnforced[scope] == nil {
			m.scopeEnforced[scope] = make(map[model.LimitType]bool)
		}
		wasEnforced := m.scopeEnforced[scope][lt]
		m.mu.Unlock()

		if exceeded && !wasEnforced {
			m.scopeEnforce(ctx, scope, lt, containers)
		} else if !exceeded && wasEnforced {
			m.scopeRelease(ctx, scope, lt, containers)
		}
	}
}

func (m *Manager) scopeEnforce(ctx context.Context, scope model.Scope, lt model.LimitType, containers []model.Container) {
	log.Printf("[enforcement] scope %q limit %s exceeded, enforcing on all containers", scope, lt)

	for _, c := range containers {
		running, err := m.docker.IsContainerRunning(ctx, c.DockerID)
		if err != nil || !running {
			continue
		}
		cgroupPath, _ := m.cgroup.FindCgroupPath(c.DockerID)
		m.enforce(ctx, c.ID, c.DockerID, lt, cgroupPath)
	}

	// Block proxy API calls when spending limit is exceeded for this scope
	if lt == model.LimitSpending && m.proxy != nil {
		m.proxy.SetScopeSpendingBlocked(scope, true)
	}

	m.mu.Lock()
	if m.scopeEnforced[scope] == nil {
		m.scopeEnforced[scope] = make(map[model.LimitType]bool)
	}
	m.scopeEnforced[scope][lt] = true
	m.mu.Unlock()

	if m.bus != nil {
		m.bus.PublishData(events.GlobalEnforcementChange, "", events.GlobalEnforcementChangeData{
			LimitType: string(lt),
			Enforced:  true,
		})
	}
}

func (m *Manager) scopeRelease(ctx context.Context, scope model.Scope, lt model.LimitType, containers []model.Container) {
	log.Printf("[enforcement] scope %q limit %s released, releasing all containers", scope, lt)

	// Unblock proxy API calls when spending limit is released for this scope
	if lt == model.LimitSpending && m.proxy != nil {
		m.proxy.SetScopeSpendingBlocked(scope, false)
	}

	// Clear scope enforced first so isOtherPauseActive sees updated state
	m.mu.Lock()
	if m.scopeEnforced[scope] != nil {
		m.scopeEnforced[scope][lt] = false
	}
	m.mu.Unlock()

	for _, c := range containers {
		running, err := m.docker.IsContainerRunning(ctx, c.DockerID)
		if err != nil || !running {
			continue
		}
		cgroupPath, _ := m.cgroup.FindCgroupPath(c.DockerID)
		m.release(ctx, c.ID, c.DockerID, lt, cgroupPath)
	}

	if m.bus != nil {
		m.bus.PublishData(events.GlobalEnforcementChange, "", events.GlobalEnforcementChangeData{
			LimitType: string(lt),
			Enforced:  false,
		})
	}
}

// StartAll begins enforcement for all registered containers.
func (m *Manager) StartAll() error {
	containers, err := m.store.ListContainers()
	if err != nil {
		return err
	}
	for _, c := range containers {
		m.StartContainer(c.ID, c.DockerID)
	}
	return nil
}

// StopAll stops enforcement for all containers and scope enforcement.
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, cancel := range m.workers {
		cancel()
		delete(m.workers, id)
	}
	if m.scopeCancel != nil {
		m.scopeCancel()
		m.scopeCancel = nil
	}
}

// Freeze freezes a container: Docker-pauses it (if not already paused) and
// suspends all byte-second accumulators. Persists to store.
func (m *Manager) Freeze(containerID string) error {
	m.mu.Lock()
	dockerID := m.dockerIDs[containerID]
	alreadyFrozen := m.frozen[containerID]
	m.mu.Unlock()

	if dockerID == "" {
		return fmt.Errorf("container %s not tracked by enforcement", containerID)
	}
	if alreadyFrozen {
		return nil
	}

	// Docker pause (ignore "already paused" errors)
	ctx := context.Background()
	if err := m.docker.PauseContainer(ctx, dockerID); err != nil {
		if !strings.Contains(err.Error(), "already paused") {
			return fmt.Errorf("docker pause: %w", err)
		}
	}

	// Persist and set in-memory state
	if err := m.store.SetFrozen(containerID, true); err != nil {
		return fmt.Errorf("persist frozen: %w", err)
	}
	m.mu.Lock()
	m.frozen[containerID] = true
	m.mu.Unlock()

	log.Printf("[enforcement] frozen container %s", containerID)
	if m.bus != nil {
		m.bus.PublishData(events.ContainerFrozen, containerID, events.ContainerFrozenData{})
	}
	return nil
}

// Unfreeze unfreezes a container: resumes byte-second accumulators and
// Docker-unpauses the container unless enforcement is still holding it paused.
// Returns true if enforcement is still active (container stays Docker-paused).
func (m *Manager) Unfreeze(containerID string) (enforcementActive bool, err error) {
	m.mu.Lock()
	dockerID := m.dockerIDs[containerID]
	wasFrozen := m.frozen[containerID]
	m.mu.Unlock()

	if dockerID == "" {
		return false, fmt.Errorf("container %s not tracked by enforcement", containerID)
	}
	if !wasFrozen {
		return false, nil
	}

	// Clear frozen state first (so isOtherPauseActive won't see it)
	if err := m.store.SetFrozen(containerID, false); err != nil {
		return false, fmt.Errorf("persist unfrozen: %w", err)
	}
	m.mu.Lock()
	m.frozen[containerID] = false

	// Check if any enforcement-pause is still holding the container
	anyEnforced := false
	pauseTypes := []model.LimitType{model.LimitCPU, model.LimitDisk}
	for _, lt := range pauseTypes {
		if m.enforced[containerID][lt] {
			anyEnforced = true
			break
		}
	}
	// Also check scope enforcement pause (host + container's segment)
	if !anyEnforced {
		if hostEnf := m.scopeEnforced[model.ScopeHost]; hostEnf != nil {
			for _, lt := range pauseTypes {
				if hostEnf[lt] {
					anyEnforced = true
					break
				}
			}
		}
	}
	if !anyEnforced {
		if c, err := m.store.GetContainer(containerID); err == nil && c.SegmentID != "" {
			segScope := model.SegmentScope(c.SegmentID)
			if segEnf := m.scopeEnforced[segScope]; segEnf != nil {
				for _, lt := range pauseTypes {
					if segEnf[lt] {
						anyEnforced = true
						break
					}
				}
			}
		}
	}
	m.mu.Unlock()

	if !anyEnforced {
		ctx := context.Background()
		paused, _ := m.docker.IsContainerPaused(ctx, dockerID)
		if paused {
			if err := m.docker.UnpauseContainer(ctx, dockerID); err != nil {
				return false, fmt.Errorf("docker unpause: %w", err)
			}
		}
	}

	log.Printf("[enforcement] unfrozen container %s (enforcement_active=%v)", containerID, anyEnforced)
	if m.bus != nil {
		m.bus.PublishData(events.ContainerUnfrozen, containerID, events.ContainerUnfrozenData{
			EnforcementActive: anyEnforced,
		})
	}
	return anyEnforced, nil
}

// IsFrozen returns whether a container is currently frozen.
func (m *Manager) IsFrozen(containerID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.frozen[containerID]
}
