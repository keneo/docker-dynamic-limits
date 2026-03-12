package testutil

import (
	"context"
	"fmt"
	"sync"

	"github.com/docker/docker/api/types"
	"github.com/keneo/docker-dynamic-limits/internal/cgroup"
	"github.com/keneo/docker-dynamic-limits/internal/model"
)

// --- MockStore ---

// MockStore implements store.DataStore using in-memory maps.
type MockStore struct {
	mu           sync.Mutex
	Containers   map[string]*model.Container
	Limits       map[string]map[model.LimitType]int64
	Usages       map[string]map[model.LimitType]int64
	GlobalLimits map[model.LimitType]int64
}

func NewMockStore() *MockStore {
	return &MockStore{
		Containers:   make(map[string]*model.Container),
		Limits:       make(map[string]map[model.LimitType]int64),
		Usages:       make(map[string]map[model.LimitType]int64),
		GlobalLimits: make(map[model.LimitType]int64),
	}
}

func (s *MockStore) RegisterContainer(dockerID, name string) (*model.Container, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := dockerID
	if len(dockerID) >= 12 {
		id = dockerID[:12]
	}
	c := &model.Container{ID: id, DockerID: dockerID, Name: name}
	s.Containers[id] = c
	return c, nil
}

func (s *MockStore) GetContainer(id string) (*model.Container, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.Containers[id]
	if !ok {
		return nil, fmt.Errorf("container %s not found", id)
	}
	return c, nil
}

func (s *MockStore) ListContainers() ([]model.Container, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []model.Container
	for _, c := range s.Containers {
		result = append(result, *c)
	}
	return result, nil
}

func (s *MockStore) RemoveContainer(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Containers, id)
	delete(s.Limits, id)
	delete(s.Usages, id)
	return nil
}

func (s *MockStore) SetLimit(containerID string, limitType model.LimitType, value int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Limits[containerID] == nil {
		s.Limits[containerID] = make(map[model.LimitType]int64)
	}
	s.Limits[containerID][limitType] = value
	return nil
}

func (s *MockStore) GetLimit(containerID string, limitType model.LimitType) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.Limits[containerID]; ok {
		return m[limitType], nil
	}
	return 0, nil
}

func (s *MockStore) GetAllLimits(containerID string) (map[model.LimitType]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[model.LimitType]int64)
	if m, ok := s.Limits[containerID]; ok {
		for k, v := range m {
			result[k] = v
		}
	}
	return result, nil
}

func (s *MockStore) SetUsage(containerID string, limitType model.LimitType, value int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Usages[containerID] == nil {
		s.Usages[containerID] = make(map[model.LimitType]int64)
	}
	s.Usages[containerID][limitType] = value
	return nil
}

func (s *MockStore) GetUsage(containerID string, limitType model.LimitType) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.Usages[containerID]; ok {
		return m[limitType], nil
	}
	return 0, nil
}

func (s *MockStore) GetAllUsage(containerID string) (map[model.LimitType]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[model.LimitType]int64)
	if m, ok := s.Usages[containerID]; ok {
		for k, v := range m {
			result[k] = v
		}
	}
	return result, nil
}

func (s *MockStore) AddUsage(containerID string, limitType model.LimitType, delta int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Usages[containerID] == nil {
		s.Usages[containerID] = make(map[model.LimitType]int64)
	}
	s.Usages[containerID][limitType] += delta
	return nil
}

func (s *MockStore) CopyLimits(fromID, toID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.Limits[fromID]
	if src == nil {
		return nil
	}
	dst := make(map[model.LimitType]int64)
	for k, v := range src {
		dst[k] = v
	}
	s.Limits[toID] = dst
	return nil
}

func (s *MockStore) ResolveContainerID(query string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Exact ID match
	if _, ok := s.Containers[query]; ok {
		return query, nil
	}
	// Name match
	for id, c := range s.Containers {
		if c.Name == query {
			return id, nil
		}
	}
	return "", fmt.Errorf("container %q not found", query)
}

