package main

import (
	"os"
	"strconv"
)

type Config struct {
	OllamaHost         string
	OllamaModel        string
	OllamaTimeout      int // seconds
	OllamaNumCtx       int
	GmailMaxResults    int
	GmailLookbackHours int
	EmailBodyTrunc     int
	LogRetentionDays   int
	PollInterval       int // seconds
	MinPollInterval    int // seconds
	HistoryMaxLimit    int
	DebugLogging       bool
	CredentialsFile    string
	DataDir            string
}

func loadConfig() Config {
	return Config{
		OllamaHost:         getEnv("OLLAMA_HOST", "http://localhost:11434"),
		OllamaModel:        getEnv("OLLAMA_MODEL", "qwen3.5:4b-q4_K_M"),
		OllamaTimeout:      getEnvInt("OLLAMA_TIMEOUT", 600),
		OllamaNumCtx:       getEnvInt("OLLAMA_NUM_CTX", 4096),
		GmailMaxResults:    getEnvInt("GMAIL_MAX_RESULTS", 50),
		GmailLookbackHours: getEnvInt("GMAIL_LOOKBACK_HOURS", 24),
		EmailBodyTrunc:     getEnvInt("EMAIL_BODY_TRUNCATION", 3000),
		LogRetentionDays:   getEnvInt("LOG_RETENTION_DAYS", 30),
		PollInterval:       getEnvInt("POLL_INTERVAL", 300),
		MinPollInterval:    getEnvInt("MIN_POLL_INTERVAL", 30),
		HistoryMaxLimit:    getEnvInt("HISTORY_MAX_LIMIT", 500),
		DebugLogging:       getEnv("DEBUG_LOGGING", "0") == "1",
		CredentialsFile:    getEnv("CREDENTIALS_FILE", "/credentials/credentials.json"),
		DataDir:            getEnv("DATA_DIR", "/data"),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
