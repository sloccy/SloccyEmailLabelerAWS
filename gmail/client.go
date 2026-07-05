package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const (
	LabelInbox  = "INBOX"
	LabelUnread = "UNREAD"
	LabelSpam   = "SPAM"
	LabelTrash  = "TRASH"
)

// PageSize is the maxResults page size FetchEmailsOlderThan requests per page. Exported so
// callers paging through its results (retention's "fewer than a full page came back" last-
// page check) share the same constant instead of a duplicated magic number.
const PageSize = 500

var gmailBase = "https://gmail.googleapis.com/gmail/v1/users/me"

var retryBaseBackoff = time.Second

// retryTransport wraps an http.RoundTripper with retry logic for 429 and 5xx.
type retryTransport struct {
	base http.RoundTripper
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var resp *http.Response
	var err error
	backoff := retryBaseBackoff
	for attempt := range 3 {
		if attempt > 0 {
			jitter := time.Duration(rand.Int63n(int64(backoff / 2))) //nolint:gosec // G404: crypto rand unnecessary for jitter
			time.Sleep(backoff + jitter)
			backoff *= 2
			// Reset the request body so retries send the full payload.
			if req.GetBody != nil {
				req.Body, _ = req.GetBody()
			}
		}
		resp, err = t.base.RoundTrip(req)
		if err != nil {
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			_ = resp.Body.Close()
			continue
		}
		return resp, nil
	}
	return resp, err
}

// Client is an authenticated Gmail REST client.
type Client struct {
	http *http.Client
}

// NewService creates an authenticated Gmail client for the given stored credentials JSON.
// If the token is refreshed, onRefresh is called with the new token JSON.
func NewService(ctx context.Context, credJSON string, oauthCfg *oauth2.Config, onRefresh func(string)) (*Client, error) {
	token, err := TokenFromJSON(credJSON)
	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}

	ts := &refreshingTokenSource{
		base:      oauthCfg.TokenSource(ctx, token),
		original:  token,
		onRefresh: onRefresh,
	}

	httpClient := &http.Client{
		Transport: &retryTransport{
			base: &oauth2.Transport{
				Source: ts,
				Base:   http.DefaultTransport,
			},
		},
	}

	return &Client{http: httpClient}, nil
}

// refreshingTokenSource calls onRefresh when the token changes.
type refreshingTokenSource struct {
	mu        sync.Mutex
	base      oauth2.TokenSource
	original  *oauth2.Token
	onRefresh func(string)
}

func (s *refreshingTokenSource) Token() (*oauth2.Token, error) {
	t, err := s.base.Token()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	changed := s.original == nil || t.AccessToken != s.original.AccessToken
	s.original = t
	s.mu.Unlock()
	if changed && s.onRefresh != nil {
		marshalToken(t, s.onRefresh)
	}
	return t, nil
}

func marshalToken(t *oauth2.Token, fn func(string)) {
	if b, err := json.Marshal(t); err == nil { //nolint:gosec // G117: token serialization is intentional
		fn(string(b))
	}
}

// ServiceWrapper holds a Gmail client.
type ServiceWrapper struct {
	Svc *Client
}

// WatchResult is the response from users.watch.
type WatchResult struct {
	HistoryID  string // Gmail's uint64 history id (JSON string)
	Expiration int64  // epoch millis when the watch lapses (~7 days out)
}

// Watch registers/renews a Gmail push subscription for INBOX changes, delivered to
// the given Cloud Pub/Sub topic (projects/<proj>/topics/<name>). Gmail expires the
// watch after ~7 days, so callers renew it on a schedule.
func (c *Client) Watch(ctx context.Context, topicName string) (WatchResult, error) {
	req := map[string]any{
		"topicName":           topicName,
		"labelIds":            []string{LabelInbox},
		"labelFilterBehavior": "INCLUDE",
	}
	var res struct {
		HistoryID  string `json:"historyId"`
		Expiration string `json:"expiration"`
	}
	if err := c.post(ctx, "/watch", req, &res); err != nil {
		return WatchResult{}, err
	}
	exp, _ := strconv.ParseInt(res.Expiration, 10, 64)
	return WatchResult{HistoryID: res.HistoryID, Expiration: exp}, nil
}

