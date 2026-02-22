package cgroup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CgroupReader defines the interface for cgroup operations.
type CgroupReader interface {
	FindCgroupPath(dockerID string) (string, error)
	CPUUsageMicroseconds(cgroupPath string) (int64, error)
	MemoryCurrent(cgroupPath string) (int64, error)
	SetMemoryMax(cgroupPath string, bytes int64) error
	ReadIOStat(cgroupPath string) (*IOStats, error)
	SetIOMax(cgroupPath string, deviceNum string, bps int64, iops int64) error
	RemoveIOMax(cgroupPath string, deviceNum string) error
	ReadNetworkStats(vethName string) (*NetworkStats, error)
}

// Reader reads cgroup v2 stats for a container.
type Reader struct {
	basePath string // e.g., /sys/fs/cgroup
}

// NewReader creates a cgroup reader with the given base path.
func NewReader(basePath string) *Reader {
	if basePath == "" {
		basePath = "/sys/fs/cgroup"
	}
	return &Reader{basePath: basePath}
}

// FindCgroupPath finds the cgroup path for a Docker container by its full ID.
func (r *Reader) FindCgroupPath(dockerID string) (string, error) {
	// Docker typically places containers at:
	// /sys/fs/cgroup/system.slice/docker-<id>.scope (systemd driver)
	// /sys/fs/cgroup/docker/<id> (cgroupfs driver)
	candidates := []string{
		filepath.Join(r.basePath, "system.slice", "docker-"+dockerID+".scope"),
		filepath.Join(r.basePath, "docker", dockerID),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("cgroup path not found for container %s", dockerID[:12])
}

// ParseCPUUsageMicroseconds parses the usage_usec value from cpu.stat content.
func ParseCPUUsageMicroseconds(content string) (int64, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "usage_usec ") {
			parts := strings.Fields(line)
			if len(parts) == 2 {
				return strconv.ParseInt(parts[1], 10, 64)
			}
		}
	}
	return 0, fmt.Errorf("usage_usec not found in cpu.stat")
}

// CPUUsageMicroseconds reads cumulative CPU usage in microseconds from cpu.stat.
func (r *Reader) CPUUsageMicroseconds(cgroupPath string) (int64, error) {
	data, err := os.ReadFile(filepath.Join(cgroupPath, "cpu.stat"))
	if err != nil {
		return 0, fmt.Errorf("read cpu.stat: %w", err)
	}
	return ParseCPUUsageMicroseconds(string(data))
}

// MemoryCurrent reads the current memory usage in bytes.
func (r *Reader) MemoryCurrent(cgroupPath string) (int64, error) {
	return r.readSingleInt(filepath.Join(cgroupPath, "memory.current"))
}

// SetMemoryMax writes the memory limit in bytes.
func (r *Reader) SetMemoryMax(cgroupPath string, bytes int64) error {
	val := "max"
	if bytes > 0 {
		val = strconv.FormatInt(bytes, 10)
	}
	return os.WriteFile(filepath.Join(cgroupPath, "memory.max"), []byte(val), 0644)
}

// IOStat reads cumulative IO statistics from io.stat.
// Returns total (read+write) bytes and operations.
type IOStats struct {
	TotalBytes int64
	TotalOps   int64
}

// ParseIOStat parses io.stat content and returns aggregated IO statistics.
func ParseIOStat(content string) (*IOStats, error) {
	stats := &IOStats{}
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		// Each line: "major:minor rbytes=X wbytes=X rios=X wios=X ..."
		for _, f := range fields {
			kv := strings.SplitN(f, "=", 2)
			if len(kv) != 2 {
				continue
			}
			val, err := strconv.ParseInt(kv[1], 10, 64)
			if err != nil {
				continue
			}
			switch kv[0] {
			case "rbytes", "wbytes":
				stats.TotalBytes += val
			case "rios", "wios":
				stats.TotalOps += val
			}
		}
	}
	return stats, nil
}

func (r *Reader) ReadIOStat(cgroupPath string) (*IOStats, error) {
	data, err := os.ReadFile(filepath.Join(cgroupPath, "io.stat"))
	if err != nil {
		return nil, fmt.Errorf("read io.stat: %w", err)
	}
	return ParseIOStat(string(data))
}

// SetIOMax throttles IO to near-zero or restores it.
// deviceNum is like "8:0" (major:minor of the block device).
func (r *Reader) SetIOMax(cgroupPath string, deviceNum string, bps int64, iops int64) error {
	val := fmt.Sprintf("%s rbps=%d wbps=%d riops=%d wiops=%d",
		deviceNum, bps, bps, iops, iops)
	return os.WriteFile(filepath.Join(cgroupPath, "io.max"), []byte(val), 0644)
}

// RemoveIOMax removes IO throttling for a device.
func (r *Reader) RemoveIOMax(cgroupPath string, deviceNum string) error {
	val := fmt.Sprintf("%s rbps=max wbps=max riops=max wiops=max", deviceNum)
	return os.WriteFile(filepath.Join(cgroupPath, "io.max"), []byte(val), 0644)
}

// NetworkStats reads network bytes from /sys/class/net/<iface>/statistics.
type NetworkStats struct {
	RxBytes int64
	TxBytes int64
}

func (r *Reader) ReadNetworkStats(vethName string) (*NetworkStats, error) {
	base := filepath.Join("/sys/class/net", vethName, "statistics")
	rx, err := r.readSingleInt(filepath.Join(base, "rx_bytes"))
	if err != nil {
		return nil, fmt.Errorf("read rx_bytes: %w", err)
	}
	tx, err := r.readSingleInt(filepath.Join(base, "tx_bytes"))
	if err != nil {
		return nil, fmt.Errorf("read tx_bytes: %w", err)
	}
	return &NetworkStats{RxBytes: rx, TxBytes: tx}, nil
}

func (r *Reader) readSingleInt(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}
