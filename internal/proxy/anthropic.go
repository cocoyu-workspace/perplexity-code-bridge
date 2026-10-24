package proxy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	ErrToolBridgeNoTool     = errors.New("tool bridge produced no usable tool action")
	errAgentStreamRewritten = errors.New("upstream stream rewritten before completion")
)

var DefaultModelMap = map[string]string{
	"opus-4.6":       "claude46opus",
	"sonnet-4.6":     "claude46sonnet",
	"haiku-4.5":      "claude46sonnet",
	"gpt-5.4":        "gpt54",
	"gemini-3-flash": "gemini31pro_high",
	"gemini-3.1-pro": "gemini31pro_high",
}

var modelMapMu sync.RWMutex

func GetModelID(name string) (string, bool) {
	modelMapMu.RLock()
	defer modelMapMu.RUnlock()
	id, ok := DefaultModelMap[name]
	return id, ok
}

func SetModelID(name, id string) {
	modelMapMu.Lock()
	defer modelMapMu.Unlock()
	DefaultModelMap[name] = id
}

func ReplaceModelMap(m map[string]string) {
	modelMapMu.Lock()
	defer modelMapMu.Unlock()
	DefaultModelMap = m
}

func SnapshotModelMap() map[string]string {
	modelMapMu.RLock()
	defer modelMapMu.RUnlock()
	copied := make(map[string]string, len(DefaultModelMap))
	for k, v := range DefaultModelMap {
		copied[k] = v
	}
	return copied
}

func ApplyConfig(cfg *Config) {
	if len(cfg.ModelMap) > 0 {
		ReplaceModelMap(cfg.ModelMap)
	}
	SetDebugLoggingEnabled(cfg.Server.DebugLogging)
	SetAPILogInputEnabled(cfg.Server.APILogInput)
	SetAPILogOutputEnabled(cfg.Server.APILogOutput)
	SetNotionRequestLoggingEnabled(cfg.Server.NotionLogReq)
	SetNotionResponseLoggingEnabled(cfg.Server.NotionLogResp)
}

func normalizeReasoningEffort(value string) (string, error) {
	effort := strings.ToLower(strings.TrimSpace(value))
	switch effort {
	case "", "none", "minimal", "low", "medium", "high", "max", "xhigh":
		return effort, nil
	default:
		return "", fmt.Errorf("unsupported reasoning effort %q", value)
	}
}

type anthropicResponseWrittenError struct {
	err error
}

func (e anthropicResponseWrittenError) Error() string { return e.err.Error() }
func (e anthropicResponseWrittenError) Unwrap() error { return e.err }

var citationRe = regexp.MustCompile(`\[\^(?:\{\{)?([^\]\}]+?)(?:\}\})?\]`)

type citationReplacer struct {
	urlToNum     map[string]int
	urls         []string
	buf          strings.Builder
	state        int
	knownURLs    *[]string
	knownDocs    *[]CitationCandidate
	toolCallURLs *map[string][]string
	context      string
}

func newCitationReplacer(knownURLs *[]string, knownDocs *[]CitationCandidate, toolCallURLs *map[string][]string) *citationReplacer {
	return &citationReplacer{
		urlToNum:     make(map[string]int),
		knownURLs:    knownURLs,
		knownDocs:    knownDocs,
		toolCallURLs: toolCallURLs,
	}
}

func (cr *citationReplacer) Process(delta string) string {
	return delta
}

func (cr *citationReplacer) Flush() string {
	return ""
}

func (cr *citationReplacer) URLs() []string { return cr.urls }

func formatCitationSources(urls []string) string {
	if len(urls) == 0 {
		return ""
	}
	var sources strings.Builder
	sources.WriteString("\n---\nSources:\n")
	for i, u := range urls {
		sources.WriteString(fmt.Sprintf("[%d] %s\n", i+1, u))
	}
	return sources.String()
}

func renderAnthropicCitationText(rawText string, knownURLs []string, knownDocs []CitationCandidate, toolCallURLs map[string][]string) string {
	return rawText
}

func cleanCitationsWithContext(text string, toolCallURLs map[string][]string, knownURLs []string, candidates []CitationCandidate) string {
	return text
}

// ========== Anthropic Messages API Types ==========

type AnthropicRequest struct {
	Model             string                 `json:"model"`
	MaxTokens         int                    `json:"max_tokens"`
	System            interface{}            `json:"system,omitempty"`
	Messages          []AnthropicMessage     `json:"messages"`
	Stream            bool                   `json:"stream"`
	Temperature       *float64               `json:"temperature,omitempty"`
	TopP              *float64               `json:"top_p,omitempty"`
	TopK              *int                   `json:"top_k,omitempty"`
	StopSequences     []string               `json:"stop_sequences,omitempty"`
	Tools             []AnthropicTool        `json:"tools,omitempty"`
	ToolChoice        interface{}            `json:"tool_choice,omitempty"`
	Thinking          interface{}            `json:"thinking,omitempty"`
	OutputConfig      *AnthropicOutputConfig `json:"output_config,omitempty"`
	Metadata          map[string]interface{} `json:"metadata,omitempty"`
	ContextManagement interface{}            `json:"context_management,omitempty"`
}

type AnthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type AnthropicContentBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Signature    string          `json:"signature,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      interface{}     `json:"content,omitempty"`
	CacheControl interface{}     `json:"cache_control,omitempty"`
}

type AnthropicTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema interface{} `json:"input_schema,omitempty"`
}

type AnthropicOutputConfig struct {
	Format *AnthropicOutputFormat `json:"format,omitempty"`
	Effort string                 `json:"effort,omitempty"`
}

type AnthropicOutputFormat struct {
	Type   string      `json:"type,omitempty"`
	Schema interface{} `json:"schema,omitempty"`
}

type AnthropicResponse struct {
	ContextManagement *anthropicContextResponse `json:"context_management,omitempty"`

	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Content      []AnthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   *string                 `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        *AnthropicUsage         `json:"usage"`
}

type AnthropicUsage struct {
	InputTokens          int `json:"input_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens,omitempty"`
	OutputTokens         int `json:"output_tokens"`
}

var structuredOutputLeadingTagRegex = regexp.MustCompile(`(?s)^\s*(?:<[A-Za-z][^>\n]*/>\s*)+`)

func isJSONSchemaOutput(outputConfig *AnthropicOutputConfig) bool {
	return outputConfig != nil && outputConfig.Format != nil && outputConfig.Format.Type == "json_schema" && outputConfig.Format.Schema != nil
}

func outputConfigReasoningEffort(outputConfig *AnthropicOutputConfig) string {
	if outputConfig == nil {
		return ""
	}
	return outputConfig.Effort
}

func extractStructuredJSONObject(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.TrimSpace(structuredOutputLeadingTagRegex.ReplaceAllString(trimmed, ""))
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		return trimmed
	}
	for _, match := range mdFenceRegex.FindAllStringSubmatch(trimmed, -1) {
		candidate := strings.TrimSpace(match[1])
		candidate = strings.TrimSpace(structuredOutputLeadingTagRegex.ReplaceAllString(candidate, ""))
		if strings.HasPrefix(candidate, "{") && json.Valid([]byte(candidate)) {
			return candidate
		}
	}
	for i, r := range trimmed {
		if r != '{' {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(trimmed[i:]))
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil || len(raw) == 0 || raw[0] != '{' || !json.Valid(raw) {
			continue
		}
		rest := strings.TrimSpace(trimmed[i+int(decoder.InputOffset()):])
		rest = strings.TrimSpace(structuredOutputLeadingTagRegex.ReplaceAllString(rest, ""))
		if rest == "" {
			return string(raw)
		}
	}
	return ""
}

func normalizeStructuredOutputText(content string) string {
	if extracted := extractStructuredJSONObject(content); extracted != "" {
		return extracted
	}
	trimmed := strings.TrimSpace(content)
	if trimmed != "" {
		log.Printf("[bridge] structured output JSON-only normalization fallback (%d chars)", len(trimmed))
	}
	return trimmed
}

type preparedToolBridgeResponse struct {
	ToolCalls      []ToolCall
	Remaining      string
	Preamble       string
	DoneText       string
	WebSearchQuery string
	HasCalls       bool
}

func prepareToolBridgeResponse(content string, nativeToolUses []AgentValueEntry) preparedToolBridgeResponse {
	prepared := preparedToolBridgeResponse{}

	if len(nativeToolUses) > 0 {
		prepared.ToolCalls = nativeToolUseToOpenAI(nativeToolUses)
		prepared.HasCalls = len(prepared.ToolCalls) > 0
		prepared.Remaining = content
	}
	if !prepared.HasCalls {
		prepared.ToolCalls, prepared.Remaining, prepared.HasCalls = parseToolCalls(content)
	}
	return finishPreparedCalls(prepared.ToolCalls, prepared.Remaining)
}

func finishPreparedCalls(calls []ToolCall, remaining string) preparedToolBridgeResponse {
	return finishPreparedCallsOpt(calls, remaining, !(AppConfig != nil && AppConfig.Proxy.AgentThread))
}

func finishPreparedCallsOpt(calls []ToolCall, remaining string, interceptSearch bool) preparedToolBridgeResponse {
	prepared := preparedToolBridgeResponse{ToolCalls: calls, Remaining: remaining, HasCalls: len(calls) > 0}

	if prepared.HasCalls {
		var realCalls []ToolCall
		for _, tc := range prepared.ToolCalls {
			if tc.Function.Name == "__done__" {
				var args map[string]interface{}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err == nil {
					if r, ok := args["result"].(string); ok {
						prepared.DoneText = r
					}
				}
				if prepared.DoneText == "" {
					prepared.DoneText = tc.Function.Arguments
				}
				log.Printf("[bridge] __done__ intercepted: %s", prepared.DoneText)
			} else {
				realCalls = append(realCalls, tc)
			}
		}
		prepared.ToolCalls = realCalls
		prepared.HasCalls = len(prepared.ToolCalls) > 0
	}

	if prepared.HasCalls && interceptSearch {
		var keptCalls []ToolCall
		for _, tc := range prepared.ToolCalls {
			if tc.Function.Name == "WebSearch" {
				var args map[string]interface{}
				var query string
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err == nil {
					if q, ok := args["query"].(string); ok {
						query = q
					}
				}
				if query == "" {
					query = tc.Function.Arguments
				}
				if prepared.WebSearchQuery != "" {
					prepared.WebSearchQuery = prepared.WebSearchQuery + "\n" + query
				} else {
					prepared.WebSearchQuery = query
				}
			} else {
				keptCalls = append(keptCalls, tc)
			}
		}
		prepared.ToolCalls = keptCalls
		prepared.HasCalls = len(prepared.ToolCalls) > 0
	}

	return prepared
}

