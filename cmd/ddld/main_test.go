package main

import "testing"

func TestDirOf(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/var/lib/ddl/ddl.db", "/var/lib/ddl"},
		{"/tmp/test.db", "/tmp"},
		{"ddl.db", ""},
		{"/a/b/c/d", "/a/b/c"},
		{"/single", ""},
		{"", ""},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := dirOf(tc.input)
			if got != tc.want {
				t.Errorf("dirOf(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
