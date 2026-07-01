package main

import (
	"os"
	"strconv"
)

type Config struct {
	BedrockModel       string
	GmailMaxResults    int
	GmailLookbackHours int
	EmailBodyTrunc     int
	LogRetentionDays   int
	HistoryMaxLimit    int
	DebugLogging       bool
	CredentialsFile    string // path to credentials.json; CREDENTIALS_JSON env var takes precedence
	DataDir            string // unused in Lambda; retained for Open() compat
	Mode               string // "web" or "scan"
}

func loadConfig() Config {
	return Config{
		BedrockModel:       getEnv("BEDROCK_MODEL", "us.amazon.nova-micro-v1:0"),
		GmailMaxResults:    getEnvInt("GMAIL_MAX_RESULTS", 50),
		GmailLookbackHours: getEnvInt("GMAIL_LOOKBACK_HOURS", 24),
		EmailBodyTrunc:     getEnvInt("EMAIL_BODY_TRUNCATION", 3000),
		LogRetentionDays:   getEnvInt("LOG_RETENTION_DAYS", 30),
		HistoryMaxLimit:    getEnvInt("HISTORY_MAX_LIMIT", 500),
		DebugLogging:       getEnv("DEBUG_LOGGING", "0") == "1",
		CredentialsFile:    getEnv("CREDENTIALS_FILE", "/credentials/credentials.json"),
		DataDir:            getEnv("DATA_DIR", "/tmp"),
		Mode:               getEnv("MODE", "web"),
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
