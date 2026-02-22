package proxy

import (
	"testing"
)

func TestIsTrackedAPIHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"api.openai.com", true},
		{"api.openai.com:443", true},
		{"api.anthropic.com", true},
		{"api.anthropic.com:443", true},
		{"example.com", false},
		{"openai.com", false},
		{"", false},
		{"api.google.com", false},
	}

	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			got := IsTrackedAPIHost(tc.host)
			if got != tc.want {
				t.Errorf("IsTrackedAPIHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

func TestCalculateSpendingCents(t *testing.T) {
	tests := []struct {
		name         string
		input        int64
		output       int64
		pricing      ModelPricing
		wantCents    int64
	}{
		{
			name:      "zero tokens",
			input:     0,
			output:    0,
			pricing:   ModelPricing{InputPerToken: 1000, OutputPerToken: 3000},
			wantCents: 0,
		},
		{
			name:      "gpt-4 pricing exact",
			input:     1000,
			output:    1000,
			pricing:   ModelPricing{InputPerToken: 3000, OutputPerToken: 6000},
			wantCents: (1000*3000 + 1000*6000) / 1_000_000,
		},
		{
			name:      "minimum 1 cent",
			input:     1,
			output:    0,
			pricing:   ModelPricing{InputPerToken: 100, OutputPerToken: 100},
			wantCents: 1,
		},
		{
			name:      "large token count",
			input:     100000,
			output:    50000,
			pricing:   ModelPricing{InputPerToken: 1000, OutputPerToken: 3000},
			wantCents: (100000*1000 + 50000*3000) / 1_000_000,
		},
		{
			name:      "output only",
			input:     0,
			output:    10000,
			pricing:   ModelPricing{InputPerToken: 1000, OutputPerToken: 3000},
			wantCents: (10000 * 3000) / 1_000_000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CalculateSpendingCents(tc.input, tc.output, tc.pricing)
			if got != tc.wantCents {
				t.Errorf("got %d, want %d", got, tc.wantCents)
			}
		})
	}
}

func TestSpendingTrackerGetSetSpending(t *testing.T) {
	st := NewSpendingTracker(nil)

	st.SetSpending("c1", 100)
	got := st.GetSpending("c1")
	if got != 100 {
		t.Errorf("got %d, want 100", got)
	}

	// Non-existent container returns 0
	got = st.GetSpending("nonexistent")
	if got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestSpendingTrackerUpdateBudget(t *testing.T) {
	st := NewSpendingTracker(nil)

	st.budgets["c1"] = 100
	st.UpdateBudget("c1", 200)

	if st.budgets["c1"] != 200 {
		t.Errorf("budget = %d, want 200", st.budgets["c1"])
	}
}

func TestSpendingTrackerTrackSpending(t *testing.T) {
	var callbackCalled bool
	var callbackID string
	var callbackTotal int64

	st := NewSpendingTracker(func(containerID string, totalCents int64) {
		callbackCalled = true
		callbackID = containerID
		callbackTotal = totalCents
	})

	st.spending["c1"] = 0

	// Simulate an API response body
	body := []byte(`{
		"model": "gpt-4o",
		"usage": {
			"prompt_tokens": 1000,
			"completion_tokens": 500
		}
	}`)

	st.trackSpending("c1", "api.openai.com", body)

	if !callbackCalled {
		t.Fatal("onSpendingUpdate callback was not called")
	}
	if callbackID != "c1" {
		t.Errorf("callback containerID = %q, want %q", callbackID, "c1")
	}
	if callbackTotal <= 0 {
		t.Errorf("callback total = %d, want > 0", callbackTotal)
	}
	if st.spending["c1"] != callbackTotal {
		t.Errorf("spending[c1] = %d, want %d", st.spending["c1"], callbackTotal)
	}
}

func TestSpendingTrackerTrackSpendingInvalidBody(t *testing.T) {
	st := NewSpendingTracker(nil)
	st.spending["c1"] = 0

	// Invalid JSON should not crash
	st.trackSpending("c1", "api.openai.com", []byte("not json"))

	if st.spending["c1"] != 0 {
		t.Errorf("spending should remain 0 for invalid body, got %d", st.spending["c1"])
	}
}

func TestSpendingTrackerTrackSpendingNoTokens(t *testing.T) {
	st := NewSpendingTracker(nil)
	st.spending["c1"] = 0

	body := []byte(`{"model": "gpt-4o", "usage": {"prompt_tokens": 0, "completion_tokens": 0}}`)
	st.trackSpending("c1", "api.openai.com", body)

	if st.spending["c1"] != 0 {
		t.Errorf("spending should remain 0 for zero tokens, got %d", st.spending["c1"])
	}
}

func TestNormalizeModelName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"gpt-4o-2024-01-01", "gpt-4o"},
		{"gpt-4o-mini-2024-01-01", "gpt-4o-mini"},
		{"gpt-4-turbo-2024-04-09", "gpt-4-turbo"},
		{"gpt-4-0613", "gpt-4"},
		{"gpt-3.5-turbo-0125", "gpt-3.5-turbo"},
		{"claude-3-opus-20240229", "claude-3-opus"},
		{"claude-3-sonnet-20240229", "claude-3-sonnet"},
		{"claude-3-haiku-20240307", "claude-3-haiku"},
		{"unknown-model", "unknown-model"},
		{"GPT-4O", "gpt-4o"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := normalizeModelName(tc.input)
			if got != tc.want {
				t.Errorf("normalizeModelName(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestSpendingTrackerAnthropicTokens(t *testing.T) {
	st := NewSpendingTracker(nil)
	st.spending["c1"] = 0

	body := []byte(`{
		"model": "claude-3-opus-20240229",
		"usage": {
			"input_tokens": 1000,
			"output_tokens": 500
		}
	}`)

	st.trackSpending("c1", "api.anthropic.com", body)

	if st.spending["c1"] <= 0 {
		t.Errorf("spending should be > 0 for Anthropic tokens, got %d", st.spending["c1"])
	}
}

func TestSpendingTrackerCumulativeSpending(t *testing.T) {
	st := NewSpendingTracker(nil)
	st.spending["c1"] = 50

	body := []byte(`{
		"model": "gpt-4",
		"usage": {
			"prompt_tokens": 10000,
			"completion_tokens": 5000
		}
	}`)

	st.trackSpending("c1", "api.openai.com", body)

	if st.spending["c1"] <= 50 {
		t.Errorf("spending should accumulate, got %d", st.spending["c1"])
	}
}
