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

	// Scan schedule (web mode rewrites the EventBridge Scheduler schedule at runtime).
	ScanIntervalMinutes int    // baseline/default cadence when no setting is stored
	ScanScheduleName    string // AWS::Scheduler::Schedule name to update (empty = disabled, e.g. local dev)
	ScanFunctionArn     string // ScanFunction ARN — re-supplied on every UpdateSchedule (full replace)
	SchedulerRoleArn    string // role EventBridge Scheduler assumes to invoke the target
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

		ScanIntervalMinutes: getEnvInt("SCAN_INTERVAL_MINUTES", 1),
		ScanScheduleName:    getEnv("SCAN_SCHEDULE_NAME", ""),
		ScanFunctionArn:     getEnv("SCAN_FUNCTION_ARN", ""),
		SchedulerRoleArn:    getEnv("SCHEDULER_ROLE_ARN", ""),
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
