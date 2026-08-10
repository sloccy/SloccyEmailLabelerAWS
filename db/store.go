package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// mustMarshalMap marshals v (a model struct with dynamodbav tags — string/int64/pointer
// fields only) to a DynamoDB item map. Panics on error: for these fixed, compiler-checked
// struct shapes, a marshal failure can only mean a programming mistake (an unsupported
// field type), not something data-dependent at runtime — the same class of "cannot fail"
// already assumed by the JSON marshal-with-fallback calls elsewhere in this codebase.
func mustMarshalMap(v any) map[string]types.AttributeValue {
	item, err := attributevalue.MarshalMap(v)
	if err != nil {
		panic(fmt.Sprintf("marshal %T: %v", v, err))
	}
	return item
}

// keyedItem marshals v (a model struct with dynamodbav tags) and stamps it with the
// DynamoDB key attributes, plus a TTL attribute when ttl > 0. Shared by the per-entity
// xToItem builders below so the PK/SK/TTL stamping can't drift between them.
func keyedItem(v any, pk, sk string, ttl int64) map[string]types.AttributeValue {
	item := mustMarshalMap(v)
	item["PK"] = sv(pk)
	item["SK"] = sv(sk)
	if ttl > 0 {
		item[attrTTL] = nv(ttl)
	}
	return item
}

// unmarshalItem unmarshals a DynamoDB item into T, logging (not returning) any error —
// matching the existing itemToX helpers' "best effort, zero value on failure" behavior,
// since callers only ever pass items this same package wrote via mustMarshalMap/keyedItem.
func unmarshalItem[T any](it map[string]types.AttributeValue) T {
	var v T
	if err := attributevalue.UnmarshalMap(it, &v); err != nil {
		slog.Error("unmarshal item", "type", fmt.Sprintf("%T", v), "err", err)
	}
	return v
}

// getKeyedItem fetches the item at (pk, sk), returning a nil map (no error) if it doesn't
// exist — callers decide their own not-found error and what to extract from the item.
// Shared by the Get* methods below to remove the repeated GetItemInput boilerplate.
func (s *Store) getKeyedItem(ctx context.Context, pk, sk string) (map[string]types.AttributeValue, error) {
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv(pk),
			"SK": sv(sk),
		},
	})
	if err != nil {
		return nil, err
	}
	return out.Item, nil
}

// updateItem issues an UpdateItem call at (pk, sk) with the given SET expression, names, and
// values — shared by the single/multi-attribute update methods below to remove the repeated
// UpdateItemInput boilerplate.
func (s *Store) updateItem(ctx context.Context, pk, sk, expr string, names map[string]string, vals map[string]types.AttributeValue) error {
	_, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv(pk),
			"SK": sv(sk),
		},
		UpdateExpression:          aws.String(expr),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: vals,
	})
	return err
}

// ssmAPI is the subset of the SSM client used for per-account OAuth token storage —
// an interface so tests can substitute an in-memory implementation.
type ssmAPI interface {
	GetParameter(ctx context.Context, in *ssm.GetParameterInput, opts ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	PutParameter(ctx context.Context, in *ssm.PutParameterInput, opts ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
	DeleteParameter(ctx context.Context, in *ssm.DeleteParameterInput, opts ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error)
}

// Store wraps a DynamoDB client. All methods are safe for concurrent use.
type Store struct {
	ddb   *dynamodb.Client
	table string
	// ssm holds per-account Gmail OAuth tokens as SecureString parameters (see
	// tokenParamName). Tokens deliberately live outside the DynamoDB table so that
	// table read access, exports, and backups never expose them — decrypting a token
	// additionally requires ssm:GetParameter on /ollamail/accounts/*.
	ssm ssmAPI
}

// Open constructs a Store.
func Open() (*Store, error) {
	table := os.Getenv("DDB_TABLE")
	if table == "" {
		table = "ollamail"
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return &Store{ddb: dynamodb.NewFromConfig(cfg), table: table, ssm: ssm.NewFromConfig(cfg)}, nil
}

// tsLayout is the standard timestamp format used for Now() and for formatting other
// time.Time values into the same sortable "ts" string form (e.g. a query's since bound).
const tsLayout = "2006-01-02 15:04:05"

// Now returns the current UTC time in the standard timestamp format.
func Now() string {
	return time.Now().UTC().Format(tsLayout)
}

// ============================================================
// DynamoDB item helpers
// ============================================================

func sv(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
func nv(v int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}

// i32 clamps an int64 into the int32 range (DynamoDB Limit fields are int32).
func i32(v int64) int32 {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	if v < math.MinInt32 {
		return math.MinInt32
	}
	return int32(v)
}

// ptr returns a pointer to v — for building *string/*int64 field literals from computed
// values (Go has no address-of operator for non-addressable expressions like a struct
// field or map value).
func ptr[T any](v T) *T { return &v }

// nullInt64Ptr converts the sql.NullInt64 "optional filter/DTO value" idiom (still used
// for params structs and query filters throughout this file) into the *int64 form used
// by attributevalue-tagged model fields.
func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	val := v.Int64
	return &val
}

func getStr(m map[string]types.AttributeValue, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(*types.AttributeValueMemberS); ok {
			return s.Value
		}
	}
	return ""
}

func getInt64(m map[string]types.AttributeValue, key string) int64 {
	if v, ok := m[key]; ok {
		if n, ok := v.(*types.AttributeValueMemberN); ok {
			i, _ := strconv.ParseInt(n.Value, 10, 64)
			return i
		}
	}
	return 0
}

func getNullInt64(m map[string]types.AttributeValue, key string) sql.NullInt64 {
	if v, ok := m[key]; ok {
		if n, ok := v.(*types.AttributeValueMemberN); ok {
			i, _ := strconv.ParseInt(n.Value, 10, 64)
			return sql.NullInt64{Int64: i, Valid: true}
		}
	}
	return sql.NullInt64{}
}

// padID zero-pads an int64 ID to 20 digits for lexicographic ordering in SK.
func padID(id int64) string { return fmt.Sprintf("%020d", id) }

// tsKey builds a sort key from a timestamp and ID (both sort correctly as strings).
func tsKey(ts string, id int64) string { return ts + "#" + padID(id) }

// Partition-key builders for per-account item collections, factored out so the prefix
// string is defined once instead of repeated at every fmt.Sprintf call site.
func pkHistory(accountID int64) string        { return fmt.Sprintf("HIST#%d", accountID) }
func pkProcessed(accountID int64) string      { return fmt.Sprintf("PROC#%d", accountID) }
func pkLabelRetention(accountID int64) string { return fmt.Sprintf("LBL_RET#%d", accountID) }
func pkLabelExemption(accountID int64) string { return fmt.Sprintf("LBL_EX#%d", accountID) }
func pkExample(promptID int64) string         { return fmt.Sprintf("EXAMPLE#%d", promptID) }

// exampleSK builds a PromptExample sort key with the verdict as a prefix, so
// ListExamplesByVerdict can scope a Query's KeyConditionExpression to begins_with(SK,
// verdict+"#") and read only that verdict's newest items instead of the whole partition.
func exampleSK(verdict, ts string, id int64) string { return verdict + "#" + tsKey(ts, id) }

// ============================================================
// Atomic counter (replaces AUTOINCREMENT)
// ============================================================

func (s *Store) nextID(ctx context.Context, entity string) (int64, error) {
	return s.nextIDs(ctx, entity, 1)
}

// nextIDs atomically reserves n sequential ids from entity's counter in one round trip
// and returns the first of them (the rest are start, start+1, ..., start+n-1). Used to
// batch-allocate ids for a group of items instead of one UpdateItem call per item.
func (s *Store) nextIDs(ctx context.Context, entity string, n int) (start int64, err error) {
	out, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("META"),
			"SK": sv("COUNTER#" + entity),
		},
		UpdateExpression: aws.String("ADD seq :n"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":n": nv(int64(n)),
		},
		ReturnValues: types.ReturnValueUpdatedNew,
	})
	if err != nil {
		return 0, err
	}
	if v, ok := out.Attributes["seq"]; ok {
		if nv, ok := v.(*types.AttributeValueMemberN); ok {
			end, perr := strconv.ParseInt(nv.Value, 10, 64)
			if perr != nil {
				return 0, perr
			}
			return end - int64(n) + 1, nil
		}
	}
	return 0, errors.New("counter response missing seq")
}

// localIDSeqBits sizes the per-process sequence counter packed into the low bits of a
// localID(): 20 bits gives ~1M distinct ids per millisecond within one process, several
// orders of magnitude beyond this app's actual write rate.
const localIDSeqBits = 20

var localIDSeq atomic.Uint32

// localID generates a process-local, time-ordered, collision-safe int64 without a
// DynamoDB round trip: a millisecond timestamp (top bits) plus an atomic per-process
// sequence (low localIDSeqBits bits) to dedupe ids minted within the same millisecond.
// Used by logs and history — both are keyed by tsKey(ts, id), where the timestamp prefix
// already carries the real sort order, so the id only needs to be unique, not strictly
// ordered; this generator's rough time-ordering is a free bonus, not a requirement.
//
// Scoped to logs/history only: other entities either round-trip their id through
// client-side JS as a JSON number (prompts, static/app.js's reorder feature — unsafe
// above 2^53, which a time-based id routinely exceeds) or rely on "highest id = most
// recent" for actual ordering (suggestions, llm_debug) rather than a stored timestamp, so
// they stay on the nextID/nextIDs counter.
func localID() int64 {
	ms := time.Now().UnixMilli()
	seq := int64(localIDSeq.Add(1)) & (1<<localIDSeqBits - 1)
	return ms<<localIDSeqBits | seq
}

// localIDs generates n distinct localID() values.
func localIDs(n int) []int64 {
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = localID()
	}
	return ids
}

// ============================================================
// Query helpers
// ============================================================

// queryAll runs a Query, following pagination, returning all matching items.
func (s *Store) queryAll(ctx context.Context, input *dynamodb.QueryInput) ([]map[string]types.AttributeValue, error) {
	var items []map[string]types.AttributeValue
	for {
		out, err := s.ddb.Query(ctx, input)
		if err != nil {
			return nil, err
		}
		items = append(items, out.Items...)
		if out.LastEvaluatedKey == nil {
			break
		}
		input.ExclusiveStartKey = out.LastEvaluatedKey
	}
	return items, nil
}

// batchDelete deletes items in batches of 25.
func (s *Store) batchDelete(ctx context.Context, keys []map[string]types.AttributeValue) error {
	for i := 0; i < len(keys); i += 25 {
		end := min(i+25, len(keys))
		batch := keys[i:end]
		reqs := make([]types.WriteRequest, len(batch))
		for j, k := range batch {
			reqs[j] = types.WriteRequest{DeleteRequest: &types.DeleteRequest{Key: k}}
		}
		_, err := s.ddb.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
			RequestItems: map[string][]types.WriteRequest{s.table: reqs},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// deleteAllByPK deletes every item under one partition key.
func (s *Store) deleteAllByPK(ctx context.Context, pk string) error {
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK: sv(pk),
		},
		ProjectionExpression: aws.String("PK, SK"),
	})
	if err != nil {
		return err
	}
	keys := make([]map[string]types.AttributeValue, len(items))
	for i, it := range items {
		keys[i] = map[string]types.AttributeValue{"PK": it["PK"], "SK": it["SK"]}
	}
	return s.batchDelete(ctx, keys)
}

// batchPut writes items in batches of 25.
func (s *Store) batchPut(ctx context.Context, items []map[string]types.AttributeValue) error {
	for i := 0; i < len(items); i += 25 {
		end := min(i+25, len(items))
		batch := items[i:end]
		reqs := make([]types.WriteRequest, len(batch))
		for j, it := range batch {
			reqs[j] = types.WriteRequest{PutRequest: &types.PutRequest{Item: it}}
		}
		_, err := s.ddb.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
			RequestItems: map[string][]types.WriteRequest{s.table: reqs},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// ============================================================
// TTL helper: epoch seconds N days from now
// ============================================================

func ttlDays(days int) int64 {
	return time.Now().UTC().AddDate(0, 0, days).Unix()
}

// logHistoryTTLDays is the item-level TTL for logs, history, and LLM-debug rows —
// DynamoDB's own TTL sweep enforces the retention policy directly, no scan+delete
// pass needed. Initialized from the same LOG_RETENTION_DAYS env template.yaml feeds
// the app config, so a retention override at deploy time propagates here instead of
// drifting from a hardcoded copy of its default.
var logHistoryTTLDays = envDaysOr("LOG_RETENTION_DAYS", 30)

// suggestionTTLDays bounds prompt-suggestion items, which snapshot a full email body
// (EmailBodySnapshot). Longer than log retention because pending suggestions are
// user-facing action items — but a suggestion unactioned for this long is stale, and
// email content shouldn't outlive it.
const suggestionTTLDays = 90

func envDaysOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// ============================================================
// Settings
// ============================================================

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	item, err := s.getKeyedItem(ctx, "META", "SETTING#"+key)
	if err != nil {
		return "", err
	}
	if item == nil {
		return "", fmt.Errorf("setting not found: %s", key)
	}
	return getStr(item, "val"), nil
}

func (s *Store) SetSetting(ctx context.Context, arg SetSettingParams) error {
	_, err := s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]types.AttributeValue{
			"PK":  sv("META"),
			"SK":  sv("SETTING#" + arg.Key),
			"val": sv(arg.Value),
		},
	})
	return err
}