func strictToolGuardActive(session *Session) bool {
	return session != nil && session.StrictToolGuard
}

func isFreePlan(acc *Account) bool {
	if acc == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(acc.PlanType), "free")
}

func isNotionUnauthorizedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "401") || strings.Contains(msg, "unauthorized") || strings.Contains(msg, "session_expired")
}

var (
	poweredByModelReminderLine = regexp.MustCompile(`(?m)^[ \t]*You are powered by the model [^\r\n]+$`)
	gitUserReminderLine        = regexp.MustCompile(`(?m)^[ \t]*Git user:[^\r\n]*\r?\n?`)
)

func normalizePoweredByModelReminder(content string) string {
	if strings.Contains(content, "You are powered by the model ") {
		content = poweredByModelReminderLine.ReplaceAllStringFunc(content, func(line string) string {
			if isSolAgentModel(line) {
				return "You are powered by OpenAI's GPT-6.1 Sol model (OpenAI 的 GPT-6.1 Sol 模型)."
			}
			return "You are powered by Anthropic's official Claude model (Anthropic 官方的 Claude 模型)."
		})
	}
	if strings.Contains(content, "Git user:") {
		content = gitUserReminderLine.ReplaceAllString(content, "")
	}
	return content
}

func stripAgentRetryNotice(text string) string {
	return text
}

func prepareToolBridgeResponseFor(content string, nativeToolUses []AgentValueEntry, session *Session) (preparedToolBridgeResponse, error) {
	if (session != nil && session.StrictToolGuard) || (AppConfig != nil && AppConfig.Proxy.AgentThread) {
		var declared declaredTools
		if session != nil {
			declared = session.Tools
		}
		return prepareToolBridgeResponseGuarded(content, nativeToolUses, declared)
	}
	return prepareToolBridgeResponse(content, nativeToolUses), nil
}

func normalizeToolBridgeResidualText(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.TrimSpace(structuredOutputLeadingTagRegex.ReplaceAllString(trimmed, ""))
	return trimmed
}

func detectToolBridgeNoToolResponse(text string) bool {
	normalized := normalizeToolBridgeResidualText(text)
	if normalized == "" {
		return false
	}
	lower := strings.ToLower(normalized)
	mentionsLocalFS := strings.Contains(normalized, "本地文件系统") ||
		strings.Contains(lower, "local file system")
	mentionsCodingAssistant := strings.Contains(normalized, "编码助手") ||
		strings.Contains(normalized, "Claude Code") ||
		strings.Contains(normalized, "Cursor") ||
		strings.Contains(lower, "coding assistant")
	mentionsManualHandOff := strings.Contains(normalized, "复制粘贴") ||
		strings.Contains(normalized, "手动添加") ||
		strings.Contains(normalized, "你可以这样做") ||
		strings.Contains(lower, "copy and paste") ||
		strings.Contains(lower, "manually add")
	mentionsMissingLocalTools := strings.Contains(lower, "read") &&
		strings.Contains(lower, "edit") &&
		strings.Contains(lower, "bash")

	switch {
	case mentionsLocalFS && mentionsCodingAssistant:
		return true
	case mentionsMissingLocalTools && mentionsCodingAssistant && mentionsManualHandOff:
		return true
	default:
		return false
	}
}

func extractAnthropicClientSessionID(metadata map[string]interface{}) string {
	if len(metadata) == 0 {
		return ""
	}
	if raw, ok := metadata["user_id"].(string); ok {
		var parsed map[string]interface{}
		if json.Unmarshal([]byte(strings.TrimSpace(raw)), &parsed) == nil {
			if sid, ok := parsed["session_id"].(string); ok && strings.TrimSpace(sid) != "" {
				return strings.TrimSpace(sid)
			}
		}
	}
	if m, ok := metadata["user_id"].(map[string]interface{}); ok {
		if sid, ok := m["session_id"].(string); ok && strings.TrimSpace(sid) != "" {
			return strings.TrimSpace(sid)
		}
	}
	for _, key := range []string{"session_id", "conversation_id", "thread_id"} {
		if sid, ok := metadata[key].(string); ok && strings.TrimSpace(sid) != "" {
			return strings.TrimSpace(sid)
		}
	}
	if pck, ok := metadata["prompt_cache_key"].(string); ok && isSessionScopedPromptCacheKey(pck) {
		return strings.TrimSpace(pck)
	}
	return ""
}

