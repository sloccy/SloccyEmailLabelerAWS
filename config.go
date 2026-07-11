package main

import (
	"os"
	"strconv"

	"github.com/sloccy/ollamail-aws/llm"
	"github.com/sloccy/ollamail-aws/processor"
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
	// AuthMode mirrors the Function URL's AuthType decision made in template.yaml
	// ("cfaccess" when the URL is public NONE, "iam" when it's AWS_IAM). It lets runWeb
	// refuse to start when the URL is public but the CF Access vars have drifted away —
	// without it, missing CF vars silently disable the only auth gate.
	AuthMode string

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
		AuthMode:           getEnv("AUTH_MODE", ""),

		ClassifyConcurrency: getEnvInt("CLASSIFY_CONCURRENCY", 6),
	}
}

// processConfig builds the processor.ProcessConfig shared by the scheduled scan and the
// push webhook, which otherwise rebuild the same fields from Config independently.
// suppressEmptyLog is true for push (frequent, often a no-op notification) and false for
// scan (once daily, worth logging even when nothing new was found).
func (c *Config) processConfig(suppressEmptyLog bool) processor.ProcessConfig {
	return processor.ProcessConfig{
		LookbackHours:       c.GmailLookbackHours,
		MaxResults:          int64(c.GmailMaxResults),
		BodyTruncation:      c.EmailBodyTrunc,
		DebugLogging:        c.DebugLogging,
		ClassifyConcurrency: c.ClassifyConcurrency,
		SuppressEmptyLog:    suppressEmptyLog,
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
