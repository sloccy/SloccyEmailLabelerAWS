package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
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

// Store wraps a DynamoDB client. All methods are safe for concurrent use.
type Store struct {
	ddb   *dynamodb.Client
	table string
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
	return &Store{ddb: dynamodb.NewFromConfig(cfg), table: table}, nil
}

// Now returns the current UTC time in the standard timestamp format.
func Now() string {
	return time.Now().UTC().Format("2006-01-02 15:04:05")
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
			":n": &types.AttributeValueMemberN{Value: strconv.Itoa(n)},
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

// batchDeleteByKey deletes items in batches of 25.
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

// ============================================================
// Settings
// ============================================================

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("META"),
			"SK": sv("SETTING#" + key),
		},
	})
	if err != nil {
		return "", err
	}
	if out.Item == nil {
		return "", fmt.Errorf("setting not found: %s", key)
	}
	return getStr(out.Item, "val"), nil
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
// Secret key (stored as a setting)
// ============================================================

func (s *Store) GetOrCreateSecretKey() ([]byte, error) {
	ctx := context.Background()
	val, err := s.GetSetting(ctx, "secret_key")
	if err == nil {
		b, e := hex.DecodeString(val)
		if e == nil {
			return b, nil
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := s.SetSetting(ctx, SetSettingParams{Key: "secret_key", Value: hex.EncodeToString(key)}); err != nil {
		return nil, err
	}
	return key, nil
}

// ============================================================
// Logs
// ============================================================

func (s *Store) Log(level, message string) {
	_ = s.AddLog(context.Background(), AddLogParams{Level: level, Message: message})
}

func logItem(id int64, ts string, arg LogEntry) map[string]types.AttributeValue {
	item := mustMarshalMap(Log{ID: id, Timestamp: ts, Level: arg.Level, Message: arg.Message})
	item["PK"] = sv("LOG")
	item["SK"] = sv(tsKey(ts, id))
	item[attrTTL] = nv(ttlDays(90)) // generous TTL; TrimLogs is also called
	return item
}

func itemToLog(it map[string]types.AttributeValue) Log {
	var l Log
	if err := attributevalue.UnmarshalMap(it, &l); err != nil {
		slog.Error("unmarshal log", "err", err)
	}
	return l
}

func (s *Store) AddLog(ctx context.Context, arg AddLogParams) error {
	id, err := s.nextID(ctx, "logs")
	if err != nil {
		return err
	}
	_, err = s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      logItem(id, Now(), arg),
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

// TrimLogs deletes log items older than the cutoff timestamp.
func (s *Store) TrimLogs(ctx context.Context, retentionDays int) error {
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays).Format("2006-01-02 15:04:05")
	return s.QueriesTrimLogs(ctx, cutoff)
}

func (s *Store) QueriesTrimLogs(ctx context.Context, cutoff string) error {
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk AND SK < :cutoff"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK:    sv("LOG"),
			":cutoff": sv(cutoff),
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

// ============================================================
// Accounts
// ============================================================

func accountItem(a Account) map[string]types.AttributeValue {
	item := mustMarshalMap(a)
	item["PK"] = sv("ACCOUNT")
	item["SK"] = sv(padID(a.ID))
	return item
}

func itemToAccount(it map[string]types.AttributeValue) Account {
	var a Account
	if err := attributevalue.UnmarshalMap(it, &a); err != nil {
		slog.Error("unmarshal account", "err", err)
	}
	return a
}

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
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("ACCOUNT"),
			"SK": sv(padID(id)),
		},
	})
	if err != nil {
		return Account{}, err
	}
	if out.Item == nil {
		return Account{}, fmt.Errorf("account not found: %d", id)
	}
	return itemToAccount(out.Item), nil
}

func (s *Store) GetAccountByEmail(ctx context.Context, email string) (int64, error) {
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("ACCT_EMAIL#" + email),
			"SK": sv("0"),
		},
	})
	if err != nil {
		return 0, err
	}
	if out.Item == nil {
		return 0, fmt.Errorf("account not found: %s", email)
	}
	return getInt64(out.Item, attrAccountID), nil
}