func isUUIDToken(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func isSessionScopedPromptCacheKey(raw string) bool {
	s := strings.TrimSpace(raw)
	if len(s) < 12 || len(s) > 128 {
		return false
	}
	if isUUIDToken(s) {
		return true
	}
	for _, sep := range []string{":", "/"} {
		if strings.Contains(s, sep) {
			for _, part := range strings.Split(s, sep) {
				if isUUIDToken(strings.TrimSpace(part)) {
					return true
				}
			}
		}
	}
	lower := strings.ToLower(s)
	for _, prefix := range []string{"ses_", "sess_", "sess-", "session_", "session-", "conv_", "conv-", "thread_", "thread-"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	if len(s) >= 32 && len(s) <= 64 {
		allHex := true
		for i := 0; i < len(s); i++ {
			c := s[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				allHex = false
				break
			}
		}
		if allHex {
			return true
		}
	}
	return false
}

func extractAnthropicSessionSalt(metadata map[string]interface{}) string {
	if len(metadata) == 0 {
		return ""
	}
	extractFromValue := func(v interface{}) string {
		switch tv := v.(type) {
		case string:
			trimmed := strings.TrimSpace(tv)
			if trimmed == "" {
				return ""
			}
			var parsed map[string]interface{}
			if json.Unmarshal([]byte(trimmed), &parsed) == nil {
				if sid, ok := parsed["session_id"].(string); ok && strings.TrimSpace(sid) != "" {
					return strings.TrimSpace(sid)
				}
				return ""
			}
			return trimmed
		case map[string]interface{}:
			if sid, ok := tv["session_id"].(string); ok && strings.TrimSpace(sid) != "" {
				return strings.TrimSpace(sid)
			}
		}
		return ""
	}

	for _, key := range []string{"prompt_cache_key", "session_id", "conversation_id", "user_id"} {
		if sid := extractFromValue(metadata[key]); sid != "" {
			return sid
		}
	}
	return ""
}

const anthropicAgentIDHeader = "X-Claude-Code-Agent-Id"
const maxAnthropicAgentIDLen = 128

func anthropicSubagentIDFromHeader(h http.Header) (id string, present bool) {
	values := h.Values(anthropicAgentIDHeader)
	if len(values) == 0 {
		return "", false
	}
	if len(values) != 1 || !validAnthropicAgentID(values[0]) {
		return "", true
	}
	return values[0], true
}

func validAnthropicAgentID(id string) bool {
	if id == "" || len(id) > maxAnthropicAgentIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

func anthropicSessionHeaderValue(h http.Header) (sessionID, parentID string) {
	if h == nil {
		return "", ""
	}
	for _, name := range []string{"x-opencode-session-id", "x-opencode-session", "x-session-id", "session-id", "session_id", "conversation-id", "conversation_id", "thread-id"} {
		if val := strings.TrimSpace(h.Get(name)); val != "" {
			sessionID = val
			break
		}
	}
	if sessionID == "" {
		if val := strings.TrimSpace(h.Get("x-session-affinity")); isSessionScopedPromptCacheKey(val) {
			sessionID = val
		}
	}
	for _, name := range []string{"x-opencode-parent-session-id", "x-parent-session-id", "parent-session-id"} {
		if val := strings.TrimSpace(h.Get(name)); val != "" {
			parentID = val
			break
		}
	}
	return sessionID, parentID
}

func subagentClientKey(parent, child string) string {
	sum := sha256.Sum256([]byte("subagent:" + strconv.Itoa(len(parent)) + ":" + parent + ":" + strconv.Itoa(len(child)) + ":" + child))
	return "subagent:" + hex.EncodeToString(sum[:])
}

func anthropicClientSessionIDForRequest(h http.Header, metadata map[string]interface{}) string {
	parent := extractAnthropicClientSessionID(metadata)
	headerSession, headerParent := anthropicSessionHeaderValue(h)
	if parent == "" {
		parent = headerSession
	}
	child, present := anthropicSubagentIDFromHeader(h)
	if !present {
		if headerParent != "" && parent != "" && headerParent != parent {
			return subagentClientKey(headerParent, parent)
		}
		return parent
	}
	if child == "" || parent == "" {
		return ""
	}
	return subagentClientKey(parent, child)
}

func anthropicSessionSaltForRequest(h http.Header, metadata map[string]interface{}) string {
	salt := extractAnthropicSessionSalt(metadata)
	headerSession, headerParent := anthropicSessionHeaderValue(h)
	if salt == "" {
		salt = headerSession
	}
	child, present := anthropicSubagentIDFromHeader(h)
	if !present {
		if headerParent != "" && headerSession != "" && headerParent != headerSession {
			return salt + "|" + subagentClientKey(headerParent, headerSession)
		}
		return salt
	}
	return salt + "|" + subagentClientKey(salt, child)
}

func applyAgentSubagentDefaults(h http.Header, model string, outCfg *AnthropicOutputConfig) (string, *AnthropicOutputConfig) {
	child, present := anthropicSubagentIDFromHeader(h)
	if !present || child == "" {
		return model, outCfg
	}
	stripped, askSuffix := StripAskModeSuffix(model)
	if stripped == "" || stripped == "auto" {
		model = "claude-sonnet-4-6"
		if askSuffix {
			model += "-ask"
		}
	} else if _, err := perplexityModelPreference(stripped, ""); err != nil {
		if strings.Contains(strings.ToLower(stripped), "opus") {
			model = "claude-opus-4-6"
		} else {
			model = "claude-sonnet-4-6"
		}
		if askSuffix {
			model += "-ask"
		}
	}
	if outCfg == nil {
		outCfg = &AnthropicOutputConfig{Effort: "high"}
	} else {
		copied := *outCfg
		copied.Effort = "high"
		outCfg = &copied
	}
	return model, outCfg
}

func applyAgentAwayRecapDefaults(messages []ChatMessage, model string, outCfg *AnthropicOutputConfig) (string, *AnthropicOutputConfig) {
	if !isAwayRecapRequest(messages) {
		return model, outCfg
	}
	_, askSuffix := StripAskModeSuffix(model)
	model = "claude-sonnet-4-6"
	if askSuffix {
		model += "-ask"
	}
	if outCfg == nil {
		outCfg = &AnthropicOutputConfig{Effort: "high"}
	} else {
		copied := *outCfg
		copied.Effort = "high"
		outCfg = &copied
	}
	return model, outCfg
}

func stripStructuredOutputSystemNoise(content string) string {
	content = normalizeSessionSystemContent(content)
	if content == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			filtered = append(filtered, "")
			continue
		}
		if strings.HasPrefix(trimmed, "You are Claude Code") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.TrimSpace(strings.Join(filtered, "\n"))
}

func applyStructuredOutputBridge(messages []ChatMessage, outputConfig *AnthropicOutputConfig) []ChatMessage {
	if outputConfig == nil || outputConfig.Format == nil || outputConfig.Format.Type != "json_schema" || outputConfig.Format.Schema == nil {
		return messages
	}

	schemaJSON, err := json.MarshalIndent(outputConfig.Format.Schema, "", "  ")
	if err != nil {
		schemaJSON, _ = json.Marshal(outputConfig.Format.Schema)
	}

	var instructionParts []string
	var conversationParts []string
	for _, msg := range messages {
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		switch msg.Role {
		case "system":
			cleaned := stripStructuredOutputSystemNoise(content)
			if cleaned != "" {
				instructionParts = append(instructionParts, cleaned)
			}
		case "user":
			cleaned := stripSystemReminders(content)
			if cleaned == "" {
				cleaned = content
			}
			conversationParts = append(conversationParts, "User:\n"+cleaned)
		case "assistant":
			conversationParts = append(conversationParts, "Assistant:\n"+content)
		case "tool":
			conversationParts = append(conversationParts, "Tool result:\n"+content)
		}
	}

	if len(conversationParts) == 0 {
		return messages
	}

	var prompt strings.Builder
	prompt.WriteString("Return exactly one JSON object that matches this schema.\n")
	prompt.WriteString("Do not output markdown fences, explanations, or extra text.\n\n")
	prompt.WriteString("Schema:\n")
	prompt.Write(schemaJSON)
	if len(instructionParts) > 0 {
		prompt.WriteString("\n\nInstructions:\n")
		prompt.WriteString(strings.Join(instructionParts, "\n\n"))
	}
	prompt.WriteString("\n\nConversation:\n")
	prompt.WriteString(strings.Join(conversationParts, "\n\n"))
	prompt.WriteString("\n\nJSON only.")

	log.Printf("[bridge] structured output bridge applied (json_schema, %d chars)", prompt.Len())
	return []ChatMessage{{
		Role:    "user",
		Content: prompt.String(),
	}}
}

func handlePremiumFeatureUnavailable(pool *AccountPool, acc *Account) {
	if isFreePlan(acc) {
		log.Printf("[premium] %s (free plan) premium feature unavailable — disabling permanently", acc.UserEmail)
		pool.MarkInferenceUnavailable(acc)
		return
	}
	log.Printf("[premium] %s premium feature unavailable, trying next account", acc.UserEmail)
}

func HandleAnthropicMessages(pool *AccountPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := "msg_" + generateUUIDv4()
		setAnthropicRequestIDHeaders(w, requestID)

		if r.Method != http.MethodPost {
			writeAnthropicError(w, requestID, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
			return
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			writeAnthropicError(w, requestID, http.StatusBadRequest, "failed to read request body: "+err.Error(), "invalid_request_error")
			return
		}
		if len(bodyBytes) == 0 {
			writeAnthropicError(w, requestID, http.StatusBadRequest, "request body is required", "invalid_request_error")
			return
		}
		if json.Valid(bodyBytes) {
			LogAPIInputJSONBytes(requestID, "incoming /v1/messages request", bodyBytes)
		} else {
			LogAPIInputText(requestID, "incoming /v1/messages request (raw)", string(bodyBytes))
		}

		var req AnthropicRequest
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			writeAnthropicError(w, requestID, http.StatusBadRequest, "invalid request body: "+err.Error(), "invalid_request_error")
			return
		}

		if len(req.Messages) == 0 {
			writeAnthropicError(w, requestID, http.StatusBadRequest, "messages is required", "invalid_request_error")
			return
		}
		if req.OutputConfig != nil {
			reasoningEffort, err := normalizeReasoningEffort(req.OutputConfig.Effort)
			if err != nil {
				writeAnthropicError(w, requestID, http.StatusBadRequest, err.Error(), "invalid_request_error")
				return
			}
			req.OutputConfig.Effort = reasoningEffort
		}

		effectiveReq, projection, err := projectAnthropicContext(&req)
		if err != nil {
			writeAnthropicError(w, requestID, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		req = *effectiveReq
		if projection != nil && projection.Requested {
			w.Header().Set("X-Bridge-Token-Count-Source", "estimated")
		}

		messages, fileAttachments := convertAnthropicMessages(req.System, req.Messages)
		stripRetainedThinkingUnlessPreserved(messages, projection)

		model := req.Model
		if model == "" && AppConfig != nil {
			model = AppConfig.Proxy.DefaultModel
		}
		if AppConfig != nil && AppConfig.Proxy.AgentThread {
			model, req.OutputConfig = applyAgentSubagentDefaults(r.Header, model, req.OutputConfig)
			model, req.OutputConfig = applyAgentAwayRecapDefaults(messages, model, req.OutputConfig)
		}

		stripped, askFromModel := StripAskModeSuffix(model)
		if askFromModel {
			log.Printf("[ask-mode] %q -> %q (per-request override)", model, stripped)
		}
		model = stripped
		resolvedModel := ResolveModel(model)
		useReadOnlyMode := askFromModel || (AppConfig != nil && AppConfig.AskModeDefault())

		logAnthropicRequest(req, model)

		if AppConfig != nil && AppConfig.Proxy.AgentThread {
			effort := ""
			if req.OutputConfig != nil {
				effort = req.OutputConfig.Effort
			}
			pref, err := perplexityModelPreference(model, effort)
			if err != nil {
				log.Printf("[perplexity-model] requested=%q preference=<rejected: unsupported> effort=%q", model, effort)
			} else {
				log.Printf("[perplexity-model] requested=%q preference=%s effort=%q", model, pref, effort)
			}
		}

		if AppConfig != nil && AppConfig.Proxy.AgentThread {
			for _, file := range fileAttachments {
				switch file.ContentType {
				case "image/png", "image/jpeg", "image/gif", "image/webp":
				default:
					writeAnthropicError(w, requestID, http.StatusBadRequest, "Agent transport currently accepts image attachments only", "invalid_request_error")
					return
				}
			}
		}
		sessionSalt := anthropicSessionSaltForRequest(r.Header, req.Metadata)
		clientSessionID := anthropicClientSessionIDForRequest(r.Header, req.Metadata)
		if sessionSalt != "" {
			log.Printf("[session] extracted metadata session salt %s", truncateForLog(sessionSalt, 8))
		}

		logConvertedMessages(messages)

		effectiveWebSearch := AppConfig != nil && AppConfig.WebSearchEnabled()
		if hdr := r.Header.Get("X-Web-Search"); hdr != "" {
			effectiveWebSearch = strings.EqualFold(hdr, "true") || hdr == "1"
		}
		var enableWorkspaceSearch *bool
		if hdr := r.Header.Get("X-Workspace-Search"); hdr != "" {
			b := strings.EqualFold(hdr, "true") || hdr == "1"
			enableWorkspaceSearch = &b
		}

		hasTools := len(req.Tools) > 0
		enableWebSearch := effectiveWebSearch

		if AppConfig != nil && AppConfig.Proxy.AgentThread && len(fileAttachments) == 0 && isDesktopInferenceProbe(req, messages, hasTools) {
			probeProvider := "perplexity"
			if pool != nil {
				probeProvider = pool.resolveTargetProvider(model)
			}
			if _, err := perplexityModelPreference(model, ""); err != nil {
				writeAnthropicCallError(w, requestID, err)
				return
			}
			probeUsable := pool != nil && pool.NextBestForProviderModel(probeProvider, model) != nil
			if !probeUsable {
				if pool != nil && pool.allQuotaExhaustedForProviderModel(probeProvider, model) {
					writeAgentFailureJSON(w, requestID, quotaExhaustedFailure())
				} else {
					writeAnthropicError(w, requestID, http.StatusServiceUnavailable, "all accounts exhausted after retries", "overloaded_error")
				}
				return
			}
			writeDesktopProbeResponse(w, requestID, model, req.Stream)
			return
		}

		fingerprint := computeSessionFingerprintForRequest(messages, sessionSalt, resolvedModel)
		session := globalSessionManager.requestSnapshot(fingerprint)
		rawMsgCount := countNonSystemMessages(messages)
		isFirstTurn := true
		isRepeatTurn := false

		if session != nil {
			if rawMsgCount < session.RawMessageCount {
				globalSessionManager.Delete(fingerprint)
				session = nil
			} else if rawMsgCount == session.RawMessageCount {
				isRepeatTurn = true
			}
		}
		if session != nil {
			isFirstTurn = false
		}

		var convertedTools []Tool
		if hasTools {
			for _, t := range req.Tools {
				if t.Name == "DeferredToolPlaceholder" {
					continue
				}
				convertedTools = append(convertedTools, Tool{
					Type: "function",
					Function: ToolFunction{
						Name:        t.Name,
						Description: t.Description,
						Parameters:  t.InputSchema,
					},
				})
			}
		} else if req.OutputConfig != nil && req.OutputConfig.Format != nil && req.OutputConfig.Format.Type == "json_schema" {
			messages = applyStructuredOutputBridge(messages, req.OutputConfig)
		}

		originalMessages := cloneChatMessages(messages)
		inputTokenEstimate := estimateAnthropicRequestTokens(&req)
		followUpInputTokenEstimate := estimateAnthropicFollowUpDeltaTokens(&req)
		inputBytesEstimate := estimateAnthropicRequestBytes(&req)

		tried := make(map[*Account]bool)
		maxAttempts := pool.Count()
		routingModel := model
		targetProvider := pool.resolveTargetProvider(routingModel)
		var lastNonQuotaErr error
		var sawEmptyResponse bool
		var sawQuotaExhausted bool
		var toolRecoveryMessages []ChatMessage
		toolBridgeRetried := false
		liveCheckInterval := time.Duration(0)
		if AppConfig != nil {
			liveCheckInterval = AppConfig.QuotaLiveCheckInterval()
		}
		var streamKA *anthropicKeepalive
		if req.Stream {
			w = &keepaliveResponseWriter{ResponseWriter: w, ka: &streamKA}
		}
		defer func() {
			if streamKA != nil {
				streamKA.Stop()
			}
		}()

		for attempt := 0; attempt < maxAttempts; attempt++ {
			var acc *Account
			checkedBoundQuota := false

			if !isFirstTurn && session != nil && len(tried) == 0 {
				var bound *Account
				if session.AccountID != "" {
					if candidate := pool.FindByAccountID(session.AccountID); candidate != nil && candidate.ProviderName() == targetProvider && !pool.isUnusableForModel(candidate, routingModel) {
						bound = candidate
					}
				}
				if bound == nil {
					if candidate := pool.GetByEmail(session.AccountEmail); candidate != nil && candidate.ProviderName() == targetProvider && !pool.isUnusableForModel(candidate, routingModel) {
						bound = candidate
					}
				}
				if bound != nil && pool.RefreshAccountQuotaForModel(bound, liveCheckInterval, routingModel) {
					acc = bound
					checkedBoundQuota = true
				} else {
					if bound != nil && pool.isUnusable(bound) {
						pool.MarkInferenceUnavailable(bound)
					}
					globalSessionManager.Delete(fingerprint)
					session = nil
					isFirstTurn = true
					isRepeatTurn = false
				}
			}

			if acc == nil {
				if len(tried) == 0 {
					if prefID := pool.PreferredAccountID(); prefID != "" {
						if pref := pool.FindByAccountID(prefID); pref != nil && pref.ProviderName() == targetProvider && !pool.isUnusableForModel(pref, routingModel) {
							acc = pref
						}
					}
					if acc == nil {
						acc = pool.NextBestForProviderModel(targetProvider, routingModel)
					}
				} else {
					acc = pool.NextBestExcludingForProviderModel(targetProvider, tried, routingModel)
				}
			}
			if acc == nil {
				break
			}

			if !checkedBoundQuota && !pool.RefreshAccountQuotaForModel(acc, liveCheckInterval, routingModel) {
				sawQuotaExhausted = true
				tried[acc] = true
				if pool.isUnusable(acc) {
					pool.MarkInferenceUnavailable(acc)
				}
				continue
			}
			tried[acc] = true

			attemptMessages := cloneChatMessages(originalMessages)
			if hasTools && !isCompactSummarisationRequest(attemptMessages) && !isAwayRecapRequest(attemptMessages) {
				attemptMessages = injectToolsIntoMessages(attemptMessages, convertedTools, model, session, req.ToolChoice)
			}

			requestMessages := attemptMessages
			if isFirstTurn {
				switch {
				case len(toolRecoveryMessages) > 0:
					requestMessages = cloneChatMessages(toolRecoveryMessages)
				case (AppConfig == nil || !AppConfig.Proxy.AgentThread) && needsFreshThreadRecovery(attemptMessages):
					requestMessages = buildFreshThreadRecoveryMessages(attemptMessages)
				}
			}

			var currentSession *Session
			if isFirstTurn {
				now := time.Now().Format(time.RFC3339Nano)
				acc.EnsureAccountID()
				currentSession = &Session{
					ThreadID:         generateUUIDv4(),
					TurnCount:        0,
					AccountEmail:     acc.UserEmail,
					AccountID:        acc.AccountID,
					CreatedAt:        time.Now(),
					LastUsedAt:       time.Now(),
					ConfigID:         generateUUIDv4(),
					ContextID:        generateUUIDv4(),
					OriginalDatetime: now,
				}
			} else {
				currentSession = session
			}
			currentSession.ClientKey = clientSessionID
			if subID, ok := anthropicSubagentIDFromHeader(r.Header); ok {
				currentSession.SubagentID = subID
			}
			currentSession.InputTokenEstimate = inputTokenEstimate
			currentSession.FollowUpInputTokenEstimate = followUpInputTokenEstimate
			currentSession.InputBytesEstimate = inputBytesEstimate
			currentSession.LastTurnFollowUp = !isFirstTurn && (AppConfig == nil || !AppConfig.Proxy.AgentThread)
			currentSession.ContextProjection = projection
			currentSession.UpstreamModelOverride = ""
			currentSession.FlexFallbackToAuto = false
			if hasTools {
				currentSession.Tools = buildDeclaredTools(req.Tools)
			}

			log.Printf("[req] %s model=%s messages=%d stream=%v tools=%d attachments=%d account=%s session=%v (attempt %d/%d) [anthropic]",
				requestID, model, len(req.Messages), req.Stream, len(req.Tools), len(fileAttachments), acc.UserEmail, !isFirstTurn, attempt+1, maxAttempts)

			var uploadedAttachments []UploadedAttachment
			for _, fa := range fileAttachments {
				faCopy := fa
				uploadedAttachments = append(uploadedAttachments, UploadedAttachment{
					agentImage:    &faCopy,
					FileName:      fa.FileName,
					ContentType:   fa.ContentType,
					FileSizeBytes: int64(len(fa.Data)),
				})
			}

			hasThinking := req.Thinking != nil || req.Stream
			var reqErr error
			if req.Stream {
				reqErr = handleAnthropicStream(w, acc, requestMessages, model, requestID, hasTools, hasThinking, enableWebSearch, enableWorkspaceSearch, useReadOnlyMode, uploadedAttachments, req.OutputConfig, currentSession, r.Context().Done())
			} else {
				reqErr = handleAnthropicNonStream(w, acc, requestMessages, model, requestID, hasTools, hasThinking, enableWebSearch, enableWorkspaceSearch, useReadOnlyMode, uploadedAttachments, req.OutputConfig, currentSession, r.Context().Done())
			}

			var respWritten anthropicResponseWrittenError
			if errors.As(reqErr, &respWritten) {
				if errors.Is(respWritten.err, ErrQuotaExhausted) {
					if isNotionUnauthorizedError(respWritten.err) {
						pool.MarkAccountAuthExpired(acc)
					} else {
						pool.MarkInferenceUnavailable(acc)
						pool.RecordAccountError(acc, AccountErrQuotaExhausted)
					}
				} else {
					pool.RefreshAccountQuotaAsync(acc)
					pool.RecordAccountError(acc, AccountErrRequestFailed)
				}
				return
			}
			if !errors.Is(reqErr, ErrQuotaExhausted) {
				pool.RefreshAccountQuotaAsync(acc)
			}

			if reqErr != nil && errors.Is(reqErr, ErrQuotaExhausted) {
				sawQuotaExhausted = true
				if isNotionUnauthorizedError(reqErr) {
					pool.MarkAccountAuthExpired(acc)
				} else {
					pool.MarkInferenceUnavailable(acc)
					pool.RecordAccountError(acc, AccountErrQuotaExhausted)
				}
				if !isFirstTurn && session != nil {
					globalSessionManager.Delete(fingerprint)
					session = nil
					isFirstTurn = true
					isRepeatTurn = false
				}
				continue
			}

			if reqErr != nil && errors.Is(reqErr, ErrEmptyResponse) {
				pool.RecordAccountError(acc, AccountErrEmptyResponse)
				sawEmptyResponse = true
				if currentSession != nil {
					globalSessionManager.Delete(fingerprint)
					session = nil
					isFirstTurn = true
					isRepeatTurn = false
				}
				continue
			}

			if reqErr != nil && errors.Is(reqErr, ErrToolBridgeNoTool) {
				if !toolBridgeRetried {
					toolBridgeRetried = true
					if fingerprint != "" {
						globalSessionManager.Delete(fingerprint)
					}
					session = nil
					isFirstTurn = true
					isRepeatTurn = false
					toolRecoveryMessages = buildToolBridgeRecoveryMessages(messages)
					tried = make(map[*Account]bool)
					attempt = -1
					continue
				}
				lastNonQuotaErr = reqErr
			}

			if reqErr != nil && errors.Is(reqErr, ErrPremiumFeatureUnavailable) {
				handlePremiumFeatureUnavailable(pool, acc)
				pool.RecordAccountError(acc, AccountErrPremiumGone)
				lastNonQuotaErr = reqErr
				if !isFirstTurn && session != nil {
					globalSessionManager.Delete(fingerprint)
					session = nil
					isFirstTurn = true
					isRepeatTurn = false
				}
				continue
			}

			if reqErr != nil {
				pool.RecordAccountError(acc, AccountErrRequestFailed)
				if !isFirstTurn && session != nil {
					globalSessionManager.Delete(fingerprint)
					session = nil
					isFirstTurn = true
					isRepeatTurn = false
					continue
				}
				lastNonQuotaErr = reqErr
			}

			if reqErr == nil {
				pool.RecordAccountUse(acc)
			}

			if currentSession != nil && reqErr == nil {
				if isFirstTurn {
					currentSession.TurnCount = 1
					currentSession.RawMessageCount = rawMsgCount
					currentSession.UpdatedConfigIDs = []string{generateUUIDv4()}
					currentSession.ModelUsed = resolvedModel
					globalSessionManager.Set(fingerprint, currentSession)
				} else if isRepeatTurn {
					currentSession.LastUsedAt = time.Now()
				} else {
					currentSession.TurnCount++
					currentSession.RawMessageCount = rawMsgCount
					currentSession.UpdatedConfigIDs = append(currentSession.UpdatedConfigIDs, generateUUIDv4())
					currentSession.LastUsedAt = time.Now()
				}
				globalSessionManager.Set(fingerprint, currentSession)
			}

			return
		}

		if targetProvider == "perplexity" && lastNonQuotaErr == nil && !sawQuotaExhausted && !sawEmptyResponse && pool.hasPerplexityModelMismatch(routingModel) {
			lastNonQuotaErr = ErrPremiumFeatureUnavailable
		}
		if lastNonQuotaErr != nil {
			if streamKA != nil && streamKA.Started() {
				streamKA.fail(lastNonQuotaErr)
				return
			}
			if errors.Is(lastNonQuotaErr, ErrPremiumFeatureUnavailable) {
				writeAgentFailureJSON(w, requestID, classifyAgentFailure(lastNonQuotaErr))
				return
			}
			writeAnthropicError(w, requestID, http.StatusBadGateway,
				"upstream API error: "+lastNonQuotaErr.Error(), "api_error")
			return
		}
		if sawEmptyResponse {
			if streamKA != nil && streamKA.Started() {
				streamKA.fail(ErrEmptyResponse)
				return
			}
			writeAnthropicError(w, requestID, http.StatusBadGateway,
				"upstream returned empty response after retries", "api_error")
			return
		}
		if sawQuotaExhausted || pool.allQuotaExhaustedForProviderModel(targetProvider, routingModel) {
			if streamKA != nil && streamKA.Started() {
				streamKA.fail(ErrQuotaExhausted)
				return
			}
			writeAgentFailureJSON(w, requestID, quotaExhaustedFailure())
			return
		}
		if streamKA != nil && streamKA.Started() {
			streamKA.fail(fmt.Errorf("all accounts exhausted after retries"))
			return
		}
		writeAnthropicError(w, requestID, http.StatusServiceUnavailable,
			"all accounts exhausted after retries", "overloaded_error")
	}
}

func convertAnthropicMessages(system interface{}, msgs []AnthropicMessage) ([]ChatMessage, []FileAttachment) {
	var attachments []FileAttachment
	var result []ChatMessage

	if system != nil {
		switch s := system.(type) {
		case string:
			if s != "" {
				result = append(result, ChatMessage{Role: "system", Content: normalizePoweredByModelReminder(s)})
			}
		case []interface{}:
			var parts []string
			for _, block := range s {
				if m, ok := block.(map[string]interface{}); ok {
					if text, ok := m["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
			if len(parts) > 0 {
				result = append(result, ChatMessage{Role: "system", Content: normalizePoweredByModelReminder(strings.Join(parts, "\n"))})
			}
		}
	}

	toolIDToName := map[string]string{}
	for _, msg := range msgs {
		if blocks, ok := msg.Content.([]interface{}); ok {
			for _, block := range blocks {
				if m, ok := block.(map[string]interface{}); ok {
					if t, _ := m["type"].(string); t == "tool_use" {
						id, _ := m["id"].(string)
						name, _ := m["name"].(string)
						if id != "" && name != "" {
							toolIDToName[id] = name
						}
					}
				}
			}
		}
	}

	for _, msg := range msgs {
		cm := ChatMessage{Role: msg.Role}
		hadToolResult := false

		switch content := msg.Content.(type) {
		case string:
			if msg.Role == "assistant" {
				content = stripAgentRetryNotice(content)
			}
			cm.Content = content
			cm.StringContent = true
		case []interface{}:
			var textParts []string
			for _, block := range content {
				if m, ok := block.(map[string]interface{}); ok {
					blockType, _ := m["type"].(string)
					switch blockType {
					case "text":
						if text, ok := m["text"].(string); ok {
							if msg.Role == "assistant" {
								text = stripAgentRetryNotice(text)
								if text == "" {
									continue
								}
							}
							textParts = append(textParts, text)
						}
					case "thinking":
						if thinking, ok := m["thinking"].(string); ok {
							cm.RetainedThinking = append(cm.RetainedThinking, thinking)
						}
					case "redacted_thinking":
					case "tool_use":
						name, _ := m["name"].(string)
						id, _ := m["id"].(string)
						inputRaw, _ := json.Marshal(m["input"])
						cm.ToolCalls = append(cm.ToolCalls, ToolCall{
							ID:   id,
							Type: "function",
							Function: ToolCallFunction{
								Name:      name,
								Arguments: string(inputRaw),
							},
						})
					case "image":
						if source, ok := m["source"].(map[string]interface{}); ok {
							fa := extractFileFromSource(source, "image")
							if fa != nil {
								attachments = append(attachments, *fa)
								cm.AttachmentCount++
							}
						}
					case "document":
						if source, ok := m["source"].(map[string]interface{}); ok {
							fa := extractFileFromSource(source, "document")
							if fa != nil {
								attachments = append(attachments, *fa)
								cm.AttachmentCount++
							}
						}
					case "tool_result":
						toolUseID, _ := m["tool_use_id"].(string)
						var resultText string
						var attachedPlaceholder string
						toolAttachCount := 0
						if c, ok := m["content"].(string); ok {
							resultText = c
						} else if c, ok := m["content"].([]interface{}); ok {
							var refNames []string
							for _, cb := range c {
								if cbm, ok := cb.(map[string]interface{}); ok {
									if t, ok := cbm["text"].(string); ok {
										resultText += t
									} else if blockType, _ := cbm["type"].(string); blockType == "tool_reference" {
										if tn, ok := cbm["tool_name"].(string); ok && strings.TrimSpace(tn) != "" {
											refNames = append(refNames, strings.TrimSpace(tn))
										}
									} else if blockType == "image" || blockType == "document" {
										if source, ok := cbm["source"].(map[string]interface{}); ok {
											if fa := extractFileFromSource(source, blockType); fa != nil {
												if AppConfig == nil || !AppConfig.Proxy.AgentThread || isAgentSupportedToolAttachmentMime(fa.ContentType) {
													attachments = append(attachments, *fa)
													toolAttachCount++
												}
												if attachedPlaceholder == "" {
													attachedPlaceholder = fmt.Sprintf("[%s attached]", blockType)
												}
											}
										}
									}
								}
							}
							if resultText == "" && attachedPlaceholder != "" {
								resultText = attachedPlaceholder
							}
							if len(refNames) > 0 {
								refSummary := "Loaded tool schema(s): " + strings.Join(refNames, ", ")
								if resultText != "" {
									resultText += "\n" + refSummary
								} else {
									resultText = refSummary
								}
							}
						}
						rawToolContent := resultText
						if resultText == attachedPlaceholder && attachedPlaceholder != "" {
							rawToolContent = ""
						}
						if isErr, _ := m["is_error"].(bool); isErr {
							trimmed := strings.TrimSpace(resultText)
							if trimmed == "" {
								resultText = "Error: tool execution failed"
							} else if !strings.HasPrefix(trimmed, "Error:") && !strings.HasPrefix(trimmed, "<tool_use_error>") {
								resultText = "Error: " + resultText
							}
						}
						resultText = normalizePoweredByModelReminder(resultText)
						result = append(result, ChatMessage{
							Role:              "tool",
							Content:           resultText,
							RawToolContent:    rawToolContent,
							HasRawToolContent: true,
							AttachmentCount:   toolAttachCount,
							ToolAttachments:   toolAttachCount,
							ToolCallID:        toolUseID,
							Name:              toolIDToName[toolUseID],
						})
						hadToolResult = true
						continue
					}
				}
			}
			if len(textParts) > 0 {
				cm.Content = strings.Join(textParts, "")
				cm.TextBlocks, cm.LastTextBlock = len(textParts), textParts[len(textParts)-1]
				cm.AfterToolResult = hadToolResult
			}
		}

		if cm.Role == "system" || cm.Role == "user" {
			cm.Content = normalizePoweredByModelReminder(cm.Content)
			if cm.LastTextBlock != "" {
				cm.LastTextBlock = normalizePoweredByModelReminder(cm.LastTextBlock)
			}
		}
		if hadToolResult && len(cm.ToolCalls) == 0 && normalizeSessionUserContent(cm.Content) == "" {
			if cm.AttachmentCount > 0 && len(result) > 0 {
				result[len(result)-1].AttachmentCount += cm.AttachmentCount
			}
			continue
		}
		if cm.Content != "" || cm.Role == "assistant" || len(cm.ToolCalls) > 0 {
			result = append(result, cm)
		} else if cm.AttachmentCount > 0 && len(result) > 0 {
			result[len(result)-1].AttachmentCount += cm.AttachmentCount
		}
	}

	return result, attachments
}

func isAgentSupportedToolAttachmentMime(contentType string) bool {
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func mimeToExt(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "application/pdf":
		return ".pdf"
	default:
		return ".bin"
	}
}

func extractFileFromSource(source map[string]interface{}, blockKind string) *FileAttachment {
	srcType, _ := source["type"].(string)

	switch srcType {
	case "base64":
		mediaType, _ := source["media_type"].(string)
		data64, _ := source["data"].(string)
		if data64 == "" {
			return nil
		}
		decoded, err := base64.StdEncoding.DecodeString(data64)
		if err != nil {
			return nil
		}
		if mediaType == "" {
			if blockKind == "image" {
				mediaType = "image/png"
			} else {
				mediaType = "application/pdf"
			}
		}
		ext := mimeToExt(mediaType)
		return &FileAttachment{
			Data:        decoded,
			FileName:    generateUUIDv4() + ext,
			ContentType: mediaType,
		}

	case "url":
		urlStr, _ := source["url"].(string)
		if urlStr == "" {
			return nil
		}
		resp, err := http.Get(urlStr)
		if err != nil {
			return nil
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil
		}
		mediaType := resp.Header.Get("Content-Type")
		if mediaType == "" {
			if blockKind == "image" {
				mediaType = "image/png"
			} else {
				mediaType = "application/pdf"
			}
		}
		if idx := strings.Index(mediaType, ";"); idx > 0 {
			mediaType = strings.TrimSpace(mediaType[:idx])
		}
		ext := mimeToExt(mediaType)
		return &FileAttachment{
			Data:        data,
			FileName:    generateUUIDv4() + ext,
			ContentType: mediaType,
		}

	default:
		return nil
	}
}

func streamAnthropicTextResponse(w http.ResponseWriter, acc *Account, messages []ChatMessage, model, requestID string, hasThinking bool, disableBuiltin bool, outputConfig *AnthropicOutputConfig, callOpts CallOptions) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, requestID, http.StatusInternalServerError, "streaming not supported", "api_error")
		return nil
	}

	var fullContent strings.Builder
	var thinkingForLog strings.Builder
	var finalUsage *UsageInfo
	var knownCitationURLs []string
	var knownCitationDocs []CitationCandidate
	knownToolCallURLs := make(map[string][]string)
	cr := newCitationReplacer(&knownCitationURLs, &knownCitationDocs, &knownToolCallURLs)
	headersSent := false
	thinkingBlockOpen := false
	textBlockOpen := false
	blockIndex := 0
	sawContent := false
	callOpts.KnownCitationURLs = &knownCitationURLs
	callOpts.KnownCitationDocs = &knownCitationDocs
	callOpts.KnownToolCallURLs = &knownToolCallURLs
	jsonOnlyOutput := isJSONSchemaOutput(outputConfig)

	ka := newAnthropicKeepalive(w, requestID, model)
	if callOpts.BridgeTrace == nil {
		var bridgeTrace BridgeTrace
		callOpts.BridgeTrace = &bridgeTrace
	}
	ka.SetBridgeTrace(callOpts.BridgeTrace)
	inEst, cacheReadEst := requestInputUsageEstimate(callOpts.Session, messages, false)
	ka.SetInputUsage(inEst, cacheReadEst)
	if callOpts.Session != nil {
		callOpts.Session.OnTurnPlanned = func(followUp bool) {
			inTokens, cacheReadTokens := requestInputUsageEstimate(callOpts.Session, messages, followUp)
			ka.SetInputUsage(inTokens, cacheReadTokens)
		}
		defer func() { callOpts.Session.OnTurnPlanned = nil }()
	}
	stopWatchingClient := ka.WatchClient(callOpts.Cancel)
	defer stopWatchingClient()
	defer ka.Stop()
	ensureHeaders := func() {
		if headersSent {
			return
		}
		ka.Stop()
		headersSent = true
		inputTokens, cacheReadTokens, _ := resolveAnthropicUsageWithCache(finalUsage, callOpts.Session, messages, "", "")
		ka.beginUsage(inputTokens, cacheReadTokens)
	}

	closeThinkingBlock := func(signature string) {
		if !thinkingBlockOpen {
			return
		}
		if signature == "" {
			signature = generateFakeSignature()
		}
		sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]interface{}{"type": "signature_delta", "signature": signature},
		})
		sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
			"type": "content_block_stop", "index": blockIndex,
		})
		blockIndex++
		thinkingBlockOpen = false
	}

	ensureTextBlock := func() {
		ensureHeaders()
		if !textBlockOpen {
			sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         blockIndex,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			textBlockOpen = true
		}
	}

	if hasThinking {
		callOpts.ThinkingCallback = func(delta string, done bool, signature string) {
			if done {
				closeThinkingBlock(signature)
				return
			}
			if delta == "" {
				return
			}
			if textBlockOpen {
				sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": blockIndex})
				blockIndex++
				textBlockOpen = false
			}
			thinkingForLog.WriteString(delta)
			sawContent = true
			ensureHeaders()
			if !thinkingBlockOpen {
				sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":          "content_block_start",
					"index":         blockIndex,
					"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
				})
				thinkingBlockOpen = true
			}
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": delta},
			})
		}
	}

	var retryTraces []string
	if callOpts.RetryTraces == nil {
		callOpts.RetryTraces = &retryTraces
	}
	emitText := func(delta string) {
		if !jsonOnlyOutput {
			cleaned := cr.Process(delta)
			if cleaned != "" {
				sawContent = true
				ensureTextBlock()
				sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": blockIndex,
					"delta": map[string]interface{}{"type": "text_delta", "text": cleaned},
				})
			}
		}
	}
	earlyText := ""
	if AppConfig != nil && AppConfig.Proxy.AgentThread && AppConfig.Proxy.AgentLiveTranscript && !jsonOnlyOutput {
		callOpts.AgentTextCallback = func(delta string) { earlyText += delta; emitText(delta) }
	}
	callOpts.Cancel = ka.Gone()
	upstreamModel := model
	if callOpts.Session != nil && callOpts.Session.UpstreamModelOverride != "" {
		upstreamModel = callOpts.Session.UpstreamModelOverride
	}
	cbErr := CallInference(acc, messages, upstreamModel, disableBuiltin, func(delta string, done bool, usage *UsageInfo) {
		if delta != "" {
			fullContent.WriteString(delta)
			if earlyText != "" && strings.HasPrefix(delta, earlyText) {
				delta = delta[len(earlyText):]
				earlyText = ""
			}
			emitText(delta)
		}
		if usage != nil {
			finalUsage = usage
		}
	}, callOpts)

	ka.Stop()
	if ka.Started() {
		headersSent = true
	}
	if cbErr != nil {
		if !headersSent {
			if errors.Is(cbErr, ErrQuotaExhausted) || errors.Is(cbErr, ErrPremiumFeatureUnavailable) {
				return cbErr
			}
			ka.fail(cbErr)
			return anthropicResponseWrittenError{err: cbErr}
		}
		if !sawContent {
			if (errors.Is(cbErr, ErrQuotaExhausted) || errors.Is(cbErr, ErrPremiumFeatureUnavailable)) && canRetryKeepalive(w) {
				return cbErr
			}
			ka.fail(cbErr)
			return anthropicResponseWrittenError{err: cbErr}
		}
		if AppConfig != nil && AppConfig.Proxy.AgentThread && AppConfig.Proxy.AgentLiveTranscript {
			ka.fail(cbErr)
			return anthropicResponseWrittenError{err: cbErr}
		}
	}

	if fullContent.Len() == 0 && !sawContent {
		if ka.Started() {
			ka.fail(ErrEmptyResponse)
			return anthropicResponseWrittenError{err: ErrEmptyResponse}
		}
		return ErrEmptyResponse
	}

	if thinkingBlockOpen {
		closeThinkingBlock("")
	}

	if !jsonOnlyOutput {
		if flushed := cr.Flush(); flushed != "" {
			sawContent = true
			ensureTextBlock()
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": flushed},
			})
		}
	} else {
		normalized := normalizeStructuredOutputText(fullContent.String())
		if normalized != "" {
			sawContent = true
			ensureTextBlock()
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": normalized},
			})
		}
	}

	if textBlockOpen {
		sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
			"type": "content_block_stop", "index": blockIndex,
		})
		blockIndex++
	}

	urls := cr.URLs()
	if !jsonOnlyOutput {
		if sourcesText := formatCitationSources(urls); sourcesText != "" {
			ensureHeaders()
			sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         blockIndex,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": sourcesText},
			})
			sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type": "content_block_stop", "index": blockIndex,
			})
			blockIndex++
		}
	}

	inputTokens, cacheReadTokens, outputTokens := resolveAnthropicUsageWithCache(finalUsage, callOpts.Session, messages, fullContent.String()+thinkingForLog.String(), requestID)
	ensureHeaders()
	sendAnthropicSSE(w, flusher, "message_delta", map[string]interface{}{
		"type":               "message_delta",
		"delta":              map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage":              anthropicMessageDeltaUsage(inputTokens, cacheReadTokens, outputTokens),
		"context_management": sessionContextResponse(callOpts.Session),
	})
	sendAnthropicMessageStop(w, flusher, callOpts.BridgeTrace)
	return nil
}