func (s *Store) SeedSetting(key, value string) error {
	_, err := s.ddb.PutItem(context.Background(), &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		ConditionExpression: aws.String("attribute_not_exists(PK)"),
		Item: map[string]types.AttributeValue{
			"PK":  sv("META"),
			"SK":  sv("SETTING#" + key),
			"val": sv(value),
		},
	})
	if err != nil {
		var ccf *types.ConditionalCheckFailedException
		if isConditionFailed(err, &ccf) {
			return nil // already seeded
		}
		return err
	}
	return nil
}

func (s *Store) GetAllSettings(ctx context.Context) ([]Setting, error) {
	out, err := s.ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :prefix)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK:    sv("META"),
			":prefix": sv("SETTING#"),
		},
	})
	if err != nil {
		return nil, err
	}
	items := make([]Setting, 0, len(out.Items))
	for _, it := range out.Items {
		key := strings.TrimPrefix(getStr(it, "SK"), "SETTING#")
		items = append(items, Setting{Key: key, Value: getStr(it, "val")})
	}
	return items, nil
}

// ============================================================
// Logs
// ============================================================

func (s *Store) Log(level, message string) {
	_ = s.AddLog(context.Background(), AddLogParams{Level: level, Message: message})
}

func logItem(id int64, ts string, arg LogEntry) map[string]types.AttributeValue {
	return keyedItem(Log{ID: id, Timestamp: ts, Level: arg.Level, Message: arg.Message}, "LOG", tsKey(ts, id), ttlDays(logHistoryTTLDays))
}

func itemToLog(it map[string]types.AttributeValue) Log { return unmarshalItem[Log](it) }

func (s *Store) AddLog(ctx context.Context, arg AddLogParams) error {
	_, err := s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      logItem(localID(), Now(), arg),
	})
	return err
}

func (s *Store) GetLogs(ctx context.Context, limit int64) ([]Log, error) {
	out, err := s.ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK: sv("LOG"),
		},
		ScanIndexForward: aws.Bool(false),
		Limit:            aws.Int32(i32(limit)),
	})
	if err != nil {
		return nil, err
	}
	logs := make([]Log, 0, len(out.Items))
	for _, it := range out.Items {
		logs = append(logs, itemToLog(it))
	}
	return logs, nil
}

// CountLogsByLevel returns how many logs at the given level were recorded at or after
// since — used for the dashboard's rolling 30-day Bedrock-timeout counter.
func (s *Store) CountLogsByLevel(ctx context.Context, level string, since time.Time) (int64, error) {
	sinceKey := tsKey(since.UTC().Format(tsLayout), 0)
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk AND SK >= :since"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK:   sv("LOG"),
			":since": sv(sinceKey),
		},
	})
	if err != nil {
		return 0, err
	}
	var count int64
	for _, it := range items {
		if getStr(it, "level") == level {
			count++
		}
	}
	return count, nil
}

func (s *Store) GetLogsRange(ctx context.Context, arg GetLogsRangeParams) ([]Log, error) {
	out, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk AND SK BETWEEN :lo AND :hi"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK: sv("LOG"),
			":lo":  sv(arg.Timestamp),
			":hi":  sv(arg.Timestamp2 + "\xff"),
		},
		ScanIndexForward: aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	logs := make([]Log, 0, len(out))
	for _, it := range out {
		logs = append(logs, itemToLog(it))
	}
	return logs, nil
}

// ============================================================
// Accounts
// ============================================================

// ============================================================
// Account OAuth tokens (SSM SecureString side-channel)
//
// Gmail refresh tokens are the crown jewel — durable gmail.modify access to the
// whole mailbox — so they never touch the DynamoDB table: Account.CredentialsJSON
// is tagged dynamodbav:"-" (never marshaled), and reads hydrate it from an SSM
// SecureString at /ollamail/accounts/<id>/token (encrypted with the AWS-managed
// aws/ssm key; IAM scoped in template.yaml).
// ============================================================

func tokenParamName(id int64) string {
	return "/ollamail/accounts/" + strconv.FormatInt(id, 10) + "/token"
}

// getAccountToken returns the account's OAuth token JSON, or "" when no token has
// been stored (e.g. a placeholder account awaiting OAuth).
func (s *Store) getAccountToken(ctx context.Context, id int64) (string, error) {
	out, err := s.ssm.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(tokenParamName(id)),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		var notFound *ssmtypes.ParameterNotFound
		if errors.As(err, &notFound) {
			return "", nil
		}
		return "", fmt.Errorf("get account token %d: %w", id, err)
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return "", nil
	}
	return *out.Parameter.Value, nil
}

func (s *Store) putAccountToken(ctx context.Context, id int64, tokenJSON string) error {
	_, err := s.ssm.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(tokenParamName(id)),
		Type:      ssmtypes.ParameterTypeSecureString,
		Value:     aws.String(tokenJSON),
		Overwrite: aws.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("put account token %d: %w", id, err)
	}
	return nil
}

func (s *Store) deleteAccountToken(ctx context.Context, id int64) error {
	_, err := s.ssm.DeleteParameter(ctx, &ssm.DeleteParameterInput{
		Name: aws.String(tokenParamName(id)),
	})
	if err != nil {
		var notFound *ssmtypes.ParameterNotFound
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("delete account token %d: %w", id, err)
	}
	return nil
}

// hydrateAccountToken fills a.CredentialsJSON from SSM. (Rows written before the SSM
// scheme carried a plaintext creds attribute; those were lazily migrated in July 2026
// and the attribute is additionally excluded from (un)marshaling via the model's
// dynamodbav:"-" tag, so any stray copy is inert.)
func (s *Store) hydrateAccountToken(ctx context.Context, a *Account) error {
	token, err := s.getAccountToken(ctx, a.ID)
	if err != nil {
		return err
	}
	a.CredentialsJSON = token
	return nil
}

func accountItem(a Account) map[string]types.AttributeValue {
	// Tokens live in SSM (see hydrateAccountToken); CredentialsJSON carries
	// dynamodbav:"-" so it can never be persisted to the table.
	return keyedItem(a, "ACCOUNT", padID(a.ID), 0)
}

func itemToAccount(it map[string]types.AttributeValue) Account { return unmarshalItem[Account](it) }

func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK: sv("ACCOUNT"),
		},
	})
	if err != nil {
		return nil, err
	}
	accs := make([]Account, len(items))
	for i, it := range items {
		accs[i] = itemToAccount(it)
		if err := s.hydrateAccountToken(ctx, &accs[i]); err != nil {
			return nil, err
		}
	}
	sort.Slice(accs, func(i, j int) bool { return accs[i].AddedAt > accs[j].AddedAt })
	return accs, nil
}

func (s *Store) ListAccountsSafe(ctx context.Context) ([]ListAccountsSafeRow, error) {
	accs, err := s.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]ListAccountsSafeRow, len(accs))
	for i, a := range accs {
		rows[i] = ListAccountsSafeRow{
			ID:         a.ID,
			Email:      a.Email,
			AddedAt:    a.AddedAt,
			LastScanAt: a.LastScanAt,
			Active:     a.Active,
		}
	}
	return rows, nil
}

func (s *Store) GetAccount(ctx context.Context, id int64) (Account, error) {
	item, err := s.getKeyedItem(ctx, "ACCOUNT", padID(id))
	if err != nil {
		return Account{}, err
	}
	if item == nil {
		return Account{}, fmt.Errorf("account not found: %d", id)
	}
	a := itemToAccount(item)
	if err := s.hydrateAccountToken(ctx, &a); err != nil {
		return Account{}, err
	}
	return a, nil
}

func (s *Store) GetAccountByEmail(ctx context.Context, email string) (int64, error) {
	item, err := s.getKeyedItem(ctx, "ACCT_EMAIL#"+email, "0")
	if err != nil {
		return 0, err
	}
	if item == nil {
		return 0, fmt.Errorf("account not found: %s", email)
	}
	return getInt64(item, attrAccountID), nil
}

func (s *Store) UpsertAccount(ctx context.Context, arg UpsertAccountParams) (int64, error) {
	// Try to get existing ID first
	existing, err := s.GetAccountByEmail(ctx, arg.Email)
	if err == nil {
		// Update existing.
		a, err2 := s.GetAccount(ctx, existing)
		if err2 != nil {
			return 0, err2
		}
		a.Active = 1
		if _, err3 := s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(s.table),
			Item:      accountItem(a),
		}); err3 != nil {
			return 0, err3
		}
		// Empty credentials (e.g. CreateAccountPlaceholder re-upserting an existing
		// email) must not clobber a stored token.
		if arg.CredentialsJSON != "" {
			if err3 := s.putAccountToken(ctx, existing, arg.CredentialsJSON); err3 != nil {
				return 0, err3
			}
		}
		return existing, nil
	}
	// Create new
	id, err := s.nextID(ctx, "accounts")
	if err != nil {
		return 0, err
	}
	a := Account{
		ID:      id,
		Email:   arg.Email,
		AddedAt: Now(),
		Active:  1,
	}
	// Write account item + email guard in a transaction
	_, err = s.ddb.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{Put: &types.Put{
				TableName: aws.String(s.table),
				Item:      accountItem(a),
			}},
			{Put: &types.Put{
				TableName:           aws.String(s.table),
				ConditionExpression: aws.String("attribute_not_exists(PK)"),
				Item: map[string]types.AttributeValue{
					"PK":          sv("ACCT_EMAIL#" + arg.Email),
					"SK":          sv("0"),
					attrAccountID: nv(id),
				},
			}},
		},
	})
	if err != nil {
		return 0, err
	}
	// Token written only after the account row exists — a failed transaction (e.g.
	// losing the email-guard race) must not leave an orphaned SSM secret behind.
	if arg.CredentialsJSON != "" {
		if err := s.putAccountToken(ctx, id, arg.CredentialsJSON); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func (s *Store) CreateAccountPlaceholder(ctx context.Context, email string) (int64, error) {
	// Return existing id if email already exists
	if id, err := s.GetAccountByEmail(ctx, email); err == nil {
		return id, nil
	}
	return s.UpsertAccount(ctx, UpsertAccountParams{Email: email, CredentialsJSON: ""})
}

func (s *Store) UpdateAccountCredentials(ctx context.Context, arg UpdateAccountCredentialsParams) error {
	return s.putAccountToken(ctx, arg.ID, arg.CredentialsJSON)
}

func (s *Store) UpdateLastScan(ctx context.Context, id int64) error {
	return s.updateItem(ctx, "ACCOUNT", padID(id), "SET lastScan = :ts", nil, map[string]types.AttributeValue{
		":ts": sv(Now()),
	})
}

// UpdateAccountWatch advances the account's stored Gmail history-id watermark.
// Conditioned to only ever move it forward: two pushes for the same account can race
// (push.go has no cross-invocation lock around this update), and an unconditional SET
// would let the slower one clobber a newer id with a stale one, causing re-listing of
// already-processed history on the next pass.
//
// The advance-only check is done on watchHistNum, a second N-type attribute written
// alongside the S-type watchHist everything else reads (Account.WatchHistoryID,
// push.go's strconv.ParseUint, gmailpkg.ListHistoryAddedMessageIDs) — not on watchHist
// itself. Gmail history ids are decimal strings that grow a digit within weeks to months
// of active use (they increment on every mailbox change, not just new mail), and
// DynamoDB compares S-type attributes lexicographically: at a rollover like "9999999" ->
// "10000000", "watchHist < :h" would evaluate true even though "10000000" is the larger,
// newer id, permanently rejecting every future advance. watchHistNum sidesteps that by
// comparing as DynamoDB N (numeric) instead. Existing accounts have no watchHistNum yet,
// so attribute_not_exists(watchHistNum) is true on their first write here — same "never
// seen" idiom ClaimMessages uses for attrLeaseExp — seeding it correctly with no
// migration step.
func (s *Store) UpdateAccountWatch(ctx context.Context, arg UpdateAccountWatchParams) error {
	hn, perr := strconv.ParseUint(arg.HistoryID, 10, 64)
	if perr != nil {
		return fmt.Errorf("invalid history id %q: %w", arg.HistoryID, perr)
	}
	if hn > math.MaxInt64 {
		return fmt.Errorf("history id %q exceeds int64 range", arg.HistoryID)
	}
	_, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("ACCOUNT"),
			"SK": sv(padID(arg.ID)),
		},
		UpdateExpression: aws.String("SET watchHist = :h, watchHistNum = :hn, watchExp = :e"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":h":  sv(arg.HistoryID),
			":hn": nv(int64(hn)), // bounds-checked above
			":e":  nv(arg.Expiration),
		},
		ConditionExpression: aws.String("attribute_not_exists(watchHistNum) OR watchHistNum < :hn"),
	})
	if err != nil {
		var ccf *types.ConditionalCheckFailedException
		if isConditionFailed(err, &ccf) {
			return nil // a newer history id is already stored; ignore the stale advance
		}
		return err
	}
	return nil
}

func (s *Store) ToggleAccount(ctx context.Context, id int64) (int64, error) {
	a, err := s.GetAccount(ctx, id)
	if err != nil {
		return 0, err
	}
	a.Active = 1 - a.Active
	if _, err := s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      accountItem(a),
	}); err != nil {
		return 0, err
	}
	return a.Active, nil
}

func (s *Store) DeleteAccount(ctx context.Context, id int64) error {
	a, err := s.GetAccount(ctx, id)
	if err != nil {
		return err
	}
	if err := s.deleteAccountToken(ctx, id); err != nil {
		return err
	}
	_, err = s.ddb.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{Delete: &types.Delete{
				TableName: aws.String(s.table),
				Key:       map[string]types.AttributeValue{"PK": sv("ACCOUNT"), "SK": sv(padID(id))},
			}},
			{Delete: &types.Delete{
				TableName: aws.String(s.table),
				Key:       map[string]types.AttributeValue{"PK": sv("ACCT_EMAIL#" + a.Email), "SK": sv("0")},
			}},
		},
	})
	return err
}