func (s *Store) UpsertAccount(ctx context.Context, arg UpsertAccountParams) (int64, error) {
	// Try to get existing ID first
	existing, err := s.GetAccountByEmail(ctx, arg.Email)
	if err == nil {
		// Update existing
		a, err2 := s.GetAccount(ctx, existing)
		if err2 != nil {
			return 0, err2
		}
		a.CredentialsJSON = arg.CredentialsJSON
		a.Active = 1
		if _, err3 := s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(s.table),
			Item:      accountItem(a),
		}); err3 != nil {
			return 0, err3
		}
		return existing, nil
	}
	// Create new
	id, err := s.nextID(ctx, "accounts")
	if err != nil {
		return 0, err
	}
	a := Account{
		ID:              id,
		Email:           arg.Email,
		CredentialsJSON: arg.CredentialsJSON,
		AddedAt:         Now(),
		Active:          1,
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
	_, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("ACCOUNT"),
			"SK": sv(padID(arg.ID)),
		},
		UpdateExpression: aws.String("SET creds = :c"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":c": sv(arg.CredentialsJSON),
		},
	})
	return err
}

func (s *Store) UpdateLastScan(ctx context.Context, id int64) error {
	_, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("ACCOUNT"),
			"SK": sv(padID(id)),
		},
		UpdateExpression: aws.String("SET lastScan = :ts"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":ts": sv(Now()),
		},
	})
	return err
}

func (s *Store) UpdateAccountWatch(ctx context.Context, arg UpdateAccountWatchParams) error {
	_, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("ACCOUNT"),
			"SK": sv(padID(arg.ID)),
		},
		UpdateExpression: aws.String("SET watchHist = :h, watchExp = :e"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":h": sv(arg.HistoryID),
			":e": nv(arg.Expiration),
		},
	})
	return err
}

func (s *Store) ToggleAccount(ctx context.Context, id int64) (int64, error) {
	a, err := s.GetAccount(ctx, id)
	if err != nil {
		return 0, err
	}
	if a.Active == 1 {
		a.Active = 0
	} else {
		a.Active = 1
	}
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

func itemToPrompt(it map[string]types.AttributeValue) Prompt {
	var p Prompt
	if err := attributevalue.UnmarshalMap(it, &p); err != nil {
		slog.Error("unmarshal prompt", "err", err)
	}
	return p
}

func promptToItem(p Prompt) map[string]types.AttributeValue {
	item := mustMarshalMap(p)
	item["PK"] = sv("PROMPT")
	item["SK"] = sv(padID(p.ID))
	return item
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

func (s *Store) GetPrompt(ctx context.Context, id int64) (Prompt, error) {
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("PROMPT"),
			"SK": sv(padID(id)),
		},
	})
	if err != nil {
		return Prompt{}, err
	}
	if out.Item == nil {
		return Prompt{}, fmt.Errorf("prompt not found: %d", id)
	}
	return itemToPrompt(out.Item), nil
}

func (s *Store) CreatePrompt(ctx context.Context, arg CreatePromptParams) (int64, error) {
	id, err := s.nextID(ctx, "prompts")
	if err != nil {
		return 0, err
	}
	p := Prompt{
		ID:             id,
		Name:           arg.Name,
		Instructions:   arg.Instructions,
		LabelName:      arg.LabelName,
		Active:         1,
		CreatedAt:      Now(),
		ActionArchive:  arg.ActionArchive,
		ActionSpam:     arg.ActionSpam,
		ActionTrash:    arg.ActionTrash,
		ActionMarkRead: arg.ActionMarkRead,
		SortOrder:      arg.SortOrder,
		StopProcessing: arg.StopProcessing,
		AccountID:      nullInt64Ptr(arg.AccountID),
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
	p.Name = arg.Name
	p.Instructions = arg.Instructions
	p.LabelName = arg.LabelName
	p.ActionArchive = arg.ActionArchive
	p.ActionSpam = arg.ActionSpam
	p.ActionTrash = arg.ActionTrash
	p.ActionMarkRead = arg.ActionMarkRead
	p.StopProcessing = arg.StopProcessing
	p.AccountID = nullInt64Ptr(arg.AccountID)
	_, err = s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      promptToItem(p),
	})
	return err
}

func (s *Store) UpdatePromptInstructions(ctx context.Context, arg UpdatePromptInstructionsParams) error {
	_, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("PROMPT"),
			"SK": sv(padID(arg.ID)),
		},
		UpdateExpression: aws.String("SET instructions = :i"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":i": sv(arg.Instructions),
		},
	})
	return err
}

func (s *Store) UpdatePromptSortOrder(ctx context.Context, arg UpdatePromptSortOrderParams) error {
	_, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("PROMPT"),
			"SK": sv(padID(arg.ID)),
		},
		UpdateExpression: aws.String("SET sortOrder = :s"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":s": nv(arg.SortOrder),
		},
	})
	return err
}