func handleAnthropicStream(w http.ResponseWriter, acc *Account, messages []ChatMessage, model, requestID string, hasTools bool, hasThinking bool, enableWebSearch bool, enableWorkspaceSearch *bool, useReadOnlyMode bool, attachments []UploadedAttachment, outputConfig *AnthropicOutputConfig, session *Session, clientGone ...<-chan struct{}) error {
	var fullContent strings.Builder
	var finalUsage *UsageInfo
	var nativeToolUses []AgentValueEntry
	var thinkingBlocks []ThinkingBlock
	var retryTraces []string
	var bridgeTrace BridgeTrace
	var knownCitationURLs []string
	var knownCitationDocs []CitationCandidate
	knownToolCallURLs := make(map[string][]string)

	disableBuiltin := AppConfig != nil && AppConfig.Proxy.DisableNotionPrompt

	callOpts := CallOptions{
		ThinkingBlocks:        &thinkingBlocks,
		RetryTraces:           &retryTraces,
		BridgeTrace:           &bridgeTrace,
		EnableWebSearch:       enableWebSearch,
		EnableWorkspaceSearch: enableWorkspaceSearch,
		UseReadOnlyMode:       useReadOnlyMode,
		ReasoningEffort:       outputConfigReasoningEffort(outputConfig),
		Attachments:           attachments,
		KnownCitationURLs:     &knownCitationURLs,
		KnownCitationDocs:     &knownCitationDocs,
		KnownToolCallURLs:     &knownToolCallURLs,
		Session:               session,
		RequestID:             requestID,
		HasClientTools:        hasTools,
		Cancel:                agentRequestGone(clientGone),
	}

	if !hasTools || isCompactSummarisationRequest(messages) || isAwayRecapRequest(messages) {
		return streamAnthropicTextResponse(w, acc, messages, model, requestID, hasThinking, disableBuiltin, outputConfig, callOpts)
	}

	callOpts.NativeToolUses = &nativeToolUses
	ka := newAnthropicKeepalive(w, requestID, model)
	ka.SetBridgeTrace(&bridgeTrace)
	inEst, cacheReadEst := requestInputUsageEstimate(session, messages, false)
	ka.SetInputUsage(inEst, cacheReadEst)
	if session != nil {
		session.OnTurnPlanned = func(followUp bool) {
			inTokens, cacheReadTokens := requestInputUsageEstimate(session, messages, followUp)
			ka.SetInputUsage(inTokens, cacheReadTokens)
		}
		defer func() { session.OnTurnPlanned = nil }()
	}
	stopWatchingClient := ka.WatchClient(callOpts.Cancel)
	defer stopWatchingClient()
	defer ka.Stop()
	callOpts.Cancel = ka.Gone()

	earlyText := ""
	earlyBlockOpen := false
	earlyFlusher, canStream := w.(http.Flusher)
	if canStream && AppConfig != nil && AppConfig.Proxy.AgentThread && AppConfig.Proxy.AgentLiveTranscript && !isJSONSchemaOutput(outputConfig) {
		callOpts.AgentTextCallback = func(delta string) {
			ka.Stop()
			followUp := session != nil && session.LastTurnFollowUp
			inTokens, cacheReadTokens := requestInputUsageEstimate(session, messages, followUp)
			ka.beginUsage(inTokens, cacheReadTokens)
			if !earlyBlockOpen {
				sendAnthropicSSE(w, earlyFlusher, "content_block_start", map[string]interface{}{"type": "content_block_start", "index": 0, "content_block": map[string]interface{}{"type": "text", "text": ""}})
				earlyBlockOpen = true
			}
			earlyText += delta
			sendAnthropicSSE(w, earlyFlusher, "content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": 0, "delta": map[string]interface{}{"type": "text_delta", "text": delta}})
		}
	}

	upstreamModel := model
	if session != nil && session.UpstreamModelOverride != "" {
		upstreamModel = session.UpstreamModelOverride
	}
	cbErr := CallInference(acc, messages, upstreamModel, disableBuiltin, func(delta string, done bool, usage *UsageInfo) {
		if delta != "" {
			fullContent.WriteString(delta)
		}
		if usage != nil {
			finalUsage = usage
		}
	}, callOpts)

	ka.Stop()
	if earlyBlockOpen {
		sendAnthropicSSE(w, earlyFlusher, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})
	}
	if cbErr != nil {
		if (errors.Is(cbErr, ErrQuotaExhausted) || errors.Is(cbErr, ErrPremiumFeatureUnavailable)) && earlyText == "" && (!ka.Started() || canRetryKeepalive(w)) {
			return cbErr
		}
		ka.fail(cbErr)
		return anthropicResponseWrittenError{err: cbErr}
	}

	contentStr := fullContent.String()
	if len(contentStr) == 0 && len(nativeToolUses) == 0 {
		if ka.Started() {
			ka.fail(ErrEmptyResponse)
			return anthropicResponseWrittenError{err: ErrEmptyResponse}
		}
		return ErrEmptyResponse
	}

	var prepared preparedToolBridgeResponse
	if hasTools {
		var guardErr error
		prepared, guardErr = prepareToolBridgeResponseFor(contentStr, nativeToolUses, session)
		if guardErr != nil {
			ka.fail(guardErr)
			return anthropicResponseWrittenError{err: guardErr}
		}
		actionDetected := prepared.HasCalls || prepared.WebSearchQuery != "" || prepared.DoneText != ""
		if !actionDetected && !(AppConfig != nil && AppConfig.Proxy.AgentThread) && !strictToolGuardActive(session) && detectToolBridgeNoToolResponse(prepared.Remaining) {
			if ka.Started() {
				ka.fail(ErrToolBridgeNoTool)
				return anthropicResponseWrittenError{err: ErrToolBridgeNoTool}
			}
			return ErrToolBridgeNoTool
		}
		if earlyText != "" {
			if prepared.HasCalls || prepared.WebSearchQuery != "" || prepared.DoneText != "" {
				if !strings.HasPrefix(prepared.Preamble, earlyText) {
					ka.fail(errAgentStreamRewritten)
					return anthropicResponseWrittenError{err: errAgentStreamRewritten}
				}
				prepared.Preamble = prepared.Preamble[len(earlyText):]
			} else {
				if !strings.HasPrefix(prepared.Remaining, earlyText) {
					ka.fail(errAgentStreamRewritten)
					return anthropicResponseWrittenError{err: errAgentStreamRewritten}
				}
				prepared.Remaining = prepared.Remaining[len(earlyText):]
			}
		}
	}

	outputForUsage := contentStr + thinkingBlocksText(thinkingBlocks)
	aUsage := &AnthropicUsage{}
	aUsage.InputTokens, aUsage.CacheReadInputTokens, aUsage.OutputTokens = resolveAnthropicUsageWithCache(finalUsage, session, messages, outputForUsage, requestID)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, requestID, http.StatusInternalServerError, "streaming not supported", "api_error")
		return nil
	}

	ka.beginUsage(aUsage.InputTokens, aUsage.CacheReadInputTokens)

	blockIndex := 0
	if earlyText != "" {
		blockIndex = 1
	}
	if hasThinking && len(thinkingBlocks) > 0 {
		for _, tb := range thinkingBlocks {
			sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         blockIndex,
				"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
			})
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": tb.Content},
			})
			sig := tb.Signature
			if sig == "" {
				sig = generateFakeSignature()
			}
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "signature_delta", "signature": sig},
			})
			sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type": "content_block_stop", "index": blockIndex,
			})
			blockIndex++
		}
	}

	if hasTools {
		toolCalls := prepared.ToolCalls
		remaining := prepared.Remaining
		hasCalls := prepared.HasCalls
		doneText := prepared.DoneText

		if prepared.Preamble != "" {
			sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         blockIndex,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": prepared.Preamble},
			})
			sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type": "content_block_stop", "index": blockIndex,
			})
			blockIndex++
		}

		if remaining != "" && (hasCalls || prepared.WebSearchQuery != "" || doneText != "") {
			remaining = ""
		} else if remaining != "" {
			sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         blockIndex,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": remaining},
			})
			sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type": "content_block_stop", "index": blockIndex,
			})
			blockIndex++
		}

		if doneText != "" {
			sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         blockIndex,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": doneText},
			})
			sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type": "content_block_stop", "index": blockIndex,
			})
			blockIndex++
		}

		if finalUsage != nil {
			aUsage.InputTokens = finalUsage.PromptTokens
			aUsage.CacheReadInputTokens = 0
			aUsage.OutputTokens = finalUsage.CompletionTokens
		}

		if hasCalls {
			for _, tc := range toolCalls {
				sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": blockIndex,
					"content_block": map[string]interface{}{
						"type":  "tool_use",
						"id":    tc.ID,
						"name":  tc.Function.Name,
						"input": map[string]interface{}{},
					},
				})
				sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": blockIndex,
					"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
				})
				sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
					"type": "content_block_stop", "index": blockIndex,
				})
				blockIndex++
			}
		}

		stopReason := "end_turn"
		if hasCalls {
			stopReason = "tool_use"
		}
		sendAnthropicSSE(w, flusher, "message_delta", map[string]interface{}{
			"type":               "message_delta",
			"delta":              map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil},
			"usage":              anthropicMessageDeltaUsage(aUsage.InputTokens, aUsage.CacheReadInputTokens, aUsage.OutputTokens),
			"context_management": sessionContextResponse(session),
		})
	}

	sendAnthropicMessageStop(w, flusher, &bridgeTrace)
	return nil
}

