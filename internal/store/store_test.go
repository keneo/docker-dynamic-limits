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

func TestSetAndGetScopeLimit(t *testing.T) {
	s := newTestStore(t)

	err := s.SetScopeLimit(model.ScopeHost, model.LimitCPU, 86400)
	if err != nil {
		t.Fatalf("SetScopeLimit: %v", err)
	}

	got, err := s.GetScopeLimit(model.ScopeHost, model.LimitCPU)
	if err != nil {
		t.Fatalf("GetScopeLimit: %v", err)
	}
	if got != 86400 {
		t.Errorf("got %d, want 86400", got)
	}
}

func TestGetScopeLimitDefault(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetScopeLimit(model.ScopeHost, model.LimitCPU)
	if err != nil {
		t.Fatalf("GetScopeLimit: %v", err)
	}
	if got != 0 {
		t.Errorf("default global limit = %d, want 0", got)
	}
}

func TestSetScopeLimitOverwrite(t *testing.T) {
	s := newTestStore(t)
	s.SetScopeLimit(model.ScopeHost, model.LimitCPU, 100)
	s.SetScopeLimit(model.ScopeHost, model.LimitCPU, 200)

	got, _ := s.GetScopeLimit(model.ScopeHost, model.LimitCPU)
	if got != 200 {
		t.Errorf("got %d, want 200", got)
	}
}