func (s *MockStore) SetGlobalLimit(limitType model.LimitType, value int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.GlobalLimits[limitType] = value
	return nil
}

func (s *MockStore) GetGlobalLimit(limitType model.LimitType) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.GlobalLimits[limitType], nil
}

func (s *MockStore) GetAllGlobalLimits() (map[model.LimitType]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[model.LimitType]int64)
	for k, v := range s.GlobalLimits {
		result[k] = v
	}
	return result, nil
}

// --- MockDocker ---

// MockDocker implements docker.DockerClient with configurable behavior.
type MockDocker struct {
	mu sync.Mutex
	// State
	Containers     map[string]*MockContainerState
	ClonedFrom     []string // track clone calls
	CloneIDCounter int
	StoppedContainers []string // track StopContainer calls
}

type MockContainerState struct {
	Running     bool
	Paused      bool
	MemoryLimit int64
	Networks    []string
	DiskUsage   int64
	Name        string
	IP          string
}

func NewMockDocker() *MockDocker {
	return &MockDocker{
		Containers: make(map[string]*MockContainerState),
	}
}

func (d *MockDocker) AddContainer(id string, name string, running bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Containers[id] = &MockContainerState{
		Running:  running,
		Networks: []string{"bridge"},
		Name:     name,
	}
}

func (d *MockDocker) InspectContainer(ctx context.Context, id string) (types.ContainerJSON, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.Containers[id]
	if !ok {
		return types.ContainerJSON{}, fmt.Errorf("container %s not found", id)
	}
	return types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			ID:   id,
			Name: "/" + state.Name,
			State: &types.ContainerState{
				Running: state.Running,
				Paused:  state.Paused,
			},
		},
	}, nil
}

func (d *MockDocker) PauseContainer(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.Containers[id]
	if !ok {
		return fmt.Errorf("container %s not found", id)
	}
	if state.Paused {
		return fmt.Errorf("Error response from daemon: Container %s is already paused", id)
	}
	state.Paused = true
	return nil
}

func (d *MockDocker) UnpauseContainer(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.Containers[id]
	if !ok {
		return fmt.Errorf("container %s not found", id)
	}
	state.Paused = false
	return nil
}

func (d *MockDocker) IsContainerPaused(ctx context.Context, id string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.Containers[id]
	if !ok {
		return false, fmt.Errorf("container %s not found", id)
	}
	return state.Paused, nil
}

func (d *MockDocker) IsContainerRunning(ctx context.Context, id string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.Containers[id]
	if !ok {
		return false, fmt.Errorf("container %s not found", id)
	}
	return state.Running, nil
}

func (d *MockDocker) UpdateMemoryLimit(ctx context.Context, id string, memoryBytes int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.Containers[id]
	if !ok {
		return fmt.Errorf("container %s not found", id)
	}
	state.MemoryLimit = memoryBytes
	return nil
}

func (d *MockDocker) ContainerVethName(ctx context.Context, id string) (string, error) {
	return "veth-mock", nil
}

func (d *MockDocker) CloneContainer(ctx context.Context, sourceID string, newName string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.Containers[sourceID]; !ok {
		return "", fmt.Errorf("container %s not found", sourceID)
	}
	d.ClonedFrom = append(d.ClonedFrom, sourceID)
	d.CloneIDCounter++
	newID := fmt.Sprintf("clone%06d%06d", d.CloneIDCounter, 0)
	if newName == "" {
		newName = "clone"
	}
	d.Containers[newID] = &MockContainerState{
		Running:  true,
		Networks: []string{"bridge"},
		Name:     newName,
	}
	return newID, nil
}

func (d *MockDocker) GetContainerDiskUsage(ctx context.Context, id string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.Containers[id]
	if !ok {
		return 0, fmt.Errorf("container %s not found", id)
	}
	return state.DiskUsage, nil
}