func handleAnthropicNonStream(w http.ResponseWriter, acc *Account, messages []ChatMessage, model, requestID string, hasTools bool, hasThinking bool, enableWebSearch bool, enableWorkspaceSearch *bool, useReadOnlyMode bool, attachments []UploadedAttachment, outputConfig *AnthropicOutputConfig, session *Session, clientGone ...<-chan struct{}) error {
	var fullContent strings.Builder
	var finalUsage *UsageInfo
	var nativeToolUses []AgentValueEntry
	var thinkingBlocks []ThinkingBlock
	var retryTraces []string
	var bridgeTrace BridgeTrace
	var thinkingProcess strings.Builder
	var thinkingSignature string
	var knownCitationURLs []string
	var knownCitationDocs []CitationCandidate
	knownToolCallURLs := make(map[string][]string)

	callOpts := CallOptions{
		ThinkingBlocks:        &thinkingBlocks,
		RetryTraces:           &retryTraces,
		BridgeTrace:           &bridgeTrace,
		EnableWebSearch:       enableWebSearch,
		EnableWorkspaceSearch: enableWorkspaceSearch,
		UseReadOnlyMode:       useReadOnlyMode,
		ReasoningEffort:       outputConfigReasoningEffort(outputConfig),
		Attachments:           attachments,
		KnownCitationURLs:     &knownCitationURLs,
		KnownCitationDocs:     &knownCitationDocs,
		KnownToolCallURLs:     &knownToolCallURLs,
		Session:               session,
		RequestID:             requestID,
		HasClientTools:        hasTools,
		Cancel:                agentRequestGone(clientGone),
	}
	if hasThinking {
		callOpts.ThinkingCallback = func(delta string, done bool, signature string) {
			if delta != "" {
				thinkingProcess.WriteString(delta)
			}
			if done && signature != "" {
				thinkingSignature = signature
			}
		}
	}

	upstreamModel := model
	if session != nil && session.UpstreamModelOverride != "" {
		upstreamModel = session.UpstreamModelOverride
	}
	parseTools := hasTools && !isCompactSummarisationRequest(messages) && !isAwayRecapRequest(messages)
	disableBuiltin := AppConfig != nil && AppConfig.Proxy.DisableNotionPrompt
	if parseTools {
		callOpts.NativeToolUses = &nativeToolUses
	}
	err := CallInference(acc, messages, upstreamModel, disableBuiltin, func(delta string, done bool, usage *UsageInfo) {
		if delta != "" {
			fullContent.WriteString(delta)
		}
		if usage != nil {
			finalUsage = usage
		}
	}, callOpts)

	if err != nil {
		if errors.Is(err, ErrQuotaExhausted) || errors.Is(err, ErrPremiumFeatureUnavailable) {
			return err
		}
		setAnthropicBridgeTraceHeader(w, &bridgeTrace)
		writeAnthropicCallError(w, requestID, err)
		return anthropicResponseWrittenError{err: err}
	}

	content := fullContent.String()
	if len(content) == 0 && len(nativeToolUses) == 0 {
		return ErrEmptyResponse
	}

	var prepared preparedToolBridgeResponse
	if parseTools {
		var guardErr error
		prepared, guardErr = prepareToolBridgeResponseFor(content, nativeToolUses, session)
		if guardErr != nil {
			setAnthropicBridgeTraceHeader(w, &bridgeTrace)
			writeAnthropicCallError(w, requestID, guardErr)
			return anthropicResponseWrittenError{err: guardErr}
		}
		actionDetected := prepared.HasCalls || prepared.WebSearchQuery != "" || prepared.DoneText != ""
		if !actionDetected && !(AppConfig != nil && AppConfig.Proxy.AgentThread) && !strictToolGuardActive(session) && detectToolBridgeNoToolResponse(prepared.Remaining) {
			return ErrToolBridgeNoTool
		}
	}

	outputForUsage := content + thinkingBlocksText(thinkingBlocks) + thinkingProcess.String()
	aUsage := &AnthropicUsage{}
	aUsage.InputTokens, aUsage.CacheReadInputTokens, aUsage.OutputTokens = resolveAnthropicUsageWithCache(finalUsage, session, messages, outputForUsage, requestID)

	var contentBlocks []AnthropicContentBlock
	stopReason := "end_turn"

	if hasThinking && thinkingProcess.Len() > 0 {
		sig := thinkingSignature
		if sig == "" {
			sig = generateFakeSignature()
		}
		contentBlocks = append(contentBlocks, AnthropicContentBlock{
			Type:      "thinking",
			Thinking:  thinkingProcess.String(),
			Signature: sig,
		})
	} else if hasThinking && len(thinkingBlocks) > 0 {
		for _, tb := range thinkingBlocks {
			sig := tb.Signature
			if sig == "" {
				sig = generateFakeSignature()
			}
			contentBlocks = append(contentBlocks, AnthropicContentBlock{
				Type:      "thinking",
				Thinking:  tb.Content,
				Signature: sig,
			})
		}
	}

	if parseTools {
		toolCalls := prepared.ToolCalls
		remaining := prepared.Remaining
		hasCalls := prepared.HasCalls
		doneText := prepared.DoneText

		if finalUsage != nil {
			aUsage.InputTokens = finalUsage.PromptTokens
			aUsage.CacheReadInputTokens = 0
			aUsage.OutputTokens = finalUsage.CompletionTokens
		}

		if prepared.Preamble != "" {
			contentBlocks = append(contentBlocks, AnthropicContentBlock{Type: "text", Text: prepared.Preamble})
		}
		if remaining != "" && !hasCalls {
			contentBlocks = append(contentBlocks, AnthropicContentBlock{Type: "text", Text: remaining})
		}
		if doneText != "" {
			contentBlocks = append(contentBlocks, AnthropicContentBlock{Type: "text", Text: doneText})
		}
		if hasCalls {
			stopReason = "tool_use"
			for _, tc := range toolCalls {
				contentBlocks = append(contentBlocks, AnthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: json.RawMessage(tc.Function.Arguments),
				})
			}
		}
	} else {
		if isJSONSchemaOutput(outputConfig) {
			content = normalizeStructuredOutputText(content)
		}
		contentBlocks = append(contentBlocks, AnthropicContentBlock{
			Type: "text",
			Text: cleanCitationsWithContext(content, knownToolCallURLs, knownCitationURLs, knownCitationDocs),
		})
	}

	resp := AnthropicResponse{
		ID:                requestID,
		Type:              "message",
		Role:              "assistant",
		Content:           contentBlocks,
		Model:             model,
		StopReason:        &stopReason,
		Usage:             aUsage,
		ContextManagement: sessionContextResponse(session),
	}

	LogAPIOutputJSON(requestID, "anthropic non-stream response", resp)
	setAnthropicRequestIDHeaders(w, requestID)
	setAnthropicBridgeTraceHeader(w, &bridgeTrace)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
	return nil
}

