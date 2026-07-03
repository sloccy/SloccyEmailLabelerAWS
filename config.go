package main

import (
	"os"
	"strconv"

	"github.com/sloccy/ollamail-aws/llm"
)

type Config struct {
	BedrockModel       string
	GmailMaxResults    int
	GmailLookbackHours int
	EmailBodyTrunc     int
	LogRetentionDays   int
	HistoryMaxLimit    int
	DebugLogging       bool
	Mode               string // "web", "scan", or "push"

	// Gmail push (users.watch → Pub/Sub → PushFunction webhook).
	PubSubTopic        string // projects/<proj>/topics/<name>; empty disables watch registration
	PushAudience       string // expected OIDC audience on the Pub/Sub push JWT
	PushServiceAccount string // expected service-account email in the push JWT

	// Cloudflare Access (Zero Trust) protection for the Web UI, fronted by CloudFront.
	CfAccessTeamDomain string // e.g. https://yourteam.cloudflareaccess.com; enables Access JWT verification
	CfAccessAud        string // Cloudflare Access application Audience (AUD) tag

	// ClassifyConcurrency caps how many emails are classified against Bedrock in parallel per
	// account. Flex-tier requests can queue for minutes, so classification no longer waits on
	// each prior call to finish; see processor.ProcessConfig.
	ClassifyConcurrency int
}

func loadConfig() Config {
	return Config{
		BedrockModel:       getEnv("BEDROCK_MODEL", llm.DefaultModel),
		GmailMaxResults:    getEnvInt("GMAIL_MAX_RESULTS", 50),
		GmailLookbackHours: getEnvInt("GMAIL_LOOKBACK_HOURS", 24),
		EmailBodyTrunc:     getEnvInt("EMAIL_BODY_TRUNCATION", 3000),
		LogRetentionDays:   getEnvInt("LOG_RETENTION_DAYS", 30),
		HistoryMaxLimit:    getEnvInt("HISTORY_MAX_LIMIT", 500),
		DebugLogging:       getEnv("DEBUG_LOGGING", "0") == "1",
		Mode:               getEnv("MODE", "web"),

		PubSubTopic:        getEnv("PUBSUB_TOPIC", ""),
		PushAudience:       getEnv("PUSH_OIDC_AUDIENCE", ""),
		PushServiceAccount: getEnv("PUSH_OIDC_SA_EMAIL", ""),

		CfAccessTeamDomain: getEnv("CF_ACCESS_TEAM_DOMAIN", ""),
		CfAccessAud:        getEnv("CF_ACCESS_AUD", ""),

		ClassifyConcurrency: getEnvInt("CLASSIFY_CONCURRENCY", 6),
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
