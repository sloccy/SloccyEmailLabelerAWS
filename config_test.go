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
	// Clear all config env vars so we get pure defaults.
	keys := []string{
		"OLLAMA_HOST", "OLLAMA_MODEL", "OLLAMA_TIMEOUT", "OLLAMA_NUM_CTX",
		"GMAIL_MAX_RESULTS", "GMAIL_LOOKBACK_HOURS", "EMAIL_BODY_TRUNCATION",
		"LOG_RETENTION_DAYS", "POLL_INTERVAL", "MIN_POLL_INTERVAL",
		"HISTORY_MAX_LIMIT", "DEBUG_LOGGING", "CREDENTIALS_FILE", "DATA_DIR",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}

	cfg := loadConfig()

	if cfg.OllamaHost != "http://localhost:11434" {
		t.Errorf("OllamaHost = %q", cfg.OllamaHost)
	}
	if cfg.OllamaTimeout != 600 {
		t.Errorf("OllamaTimeout = %d", cfg.OllamaTimeout)
	}
	if cfg.OllamaNumCtx != 4096 {
		t.Errorf("OllamaNumCtx = %d", cfg.OllamaNumCtx)
	}
	if cfg.GmailMaxResults != 50 {
		t.Errorf("GmailMaxResults = %d", cfg.GmailMaxResults)
	}
	if cfg.PollInterval != 300 {
		t.Errorf("PollInterval = %d", cfg.PollInterval)
	}
	if cfg.MinPollInterval != 30 {
		t.Errorf("MinPollInterval = %d", cfg.MinPollInterval)
	}
	if cfg.DebugLogging {
		t.Error("DebugLogging should default to false")
	}
	if cfg.CredentialsFile != "/credentials/credentials.json" {
		t.Errorf("CredentialsFile = %q", cfg.CredentialsFile)
	}
	if cfg.DataDir != "/data" {
		t.Errorf("DataDir = %q", cfg.DataDir)
	}
}

func TestLoadConfig_EnvOverrides(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "http://ollama:11434")
	t.Setenv("OLLAMA_TIMEOUT", "120")
	t.Setenv("DEBUG_LOGGING", "1")
	t.Setenv("DATA_DIR", "/tmp/data")

	cfg := loadConfig()

	if cfg.OllamaHost != "http://ollama:11434" {
		t.Errorf("OllamaHost = %q", cfg.OllamaHost)
	}
	if cfg.OllamaTimeout != 120 {
		t.Errorf("OllamaTimeout = %d", cfg.OllamaTimeout)
	}
	if !cfg.DebugLogging {
		t.Error("DebugLogging should be true when DEBUG_LOGGING=1")
	}
	if cfg.DataDir != "/tmp/data" {
		t.Errorf("DataDir = %q", cfg.DataDir)
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
