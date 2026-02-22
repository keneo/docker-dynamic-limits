package enforcement

import (
	"testing"
	"time"

	"github.com/keneo/docker-dynamic-limits/internal/model"
	"github.com/keneo/docker-dynamic-limits/internal/testutil"
)

func newTestManager() (*Manager, *testutil.MockStore, *testutil.MockDocker, *testutil.MockCgroup, *testutil.MockProxy) {
	ms := testutil.NewMockStore()
	md := testutil.NewMockDocker()
	mc := testutil.NewMockCgroup()
	mp := testutil.NewMockProxy()
	m := NewManager(ms, md, mc, mp)
	m.interval = 50 * time.Millisecond // faster for tests
	return m, ms, md, mc, mp
}

func TestStartStopContainer(t *testing.T) {
	m, _, _, _, _ := newTestManager()

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
	m, _, _, _, _ := newTestManager()

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
	m, _, _, _, _ := newTestManager()
	// Should not panic
	m.StopContainer("nonexistent")
}

func TestIsEnforcedDefault(t *testing.T) {
	m, _, _, _, _ := newTestManager()
	if m.IsEnforced("c1", model.LimitCPU) {
		t.Error("should not be enforced by default")
	}
}

func TestGetEnforcedEmpty(t *testing.T) {
	m, _, _, _, _ := newTestManager()
	enforced := m.GetEnforced("c1")
	if len(enforced) != 0 {
		t.Errorf("expected empty map, got %v", enforced)
	}
}

func TestNotifyLimitChanged(t *testing.T) {
	m, _, _, _, _ := newTestManager()
	// Should not panic
	m.NotifyLimitChanged("c1")
}

func TestEnforcementCPUPause(t *testing.T) {
	m, ms, md, mc, _ := newTestManager()

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
	m, ms, md, mc, _ := newTestManager()

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
	m, ms, md, mc, _ := newTestManager()

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
	m, ms, md, _, _ := newTestManager()

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

func TestIsOtherPauseActive(t *testing.T) {
	m, _, _, _, _ := newTestManager()

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
