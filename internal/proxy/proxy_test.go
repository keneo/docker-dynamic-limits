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

func TestCalculateSpendingMicroCents(t *testing.T) {
	tests := []struct {
		name           string
		input          int64
		output         int64
		pricing        ModelPricing
		wantMicroCents int64
	}{
		{
			name:           "zero tokens",
			input:          0,
			output:         0,
			pricing:        ModelPricing{InputPerToken: 1000, OutputPerToken: 3000},
			wantMicroCents: 0,
		},
		{
			name:           "gpt-4 pricing exact",
			input:          1000,
			output:         1000,
			pricing:        ModelPricing{InputPerToken: 3000, OutputPerToken: 6000},
			wantMicroCents: 1000*3000 + 1000*6000,
		},
		{
			name:           "sub-cent cost preserved",
			input:          1,
			output:         0,
			pricing:        ModelPricing{InputPerToken: 100, OutputPerToken: 100},
			wantMicroCents: 100,
		},
		{
			name:           "large token count",
			input:          100000,
			output:         50000,
			pricing:        ModelPricing{InputPerToken: 1000, OutputPerToken: 3000},
			wantMicroCents: 100000*1000 + 50000*3000,
		},
		{
			name:           "output only",
			input:          0,
			output:         10000,
			pricing:        ModelPricing{InputPerToken: 1000, OutputPerToken: 3000},
			wantMicroCents: 10000 * 3000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CalculateSpendingMicroCents(tc.input, tc.output, tc.pricing)
			if got != tc.wantMicroCents {
				t.Errorf("got %d, want %d", got, tc.wantMicroCents)
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

	st.budgets["c1"] = 100_000 // 100 milli-cents in micro-cents
	st.UpdateBudget("c1", 200) // 200 milli-cents

	if st.budgets["c1"] != 200_000 {
		t.Errorf("budget = %d, want 200_000 (200 milli-cents in micro-cents)", st.budgets["c1"])
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

	// Simulate an API response body (gpt-4: 10000*3000 + 5000*6000 = 60M micro-cents = 60 cents)
	body := []byte(`{
		"model": "gpt-4",
		"usage": {
			"prompt_tokens": 10000,
			"completion_tokens": 5000
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
		t.Errorf("callback total = %d cents, want > 0", callbackTotal)
	}
	if st.GetSpending("c1") != callbackTotal {
		t.Errorf("GetSpending(c1) = %d, callback total = %d, should match", st.GetSpending("c1"), callbackTotal)
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

	// claude-3-opus: 1000*1500 + 500*7500 = 5,250,000 micro-cents
	body := []byte(`{
		"model": "claude-3-opus-20240229",
		"usage": {
			"input_tokens": 1000,
			"output_tokens": 500
		}
	}`)

	st.trackSpending("c1", "api.anthropic.com", body)

	expected := int64(1000*1500 + 500*7500) // 5,250,000 micro-cents
	if st.spending["c1"] != expected {
		t.Errorf("spending = %d micro-cents, want %d", st.spending["c1"], expected)
	}
}

func TestSpendingTrackerCumulativeSpending(t *testing.T) {
	st := NewSpendingTracker(nil)
	st.spending["c1"] = 50_000_000 // 50 cents in micro-cents

	// gpt-4: 10000*3000 + 5000*6000 = 60,000,000 micro-cents
	body := []byte(`{
		"model": "gpt-4",
		"usage": {
			"prompt_tokens": 10000,
			"completion_tokens": 5000
		}
	}`)

	st.trackSpending("c1", "api.openai.com", body)

	if st.spending["c1"] <= 50_000_000 {
		t.Errorf("spending should accumulate, got %d micro-cents", st.spending["c1"])
	}
}
