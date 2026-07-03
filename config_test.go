package main

import (
	"testing"
)

func TestGetEnv(t *testing.T) {
	tests := []struct {
		name string
		key  string
		def  string
		set  string // empty means unset
		want string
	}{
		{name: "returns default when unset", key: "TEST_GETENV_UNSET", def: "default", set: "", want: "default"},
		{name: "returns value when set", key: "TEST_GETENV_SET", def: "default", set: "hello", want: "hello"},
		{name: "empty string falls back to default", key: "TEST_GETENV_EMPTY", def: "fallback", set: "", want: "fallback"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set != "" {
				t.Setenv(tc.key, tc.set)
			}
			got := getEnv(tc.key, tc.def)
			if got != tc.want {
				t.Errorf("getEnv(%q, %q) = %q, want %q", tc.key, tc.def, got, tc.want)
			}
		})
	}
}

func TestGetEnvInt(t *testing.T) {
	tests := []struct {
		name string
		key  string
		def  int
		set  string
		want int
	}{
		{name: "returns default when unset", key: "TEST_ENVINT_UNSET", def: 42, set: "", want: 42},
		{name: "parses valid integer", key: "TEST_ENVINT_VALID", def: 0, set: "99", want: 99},
		{name: "returns default on non-integer", key: "TEST_ENVINT_BAD", def: 7, set: "notanumber", want: 7},
		{name: "parses zero", key: "TEST_ENVINT_ZERO", def: 5, set: "0", want: 0},
		{name: "parses negative", key: "TEST_ENVINT_NEG", def: 0, set: "-10", want: -10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set != "" {
				t.Setenv(tc.key, tc.set)
			}
			got := getEnvInt(tc.key, tc.def)
			if got != tc.want {
				t.Errorf("getEnvInt(%q, %d) = %d, want %d", tc.key, tc.def, got, tc.want)
			}
		})
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	keys := []string{
		"BEDROCK_MODEL", "GMAIL_MAX_RESULTS", "GMAIL_LOOKBACK_HOURS", "EMAIL_BODY_TRUNCATION",
		"LOG_RETENTION_DAYS",
		"HISTORY_MAX_LIMIT", "DEBUG_LOGGING", "MODE",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}

	cfg := loadConfig()

	if cfg.BedrockModel != "us.amazon.nova-micro-v1:0" {
		t.Errorf("BedrockModel = %q", cfg.BedrockModel)
	}
	if cfg.GmailMaxResults != 50 {
		t.Errorf("GmailMaxResults = %d", cfg.GmailMaxResults)
	}
	if cfg.DebugLogging {
		t.Error("DebugLogging should default to false")
	}
	if cfg.Mode != "web" {
		t.Errorf("Mode = %q", cfg.Mode)
	}
}

func TestLoadConfig_EnvOverrides(t *testing.T) {
	t.Setenv("BEDROCK_MODEL", "us.amazon.nova-lite-v1:0")
	t.Setenv("DEBUG_LOGGING", "1")
	t.Setenv("MODE", "scan")

	cfg := loadConfig()

	if cfg.BedrockModel != "us.amazon.nova-lite-v1:0" {
		t.Errorf("BedrockModel = %q", cfg.BedrockModel)
	}
	if !cfg.DebugLogging {
		t.Error("DebugLogging should be true when DEBUG_LOGGING=1")
	}
	if cfg.Mode != "scan" {
		t.Errorf("Mode = %q", cfg.Mode)
	}
}

func TestLoadConfig_DebugLoggingValues(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"1", true},
		{"0", false},
		{"true", false}, // only "1" is truthy
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("DEBUG_LOGGING", tc.val)
			cfg := loadConfig()
			if cfg.DebugLogging != tc.want {
				t.Errorf("DEBUG_LOGGING=%q → DebugLogging = %v, want %v", tc.val, cfg.DebugLogging, tc.want)
			}
		})
	}
}
