package main

// scanCadenceLabel is the human-readable schedule shown in the UI (sidebar + dashboard).
// The catch-up scan runs on a fixed daily 2 AM ET schedule (see ScanSchedule in
// template.yaml) — off-peak, so its flex-tier Bedrock traffic doesn't compete with
// real-time push traffic during business hours. This is intentionally not configurable:
// there is no EventBridge Scheduler rewrite path anymore.
const scanCadenceLabel = "Daily · 2 AM ET"