func (s *Store) DeletePrompt(ctx context.Context, id int64) error {
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
		}
	}
	return s.batchDelete(ctx, keys)
}

func (s *Store) TogglePrompt(ctx context.Context, id int64) (int64, error) {
	p, err := s.GetPrompt(ctx, id)
	if err != nil {
		return 0, err
	}
	if p.Active == 1 {
		p.Active = 0
	} else {
		p.Active = 1
	}
	_, err = s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      promptToItem(p),
	})
	return p.Active, err
}

func (s *Store) MaxPromptSortOrder(ctx context.Context) (any, error) {
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

func (s *Store) PromptExistsForAccount(ctx context.Context, arg PromptExistsForAccountParams) (int64, error) {
	all, err := s.listAllPrompts(ctx)
	if err != nil {
		return 0, err
	}
	for _, p := range all {
		if p.Name == arg.Name && accountIDMatches(p.AccountID, arg.AccountID) {
			return 1, nil
		}
	}
	return 0, nil
}

// accountIDMatches reports whether a prompt's (possibly nil) AccountID matches the
// sql.NullInt64 filter value, including the case where both are unset (global prompt,
// no account filter).
func accountIDMatches(p *int64, filter sql.NullInt64) bool {
	if p == nil {
		return !filter.Valid
	}
	return filter.Valid && *p == filter.Int64
}

// ReorderPrompts updates sort_order for each prompt ID in order.
func (s *Store) ReorderPrompts(ctx context.Context, ids []int64) error {
	for i, id := range ids {
		if err := s.UpdatePromptSortOrder(ctx, UpdatePromptSortOrderParams{
			SortOrder: int64(i),
			ID:        id,
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
	var h CategorizationHistory
	if err := attributevalue.UnmarshalMap(it, &h); err != nil {
		slog.Error("unmarshal history", "err", err)
	}
	return h
}

// historyItem builds a history item from id/ts (allocated by the caller) plus a
// HistoryEntry write DTO. ttl isn't part of CategorizationHistory (it's DynamoDB-internal
// expiry, never read back into the model), so it's added after marshaling.
func historyItem(id int64, ts string, arg HistoryEntry) map[string]types.AttributeValue {
	item := mustMarshalMap(CategorizationHistory{
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
	})
	item["PK"] = sv(pkHistory(arg.AccountID))
	item["SK"] = sv(tsKey(ts, id))
	item[attrTTL] = nv(ttlDays(90))
	return item
}

func (s *Store) AddHistory(ctx context.Context, arg AddHistoryParams) error {
	id, err := s.nextID(ctx, "history")
	if err != nil {
		return err
	}
	_, err = s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      historyItem(id, Now(), arg),
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

func (s *Store) GetHistoryFiltered(ctx context.Context, f HistoryFilter) ([]CategorizationHistory, error) {
	var accountIDs []int64
	if f.AccountID != nil {
		accountIDs = []int64{*f.AccountID}
	} else {
		accs, err := s.ListAccounts(ctx)
		if err != nil {
			return nil, err
		}
		for _, a := range accs {
			accountIDs = append(accountIDs, a.ID)
		}
	}

	// With no text/prompt filter to apply in Go, each account's query can be capped at
	// f.Limit directly in DynamoDB: per-account results are already newest-first, so the
	// merged top f.Limit across accounts is still exact. Filtered queries (subject/sender
	// substring, prompt id, unmatched) still read the full partition since matches can be
	// sparse and a DynamoDB-side Limit would cut off pre-filter, not post-filter.
	unfiltered := !f.Unmatched && f.PromptID == nil && f.SubjectQ == "" && f.SenderQ == ""

	var all []CategorizationHistory
	for _, aid := range accountIDs {
		var items []map[string]types.AttributeValue
		var err error
		if unfiltered && f.Limit > 0 {
			var out *dynamodb.QueryOutput
			out, err = s.ddb.Query(ctx, &dynamodb.QueryInput{
				TableName:              aws.String(s.table),
				KeyConditionExpression: aws.String("PK = :pk"),
				ExpressionAttributeValues: map[string]types.AttributeValue{
					exprPK: sv(pkHistory(aid)),
				},
				ScanIndexForward: aws.Bool(false),
				Limit:            aws.Int32(i32(f.Limit)),
			})
			if out != nil {
				items = out.Items
			}
		} else {
			items, err = s.queryAll(ctx, &dynamodb.QueryInput{
				TableName:              aws.String(s.table),
				KeyConditionExpression: aws.String("PK = :pk"),
				ExpressionAttributeValues: map[string]types.AttributeValue{
					exprPK: sv(pkHistory(aid)),
				},
				ScanIndexForward: aws.Bool(false),
			})
		}
		if err != nil {
			continue
		}
		for _, it := range items {
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

	// Sort all results newest first
	sort.Slice(all, func(i, j int) bool {
		if all[i].Timestamp != all[j].Timestamp {
			return all[i].Timestamp > all[j].Timestamp
		}
		return all[i].ID > all[j].ID
	})

	// Apply filters in Go
	var filtered []CategorizationHistory
	for _, h := range all {
		if f.Unmatched && h.PromptID != nil {
			continue
		}
		if f.PromptID != nil && (h.PromptID == nil || *h.PromptID != *f.PromptID) {
			continue
		}
		if f.SubjectQ != "" && !strings.Contains(strings.ToLower(h.Subject), strings.ToLower(f.SubjectQ)) {
			continue
		}
		if f.SenderQ != "" && !strings.Contains(strings.ToLower(h.Sender), strings.ToLower(f.SenderQ)) {
			continue
		}
		filtered = append(filtered, h)
		if f.Limit > 0 && int64(len(filtered)) >= f.Limit {
			break
		}
	}
	return filtered, nil
}

// GetHistory returns the N most recent history rows across all accounts.
func (s *Store) GetHistory(ctx context.Context, limit int64) ([]CategorizationHistory, error) {
	return s.GetHistoryFiltered(ctx, HistoryFilter{Limit: limit})
}

func (s *Store) DeleteHistoryByAccount(ctx context.Context, accountID int64) error {
	return s.deleteAllByPK(ctx, pkHistory(accountID))
}

func (s *Store) TrimHistory(ctx context.Context, retentionDays int) error {
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays).Format("2006-01-02 15:04:05")
	accs, err := s.ListAccounts(ctx)
	if err != nil {
		return err
	}
	for _, acc := range accs {
		items, err := s.queryAll(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(s.table),
			KeyConditionExpression: aws.String("PK = :pk AND SK < :cutoff"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				exprPK:    sv(pkHistory(acc.ID)),
				":cutoff": sv(cutoff),
			},
			ProjectionExpression: aws.String("PK, SK"),
		})
		if err != nil {
			continue
		}
		keys := make([]map[string]types.AttributeValue, len(items))
		for i, it := range items {
			keys[i] = map[string]types.AttributeValue{"PK": it["PK"], "SK": it["SK"]}
		}
		_ = s.batchDelete(ctx, keys)
	}
	return nil
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

// ============================================================
// Processed emails
// ============================================================

func (s *Store) MarkProcessed(ctx context.Context, arg MarkProcessedParams) error {
	if arg.MessageID == "" {
		return nil
	}
	_, err := s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]types.AttributeValue{
			"PK":    sv(pkProcessed(arg.AccountID)),
			"SK":    sv(arg.MessageID),
			attrTTL: nv(ttlDays(7)), // keep processed record for 7 days (2x lookback default)
		},
		ConditionExpression: aws.String("attribute_not_exists(PK)"),
	})
	if err != nil {
		var ccf *types.ConditionalCheckFailedException
		if isConditionFailed(err, &ccf) {
			return nil // already marked
		}
		return err
	}
	return nil
}

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
	processed := map[string]bool{}
	// BatchGetItem in chunks of 100
	for i := 0; i < len(keys); i += 100 {
		end := min(i+100, len(keys))
		out, err := s.ddb.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{
			RequestItems: map[string]types.KeysAndAttributes{
				s.table: {Keys: keys[i:end]},
			},
		})
		if err != nil {
			return nil, err
		}
		for _, it := range out.Responses[s.table] {
			mid := getStr(it, "SK")
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

func (s *Store) TrimProcessedEmails(ctx context.Context, lookbackHours int) error {
	// DynamoDB TTL handles cleanup; this is a no-op.
	return nil
}

// BatchInsertProcessingResults persists logs, history, and marks the email processed.
// BatchInsertProcessingResults writes one email's worth of log lines and history entries.
// IDs are reserved for the whole group in a single counter increment per entity (instead
// of one per item) and all items are written via BatchWriteItem — cutting what was ~2
// DynamoDB writes per log/history line down to one counter update per entity plus one
// batched write per 25 items.
func (s *Store) BatchInsertProcessingResults(ctx context.Context, logs []LogEntry, history []HistoryEntry, accountID int64, messageID string) error {
	ts := Now()
	items := make([]map[string]types.AttributeValue, 0, len(logs)+len(history))

	if len(logs) > 0 {
		start, err := s.nextIDs(ctx, "logs", len(logs))
		if err != nil {
			return err
		}
		for i, l := range logs {
			items = append(items, logItem(start+int64(i), ts, l))
		}
	}
	if len(history) > 0 {
		start, err := s.nextIDs(ctx, "history", len(history))
		if err != nil {
			return err
		}
		for i, h := range history {
			items = append(items, historyItem(start+int64(i), ts, h))
		}
	}
	if len(items) > 0 {
		if err := s.batchPut(ctx, items); err != nil {
			return err
		}
	}
	if messageID != "" {
		if err := s.MarkProcessed(ctx, MarkProcessedParams{AccountID: accountID, MessageID: messageID}); err != nil {
			return err
		}
	}
	return nil
}

// ============================================================
// Retention
// ============================================================

func (s *Store) GetAccountRetention(ctx context.Context, accountID int64) (AccountRetention, error) {
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("RETENTION"),
			"SK": sv(padID(accountID)),
		},
	})
	if err != nil {
		return AccountRetention{}, err
	}
	if out.Item == nil {
		return AccountRetention{AccountID: accountID}, errors.New("not found")
	}
	return AccountRetention{
		AccountID:  accountID,
		GlobalDays: getNullInt64(out.Item, "globalDays"),
	}, nil
}

func (s *Store) HasGlobalRetention(ctx context.Context, accountID int64) (int64, error) {
	r, err := s.GetAccountRetention(ctx, accountID)
	//nolint:nilerr // a missing or unreadable retention record means "no global retention"
	if err != nil || !r.GlobalDays.Valid {
		return 0, nil
	}
	return 1, nil
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
			attrAccountID: nv(arg.AccountID),
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

func (s *Store) LabelRetentionExists(ctx context.Context, arg LabelRetentionExistsParams) (int64, error) {
	items, err := s.GetLabelRetention(ctx, arg.AccountID)
	if err != nil {
		return 0, err
	}
	for _, r := range items {
		if r.LabelName == arg.LabelName {
			return 1, nil
		}
	}
	return 0, nil
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
			attrAccountID: nv(arg.AccountID),
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

func (s *Store) GetLatestCorrectionForMessage(ctx context.Context, messageID string) (EmailCorrection, error) {
	out, err := s.ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK: sv("CORRECTION#" + messageID),
		},
		ScanIndexForward: aws.Bool(false),
		Limit:            aws.Int32(1),
	})
	if err != nil {
		return EmailCorrection{}, err
	}
	if len(out.Items) == 0 {
		return EmailCorrection{}, fmt.Errorf("no correction for message %s", messageID)
	}
	it := out.Items[0]
	return EmailCorrection{
		ID:               getInt64(it, "id"),
		CreatedAt:        getStr(it, attrCreatedAt),
		AccountID:        getInt64(it, attrAccountID),
		MessageID:        getStr(it, attrMessageID),
		AddedPrompts:     getStr(it, "addedPrompts"),
		RemovedPrompts:   getStr(it, "removedPrompts"),
		CurrentPromptIds: getStr(it, "currentPromptIds"),
		Note:             getStr(it, "note"),
	}, nil
}

