package cgroup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCPUUsageMicroseconds(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int64
		wantErr bool
	}{
		{
			name:    "valid",
			content: "usage_usec 123456789\nuser_usec 100000000\nsystem_usec 23456789\n",
			want:    123456789,
		},
		{
			name:    "with other fields",
			content: "nr_periods 0\nnr_throttled 0\nthrottled_usec 0\nusage_usec 42\n",
			want:    42,
		},
		{
			name:    "missing usage_usec",
			content: "user_usec 100\nsystem_usec 200\n",
			wantErr: true,
		},
		{
			name:    "empty",
			content: "",
			wantErr: true,
		},
		{
			name:    "zero value",
			content: "usage_usec 0\n",
			want:    0,
		},
		{
			name:    "large value",
			content: "usage_usec 999999999999\n",
			want:    999999999999,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCPUUsageMicroseconds(tc.content)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseIOStat(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantBytes int64
		wantOps   int64
	}{
		{
			name:      "single device",
			content:   "8:0 rbytes=1000 wbytes=2000 rios=10 wios=20\n",
			wantBytes: 3000,
			wantOps:   30,
		},
		{
			name:      "multiple devices",
			content:   "8:0 rbytes=100 wbytes=200 rios=1 wios=2\n8:16 rbytes=300 wbytes=400 rios=3 wios=4\n",
			wantBytes: 1000,
			wantOps:   10,
		},
		{
			name:      "empty",
			content:   "",
			wantBytes: 0,
			wantOps:   0,
		},
		{
			name:      "extra fields",
			content:   "8:0 rbytes=500 wbytes=500 rios=5 wios=5 dbytes=100 dios=1\n",
			wantBytes: 1000,
			wantOps:   10,
		},
		{
			name:      "zero values",
			content:   "8:0 rbytes=0 wbytes=0 rios=0 wios=0\n",
			wantBytes: 0,
			wantOps:   0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stats, err := ParseIOStat(tc.content)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if stats.TotalBytes != tc.wantBytes {
				t.Errorf("TotalBytes = %d, want %d", stats.TotalBytes, tc.wantBytes)
			}
			if stats.TotalOps != tc.wantOps {
				t.Errorf("TotalOps = %d, want %d", stats.TotalOps, tc.wantOps)
			}
		})
	}
}

func TestFindCgroupPath(t *testing.T) {
	tmp := t.TempDir()
	r := NewReader(tmp)
	dockerID := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	// No paths exist yet
	_, err := r.FindCgroupPath(dockerID)
	if err == nil {
		t.Fatal("expected error when no cgroup paths exist")
	}

	// Create systemd-style path
	systemdPath := filepath.Join(tmp, "system.slice", "docker-"+dockerID+".scope")
	os.MkdirAll(systemdPath, 0755)

	got, err := r.FindCgroupPath(dockerID)
	if err != nil {
		t.Fatalf("FindCgroupPath: %v", err)
	}
	if got != systemdPath {
		t.Errorf("got %q, want %q", got, systemdPath)
	}
}

func TestFindCgroupPathCgroupfs(t *testing.T) {
	tmp := t.TempDir()
	r := NewReader(tmp)
	dockerID := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	// Create cgroupfs-style path
	cgroupfsPath := filepath.Join(tmp, "docker", dockerID)
	os.MkdirAll(cgroupfsPath, 0755)

	got, err := r.FindCgroupPath(dockerID)
	if err != nil {
		t.Fatalf("FindCgroupPath: %v", err)
	}
	if got != cgroupfsPath {
		t.Errorf("got %q, want %q", got, cgroupfsPath)
	}
}

func TestCPUUsageMicroseconds(t *testing.T) {
	tmp := t.TempDir()
	r := NewReader(tmp)

	cgPath := filepath.Join(tmp, "testcg")
	os.MkdirAll(cgPath, 0755)
	os.WriteFile(filepath.Join(cgPath, "cpu.stat"), []byte("usage_usec 5000000\nuser_usec 3000000\nsystem_usec 2000000\n"), 0644)

	got, err := r.CPUUsageMicroseconds(cgPath)
	if err != nil {
		t.Fatalf("CPUUsageMicroseconds: %v", err)
	}
	if got != 5000000 {
		t.Errorf("got %d, want 5000000", got)
	}
}

func TestReadIOStat(t *testing.T) {
	tmp := t.TempDir()
	r := NewReader(tmp)

	cgPath := filepath.Join(tmp, "testcg")
	os.MkdirAll(cgPath, 0755)
	os.WriteFile(filepath.Join(cgPath, "io.stat"), []byte("8:0 rbytes=1024 wbytes=2048 rios=10 wios=20\n"), 0644)

	stats, err := r.ReadIOStat(cgPath)
	if err != nil {
		t.Fatalf("ReadIOStat: %v", err)
	}
	if stats.TotalBytes != 3072 {
		t.Errorf("TotalBytes = %d, want 3072", stats.TotalBytes)
	}
	if stats.TotalOps != 30 {
		t.Errorf("TotalOps = %d, want 30", stats.TotalOps)
	}
}

func TestNewReaderDefaultPath(t *testing.T) {
	r := NewReader("")
	if r.basePath != "/sys/fs/cgroup" {
		t.Errorf("basePath = %q, want /sys/fs/cgroup", r.basePath)
	}
}

func TestNewReaderCustomPath(t *testing.T) {
	r := NewReader("/custom/path")
	if r.basePath != "/custom/path" {
		t.Errorf("basePath = %q, want /custom/path", r.basePath)
	}
}