// --- local JSON types (only fields we use) ---

type apiLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type apiListLabelsResponse struct {
	Labels []apiLabel `json:"labels"`
}

type apiMessageRef struct {
	ID string `json:"id"`
}

type apiListMessagesResponse struct {
	Messages      []apiMessageRef `json:"messages"`
	NextPageToken string          `json:"nextPageToken"`
}

type apiMessagePartBody struct {
	Data         string `json:"data"`
	AttachmentID string `json:"attachmentId"`
	Size         int    `json:"size"`
}

type apiMessagePart struct {
	MimeType string `json:"mimeType"`
	Headers  []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
	Parts []apiMessagePart    `json:"parts"`
	Body  *apiMessagePartBody `json:"body"`
}

type apiMessage struct {
	ID      string          `json:"id"`
	Snippet string          `json:"snippet"`
	Payload *apiMessagePart `json:"payload"`
}

type apiBatchModifyRequest struct {
	IDs            []string `json:"ids"`
	AddLabelIDs    []string `json:"addLabelIds"`
	RemoveLabelIDs []string `json:"removeLabelIds"`
}

// --- HTTP helpers ---

func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	u := gmailBase + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gmail API %s: %s", path, body)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) post(ctx context.Context, path string, in any, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gmailBase+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gmail API %s: %s", path, b)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// --- Label is a Gmail label id/name pair ---

type Label struct {
	ID   string
	Name string
}

// ListLabels returns all labels for the account, sorted by name.
func ListLabels(ctx context.Context, svc *Client) ([]Label, error) {
	var res apiListLabelsResponse
	if err := svc.get(ctx, "/labels", nil, &res); err != nil {
		return nil, err
	}
	labels := make([]Label, 0, len(res.Labels))
	for _, l := range res.Labels {
		labels = append(labels, Label(l))
	}
	sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })
	return labels, nil
}

// BuildLabelCache returns a map of label name -> label ID, creating missing labels.
func BuildLabelCache(ctx context.Context, svc *Client, needed []string) (map[string]string, error) {
	existing, err := ListLabels(ctx, svc)
	if err != nil {
		return nil, err
	}
	cache := make(map[string]string, len(existing))
	for _, l := range existing {
		cache[l.Name] = l.ID
	}
	for _, name := range needed {
		if _, ok := cache[name]; ok {
			continue
		}
		var created apiLabel
		if err := svc.post(ctx, "/labels", map[string]string{"name": name}, &created); err != nil {
			return nil, fmt.Errorf("create label %q: %w", name, err)
		}
		cache[name] = created.ID
	}
	return cache, nil
}

// LabelCacheTTL bounds how long a cached per-account label map is reused.
const LabelCacheTTL = 10 * time.Minute

type labelCacheEntry struct {
	labels    map[string]string
	fetchedAt time.Time
}

var (
	labelCacheMu    sync.Mutex
	labelCacheStore = map[int64]labelCacheEntry{}
)

// BuildLabelCacheFor is a caching wrapper over BuildLabelCache keyed by account ID. The scan
// process stays warm across EventBridge invocations and Gmail labels change rarely, so within
// LabelCacheTTL this reuses the map and skips the ListLabels round-trip — as long as every
// needed label is already present. A missing needed label (or an expired entry) forces a
// rebuild, which also creates the label. Labels are per-account, hence the account-ID key.
func BuildLabelCacheFor(ctx context.Context, svc *Client, accountID int64, needed []string) (map[string]string, error) {
	labelCacheMu.Lock()
	entry, ok := labelCacheStore[accountID]
	if ok && time.Since(entry.fetchedAt) < LabelCacheTTL && hasAll(entry.labels, needed) {
		out := cloneLabels(entry.labels)
		labelCacheMu.Unlock()
		return out, nil
	}
	labelCacheMu.Unlock()

	labels, err := BuildLabelCache(ctx, svc, needed)
	if err != nil {
		return nil, err
	}
	labelCacheMu.Lock()
	labelCacheStore[accountID] = labelCacheEntry{labels: cloneLabels(labels), fetchedAt: time.Now()}
	labelCacheMu.Unlock()
	return labels, nil
}

