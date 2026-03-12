package store

import (
	"testing"

	"github.com/keneo/docker-dynamic-limits/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRegisterAndGetContainer(t *testing.T) {
	s := newTestStore(t)
	dockerID := "abcdef123456789000"
	c, err := s.RegisterContainer(dockerID, "test-container")
	if err != nil {
		t.Fatalf("RegisterContainer: %v", err)
	}
	if c.ID != dockerID[:12] {
		t.Errorf("ID = %q, want %q", c.ID, dockerID[:12])
	}
	if c.DockerID != dockerID {
		t.Errorf("DockerID = %q, want %q", c.DockerID, dockerID)
	}
	if c.Name != "test-container" {
		t.Errorf("Name = %q, want %q", c.Name, "test-container")
	}

	got, err := s.GetContainer(c.ID)
	if err != nil {
		t.Fatalf("GetContainer: %v", err)
	}
	if got.DockerID != dockerID {
		t.Errorf("GetContainer DockerID = %q, want %q", got.DockerID, dockerID)
	}
}

func TestGetContainerNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetContainer("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent container")
	}
}

func TestListContainers(t *testing.T) {
	s := newTestStore(t)
	s.RegisterContainer("aaaaaaaaaaaa000000", "c1")
	s.RegisterContainer("bbbbbbbbbbbb000000", "c2")

	list, err := s.ListContainers()
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
}

func TestListContainersEmpty(t *testing.T) {
	s := newTestStore(t)
	list, err := s.ListContainers()
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("len = %d, want 0", len(list))
	}
}

func TestRemoveContainer(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.RegisterContainer("abcdef123456789000", "test")
	s.SetLimit(c.ID, model.LimitCPU, 100)
	s.SetUsage(c.ID, model.LimitCPU, 50)

	err := s.RemoveContainer(c.ID)
	if err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}

	_, err = s.GetContainer(c.ID)
	if err == nil {
		t.Fatal("expected error after removal")
	}

	// Limits and usage should also be gone
	lim, _ := s.GetLimit(c.ID, model.LimitCPU)
	if lim != 0 {
		t.Errorf("limit after removal = %d, want 0", lim)
	}
	usg, _ := s.GetUsage(c.ID, model.LimitCPU)
	if usg != 0 {
		t.Errorf("usage after removal = %d, want 0", usg)
	}
}

func TestSetAndGetLimit(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.RegisterContainer("abcdef123456789000", "test")

	err := s.SetLimit(c.ID, model.LimitCPU, 3600)
	if err != nil {
		t.Fatalf("SetLimit: %v", err)
	}

	got, err := s.GetLimit(c.ID, model.LimitCPU)
	if err != nil {
		t.Fatalf("GetLimit: %v", err)
	}
	if got != 3600 {
		t.Errorf("got %d, want 3600", got)
	}
}

func TestGetLimitDefault(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.RegisterContainer("abcdef123456789000", "test")
	got, err := s.GetLimit(c.ID, model.LimitCPU)
	if err != nil {
		t.Fatalf("GetLimit: %v", err)
	}
	if got != 0 {
		t.Errorf("default limit = %d, want 0", got)
	}
}

func TestSetLimitOverwrite(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.RegisterContainer("abcdef123456789000", "test")
	s.SetLimit(c.ID, model.LimitCPU, 100)
	s.SetLimit(c.ID, model.LimitCPU, 200)

	got, _ := s.GetLimit(c.ID, model.LimitCPU)
	if got != 200 {
		t.Errorf("got %d, want 200", got)
	}
}

func TestGetAllLimits(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.RegisterContainer("abcdef123456789000", "test")
	s.SetLimit(c.ID, model.LimitCPU, 100)
	s.SetLimit(c.ID, model.LimitRAM, 200)

	all, err := s.GetAllLimits(c.ID)
	if err != nil {
		t.Fatalf("GetAllLimits: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len = %d, want 2", len(all))
	}
	if all[model.LimitCPU] != 100 {
		t.Errorf("cpu = %d, want 100", all[model.LimitCPU])
	}
	if all[model.LimitRAM] != 200 {
		t.Errorf("ram = %d, want 200", all[model.LimitRAM])
	}
}

func TestSetAndGetUsage(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.RegisterContainer("abcdef123456789000", "test")
	s.SetUsage(c.ID, model.LimitCPU, 42)

	got, err := s.GetUsage(c.ID, model.LimitCPU)
	if err != nil {
		t.Fatalf("GetUsage: %v", err)
	}
	if got != 42 {
		t.Errorf("got %d, want 42", got)
	}
}

func TestGetAllUsage(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.RegisterContainer("abcdef123456789000", "test")
	s.SetUsage(c.ID, model.LimitCPU, 10)
	s.SetUsage(c.ID, model.LimitRAM, 20)

	all, err := s.GetAllUsage(c.ID)
	if err != nil {
		t.Fatalf("GetAllUsage: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len = %d, want 2", len(all))
	}
}