// DeleteAccountCascade removes all data associated with an account.
func (s *Store) DeleteAccountCascade(ctx context.Context, accountID int64) error {
	// Delete prompts by account
	if err := s.DeletePromptsByAccount(ctx, sql.NullInt64{Int64: accountID, Valid: true}); err != nil {
		return err
	}
	// Delete history
	if err := s.DeleteHistoryByAccount(ctx, accountID); err != nil {
		return err
	}
	// Delete retention
	if err := s.DeleteAccountRetention(ctx, accountID); err != nil {
		return err
	}
	if err := s.DeleteLabelRetentionByAccount(ctx, accountID); err != nil {
		return err
	}
	if err := s.DeleteLabelExemptionsByAccount(ctx, accountID); err != nil {
		return err
	}
	// Delete processed emails
	if err := s.DeleteProcessedEmailsByAccount(ctx, accountID); err != nil {
		return err
	}
	// Delete account (and email guard)
	return s.DeleteAccount(ctx, accountID)
}

// ============================================================
// Prompts
// ============================================================

func itemToPrompt(it map[string]types.AttributeValue) Prompt { return unmarshalItem[Prompt](it) }

func promptToItem(p Prompt) map[string]types.AttributeValue {
	return keyedItem(p, "PROMPT", padID(p.ID), 0)
}

func (s *Store) listAllPrompts(ctx context.Context) ([]Prompt, error) {
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK: sv("PROMPT"),
		},
	})
	if err != nil {
		return nil, err
	}
	prompts := make([]Prompt, len(items))
	for i, it := range items {
		prompts[i] = itemToPrompt(it)
	}
	sort.Slice(prompts, func(i, j int) bool {
		if prompts[i].SortOrder != prompts[j].SortOrder {
			return prompts[i].SortOrder < prompts[j].SortOrder
		}
		return prompts[i].ID < prompts[j].ID
	})
	return prompts, nil
}

func (s *Store) ListPrompts(ctx context.Context) ([]Prompt, error) {
	return s.listAllPrompts(ctx)
}

func (s *Store) ListPromptsByAccount(ctx context.Context, accountID sql.NullInt64) ([]Prompt, error) {
	all, err := s.listAllPrompts(ctx)
	if err != nil {
		return nil, err
	}
	if !accountID.Valid {
		return all, nil
	}
	var filtered []Prompt
	for _, p := range all {
		if p.AccountID == nil || *p.AccountID == accountID.Int64 {
			filtered = append(filtered, p)
		}
	}
	return filtered, nil
}

func (s *Store) ListActivePrompts(ctx context.Context) ([]Prompt, error) {
	all, err := s.listAllPrompts(ctx)
	if err != nil {
		return nil, err
	}
	var active []Prompt
	for _, p := range all {
		if p.Active == 1 {
			active = append(active, p)
		}
	}
	return active, nil
}

func (s *Store) ListActivePromptsByAccount(ctx context.Context, accountID sql.NullInt64) ([]Prompt, error) {
	all, err := s.listAllPrompts(ctx)
	if err != nil {
		return nil, err
	}
	var filtered []Prompt
	for _, p := range all {
		if p.Active == 0 {
			continue
		}
		if accountID.Valid && p.AccountID != nil && *p.AccountID != accountID.Int64 {
			continue
		}
		filtered = append(filtered, p)
	}
	return filtered, nil
}

// ListActivePromptsForAccount returns the active prompts scoped to accountID, or every
// active prompt when accountID is 0 (the "no specific account" case some history rows
// have). Small wrapper so callers don't hand-roll the ListActivePromptsByAccount vs.
// ListActivePrompts branch themselves.
func (s *Store) ListActivePromptsForAccount(ctx context.Context, accountID int64) ([]Prompt, error) {
	if accountID != 0 {
		return s.ListActivePromptsByAccount(ctx, sql.NullInt64{Int64: accountID, Valid: true})
	}
	return s.ListActivePrompts(ctx)
}

func (s *Store) GetPrompt(ctx context.Context, id int64) (Prompt, error) {
	item, err := s.getKeyedItem(ctx, "PROMPT", padID(id))
	if err != nil {
		return Prompt{}, err
	}
	if item == nil {
		return Prompt{}, fmt.Errorf("prompt not found: %d", id)
	}
	return itemToPrompt(item), nil
}

func (s *Store) CreatePrompt(ctx context.Context, arg CreatePromptParams) (int64, error) {
	id, err := s.nextID(ctx, "prompts")
	if err != nil {
		return 0, err
	}
	// Mint the initial version before the row's own PutItem below, so that write can carry
	// the resulting CurrentVersionID directly instead of leaving a window where the prompt
	// exists with no current version — see InsertPromptVersion's doc comment on why minting
	// against a prompt row that doesn't exist yet here is safe (a sparse write immediately
	// superseded by the full PutItem). Best-effort: a failure here is logged, not returned —
	// it must not block creating the rule itself, and CurrentVersionID simply stays 0, same
	// as any prompt that predates the version ledger.
	versionID, verr := s.InsertPromptVersion(ctx, InsertPromptVersionParams{
		PromptID: id, Instructions: arg.Instructions, Source: PromptVersionSourceInitial,
	})
	if verr != nil {
		slog.Error("create prompt: insert initial version", "prompt_id", id, "err", verr)
	}
	p := Prompt{
		ID:               id,
		Name:             arg.Name,
		Instructions:     arg.Instructions,
		LabelName:        arg.LabelName,
		Active:           1,
		CreatedAt:        Now(),
		ActionArchive:    arg.ActionArchive,
		ActionSpam:       arg.ActionSpam,
		ActionTrash:      arg.ActionTrash,
		ActionMarkRead:   arg.ActionMarkRead,
		SortOrder:        arg.SortOrder,
		StopProcessing:   arg.StopProcessing,
		AccountID:        nullInt64Ptr(arg.AccountID),
		CurrentVersionID: versionID,
	}
	_, err = s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      promptToItem(p),
	})
	return id, err
}

func (s *Store) UpdatePrompt(ctx context.Context, arg UpdatePromptParams) error {
	p, err := s.GetPrompt(ctx, arg.ID)
	if err != nil {
		return err
	}
	instructionsChanged := p.Instructions != arg.Instructions
	p.Name = arg.Name
	p.Instructions = arg.Instructions
	p.LabelName = arg.LabelName
	p.ActionArchive = arg.ActionArchive
	p.ActionSpam = arg.ActionSpam
	p.ActionTrash = arg.ActionTrash
	p.ActionMarkRead = arg.ActionMarkRead
	p.StopProcessing = arg.StopProcessing
	p.AccountID = nullInt64Ptr(arg.AccountID)
	if instructionsChanged {
		// Only mint a version when the rule text itself changed — not on every edit
		// (renaming the rule, changing its actions, moving accounts). Mint before the
		// PutItem below, same reasoning as CreatePrompt, so this write carries the fresh
		// CurrentVersionID directly rather than a separate trailing write. Best-effort:
		// a failure here must not block a user-requested edit; CurrentVersionID just stays
		// wherever it was, matching CreatePrompt's failure handling above.
		versionID, verr := s.InsertPromptVersion(ctx, InsertPromptVersionParams{
			PromptID: arg.ID, Instructions: arg.Instructions, Source: PromptVersionSourceManual,
		})
		if verr != nil {
			slog.Error("update prompt: insert version", "prompt_id", arg.ID, "err", verr)
		} else {
			p.CurrentVersionID = versionID
		}
	}
	_, err = s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      promptToItem(p),
	})
	return err
}

// DeletePrompt removes the prompt itself and its example corpus (DeleteExamplesForPrompt) —
// folded in here rather than left to each call site, so a rule can never be deleted while
// leaving an orphaned EXAMPLE# partition behind.
func (s *Store) DeletePrompt(ctx context.Context, id int64) error {
	if err := s.DeleteExamplesForPrompt(ctx, id); err != nil {
		return err
	}
	_, err := s.ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("PROMPT"),
			"SK": sv(padID(id)),
		},
	})
	return err
}

func (s *Store) DeletePromptsByAccount(ctx context.Context, accountID sql.NullInt64) error {
	if !accountID.Valid {
		return nil
	}
	all, err := s.listAllPrompts(ctx)
	if err != nil {
		return err
	}
	var keys []map[string]types.AttributeValue
	for _, p := range all {
		if p.AccountID != nil && *p.AccountID == accountID.Int64 {
			keys = append(keys, map[string]types.AttributeValue{
				"PK": sv("PROMPT"),
				"SK": sv(padID(p.ID)),
			})
			if err := s.DeleteExamplesForPrompt(ctx, p.ID); err != nil {
				return err
			}
		}
	}
	return s.batchDelete(ctx, keys)
}

// ============================================================
// Prompt versions (improve loop's long-term memory)
// ============================================================
//
// See PromptVersion's doc comment (db/models.go) for the schema and why manual edits are
// versioned exactly like applied suggestions. Every write path that changes a Prompt's
// Instructions — CreatePrompt (source "initial"), ApplyPromptSuggestionAndUpdatePrompt
// (source "suggestion"), and UpdatePrompt (source "manual", only when Instructions
// actually changed) — mints a version through InsertPromptVersion, which is also what
// actually writes the new text onto the prompt row; there is no separate
// "just update instructions" path anymore.

func pkPromptVersion(promptID int64) string { return fmt.Sprintf("PVER#%d", promptID) }

// InsertPromptVersionParams captures what a version records at creation — see
// PromptVersion's doc comment for what each field means.
type InsertPromptVersionParams struct {
	PromptID     int64
	Instructions string
	Source       string
	SuggestionID *int64
	ReplayModel  string
	ReplayTotal  int64
	ReplayPassed int64
}

// InsertPromptVersion mints a new PromptVersion row and writes its text onto the prompt
// row as the new live instructions, pointing CurrentVersionID at the version that produced
// them — this is the only place either write happens, and the only place a prompt's
// Instructions ever changes. Not transactional (same non-transactional, best-effort-
// ordered pattern every other multi-step write in this package uses, e.g.
// ApplyPromptSuggestionAndUpdatePrompt): a failure between the two calls leaves an
// orphaned version row, which is harmless, rather than risking the reverse (a prompt
// pointing at a version that was never written). Safe to call on a prompt row that doesn't
// exist yet (CreatePrompt calls this before its own PutItem) — the resulting sparse
// UpdateItem write is immediately superseded by that PutItem.
func (s *Store) InsertPromptVersion(ctx context.Context, arg InsertPromptVersionParams) (int64, error) {
	id, err := s.nextID(ctx, "prompt_versions")
	if err != nil {
		return 0, err
	}
	item := keyedItem(PromptVersion{
		ID:           id,
		PromptID:     arg.PromptID,
		CreatedAt:    Now(),
		Instructions: arg.Instructions,
		Source:       arg.Source,
		SuggestionID: arg.SuggestionID,
		ReplayModel:  arg.ReplayModel,
		ReplayTotal:  arg.ReplayTotal,
		ReplayPassed: arg.ReplayPassed,
	}, pkPromptVersion(arg.PromptID), padID(id), 0)
	if _, err := s.ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(s.table), Item: item}); err != nil {
		return 0, err
	}
	if err := s.updateItem(ctx, "PROMPT", padID(arg.PromptID), "SET currentVersionId = :v, instructions = :i", nil,
		map[string]types.AttributeValue{":v": nv(id), ":i": sv(arg.Instructions)}); err != nil {
		return 0, err
	}
	return id, nil
}

// ListPromptVersions returns a prompt's version history, newest first, capped at limit —
// the improve loop's source for "earlier attempts" context (see improve.go's
// attemptsForPrompt). Uses ddb.Query directly rather than queryAll, same reasoning as
// ListExamplesByVerdict: a bounded read stays cheap regardless of how long a rule's edit
// history grows.
func (s *Store) ListPromptVersions(ctx context.Context, promptID int64, limit int32) ([]PromptVersion, error) {
	out, err := s.ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK: sv(pkPromptVersion(promptID)),
		},
		ScanIndexForward: aws.Bool(false),
		Limit:            aws.Int32(limit),
	})
	if err != nil {
		return nil, err
	}
	result := make([]PromptVersion, len(out.Items))
	for i, it := range out.Items {
		result[i] = unmarshalItem[PromptVersion](it)
	}
	return result, nil
}

// IncrementVersionObserved adds one to a version's ObservedFP or ObservedFN count — called
// when a recategorization records a false_positive/false_negative example, using the
// version that was live at the time (db.PromptExample.PromptVersionID). This is what lets
// a later improve round see not just how a version scored in the replay "lab" but how it
// actually did once real mail started arriving against it. versionID == 0 (an example
// written before the ledger existed, or against a prompt that predates it) is a silent
// no-op, not an error — there's nothing to attribute it to. verdict must be
// VerdictFalsePositive or VerdictFalseNegative; anything else (VerdictConfirmedPositive) is
// also a no-op, since a confirmation isn't a problem to track here. Best-effort, same
// reasoning MarkExamplesResolved's own doc comment gives: this is bookkeeping for a future
// improve round, not the primary effect of recording a correction, so it must never be
// able to block one.
func (s *Store) IncrementVersionObserved(ctx context.Context, promptID, versionID int64, verdict string) {
	if versionID == 0 {
		return
	}
	var attr string
	switch verdict {
	case VerdictFalsePositive:
		attr = "observedFp"
	case VerdictFalseNegative:
		attr = "observedFn"
	default:
		return
	}
	if err := s.updateItem(ctx, pkPromptVersion(promptID), padID(versionID),
		"ADD "+attr+" :one", nil, map[string]types.AttributeValue{":one": nv(1)}); err != nil {
		slog.Error("increment version observed", "prompt_id", promptID, "version_id", versionID, "verdict", verdict, "err", err)
	}
}

