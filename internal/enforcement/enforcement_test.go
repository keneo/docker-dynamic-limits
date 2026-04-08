package enforcement

import (
	"testing"
	"time"

	"github.com/keneo/docker-dynamic-limits/internal/events"
	"github.com/keneo/docker-dynamic-limits/internal/model"
	"github.com/keneo/docker-dynamic-limits/internal/testutil"
)

func newTestManager() (*Manager, *testutil.MockStore, *testutil.MockDocker, *testutil.MockCgroup, *testutil.MockProxy, *events.Bus) {
	ms := testutil.NewMockStore()
	md := testutil.NewMockDocker()
	mc := testutil.NewMockCgroup()
	mp := testutil.NewMockProxy()
	bus := events.NewBus()
	m := NewManager(ms, md, mc, mp, bus)
	m.interval = 50 * time.Millisecond // faster for tests
	return m, ms, md, mc, mp, bus
}

func TestStartStopContainer(t *testing.T) {
	m, _, _, _, _, _ := newTestManager()

	m.StartContainer("c1", "dockerid1")

	m.mu.Lock()
	_, exists := m.workers["c1"]
	m.mu.Unlock()
	if !exists {
		t.Error("worker not found after StartContainer")
	}

	m.StopContainer("c1", "test")

	m.mu.Lock()
	_, exists = m.workers["c1"]
	m.mu.Unlock()
	if exists {
		t.Error("worker still exists after StopContainer")
	}
}

func TestStartContainerIdempotent(t *testing.T) {
	m, _, _, _, _, _ := newTestManager()

	m.StartContainer("c1", "dockerid1")
	m.StartContainer("c1", "dockerid1") // should not panic or create duplicate

	m.mu.Lock()
	count := len(m.workers)
	m.mu.Unlock()
	if count != 1 {
		t.Errorf("expected 1 worker, got %d", count)
	}

	m.StopContainer("c1", "test")
}

func TestStopContainerNonexistent(t *testing.T) {
	m, _, _, _, _, _ := newTestManager()
	// Should not panic
	m.StopContainer("nonexistent", "test")
}

func TestIsEnforcedDefault(t *testing.T) {
	m, _, _, _, _, _ := newTestManager()
	if m.IsEnforced("c1", model.LimitCPU) {
		t.Error("should not be enforced by default")
	}
}

func TestGetEnforcedEmpty(t *testing.T) {
	m, _, _, _, _, _ := newTestManager()
	enforced := m.GetEnforced("c1")
	if len(enforced) != 0 {
		t.Errorf("expected empty map, got %v", enforced)
	}
}

func TestNotifyLimitChanged(t *testing.T) {
	m, _, _, _, _, _ := newTestManager()
	// Should not panic
	m.NotifyLimitChanged("c1")
}

func TestEnforcementCPUPause(t *testing.T) {
	m, ms, md, mc, _, _ := newTestManager()

	dockerID := "abcdef123456789000"
	containerID := dockerID[:12]

	// Set up state
	md.AddContainer(dockerID, "test", true)
	ms.RegisterContainer(dockerID, "test")
	ms.SetLimit(containerID, model.LimitCPU, 100) // 100 seconds

	cgPath := "/cgroup/test"
	mc.CgroupPaths[dockerID] = cgPath
	mc.CPUUsage[cgPath] = 200_000_000 // 200 seconds in microseconds (exceeds limit)

	m.StartContainer(containerID, dockerID)
	time.Sleep(200 * time.Millisecond)
	m.StopContainer(containerID, "test")

	// Verify container was paused (goroutine stopped, safe to read)
	paused, _ := md.IsContainerPaused(nil, dockerID)
	if !paused {
		t.Error("container should be paused when CPU limit exceeded")
	}
}

func TestEnforcementNoLimitNoAction(t *testing.T) {
	m, ms, md, mc, _, _ := newTestManager()

	dockerID := "abcdef123456789000"
	containerID := dockerID[:12]

	md.AddContainer(dockerID, "test", true)
	ms.RegisterContainer(dockerID, "test")
	// No limits set

	cgPath := "/cgroup/test"
	mc.CgroupPaths[dockerID] = cgPath
	mc.CPUUsage[cgPath] = 999_000_000

	m.StartContainer(containerID, dockerID)
	time.Sleep(200 * time.Millisecond)
	m.StopContainer(containerID, "test")

	paused, _ := md.IsContainerPaused(nil, dockerID)
	if paused {
		t.Error("container should not be paused when no limits set")
	}
}

