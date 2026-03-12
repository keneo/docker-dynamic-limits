package main

import (
	"testing"
	"time"
)

func TestParseConfigValue(t *testing.T) {
	tests := []struct {
		name    string
		info    configKeyInfo
		input   string
		want    interface{}
		wantErr bool
	}{
		{"bool true", configKeyInfo{keyType: "bool"}, "true", true, false},
		{"bool false", configKeyInfo{keyType: "bool"}, "false", false, false},
		{"bool yes", configKeyInfo{keyType: "bool"}, "yes", true, false},
		{"bool no", configKeyInfo{keyType: "bool"}, "no", false, false},
		{"bool 1", configKeyInfo{keyType: "bool"}, "1", true, false},
		{"bool 0", configKeyInfo{keyType: "bool"}, "0", false, false},
		{"bool invalid", configKeyInfo{keyType: "bool"}, "maybe", nil, true},
		{"string", configKeyInfo{keyType: "string"}, "sk-ant-xxx", "sk-ant-xxx", false},
		{"int", configKeyInfo{keyType: "int"}, "100", 100, false},
		{"int invalid", configKeyInfo{keyType: "int"}, "abc", nil, true},
		{"duration", configKeyInfo{keyType: "duration"}, "2m", (2 * time.Minute).String(), false},
		{"duration seconds", configKeyInfo{keyType: "duration"}, "120s", (120 * time.Second).String(), false},
		{"duration invalid", configKeyInfo{keyType: "duration"}, "abc", nil, true},
		{"stringlist", configKeyInfo{keyType: "stringlist"}, "llama3.2:3b,qwen3:8b", []string{"llama3.2:3b", "qwen3:8b"}, false},
		{"stringlist single", configKeyInfo{keyType: "stringlist"}, "llama3.2:3b", []string{"llama3.2:3b"}, false},
		{"stringlist with spaces", configKeyInfo{keyType: "stringlist"}, "model1 , model2 , model3", []string{"model1", "model2", "model3"}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseConfigValue(tc.info, tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Compare based on type
			switch want := tc.want.(type) {
			case []string:
				gotSlice, ok := got.([]string)
				if !ok {
					t.Fatalf("got type %T, want []string", got)
				}
				if len(gotSlice) != len(want) {
					t.Fatalf("got %v, want %v", gotSlice, want)
				}
				for i := range want {
					if gotSlice[i] != want[i] {
						t.Errorf("index %d: got %q, want %q", i, gotSlice[i], want[i])
					}
				}
			default:
				if got != tc.want {
					t.Errorf("got %v (%T), want %v (%T)", got, got, tc.want, tc.want)
				}
			}
		})
	}
}

func TestFormatConfigValue(t *testing.T) {
	tests := []struct {
		name string
		key  string
		info configKeyInfo
		resp map[string]interface{}
		want string
	}{
		{
			"anthropic key set",
			"anthropic-key",
			configKeyInfo{jsonKey: "anthropic_key"},
			map[string]interface{}{"anthropic_key_set": true},
			"****",
		},
		{
			"anthropic key not set",
			"anthropic-key",
			configKeyInfo{jsonKey: "anthropic_key"},
			map[string]interface{}{"anthropic_key_set": false},
			"(not set)",
		},
		{
			"openai key not set - missing field",
			"openai-key",
			configKeyInfo{jsonKey: "openai_key"},
			map[string]interface{}{},
			"(not set)",
		},
		{
			"bool true",
			"anthropic-enabled",
			configKeyInfo{jsonKey: "anthropic_enabled"},
			map[string]interface{}{"anthropic_enabled": true},
			"true",
		},
		{
			"bool false",
			"openai-enabled",
			configKeyInfo{jsonKey: "openai_enabled"},
			map[string]interface{}{"openai_enabled": false},
			"false",
		},
		{
			"string value",
			"ollama-url",
			configKeyInfo{jsonKey: "ollama_url"},
			map[string]interface{}{"ollama_url": "http://192.168.1.100:11434"},
			"http://192.168.1.100:11434",
		},
		{
			"number",
			"ollama-queue-size",
			configKeyInfo{jsonKey: "ollama_queue_size"},
			map[string]interface{}{"ollama_queue_size": float64(50)},
			"50",
		},
		{
			"string list",
			"ollama-models",
			configKeyInfo{jsonKey: "ollama_models"},
			map[string]interface{}{"ollama_models": []interface{}{"llama3.2:3b", "qwen3:8b"}},
			"llama3.2:3b, qwen3:8b",
		},
		{
			"empty list",
			"ollama-models",
			configKeyInfo{jsonKey: "ollama_models"},
			map[string]interface{}{"ollama_models": []interface{}{}},
			"(none)",
		},
		{
			"nil value",
			"ollama-url",
			configKeyInfo{jsonKey: "ollama_url"},
			map[string]interface{}{"ollama_url": nil},
			"(not configured)",
		},
		{
			"missing key",
			"ollama-url",
			configKeyInfo{jsonKey: "ollama_url"},
			map[string]interface{}{},
			"(not configured)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatConfigValue(tc.key, tc.info, tc.resp)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConfigKeyMapping(t *testing.T) {
	// Verify all display order keys exist in the config keys map
	for _, key := range configDisplayOrder {
		if _, ok := configKeys[key]; !ok {
			t.Errorf("display order key %q not found in configKeys map", key)
		}
	}
}