func (s *Store) TogglePrompt(ctx context.Context, id int64) (int64, error) {
	p, err := s.GetPrompt(ctx, id)
	if err != nil {
		return 0, err
	}
	p.Active = 1 - p.Active
	_, err = s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      promptToItem(p),
	})
	return p.Active, err
}

func (s *Store) MaxPromptSortOrder(ctx context.Context) (int64, error) {
	all, err := s.listAllPrompts(ctx)
	if err != nil {
		return int64(-1), err
	}
	var maxOrder int64 = -1
	for _, p := range all {
		if p.SortOrder > maxOrder {
			maxOrder = p.SortOrder
		}
	}
	return maxOrder, nil
}

func (s *Store) CountActivePrompts(ctx context.Context) (int64, error) {
	all, err := s.ListActivePrompts(ctx)
	if err != nil {
		return 0, err
	}
	return int64(len(all)), nil
}

func (s *Store) PromptExistsGlobal(ctx context.Context, name string) (int64, error) {
	all, err := s.listAllPrompts(ctx)
	if err != nil {
		return 0, err
	}
	for _, p := range all {
		if p.Name == name && p.AccountID == nil {
			return 1, nil
		}
	}
	return 0, nil
}

// ReorderPrompts updates sort_order for each prompt ID in order.
func (s *Store) ReorderPrompts(ctx context.Context, ids []int64) error {
	// TransactWriteItems caps at 100 items per call, so batch in chunks of that size instead
	// of one UpdateItem round trip per prompt.
	const maxTransactItems = 100
	for i := 0; i < len(ids); i += maxTransactItems {
		end := min(i+maxTransactItems, len(ids))
		batch := ids[i:end]
		items := make([]types.TransactWriteItem, len(batch))
		for j, id := range batch {
			items[j] = types.TransactWriteItem{
				Update: &types.Update{
					TableName: aws.String(s.table),
					Key: map[string]types.AttributeValue{
						"PK": sv("PROMPT"),
						"SK": sv(padID(id)),
					},
					UpdateExpression: aws.String("SET sortOrder = :s"),
					ExpressionAttributeValues: map[string]types.AttributeValue{
						":s": nv(int64(i + j)),
					},
				},
			}
		}
		if _, err := s.ddb.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
			TransactItems: items,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ============================================================
// Categorization History
// ============================================================

func itemToHistory(it map[string]types.AttributeValue) CategorizationHistory {
	return unmarshalItem[CategorizationHistory](it)
}

// historyItem builds a history item from id/ts (allocated by the caller) plus a
// HistoryEntry write DTO. ttl isn't part of CategorizationHistory (it's DynamoDB-internal
// expiry, never read back into the model), so it's added after marshaling.
func historyItem(id int64, ts string, arg HistoryEntry) map[string]types.AttributeValue {
	return keyedItem(CategorizationHistory{
		ID:           id,
		Timestamp:    ts,
		AccountID:    arg.AccountID,
		AccountEmail: arg.AccountEmail,
		MessageID:    arg.MessageID,
		Subject:      arg.Subject,
		Sender:       arg.Sender,
		PromptID:     arg.PromptID,
		PromptName:   arg.PromptName,
		LabelName:    arg.LabelName,
		Actions:      arg.Actions,
		LlmResponse:  arg.LlmResponse,
		DurationMs:   arg.DurationMs,
	}, pkHistory(arg.AccountID), tsKey(ts, id), ttlDays(logHistoryTTLDays))
}

func (s *Store) AddHistory(ctx context.Context, arg AddHistoryParams) error {
	_, err := s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      historyItem(localID(), Now(), arg),
	})
	return err
}

func (s *Store) GetHistoryRow(ctx context.Context, id int64) (CategorizationHistory, error) {
	// We need to scan to find by ID (no GSI). For this personal-scale app,
	// query all accounts and find the row. Callers only use this for single rows.
	accs, err := s.ListAccounts(ctx)
	if err != nil {
		return CategorizationHistory{}, err
	}
	for _, acc := range accs {
		items, err := s.queryAll(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(s.table),
			KeyConditionExpression: aws.String("PK = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				exprPK: sv(pkHistory(acc.ID)),
			},
		})
		if err != nil {
			continue
		}
		for _, it := range items {
			if getInt64(it, "id") == id {
				return itemToHistory(it), nil
			}
		}
	}
	return CategorizationHistory{}, fmt.Errorf("history row not found: %d", id)
}

func (s *Store) GetHistoryLlmResponse(ctx context.Context, id int64) (string, error) {
	row, err := s.GetHistoryRow(ctx, id)
	if err != nil {
		return "", err
	}
	return row.LlmResponse, nil
}

// HistoryPage is one page of GetHistoryFiltered results plus a cursor to resume from.
// NextCursor is an opaque SK ("ts#paddedID") to pass back as HistoryFilter.Cursor for
// the next page; "" means no more data.
type HistoryPage struct {
	Rows       []CategorizationHistory
	NextCursor string
}

// GetHistoryFiltered returns one page (f.Limit rows, or the codebase default of 50 if
// unset) of history, newest first, optionally scoped to one account and filtered by
// prompt/unmatched/subject/sender.
//
// Each account gets exactly one bounded Query — never queryAll, which would read an
// entire (up to 30-day) partition per request; see ListExamplesByVerdict's doc comment
// for why queryAll defeats a Limit. Unmatched/PromptID are pushed into DynamoDB as a
// FilterExpression; SubjectQ/SenderQ are case-insensitive substring matches, which
// DynamoDB's case-sensitive contains() can't express, so they're applied in Go after the
// merge. Because a FilterExpression (like any post-Limit filter) can shrink a page below
// f.Limit — or empty it out entirely, for a sparse search term — callers paginating via
// NextCursor should be prepared for short or empty pages that still have more after them;
// NextCursor is only "" once every account's partition is truly exhausted at this cursor.
func (s *Store) GetHistoryFiltered(ctx context.Context, f HistoryFilter) (HistoryPage, error) {
	var accountIDs []int64
	if f.AccountID != nil {
		accountIDs = []int64{*f.AccountID}
	} else {
		accs, err := s.ListAccounts(ctx)
		if err != nil {
			return HistoryPage{}, err
		}
		for _, a := range accs {
			accountIDs = append(accountIDs, a.ID)
		}
	}

	pageSize := f.Limit
	if pageSize <= 0 {
		pageSize = 50
	}

	keyCond := "PK = :pk"
	if f.Cursor != "" {
		keyCond += " AND SK < :cur"
	}
	var filterExpr *string
	filterVals := map[string]types.AttributeValue{}
	switch {
	case f.Unmatched:
		filterExpr = aws.String("attribute_not_exists(promptId)")
	case f.PromptID != nil:
		filterExpr = aws.String("promptId = :pid")
		filterVals[":pid"] = nv(*f.PromptID)
	}

	var all []CategorizationHistory
	// moreBeyondFetched is true if any account's Query was cut off by Limit rather than
	// running out of items — i.e. that account's partition has more data past what was
	// fetched here, so the overall result can't be "done" even if every fetched item gets
	// consumed below.
	moreBeyondFetched := false
	for _, aid := range accountIDs {
		vals := map[string]types.AttributeValue{exprPK: sv(pkHistory(aid))}
		if f.Cursor != "" {
			vals[":cur"] = sv(f.Cursor)
		}
		maps.Copy(vals, filterVals)
		out, err := s.ddb.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String(s.table),
			KeyConditionExpression:    aws.String(keyCond),
			FilterExpression:          filterExpr,
			ExpressionAttributeValues: vals,
			ScanIndexForward:          aws.Bool(false),
			Limit:                     aws.Int32(i32(pageSize)),
		})
		if err != nil {
			continue
		}
		if out.LastEvaluatedKey != nil {
			moreBeyondFetched = true
		}
		for _, it := range out.Items {
			h := itemToHistory(it)
			// Strip llmResponse for list view (return sentinel if exists)
			llmR := h.LlmResponse
			h.LlmResponse = ""
			if llmR != "" {
				h.LlmResponse = "1"
			}
			all = append(all, h)
		}
	}

	// Merge-sort newest first — same order as the SK each row came from, so walking this
	// list in order and cutting at any point yields a valid resume cursor.
	sort.Slice(all, func(i, j int) bool {
		if all[i].Timestamp != all[j].Timestamp {
			return all[i].Timestamp > all[j].Timestamp
		}
		return all[i].ID > all[j].ID
	})

	// Walk the merge, applying the Go-only text filters, until pageSize matches or the
	// merged list runs out. The cursor tracks the last item *examined* here (matched or
	// not) — not the last matched row — so a resumed page picks up exactly where this one
	// left off instead of skipping or repeating unexamined rows.
	var filtered []CategorizationHistory
	lastConsumedSK := ""
	consumedAll := true
	for i, h := range all {
		if f.SubjectQ != "" && !strings.Contains(strings.ToLower(h.Subject), strings.ToLower(f.SubjectQ)) {
			lastConsumedSK = tsKey(h.Timestamp, h.ID)
			continue
		}
		if f.SenderQ != "" && !strings.Contains(strings.ToLower(h.Sender), strings.ToLower(f.SenderQ)) {
			lastConsumedSK = tsKey(h.Timestamp, h.ID)
			continue
		}
		filtered = append(filtered, h)
		lastConsumedSK = tsKey(h.Timestamp, h.ID)
		if int64(len(filtered)) >= pageSize {
			consumedAll = i == len(all)-1
			break
		}
	}

	nextCursor := ""
	if !consumedAll || moreBeyondFetched {
		nextCursor = lastConsumedSK
	}
	return HistoryPage{Rows: filtered, NextCursor: nextCursor}, nil
}

// TurnaroundSample is one LLM latency data point (one per processed email) used to build
// the dashboard's turnaround-time charts.
type TurnaroundSample struct {
	Timestamp  string
	DurationMs int64
}

// GetTurnaroundSamples returns latency samples recorded since the given time, one per
// email. A single email can produce multiple history rows (one per matched rule, all
// carrying the same LLM latency), so rows are deduped by MessageID.
func (s *Store) GetTurnaroundSamples(ctx context.Context, since time.Time) ([]TurnaroundSample, error) {
	accs, err := s.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	// SK is "ts#paddedID"; padID(0) is all zeros, so this is the smallest possible SK at
	// or after `since`, letting DynamoDB skip older rows instead of reading the whole
	// partition.
	sinceKey := tsKey(since.UTC().Format(tsLayout), 0)

	seen := make(map[string]bool)
	var samples []TurnaroundSample
	for _, acc := range accs {
		items, err := s.queryAll(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(s.table),
			KeyConditionExpression: aws.String("PK = :pk AND SK >= :since"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				exprPK:   sv(pkHistory(acc.ID)),
				":since": sv(sinceKey),
			},
		})
		if err != nil {
			continue
		}
		for _, it := range items {
			dur := getInt64(it, "durationMs")
			if dur <= 0 {
				continue
			}
			if msgID := getStr(it, "messageId"); msgID != "" {
				if seen[msgID] {
					continue
				}
				seen[msgID] = true
			}
			samples = append(samples, TurnaroundSample{
				Timestamp:  getStr(it, "ts"),
				DurationMs: dur,
			})
		}
	}
	return samples, nil
}

// DeleteHistoryByAccount deletes every history row for one account.
func (s *Store) DeleteHistoryByAccount(ctx context.Context, accountID int64) error {
	return s.deleteAllByPK(ctx, pkHistory(accountID))
}

// GetPromptIDsByMessageID returns the distinct prompt ids recorded in history for one
// message, scoped to the message's own account partition (the caller already knows the
// account, e.g. from a prior GetHistoryRow) rather than fanning out across every account.
func (s *Store) GetPromptIDsByMessageID(ctx context.Context, accountID int64, messageID string) ([]sql.NullInt64, error) {
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		FilterExpression:       aws.String("messageId = :mid"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK: sv(pkHistory(accountID)),
			":mid": sv(messageID),
		},
	})
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var result []sql.NullInt64
	for _, it := range items {
		pid := getNullInt64(it, "promptId")
		if pid.Valid && !seen[pid.Int64] {
			seen[pid.Int64] = true
			result = append(result, pid)
		}
	}
	return result, nil
}

func (s *Store) GetCurrentPromptIDsForMessage(ctx context.Context, accountID int64, messageID string) (map[int64]bool, error) {
	nullIDs, err := s.GetPromptIDsByMessageID(ctx, accountID, messageID)
	if err != nil {
		return nil, err
	}
	set := make(map[int64]bool)
	for _, nid := range nullIDs {
		if nid.Valid {
			set[nid.Int64] = true
		}
	}
	return set, nil
}