// ============================================================
// Prompt suggestions
// ============================================================

func itemToSuggestion(it map[string]types.AttributeValue) PromptSuggestion {
	var sg PromptSuggestion
	if err := attributevalue.UnmarshalMap(it, &sg); err != nil {
		slog.Error("unmarshal suggestion", "err", err)
	}
	return sg
}

func suggestionItem(id int64, ts string, arg InsertPromptSuggestionParams) map[string]types.AttributeValue {
	item := mustMarshalMap(PromptSuggestion{
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
	})
	item["PK"] = sv("SUGGESTION")
	item["SK"] = sv(padID(id))
	return item
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
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("SUGGESTION"),
			"SK": sv(padID(id)),
		},
	})
	if err != nil {
		return PromptSuggestion{}, err
	}
	if out.Item == nil {
		return PromptSuggestion{}, fmt.Errorf("suggestion not found: %d", id)
	}
	return itemToSuggestion(out.Item), nil
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
		s := itemToSuggestion(it)
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
	_, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("SUGGESTION"),
			"SK": sv(padID(arg.ID)),
		},
		UpdateExpression: aws.String("SET suggestedInstructions = :si, conversationJson = :cj, #s = :st, userComment = :uc, updatedAt = :ua"),
		ExpressionAttributeNames: map[string]string{
			"#s": attrStatus,
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":si":         sv(arg.SuggestedInstructions),
			":cj":         sv(arg.ConversationJSON),
			exprStatus:    sv(arg.Status),
			":uc":         sv(arg.UserComment),
			exprUpdatedAt: sv(Now()),
		},
	})
	return err
}