func TestEnforcementUsageBelowLimit(t *testing.T) {
	m, ms, md, mc, _, _ := newTestManager()

	dockerID := "abcdef123456789000"
	containerID := dockerID[:12]

	md.AddContainer(dockerID, "test", true)
	ms.RegisterContainer(dockerID, "test")
	ms.SetLimit(containerID, model.LimitCPU, 100)

	cgPath := "/cgroup/test"
	mc.CgroupPaths[dockerID] = cgPath
	mc.CPUUsage[cgPath] = 50_000_000 // 50 seconds (below 100)

	m.StartContainer(containerID, dockerID)
	time.Sleep(200 * time.Millisecond)
	m.StopContainer(containerID, "test")

	paused, _ := md.IsContainerPaused(nil, dockerID)
	if paused {
		t.Error("container should not be paused when under CPU limit")
	}
}

func TestStartAllStopAll(t *testing.T) {
	m, ms, md, _, _, _ := newTestManager()

	md.AddContainer("aaaaaaaaaaaa000000", "c1", true)
	md.AddContainer("bbbbbbbbbbbb000000", "c2", true)
	ms.RegisterContainer("aaaaaaaaaaaa000000", "c1")
	ms.RegisterContainer("bbbbbbbbbbbb000000", "c2")

	err := m.StartAll()
	if err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	m.mu.Lock()
	count := len(m.workers)
	m.mu.Unlock()
	if count != 2 {
		t.Errorf("expected 2 workers, got %d", count)
	}

	m.StopAll()

	m.mu.Lock()
	count = len(m.workers)
	m.mu.Unlock()
	if count != 0 {
		t.Errorf("expected 0 workers after StopAll, got %d", count)
	}
}

func TestEnforcementAlreadyPaused(t *testing.T) {
	// Bug fix: when a container is already paused (e.g. from a previous daemon),
	// enforce() should treat "already paused" as success and set the enforced flag.
	// This ensures that if the limit is later increased, release() will fire.
	m, ms, md, mc, _, _ := newTestManager()

	dockerID := "abcdef123456789000"
	containerID := dockerID[:12]

	md.AddContainer(dockerID, "test", true)
	ms.RegisterContainer(dockerID, "test")
	ms.SetLimit(containerID, model.LimitCPU, 100) // 100 seconds

	cgPath := "/cgroup/test"
	mc.CgroupPaths[dockerID] = cgPath
	mc.CPUUsage[cgPath] = 200_000_000 // 200 seconds (exceeds limit)

	// Pre-pause the container (simulates previous daemon enforcement)
	md.PauseContainer(nil, dockerID)

	m.StartContainer(containerID, dockerID)
	time.Sleep(200 * time.Millisecond)

	// The enforced flag should be set despite "already paused" error
	// (check before StopContainer which clears the map)
	if !m.IsEnforced(containerID, model.LimitCPU) {
		t.Error("CPU should be marked as enforced even when container was already paused")
	}

	// Now increase the limit so usage < limit → should release
	ms.SetLimit(containerID, model.LimitCPU, 7200) // 2 hours
	time.Sleep(200 * time.Millisecond)
	m.StopContainer(containerID, "test")

	paused, _ := md.IsContainerPaused(nil, dockerID)
	if paused {
		t.Error("container should be unpaused after limit was increased above usage")
	}
}

func TestEnforcementReconcileAfterRestart(t *testing.T) {
	// Bug fix: after daemon restart, in-memory enforced state is lost.
	// If the container is paused but usage is now below the limit (e.g. limit
	// was increased), the enforcement loop should detect the paused state and
	// release the container.
	m, ms, md, mc, _, _ := newTestManager()

	dockerID := "abcdef123456789000"
	containerID := dockerID[:12]

	md.AddContainer(dockerID, "test", true)
	ms.RegisterContainer(dockerID, "test")
	ms.SetLimit(containerID, model.LimitCPU, 7200) // 2 hours

	cgPath := "/cgroup/test"
	mc.CgroupPaths[dockerID] = cgPath
	mc.CPUUsage[cgPath] = 1800_000_000 // 30 minutes (below 2h limit)

	// Pre-pause the container (simulates previous daemon enforcement)
	md.PauseContainer(nil, dockerID)

	// enforced map is empty (simulates daemon restart)
	m.StartContainer(containerID, dockerID)
	time.Sleep(200 * time.Millisecond)
	m.StopContainer(containerID, "test")

	// Container should be unpaused since usage < limit
	paused, _ := md.IsContainerPaused(nil, dockerID)
	if paused {
		t.Error("container should be unpaused after reconciliation (usage below limit)")
	}
}