// RewriteHistoryForMessage rewrites categorization history for a message after manual
// correction, scoped to base.AccountID (the caller already resolved the row, so the
// account is known — no need to fan out across every account's partition).
func (s *Store) RewriteHistoryForMessage(ctx context.Context, messageID string, keptIDs []int64, addedPrompts []Prompt, base CategorizationHistory) error {
	keptSet := map[int64]bool{}
	for _, id := range keptIDs {
		keptSet[id] = true
	}

	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		FilterExpression:       aws.String("messageId = :mid"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK: sv(pkHistory(base.AccountID)),
			":mid": sv(messageID),
		},
	})
	if err != nil {
		return err
	}
	var deleteKeys []map[string]types.AttributeValue
	for _, it := range items {
		pid := getNullInt64(it, "promptId")
		if len(keptIDs) == 0 || !pid.Valid || !keptSet[pid.Int64] {
			deleteKeys = append(deleteKeys, map[string]types.AttributeValue{
				"PK": it["PK"], "SK": it["SK"],
			})
		}
	}
	if err := s.batchDelete(ctx, deleteKeys); err != nil {
		return err
	}

	for _, p := range addedPrompts {
		var labelName *string
		if p.LabelName != "" {
			labelName = ptr(p.LabelName)
		}
		if err := s.AddHistory(ctx, AddHistoryParams{
			AccountID:    base.AccountID,
			AccountEmail: base.AccountEmail,
			MessageID:    messageID,
			Subject:      base.Subject,
			Sender:       base.Sender,
			PromptID:     ptr(p.ID),
			PromptName:   ptr(p.Name),
			LabelName:    labelName,
			Actions:      "manual",
		}); err != nil {
			return err
		}
	}

	if len(keptIDs) == 0 && len(addedPrompts) == 0 {
		return s.AddHistory(ctx, AddHistoryParams{
			AccountID:    base.AccountID,
			AccountEmail: base.AccountEmail,
			MessageID:    messageID,
			Subject:      base.Subject,
			Sender:       base.Sender,
		})
	}
	return nil
}

// MessageHistoryState is one message's resolved state within an account's HIST# partition:
// its subject/sender/account-email (identical across every row for that message) and the
// set of prompt ids currently applied to it. Returned by GetHistoryStateForMessages.
type MessageHistoryState struct {
	Subject          string
	Sender           string
	AccountEmail     string
	CurrentPromptIDs map[int64]bool
}

// GetHistoryStateForMessages resolves subject/sender/account-email/current-prompt-ids for
// many messages in one account with a single partition query — the bulk-recategorize
// counterpart to GetCurrentPromptIDsForMessage, which reads the whole partition per call
// and would mean one query per selected email if looped. Messages with no history rows
// (shouldn't happen for anything selected from the History table, but handled defensively)
// are simply absent from the returned map.
func (s *Store) GetHistoryStateForMessages(ctx context.Context, accountID int64, messageIDs []string) (map[string]MessageHistoryState, error) {
	want := make(map[string]bool, len(messageIDs))
	for _, mid := range messageIDs {
		want[mid] = true
	}

	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK: sv(pkHistory(accountID)),
		},
	})
	if err != nil {
		return nil, err
	}

	result := make(map[string]MessageHistoryState, len(messageIDs))
	for _, it := range items {
		mid := getStr(it, "messageId")
		if !want[mid] {
			continue
		}
		st, ok := result[mid]
		if !ok {
			st = MessageHistoryState{
				Subject:          getStr(it, "subject"),
				Sender:           getStr(it, "sender"),
				AccountEmail:     getStr(it, "accountEmail"),
				CurrentPromptIDs: map[int64]bool{},
			}
		}
		if pid := getNullInt64(it, "promptId"); pid.Valid {
			st.CurrentPromptIDs[pid.Int64] = true
		}
		result[mid] = st
	}
	return result, nil
}

// RewriteMessagePlan is one message's post-correction state for RewriteHistoryForMessages:
// the prompt ids that should remain (already-history rows to keep, same semantics as
// RewriteHistoryForMessage's keptIDs) and the prompts newly applied (fresh rows to add).
type RewriteMessagePlan struct {
	MessageID    string
	Subject      string
	Sender       string
	KeptIDs      []int64
	AddedPrompts []Prompt
}

// RewriteHistoryForMessages is the bulk counterpart to RewriteHistoryForMessage: one query
// over the account's HIST# partition instead of one per message, then batched deletes and
// puts across every plan. This is the change that keeps a 50-email bulk recategorize inside
// the 2 RCU/2 WCU provisioned table instead of throttling — looping
// RewriteHistoryForMessage would mean 50 full partition reads.
func (s *Store) RewriteHistoryForMessages(ctx context.Context, accountID int64, accountEmail string, plans []RewriteMessagePlan) error {
	if len(plans) == 0 {
		return nil
	}
	want := make(map[string]bool, len(plans))
	for _, p := range plans {
		want[p.MessageID] = true
	}

	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK: sv(pkHistory(accountID)),
		},
	})
	if err != nil {
		return err
	}
	byMessage := make(map[string][]map[string]types.AttributeValue, len(plans))
	for _, it := range items {
		if mid := getStr(it, "messageId"); want[mid] {
			byMessage[mid] = append(byMessage[mid], it)
		}
	}

	var deleteKeys []map[string]types.AttributeValue
	var putItems []map[string]types.AttributeValue
	ts := Now()

	for _, p := range plans {
		keptSet := make(map[int64]bool, len(p.KeptIDs))
		for _, id := range p.KeptIDs {
			keptSet[id] = true
		}
		for _, it := range byMessage[p.MessageID] {
			pid := getNullInt64(it, "promptId")
			if len(p.KeptIDs) == 0 || !pid.Valid || !keptSet[pid.Int64] {
				deleteKeys = append(deleteKeys, map[string]types.AttributeValue{
					"PK": it["PK"], "SK": it["SK"],
				})
			}
		}
		for _, prompt := range p.AddedPrompts {
			var labelName *string
			if prompt.LabelName != "" {
				labelName = ptr(prompt.LabelName)
			}
			putItems = append(putItems, historyItem(localID(), ts, HistoryEntry{
				AccountID:    accountID,
				AccountEmail: accountEmail,
				MessageID:    p.MessageID,
				Subject:      p.Subject,
				Sender:       p.Sender,
				PromptID:     ptr(prompt.ID),
				PromptName:   ptr(prompt.Name),
				LabelName:    labelName,
				Actions:      "manual",
			}))
		}
		if len(p.KeptIDs) == 0 && len(p.AddedPrompts) == 0 {
			putItems = append(putItems, historyItem(localID(), ts, HistoryEntry{
				AccountID:    accountID,
				AccountEmail: accountEmail,
				MessageID:    p.MessageID,
				Subject:      p.Subject,
				Sender:       p.Sender,
			}))
		}
	}

	if err := s.batchDelete(ctx, deleteKeys); err != nil {
		return err
	}
	return s.batchPut(ctx, putItems)
}

// ============================================================
// Processed emails
// ============================================================

// claimLeaseSeconds bounds how long a claimed-but-unconfirmed message blocks other
// invocations from reclaiming it. Matches the scan/push Lambda timeout (template.yaml),
// so a lease can never expire while its owning invocation is still able to do work; a
// hard crash instead leaves the message to retry after this lease elapses (or at the
// next lookback scan, whichever comes first).
const claimLeaseSeconds = 900

// attrLeaseExp is the "PROC#" item's lease-expiry attribute (epoch seconds). Present
// and in the future: another invocation owns this message, still classifying it.
// Present and in the past: the owner crashed; reclaimable. Absent: either never seen
// (free to claim) or fully processed (BatchInsertProcessingResults writes the confirmed
// marker without this attribute).
const attrLeaseExp = "leaseExp"

// ClaimMessages attempts to reserve each of messageIDs for classification by this
// invocation, so that concurrent invocations racing on the same account never both pay
// for an LLM call on the same email. Each id gets a conditional PutItem that succeeds
// only if the "PROC#" marker doesn't exist yet or its lease has expired; the condition
// is evaluated strongly-consistent by DynamoDB, unlike the FilterUnprocessed pre-filter,
// so it is the authoritative gate. Returns just the ids this call actually won.
func (s *Store) ClaimMessages(ctx context.Context, accountID int64, messageIDs []string) ([]string, error) {
	now := time.Now().UTC().Unix()
	claimed := make([]string, 0, len(messageIDs))
	for _, mid := range messageIDs {
		_, err := s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(s.table),
			Item: map[string]types.AttributeValue{
				"PK":         sv(pkProcessed(accountID)),
				"SK":         sv(mid),
				attrLeaseExp: nv(now + claimLeaseSeconds),
				attrTTL:      nv(ttlDays(7)), // keep processed record for 7 days (2x lookback default)
			},
			ConditionExpression: aws.String("attribute_not_exists(PK) OR " + attrLeaseExp + " < :now"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":now": nv(now),
			},
		})
		if err != nil {
			var ccf *types.ConditionalCheckFailedException
			if isConditionFailed(err, &ccf) {
				continue // another invocation already owns (or confirmed) this message
			}
			return claimed, err
		}
		claimed = append(claimed, mid)
	}
	return claimed, nil
}

// ReleaseClaim gives up a lease taken by ClaimMessages, e.g. after an LLM error, so the
// message is immediately eligible for retry instead of waiting out the full lease. The
// attribute_exists(leaseExp) condition guarantees this can never delete a confirmed
// marker — only claims have a leaseExp attribute — so a release racing a concurrent
// confirm is always safe.
func (s *Store) ReleaseClaim(ctx context.Context, accountID int64, messageID string) error {
	_, err := s.ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv(pkProcessed(accountID)),
			"SK": sv(messageID),
		},
		ConditionExpression: aws.String("attribute_exists(" + attrLeaseExp + ")"),
	})
	if err != nil {
		var ccf *types.ConditionalCheckFailedException
		if isConditionFailed(err, &ccf) {
			return nil // already confirmed or already released
		}
		return err
	}
	return nil
}

// FilterUnprocessed is a cheap, eventually-consistent pre-filter ahead of ClaimMessages:
// it keeps the common case (nothing new since the last pass) off the write path. It is
// only an optimization, not the dedup gate — ClaimMessages' conditional write is — so an
// item with an expired lease must still be reported unprocessed here, letting
// ClaimMessages reclaim it.
func (s *Store) FilterUnprocessed(ctx context.Context, accountID int64, messageIDs []string) ([]string, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	keys := make([]map[string]types.AttributeValue, len(messageIDs))
	for i, mid := range messageIDs {
		keys[i] = map[string]types.AttributeValue{
			"PK": sv(pkProcessed(accountID)),
			"SK": sv(mid),
		}
	}
	now := time.Now().UTC().Unix()
	processed := map[string]bool{}
	// BatchGetItem in chunks of 100
	for i := 0; i < len(keys); i += 100 {
		end := min(i+100, len(keys))
		out, err := s.ddb.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{
			RequestItems: map[string]types.KeysAndAttributes{
				s.table: {Keys: keys[i:end], ProjectionExpression: aws.String("SK, " + attrLeaseExp)},
			},
		})
		if err != nil {
			return nil, err
		}
		for _, it := range out.Responses[s.table] {
			mid := getStr(it, "SK")
			if lease, ok := it[attrLeaseExp]; ok {
				if n, ok := lease.(*types.AttributeValueMemberN); ok {
					if exp, perr := strconv.ParseInt(n.Value, 10, 64); perr == nil && exp <= now {
						continue // lease expired: not processed, still a claimable candidate
					}
				}
			}
			processed[mid] = true
		}
	}
	var unprocessed []string
	for _, mid := range messageIDs {
		if !processed[mid] {
			unprocessed = append(unprocessed, mid)
		}
	}
	return unprocessed, nil
}

func (s *Store) DeleteProcessedEmailsByAccount(ctx context.Context, accountID int64) error {
	return s.deleteAllByPK(ctx, pkProcessed(accountID))
}

// BatchInsertProcessingResults writes one email's worth of log lines, history entries, and
// (passively-confirmed) prompt examples in a single batched write. IDs come from localIDs
// for every entity here — a process-local, no-round-trip generator, not the atomic nextIDs
// counter — since each entity's SK already carries a real timestamp for ordering, so the id
// only needs to be unique, not globally sequential (see localID's doc comment). That's what
// keeps this call to exactly one BatchWriteItem call per 25 items total, regardless of how
// many of logs/history/examples are non-empty, instead of a counter round trip per entity.
//
// examples is usually just the confirmed_positive rows for whichever prompts this email
// matched (processor.processEmail) — every email a rule matches and the user never corrects
// becomes evidence the rule is right about it. Folding them into this same call (rather than
// a separate InsertPromptExamples call per email) is deliberate: it means passive
// confirmation adds zero extra DynamoDB API calls per email, only more items in the same
// existing batch.
func (s *Store) BatchInsertProcessingResults(ctx context.Context, logs []LogEntry, history []HistoryEntry, examples []PromptExample, accountID int64, messageID string) error {
	ts := Now()
	items := make([]map[string]types.AttributeValue, 0, len(logs)+len(history)+len(examples))

	if len(logs) > 0 {
		ids := localIDs(len(logs))
		for i, l := range logs {
			items = append(items, logItem(ids[i], ts, l))
		}
	}
	if len(history) > 0 {
		ids := localIDs(len(history))
		for i, h := range history {
			items = append(items, historyItem(ids[i], ts, h))
		}
	}
	if len(examples) > 0 {
		ids := localIDs(len(examples))
		for i, e := range examples {
			e.ID = ids[i]
			e.CreatedAt = ts
			items = append(items, promptExampleItem(e))
		}
	}
	if messageID != "" {
		// Overwrite our own claim with a confirmed marker (no leaseExp attribute), folded
		// into the same batch rather than a separate MarkProcessed call.
		items = append(items, map[string]types.AttributeValue{
			"PK":    sv(pkProcessed(accountID)),
			"SK":    sv(messageID),
			attrTTL: nv(ttlDays(7)),
		})
	}
	if len(items) > 0 {
		if err := s.batchPut(ctx, items); err != nil {
			return err
		}
	}
	return nil
}

