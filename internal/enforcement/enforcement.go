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
	StopContainer(containerID string)
	IsEnforced(containerID string, lt model.LimitType) bool
	GetEnforced(containerID string) map[model.LimitType]bool
	NotifyLimitChanged(containerID string)
	StartGlobalEnforcement()
	IsGlobalEnforced(lt model.LimitType) bool
	GetGlobalEnforced() map[model.LimitType]bool
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
	globalEnforced map[model.LimitType]bool
	globalCancel   context.CancelFunc
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
		globalEnforced: make(map[model.LimitType]bool),
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
	m.mu.Unlock()

	go m.enforcementLoop(ctx, containerID, dockerID)
	log.Printf("[enforcement] started monitoring container %s", containerID)
}

// StopContainer stops enforcement for a container.
func (m *Manager) StopContainer(containerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cancel, ok := m.workers[containerID]; ok {
		cancel()
		delete(m.workers, containerID)
		delete(m.enforced, containerID)
		log.Printf("[enforcement] stopped monitoring container %s", containerID)
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
			m.mu.Unlock()

			if !wasEnforced && !slept {
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
			log.Printf("[enforcement] killed container %s: %s limit exceeded", containerID, lt)
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
// including global enforcement.
func (m *Manager) isOtherPauseActive(containerID string, except model.LimitType) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	pauseTypes := []model.LimitType{model.LimitCPU, model.LimitDisk}
	for _, lt := range pauseTypes {
		if lt != except && m.enforced[containerID][lt] {
			return true
		}
	}
	// Also check global enforcement
	for _, lt := range pauseTypes {
		if lt != except && m.globalEnforced[lt] {
			return true
		}
	}
	return false
}

// StartGlobalEnforcement spawns a goroutine that enforces global limits.
func (m *Manager) StartGlobalEnforcement() {
	m.mu.Lock()
	if m.globalCancel != nil {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.globalCancel = cancel
	m.mu.Unlock()

	go m.globalEnforcementLoop(ctx)
	log.Println("[enforcement] started global enforcement")
}

// IsGlobalEnforced returns whether a specific limit type is globally enforced.
func (m *Manager) IsGlobalEnforced(lt model.LimitType) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.globalEnforced[lt]
}

// GetGlobalEnforced returns all global enforcement states.
func (m *Manager) GetGlobalEnforced() map[model.LimitType]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[model.LimitType]bool)
	for k, v := range m.globalEnforced {
		result[k] = v
	}
	return result
}

func (m *Manager) globalEnforcementLoop(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkGlobalEnforcement(ctx)
		}
	}
}

func (m *Manager) checkGlobalEnforcement(ctx context.Context) {
	globalLimits, err := m.store.GetAllGlobalLimits()
	if err != nil {
		log.Printf("[enforcement] error getting global limits: %v", err)
		return
	}
	if len(globalLimits) == 0 {
		return
	}

	containers, err := m.store.ListContainers()
	if err != nil {
		log.Printf("[enforcement] error listing containers for global enforcement: %v", err)
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

	for lt, limit := range globalLimits {
		if limit == 0 {
			continue
		}

		usage := totalUsage[lt]
		exceeded := usage >= limit

		m.mu.Lock()
		wasEnforced := m.globalEnforced[lt]
		m.mu.Unlock()

		if exceeded && !wasEnforced {
			m.globalEnforce(ctx, lt, containers)
		} else if !exceeded && wasEnforced {
			m.globalRelease(ctx, lt, containers)
		}
	}
}

func (m *Manager) globalEnforce(ctx context.Context, lt model.LimitType, containers []model.Container) {
	log.Printf("[enforcement] global limit %s exceeded, enforcing on all containers", lt)

	for _, c := range containers {
		running, err := m.docker.IsContainerRunning(ctx, c.DockerID)
		if err != nil || !running {
			continue
		}
		cgroupPath, _ := m.cgroup.FindCgroupPath(c.DockerID)
		m.enforce(ctx, c.ID, c.DockerID, lt, cgroupPath)
	}

	m.mu.Lock()
	m.globalEnforced[lt] = true
	m.mu.Unlock()

	if m.bus != nil {
		m.bus.PublishData(events.GlobalEnforcementChange, "", events.GlobalEnforcementChangeData{
			LimitType: string(lt),
			Enforced:  true,
		})
	}
}

func (m *Manager) globalRelease(ctx context.Context, lt model.LimitType, containers []model.Container) {
	log.Printf("[enforcement] global limit %s released, releasing all containers", lt)

	// Clear global enforced first so isOtherPauseActive sees updated state
	m.mu.Lock()
	m.globalEnforced[lt] = false
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

// StopAll stops enforcement for all containers and global enforcement.
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, cancel := range m.workers {
		cancel()
		delete(m.workers, id)
	}
	if m.globalCancel != nil {
		m.globalCancel()
		m.globalCancel = nil
	}
}