func (s *Store) UpdatePromptSuggestion(ctx context.Context, arg UpdatePromptSuggestionParams) error {
	_, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("SUGGESTION"),
			"SK": sv(padID(arg.ID)),
		},
		UpdateExpression: aws.String("SET suggestedInstructions = :si, conversationJson = :cj, userComment = :uc, #s = :st, updatedAt = :ua"),
		ExpressionAttributeNames: map[string]string{
			"#s": attrStatus,
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":si":         sv(arg.SuggestedInstructions),
			":cj":         sv(arg.ConversationJSON),
			":uc":         sv(arg.UserComment),
			exprStatus:    sv(SuggestionStatusPending),
			exprUpdatedAt: sv(Now()),
		},
	})
	return err
}

func (s *Store) ApplyPromptSuggestion(ctx context.Context, id int64) error {
	_, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("SUGGESTION"),
			"SK": sv(padID(id)),
		},
		UpdateExpression: aws.String("SET #s = :st, updatedAt = :ua"),
		ExpressionAttributeNames: map[string]string{
			"#s": attrStatus,
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprStatus:    sv(SuggestionStatusApplied),
			exprUpdatedAt: sv(Now()),
		},
	})
	return err
}

func (s *Store) DismissPromptSuggestion(ctx context.Context, id int64) error {
	_, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"PK": sv("SUGGESTION"),
			"SK": sv(padID(id)),
		},
		UpdateExpression: aws.String("SET #s = :st, updatedAt = :ua"),
		ExpressionAttributeNames: map[string]string{
			"#s": attrStatus,
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprStatus:    sv(SuggestionStatusDismissed),
			exprUpdatedAt: sv(Now()),
		},
	})
	return err
}