// ============================================================
// Retention
// ============================================================

func (s *Store) GetAccountRetention(ctx context.Context, accountID int64) (AccountRetention, error) {
	item, err := s.getKeyedItem(ctx, "RETENTION", padID(accountID))
	if err != nil {
		return AccountRetention{}, err
	}
	if item == nil {
		return AccountRetention{AccountID: accountID}, errors.New("not found")
	}
	return AccountRetention{
		AccountID:  accountID,
		GlobalDays: getNullInt64(item, "globalDays"),
	}, nil
}

func (s *Store) SetGlobalRetention(ctx context.Context, arg SetGlobalRetentionParams) error {
	item := map[string]types.AttributeValue{
		"PK": sv("RETENTION"),
		"SK": sv(padID(arg.AccountID)),
	}
	if arg.GlobalDays.Valid {
		item["globalDays"] = nv(arg.GlobalDays.Int64)
	} else {
		item["globalDays"] = &types.AttributeValueMemberNULL{Value: true}
	}
	_, err := s.ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(s.table), Item: item})
	return err
}

func (s *Store) ClearGlobalRetention(ctx context.Context, accountID int64) error {
	_, err := s.ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("RETENTION"),
			"SK": sv(padID(accountID)),
		},
	})
	return err
}

func (s *Store) DeleteAccountRetention(ctx context.Context, accountID int64) error {
	return s.ClearGlobalRetention(ctx, accountID)
}

func (s *Store) GetLabelRetention(ctx context.Context, accountID int64) ([]LabelRetention, error) {
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK: sv(pkLabelRetention(accountID)),
		},
	})
	if err != nil {
		return nil, err
	}
	ret := make([]LabelRetention, len(items))
	for i, it := range items {
		ret[i] = LabelRetention{
			ID:        getInt64(it, "id"),
			AccountID: accountID,
			LabelName: getStr(it, attrLabelName),
			Days:      getInt64(it, "days"),
		}
	}
	return ret, nil
}

func (s *Store) AddLabelRetention(ctx context.Context, arg AddLabelRetentionParams) error {
	// Check if exists first (upsert by label name)
	items, err := s.GetLabelRetention(ctx, arg.AccountID)
	if err == nil {
		for _, r := range items {
			if r.LabelName == arg.LabelName {
				_, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
					TableName: aws.String(s.table),
					Key: map[string]types.AttributeValue{
						"PK": sv(pkLabelRetention(arg.AccountID)),
						"SK": sv(padID(r.ID)),
					},
					UpdateExpression: aws.String("SET days = :d"),
					ExpressionAttributeValues: map[string]types.AttributeValue{
						":d": nv(arg.Days),
					},
				})
				return err
			}
		}
	}
	id, err := s.nextID(ctx, "label_retention")
	if err != nil {
		return err
	}
	_, err = s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]types.AttributeValue{
			"PK":          sv(pkLabelRetention(arg.AccountID)),
			"SK":          sv(padID(id)),
			"id":          nv(id),
			attrLabelName: sv(arg.LabelName),
			"days":        nv(arg.Days),
		},
	})
	return err
}

func (s *Store) DeleteLabelRetention(ctx context.Context, arg DeleteLabelRetentionParams) error {
	_, err := s.ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv(pkLabelRetention(arg.AccountID)),
			"SK": sv(padID(arg.ID)),
		},
	})
	return err
}

func (s *Store) DeleteLabelRetentionByAccount(ctx context.Context, accountID int64) error {
	return s.deleteAllByPK(ctx, pkLabelRetention(accountID))
}

func (s *Store) GetLabelExemptions(ctx context.Context, accountID int64) ([]LabelExemption, error) {
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK: sv(pkLabelExemption(accountID)),
		},
	})
	if err != nil {
		return nil, err
	}
	ex := make([]LabelExemption, len(items))
	for i, it := range items {
		ex[i] = LabelExemption{
			ID:        getInt64(it, "id"),
			AccountID: accountID,
			LabelName: getStr(it, attrLabelName),
		}
	}
	sort.Slice(ex, func(i, j int) bool { return ex[i].LabelName < ex[j].LabelName })
	return ex, nil
}

func (s *Store) AddLabelExemption(ctx context.Context, arg AddLabelExemptionParams) error {
	// Upsert: check if exists first
	items, _ := s.GetLabelExemptions(ctx, arg.AccountID)
	for _, e := range items {
		if e.LabelName == arg.LabelName {
			return nil // already exists
		}
	}
	id, err := s.nextID(ctx, "label_exemptions")
	if err != nil {
		return err
	}
	_, err = s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]types.AttributeValue{
			"PK":          sv(pkLabelExemption(arg.AccountID)),
			"SK":          sv(padID(id)),
			"id":          nv(id),
			attrLabelName: sv(arg.LabelName),
		},
	})
	return err
}

func (s *Store) DeleteLabelExemption(ctx context.Context, arg DeleteLabelExemptionParams) error {
	_, err := s.ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv(pkLabelExemption(arg.AccountID)),
			"SK": sv(padID(arg.ID)),
		},
	})
	return err
}

func (s *Store) DeleteLabelExemptionsByAccount(ctx context.Context, accountID int64) error {
	return s.deleteAllByPK(ctx, pkLabelExemption(accountID))
}

// ============================================================
// Email corrections
// ============================================================

func (s *Store) InsertEmailCorrection(ctx context.Context, arg InsertEmailCorrectionParams) (int64, error) {
	id, err := s.nextID(ctx, "corrections")
	if err != nil {
		return 0, err
	}
	ts := Now()
	_, err = s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]types.AttributeValue{
			"PK":               sv("CORRECTION#" + arg.MessageID),
			"SK":               sv(tsKey(ts, id)),
			"id":               nv(id),
			attrCreatedAt:      sv(ts),
			attrAccountID:      nv(arg.AccountID),
			attrMessageID:      sv(arg.MessageID),
			"addedPrompts":     sv(arg.AddedPrompts),
			"removedPrompts":   sv(arg.RemovedPrompts),
			"currentPromptIds": sv(arg.CurrentPromptIds),
			"note":             sv(arg.Note),
		},
	})
	return id, err
}

// ============================================================
// Prompt examples
// ============================================================
//
// Growth and retention: examples are written from two sources — the recategorize handlers
// (any verdict, on manual correction) and processor.processEmail (confirmed_positive only,
// on every ordinary classify match — see BatchInsertProcessingResults). Even at a heavy 200
// matched-emails/day of passive confirmation, storage grows by roughly
// 200 × 800B × 365 ≈ 58MB/year — still trivial against the 25GB free-tier allowance — and
// read cost is unaffected by corpus size either way, since ListExamplesByVerdict is always
// Limit-bounded regardless of how much has accumulated. So there's still no TTL here — but
// there IS now a daily active trim: prunePromptExamples (prune.go), run once per scheduled
// scan, deletes whatever a prompt's example-selection sampler (improve.go's sampleVerdict)
// would never actually pick, via DeletePromptExamples below, keeping each verdict's corpus
// bounded to roughly 2x the replay-validation cap instead of growing forever. The
// DeleteExamplesForPrompt escape hatch (wired into DeletePrompt) is separate and unchanged —
// it covers the one thing the daily prune doesn't: a rule's intent changing enough that its
// whole history should be cleared at once, not just trimmed to what's still useful. See
// db/models.go's PromptExample doc comment for the key schema (PK = EXAMPLE#<promptId>,
// SK = <verdict>#<ts>#<padID(id)>).

// InsertPromptExamples writes a batch of examples produced by one recategorization (single
// or bulk). Deliberately append-only: a re-correction of the same (promptId, messageId)
// pair writes a new row rather than overwriting the old one. Deduping to "the newest
// verdict wins" happens at read time in ListExamplesByVerdict's caller
// (improve.go's gatherRawExamples), not here — a dedupe index would cost an
// extra read-then-write per example, unaffordable during a bulk write at 2 WCU.
// promptExampleItem and itemToPromptExample are the marshal/unmarshal pair for
// PromptExample, mirroring every other entity's xToItem/itemToX helpers (accountItem/
// itemToAccount, suggestionItem/itemToSuggestion, etc.) so the wire format has one place to
// verify — see db/attributevalue_test.go's PromptExample round-trip tests.
func promptExampleItem(e PromptExample) map[string]types.AttributeValue {
	return keyedItem(e, pkExample(e.PromptID), exampleSK(e.Verdict, e.CreatedAt, e.ID), 0)
}

func itemToPromptExample(it map[string]types.AttributeValue) PromptExample {
	return unmarshalItem[PromptExample](it)
}

func (s *Store) InsertPromptExamples(ctx context.Context, examples []PromptExample) error {
	if len(examples) == 0 {
		return nil
	}
	// localIDs, not nextIDs: same reasoning as BatchInsertProcessingResults — the SK already
	// carries CreatedAt for ordering, so the id only needs to be unique, not a counter round
	// trip. This also matters for correctness, not just cost: selectExamplesForPrompt's
	// "newest verdict wins" dedup (recategorize.go) depends on every PromptExample write
	// path — this one (manual recategorize) and BatchInsertProcessingResults' (passive
	// confirmation on classify) — sharing the same monotonically-ordered id source, so a
	// later correction's id reliably outranks an earlier passive confirmation's regardless
	// of which of the two code paths wrote which.
	ids := localIDs(len(examples))
	ts := Now()
	items := make([]map[string]types.AttributeValue, len(examples))
	for i, e := range examples {
		e.ID = ids[i]
		e.CreatedAt = ts
		items[i] = promptExampleItem(e)
	}
	return s.batchPut(ctx, items)
}

// ListExamplesByVerdict returns the newest limit examples of one verdict for a prompt. The
// KeyConditionExpression's begins_with(SK, verdict+"#") scopes the query to that verdict's
// contiguous SK range — DynamoDB seeks directly there rather than scanning the rest of the
// partition — so this reads exactly `limit` items regardless of how large the corpus grows.
// Uses ddb.Query directly rather than queryAll, which follows pagination to read an entire
// result set and would defeat the Limit.
func (s *Store) ListExamplesByVerdict(ctx context.Context, promptID int64, verdict string, limit int32) ([]PromptExample, error) {
	out, err := s.ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :vprefix)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK:     sv(pkExample(promptID)),
			":vprefix": sv(verdict + "#"),
		},
		ScanIndexForward: aws.Bool(false),
		Limit:            aws.Int32(limit),
	})
	if err != nil {
		return nil, err
	}
	result := make([]PromptExample, len(out.Items))
	for i, it := range out.Items {
		result[i] = itemToPromptExample(it)
	}
	return result, nil
}

// CountExamplesByVerdict returns per-verdict counts for a prompt's example corpus (UI
// badges). Select: COUNT reads only key attributes, not full items; paginates like queryAll
// since a single Query page caps at ~1MB and a long-lived rule could exceed that.
func (s *Store) CountExamplesByVerdict(ctx context.Context, promptID int64) (map[string]int64, error) {
	verdicts := []string{VerdictFalsePositive, VerdictFalseNegative, VerdictConfirmedPositive}
	counts := make(map[string]int64, len(verdicts))
	for _, v := range verdicts {
		var total int64
		var start map[string]types.AttributeValue
		for {
			out, err := s.ddb.Query(ctx, &dynamodb.QueryInput{
				TableName:              aws.String(s.table),
				KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :vprefix)"),
				ExpressionAttributeValues: map[string]types.AttributeValue{
					exprPK:     sv(pkExample(promptID)),
					":vprefix": sv(v + "#"),
				},
				Select:            types.SelectCount,
				ExclusiveStartKey: start,
			})
			if err != nil {
				return nil, err
			}
			total += int64(out.Count)
			if out.LastEvaluatedKey == nil {
				break
			}
			start = out.LastEvaluatedKey
		}
		counts[v] = total
	}
	return counts, nil
}

// DeleteExamplesForPrompt removes every example recorded against a prompt. Called when the
// prompt itself is deleted (handleDeletePrompt, server.go), and as a manual "Clear
// examples" action on the prompt card for when a rule's intent has changed enough that its
// history would mislead the improver.
func (s *Store) DeleteExamplesForPrompt(ctx context.Context, promptID int64) error {
	return s.deleteAllByPK(ctx, pkExample(promptID))
}

// DeletePromptExamples deletes exactly the given examples — unlike DeleteExamplesForPrompt
// (which wipes a whole prompt's corpus), this is for prunePromptExamples (prune.go), which
// already has the full PromptExample values in hand (from ListExamplesByVerdict) and just
// needs their keys rebuilt, not re-queried. Callers don't need to know the key format
// (pkExample/exampleSK stay unexported) — just hand back whichever examples pruneKeepSet
// (improve.go) decided not to keep.
func (s *Store) DeletePromptExamples(ctx context.Context, examples []PromptExample) error {
	if len(examples) == 0 {
		return nil
	}
	keys := make([]map[string]types.AttributeValue, len(examples))
	for i, ex := range examples {
		keys[i] = map[string]types.AttributeValue{
			"PK": sv(pkExample(ex.PromptID)),
			"SK": sv(exampleSK(ex.Verdict, ex.CreatedAt, ex.ID)),
		}
	}
	return s.batchDelete(ctx, keys)
}

// ============================================================
// Prompt suggestions
// ============================================================