func hasAll(m map[string]string, needed []string) bool {
	for _, n := range needed {
		if n == "" {
			continue
		}
		if _, ok := m[n]; !ok {
			return false
		}
	}
	return true
}

func cloneLabels(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	maps.Copy(out, m)
	return out
}

// EnsureLabel creates a label if it doesn't exist. Safe to call concurrently.
func EnsureLabel(ctx context.Context, svc *Client, name string) error {
	labels, err := ListLabels(ctx, svc)
	if err != nil {
		return err
	}
	for _, l := range labels {
		if l.Name == name {
			return nil
		}
	}
	return svc.post(ctx, "/labels", map[string]string{"name": name}, nil)
}

// ErrHistoryTooOld is returned by ListHistoryAddedMessageIDs when the supplied
// startHistoryId has aged out of Gmail's history window (HTTP 404). Callers should
// fall back to a full lookback scan and reset their stored history id.
var ErrHistoryTooOld = errors.New("gmail: startHistoryId too old")

type apiHistoryMessageAdded struct {
	Message apiMessageRef `json:"message"`
}

type apiHistoryRecord struct {
	MessagesAdded []apiHistoryMessageAdded `json:"messagesAdded"`
}

type apiHistoryResponse struct {
	History       []apiHistoryRecord `json:"history"`
	NextPageToken string             `json:"nextPageToken"`
	HistoryID     string             `json:"historyId"`
}

// ListHistoryAddedMessageIDs returns the IDs of messages *added to the inbox* since
// startHistoryID, plus Gmail's latest history id (the next start point). It filters to
// messageAdded events on INBOX, so label/read changes the app makes itself are ignored —
// that's what keeps push notifications from retriggering on our own modifications.
// Returns ErrHistoryTooOld if startHistoryID has expired.
func ListHistoryAddedMessageIDs(ctx context.Context, svc *Client, startHistoryID string) ([]string, string, error) {
	var ids []string
	seen := map[string]bool{}
	latest := startHistoryID
	pageToken := ""
	for {
		params := url.Values{
			"startHistoryId": {startHistoryID},
			"historyTypes":   {"messageAdded"},
			"labelId":        {LabelInbox},
		}
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, gmailBase+"/history?"+params.Encode(), nil)
		if err != nil {
			return nil, latest, err
		}
		resp, err := svc.http.Do(req)
		if err != nil {
			return nil, latest, err
		}
		if resp.StatusCode == http.StatusNotFound {
			_ = resp.Body.Close()
			return nil, latest, ErrHistoryTooOld
		}
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return nil, latest, fmt.Errorf("gmail API /history: %s", body)
		}
		var res apiHistoryResponse
		derr := json.NewDecoder(resp.Body).Decode(&res)
		_ = resp.Body.Close()
		if derr != nil {
			return nil, latest, derr
		}
		for _, h := range res.History {
			for _, ma := range h.MessagesAdded {
				if id := ma.Message.ID; id != "" && !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
		if res.HistoryID != "" {
			latest = res.HistoryID
		}
		pageToken = res.NextPageToken
		if pageToken == "" {
			break
		}
	}
	return ids, latest, nil
}

// ListRecentMessageIDs returns message IDs from the inbox for the last lookbackHours hours.
func ListRecentMessageIDs(ctx context.Context, svc *Client, lookbackHours int, maxResults int64) ([]string, error) {
	after := time.Now().UTC().Add(-time.Duration(lookbackHours) * time.Hour)
	q := fmt.Sprintf("in:inbox after:%d", after.Unix())
	return paginateMessageIDs(ctx, svc, q, maxResults, 0)
}

func paginateMessageIDs(ctx context.Context, svc *Client, q string, maxResults int64, maxPages int) ([]string, error) {
	var ids []string
	var pageToken string
	page := 0
	for {
		params := url.Values{
			"q":          {q},
			"maxResults": {strconv.FormatInt(maxResults, 10)},
		}
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}
		var res apiListMessagesResponse
		if err := svc.get(ctx, "/messages", params, &res); err != nil {
			return nil, err
		}
		for _, m := range res.Messages {
			ids = append(ids, m.ID)
		}
		pageToken = res.NextPageToken
		page++
		if pageToken == "" || (maxPages > 0 && page >= maxPages) {
			break
		}
	}
	return ids, nil
}

