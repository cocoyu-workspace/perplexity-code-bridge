package proxy

import (
	"compress/flate"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

const (
	DefaultClientVersion = "23.13.20260324.2054"
	AccountRolePrimary   = "primary"
	AccountRoleEducation = "education"
	rankUnknown          = -1
)

var (
	ErrQuotaExhausted            = errors.New("account quota exhausted")
	ErrResearchQuotaExhausted    = errors.New("research quota exhausted")
	ErrPremiumFeatureUnavailable = errors.New("premium feature unavailable")
	ErrEmptyResponse             = errors.New("empty response from upstream")
	ErrInvalidModelConfig        = errors.New("invalid model or reasoning effort")
)

type agentModelError struct {
	model  string
	effort string
}

func (e *agentModelError) Error() string {
	if e.effort != "" {
		return fmt.Sprintf("unsupported reasoning effort %q for model %q", e.effort, e.model)
	}
	return fmt.Sprintf("unsupported model %q", e.model)
}

func (e *agentModelError) Unwrap() error {
	return ErrInvalidModelConfig
}

type accountRank struct {
	layer int
	plan  int
	rem   int
}

func (a accountRank) betterThan(b accountRank) bool {
	if a.layer != b.layer {
		return a.layer > b.layer
	}
	if a.plan != b.plan {
		return a.plan > b.plan
	}
	return a.rem > b.rem
}

type ModelEntry struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

type QuotaInfo struct {
	IsEligible        bool  `json:"is_eligible"`
	LegacyEligible    bool  `json:"legacy_eligible"`
	SpaceUsage        int   `json:"space_usage"`
	SpaceLimit        int   `json:"space_limit"`
	UserUsage         int   `json:"user_usage"`
	UserLimit         int   `json:"user_limit"`
	LastUsageAtMs     int64 `json:"last_usage_at"`
	ResearchModeUsage int   `json:"research_mode_usage"`
	HasPremium        bool  `json:"has_premium"`
	PremiumBalance    int   `json:"premium_balance"`
	PremiumUsage      int   `json:"premium_usage"`
	PremiumLimit      int   `json:"premium_limit"`
}

func (q *QuotaInfo) UnmarshalJSON(data []byte) error {
	type Alias QuotaInfo
	var raw struct {
		Alias
		CamelIsEligible     *bool `json:"isEligible"`
		CamelSpaceUsage     *int  `json:"spaceUsage"`
		CamelSpaceLimit     *int  `json:"spaceLimit"`
		CamelUserUsage      *int  `json:"userUsage"`
		CamelUserLimit      *int  `json:"userLimit"`
		CamelHasPremium     *bool `json:"hasPremium"`
		CamelPremiumBalance *int  `json:"premiumBalance"`
		CamelPremiumUsage   *int  `json:"premiumUsage"`
		CamelPremiumLimit   *int  `json:"premiumLimit"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*q = QuotaInfo(raw.Alias)
	if raw.CamelIsEligible != nil && !q.IsEligible {
		q.IsEligible = *raw.CamelIsEligible
	}
	if raw.CamelSpaceUsage != nil && q.SpaceUsage == 0 {
		q.SpaceUsage = *raw.CamelSpaceUsage
	}
	if raw.CamelSpaceLimit != nil && q.SpaceLimit == 0 {
		q.SpaceLimit = *raw.CamelSpaceLimit
	}
	if raw.CamelUserUsage != nil && q.UserUsage == 0 {
		q.UserUsage = *raw.CamelUserUsage
	}
	if raw.CamelUserLimit != nil && q.UserLimit == 0 {
		q.UserLimit = *raw.CamelUserLimit
	}
	if raw.CamelHasPremium != nil && !q.HasPremium {
		q.HasPremium = *raw.CamelHasPremium
	}
	if raw.CamelPremiumBalance != nil && q.PremiumBalance == 0 {
		q.PremiumBalance = *raw.CamelPremiumBalance
	}
	if raw.CamelPremiumUsage != nil && q.PremiumUsage == 0 {
		q.PremiumUsage = *raw.CamelPremiumUsage
	}
	if raw.CamelPremiumLimit != nil && q.PremiumLimit == 0 {
		q.PremiumLimit = *raw.CamelPremiumLimit
	}
	return nil
}

func basicRemaining(q *QuotaInfo) int {
	if q == nil {
		return 0
	}
	if q.SpaceLimit > 0 {
		rem := q.SpaceLimit - q.SpaceUsage
		if rem < 0 {
			return 0
		}
		return rem
	}
	if q.UserLimit > 0 {
		rem := q.UserLimit - q.UserUsage
		if rem < 0 {
			return 0
		}
		return rem
	}
	if q.PremiumBalance > 0 {
		return q.PremiumBalance
	}
	return 0
}

type Account struct {
	mu                        sync.RWMutex
	TokenV2                   string       `json:"token_v2"`
	SessionToken              string       `json:"session_token,omitempty"`
	UserID                    string       `json:"user_id"`
	UserName                  string       `json:"user_name"`
	UserEmail                 string       `json:"user_email"`
	SpaceID                   string       `json:"space_id"`
	SpaceName                 string       `json:"space_name"`
	AccountID                 string       `json:"account_id,omitempty"`
	SpaceViewID               string       `json:"space_view_id,omitempty"`
	PlanType                  string       `json:"plan_type"`
	Timezone                  string       `json:"timezone"`
	ClientVersion             string       `json:"client_version"`
	BrowserID                 string       `json:"browser_id,omitempty"`
	DeviceID                  string       `json:"device_id,omitempty"`
	Cookie                    string       `json:"cookie,omitempty"`
	FullCookie                string       `json:"full_cookie,omitempty"`
	Models                    []ModelEntry `json:"available_models"`
	Provider                  string       `json:"provider,omitempty"`
	RegisteredVia             string       `json:"registered_via,omitempty"`
	Role                      string       `json:"role,omitempty"`
	QuotaExhaustedAt          *time.Time   `json:"-"`
	QuotaInfo                 *QuotaInfo   `json:"-"`
	QuotaCheckedAt            *time.Time   `json:"-"`
	PermanentlyExhausted      bool         `json:"-"`
	perplexityOpusUnavailable bool         `json:"-"`
	LastUsedAt                *time.Time   `json:"-"`
	LastError                 string       `json:"-"`
	LastErrorAt               *time.Time   `json:"-"`
	SpaceCount                int          `json:"-"`
	WorkspaceCheckedAt        *time.Time   `json:"-"`
	removed                   bool         `json:"-"`
}

func CanonicalAccountID(userID, spaceID string) string {
	u := strings.ToLower(strings.TrimSpace(userID))
	s := strings.ToLower(strings.TrimSpace(spaceID))
	if u == "" || s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(u + "\x00" + s))
	return hex.EncodeToString(sum[:])
}

func isCanonicalSHA256AccountID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func (acc *Account) EnsureAccountID() string {
	if acc == nil {
		return ""
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if isCanonicalSHA256AccountID(acc.AccountID) {
		return acc.AccountID
	}
	acc.AccountID = CanonicalAccountID(acc.UserID, acc.SpaceID)
	return acc.AccountID
}

func (acc *Account) accountIDSnapshot() string {
	if acc == nil {
		return ""
	}
	acc.mu.RLock()
	id := acc.AccountID
	acc.mu.RUnlock()
	if isCanonicalSHA256AccountID(id) {
		return id
	}
	return acc.EnsureAccountID()
}

func (acc *Account) ShortSpaceID() string {
	if acc == nil {
		return ""
	}
	id := acc.accountIDSnapshot()
	if len(id) >= 8 {
		return id[:8]
	}
	if len(acc.SpaceID) >= 8 {
		return acc.SpaceID[:8]
	}
	return acc.SpaceID
}

func (acc *Account) tokenV2Snapshot() string {
	if acc == nil {
		return ""
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	if strings.TrimSpace(acc.TokenV2) != "" {
		return acc.TokenV2
	}
	return acc.SessionToken
}

func (acc *Account) fullCookieSnapshot() string {
	if acc == nil {
		return ""
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	if strings.TrimSpace(acc.FullCookie) != "" {
		return acc.FullCookie
	}
	return acc.Cookie
}

func (acc *Account) roleSnapshot() string {
	if acc == nil {
		return ""
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	return acc.Role
}

func (acc *Account) roleTier() int {
	role, _ := normalizeAccountRole(acc.roleSnapshot())
	if role == AccountRoleEducation {
		return 1
	}
	return 0
}

func normalizeAccountRole(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return "", nil
	case AccountRolePrimary:
		return AccountRolePrimary, nil
	case AccountRoleEducation, "edu":
		return AccountRoleEducation, nil
	default:
		return "", fmt.Errorf("invalid account role %q", raw)
	}
}

type ChatMessage struct {
	RetainedThinking  []string   `json:"retained_thinking,omitempty"`
	Role              string     `json:"role"`
	Content           string     `json:"content"`
	ToolCalls         []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID        string     `json:"tool_call_id,omitempty"`
	Name              string     `json:"name,omitempty"`
	StringContent     bool       `json:"-"`
	TextBlocks        int        `json:"-"`
	LastTextBlock     string     `json:"-"`
	AfterToolResult   bool       `json:"-"`
	RawToolContent    string     `json:"-"`
	HasRawToolContent bool       `json:"-"`
	AttachmentCount   int        `json:"-"`
	ToolAttachments   int        `json:"-"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
}

type ToolCall struct {
	Index_   int              `json:"index"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type AgentValueEntry struct {
	Type      string          `json:"type"`
	Content   string          `json:"content"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

type ThinkingBlock struct {
	Content   string
	Signature string
}

type StreamCallback func(delta string, done bool, usage *UsageInfo)
type ThinkingDeltaCallback func(delta string, done bool, signature string)

type CitationCandidate struct {
	URL   string
	Title string
	Text  string
}

type BridgeTrace struct {
	TraceID   string `json:"trace_id,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
	Mode      string `json:"mode,omitempty"`
	SubmitMs  int64  `json:"submit_ms"`
	PollMs    int64  `json:"poll_ms"`
	Polls     int    `json:"polls"`
	Compacted bool   `json:"compacted"`
	Retries   int    `json:"retries"`
	TotalMs   int64  `json:"total_ms"`
}

type FileAttachment struct {
	Data        []byte
	FileName    string
	ContentType string
}

type UploadedAttachment struct {
	agentImage    *FileAttachment
	AgentFileID   string
	AttachmentURL string
	FileName      string
	ContentType   string
	FileSizeBytes int64
	SessionID     string
}

type CallOptions struct {
	NativeToolUses        *[]AgentValueEntry
	ThinkingBlocks        *[]ThinkingBlock
	EnableWebSearch       bool
	EnableWorkspaceSearch *bool
	UseReadOnlyMode       bool
	ReasoningEffort       string
	Attachments           []UploadedAttachment
	IsResearcher          bool
	ThinkingCallback      ThinkingDeltaCallback
	AgentTextCallback     func(string)
	KnownCitationURLs     *[]string
	KnownCitationDocs     *[]CitationCandidate
	KnownToolCallURLs     *map[string][]string
	Session               *Session
	RequestID             string
	HasClientTools        bool
	Cancel                <-chan struct{}
	RetryTraces           *[]string
	BridgeTrace           *BridgeTrace
}

type anthropicContextProjection struct {
	Requested        bool
	PreserveThinking bool
	OriginalTokens   int
	InputTokens      int
}

type anthropicContextResponse struct {
	AppliedEdits []interface{} `json:"applied_edits,omitempty"`
}

type contextCountResponse struct {
	OriginalInputTokens int `json:"original_input_tokens"`
}

func projectAnthropicContext(req *AnthropicRequest) (*AnthropicRequest, *anthropicContextProjection, error) {
	est := estimateAnthropicRequestTokens(req)
	p := &anthropicContextProjection{
		Requested:      req.ContextManagement != nil,
		OriginalTokens: est,
		InputTokens:    est,
	}
	return req, p, nil
}

func stripRetainedThinkingUnlessPreserved(messages []ChatMessage, p *anthropicContextProjection) {
	if p != nil && p.PreserveThinking {
		return
	}
	for i := range messages {
		messages[i].RetainedThinking = nil
	}
}

func sessionContextResponse(session *Session) *anthropicContextResponse {
	if session == nil || session.ContextProjection == nil || !session.ContextProjection.Requested {
		return nil
	}
	return &anthropicContextResponse{AppliedEdits: []interface{}{}}
}

func generateUUIDv4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func StripAskModeSuffix(model string) (string, bool) {
	trimmed := strings.TrimSpace(model)
	lower := strings.ToLower(trimmed)
	if strings.HasSuffix(lower, "-ask") {
		return trimmed[:len(trimmed)-4], true
	}
	return trimmed, false
}

func isAgentThreadEligible(model string) bool {
	return AppConfig == nil || AppConfig.Proxy.AgentThread
}

func ResolveModel(model string) string {
	trimmed, _ := StripAskModeSuffix(strings.TrimSpace(model))
	if trimmed == "" && AppConfig != nil {
		trimmed = AppConfig.Proxy.DefaultModel
	}
	return trimmed
}

func IsResearcherModel(model string) bool {
	return false
}

func replaceCookieValue(cookieHeader, key, newVal string) string {
	parts := strings.Split(cookieHeader, ";")
	replaced := false
	for i, part := range parts {
		trimmed := strings.TrimSpace(part)
		if strings.HasPrefix(trimmed, key+"=") {
			parts[i] = " " + key + "=" + newVal
			replaced = true
		}
	}
	if !replaced {
		if strings.TrimSpace(cookieHeader) == "" {
			return key + "=" + newVal
		}
		return strings.TrimRight(cookieHeader, "; ") + "; " + key + "=" + newVal
	}
	return strings.TrimSpace(strings.Join(parts, ";"))
}

func decompressBody(resp *http.Response) (io.Reader, func(), error) {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	switch enc {
	case "", "identity":
		return resp.Body, nil, nil
	case "gzip":
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, nil, err
		}
		return gr, func() { _ = gr.Close() }, nil
	case "br":
		br := brotli.NewReader(resp.Body)
		return br, nil, nil
	case "deflate":
		fr := flate.NewReader(resp.Body)
		return fr, func() { _ = fr.Close() }, nil
	case "zstd":
		zr, err := zstd.NewReader(resp.Body)
		if err != nil {
			return nil, nil, err
		}
		return zr, func() { zr.Close() }, nil
	default:
		return resp.Body, nil, nil
	}
}

func nonSystemMessages(messages []ChatMessage) []ChatMessage {
	var out []ChatMessage
	for _, m := range messages {
		if m.Role != "system" {
			out = append(out, m)
		}
	}
	return out
}

func incrementalSuffix(prev, next string) string {
	if next == "" {
		return ""
	}
	if prev == "" {
		return next
	}
	if strings.HasPrefix(next, prev) {
		return next[len(prev):]
	}
	maxOverlap := len(prev)
	if len(next) < maxOverlap {
		maxOverlap = len(next)
	}
	for k := maxOverlap; k > 0; k-- {
		if strings.HasSuffix(prev, next[:k]) && utf8.ValidString(next[k:]) {
			return next[k:]
		}
	}
	return ""
}

func extractLastUserQueryText(messages []ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != "user" || m.ToolCallID != "" {
			continue
		}
		if strings.TrimSpace(m.LastTextBlock) != "" {
			return strings.TrimSpace(stripSystemReminders(m.LastTextBlock))
		}
		return strings.TrimSpace(stripSystemReminders(m.Content))
	}
	return ""
}

func isUserModelIdentityQuery(messages []ChatMessage, lastUserText string) bool {
	q := strings.ToLower(strings.TrimSpace(lastUserText))
	if q == "" {
		q = strings.ToLower(strings.TrimSpace(extractLastUserQueryText(messages)))
	}
	if q == "" || utf8.RuneCountInString(q) > 160 {
		return false
	}
	for _, kw := range []string{
		"你是谁", "你是什么模型", "你是哪个模型", "你底层是什么", "真实模型", "实际模型",
		"who are you", "what model are you", "which model are you", "underlying model",
	} {
		if strings.Contains(q, kw) {
			return true
		}
	}
	return false
}

func agentClientDeclaredIdentities(messages []ChatMessage) map[string]bool {
	out := make(map[string]bool)
	for _, m := range messages {
		if m.Role != "system" && m.Role != "user" {
			continue
		}
		for _, tok := range strings.Fields(m.Content) {
			tok = strings.Trim(tok, ".,;:!?\"'()[]{}<>")
			if strings.Contains(tok, "@") && len(tok) > 3 {
				out[strings.ToLower(tok)] = true
			}
		}
	}
	return out
}

func agentPoolTokens(acc *Account, declared map[string]bool) []string {
	if acc == nil {
		return nil
	}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if utf8.RuneCountInString(s) < 2 {
			return
		}
		if declared != nil && declared[strings.ToLower(s)] {
			return
		}
		for _, existing := range out {
			if strings.EqualFold(existing, s) {
				return
			}
		}
		out = append(out, s)
	}
	add(acc.UserEmail)
	if at := strings.Index(acc.UserEmail, "@"); at > 1 {
		add(acc.UserEmail[:at])
	}
	add(acc.UserName)
	for _, part := range strings.Fields(acc.UserName) {
		if utf8.RuneCountInString(part) >= 1 {
			add(part)
		}
	}
	return out
}

func redactPoolTokens(text string, tokens []string) (string, bool) {
	if text == "" || len(tokens) == 0 {
		return text, false
	}
	out := text
	changed := false
	for _, tok := range tokens {
		if tok == "" {
			continue
		}
		if strings.Contains(out, tok) {
			out = strings.ReplaceAll(out, tok, "[upstream-pool-account]")
			changed = true
		}
	}
	return out, changed
}

func isSolAgentModel(model string) bool {
	base, _ := StripAskModeSuffix(model)
	key := strings.ToLower(strings.TrimSpace(base))
	for _, p := range []string{"perplexity/", "perplexity-", "pplx/", "pplx-", "notion/", "notion-"} {
		if strings.HasPrefix(key, p) {
			key = strings.TrimPrefix(key, p)
			break
		}
	}
	key = strings.TrimSuffix(key, "[1m]")
	switch key {
	case "gpt-6.1-sol", "claude-sol-6-1", "claude-sol-6.1", "gpt6_1_sol", "gpt6_1_sol_thinking", "gpt-5.4", "gpt-5.2":
		return true
	default:
		return false
	}
}

func agentMessageDigest(m ChatMessage) string {
	role := strings.TrimSpace(m.Role)
	content := m.Content
	if role == "user" {
		content = stripSystemReminders(content)
	}
	payload := struct {
		Role       string `json:"r"`
		Content    string `json:"c"`
		ToolCallID string `json:"tcid,omitempty"`
		Name       string `json:"tn,omitempty"`
		ToolCalls  int    `json:"tcs,omitempty"`
	}{
		Role:       role,
		Content:    strings.TrimSpace(content),
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
		ToolCalls:  len(m.ToolCalls),
	}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

func isAwayRecapRequest(messages []ChatMessage) bool {
	for _, m := range messages {
		if strings.Contains(m.Content, "away-recap") || strings.Contains(m.Content, "away_recap") {
			return true
		}
	}
	return false
}

func isCompactSummarisationRequest(messages []ChatMessage) bool {
	for _, m := range messages {
		if strings.Contains(m.Content, "Your task is to create a detailed summary of the conversation so far") {
			return true
		}
	}
	return false
}

func isUntrackedHelperMessages(messages []ChatMessage, tooled bool) bool {
	if tooled {
		return false
	}
	if isAwayRecapRequest(messages) || isCompactSummarisationRequest(messages) {
		return true
	}
	for _, m := range messages {
		if m.Role == "system" {
			sysLower := strings.ToLower(m.Content)
			if strings.Contains(sysLower, "you write short session titles") ||
				strings.Contains(sysLower, "title generator") {
				return true
			}
		}
	}
	ns := nonSystemMessages(messages)
	if len(ns) == 0 {
		return false
	}
	last := ns[len(ns)-1]
	if last.Role != "user" {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(last.Content))
	for _, prefix := range []string{
		"generate a short kebab-case name",
		"write a short title",
		"summarize this conversation in 3-5 words",
		"extract any new facts",
	} {
		if strings.Contains(lower, prefix) {
			return true
		}
	}
	return false
}