// ApplyPromptSuggestionAndUpdatePrompt applies a suggestion and updates the prompt in sequence.
func (s *Store) ApplyPromptSuggestionAndUpdatePrompt(ctx context.Context, suggestionID int64, promptID int64, newInstructions string) error {
	if err := s.UpdatePromptInstructions(ctx, UpdatePromptInstructionsParams{
		Instructions: newInstructions, ID: promptID,
	}); err != nil {
		return err
	}
	return s.ApplyPromptSuggestion(ctx, suggestionID)
}

// ============================================================
// LLM Debug
// ============================================================

func llmDebugItem(id int64, ts string, arg AddLlmDebugParams) map[string]types.AttributeValue {
	item := mustMarshalMap(LlmDebug{
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
	})
	item["PK"] = sv("LLM_DEBUG")
	item["SK"] = sv(padID(id))
	return item
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

func itemToLlmDebug(it map[string]types.AttributeValue) LlmDebug {
	var d LlmDebug
	if err := attributevalue.UnmarshalMap(it, &d); err != nil {
		slog.Error("unmarshal llm debug", "err", err)
	}
	return d
}

func (s *Store) DeleteIncompleteLlmDebug(ctx context.Context) error {
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("PK = :pk"),
		FilterExpression:       aws.String("gmailRaw = :empty OR llmRequest = :empty"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			exprPK:   sv("LLM_DEBUG"),
			":empty": sv(""),
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

type SeedSettingParams struct {
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

type UpdatePromptInstructionsParams struct {
	Instructions string
	ID           int64
}

type UpdatePromptSortOrderParams struct {
	SortOrder int64
	ID        int64
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

type MarkProcessedParams struct {
	AccountID int64
	MessageID string
}

type GetLogsRangeParams struct {
	Timestamp  string
	Timestamp2 string
}

type GetHistoryByAccountParams struct {
	AccountID int64
	Limit     int64
}

type GetHistoryByPromptParams struct {
	PromptID sql.NullInt64
	Limit    int64
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

type LabelRetentionExistsParams struct {
	AccountID int64
	LabelName string
}

type PromptExistsForAccountParams struct {
	Name      string
	AccountID sql.NullInt64
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
}

type HistoryFilter struct {
	AccountID *int64
	PromptID  *int64
	Unmatched bool
	SubjectQ  string
	SenderQ   string
	Limit     int64
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
