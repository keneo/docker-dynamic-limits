package model

import "testing"

func TestAllLimitTypesCompleteness(t *testing.T) {
	known := map[LimitType]bool{
		LimitCPU:             true,
		LimitRAM:             true,
		LimitDisk:            true,
		LimitNetwork:         true,
		LimitDiskIOByte:      true,
		LimitDiskIOOps:       true,
		LimitSpending:        true,
		LimitRAMUsageBSec:    true,
		LimitDiskUsageBSec:   true,
		LimitRAMRequestBSec:  true,
		LimitDiskRequestBSec: true,
	}

	if len(AllLimitTypes) != len(known) {
		t.Fatalf("AllLimitTypes has %d entries, expected %d", len(AllLimitTypes), len(known))
	}

	for _, lt := range AllLimitTypes {
		if !known[lt] {
			t.Errorf("unexpected limit type in AllLimitTypes: %s", lt)
		}
	}
}

func TestLimitTypeStringValues(t *testing.T) {
	tests := []struct {
		lt   LimitType
		want string
	}{
		{LimitCPU, "cpu"},
		{LimitRAM, "ram"},
		{LimitDisk, "disk"},
		{LimitNetwork, "net"},
		{LimitDiskIOByte, "disk-io-bytes"},
		{LimitDiskIOOps, "disk-io-ops"},
		{LimitSpending, "spending"},
		{LimitRAMUsageBSec, "ram-usage-bsec"},
		{LimitDiskUsageBSec, "disk-usage-bsec"},
		{LimitRAMRequestBSec, "ram-request-bsec"},
		{LimitDiskRequestBSec, "disk-request-bsec"},
	}

	for _, tc := range tests {
		if string(tc.lt) != tc.want {
			t.Errorf("LimitType %v: got %q, want %q", tc.lt, string(tc.lt), tc.want)
		}
	}
}