func (d *MockDocker) DisconnectNetwork(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.Containers[id]
	if !ok {
		return fmt.Errorf("container %s not found", id)
	}
	state.Networks = nil
	return nil
}

func (d *MockDocker) ReconnectNetwork(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.Containers[id]
	if !ok {
		return fmt.Errorf("container %s not found", id)
	}
	state.Networks = []string{"bridge"}
	return nil
}

func (d *MockDocker) ContainerIP(ctx context.Context, id string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.Containers[id]
	if !ok {
		return "", fmt.Errorf("container %s not found", id)
	}
	if state.IP == "" {
		return "", fmt.Errorf("no IP address for container %s", id)
	}
	return state.IP, nil
}

func (d *MockDocker) StopContainer(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.Containers[id]
	if !ok {
		return fmt.Errorf("container %s not found", id)
	}
	state.Running = false
	d.StoppedContainers = append(d.StoppedContainers, id)
	return nil
}

func (d *MockDocker) SetContainerIP(id, ip string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if state, ok := d.Containers[id]; ok {
		state.IP = ip
	}
}

// --- MockCgroup ---

// MockCgroup implements cgroup.CgroupReader with configurable maps.
type MockCgroup struct {
	mu          sync.Mutex
	CgroupPaths map[string]string // dockerID -> cgroupPath
	CPUUsage    map[string]int64  // cgroupPath -> usec
	MemCurrent  map[string]int64  // cgroupPath -> bytes
	MemMax      map[string]int64  // cgroupPath -> bytes
	IOStats     map[string]*cgroup.IOStats
	IOMax       map[string]string // cgroupPath -> last SetIOMax call
	NetStats    map[string]*cgroup.NetworkStats
}

func NewMockCgroup() *MockCgroup {
	return &MockCgroup{
		CgroupPaths: make(map[string]string),
		CPUUsage:    make(map[string]int64),
		MemCurrent:  make(map[string]int64),
		MemMax:      make(map[string]int64),
		IOStats:     make(map[string]*cgroup.IOStats),
		IOMax:       make(map[string]string),
		NetStats:    make(map[string]*cgroup.NetworkStats),
	}
}

func (c *MockCgroup) FindCgroupPath(dockerID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.CgroupPaths[dockerID]
	if !ok {
		return "", fmt.Errorf("cgroup path not found for container %s", dockerID)
	}
	return p, nil
}

func (c *MockCgroup) CPUUsageMicroseconds(cgroupPath string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.CPUUsage[cgroupPath]
	if !ok {
		return 0, fmt.Errorf("cpu.stat not found")
	}
	return v, nil
}

func (c *MockCgroup) MemoryCurrent(cgroupPath string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.MemCurrent[cgroupPath]
	if !ok {
		return 0, fmt.Errorf("memory.current not found")
	}
	return v, nil
}

func (c *MockCgroup) SetMemoryMax(cgroupPath string, bytes int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.MemMax[cgroupPath] = bytes
	return nil
}

func (c *MockCgroup) ReadIOStat(cgroupPath string) (*cgroup.IOStats, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.IOStats[cgroupPath]
	if !ok {
		return &cgroup.IOStats{}, nil
	}
	return s, nil
}

func (c *MockCgroup) SetIOMax(cgroupPath string, deviceNum string, bps int64, iops int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.IOMax[cgroupPath] = fmt.Sprintf("%s rbps=%d wbps=%d riops=%d wiops=%d", deviceNum, bps, bps, iops, iops)
	return nil
}

func (c *MockCgroup) RemoveIOMax(cgroupPath string, deviceNum string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.IOMax, cgroupPath)
	return nil
}

func (c *MockCgroup) ReadNetworkStats(vethName string) (*cgroup.NetworkStats, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.NetStats[vethName]
	if !ok {
		return &cgroup.NetworkStats{}, nil
	}
	return s, nil
}

