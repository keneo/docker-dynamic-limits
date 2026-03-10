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

	m.StopContainer("c1")

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

	m.StopContainer("c1")
}

func TestStopContainerNonexistent(t *testing.T) {
	m, _, _, _, _, _ := newTestManager()
	// Should not panic
	m.StopContainer("nonexistent")
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
	m.StopContainer(containerID)

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
	m.StopContainer(containerID)

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
	m.StopContainer(containerID)

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
	m.StopContainer(containerID)

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
	m.StopContainer(containerID)

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
	defer m.StopContainer(containerID)

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
	defer m.StopContainer(containerID)

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