// Message is a simplified view of a Gmail message.
type Message struct {
	ID      string `json:"id"`
	Sender  string `json:"sender"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	Snippet string `json:"snippet"`
}

// IterMessageDetails fetches full message details concurrently (up to 10 at a time).
func IterMessageDetails(ctx context.Context, svc *Client, ids []string, maxBodyChars int) (<-chan Message, <-chan error) {
	msgCh := make(chan Message, 5)
	errCh := make(chan error, 1)

	go func() {
		defer close(msgCh)
		defer close(errCh)

		sem := make(chan struct{}, 10)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var firstErr error

		for _, id := range ids {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				msg, err := fetchMessage(ctx, svc, id, maxBodyChars)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					return
				}
				select {
				case msgCh <- msg:
				case <-ctx.Done():
				}
			}()
		}
		wg.Wait()
		if firstErr != nil {
			errCh <- firstErr
		}
	}()
	return msgCh, errCh
}

// FetchMessage retrieves a single message's subject, sender, and body by ID.
// maxBodyChars limits the body length; pass 0 to use the default (4000 chars).
func FetchMessage(ctx context.Context, svc *Client, id string, maxBodyChars int) (Message, error) {
	if maxBodyChars <= 0 {
		maxBodyChars = 4000
	}
	return fetchMessage(ctx, svc, id, maxBodyChars)
}

func fetchMessage(ctx context.Context, svc *Client, id string, maxBodyChars int) (Message, error) {
	var m apiMessage
	if err := svc.get(ctx, "/messages/"+id, url.Values{"format": {"full"}}, &m); err != nil {
		return Message{}, err
	}
	msg := Message{
		ID:      id,
		Snippet: m.Snippet,
	}
	if m.Payload != nil {
		for _, h := range m.Payload.Headers {
			switch h.Name {
			case "From":
				msg.Sender = h.Value
			case "Subject":
				msg.Subject = h.Value
			}
		}
		msg.Body = extractPayloadBody(ctx, svc, id, m.Payload, maxBodyChars)
	}
	return msg, nil
}

func extractPayloadBody(ctx context.Context, svc *Client, msgID string, payload *apiMessagePart, maxChars int) string {
	if payload == nil {
		return ""
	}
	return Truncate(extractBodyRecursive(ctx, svc, msgID, payload, maxChars*3), maxChars)
}

func extractBodyRecursive(ctx context.Context, svc *Client, msgID string, part *apiMessagePart, maxChars int) string {
	if part == nil {
		return ""
	}
	mimeType := strings.ToLower(part.MimeType)

	if len(part.Parts) == 0 {
		// Only process text/* parts — skip image/*, application/*, etc.
		if !strings.HasPrefix(mimeType, "text/") {
			return ""
		}
		// Leaf node — get the raw base64 data, fetching via attachment API if not inline
		rawData := ""
		if part.Body != nil {
			rawData = part.Body.Data
			if rawData == "" && part.Body.AttachmentID != "" {
				var att struct {
					Data string `json:"data"`
				}
				if err := svc.get(ctx, "/messages/"+msgID+"/attachments/"+part.Body.AttachmentID, nil, &att); err != nil {
					slog.Debug("attachment fetch failed", "msg", msgID, "att", part.Body.AttachmentID, "err", err)
				} else {
					rawData = att.Data
				}
			}
		}
		if rawData == "" {
			return ""
		}
		data, err := base64.URLEncoding.DecodeString(rawData)
		if err != nil {
			data, err = base64.StdEncoding.DecodeString(rawData)
			if err != nil {
				return ""
			}
		}
		text := string(data)
		if strings.Contains(mimeType, "html") {
			text = extractText(text)
		} else {
			text = cleanInvisibles(text)
		}
		return Truncate(text, maxChars)
	}

	// Prefer html-processed output over plaintext. extractText already strips
	// <style>/<script> blocks and all tags, so html-processed has a known lower
	// bound on cleanliness. text/plain alternatives from broken senders (e.g.
	// CSS dumps) have no equivalent sanitizer and can fill the LLM context with
	// garbage. Fall back to the best plaintext only when html is absent or
	// trivially short (< 40 chars), to handle degenerate html-only stubs.
	const htmlFloor = 40
	var htmlBest, plainBest string
	for _, p := range part.Parts {
		t := extractBodyRecursive(ctx, svc, msgID, &p, maxChars)
		if t == "" {
			continue
		}
		childMime := strings.ToLower(p.MimeType)
		switch {
		case strings.Contains(childMime, "html"):
			if len(t) > len(htmlBest) {
				htmlBest = t
			}
		case strings.HasPrefix(childMime, "text/"):
			if len(t) > len(plainBest) {
				plainBest = t
			}
		default:
			// Nested multipart — recursive result already applied this rule;
			// treat as html so it beats a sibling plaintext stub.
			if len(t) > len(htmlBest) {
				htmlBest = t
			}
		}
	}
	if len(htmlBest) >= htmlFloor {
		return htmlBest
	}
	if plainBest != "" {
		return plainBest
	}
	return htmlBest
}

// Modify represents a set of label changes to apply to messages.
type Modify struct {
	MessageIDs   []string
	AddLabels    []string
	RemoveLabels []string
}

// postBatchModify posts /messages/batchModify in chunks of at most 1000 ids (the API's
// per-call limit), applying the same add/remove label sets to every chunk. Shared by
// BatchModifyEmails and BatchTrashEmails so the chunking loop can't drift between them.
func postBatchModify(ctx context.Context, svc *Client, ids, add, remove []string) error {
	for len(ids) > 0 {
		batch := ids
		if len(batch) > 1000 {
			batch = batch[:1000]
		}
		if err := svc.post(ctx, "/messages/batchModify", &apiBatchModifyRequest{
			IDs:            batch,
			AddLabelIDs:    add,
			RemoveLabelIDs: remove,
		}, nil); err != nil {
			return err
		}
		ids = ids[len(batch):]
	}
	return nil
}

// BatchModifyEmails applies label changes, grouped by identical add/remove sets.
func BatchModifyEmails(ctx context.Context, svc *Client, mods []Modify) error {
	type key struct{ add, remove string }
	grouped := make(map[key]*apiBatchModifyRequest)

	for _, m := range mods {
		add := strings.Join(m.AddLabels, ",")
		remove := strings.Join(m.RemoveLabels, ",")
		k := key{add, remove}
		if _, ok := grouped[k]; !ok {
			grouped[k] = &apiBatchModifyRequest{
				AddLabelIDs:    m.AddLabels,
				RemoveLabelIDs: m.RemoveLabels,
			}
		}
		grouped[k].IDs = append(grouped[k].IDs, m.MessageIDs...)
	}

	for _, req := range grouped {
		if err := postBatchModify(ctx, svc, req.IDs, req.AddLabelIDs, req.RemoveLabelIDs); err != nil {
			return err
		}
	}
	return nil
}

// BatchTrashEmails moves messages to trash.
func BatchTrashEmails(ctx context.Context, svc *Client, ids []string) error {
	return postBatchModify(ctx, svc, ids, []string{LabelTrash}, []string{LabelInbox})
}

// FetchEmailsOlderThan returns message IDs older than `days` days with the given label,
// excluding any labels in excludeLabels.
func FetchEmailsOlderThan(ctx context.Context, svc *Client, days int, label string, excludeLabels []string, maxPages int) ([]string, error) {
	before := time.Now().UTC().AddDate(0, 0, -days)
	var sb strings.Builder
	fmt.Fprintf(&sb, "before:%s", before.Format("2006/01/02"))
	if label != "" {
		sb.WriteString(" label:")
		sb.WriteString(label)
	}
	for _, ex := range excludeLabels {
		sb.WriteString(" -label:")
		sb.WriteString(ex)
	}
	return paginateMessageIDs(ctx, svc, sb.String(), PageSize, maxPages)
}