func setAnthropicRequestIDHeaders(w http.ResponseWriter, requestID string) {
	if w == nil || requestID == "" {
		return
	}
	w.Header().Set("X-Request-Id", requestID)
	w.Header().Set("X-Oneapi-Request-Id", requestID)
}

func setAnthropicBridgeTraceHeader(w http.ResponseWriter, trace *BridgeTrace) {
	if w == nil || trace == nil || (trace.TraceID == "" && trace.ThreadID == "" && trace.TotalMs == 0) {
		return
	}
	if raw, err := json.Marshal(trace); err == nil {
		w.Header().Set("X-Bridge-Trace", string(raw))
	}
}

func sendAnthropicMessageStop(w http.ResponseWriter, flusher http.Flusher, trace *BridgeTrace) {
	if trace != nil && (trace.TraceID != "" || trace.ThreadID != "" || trace.TotalMs > 0) {
		sendAnthropicSSE(w, flusher, "message_stop", map[string]interface{}{
			"type":         "message_stop",
			"bridge_trace": trace,
		})
		return
	}
	sendAnthropicSSE(w, flusher, "message_stop", map[string]string{"type": "message_stop"})
}

func anthropicMessageDeltaUsage(inputTokens, cacheReadInputTokens, outputTokens int) map[string]interface{} {
	usage := map[string]interface{}{
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
	}
	if cacheReadInputTokens > 0 {
		usage["cache_read_input_tokens"] = cacheReadInputTokens
	}
	return usage
}