func itemToSuggestion(it map[string]types.AttributeValue) PromptSuggestion {
	return unmarshalItem[PromptSuggestion](it)
}

// generatingStaleAfter bounds how long a suggestion may sit in "generating" before
// GetPromptSuggestion/ListPromptSuggestions treat it as failed instead of showing an
// infinite spinner. The improve worker (see improve.go's improveRunner) derives its own
// deadline from the invoking Lambda's remaining time and always writes a terminal status
// before returning — either normally, via its deferred failure write, or via
// server.failDispatch when the async hand-off to the worker never happened at all. This is
// the backstop for whatever slips past all three: an invocation killed by something other
// than its own deadline (e.g. an account-level throttle), or an operational gap this
// session hasn't hit yet. Read-only — the stored row is untouched, so a genuinely-
// still-running call within the window is unaffected, and a caller that wants the raw
// stored status (e.g. attributevalue_test.go's wire-format round-trip checks) still gets
// it by calling itemToSuggestion directly instead of through these two methods.
const generatingStaleAfter = 20 * time.Minute

func withGeneratingStaleness(sg PromptSuggestion) PromptSuggestion {
	if sg.Status != SuggestionStatusGenerating {
		return sg
	}
	updated, err := time.Parse(tsLayout, sg.UpdatedAt)
	if err != nil || time.Since(updated) < generatingStaleAfter {
		return sg
	}
	sg.Status = SuggestionStatusFailed
	if sg.UserComment == "" {
		sg.UserComment = "Timed out waiting for a result — try regenerating."
	}
	return sg
}

func suggestionItem(id int64, ts string, arg InsertPromptSuggestionParams) map[string]types.AttributeValue {
	return keyedItem(PromptSuggestion{
		ID:                    id,
		CreatedAt:             ts,
		UpdatedAt:             ts,
		PromptID:              arg.PromptID,
		CorrectionID:          nullInt64Ptr(arg.CorrectionID),
		TriggerKind:           arg.TriggerKind,
		MessageID:             arg.MessageID,
		EmailSubject:          arg.EmailSubject,
		EmailSender:           arg.EmailSender,
		EmailBodySnapshot:     arg.EmailBodySnapshot,
		OriginalInstructions:  arg.OriginalInstructions,
		SuggestedInstructions: arg.SuggestedInstructions,
		ConversationJSON:      arg.ConversationJSON,
		UserComment:           "",
		Status:                arg.Status,
	}, "SUGGESTION", padID(id), ttlDays(suggestionTTLDays))
}

func (s *Store) InsertPromptSuggestion(ctx context.Context, arg InsertPromptSuggestionParams) (int64, error) {
	id, err := s.nextID(ctx, "suggestions")
	if err != nil {
		return 0, err
	}
	_, err = s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      suggestionItem(id, Now(), arg),
	})
	return id, err
}

func (s *Store) GetPromptSuggestion(ctx context.Context, id int64) (PromptSuggestion, error) {
	item, err := s.getKeyedItem(ctx, "SUGGESTION", padID(id))
	if err != nil {
		return PromptSuggestion{}, err
	}
	if item == nil {
		return PromptSuggestion{}, fmt.Errorf("suggestion not found: %d", id)
	}
	return withGeneratingStaleness(itemToSuggestion(item)), nil
}

func (s *Store) ListPromptSuggestions(ctx context.Context) ([]PromptSuggestion, error) {
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK: sv("SUGGESTION"),
		},
		ScanIndexForward: aws.Bool(false),
	})
	if err != nil {
		return nil, err
	}
	var result []PromptSuggestion
	for _, it := range items {
		s := withGeneratingStaleness(itemToSuggestion(it))
		if s.Status == SuggestionStatusApplied || s.Status == SuggestionStatusDismissed {
			continue
		}
		result = append(result, s)
	}
	// Sort: generating first, then pending, then others; newest first within status
	sort.SliceStable(result, func(i, j int) bool {
		rank := map[string]int{SuggestionStatusGenerating: 0, SuggestionStatusPending: 1}
		ri, rj := rank[result[i].Status], rank[result[j].Status]
		if ri != rj {
			return ri < rj
		}
		return result[i].ID > result[j].ID
	})
	return result, nil
}

func (s *Store) CountPendingPromptSuggestions(ctx context.Context) (int64, error) {
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		FilterExpression:       aws.String("#s = :pending"),
		ExpressionAttributeNames: map[string]string{
			"#s": attrStatus,
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK:     sv("SUGGESTION"),
			":pending": sv(SuggestionStatusPending),
		},
	})
	if err != nil {
		return 0, err
	}
	return int64(len(items)), nil
}

func (s *Store) FinalizePromptSuggestion(ctx context.Context, arg FinalizePromptSuggestionParams) error {
	return s.updateItem(ctx, "SUGGESTION", padID(arg.ID),
		"SET suggestedInstructions = :si, conversationJson = :cj, #s = :st, userComment = :uc, updatedAt = :ua, "+
			"replayModel = :rm, replayTotal = :rt, replayPassed = :rp, replayBaseline = :rb, replayFailures = :rf, "+
			"problemExampleKeys = :pk, roundsJson = :rj, roundsRun = :rr, bestRound = :br",
		map[string]string{"#s": attrStatus},
		map[string]types.AttributeValue{
			":si":         sv(arg.SuggestedInstructions),
			":cj":         sv(arg.ConversationJSON),
			exprStatus:    sv(arg.Status),
			":uc":         sv(arg.UserComment),
			exprUpdatedAt: sv(Now()),
			":rm":         sv(arg.ReplayModel),
			":rt":         nv(arg.ReplayTotal),
			":rp":         nv(arg.ReplayPassed),
			":rb":         nv(arg.ReplayBaseline),
			":rf":         sv(arg.ReplayFailures),
			":pk":         sv(arg.ProblemExampleKeys),
			":rj":         sv(arg.RoundsJSON),
			":rr":         nv(arg.RoundsRun),
			":br":         nv(arg.BestRound),
		})
}

// UpdatePromptSuggestion saves edits to a suggestion and resets it to pending — the same
// write as FinalizePromptSuggestion, with the status fixed to SuggestionStatusPending.
func (s *Store) UpdatePromptSuggestion(ctx context.Context, arg UpdatePromptSuggestionParams) error {
	return s.FinalizePromptSuggestion(ctx, FinalizePromptSuggestionParams{
		SuggestedInstructions: arg.SuggestedInstructions,
		ConversationJSON:      arg.ConversationJSON,
		Status:                SuggestionStatusPending,
		UserComment:           arg.UserComment,
		ID:                    arg.ID,
	})
}

// setSuggestionStatus stamps a suggestion's status + updatedAt — shared by
// ApplyPromptSuggestion and DismissPromptSuggestion, which differ only in the target status.
func (s *Store) setSuggestionStatus(ctx context.Context, id int64, status string) error {
	return s.updateItem(ctx, "SUGGESTION", padID(id), "SET #s = :st, updatedAt = :ua",
		map[string]string{"#s": attrStatus},
		map[string]types.AttributeValue{
			exprStatus:    sv(status),
			exprUpdatedAt: sv(Now()),
		})
}

func (s *Store) ApplyPromptSuggestion(ctx context.Context, id int64) error {
	return s.setSuggestionStatus(ctx, id, SuggestionStatusApplied)
}

// MarkPromptSuggestionGenerating flips a suggestion back to "generating" — used when
// regenerate kicks off a fresh (now asynchronous) improve+replay round in the background,
// so the detail page immediately shows the same spinner state it uses for a brand-new
// suggestion instead of the stale previous instructions while Bedrock runs.
//
// Must REMOVE claimedAt, not just SET status — this cannot share setSuggestionStatus's
// plain SET expression. ClaimPromptSuggestion's attribute_not_exists(claimedAt) condition
// only ever succeeds once per id; without clearing it here, every regenerate after the
// first would have its worker invocation's claim fail, runOne would log "already claimed,
// skipping" and return before its deferred failure write is even registered, and the
// suggestion would sit on "generating" until generatingStaleAfter's 20-minute read-side
// flip — whose own remedy text ("try regenerating") would silently do nothing again.
func (s *Store) MarkPromptSuggestionGenerating(ctx context.Context, id int64) error {
	return s.updateItem(ctx, "SUGGESTION", padID(id), "SET #s = :st, updatedAt = :ua REMOVE "+attrClaimedAt,
		map[string]string{"#s": attrStatus},
		map[string]types.AttributeValue{
			exprStatus:    sv(SuggestionStatusGenerating),
			exprUpdatedAt: sv(Now()),
		})
}

func (s *Store) DismissPromptSuggestion(ctx context.Context, id int64) error {
	return s.setSuggestionStatus(ctx, id, SuggestionStatusDismissed)
}

// attrClaimedAt marks a suggestion row as claimed by an improve-worker invocation — see
// ClaimPromptSuggestion.
const attrClaimedAt = "claimedAt"

// ClaimPromptSuggestion marks a suggestion row as claimed by an improve-worker invocation
// (improveRunner.runOne, improve.go). The MODE=improve Lambda is invoked async (Event),
// which AWS automatically retries up to twice on error; without this, a retry after a
// partial failure would redo (and re-bill) the same improve+replay round from scratch
// instead of skipping a suggestion an earlier attempt already claimed. The conditional
// UpdateItem only succeeds the first time for a given id — same
// attribute_not_exists(...)-gated pattern ClaimMessages uses for the per-message lease,
// just without a lease *expiry*: an improve round is a single bounded call, not a
// long-lived worker loop, so there's nothing here for a crashed claimant to hand back the
// way ReleaseClaim does for ClaimMessages.
func (s *Store) ClaimPromptSuggestion(ctx context.Context, id int64) (bool, error) {
	_, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("SUGGESTION"),
			"SK": sv(padID(id)),
		},
		UpdateExpression:    aws.String("SET " + attrClaimedAt + " = :ca"),
		ConditionExpression: aws.String("attribute_not_exists(" + attrClaimedAt + ")"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":ca": sv(Now()),
		},
	})
	if err != nil {
		var ccf *types.ConditionalCheckFailedException
		if isConditionFailed(err, &ccf) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// MarkExamplesResolved sets resolvedBySuggestionId on each of the given examples — called
// once, when the suggestion that used them as evidence is applied (see
// PromptSuggestion.ProblemExampleKeys; never on generate or dismiss, since nothing about
// the rule has changed in either of those cases). Best-effort per key: a failure on one
// example is logged and skipped rather than propagated, since this is bookkeeping for
// future improve rounds, not the primary effect of applying a suggestion — it must never be
// able to block a rule update the user explicitly asked for.
func (s *Store) MarkExamplesResolved(ctx context.Context, keys []ResolvedExampleKey, suggestionID int64) {
	for _, k := range keys {
		err := s.updateItem(ctx, pkExample(k.PromptID), exampleSK(k.Verdict, k.CreatedAt, k.ID),
			"SET resolvedBySuggestionId = :sid",
			nil,
			map[string]types.AttributeValue{":sid": nv(suggestionID)},
		)
		if err != nil {
			slog.Error("mark example resolved", "prompt_id", k.PromptID, "verdict", k.Verdict, "id", k.ID, "suggestion_id", suggestionID, "err", err)
		}
	}
}

// ApplyPromptSuggestionAndUpdatePrompt applies a suggestion and updates the prompt in
// sequence, then marks the false_negative/false_positive examples it was built from as
// resolved (problemExampleKeys — see ResolvedExampleKey) so they're excluded from future
// improve rounds for this rule. Not transactional, same as the two steps before it — a
// failure partway through leaves the prompt/suggestion state ahead of the resolved-marking,
// which is the same order of priority MarkExamplesResolved's own best-effort behavior
// already assumes.
// ApplyPromptSuggestionAndUpdatePrompt applies sg to its prompt: mints a new PromptVersion
// carrying sg's replay evidence (source "suggestion" — see PromptVersion's doc comment),
// applies the suggestion, and marks the examples it was built from as resolved. Not
// transactional, same as every other multi-step write in this package — a failure partway
// through leaves state ahead of the resolved-marking, which is the same order of priority
// MarkExamplesResolved's own best-effort behavior already assumes.
func (s *Store) ApplyPromptSuggestionAndUpdatePrompt(ctx context.Context, sg PromptSuggestion, problemExampleKeys []ResolvedExampleKey) error {
	sid := sg.ID
	if _, err := s.InsertPromptVersion(ctx, InsertPromptVersionParams{
		PromptID:     sg.PromptID,
		Instructions: sg.SuggestedInstructions,
		Source:       PromptVersionSourceSuggestion,
		SuggestionID: &sid,
		ReplayModel:  sg.ReplayModel,
		ReplayTotal:  sg.ReplayTotal,
		ReplayPassed: sg.ReplayPassed,
	}); err != nil {
		return err
	}
	if err := s.ApplyPromptSuggestion(ctx, sg.ID); err != nil {
		return err
	}
	s.MarkExamplesResolved(ctx, problemExampleKeys, sg.ID)
	return nil
}

// ============================================================
// Suggestion trace (live progress log)
// ============================================================
//
// traceTTLDays is deliberately much shorter than suggestionTTLDays (90): the trace is a
// watch/troubleshoot artifact whose value decays fast once a round finishes, not a
// permanent record. Keeping it short bounds storage automatically as suggestions
// accumulate, with no separate sweep — DynamoDB's TTL sweep does the work.
const traceTTLDays = 7

func pkSuggestionTrace(suggestionID int64) string { return fmt.Sprintf("SUGG_TRACE#%d", suggestionID) }

func suggestionTraceItem(suggestionID int64, e SuggestionTraceEvent) map[string]types.AttributeValue {
	return keyedItem(e, pkSuggestionTrace(suggestionID), padID(e.Seq), ttlDays(traceTTLDays))
}

// AppendSuggestionTrace writes a batch of trace events for one suggestion in a single
// BatchWriteItem call. Best-effort by design at the caller (improve.go's trace writer): a
// failure here must never fail the improve round it's merely narrating.
func (s *Store) AppendSuggestionTrace(ctx context.Context, suggestionID int64, events []SuggestionTraceEvent) error {
	if len(events) == 0 {
		return nil
	}
	items := make([]map[string]types.AttributeValue, len(events))
	for i, e := range events {
		items[i] = suggestionTraceItem(suggestionID, e)
	}
	return s.batchPut(ctx, items)
}

// ListSuggestionTrace returns a suggestion's trace events with Seq > afterSeq, oldest
// first — the shape a polling cursor wants (append what's new, in order). Uses ddb.Query
// directly rather than queryAll: a suggestion's trace is capped in practice by how long one
// improve round runs, so following pagination here would only matter for a pathological
// case, not the common one, and a direct bounded Query keeps this cheap regardless.
func (s *Store) ListSuggestionTrace(ctx context.Context, suggestionID, afterSeq int64) ([]SuggestionTraceEvent, error) {
	out, err := s.ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk AND SK > :after"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK:   sv(pkSuggestionTrace(suggestionID)),
			":after": sv(padID(afterSeq)),
		},
		ScanIndexForward: aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	result := make([]SuggestionTraceEvent, len(out.Items))
	for i, it := range out.Items {
		result[i] = unmarshalItem[SuggestionTraceEvent](it)
	}
	return result, nil
}

