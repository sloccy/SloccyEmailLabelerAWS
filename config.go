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
	PollInterval       int // informational only; EventBridge controls cadence
	MinPollInterval    int
	HistoryMaxLimit    int
	DebugLogging       bool
	CredentialsFile    string // path to credentials.json; CREDENTIALS_JSON env var takes precedence
	DataDir            string // unused in Lambda; retained for Open() compat
	Mode               string // "web" or "scan"
	CfAccessTeamDomain string // e.g. https://yourteam.cloudflareaccess.com; enables Access JWT verification
	CfAccessAud        string // Cloudflare Access application Audience (AUD) tag
}

func loadConfig() Config {
	return Config{
		BedrockModel:       getEnv("BEDROCK_MODEL", "us.amazon.nova-micro-v1:0"),
		GmailMaxResults:    getEnvInt("GMAIL_MAX_RESULTS", 50),
		GmailLookbackHours: getEnvInt("GMAIL_LOOKBACK_HOURS", 24),
		EmailBodyTrunc:     getEnvInt("EMAIL_BODY_TRUNCATION", 3000),
		LogRetentionDays:   getEnvInt("LOG_RETENTION_DAYS", 30),
		PollInterval:       getEnvInt("POLL_INTERVAL", 300),
		MinPollInterval:    getEnvInt("MIN_POLL_INTERVAL", 30),
		HistoryMaxLimit:    getEnvInt("HISTORY_MAX_LIMIT", 500),
		DebugLogging:       getEnv("DEBUG_LOGGING", "0") == "1",
		CredentialsFile:    getEnv("CREDENTIALS_FILE", "/credentials/credentials.json"),
		DataDir:            getEnv("DATA_DIR", "/tmp"),
		Mode:               getEnv("MODE", "web"),
		CfAccessTeamDomain: getEnv("CF_ACCESS_TEAM_DOMAIN", ""),
		CfAccessAud:        getEnv("CF_ACCESS_AUD", ""),
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
