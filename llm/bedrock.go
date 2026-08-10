package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"

	gmailpkg "github.com/sloccy/ollamail-aws/gmail"
)

// bedrockHTTPTimeout is the HTTP client read-timeout for Bedrock calls. Flex-tier requests
// are queued at lower priority and can take minutes to return (vs. the near-instant default
// tier), so this needs to be generous — set just under the Lambda's own hard timeout (900s)
// so a killed request surfaces as a clean Lambda timeout rather than a silent hang. Everything
// after the Converse call returns (parsing, logging, DynamoDB writes) runs in well under a
// second, so only a 30s margin is reserved for it rather than a full minute.
const bedrockHTTPTimeout = 14*time.Minute + 30*time.Second

// improveCallTimeout bounds a single ImprovePromptInstructions call. Unlike classify
// (many short calls, fine to let bedrockHTTPTimeout be the only cap) or a queued flex-tier
// request (genuinely needs minutes), one improve call is a single short rule rewrite —
// this is the real latency cap for it, well under bedrockHTTPTimeout, so a stuck call
// fails fast enough for the improve worker's own deadline margin (see improveWorkerMargin,
// improve.go) to still have room to write a terminal status.
const improveCallTimeout = 120 * time.Second

// replayCallTimeout bounds a single classify call inside ReplayAgainstExamples. Replay
// fans out one call per example (up to ~30) concurrently; without a per-call cap, one
// stuck call would hold up the whole batch until bedrockHTTPTimeout, not just itself.
const replayCallTimeout = 90 * time.Second

// LogLevelTimeout is the db.Log level written when a Converse call is aborted by
// bedrockHTTPTimeout, so the dashboard can count these over a rolling window.
const LogLevelTimeout = "TIMEOUT"

// isBedrockTimeout reports whether err stems from the client-side bedrockHTTPTimeout
// (a queued flex-tier request that never returned) rather than some other failure
// (bad request, throttling, etc).
func isBedrockTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var te interface{ Timeout() bool }
	if errors.As(err, &te) {
		return te.Timeout()
	}
	return false
}

// Settings is the minimal interface the Client needs to look up per-call model
// overrides. *db.Store and *db.FakeStore both satisfy this.
type Settings interface {
	GetSetting(ctx context.Context, key string) (string, error)
}

// Setting keys for the two independent model selections.
const (
	SettingClassifyModel = "classify_model"
	SettingImproveModel  = "improve_model"
	// SettingClassifyTier holds the Bedrock service tier ("standard" or "flex") used for
	// classification requests. Flex trades latency for lower cost; see resolveClassifyTier.
	SettingClassifyTier = "classify_tier"
	// SettingImproveTier is the same standard/flex choice for the prompt-improver model
	// (Generate instruction + Improve rule); see resolveImproveTier.
	SettingImproveTier = "improve_tier"
	// SettingClassifyReasoningDirective holds an optional override for the
	// reasoning-suppression system-prompt switch reasoningOff would otherwise pick from
	// reasoningRegistry (reasoning.go) based on the classify model's id. Only needed for
	// a model family the registry doesn't recognize yet; empty (the default) means "use
	// the registry, or no-op if the model isn't in it."
	SettingClassifyReasoningDirective = "classify_reasoning_directive"
	// SettingImproveReplay toggles replaying a candidate rule against its example corpus
	// on the classify model before showing a suggestion (see ReplayAgainstExamples).
	// "1" (the default when unset) enables it; any other value disables it. Read directly
	// via db.Store.GetSetting from recategorize.go/server.go, not through this package's
	// resolveSetting — replay is orchestrated by the caller (which example set to use, when
	// to run it), not by Client itself, so there's no per-call resolution to centralize here.
	SettingImproveReplay = "improve_replay"
	// SettingImproveReasoningEffort selects how hard the improve model is allowed to
	// think before answering, for model families that expose a real effort ladder rather
	// than the blunt on/off reasoningOff (reasoning.go) suppresses. Resolved via
	// resolveSetting, defaulting to ReasoningEffortOff; see reasoningEffortFields for the
	// per-family field mapping.
	SettingImproveReasoningEffort = "improve_reasoning_effort"
	// SettingImproveMaxRounds caps how many improve->replay rounds one suggestion may run
	// (improveRunner's improve loop, improve.go) before finalizing whichever round scored
	// best. Read directly via db.Store.GetSetting from improve.go, the same pattern as
	// SettingImproveReplay — the loop is orchestrated by the caller (which examples to
	// replay against, when to stop), not by this Client, so there's no per-call resolution
	// to centralize here. Meaningless with replay disabled: with no score to iterate on,
	// the loop always runs exactly one round regardless of this value.
	SettingImproveMaxRounds = "improve_max_rounds"
	// SettingImproveExampleCap caps how many examples per verdict selectExamplesForImprove
	// (improve.go) curates for the improve prompt itself — small and token-bounded, unlike
	// SettingReplayExampleCap below. Read directly via db.Store.GetSetting, same
	// caller-orchestrated pattern as SettingImproveMaxRounds.
	SettingImproveExampleCap = "improve_example_cap"
	// SettingReplayExampleCap caps how many examples per verdict ReplayAgainstExamples
	// (called from improve.go's improveAndFinalizeSuggestion) scores a candidate rule
	// against — deliberately independent of and larger than SettingImproveExampleCap,
	// since replay doesn't cost prompt tokens the way the improve call does, just one
	// classify call per example. A bigger, more representative sample here means a much
	// less noisy pass/fail score, at the cost of more classify calls during replay.
	// Meaningless with replay disabled (SettingImproveReplay).
	SettingReplayExampleCap = "replay_example_cap"
)

// ImproveMaxRoundsDefault/ImproveMaxRoundsCap bound SettingImproveMaxRounds: unset (or
// unparsable) falls back to the default; anything above the cap is clamped down to it. A
// cap exists because each extra round costs one improve call plus a full replay fan-out
// (up to ~30 classify calls) — see improveRunner.improveMaxRounds, improve.go.
const (
	ImproveMaxRoundsDefault = 3
	ImproveMaxRoundsCap     = 5
)

// ImproveExampleCapDefault/ImproveExampleCapMax bound SettingImproveExampleCap — the small,
// token-bounded set the improve prompt itself sees. ReplayExampleCapDefault/
// ReplayExampleCapMax bound SettingReplayExampleCap — deliberately much larger, since
// replay's cost is classify calls, not prompt tokens; see improve.go's sampleExamples for
// the selection policy that fills either cap.
const (
	ImproveExampleCapDefault = 12
	ImproveExampleCapMax     = 20
	ReplayExampleCapDefault  = 40
	ReplayExampleCapMax      = 100
)

// Values for SettingImproveReasoningEffort. Every reasoning-capable model this project has
// tested on Bedrock exposes reasoning_config as a bare on/off switch, not a real graduated
// ladder (see reasoningEffortSupported, reasoning.go, for the live-verified sweep across
// vendors), so the vocabulary is deliberately just two values rather than a Low/Medium/High
// ladder the UI can't actually back for any known model.
const (
	ReasoningEffortOff = "off"
	ReasoningEffortOn  = "on"
)

// Values for SettingClassifyTier and SettingImproveTier.
const (
	TierStandard = "standard"
	TierFlex     = "flex"
)

// ModelOption is one entry in the model-selection dropdown.
type ModelOption struct {
	ID                  string  // value sent to Bedrock (modelId or inferenceProfileId)
	Label               string  // human-readable display name
	InputCostPer1M      float64 // standard on-demand input price per 1M tokens; CostUnknown if unpriced
	OutputCostPer1M     float64 // standard on-demand output price per 1M tokens; CostUnknown if unpriced
	FlexCostPer1M       float64 // flex-tier input price per 1M tokens; CostUnknown if unpriced or not flex-capable
	FlexOutputCostPer1M float64 // flex-tier output price per 1M tokens; CostUnknown if unpriced or not flex-capable
	// ProfileRegion is the cross-region inference-profile geography ("us", "global", "eu",
	// "apac", "us-gov"), or "" for a bare/single-datacenter foundation-model id.
	ProfileRegion string
	Flex          bool // true when the AWS Price List API reports a flex-tier SKU
}