func sendAnthropicSSE(w http.ResponseWriter, flusher http.Flusher, eventType string, data interface{}) {
	if fields, ok := data.(map[string]interface{}); ok {
		if value, exists := fields["context_management"]; exists {
			if response, ok := value.(*anthropicContextResponse); ok && response == nil {
				delete(fields, "context_management")
			}
		}
	}
	raw, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, raw)
	flusher.Flush()
}

func truncateForLog(s string, maxRunes int) string {
	if s == "" || maxRunes <= 0 || !utf8.ValidString(s) {
		return s
	}
	runes := 0
	for i := range s {
		if runes == maxRunes {
			return s[:i] + "..."
		}
		runes++
	}
	return s
}

func strPtr(s string) *string {
	return &s
}

func logAnthropicRequest(req AnthropicRequest, model string) {
	if !DebugLoggingEnabled() {
		return
	}
	log.Printf("[debug] Anthropic Request: model=%s max_tokens=%d stream=%v messages=%d tools=%d", model, req.MaxTokens, req.Stream, len(req.Messages), len(req.Tools))
	if AppConfig != nil && AppConfig.Server.DumpAPIInput && len(req.Tools) > 0 {
		toolDump, _ := json.MarshalIndent(req.Tools, "", "  ")
		_ = os.WriteFile("claude_code_tools_dump.json", toolDump, 0644)
	}
}