// --- MockEnforcement ---

// MockEnforcement implements enforcement.EnforcementController.
type MockEnforcement struct {
	mu              sync.Mutex
	Started         map[string]string          // containerID -> dockerID
	Stopped         []string
	Enforced        map[string]map[model.LimitType]bool
	Notified        []string
	GlobalEnforced  map[model.LimitType]bool
	GlobalStarted   bool
}

func NewMockEnforcement() *MockEnforcement {
	return &MockEnforcement{
		Started:        make(map[string]string),
		Enforced:       make(map[string]map[model.LimitType]bool),
		GlobalEnforced: make(map[model.LimitType]bool),
	}
}

func (e *MockEnforcement) StartContainer(containerID string, dockerID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Started[containerID] = dockerID
}

func (e *MockEnforcement) StopContainer(containerID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Stopped = append(e.Stopped, containerID)
	delete(e.Started, containerID)
}

func (e *MockEnforcement) IsEnforced(containerID string, lt model.LimitType) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if m, ok := e.Enforced[containerID]; ok {
		return m[lt]
	}
	return false
}

func (e *MockEnforcement) GetEnforced(containerID string) map[model.LimitType]bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make(map[model.LimitType]bool)
	if m, ok := e.Enforced[containerID]; ok {
		for k, v := range m {
			result[k] = v
		}
	}
	return result
}

func (e *MockEnforcement) NotifyLimitChanged(containerID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Notified = append(e.Notified, containerID)
}

// WasStarted returns true if StartContainer was called for the given ID.
func (e *MockEnforcement) WasStarted(containerID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.Started[containerID]
	return ok
}

// WasStopped returns true if StopContainer was called for the given ID.
func (e *MockEnforcement) WasStopped(containerID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range e.Stopped {
		if id == containerID {
			return true
		}
	}
	return false
}

// WasNotified returns true if NotifyLimitChanged was called at least once.
func (e *MockEnforcement) WasNotified() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.Notified) > 0
}

func (e *MockEnforcement) StartGlobalEnforcement() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.GlobalStarted = true
}

func (e *MockEnforcement) IsGlobalEnforced(lt model.LimitType) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.GlobalEnforced[lt]
}

func (e *MockEnforcement) GetGlobalEnforced() map[model.LimitType]bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make(map[model.LimitType]bool)
	for k, v := range e.GlobalEnforced {
		result[k] = v
	}
	return result
}

// --- MockProxy ---

// MockProxy implements proxy.SpendingProxy with in-memory state.
type MockProxy struct {
	mu       sync.Mutex
	Spending map[string]int64
	Budgets  map[string]int64
	Addrs    map[string]string
}

func NewMockProxy() *MockProxy {
	return &MockProxy{
		Spending: make(map[string]int64),
		Budgets:  make(map[string]int64),
		Addrs:    make(map[string]string),
	}
}

func (p *MockProxy) RegisterContainer(containerID string, budget int64, existingSpending int64) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	addr := fmt.Sprintf("127.0.0.1:%d", 10000+len(p.Addrs))
	p.Addrs[containerID] = addr
	p.Spending[containerID] = existingSpending
	p.Budgets[containerID] = budget
	return addr, nil
}

func (p *MockProxy) UpdateBudget(containerID string, budget int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Budgets[containerID] = budget
}

func (p *MockProxy) GetSpending(containerID string) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Spending[containerID]
}

func (p *MockProxy) SetSpending(containerID string, milliCents int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Spending[containerID] = milliCents
}

// GetProxyAddr returns the proxy address for a container.
func (p *MockProxy) GetProxyAddr(containerID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Addrs[containerID]
}

// GetBudget returns the budget for a container.
func (p *MockProxy) GetBudget(containerID string) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Budgets[containerID]
}

// AddSpending adds milliCents to the spending for a container.
func (p *MockProxy) AddSpending(containerID string, milliCents int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Spending[containerID] += milliCents
}