// traceStaleAfter bounds how long a generating suggestion may go without a trace event
// before IsSuggestionTraceStale reports it stale. Much tighter than generatingStaleAfter's
// 20 minutes: a live trace updates roughly every traceFlushInterval while a round is
// actually progressing (see improve_trace.go), so a gap this long is a far earlier and
// sharper "the worker likely died" signal than generatingStaleAfter's read-side flip,
// which is keyed off updatedAt — an attribute the worker only touches on its terminal
// write, not on progress.
const traceStaleAfter = 3 * time.Minute

// latestSuggestionTraceEvent returns the newest trace event for a suggestion regardless of
// any polling cursor. Used only for staleness detection (IsSuggestionTraceStale) — the
// polling endpoint itself uses ListSuggestionTrace's cursor-scoped query instead.
func (s *Store) latestSuggestionTraceEvent(ctx context.Context, suggestionID int64) (SuggestionTraceEvent, bool, error) {
	out, err := s.ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK: sv(pkSuggestionTrace(suggestionID)),
		},
		ScanIndexForward: aws.Bool(false),
		Limit:            aws.Int32(1),
	})
	if err != nil {
		return SuggestionTraceEvent{}, false, err
	}
	if len(out.Items) == 0 {
		return SuggestionTraceEvent{}, false, nil
	}
	return unmarshalItem[SuggestionTraceEvent](out.Items[0]), true, nil
}

// LatestSuggestionTraceSeq returns the highest Seq already written for a suggestion's
// trace, or 0 if it has none yet. The improve worker (improve_trace.go's newTraceWriter,
// called from improve.go's runOne) seeds its in-process seq counter from this on every
// invocation — a regenerate re-invokes the worker from scratch, and without this a fresh
// traceWriter would restart at Seq 1 and silently overwrite the first round's Seq 1..N
// items (same PK+SK) instead of continuing the sequence, corrupting the trace and making
// the polling endpoint's cursor (already past those seqs from the first round) never see
// the regenerate round's events at all.
func (s *Store) LatestSuggestionTraceSeq(ctx context.Context, suggestionID int64) (int64, error) {
	latest, found, err := s.latestSuggestionTraceEvent(ctx, suggestionID)
	if err != nil || !found {
		return 0, err
	}
	return latest.Seq, nil
}

// IsSuggestionTraceStale reports whether a generating suggestion has gone quiet long
// enough that its worker likely died — the trace endpoint's (server.go) signal for
// rendering "no progress for a while — the worker may have died; try regenerating" instead
// of a spinner with no information behind it. Always false for any status other than
// generating: a finished suggestion's trace going quiet is normal, not a problem. Falls
// back to the suggestion's own UpdatedAt when no trace event exists yet at all (the worker
// was invoked but hasn't reached its first Event call) rather than reporting stale
// prematurely. This is read-side only, exactly like withGeneratingStaleness — the harder
// backstop of actually flipping the stored status still belongs to generatingStaleAfter.
func (s *Store) IsSuggestionTraceStale(ctx context.Context, sg PromptSuggestion) (bool, error) {
	if sg.Status != SuggestionStatusGenerating {
		return false, nil
	}
	lastActivity := sg.UpdatedAt
	latest, found, err := s.latestSuggestionTraceEvent(ctx, sg.ID)
	if err != nil {
		return false, err
	}
	if found {
		lastActivity = latest.CreatedAt
	}
	return isTraceStale(lastActivity, time.Now()), nil
}

// isTraceStale is the pure predicate IsSuggestionTraceStale wraps around a DynamoDB
// lookup: given the last known activity timestamp (a trace event's CreatedAt, or the
// suggestion's own UpdatedAt when no trace event exists yet) and the current time, has
// traceStaleAfter elapsed with no progress? Factored out so the threshold logic is
// unit-testable without a live DynamoDB client — Store.ddb is a concrete
// *dynamodb.Client, not an interface, so IsSuggestionTraceStale itself can't be exercised
// directly in this package's tests (see db/attributevalue_test.go's doc comment on the
// same constraint for every other *Store method that touches s.ddb). An unparseable
// timestamp reads as "not stale" rather than an error — the same fail-open choice
// withGeneratingStaleness makes for the same field.
func isTraceStale(lastActivity string, now time.Time) bool {
	t, err := time.Parse(tsLayout, lastActivity)
	if err != nil {
		return false
	}
	return now.Sub(t) > traceStaleAfter
}

// ============================================================
// LLM Debug
// ============================================================

func llmDebugItem(id int64, ts string, arg AddLlmDebugParams) map[string]types.AttributeValue {
	return keyedItem(LlmDebug{
		ID:           id,
		Timestamp:    ts,
		AccountID:    arg.AccountID,
		AccountEmail: arg.AccountEmail,
		MessageID:    arg.MessageID,
		Subject:      arg.Subject,
		Sender:       arg.Sender,
		GmailRaw:     arg.GmailRaw,
		LlmRequest:   arg.LlmRequest,
		LlmResponse:  arg.LlmResponse,
		// TTL: LLM-debug rows hold the raw Gmail message; TrimLlmDebug only keeps the
		// newest 3, but without a TTL the final 3 would linger forever once debugging ends.
	}, "LLM_DEBUG", padID(id), ttlDays(logHistoryTTLDays))
}

func (s *Store) AddLlmDebug(ctx context.Context, arg AddLlmDebugParams) error {
	id, err := s.nextID(ctx, "llm_debug")
	if err != nil {
		return err
	}
	_, err = s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      llmDebugItem(id, Now(), arg),
	})
	return err
}

func (s *Store) TrimLlmDebug(ctx context.Context) error {
	// Keep only the 3 most recent items
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK: sv("LLM_DEBUG"),
		},
		ScanIndexForward:     aws.Bool(false),
		ProjectionExpression: aws.String("PK, SK"),
	})
	if err != nil {
		return err
	}
	if len(items) <= 3 {
		return nil
	}
	// Delete all beyond the 3 most recent (items are newest-first)
	var keys []map[string]types.AttributeValue
	for _, it := range items[3:] {
		keys = append(keys, map[string]types.AttributeValue{"PK": it["PK"], "SK": it["SK"]})
	}
	return s.batchDelete(ctx, keys)
}

func (s *Store) GetLatestLlmDebug(ctx context.Context) ([]LlmDebug, error) {
	out, err := s.ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK: sv("LLM_DEBUG"),
		},
		ScanIndexForward: aws.Bool(false),
		Limit:            aws.Int32(3),
	})
	if err != nil {
		return nil, err
	}
	result := make([]LlmDebug, len(out.Items))
	for i, it := range out.Items {
		result[i] = itemToLlmDebug(it)
	}
	return result, nil
}

func itemToLlmDebug(it map[string]types.AttributeValue) LlmDebug { return unmarshalItem[LlmDebug](it) }

func (s *Store) RecordLlmDebug(ctx context.Context, e AddLlmDebugParams) error {
	if err := s.AddLlmDebug(ctx, e); err != nil {
		return err
	}
	return s.TrimLlmDebug(ctx)
}

// ============================================================
// Params and row types (replaces sqlc-generated definitions)
// ============================================================

type SetSettingParams struct {
	Key   string
	Value string
}

type AddHistoryParams = HistoryEntry

type AddLogParams = LogEntry

type AddLlmDebugParams struct {
	AccountID    int64
	AccountEmail string
	MessageID    string
	Subject      string
	Sender       string
	GmailRaw     string
	LlmRequest   string
	LlmResponse  string
}

type CreatePromptParams struct {
	Name           string
	Instructions   string
	LabelName      string
	ActionArchive  int64
	ActionSpam     int64
	ActionTrash    int64
	ActionMarkRead int64
	SortOrder      int64
	StopProcessing int64
	AccountID      sql.NullInt64
}

type UpdatePromptParams struct {
	Name           string
	Instructions   string
	LabelName      string
	ActionArchive  int64
	ActionSpam     int64
	ActionTrash    int64
	ActionMarkRead int64
	StopProcessing int64
	AccountID      sql.NullInt64
	ID             int64
}

type UpdateAccountCredentialsParams struct {
	CredentialsJSON string
	ID              int64
}

type UpdateAccountWatchParams struct {
	ID         int64
	HistoryID  string
	Expiration int64
}

type UpsertAccountParams struct {
	Email           string
	CredentialsJSON string
}

type GetLogsRangeParams struct {
	Timestamp  string
	Timestamp2 string
}

type InsertEmailCorrectionParams struct {
	AccountID        int64
	MessageID        string
	AddedPrompts     string
	RemovedPrompts   string
	CurrentPromptIds string
	Note             string
}

type InsertPromptSuggestionParams struct {
	PromptID              int64
	CorrectionID          sql.NullInt64
	TriggerKind           string
	MessageID             string
	EmailSubject          string
	EmailSender           string
	EmailBodySnapshot     string
	OriginalInstructions  string
	SuggestedInstructions string
	ConversationJSON      string
	Status                string
}

type FinalizePromptSuggestionParams struct {
	SuggestedInstructions string
	ConversationJSON      string
	Status                string
	UserComment           string
	ID                    int64

	// Replay validation fields — always set (possibly zero-value when replay didn't run,
	// e.g. improve_replay disabled or the improve call itself failed). See
	// PromptSuggestion's doc comment in db/models.go.
	ReplayModel    string
	ReplayTotal    int64
	ReplayPassed   int64
	ReplayBaseline int64
	ReplayFailures string

	// ProblemExampleKeys is a JSON-encoded []ResolvedExampleKey — see PromptSuggestion's
	// doc comment in db/models.go. Empty ("") on a failed improve call, since nothing was
	// built from anything in that case.
	ProblemExampleKeys string

	// Round trajectory — see PromptSuggestion.RoundsJSON's doc comment in db/models.go.
	// All zero-value on a failed improve call, same as the replay fields above.
	RoundsJSON string
	RoundsRun  int64
	BestRound  int64
}

type UpdatePromptSuggestionParams struct {
	SuggestedInstructions string
	ConversationJSON      string
	UserComment           string
	ID                    int64
}

type SetGlobalRetentionParams struct {
	AccountID  int64
	GlobalDays sql.NullInt64
}

type AddLabelRetentionParams struct {
	AccountID int64
	LabelName string
	Days      int64
}

type DeleteLabelRetentionParams struct {
	ID        int64
	AccountID int64
}

type AddLabelExemptionParams struct {
	AccountID int64
	LabelName string
}

type DeleteLabelExemptionParams struct {
	ID        int64
	AccountID int64
}

type ListAccountsSafeRow struct {
	ID         int64
	Email      string
	AddedAt    string
	LastScanAt *string
	Active     int64
}

type LogEntry struct {
	Level   string
	Message string
}

type HistoryEntry struct {
	AccountID    int64
	AccountEmail string
	MessageID    string
	Subject      string
	Sender       string
	PromptID     *int64
	PromptName   *string
	LabelName    *string
	Actions      string
	LlmResponse  string
	DurationMs   int64
}

type HistoryFilter struct {
	AccountID *int64
	PromptID  *int64
	Unmatched bool
	SubjectQ  string
	SenderQ   string
	Limit     int64
	// Cursor resumes a previous GetHistoryFiltered page: only rows with SK strictly less
	// than this are considered. "" starts from the newest row. See HistoryPage.NextCursor.
	Cursor string
}

// ============================================================
// Error helpers
// ============================================================

func isConditionFailed(err error, target **types.ConditionalCheckFailedException) bool {
	if err == nil {
		return false
	}
	// errors.As unwraps the smithy OperationError that the SDK wraps the
	// ConditionalCheckFailedException in; a plain type assertion would miss it.
	var ccf *types.ConditionalCheckFailedException
	if errors.As(err, &ccf) {
		if target != nil {
			*target = ccf
		}
		return true
	}
	return false
}