// converseAPI is the subset of *bedrockruntime.Client used for chat calls. Narrowing to
// an interface lets tests substitute a fake without a network round-trip.
//
// ConverseStream returns *bedrockruntime.ConverseStreamEventStream rather than the SDK's
// own *bedrockruntime.ConverseStreamOutput: that wrapper's event-stream reader is an
// unexported field with no public constructor, so a test fake could never produce one —
// only the real SDK client can. ConverseStreamEventStream, by contrast, is explicitly
// documented by the SDK as constructible via NewConverseStreamEventStream "for testing and
// mocking," with an exported Reader field. Unwrapping one layer here (see bedrockAdapter)
// is what keeps streamGenerate and ImprovePromptInstructions fakeable end to end.
type converseAPI interface {
	Converse(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
	ConverseStream(ctx context.Context, params *bedrockruntime.ConverseStreamInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamEventStream, error)
}

// bedrockAdapter adapts *bedrockruntime.Client to converseAPI — see converseAPI's doc
// comment for why ConverseStream needs to unwrap the SDK's output to its event stream.
type bedrockAdapter struct{ c *bedrockruntime.Client }

func (a bedrockAdapter) Converse(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	return a.c.Converse(ctx, params, optFns...)
}

func (a bedrockAdapter) ConverseStream(ctx context.Context, params *bedrockruntime.ConverseStreamInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamEventStream, error) {
	out, err := a.c.ConverseStream(ctx, params, optFns...)
	if err != nil {
		return nil, err
	}
	return out.GetStream(), nil
}

// Client wraps the Bedrock runtime client with per-call model resolution.
type Client struct {
	br converseAPI

	// awsCfg backs the lazily-built control-plane/pricing clients below. Only
	// ListAvailableModels (the settings-UI model dropdown) ever touches them — scan/push
	// cold starts never call it, so building bc/pc eagerly in NewClient would be pure
	// waste on those paths.
	awsCfg aws.Config
	bcOnce sync.Once
	bc     *bedrock.Client // control-plane, for listing
	pcOnce sync.Once
	pc     *pricing.Client // AWS Price List API, for dynamic pricing + flex eligibility

	defaultModel string
	settings     Settings
}

// DefaultModel is the fallback Bedrock model id used only when nothing else specifies
// one (no classify_model setting and no BEDROCK_MODEL env var) — e.g. on a fresh
// install before Settings has been configured. The model actually used for
// classification at runtime is whatever's configured there; nothing elsewhere in this
// package should assume this specific model's behavior (see reasoningRegistry in
// reasoning.go for why that assumption bit us before).
const DefaultModel = "us.amazon.nova-micro-v1:0"

// newBedrockRetryer builds the aws.Retryer used by NewClient: adaptive retry + client-side
// rate limiting (classification is fanned out across goroutines — see
// processor.ProcessConfig.ClassifyConcurrency — so concurrent requests are more likely to
// hit on-demand throttling; back off instead of failing fast), plus retries for
// signature/clock-skew errors. A Lambda execution environment frozen and thawed between
// invocations can briefly sign a request with a stale wall clock, which Bedrock rejects as
// InvalidSignatureException even though the request itself was fine — the SDK doesn't treat
// that code as retryable by default, so it's added explicitly here. A retry a moment later
// re-signs with the (by then re-synced) clock and succeeds. Factored out of NewClient so
// tests can exercise the retry classification without a network round-trip.
func newBedrockRetryer() aws.Retryer {
	return retry.AddWithErrorCodes(
		retry.NewAdaptiveMode(func(o *retry.AdaptiveModeOptions) {
			o.StandardOptions = append(o.StandardOptions, func(so *retry.StandardOptions) {
				so.MaxAttempts = 5
			})
		}),
		"InvalidSignatureException",
		"RequestExpired",
		"RequestTimeTooSkewed",
		"SignatureDoesNotMatch",
	)
}

// NewClient creates a Bedrock client.
// settings provides per-call model lookups (classify_model / improve_model keys).
// defaultModel is the fallback when neither a setting nor BEDROCK_MODEL is set.
func NewClient(settings Settings, defaultModel string) *Client {
	if defaultModel == "" {
		defaultModel = os.Getenv("BEDROCK_MODEL")
	}
	if defaultModel == "" {
		defaultModel = DefaultModel
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		// Generous read-timeout so the HTTP layer doesn't abort a queued flex-tier request
		// before the Lambda's own timeout does.
		awsconfig.WithHTTPClient(awshttp.NewBuildableClient().WithTimeout(bedrockHTTPTimeout)),
		// Adaptive retry + client-side rate limiting, plus clock-skew signature errors;
		// see newBedrockRetryer.
		awsconfig.WithRetryer(newBedrockRetryer),
	)
	if err != nil {
		panic(fmt.Sprintf("bedrock: load aws config: %v", err))
	}
	return &Client{
		br:           bedrockAdapter{c: bedrockruntime.NewFromConfig(cfg)},
		awsCfg:       cfg,
		defaultModel: defaultModel,
		settings:     settings,
	}
}

// controlPlane lazily builds the Bedrock control-plane client (ListFoundationModels/
// ListInferenceProfiles), built on first use rather than in NewClient.
func (c *Client) controlPlane() *bedrock.Client {
	c.bcOnce.Do(func() { c.bc = bedrock.NewFromConfig(c.awsCfg) })
	return c.bc
}

// pricingClient lazily builds the AWS Price List API client, built on first use rather
// than in NewClient. The Price List API only runs in us-east-1 (and ap-south-1); pin it
// there regardless of c.awsCfg.Region — it's a read-only pricing catalog, not LLM traffic.
func (c *Client) pricingClient() *pricing.Client {
	c.pcOnce.Do(func() {
		c.pc = pricing.NewFromConfig(c.awsCfg, func(o *pricing.Options) { o.Region = pricingRegion })
	})
	return c.pc
}

// resolveSetting looks up key in the store; falls back to def when unset, empty, or the
// store has no value.
func (c *Client) resolveSetting(ctx context.Context, key, def string) string {
	if c.settings != nil {
		if v, err := c.settings.GetSetting(ctx, key); err == nil && v != "" {
			return v
		}
	}
	return def
}

// resolveModel looks up the setting key in the store; falls back to defaultModel.
func (c *Client) resolveModel(ctx context.Context, key string) string {
	return c.resolveSetting(ctx, key, c.defaultModel)
}

// resolveClassifyTier looks up the classification service tier; defaults to "standard".
func (c *Client) resolveClassifyTier(ctx context.Context) string {
	return c.resolveSetting(ctx, SettingClassifyTier, TierStandard)
}

// resolveImproveTier looks up the prompt-improver service tier; defaults to "standard".
func (c *Client) resolveImproveTier(ctx context.Context) string {
	return c.resolveSetting(ctx, SettingImproveTier, TierStandard)
}

// serviceTierFor converts a tier setting value into the Converse ServiceTier parameter:
// the flex tier when selected, nil (Bedrock's implicit standard) otherwise.
func serviceTierFor(tier string) *types.ServiceTier {
	if tier == TierFlex {
		return &types.ServiceTier{Type: types.ServiceTierTypeFlex}
	}
	return nil
}

// requestMetadataFor tags a Converse/ConverseStream call with which flow issued it
// ("classify", "improve", or "generate"), so Bedrock model-invocation logs/CloudTrail
// can be filtered by call type when invocation logging is enabled. Purely additive:
// harmless (and unread) when it isn't.
func requestMetadataFor(flow string) map[string]string {
	return map[string]string{"flow": flow}
}

// ResolveClassifySettings resolves the classify model, service tier, and reasoning-
// directive override once. Callers classifying many emails in one pass (e.g. a scan)
// should call this once up front and pass the result into each ClassifyEmailBatch call,
// instead of re-resolving (a GetSetting DynamoDB read) per email.
func (c *Client) ResolveClassifySettings(ctx context.Context) (model, tier, reasoningOverride string) {
	return c.resolveModel(ctx, SettingClassifyModel), c.resolveClassifyTier(ctx), c.resolveSetting(ctx, SettingClassifyReasoningDirective, "")
}

// ============================================================
// Public types (preserved from the pre-Bedrock LLM client for caller compatibility)
// ============================================================

type Email struct {
	Sender  string
	Subject string
	Body    string
	Snippet string
}

type Prompt struct {
	ID           int64
	Name         string
	Instructions string
}

type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

type ClassifyResult struct {
	Results     map[int64]bool
	RequestJSON string
	RawResponse string
	LatencyMs   int64 // wall time of the Bedrock Converse call, in milliseconds

	// Token usage from the Converse call's Usage block, populated on a best-effort basis
	// (Bedrock's Usage field is optional in the SDK response) so ad-hoc token-cost
	// investigations don't need a debug build. See ClassifyEmailBatch and logUsage.
	InputTokens  int32
	OutputTokens int32
	TotalTokens  int32

	// ReasoningDetected is true if the model produced chain-of-thought/reasoning
	// content despite the reasoning-suppression directive (see reasoningOff in
	// reasoning.go) — the signal that suppression is or isn't actually working, since
	// no Bedrock API reports this directly.
	ReasoningDetected bool

	// StopReason is the Converse response's stop reason (e.g. "end_turn", "max_tokens"),
	// recorded verbatim from the SDK's types.StopReason. Populated on every successful
	// call; empty only if the call itself failed. See ClassifyEmailBatch, which also
	// turns a max_tokens stop paired with a failed JSON extraction into a dedicated
	// truncation error instead of the generic parse-failure one.
	StopReason string
}

type StreamChunk struct {
	Text string
	// Reasoning carries a chain-of-thought delta, kept separate from Text so a caller
	// can render (or discard) thinking output independently of the answer it's building
	// toward — see ImprovePromptInstructions and streamGenerate, both of which forward
	// ContentBlockDeltaMemberReasoningContent text here rather than mixing it into Text.
	Reasoning string
	Err       error
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ExampleRef is one labeled example rendered into an improve call's user turn — a
// compact view of db.PromptExample (sender/subject/body-excerpt only; the verdict is
// carried by which ImproveRequest slice the example sits in, not a field here).
type ExampleRef struct {
	Sender  string
	Subject string
	Excerpt string
	// Recurred marks an example whose problem an earlier applied suggestion already
	// claimed to fix, and it happened again — see db.PromptExample.Recurred, which this is
	// copied from. formatExampleRefs renders it as a "[RECURRED]" prefix so the improver
	// can tell "first time seeing this" apart from "already tried once and failed," which
	// calls for a bigger rewrite than a small wording tweak.
	Recurred bool
}

// ImproveRequest carries a rule and its labeled-example corpus into ImprovePromptInstructions,
// grouped by verdict instead of the single mishandled email the pre-corpus version used:
// ShouldMatch (false negatives — emails the rule missed), ShouldNotMatch (false positives —
// emails the rule wrongly caught), and AlreadyCorrect (confirmed positives — emails the rule
// must keep matching). Any slice may be empty, including all three for a rule with no
// recorded corpus yet; the improve prompt is written to stay coherent in that case.
type ImproveRequest struct {
	PromptName           string
	LabelName            string
	OriginalInstructions string
	ShouldMatch          []ExampleRef
	ShouldNotMatch       []ExampleRef
	AlreadyCorrect       []ExampleRef
	UserNote             string
	PriorConversation    []ChatMessage
	UserComment          string
	// PastAttempts is this rule's earlier live versions (excluding the current one —
	// OriginalInstructions already shows that), oldest evidence discarded first by the
	// caller (see improve.go's attemptsForPrompt). Rendered by buildImproveUserTurn's PAST
	// ATTEMPTS section, capped at maxAttemptLines — this is the improve loop's cross-
	// session memory: without it, every round starts from a blank slate and can re-propose
	// a phrasing this exact rule already tried and already failed.
	PastAttempts []AttemptRef
}

// ============================================================
// Classification
// ============================================================

// classifySystemPrompt is the invariant role + output-contract instructions for a
// classify call, sent as a Converse system content block. Everything that varies per
// call (the rules, the count-dependent example, the email) lives in the user turn built
// by buildUserTurn instead — see classifyPayload. Splitting it this way is the proper
// Converse idiom; the pre-cleanup version crammed all of this into a single user message
// (an artifact of the earlier Ollama single-prompt-string port).
const classifySystemPrompt = `You are an email classification assistant. You will be given a numbered list of rules and an email. Decide which rules apply to the email.

Respond with a single JSON object and nothing else: {"m": [rule numbers that apply]}. List only the numbers of rules that apply, in ascending order. Use {"m": []} when no rule applies.
Output ONLY that JSON object. Do not include any explanation, reasoning, preamble, "<think>" block, or markdown code fences before or after it.`

// classifyURLRe matches an http(s) URL for stripping from the email body sent to the
// classify prompt. Visible URL text in marketing/redirect-heavy email is pure noise for
// classification — rules key off sender, subject, and prose content, not link targets — and
// long tracking URLs (utm_* params, etc.) can consume a disproportionate share of the fixed
// truncation budget. Only visible link *text* ever reaches here; href attribute values are
// already excluded upstream by gmail/html.go's extractText.
var classifyURLRe = regexp.MustCompile(`https?://\S+`)

// stripURLs removes URLs from s, replacing each with a single space so the surrounding words
// don't fuse together. Callers should follow with gmail.CollapseWhitespace to normalize the
// space left behind.
func stripURLs(s string) string {
	return classifyURLRe.ReplaceAllString(s, " ")
}

// buildUserTurn renders the per-call data (rules, a count-matched example, and the
// email) as the classify user turn. The role and output-format contract are invariant
// across calls and live in classifySystemPrompt instead.
func buildUserTurn(email Email, prompts []Prompt) string {
	var sb strings.Builder
	for i, p := range prompts {
		fmt.Fprintf(&sb, "%d. %s: %s\n", i+1, p.Name, p.Instructions)
	}
	rulesText := sb.String()

	// The body is often HTML-derived prose fragmented into one-word-per-line runs by
	// extractText's per-tag newlines, plus NBSP-padding and visible tracking URLs — all
	// artifacts of the extraction pipeline with no classification signal. Cleaned up here
	// (rather than upstream in the gmail package) to keep the blast radius scoped to the
	// classify prompt only; Email.Body itself, and every other consumer of it, is untouched.
	body := email.Body
	if body == "" {
		body = email.Snippet
	}
	body = strings.ReplaceAll(body, "\r", "")
	body = stripURLs(body)
	body = gmailpkg.CollapseWhitespace(body)

	// The example is a fixed constant regardless of rule count — this prompt is the
	// model's only signal for the expected output shape (Converse tool-use isn't
	// attempted; see reasoning.go for why several model families this project has run
	// don't support it there), but the {"m": [...]} match-list contract doesn't need a
	// per-rule slot to demonstrate, unlike the old per-rule boolean map it replaced.
	return fmt.Sprintf(`Rules:
%s
Example (rule 1 applies, no others): {"m": [1]}

Email:
From: %s
Subject: %s
Body:
%s`,
		rulesText, email.Sender, email.Subject, body)
}

// mapKeysToResults converts a {"1": true, "2": false, ...} map (1-based rule index →
// verdict) into the prompt-ID-keyed result map ClassifyEmailBatch returns. This is the
// legacy response shape classifySystemPrompt asked for before the {"m": [...]} match-list
// contract; parseClassifyResponse falls back to it for older stored responses and models
// that don't follow the current instruction.
func mapKeysToResults(parsed map[string]any, prompts []Prompt) map[int64]bool {
	results := make(map[int64]bool, len(prompts))
	for k, v := range parsed {
		idx, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		idx-- // 1-based → 0-based
		if idx >= 0 && idx < len(prompts) {
			b, _ := v.(bool)
			results[prompts[idx].ID] = b
		}
	}
	return results
}

// parseClassifyResponse decodes either response shape: the compact {"m": [2, 5]}
// match-list contract classifySystemPrompt now asks for, or the legacy {"1": true,
// "2": false} per-rule boolean map that older stored responses (and models that ignore
// the current instruction) may still produce. Both mean the same thing, since an absent
// prompt ID is already read as false by the caller — mapKeysToResults never populated a
// key for rules the model omitted either.
func parseClassifyResponse(parsed map[string]any, prompts []Prompt) map[int64]bool {
	matches, ok := parsed["m"].([]any)
	if !ok {
		return mapKeysToResults(parsed, prompts)
	}

	results := make(map[int64]bool, len(prompts))
	for _, p := range prompts {
		results[p.ID] = false
	}
	for _, m := range matches {
		var idx int
		switch v := m.(type) {
		case float64:
			idx = int(v)
		case string:
			n, err := strconv.Atoi(v)
			if err != nil {
				continue
			}
			idx = n
		default:
			continue
		}
		idx-- // 1-based -> 0-based
		if idx >= 0 && idx < len(prompts) {
			results[prompts[idx].ID] = true
		}
	}
	return results
}

// rawDump renders a raw LLM response for a log message, but only under DEBUG_LOGGING —
// the response can quote email content back, which shouldn't be persisted to the log
// rows during normal operation. Returns "" when debug is off.
func rawDump(debug bool, raw string) string {
	if !debug {
		return ""
	}
	return " | raw: " + raw
}

// extractJSONObject scans s for the first top-level {...} span that is itself valid
// JSON, and returns it, or "" if none is found. Used to pull classification JSON out of
// a text-fallback response (see ClassifyEmailBatch) that may be wrapped in prose,
// markdown fences, or a reasoning model's "<think>...</think>" preamble — anything
// outside the braces is simply ignored rather than needing to be pattern-matched and
// stripped. A reasoning model's prose can itself contain unquoted "{...}" asides (e.g.
// "seems {like a} match") that balance but aren't valid JSON; those are skipped in
// favor of the next candidate rather than returned as a false match.
func extractJSONObject(s string) string {
	for start := strings.IndexByte(s, '{'); start >= 0; {
		end, ok := balancedBraceEnd(s, start)
		if !ok {
			return ""
		}
		candidate := s[start : end+1]
		if json.Valid([]byte(candidate)) {
			return candidate
		}
		next := strings.IndexByte(s[end+1:], '{')
		if next < 0 {
			return ""
		}
		start = end + 1 + next
	}
	return ""
}

// balancedBraceEnd returns the index of the '}' that closes the '{' at s[start],
// respecting quoted strings and backslash-escaped characters within them so braces
// inside string values don't affect nesting depth. ok is false if the braces never
// balance before the end of s.
func balancedBraceEnd(s string, start int) (end int, ok bool) {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// recordUsage records the Converse call's token usage onto res and returns the token
// fragment for the classify summary log line. Usage is populated best-effort: the SDK
// response's Usage field isn't guaranteed non-nil, so a nil usage returns "" rather than
// reporting misleading zeros.
func recordUsage(res *ClassifyResult, usage *types.TokenUsage) string {
	if usage == nil {
		return ""
	}
	res.InputTokens = aws.ToInt32(usage.InputTokens)
	res.OutputTokens = aws.ToInt32(usage.OutputTokens)
	res.TotalTokens = aws.ToInt32(usage.TotalTokens)
	return fmt.Sprintf("tokens input=%d output=%d total=%d",
		res.InputTokens, res.OutputTokens, res.TotalTokens)
}

// debugRequestJSON renders in as the JSON body ClassifyEmailBatch's debug output persists on
// ClassifyResult.RequestJSON — a projection of the actual Converse request (read straight off
// in), not a separately rebuilt copy, so it can never drift from what's really sent.
// AdditionalModelRequestFields is the one field handled specially: document.Interface has no
// exported way to recover its underlying value except via its smithy Marshaler method, which
// renders it straight to JSON bytes — those are substituted in as-is (its fields are otherwise
// unexported and would render as "{}").
func debugRequestJSON(in *bedrockruntime.ConverseInput) string {
	type dump struct {
		ModelID                      *string                       `json:"modelId,omitempty"`
		Messages                     []types.Message               `json:"messages,omitempty"`
		System                       []types.SystemContentBlock    `json:"system,omitempty"`
		InferenceConfig              *types.InferenceConfiguration `json:"inferenceConfig,omitempty"`
		ServiceTier                  *types.ServiceTier            `json:"serviceTier,omitempty"`
		RequestMetadata              map[string]string             `json:"requestMetadata,omitempty"`
		AdditionalModelRequestFields json.RawMessage               `json:"additionalModelRequestFields,omitempty"`
	}
	d := dump{
		ModelID:         in.ModelId,
		Messages:        in.Messages,
		System:          in.System,
		InferenceConfig: in.InferenceConfig,
		ServiceTier:     in.ServiceTier,
		RequestMetadata: in.RequestMetadata,
	}
	if in.AdditionalModelRequestFields != nil {
		if b, err := in.AdditionalModelRequestFields.MarshalSmithyDocument(); err == nil {
			d.AdditionalModelRequestFields = b
		}
	}
	b, err := json.Marshal(d)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// textBlock wraps text as a single-item ContentBlock slice — the shape every Message in
// this file uses for its content.
func textBlock(text string) []types.ContentBlock {
	return []types.ContentBlock{&types.ContentBlockMemberText{Value: text}}
}

// chatMessage builds a Message with a single text content block for role.
func chatMessage(role types.ConversationRole, text string) types.Message {
	return types.Message{Role: role, Content: textBlock(text)}
}

// userMessage builds a single-block user-role Message — the common case among chatMessage's
// callers.
func userMessage(text string) types.Message {
	return chatMessage(types.ConversationRoleUser, text)
}

// sysText wraps text as a SystemContentBlock.
func sysText(text string) types.SystemContentBlock {
	return &types.SystemContentBlockMemberText{Value: text}
}

// systemBlock wraps text as a single-item SystemContentBlock slice — the shape every
// Converse/ConverseStream call in this file uses for its system prompt.
func systemBlock(text string) []types.SystemContentBlock {
	return []types.SystemContentBlock{sysText(text)}
}

// classifyPayload returns the request pieces for a classify Converse call: the user
// message, inference config, the system content blocks (the invariant role/output
// contract, plus — when modelID's family is in reasoningRegistry, or reasoningOverride
// is set — a trailing chain-of-thought-suppression block; see reasoning.go), and any
// additional model request fields that suppression directive also carries.
func classifyPayload(email Email, prompts []Prompt, modelID, reasoningOverride string) ([]types.Message, *types.InferenceConfiguration, []types.SystemContentBlock, document.Interface) {
	turn := buildUserTurn(email, prompts)
	// Reasoning-capable models can emit chain-of-thought as ordinary output tokens even
	// when asked to suppress it (see reasoning.go) — some don't honor the suppression
	// directive on every call. MaxTokens is sized to absorb that rather than truncate a
	// response mid-stream; it's a ceiling, not a target, so it costs nothing when the
	// model finishes well under it. The len(prompts)*12 scaling predates the {"m": [...]}
	// match-list response (which stays a handful of tokens regardless of rule count) and
	// is now purely headroom for a rule-count-proportional wall of suppressed CoT, not a
	// sizing of the expected answer.
	maxTokens := int32(3000)
	if n := len(prompts) * 12; n > 3000 {
		maxTokens = int32(min(n, math.MaxInt32)) //nolint:gosec // bounded to int32 range by min()
	}
	msgs := []types.Message{userMessage(turn)}
	inf := &types.InferenceConfiguration{
		MaxTokens:   aws.Int32(maxTokens),
		Temperature: aws.Float32(0),
	}

	sys := systemBlock(classifySystemPrompt)
	var fields document.Interface
	if d := reasoningOff(modelID, reasoningOverride); !d.isZero() {
		if d.system != "" {
			sys = append(sys, sysText(d.system))
		}
		if d.fields != nil {
			fields = document.NewLazyDocument(d.fields)
		}
	}
	return msgs, inf, sys, fields
}

// ClassifyEmailBatch classifies one email against prompts using the given model and
// service tier. Callers classifying many emails in one pass should resolve
// model/tier/reasoningOverride once via ResolveClassifySettings and reuse them across
// calls. reasoningOverride overrides reasoningRegistry's suppression directive for a
// model the registry doesn't know about yet (see SettingClassifyReasoningDirective);
// pass "" to use the registry as-is. debug gates building the (comparatively expensive)
// serialized request JSON onto ClassifyResult.RequestJSON — it's only ever read by the
// Troubleshooting UI's debug write, so normal operation skips it.
func (c *Client) ClassifyEmailBatch(ctx context.Context, store StoreLogger, email Email, prompts []Prompt, model, tier, reasoningOverride string, debug bool) (ClassifyResult, error) {
	if len(prompts) == 0 {
		return ClassifyResult{}, nil
	}

	msgs, inf, sys, fields := classifyPayload(email, prompts, model, reasoningOverride)

	in := &bedrockruntime.ConverseInput{
		ModelId:                      aws.String(model),
		Messages:                     msgs,
		System:                       sys,
		InferenceConfig:              inf,
		ServiceTier:                  serviceTierFor(tier),
		AdditionalModelRequestFields: fields,
		RequestMetadata:              requestMetadataFor("classify"),
	}

	var res ClassifyResult
	if debug {
		res.RequestJSON = debugRequestJSON(in)
	}

	tierLabel := tier
	if tierLabel == "" {
		tierLabel = TierStandard
	}

	// The Lambda invoking this has a hard 900s (15min) ceiling; bedrockHTTPTimeout aborts
	// a stuck Converse call ~1min before that so the error below can still be logged
	// rather than the whole invocation being silently killed.
	start := time.Now()
	out, err := c.br.Converse(ctx, in)
	res.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		if isBedrockTimeout(err) {
			store.Log(LogLevelTimeout, fmt.Sprintf("Bedrock Converse call exceeded the %s client timeout (tier: %s): %v", bedrockHTTPTimeout, tierLabel, err))
		}
		store.Log("ERROR", fmt.Sprintf("LLM request failed after %dms (tier: %s): %v", res.LatencyMs, tierLabel, err))
		return res, &Error{Msg: fmt.Sprintf("LLM request failed: %v", err)}
	}
	tokenFragment := recordUsage(&res, out.Usage)
	res.StopReason = string(out.StopReason)

	raw := extractText(out.Output)
	res.ReasoningDetected = detectReasoning(out.Output, raw)
	// One summary line per call. The "reasoning: suppressed=true/false" phrasing is
	// load-bearing: the settings page's reasoning-override help text tells the user to
	// look for exactly that in the logs.
	combined := fmt.Sprintf("LLM classify: %dms", res.LatencyMs)
	if tokenFragment != "" {
		combined += ", " + tokenFragment
	}
	combined += fmt.Sprintf(", reasoning: suppressed=%v, content=%d chars (tier: %s), stop=%s", !res.ReasoningDetected, len(raw), tierLabel, res.StopReason)
	// Response previews/dumps can quote email content back, so they're persisted to the
	// (auth-gated, TTL'd) log rows only under DEBUG_LOGGING — defense in depth.
	if debug && len(raw) > 0 {
		preview := raw
		if len(preview) > 500 {
			preview = preview[:500]
		}
		combined += " | " + preview
	}
	store.Log("INFO", combined)
	res.RawResponse = raw

	cleaned := extractJSONObject(raw)
	if cleaned == "" {
		if out.StopReason == types.StopReasonMaxTokens {
			store.Log("ERROR", "LLM response truncated at max_tokens before a JSON object could be found"+rawDump(debug, raw))
			return res, &Error{Msg: "LLM response truncated at max_tokens before a JSON object could be found"}
		}
		store.Log("ERROR", "LLM parse error: no JSON object found"+rawDump(debug, raw))
		return res, &Error{Msg: "LLM parse error: no JSON object found in response"}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		store.Log("ERROR", fmt.Sprintf("LLM parse error: %v%s", err, rawDump(debug, raw)))
		return res, &Error{Msg: fmt.Sprintf("LLM parse error: %v", err)}
	}

	res.Results = parseClassifyResponse(parsed, prompts)
	return res, nil
}

// ============================================================
// Streaming prompt generation
// ============================================================

func (c *Client) StreamGeneratePromptInstruction(ctx context.Context, description string) <-chan StreamChunk {
	ch := make(chan StreamChunk, 16)
	go func() {
		defer close(ch)
		if err := c.streamGenerate(ctx, description, ch); err != nil {
			select {
			case ch <- StreamChunk{Err: err}:
			case <-ctx.Done():
			}
		}
	}()
	return ch
}

func (c *Client) streamGenerate(ctx context.Context, description string, ch chan<- StreamChunk) error {
	model := c.resolveModel(ctx, SettingImproveModel)
	// Same 60-word/single-line/no-markdown shape as improveSystemPrompt, so a rule
	// written by the builder and a rule rewritten by the improver read the same way — both
	// end up inline in buildUserTurn's numbered rule list at classify time, and both are
	// applied by a small model with reasoning disabled.
	systemPrompt := "You write email filter rules for an AI classifier. Output only the rule text: one line, at most 60 words, plain declarative prose, no bullets, headings, markdown, quotes, preamble, or self-critique."
	userMsg := fmt.Sprintf(
		"Write a one-line classifier instruction for emails matching: %q\n\n"+
			"The instruction must describe: what the email is about, its purpose/intent, "+
			"and what distinguishes it from similar-but-non-matching emails. "+
			"Do not use keywords or sender addresses as criteria — focus on meaning and context.\n\n"+
			"Output ONLY the instruction text.",
		description)

	// Reasoning suppression (see reasoning.go): same rationale as ImprovePromptInstructions
	// — a rule padded with chain-of-thought is exactly what this prompt is trying to avoid.
	sys := systemBlock(systemPrompt)
	var fields document.Interface
	if d := reasoningOff(model, ""); !d.isZero() {
		if d.system != "" {
			sys = append(sys, sysText(d.system))
		}
		if d.fields != nil {
			fields = document.NewLazyDocument(d.fields)
		}
	}

	stream, err := c.br.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{
		ModelId:  aws.String(model),
		System:   sys,
		Messages: []types.Message{userMessage(userMsg)},
		InferenceConfig: &types.InferenceConfiguration{
			MaxTokens:   aws.Int32(2048),
			Temperature: aws.Float32(0.7),
		},
		ServiceTier:                  serviceTierFor(c.resolveImproveTier(ctx)),
		AdditionalModelRequestFields: fields,
		RequestMetadata:              requestMetadataFor("generate"),
	})
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()

	for event := range stream.Events() {
		switch e := event.(type) {
		case *types.ConverseStreamOutputMemberContentBlockDelta:
			switch d := e.Value.Delta.(type) {
			case *types.ContentBlockDeltaMemberText:
				if d.Value != "" {
					select {
					case ch <- StreamChunk{Text: d.Value}:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			case *types.ContentBlockDeltaMemberReasoningContent:
				// Same thinking view ImprovePromptInstructions streams — the builder call
				// uses this system prompt's own reasoning suppression above, but a model
				// that leaks a ReasoningContent block anyway is worth showing rather than
				// silently dropping (matches ImprovePromptInstructions' behavior).
				if rt, ok := d.Value.(*types.ReasoningContentBlockDeltaMemberText); ok && rt.Value != "" {
					select {
					case ch <- StreamChunk{Reasoning: rt.Value}:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			}
		case *types.ConverseStreamOutputMemberMessageStop:
			if e.Value.StopReason == types.StopReasonMaxTokens {
				return errors.New("LLM response truncated at max_tokens")
			}
		}
	}
	return stream.Err()
}

// ============================================================
// Prompt improvement
// ============================================================

// thinkBlockRe strips a leaked <think>...</think> span — reasoning suppression (reasoningOff
// in reasoning.go) isn't guaranteed to work for every model family, and this is the one
// concrete artifact that shows up in raw output when it doesn't.
var thinkBlockRe = regexp.MustCompile(`(?is)<think>.*?</think>`)

// codeFenceRe unwraps a ```...``` fenced block some models default to for anything that
// looks like "output text", despite improveSystemPrompt asking for plain prose.
var codeFenceRe = regexp.MustCompile("(?s)```(?:[a-zA-Z]*\n)?(.*?)```")

// sanitizeRuleText cleans an LLM-produced rule instruction before it's stored or shown to
// the user: strips a leaked <think> block, unwraps a markdown code fence and surrounding
// quotes, then collapses all embedded whitespace (including newlines) to single spaces.
// That last step isn't cosmetic — buildUserTurn (see the classify section below) renders
// every rule inline as "N. Name: Instructions" in the numbered rule list sent to the
// classify model, so a multi-line rule would corrupt the shape of that list for every rule
// after it. Applied to ImprovePromptInstructions' output; not applied to
// StreamGeneratePromptInstruction, which streams chunks live as they arrive and has
// nothing to post-process once the call is a single buffered string.
func sanitizeRuleText(s string) string {
	s = thinkBlockRe.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	if m := codeFenceRe.FindStringSubmatch(s); m != nil {
		s = strings.TrimSpace(m[1])
	}
	s = strings.Trim(s, "\"'“”‘’")
	s = strings.TrimSpace(s)
	return strings.Join(strings.Fields(s), " ")
}

// improveSystemPrompt targets a small model with reasoning disabled (see reasoningOff,
// applied below) applying the *output* of this call, one email at a time, with no
// chain-of-thought budget of its own — so the rewritten rule has to be short and literal
// enough to be decidable on sight. The pre-corpus version of this prompt was framed around
// a single mishandled email and told the model to "think as long as you need internally,"
// which is exactly backwards for that target: it produced long, hedged, multi-clause rules
// that a reasoning-disabled classifier then had to interpret under the same constraint.
const improveSystemPrompt = `You rewrite email-classification rules. Output only the rewritten rule text.

- One paragraph, one line, at most 60 words.
- First sentence: what the email IS, by purpose and intent.
- Then, only if needed: what to exclude, phrased as "Do not match ...".
- Keep the original scope. Never widen a narrow rule into a catch-all.
- Never cite a sender, subject, or body phrase from the examples. Generalize to the category.
- Plain declarative prose. No bullets, headings, markdown, quotes, or hedging.
- If PAST ATTEMPTS are shown, do not restate one of them. They already failed.

A small model with no reasoning applies this rule to one email at a time.
It must be decidable from the email alone.`

// formatExampleRefs renders a labeled-example group as "- sender | subject | excerpt" lines
// for the improve user turn, one call per ImproveRequest slice (ShouldMatch/ShouldNotMatch/
// AlreadyCorrect). Returns "" for an empty slice so buildImproveUserTurn can omit the
// section entirely rather than print an empty heading — matters most for a brand-new rule
// with no corpus yet, where the prompt otherwise still has to read coherently. A recurring
// example (r.Recurred — see ExampleRef's doc comment) gets a "[RECURRED] " prefix: three
// tokens, only on the lines that matter, rather than a whole extra section.
func formatExampleRefs(refs []ExampleRef) string {
	if len(refs) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, r := range refs {
		prefix := ""
		if r.Recurred {
			prefix = "[RECURRED] "
		}
		fmt.Fprintf(&sb, "- %s%s | %s | %s\n", prefix, r.Sender, r.Subject, r.Excerpt)
	}
	return sb.String()
}

// buildImproveUserTurn renders one ImproveRequest as the improve call's first user turn.
// Sections for empty example groups are omitted rather than printed empty.
func buildImproveUserTurn(req ImproveRequest) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "RULE: %s   LABEL: %s\n\nCURRENT:\n%s\n", req.PromptName, req.LabelName, req.OriginalInstructions)

	if s := formatAttempts(req.PastAttempts); s != "" {
		fmt.Fprintf(&sb, "\nPAST ATTEMPTS (already tried on this rule; each still had problems — do not repeat them):\n%s", s)
	}

	// Each clause of the closing instruction is only meaningful if its section was
	// actually printed above — a brand-new rule with no corpus yet would otherwise get a
	// closing line that references three example groups that don't exist ("every SHOULD
	// MATCH matches" with no SHOULD MATCH section anywhere above it), which is confusing
	// rather than merely redundant. goals collects only the clauses that apply.
	var goals []string
	var anyRecurred bool
	if s := formatExampleRefs(req.ShouldMatch); s != "" {
		fmt.Fprintf(&sb, "\nSHOULD MATCH (missed these):\n%s", s)
		goals = append(goals, "every SHOULD MATCH matches")
	}
	if s := formatExampleRefs(req.ShouldNotMatch); s != "" {
		fmt.Fprintf(&sb, "\nSHOULD NOT MATCH (wrongly caught these):\n%s", s)
		goals = append(goals, "no SHOULD NOT MATCH matches")
	}
	if s := formatExampleRefs(req.AlreadyCorrect); s != "" {
		fmt.Fprintf(&sb, "\nALREADY CORRECT (do not break these):\n%s", s)
		goals = append(goals, "every ALREADY CORRECT still matches")
	}
	for _, refs := range [][]ExampleRef{req.ShouldMatch, req.ShouldNotMatch, req.AlreadyCorrect} {
		for _, r := range refs {
			if r.Recurred {
				anyRecurred = true
			}
		}
	}
	if req.UserNote != "" {
		fmt.Fprintf(&sb, "\nUSER NOTE: %s\n", req.UserNote)
	}

	if len(goals) > 0 {
		fmt.Fprintf(&sb, "\nRewrite CURRENT so %s.", strings.Join(goals, ", "))
		if anyRecurred {
			sb.WriteString(" The [RECURRED] cases survived an earlier rewrite — a small wording tweak will not fix them; change what the rule actually checks for.")
		}
	} else {
		sb.WriteString("\nRewrite CURRENT to be clearer and more precise, preserving its exact scope and intent.")
	}
	return sb.String()
}

// AttemptRef is one earlier version of this rule, shown to the improver as evidence of
// what was already tried — see ImproveRequest.PastAttempts and buildImproveUserTurn's PAST
// ATTEMPTS section, and improve.go's attemptsForPrompt for how these are built from
// db.PromptVersion.
type AttemptRef struct {
	Instructions string
	// Passed/Total is the evidence for this attempt. Total == 0 means no evidence at all
	// (a manual edit with replay off, or a version too new to have accrued any observed
	// corrections yet) — formatAttempts omits that attempt entirely rather than render a
	// misleading "0/0", the same convention ReplayTotal == 0 uses elsewhere in this
	// package's callers.
	Passed, Total int
}

// maxAttemptLines caps how many PAST ATTEMPTS this codebase will ever show the improver —
// deliberately small, in line with this repo's recent token-compression commits (rule
// generation and classify responses were both cut for the same reason). Each attempt costs
// roughly one rule's worth of tokens (~85), so even the cap is a small fraction of a corpus
// turn's ~3,000 tokens.
const maxAttemptLines = 3

// formatAttempts renders req.PastAttempts as numbered "N. "text" -> P/T" lines, skipping
// any attempt with no evidence (Total == 0) and stopping at maxAttemptLines. Returns "" for
// an empty or all-skipped list, so buildImproveUserTurn can omit the section entirely —
// same empty-section convention formatExampleRefs uses.
func formatAttempts(attempts []AttemptRef) string {
	var sb strings.Builder
	n := 0
	for _, a := range attempts {
		if a.Total == 0 {
			continue
		}
		if n >= maxAttemptLines {
			break
		}
		n++
		text := a.Instructions
		if len(text) > 200 {
			text = text[:200]
		}
		fmt.Fprintf(&sb, "%d. %q -> scored %d/%d\n", n, text, a.Passed, a.Total)
	}
	return sb.String()
}

// ImproveSink receives incremental deltas from a streaming ImprovePromptInstructions call
// — Text for the answer as it's written, Reasoning for chain-of-thought on a model with
// reasoning turned on. nil means no one is watching; the call still streams (identical
// cost/latency to a non-streaming Converse) and the deltas are simply discarded. Passing a
// sink here rather than adding a second, streaming-only function keeps the max_tokens
// 2048/16384 split, the reasoning-fields ValidationException retry, sanitizeRuleText, and
// the conversation-building below in one place instead of forked between two call shapes.
type ImproveSink func(StreamChunk)

func (c *Client) ImprovePromptInstructions(ctx context.Context, req ImproveRequest, sink ImproveSink) (string, []ChatMessage, error) {
	// See improveCallTimeout's doc comment: this is the real latency cap for one improve
	// call, tighter than the blanket bedrockHTTPTimeout the underlying HTTP client also
	// enforces.
	ctx, cancel := context.WithTimeout(ctx, improveCallTimeout)
	defer cancel()

	model := c.resolveModel(ctx, SettingImproveModel)
	var msgs []types.Message

	// The first-turn prompt (no prior conversation) is used twice below — once as the
	// Bedrock message, once as the stored conversation entry — so it's built once here
	// rather than duplicating buildImproveUserTurn at both call sites.
	firstTurnMsg := buildImproveUserTurn(req)

	if len(req.PriorConversation) > 0 {
		for _, m := range req.PriorConversation {
			role := types.ConversationRoleUser
			if m.Role == "assistant" {
				role = types.ConversationRoleAssistant
			}
			msgs = append(msgs, chatMessage(role, m.Content))
		}
		msgs = append(msgs, userMessage(req.UserComment))
	} else {
		msgs = append(msgs, userMessage(firstTurnMsg))
	}

	// Reasoning: off by default (see improveSystemPrompt's target of one short line, not a
	// paragraph wrapped in a <think> block), same suppression classify uses (reasoning.go).
	// SettingImproveReasoningEffort (Settings UI) can turn it back on for a model family
	// that supports it. Deliberately NOT an if/else-if into reasoningOff: a non-off effort
	// that reasoningEffortFields can't honor (unrecognized family, or an effort outside
	// that family's levels) must leave the model at its own default, not fall through into
	// suppression — turning reasoning on and suppressing it are opposite intents for the
	// same call, and inverting one into the other silently would be worse than a no-op.
	sys := systemBlock(improveSystemPrompt)
	var fields document.Interface
	effort := c.resolveSetting(ctx, SettingImproveReasoningEffort, ReasoningEffortOff)
	switch {
	case effort == "" || effort == ReasoningEffortOff:
		if d := reasoningOff(model, ""); !d.isZero() {
			if d.system != "" {
				sys = append(sys, sysText(d.system))
			}
			if d.fields != nil {
				fields = document.NewLazyDocument(d.fields)
			}
		}
	case reasoningEffortFields(model, effort) != nil:
		fields = document.NewLazyDocument(reasoningEffortFields(model, effort))
	default:
		slog.Warn("improve reasoning effort requested but unsupported for model", "model", model, "effort", effort)
	}

	emit := func(chunk StreamChunk) {
		if sink != nil {
			sink(chunk)
		}
	}

	// stream runs one ConverseStream call and accumulates its answer text. emitted reports
	// whether any delta at all reached the sink — used below to gate the ValidationException
	// retry, since that retry must never re-run after a partial answer has already been
	// shown to the caller.
	stream := func(f document.Interface) (answer string, stopReason types.StopReason, emitted bool, err error) {
		// 2048 when reasoning is off/suppressed: with a 60-word target in the system prompt,
		// anything near that signals a truncation or a suppression failure, not a
		// legitimately long answer — worth keeping tight as that signal. But when reasoning
		// fields are actually being sent (f != nil), 2048 is the wrong side of that same
		// argument: it's a token budget for the *final answer*, and a model that's genuinely
		// thinking spends most of its budget on the reasoning trace first (observed up to
		// ~2000 chars of it against real models), so a 2048 ceiling truncates mid-thought
		// before the model ever reaches the answer — the improve call comes back with an
		// empty suggestion, not a short one. 16384 gives the thinking room to finish.
		maxTokens := int32(2048)
		if f != nil {
			maxTokens = 16384
		}
		es, streamErr := c.br.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{
			ModelId:  aws.String(model),
			System:   sys,
			Messages: msgs,
			InferenceConfig: &types.InferenceConfiguration{
				MaxTokens:   aws.Int32(maxTokens),
				Temperature: aws.Float32(0.4),
			},
			ServiceTier:                  serviceTierFor(c.resolveImproveTier(ctx)),
			AdditionalModelRequestFields: f,
			RequestMetadata:              requestMetadataFor("improve"),
		})
		if streamErr != nil {
			return "", "", false, streamErr
		}
		defer func() { _ = es.Close() }()

		var sb strings.Builder
		for event := range es.Events() {
			switch e := event.(type) {
			case *types.ConverseStreamOutputMemberContentBlockDelta:
				switch d := e.Value.Delta.(type) {
				case *types.ContentBlockDeltaMemberText:
					if d.Value != "" {
						sb.WriteString(d.Value)
						emitted = true
						emit(StreamChunk{Text: d.Value})
					}
				case *types.ContentBlockDeltaMemberReasoningContent:
					if rt, ok := d.Value.(*types.ReasoningContentBlockDeltaMemberText); ok && rt.Value != "" {
						emitted = true
						emit(StreamChunk{Reasoning: rt.Value})
					}
				}
			case *types.ConverseStreamOutputMemberMessageStop:
				stopReason = e.Value.StopReason
			}
		}
		if streamErr := es.Err(); streamErr != nil {
			return sb.String(), stopReason, emitted, streamErr
		}
		return sb.String(), stopReason, emitted, nil
	}

	answer, stopReason, emitted, err := stream(fields)
	if err != nil && fields != nil && !emitted {
		// reasoningEffortSupported (reasoning.go) defaults an unrecognized model to "assume
		// reasoning_config works" based on a broad but not exhaustive live sweep — a model
		// outside that sweep, or a provider-side change to one inside it, can still reject
		// this unvalidated passthrough field. Rather than let that take down every improve
		// call, retry once with the field dropped: reasoning silently stays off (loud in the
		// log instead), but the suggestion still gets generated. Gated on !emitted: once any
		// delta has reached the sink, a retry would replay a second (different) answer on
		// top of a partial one the caller has already seen — surface the error instead.
		var ve *types.ValidationException
		if errors.As(err, &ve) {
			slog.Warn("improve call rejected reasoning fields, retrying without them", "model", model, "effort", effort, "err", err)
			answer, stopReason, _, err = stream(nil)
		}
	}
	if err != nil {
		return "", nil, err
	}
	if stopReason == types.StopReasonMaxTokens {
		// stream's 2048/16384 split (see its comment) is sized for the common case either
		// way — hitting the ceiling regardless means either reasoning suppression/effort
		// selection above isn't actually working for this model, the model ignored
		// improveSystemPrompt's length constraint, or (reasoning on) the model's thinking
		// itself ran unusually long. Either way the stored suggestion below is a silent
		// truncation, not a complete rule, so this is worth a log line even though
		// sanitizeRuleText can't distinguish it from a normal response.
		slog.Warn("improve call hit max_tokens", "model", model, "effort", effort)
	}
	suggestion := sanitizeRuleText(answer)

	// Build updated conversation for storage
	var conv []ChatMessage
	conv = append(conv, req.PriorConversation...)
	if len(req.PriorConversation) > 0 {
		conv = append(conv, ChatMessage{Role: "user", Content: req.UserComment})
	} else {
		conv = append(conv, ChatMessage{Role: "user", Content: firstTurnMsg})
	}
	conv = append(conv, ChatMessage{Role: "assistant", Content: suggestion})

	return suggestion, conv, nil
}

// ============================================================
// Replay validation
// ============================================================

// ReplayExample is one labeled example scored during replay validation. Want is what the
// candidate rule is expected to output for it (true for a false_negative or
// confirmed_positive example, false for a false_positive one) — that verdict→bool mapping
// is the caller's job (recategorize.go's db.Verdict* constants aren't visible from this
// package, and shouldn't be: this package stays decoupled from db, see the Settings
// interface above for the same reasoning). Verdict is carried through only for display in
// ReplayFailures.
type ReplayExample struct {
	Verdict string
	Sender  string
	Subject string
	Excerpt string
	Want    bool
}

// ReplayFailure is one example the candidate rule scored incorrectly, for display next to
// the suggestion.
type ReplayFailure struct {
	Verdict string `json:"verdict"`
	Sender  string `json:"sender"`
	Subject string `json:"subject"`
	Got     bool   `json:"got"`
	// ExampleIndex is this failure's position in the []ReplayExample slice passed to
	// ReplayAgainstExamples. json:"-" is deliberate and load-bearing, not just tidiness:
	// the index is only meaningful within the process that ran this replay call — the
	// caller (improve.go's improve loop) rebuilds its example corpus fresh on every round,
	// so a persisted index would silently point at the wrong email after a reload or a
	// later round. It exists purely so a same-process caller (buildReplayFeedbackTurn) can
	// recover the full source example — body excerpt included — for the next round's
	// feedback turn without ReplayFailure itself needing to carry a copy of it.
	ExampleIndex int `json:"-"`
}

// ReplayResult summarizes a replay run. Total counts only examples that were successfully
// classified — a Bedrock error for one example (throttling, a transient timeout) isn't a
// signal about the candidate rule's quality, so it's logged and excluded rather than
// counted as a failure; Total < len(examples) is possible when that happens.
type ReplayResult struct {
	Model    string
	Total    int
	Passed   int
	Failures []ReplayFailure
}

// ReplayAgainstExamples re-runs candidateInstructions through the *classification* model —
// deliberately not the improve model that produced it — against a set of labeled examples,
// to answer "will the model that actually labels production mail apply this rewritten rule
// correctly?" Scoring it with the improve model would measure a model that never sees
// production email and would flatter every rewrite.
//
// This is the one place in the improve flow that's easy to get backwards: every other
// Bedrock call in ImprovePromptInstructions/streamGenerate resolves SettingImproveModel,
// and this function is invoked from deep inside that same flow
// (improveRunner.improveAndFinalizeSuggestion, improve.go). It must call
// ResolveClassifySettings — never resolveModel(ctx, SettingImproveModel) — resolved once
// here and reused across every example, exactly as ResolveClassifySettings' own doc
// comment directs for a batch of calls.
//
// concurrency <= 0 means unbounded: every example is classified at once, no semaphore.
// The MODE=improve worker (improve.go) always passes 0 — it runs inside its own 900s/1024MB
// Lambda invocation with no live HTTP request sharing the budget, so there's no reason to
// throttle a call fan-out that newBedrockRetryer's adaptive rate limiting already
// backpressures. A positive value still throttles, for any other caller that does need it.
func (c *Client) ReplayAgainstExamples(ctx context.Context, store StoreLogger, candidateInstructions string, examples []ReplayExample, concurrency int) ReplayResult {
	model, tier, reasoningOverride := c.ResolveClassifySettings(ctx)
	result := ReplayResult{Model: model}
	if len(examples) == 0 {
		return result
	}

	// candidatePrompt is a placeholder — the candidate rule isn't a saved db.Prompt, just
	// text being evaluated — so ClassifyEmailBatch only ever sees one prompt per call and
	// its id is never persisted or shown to the user.
	candidatePrompt := []Prompt{{ID: 1, Name: "candidate", Instructions: candidateInstructions}}

	type outcome struct {
		ex      ReplayExample
		got     bool
		errored bool
	}
	outcomes := make([]outcome, len(examples))
	var sem chan struct{}
	if concurrency > 0 {
		sem = make(chan struct{}, concurrency)
	}
	var wg sync.WaitGroup

	for i, ex := range examples {
		if sem != nil {
			sem <- struct{}{}
		}
		wg.Go(func() {
			if sem != nil {
				defer func() { <-sem }()
			}
			// See replayCallTimeout's doc comment: bounds this one example's classify call
			// so a single stuck call can't hold up the whole batch until bedrockHTTPTimeout.
			callCtx, cancel := context.WithTimeout(ctx, replayCallTimeout)
			defer cancel()
			email := Email{Sender: ex.Sender, Subject: ex.Subject, Body: ex.Excerpt}
			res, err := c.ClassifyEmailBatch(callCtx, store, email, candidatePrompt, model, tier, reasoningOverride, false)
			if err != nil {
				outcomes[i] = outcome{ex: ex, errored: true}
				return
			}
			outcomes[i] = outcome{ex: ex, got: res.Results[1]}
		})
	}
	wg.Wait()

	for i, o := range outcomes {
		if o.errored {
			store.Log("ERROR", fmt.Sprintf("replay validation: classify failed for example (verdict=%s), excluded from score", o.ex.Verdict))
			continue
		}
		result.Total++
		if o.got == o.ex.Want {
			result.Passed++
		} else {
			result.Failures = append(result.Failures, ReplayFailure{
				Verdict: o.ex.Verdict, Sender: o.ex.Sender, Subject: o.ex.Subject, Got: o.got,
				ExampleIndex: i,
			})
		}
	}
	return result
}

// ============================================================
// Model listing (control-plane)
// ============================================================

// modelModality records whether a foundation model takes text in and emits text out.
type modelModality struct{ textIn, textOut bool }

// regionPrefixes are the cross-region inference-profile prefixes stripped to recover the
// underlying foundation-model id (e.g. "us.anthropic.claude-..." -> "anthropic.claude-...").
var regionPrefixes = []string{"us-gov.", "global.", "apac.", "us.", "eu."}

func baseModelID(id string) string {
	for _, p := range regionPrefixes {
		if strings.HasPrefix(id, p) {
			return id[len(p):]
		}
	}
	return id
}

// profileRegion returns the cross-region inference-profile geography embedded in id
// ("us", "global", "eu", "apac", "us-gov"), or "" for a bare foundation-model id (no
// profile — pinned to whichever single region/datacenter the caller's endpoint is in).
func profileRegion(id string) string {
	for _, p := range regionPrefixes {
		if strings.HasPrefix(id, p) {
			return strings.TrimSuffix(p, ".")
		}
	}
	return ""
}

// ListAvailableModels returns Bedrock models suitable for text classification: text-in,
// text-out, streaming, on-demand foundation models, unioned with all system-defined
// inference profiles whose underlying model is likewise text-in/text-out (any geography —
// "us.", "global.", "eu.", "apac.", "us-gov.", or whatever AWS adds next; see
// ModelOption.ProfileRegion). Image, embedding, and other non-text models (e.g.
// stability.*, *embed*) are excluded. ModelOption.Flex marks flex-tier eligibility per the
// AWS Price List API. Callers decide their own geo policy from ProfileRegion/Flex — e.g.
// the Settings UI restricts Standard to "" (bare/single-datacenter)/"us"/"global" but lets
// Flex use any geography, since flex-eligible families currently have no "us." profile at
// all (see dynamic-sources-only project memory). Sorted cheapest input cost first, unpriced
// last.
func (c *Client) ListAvailableModels(ctx context.Context) ([]ModelOption, error) {
	seen := make(map[string]bool)
	var opts []ModelOption

	// Pricing + flex-tier eligibility come entirely from the AWS Price List API — no
	// hardcoded model data. Non-fatal on error (e.g. missing pricing:GetProducts IAM
	// permission): the dropdown still lists models, just without prices/flex info. It has
	// no data dependency on the foundation-model list below (only the inference-profile
	// pass, further down, actually reads cat), so the fetch — a paginated GetProducts call,
	// potentially several round trips — runs concurrently with ListFoundationModels instead
	// of serialized ahead of it.
	var cat *pricingCatalog
	var pricingWG sync.WaitGroup
	pricingWG.Go(func() {
		var pricingErr error
		cat, pricingErr = fetchPricingCatalog(ctx, c.pricingClient())
		if pricingErr != nil {
			cat = &pricingCatalog{
				inputPricePer1M:     map[string]float64{},
				flexInputPricePer1M: map[string]float64{},
				flexCapable:         map[string]bool{},
			}
		}
	})

	// One unfiltered catalog call: drives the foundation-model list AND supplies the
	// modality of the models that back each inference profile.
	fmOut, err := c.controlPlane().ListFoundationModels(ctx, &bedrock.ListFoundationModelsInput{})
	if err != nil {
		pricingWG.Wait() // let the in-flight fetch finish before returning, so it can't outlive this call
		return nil, fmt.Errorf("list foundation models: %w", err)
	}
	pricingWG.Wait()
	type summary struct {
		id       string
		modality modelModality
	}
	var summaries []summary
	for _, m := range fmOut.ModelSummaries {
		id := aws.ToString(m.ModelId)
		if id == "" {
			continue
		}
		mod := modelModality{
			textIn:  slices.Contains(m.InputModalities, bedrocktypes.ModelModalityText),
			textOut: slices.Contains(m.OutputModalities, bedrocktypes.ModelModalityText),
		}
		summaries = append(summaries, summary{id: id, modality: mod})

		// Foundation-model dropdown entry: text-in/out, streaming, on-demand.
		if seen[id] || !mod.textIn || !mod.textOut {
			continue
		}
		if m.ResponseStreamingSupported == nil || !*m.ResponseStreamingSupported {
			continue
		}
		if !slices.Contains(m.InferenceTypesSupported, bedrocktypes.InferenceTypeOnDemand) {
			continue
		}
		label := aws.ToString(m.ModelName)
		if label == "" {
			label = id
		}
		seen[id] = true
		opts = append(opts, ModelOption{
			ID:                  id,
			Label:               label,
			InputCostPer1M:      cat.inputCostPer1M(id),
			OutputCostPer1M:     cat.outputCostPer1M(id),
			FlexCostPer1M:       cat.flexCostPer1M(id),
			FlexOutputCostPer1M: cat.flexOutputCostPer1M(id),
			Flex:                cat.isFlexCapable(id),
		}) // ProfileRegion left as "" — bare foundation-model id, single datacenter
	}

	// modalityOf resolves a profile's underlying model modality. Foundation ids sometimes
	// carry a context suffix (e.g. "...-v1:0:8k") not present on the profile id, so match by
	// exact id or prefix. Unknown models are kept (conservative) rather than hidden.
	modalityOf := func(base string) (modelModality, bool) {
		for _, s := range summaries {
			if s.id == base || strings.HasPrefix(s.id, base) {
				return s.modality, true
			}
		}
		return modelModality{}, false
	}

	// System-defined inference profiles (cross-region; e.g. us.amazon.nova-micro-v1:0).
	// These are the IDs required for models that mandate an inference profile.
	ipOut, err := c.controlPlane().ListInferenceProfiles(ctx, &bedrock.ListInferenceProfilesInput{
		TypeEquals: bedrocktypes.InferenceProfileTypeSystemDefined,
	})
	if err != nil {
		// Non-fatal: some regions may not support this API yet.
		ipOut = &bedrock.ListInferenceProfilesOutput{}
	}
	for _, p := range ipOut.InferenceProfileSummaries {
		id := aws.ToString(p.InferenceProfileId)
		if id == "" || seen[id] {
			continue
		}
		// Drop image/embedding/other non-text profiles by checking the backing model.
		if mod, known := modalityOf(baseModelID(id)); known && (!mod.textIn || !mod.textOut) {
			continue
		}
		label := aws.ToString(p.InferenceProfileName)
		if label == "" {
			label = id
		}
		seen[id] = true
		base := baseModelID(id)
		opts = append(opts, ModelOption{
			ID:                  id,
			Label:               label,
			InputCostPer1M:      cat.inputCostPer1M(base),
			OutputCostPer1M:     cat.outputCostPer1M(base),
			FlexCostPer1M:       cat.flexCostPer1M(base),
			FlexOutputCostPer1M: cat.flexOutputCostPer1M(base),
			ProfileRegion:       profileRegion(id),
			Flex:                cat.isFlexCapable(base),
		})
	}

	// Sort cheapest input cost first; unpriced (CostUnknown) sinks to the bottom,
	// tie-broken by label for a stable order.
	sort.Slice(opts, func(i, j int) bool {
		return costLess(opts[i], opts[j], func(m ModelOption) float64 { return m.InputCostPer1M })
	})
	return opts, nil
}

// costLess orders two models by cost ascending; unpriced (CostUnknown) sinks to the bottom,
// tie-broken by label for a stable order. cost extracts the price to compare (InputCostPer1M or
// FlexCostPer1M) — shared by ListAvailableModels' standard-cost sort and SortModelsByFlexCost.
func costLess(a, b ModelOption, cost func(ModelOption) float64) bool {
	ca, cb := cost(a), cost(b)
	aUnknown, bUnknown := ca < 0, cb < 0
	if aUnknown != bUnknown {
		return !aUnknown // known prices before unknown
	}
	if !aUnknown && ca != cb {
		return ca < cb
	}
	return a.Label < b.Label
}

// SortModelsByFlexCost returns a copy of opts ordered by flex-tier input cost — cheapest first,
// unpriced (CostUnknown) flex cost last, tie-broken by label. ListAvailableModels sorts its
// result by standard on-demand cost (InputCostPer1M); the Settings UI's Flex dropdown needs its
// own ordering by FlexCostPer1M so a flex-capable model with no published flex price sinks to
// the bottom of that list rather than inheriting an arbitrary position from the standard sort.
func SortModelsByFlexCost(opts []ModelOption) []ModelOption {
	sorted := make([]ModelOption, len(opts))
	copy(sorted, opts)
	sort.Slice(sorted, func(i, j int) bool {
		return costLess(sorted[i], sorted[j], func(m ModelOption) float64 { return m.FlexCostPer1M })
	})
	return sorted
}

// ============================================================
// Internal helpers
// ============================================================

func extractText(output types.ConverseOutput) string {
	if output == nil {
		return ""
	}
	msg, ok := output.(*types.ConverseOutputMemberMessage)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, block := range msg.Value.Content {
		if t, ok := block.(*types.ContentBlockMemberText); ok {
			sb.WriteString(t.Value)
		}
	}
	return sb.String()
}