func TestGetAllScopeLimits(t *testing.T) {
	s := newTestStore(t)
	s.SetScopeLimit(model.ScopeHost, model.LimitCPU, 100)
	s.SetScopeLimit(model.ScopeHost, model.LimitSpending, 500)

	all, err := s.GetAllScopeLimits(model.ScopeHost)
	if err != nil {
		t.Fatalf("GetAllScopeLimits: %v", err)
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

func TestGetAllScopeLimitsEmpty(t *testing.T) {
	s := newTestStore(t)
	all, err := s.GetAllScopeLimits(model.ScopeHost)
	if err != nil {
		t.Fatalf("GetAllScopeLimits: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("len = %d, want 0", len(all))
	}
}

func TestCreateSegment(t *testing.T) {
	s := newTestStore(t)
	seg, err := s.CreateSegment("seg1", "Segment One")
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	if seg.ID != "seg1" {
		t.Errorf("ID = %q, want %q", seg.ID, "seg1")
	}
	if seg.Name != "Segment One" {
		t.Errorf("Name = %q, want %q", seg.Name, "Segment One")
	}
	if seg.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	got, err := s.GetSegment("seg1")
	if err != nil {
		t.Fatalf("GetSegment: %v", err)
	}
	if got.ID != "seg1" {
		t.Errorf("GetSegment ID = %q, want %q", got.ID, "seg1")
	}
	if got.Name != "Segment One" {
		t.Errorf("GetSegment Name = %q, want %q", got.Name, "Segment One")
	}
}

func TestCreateSegmentDuplicate(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CreateSegment("seg1", "First")
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	_, err = s.CreateSegment("seg1", "Second")
	if err == nil {
		t.Fatal("expected error for duplicate segment ID")
	}
}

func TestListSegments(t *testing.T) {
	s := newTestStore(t)
	s.CreateSegment("seg-a", "Alpha")
	s.CreateSegment("seg-b", "Beta")
	s.CreateSegment("seg-c", "Gamma")

	list, err := s.ListSegments()
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3", len(list))
	}
	if list[0].ID != "seg-a" {
		t.Errorf("list[0].ID = %q, want %q", list[0].ID, "seg-a")
	}
	if list[1].ID != "seg-b" {
		t.Errorf("list[1].ID = %q, want %q", list[1].ID, "seg-b")
	}
	if list[2].ID != "seg-c" {
		t.Errorf("list[2].ID = %q, want %q", list[2].ID, "seg-c")
	}
}

func TestDeleteSegment(t *testing.T) {
	s := newTestStore(t)
	s.CreateSegment("seg1", "ToDelete")

	err := s.DeleteSegment("seg1")
	if err != nil {
		t.Fatalf("DeleteSegment: %v", err)
	}

	_, err = s.GetSegment("seg1")
	if err == nil {
		t.Fatal("expected error after deleting segment")
	}

	list, _ := s.ListSegments()
	if len(list) != 0 {
		t.Errorf("ListSegments len = %d, want 0", len(list))
	}
}

func TestDeleteSegmentWithContainers(t *testing.T) {
	s := newTestStore(t)
	s.CreateSegment("seg1", "HasContainers")
	c, _ := s.RegisterContainer("abcdef123456789000", "test")
	segID := "seg1"
	s.SetContainerSegment(c.ID, &segID)

	err := s.DeleteSegment("seg1")
	if err == nil {
		t.Fatal("expected error when deleting segment with containers")
	}
}

func TestSetContainerSegment(t *testing.T) {
	s := newTestStore(t)
	s.CreateSegment("seg1", "MySegment")
	c, _ := s.RegisterContainer("abcdef123456789000", "test")

	segID := "seg1"
	err := s.SetContainerSegment(c.ID, &segID)
	if err != nil {
		t.Fatalf("SetContainerSegment: %v", err)
	}

	got, err := s.GetContainer(c.ID)
	if err != nil {
		t.Fatalf("GetContainer: %v", err)
	}
	if got.SegmentID != "seg1" {
		t.Errorf("SegmentID = %q, want %q", got.SegmentID, "seg1")
	}

	// Remove from segment
	err = s.SetContainerSegment(c.ID, nil)
	if err != nil {
		t.Fatalf("SetContainerSegment(nil): %v", err)
	}
	got, _ = s.GetContainer(c.ID)
	if got.SegmentID != "" {
		t.Errorf("SegmentID after nil = %q, want empty", got.SegmentID)
	}
}

func TestListContainersByScope(t *testing.T) {
	s := newTestStore(t)
	s.CreateSegment("seg-a", "Alpha")
	s.CreateSegment("seg-b", "Beta")

	c1, _ := s.RegisterContainer("aaaaaaaaaaaa000000", "c1")
	c2, _ := s.RegisterContainer("bbbbbbbbbbbb000000", "c2")
	s.RegisterContainer("cccccccccccc000000", "c3")
	// c3 has no segment

	segA := "seg-a"
	segB := "seg-b"
	s.SetContainerSegment(c1.ID, &segA)
	s.SetContainerSegment(c2.ID, &segB)

	// ScopeHost returns all containers
	all, err := s.ListContainersByScope(model.ScopeHost)
	if err != nil {
		t.Fatalf("ListContainersByScope(Host): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("Host scope len = %d, want 3", len(all))
	}

	// Segment A has only c1
	listA, err := s.ListContainersByScope(model.SegmentScope("seg-a"))
	if err != nil {
		t.Fatalf("ListContainersByScope(seg-a): %v", err)
	}
	if len(listA) != 1 {
		t.Fatalf("seg-a len = %d, want 1", len(listA))
	}
	if listA[0].ID != c1.ID {
		t.Errorf("seg-a[0].ID = %q, want %q", listA[0].ID, c1.ID)
	}

	// Segment B has only c2
	listB, err := s.ListContainersByScope(model.SegmentScope("seg-b"))
	if err != nil {
		t.Fatalf("ListContainersByScope(seg-b): %v", err)
	}
	if len(listB) != 1 {
		t.Fatalf("seg-b len = %d, want 1", len(listB))
	}
	if listB[0].ID != c2.ID {
		t.Errorf("seg-b[0].ID = %q, want %q", listB[0].ID, c2.ID)
	}
}

func TestSegmentScopeLimits(t *testing.T) {
	s := newTestStore(t)
	s.CreateSegment("seg1", "WithLimits")

	segScope := model.SegmentScope("seg1")

	// Set limits on segment scope
	err := s.SetScopeLimit(segScope, model.LimitCPU, 7200)
	if err != nil {
		t.Fatalf("SetScopeLimit(segment): %v", err)
	}
	err = s.SetScopeLimit(segScope, model.LimitSpending, 1000)
	if err != nil {
		t.Fatalf("SetScopeLimit(segment spending): %v", err)
	}

	// Set a different limit on host scope
	err = s.SetScopeLimit(model.ScopeHost, model.LimitCPU, 86400)
	if err != nil {
		t.Fatalf("SetScopeLimit(host): %v", err)
	}

	// Verify segment limits are independent from host
	segCPU, err := s.GetScopeLimit(segScope, model.LimitCPU)
	if err != nil {
		t.Fatalf("GetScopeLimit(segment cpu): %v", err)
	}
	if segCPU != 7200 {
		t.Errorf("segment cpu = %d, want 7200", segCPU)
	}

	hostCPU, err := s.GetScopeLimit(model.ScopeHost, model.LimitCPU)
	if err != nil {
		t.Fatalf("GetScopeLimit(host cpu): %v", err)
	}
	if hostCPU != 86400 {
		t.Errorf("host cpu = %d, want 86400", hostCPU)
	}

	// GetAllScopeLimits for segment
	segAll, err := s.GetAllScopeLimits(segScope)
	if err != nil {
		t.Fatalf("GetAllScopeLimits(segment): %v", err)
	}
	if len(segAll) != 2 {
		t.Fatalf("segment limits len = %d, want 2", len(segAll))
	}
	if segAll[model.LimitSpending] != 1000 {
		t.Errorf("segment spending = %d, want 1000", segAll[model.LimitSpending])
	}
}

func TestRemoveContainerAccumulatesIntoSegmentScope(t *testing.T) {
	s := newTestStore(t)
	s.CreateSegment("seg1", "AccumTest")

	c, _ := s.RegisterContainer("abcdef123456789000", "test")
	segID := "seg1"
	s.SetContainerSegment(c.ID, &segID)

	// Set usage on a cumulative limit type
	s.SetUsage(c.ID, model.LimitCPU, 500)
	s.SetUsage(c.ID, model.LimitSpending, 200)

	err := s.RemoveContainer(c.ID)
	if err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}

	// Verify host scope accumulator
	hostAccum, err := s.GetScopeUsageAccum(model.ScopeHost)
	if err != nil {
		t.Fatalf("GetScopeUsageAccum(host): %v", err)
	}
	if hostAccum[model.LimitCPU] != 500 {
		t.Errorf("host accum cpu = %d, want 500", hostAccum[model.LimitCPU])
	}
	if hostAccum[model.LimitSpending] != 200 {
		t.Errorf("host accum spending = %d, want 200", hostAccum[model.LimitSpending])
	}

	// Verify segment scope accumulator
	segScope := model.SegmentScope("seg1")
	segAccum, err := s.GetScopeUsageAccum(segScope)
	if err != nil {
		t.Fatalf("GetScopeUsageAccum(segment): %v", err)
	}
	if segAccum[model.LimitCPU] != 500 {
		t.Errorf("segment accum cpu = %d, want 500", segAccum[model.LimitCPU])
	}
	if segAccum[model.LimitSpending] != 200 {
		t.Errorf("segment accum spending = %d, want 200", segAccum[model.LimitSpending])
	}
}

func TestSegmentConfig(t *testing.T) {
	s := newTestStore(t)
	s.CreateSegment("seg1", "ConfigTest")

	// Set config keys
	err := s.SetSegmentConfig("seg1", "anthropic-enabled", "true")
	if err != nil {
		t.Fatalf("SetSegmentConfig: %v", err)
	}
	err = s.SetSegmentConfig("seg1", "openai-enabled", "false")
	if err != nil {
		t.Fatalf("SetSegmentConfig: %v", err)
	}

	// Get single key
	val, ok, err := s.GetSegmentConfig("seg1", "anthropic-enabled")
	if err != nil {
		t.Fatalf("GetSegmentConfig: %v", err)
	}
	if !ok {
		t.Fatal("expected key to exist")
	}
	if val != "true" {
		t.Errorf("val = %q, want %q", val, "true")
	}

	// Get nonexistent key
	_, ok, err = s.GetSegmentConfig("seg1", "nonexistent")
	if err != nil {
		t.Fatalf("GetSegmentConfig(nonexistent): %v", err)
	}
	if ok {
		t.Error("expected ok=false for nonexistent key")
	}

	// Get all config
	all, err := s.GetAllSegmentConfig("seg1")
	if err != nil {
		t.Fatalf("GetAllSegmentConfig: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len = %d, want 2", len(all))
	}
	if all["anthropic-enabled"] != "true" {
		t.Errorf("anthropic-enabled = %q, want %q", all["anthropic-enabled"], "true")
	}
	if all["openai-enabled"] != "false" {
		t.Errorf("openai-enabled = %q, want %q", all["openai-enabled"], "false")
	}

	// Delete a key
	err = s.DeleteSegmentConfig("seg1", "anthropic-enabled")
	if err != nil {
		t.Fatalf("DeleteSegmentConfig: %v", err)
	}
	_, ok, _ = s.GetSegmentConfig("seg1", "anthropic-enabled")
	if ok {
		t.Error("expected key to be deleted")
	}

	// Verify only one key remains
	all, _ = s.GetAllSegmentConfig("seg1")
	if len(all) != 1 {
		t.Errorf("len after delete = %d, want 1", len(all))
	}
}