func TestIsOtherPauseActive(t *testing.T) {
	m, _, _, _, _, _ := newTestManager()

	m.enforced["c1"] = map[model.LimitType]bool{
		model.LimitCPU:  true,
		model.LimitDisk: false,
	}

	// CPU is enforced, checking from Disk's perspective
	if !m.isOtherPauseActive("c1", model.LimitDisk) {
		t.Error("should detect CPU pause as active when checking from Disk")
	}

	// CPU is enforced, checking from CPU's perspective (should not count itself)
	if m.isOtherPauseActive("c1", model.LimitCPU) {
		t.Error("should not count self as other pause")
	}
}

func TestEnforcementEmitsUsageUpdate(t *testing.T) {
	m, ms, md, mc, _, bus := newTestManager()

	dockerID := "abcdef123456789000"
	containerID := dockerID[:12]

	md.AddContainer(dockerID, "test", true)
	ms.RegisterContainer(dockerID, "test")
	ms.SetLimit(containerID, model.LimitCPU, 7200)

	cgPath := "/cgroup/test"
	mc.CgroupPaths[dockerID] = cgPath
	mc.CPUUsage[cgPath] = 50_000_000

	sub := bus.Subscribe(events.Filter{Types: []events.EventType{events.UsageUpdate}})
	defer bus.Unsubscribe(sub)

	m.StartContainer(containerID, dockerID)
	defer m.StopContainer(containerID, "test")

	select {
	case e := <-sub.C:
		if e.Type != events.UsageUpdate {
			t.Errorf("type = %s, want usage_update", e.Type)
		}
		if e.ContainerID != containerID {
			t.Errorf("container = %s, want %s", e.ContainerID, containerID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for usage_update event")
	}
}

func TestByteSecondSkippedDuringSleep(t *testing.T) {
	m, ms, md, mc, _, _ := newTestManager()

	dockerID := "abcdef123456789000"
	containerID := dockerID[:12]

	md.AddContainer(dockerID, "test", true)
	ms.RegisterContainer(dockerID, "test")
	ms.SetLimit(containerID, model.LimitRAMUsageBSec, 999_999_999) // high limit so no enforcement

	cgPath := "/cgroup/test"
	mc.CgroupPaths[dockerID] = cgPath
	mc.MemCurrent[cgPath] = 1_000_000 // 1 MB

	// Simulate a normal tick (slept=false): should accumulate
	m.enforced[containerID] = make(map[model.LimitType]bool)
	m.checkAndEnforce(nil, containerID, dockerID, false)

	usage1, _ := ms.GetUsage(containerID, model.LimitRAMUsageBSec)
	if usage1 != 1_000_000 {
		t.Fatalf("after normal tick: usage = %d, want 1000000", usage1)
	}

	// Simulate a sleep tick (slept=true): should NOT accumulate
	m.checkAndEnforce(nil, containerID, dockerID, true)

	usage2, _ := ms.GetUsage(containerID, model.LimitRAMUsageBSec)
	if usage2 != usage1 {
		t.Errorf("after sleep tick: usage = %d, want %d (unchanged)", usage2, usage1)
	}

	// Another normal tick: should accumulate again
	m.checkAndEnforce(nil, containerID, dockerID, false)

	usage3, _ := ms.GetUsage(containerID, model.LimitRAMUsageBSec)
	if usage3 != 2_000_000 {
		t.Errorf("after second normal tick: usage = %d, want 2000000", usage3)
	}
}

func TestGlobalEnforcementPausesAllContainers(t *testing.T) {
	m, ms, md, mc, _, _ := newTestManager()

	dockerID1 := "aaaaaaaaaaaa000000"
	dockerID2 := "bbbbbbbbbbbb000000"
	containerID1 := dockerID1[:12]
	containerID2 := dockerID2[:12]

	md.AddContainer(dockerID1, "c1", true)
	md.AddContainer(dockerID2, "c2", true)
	ms.RegisterContainer(dockerID1, "c1")
	ms.RegisterContainer(dockerID2, "c2")

	cgPath1 := "/cgroup/c1"
	cgPath2 := "/cgroup/c2"
	mc.CgroupPaths[dockerID1] = cgPath1
	mc.CgroupPaths[dockerID2] = cgPath2

	// Store usage directly (global enforcement reads from store)
	ms.SetUsage(containerID1, model.LimitCPU, 60)
	ms.SetUsage(containerID2, model.LimitCPU, 50)

	// Set global CPU limit at 100s (total usage 110s exceeds it)
	ms.SetScopeLimit(model.ScopeHost, model.LimitCPU, 100)

	m.checkScopeEnforcement(nil, model.ScopeHost)

	// Both containers should be paused
	paused1, _ := md.IsContainerPaused(nil, dockerID1)
	paused2, _ := md.IsContainerPaused(nil, dockerID2)
	if !paused1 {
		t.Error("container c1 should be paused when global CPU limit exceeded")
	}
	if !paused2 {
		t.Error("container c2 should be paused when global CPU limit exceeded")
	}

	if !m.IsScopeEnforced(model.ScopeHost, model.LimitCPU) {
		t.Error("global CPU should be marked as enforced")
	}
}

func TestGlobalEnforcementNoLimitNoAction(t *testing.T) {
	m, ms, md, mc, _, _ := newTestManager()

	dockerID := "aaaaaaaaaaaa000000"
	containerID := dockerID[:12]

	md.AddContainer(dockerID, "c1", true)
	ms.RegisterContainer(dockerID, "c1")

	cgPath := "/cgroup/c1"
	mc.CgroupPaths[dockerID] = cgPath
	mc.CPUUsage[cgPath] = 999_000_000

	m.enforced[containerID] = make(map[model.LimitType]bool)
	m.checkAndEnforce(nil, containerID, dockerID, false)

	// No global limits set
	m.checkScopeEnforcement(nil, model.ScopeHost)

	paused, _ := md.IsContainerPaused(nil, dockerID)
	if paused {
		t.Error("container should not be paused when no global limits set")
	}
}

func TestGlobalEnforcementReleasesWhenLimitIncreased(t *testing.T) {
	m, ms, md, mc, _, _ := newTestManager()

	dockerID := "aaaaaaaaaaaa000000"
	containerID := dockerID[:12]

	md.AddContainer(dockerID, "c1", true)
	ms.RegisterContainer(dockerID, "c1")

	cgPath := "/cgroup/c1"
	mc.CgroupPaths[dockerID] = cgPath

	// Store usage directly
	ms.SetUsage(containerID, model.LimitCPU, 200)

	// Set global limit at 100s (exceeded)
	ms.SetScopeLimit(model.ScopeHost, model.LimitCPU, 100)
	m.checkScopeEnforcement(nil, model.ScopeHost)

	paused, _ := md.IsContainerPaused(nil, dockerID)
	if !paused {
		t.Fatal("container should be paused when global limit exceeded")
	}

	// Increase global limit to 300s (no longer exceeded)
	ms.SetScopeLimit(model.ScopeHost, model.LimitCPU, 300)
	m.checkScopeEnforcement(nil, model.ScopeHost)

	paused, _ = md.IsContainerPaused(nil, dockerID)
	if paused {
		t.Error("container should be unpaused after global limit increased above usage")
	}

	if m.IsScopeEnforced(model.ScopeHost, model.LimitCPU) {
		t.Error("global CPU should not be marked as enforced after release")
	}
}

func TestIsOtherPauseActiveIncludesGlobal(t *testing.T) {
	m, _, _, _, _, _ := newTestManager()

	m.enforced["c1"] = map[model.LimitType]bool{}
	if m.scopeEnforced[model.ScopeHost] == nil {
		m.scopeEnforced[model.ScopeHost] = make(map[model.LimitType]bool)
	}
	m.scopeEnforced[model.ScopeHost][model.LimitCPU] = true

	// From disk's perspective, global CPU enforcement should count
	if !m.isOtherPauseActive("c1", model.LimitDisk) {
		t.Error("should detect global CPU enforcement as active when checking from Disk")
	}

	// From CPU's perspective, global CPU should not count itself
	if m.isOtherPauseActive("c1", model.LimitCPU) {
		t.Error("should not count same limit type as other pause")
	}
}

func TestFrozenContainerSkipsExpensiveOps(t *testing.T) {
	m, ms, md, mc, _, _ := newTestManager()

	dockerID := "abcdef123456789000"
	containerID := dockerID[:12]

	md.AddContainer(dockerID, "test", true)
	ms.RegisterContainer(dockerID, "test")
	ms.SetLimit(containerID, model.LimitCPU, 100)

	cgPath := "/cgroup/test"
	mc.CgroupPaths[dockerID] = cgPath
	mc.CPUUsage[cgPath] = 50_000_000 // 50 seconds

	// Mark container as frozen
	m.mu.Lock()
	m.frozen[containerID] = true
	m.enforced[containerID] = make(map[model.LimitType]bool)
	m.mu.Unlock()

	md.ResetIsRunningCalls()

	// Run several ticks — none should call Docker API
	for i := 0; i < 10; i++ {
		m.checkAndEnforce(nil, containerID, dockerID, false)
	}

	if calls := md.GetIsRunningCalls(); calls != 0 {
		t.Errorf("frozen container triggered %d IsContainerRunning calls, want 0", calls)
	}
}

func TestFrozenContainerResumesAfterUnfreeze(t *testing.T) {
	m, ms, md, mc, _, _ := newTestManager()

	dockerID := "abcdef123456789000"
	containerID := dockerID[:12]

	md.AddContainer(dockerID, "test", true)
	ms.RegisterContainer(dockerID, "test")
	ms.SetLimit(containerID, model.LimitCPU, 100)

	cgPath := "/cgroup/test"
	mc.CgroupPaths[dockerID] = cgPath
	mc.CPUUsage[cgPath] = 200_000_000 // 200 seconds (exceeds limit)

	// Start frozen — should not enforce
	m.mu.Lock()
	m.frozen[containerID] = true
	m.enforced[containerID] = make(map[model.LimitType]bool)
	m.mu.Unlock()

	m.checkAndEnforce(nil, containerID, dockerID, false)
	paused, _ := md.IsContainerPaused(nil, dockerID)
	if paused {
		t.Error("frozen container should not be paused by enforcement")
	}

	// Unfreeze — next tick should enforce
	m.mu.Lock()
	m.frozen[containerID] = false
	m.mu.Unlock()

	m.checkAndEnforce(nil, containerID, dockerID, false)
	paused, _ = md.IsContainerPaused(nil, dockerID)
	if !paused {
		t.Error("unfrozen container should be paused when CPU limit exceeded")
	}
}

func TestGlobalSpendingEnforcementBlocksProxy(t *testing.T) {
	m, ms, md, mc, mp, _ := newTestManager()

	dockerID1 := "aaaaaaaaaaaa000000"
	dockerID2 := "bbbbbbbbbbbb000000"
	containerID1 := dockerID1[:12]
	containerID2 := dockerID2[:12]

	md.AddContainer(dockerID1, "c1", true)
	md.AddContainer(dockerID2, "c2", true)
	ms.RegisterContainer(dockerID1, "c1")
	ms.RegisterContainer(dockerID2, "c2")

	cgPath1 := "/cgroup/c1"
	cgPath2 := "/cgroup/c2"
	mc.CgroupPaths[dockerID1] = cgPath1
	mc.CgroupPaths[dockerID2] = cgPath2

	// Set spending usage in store (global enforcement reads from store)
	ms.SetUsage(containerID1, model.LimitSpending, 400)
	ms.SetUsage(containerID2, model.LimitSpending, 300)

	// Set global spending limit at 600 (total usage 700 exceeds it)
	ms.SetScopeLimit(model.ScopeHost, model.LimitSpending, 600)

	m.checkScopeEnforcement(nil, model.ScopeHost)

	// Proxy should be globally blocked
	if !mp.IsScopeSpendingBlocked(model.ScopeHost) {
		t.Error("global spending enforcement should set proxy blocked flag")
	}

	if !m.IsScopeEnforced(model.ScopeHost, model.LimitSpending) {
		t.Error("global spending should be marked as enforced")
	}

	// Now increase global limit so usage < limit → should release
	ms.SetScopeLimit(model.ScopeHost, model.LimitSpending, 1000)
	m.checkScopeEnforcement(nil, model.ScopeHost)

	if mp.IsScopeSpendingBlocked(model.ScopeHost) {
		t.Error("global spending enforcement should clear proxy blocked flag after release")
	}

	if m.IsScopeEnforced(model.ScopeHost, model.LimitSpending) {
		t.Error("global spending should not be marked as enforced after release")
	}
}

func TestEnforcementEmitsEnforcementChange(t *testing.T) {
	m, ms, md, mc, _, bus := newTestManager()

	dockerID := "abcdef123456789000"
	containerID := dockerID[:12]

	md.AddContainer(dockerID, "test", true)
	ms.RegisterContainer(dockerID, "test")
	ms.SetLimit(containerID, model.LimitCPU, 100)

	cgPath := "/cgroup/test"
	mc.CgroupPaths[dockerID] = cgPath
	mc.CPUUsage[cgPath] = 200_000_000 // exceeds limit

	sub := bus.Subscribe(events.Filter{Types: []events.EventType{events.EnforcementChange}})
	defer bus.Unsubscribe(sub)

	m.StartContainer(containerID, dockerID)
	defer m.StopContainer(containerID, "test")

	select {
	case e := <-sub.C:
		if e.Type != events.EnforcementChange {
			t.Errorf("type = %s, want enforcement_change", e.Type)
		}
		if e.ContainerID != containerID {
			t.Errorf("container = %s, want %s", e.ContainerID, containerID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for enforcement_change event")
	}
}