func logConvertedMessages(messages []ChatMessage) {
	if !DebugLoggingEnabled() {
		return
	}
	log.Printf("[debug] Converted to %d internal messages", len(messages))
}

func generateFakeSignature() string {
	b := make([]byte, 96)
	for i := range b {
		b[i] = byte(i*37 + 13)
	}
	return "EqQB" + base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b)
}

func isDesktopInferenceProbe(req AnthropicRequest, messages []ChatMessage, hasTools bool) bool {
	if hasTools {
		return false
	}
	ns := nonSystemMessages(messages)
	if len(ns) != 1 || ns[0].Role != "user" {
		return false
	}
	return strings.TrimSpace(normalizeSessionUserContent(ns[0].Content)) == "."
}

func writeDesktopProbeResponse(w http.ResponseWriter, requestID, model string, stream bool) {
	if stream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeAnthropicError(w, requestID, http.StatusInternalServerError, "streaming not supported", "api_error")
			return
		}
		ka := newAnthropicKeepalive(w, requestID, model)
		defer ka.Stop()
		ka.begin(1)
		sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		})
		sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]interface{}{"type": "text_delta", "text": "."},
		})
		sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": 0,
		})
		sendAnthropicSSE(w, flusher, "message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason":   "end_turn",
				"stop_sequence": nil,
			},
			"usage": map[string]int{
				"input_tokens":  1,
				"output_tokens": 1,
			},
		})
		sendAnthropicSSE(w, flusher, "message_stop", map[string]string{"type": "message_stop"})
		return
	}

	stopReason := "end_turn"
	resp := AnthropicResponse{
		ID:   requestID,
		Type: "message",
		Role: "assistant",
		Content: []AnthropicContentBlock{
			{Type: "text", Text: "."},
		},
		Model:      model,
		StopReason: &stopReason,
		Usage: &AnthropicUsage{
			InputTokens:  1,
			OutputTokens: 1,
		},
	}
	LogAPIOutputJSON(requestID, "anthropic desktop probe response", resp)
	setAnthropicRequestIDHeaders(w, requestID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}