func TestAddUsage(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.RegisterContainer("abcdef123456789000", "test")

	s.AddUsage(c.ID, model.LimitCPU, 10)
	s.AddUsage(c.ID, model.LimitCPU, 5)

	got, _ := s.GetUsage(c.ID, model.LimitCPU)
	if got != 15 {
		t.Errorf("got %d, want 15", got)
	}
}

func TestCopyLimits(t *testing.T) {
	s := newTestStore(t)
	c1, _ := s.RegisterContainer("aaaaaaaaaaaa000000", "src")
	c2, _ := s.RegisterContainer("bbbbbbbbbbbb000000", "dst")

	s.SetLimit(c1.ID, model.LimitCPU, 100)
	s.SetLimit(c1.ID, model.LimitRAM, 200)

	err := s.CopyLimits(c1.ID, c2.ID)
	if err != nil {
		t.Fatalf("CopyLimits: %v", err)
	}

	cpu, _ := s.GetLimit(c2.ID, model.LimitCPU)
	ram, _ := s.GetLimit(c2.ID, model.LimitRAM)
	if cpu != 100 {
		t.Errorf("copied cpu = %d, want 100", cpu)
	}
	if ram != 200 {
		t.Errorf("copied ram = %d, want 200", ram)
	}
}

func TestResolveContainerIDByExactID(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.RegisterContainer("abcdef123456789000", "test")

	got, err := s.ResolveContainerID(c.ID)
	if err != nil {
		t.Fatalf("ResolveContainerID: %v", err)
	}
	if got != c.ID {
		t.Errorf("got %q, want %q", got, c.ID)
	}
}

func TestResolveContainerIDByDockerPrefix(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.RegisterContainer("abcdef123456789000", "test")

	got, err := s.ResolveContainerID("abcdef123456789")
	if err != nil {
		t.Fatalf("ResolveContainerID: %v", err)
	}
	if got != c.ID {
		t.Errorf("got %q, want %q", got, c.ID)
	}
}

func TestResolveContainerIDByName(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.RegisterContainer("abcdef123456789000", "my-container")

	got, err := s.ResolveContainerID("my-container")
	if err != nil {
		t.Fatalf("ResolveContainerID: %v", err)
	}
	if got != c.ID {
		t.Errorf("got %q, want %q", got, c.ID)
	}
}

func TestResolveContainerIDNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.ResolveContainerID("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent query")
	}
}

func TestRegisterContainerReplace(t *testing.T) {
	s := newTestStore(t)
	dockerID := "abcdef123456789000"
	s.RegisterContainer(dockerID, "name1")
	c, err := s.RegisterContainer(dockerID, "name2")
	if err != nil {
		t.Fatalf("RegisterContainer replace: %v", err)
	}
	if c.Name != "name2" {
		t.Errorf("Name = %q, want %q", c.Name, "name2")
	}

	got, _ := s.GetContainer(c.ID)
	if got.Name != "name2" {
		t.Errorf("GetContainer Name = %q, want %q", got.Name, "name2")
	}
}

func TestSetAndGetGlobalLimit(t *testing.T) {
	s := newTestStore(t)

	err := s.SetGlobalLimit(model.LimitCPU, 86400)
	if err != nil {
		t.Fatalf("SetGlobalLimit: %v", err)
	}

	got, err := s.GetGlobalLimit(model.LimitCPU)
	if err != nil {
		t.Fatalf("GetGlobalLimit: %v", err)
	}
	if got != 86400 {
		t.Errorf("got %d, want 86400", got)
	}
}

func TestGetGlobalLimitDefault(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetGlobalLimit(model.LimitCPU)
	if err != nil {
		t.Fatalf("GetGlobalLimit: %v", err)
	}
	if got != 0 {
		t.Errorf("default global limit = %d, want 0", got)
	}
}

func TestSetGlobalLimitOverwrite(t *testing.T) {
	s := newTestStore(t)
	s.SetGlobalLimit(model.LimitCPU, 100)
	s.SetGlobalLimit(model.LimitCPU, 200)

	got, _ := s.GetGlobalLimit(model.LimitCPU)
	if got != 200 {
		t.Errorf("got %d, want 200", got)
	}
}

func TestGetAllGlobalLimits(t *testing.T) {
	s := newTestStore(t)
	s.SetGlobalLimit(model.LimitCPU, 100)
	s.SetGlobalLimit(model.LimitSpending, 500)

	all, err := s.GetAllGlobalLimits()
	if err != nil {
		t.Fatalf("GetAllGlobalLimits: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len = %d, want 2", len(all))
	}
	if all[model.LimitCPU] != 100 {
		t.Errorf("cpu = %d, want 100", all[model.LimitCPU])
	}
	if all[model.LimitSpending] != 500 {
		t.Errorf("spending = %d, want 500", all[model.LimitSpending])
	}
}

func TestGetAllGlobalLimitsEmpty(t *testing.T) {
	s := newTestStore(t)
	all, err := s.GetAllGlobalLimits()
	if err != nil {
		t.Fatalf("GetAllGlobalLimits: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("len = %d, want 0", len(all))
	}
}
