package main

import (
	"testing"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{"3600s", 3600, false},
		{"60m", 3600, false},
		{"1h", 3600, false},
		{"30s", 30, false},
		{"100", 100, false},
		{"0s", 0, false},
		{"", 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseDuration(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("parseDuration(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestParseBytes(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{"1024", 1024, false},
		{"1k", 1024, false},
		{"1K", 1024, false},
		{"1m", 1024 * 1024, false},
		{"1g", 1024 * 1024 * 1024, false},
		{"1t", 1024 * 1024 * 1024 * 1024, false},
		{"512m", 512 * 1024 * 1024, false},
		{"1.5g", int64(1.5 * 1024 * 1024 * 1024), false},
		{"", 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseBytes(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("parseBytes(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestParseMilliCents(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{"10.00", 1_000_000, false},
		{"10", 1_000_000, false},
		{"0.01", 1_000, false},
		{"0.50", 50_000, false},
		{"100.99", 10_099_000, false},
		{"abc", 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseMilliCents(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("parseMilliCents(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestParseValue(t *testing.T) {
	tests := []struct {
		limitType string
		input     string
		want      int64
		wantErr   bool
	}{
		{"cpu", "1h", 3600, false},
		{"cpu", "60m", 3600, false},
		{"ram", "512m", 512 * 1024 * 1024, false},
		{"disk", "1g", 1024 * 1024 * 1024, false},
		{"net", "100k", 100 * 1024, false},
		{"disk-io-bytes", "1g", 1024 * 1024 * 1024, false},
		{"disk-io-ops", "1000000", 1000000, false},
		{"spending", "10.00", 1_000_000, false},
		{"unknown", "42", 42, false},
	}

	for _, tc := range tests {
		t.Run(tc.limitType+"_"+tc.input, func(t *testing.T) {
			got, err := parseValue(tc.limitType, tc.input)
			if tc.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("parseValue(%q, %q) = %d, want %d", tc.limitType, tc.input, got, tc.want)
			}
		})
	}
}

func TestFormatValue(t *testing.T) {
	tests := []struct {
		limitType string
		value     int64
		want      string
	}{
		{"cpu", 0, "-"},
		{"cpu", 30, "30s"},
		{"cpu", 90, "1m30s"},
		{"cpu", 3661, "1h1m1s"},
		{"ram", 0, "-"},
		{"ram", 500, "500B"},
		{"ram", 1024, "1.0K"},
		{"ram", 1048576, "1.0M"},
		{"ram", 1073741824, "1.0G"},
		{"ram", 1099511627776, "1.0T"},
		{"spending", 0, "-"},
		{"spending", 100_000, "$1.00"},
		{"spending", 1_050_000, "$10.50"},
		{"disk-io-ops", 0, "-"},
		{"disk-io-ops", 42, "42"},
	}

	for _, tc := range tests {
		t.Run(tc.limitType+"_"+tc.want, func(t *testing.T) {
			got := formatValue(tc.limitType, tc.value)
			if got != tc.want {
				t.Errorf("formatValue(%q, %d) = %q, want %q", tc.limitType, tc.value, got, tc.want)
			}
		})
	}
}

func TestFormatBytesHuman(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0B"},
		{100, "100B"},
		{1024, "1.0K"},
		{1536, "1.5K"},
		{1048576, "1.0M"},
		{1073741824, "1.0G"},
		{1099511627776, "1.0T"},
	}

	for _, tc := range tests {
		got := formatBytesHuman(tc.input)
		if got != tc.want {
			t.Errorf("formatBytesHuman(%d) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestGetJSONFloat(t *testing.T) {
	tests := []struct {
		name string
		m    map[string]interface{}
		key  string
		want float64
	}{
		{"nil map", nil, "key", 0},
		{"missing key", map[string]interface{}{"a": 1.0}, "b", 0},
		{"float64 value", map[string]interface{}{"a": 42.5}, "a", 42.5},
		{"zero", map[string]interface{}{"a": 0.0}, "a", 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := getJSONFloat(tc.m, tc.key)
			if got != tc.want {
				t.Errorf("got %f, want %f", got, tc.want)
			}
		})
	}
}
