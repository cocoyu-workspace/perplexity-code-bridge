package proxy

import (
	"encoding/json"
	"log"
	"unicode/utf8"
)

// Token usage for the Agent-thread transport.
//
// The Notion Agent transcript carries no token counts and the proxy never reads
// any (callAgentThreadResumable reports usage=nil), so Anthropic responses used to
// say input_tokens=0/output_tokens=0. Claude Code decides on auto-compaction from
// those numbers, so a long session would grow without bound. When the upstream
// gives no reading the proxy therefore reports an ESTIMATE:
//
//	input_tokens  = estimate of the whole client request (system + tools +
//	                every message incl. tool_use/tool_result/thinking blocks)
//	output_tokens = estimate of the text, thinking and tool request returned
//
// Rule (counted in quarter-token units, rounded up once at the end):
//
//	ASCII byte                      = 1 unit  (4 chars per token)
//	other rune (CJK, accents, ...)  = 6 units (1.5 tokens per char, conservative)
//	rune >= U+10000 (emoji, ...)    = 8 units (2 tokens per char)
//	base64 payload (images/files)   = imageTokenEstimate, never its length
//	each message                    = messageOverheadTokens
//
// It deliberately leans high so compaction triggers a little early rather than
// late. These are estimates, not upstream readings; logs mark them "estimated".
const (
	imageTokenEstimate    = 1500
	messageOverheadTokens = 4
)

func textUnits(s string) int {
	units := 0
	for _, r := range s {
		switch {
		case r < utf8.RuneSelf:
			units++
		case r >= 0x10000:
			units += 8
		default:
			units += 6
		}
	}
	return units
}

func unitsToTokens(units int) int { return (units + 3) / 4 }

// estimateTextTokens estimates the tokens of plain text (see the rule above).
func estimateTextTokens(s string) int { return unitsToTokens(textUnits(s)) }

// valueUnits walks a decoded JSON value (strings, arrays, objects, scalars).
func valueUnits(v interface{}) int {
	switch t := v.(type) {
	case nil:
		return 0
	case string:
		return textUnits(t)
	case float64, bool:
		return 4
	case []interface{}:
		n := 0
		for _, e := range t {
			n += valueUnits(e)
		}
		return n
	case map[string]interface{}:
		n := 0
		_, isBase64 := t["data"].(string)
		isBase64 = isBase64 && t["type"] == "base64"
		for k, e := range t {
			if isBase64 && k == "data" {
				n += imageTokenEstimate * 4
				continue
			}
			n += textUnits(k) + valueUnits(e)
		}
		return n
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return 0
		}
		var generic interface{}
		if json.Unmarshal(raw, &generic) != nil {
			return 0
		}
		return valueUnits(generic)
	}
}

// estimateAnthropicRequestTokens estimates the full client request.
func estimateAnthropicRequestTokens(req *AnthropicRequest) int {
	if req == nil {
		return 0
	}
	units := valueUnits(req.System)
	for _, m := range req.Messages {
		units += messageOverheadTokens*4 + textUnits(m.Role) + valueUnits(m.Content)
	}
	toolUnits := 0
	for _, tool := range req.Tools {
		toolUnits += valueUnits(tool)
	}
	if toolUnits > maxOpenAIToolsBlockJSONBytes && AppConfig != nil && AppConfig.Proxy.AgentThread {
		converted := make([]Tool, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Name == "DeferredToolPlaceholder" {
				continue
			}
			converted = append(converted, Tool{
				Type: "function",
				Function: ToolFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.InputSchema,
				},
			})
		}
		if compactedUnits := textUnits(buildOpenAIToolsBlock(converted)); compactedUnits < toolUnits {
			toolUnits = compactedUnits
		}
	}
	units += toolUnits
	return unitsToTokens(units)
}

// estimateAnthropicFollowUpDeltaTokens estimates only the trailing messages
// after the last assistant turn in the request. On a resumed thread
// (sendEventToAgentThread), the prior prefix (system + tools + history through
// the last assistant message) is already in the upstream thread and is reported
// as cache_read_input_tokens, while this delta is reported as input_tokens (#233).
// Returns 0 when the request has no prior assistant message.
func estimateAnthropicFollowUpDeltaTokens(req *AnthropicRequest) int {
	if req == nil || len(req.Messages) == 0 {
		return 0
	}
	lastAssistant := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "assistant" {
			lastAssistant = i
			break
		}
	}
	if lastAssistant < 0 || lastAssistant >= len(req.Messages)-1 {
		return 0
	}
	units := 0
	for _, m := range req.Messages[lastAssistant+1:] {
		units += messageOverheadTokens*4 + textUnits(m.Role) + valueUnits(m.Content)
	}
	tok := unitsToTokens(units)
	if tok < 1 {
		tok = 1
	}
	return tok
}

// estimateChatMessagesTokens is the fallback when only the converted messages are
// at hand (no tool definitions): system and history incl. tool calls/results.
func estimateChatMessagesTokens(msgs []ChatMessage) int {
	units := 0
	for _, m := range msgs {
		units += messageOverheadTokens*4 + textUnits(m.Role) + textUnits(m.Content) + textUnits(m.Name)
		for _, tc := range m.ToolCalls {
			units += textUnits(tc.Function.Name) + textUnits(tc.Function.Arguments)
		}
	}
	return unitsToTokens(units)
}

// estimateChatMessagesFollowUpDeltaTokens estimates the trailing messages after
// the last assistant message when only converted ChatMessages are available.
func estimateChatMessagesFollowUpDeltaTokens(msgs []ChatMessage) int {
	lastAssistant := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			lastAssistant = i
			break
		}
	}
	if lastAssistant < 0 || lastAssistant >= len(msgs)-1 {
		return 0
	}
	tok := estimateChatMessagesTokens(msgs[lastAssistant+1:])
	if tok < 1 {
		tok = 1
	}
	return tok
}

// requestInputEstimate: the request-level estimate recorded on the session, else
// the converted messages.
func requestInputEstimate(session *Session, messages []ChatMessage) int {
	if session != nil && session.InputTokenEstimate > 0 {
		return session.InputTokenEstimate
	}
	return estimateChatMessagesTokens(messages)
}

func valueBytes(v interface{}) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case string:
		return int64(len(t))
	case float64, bool:
		return 8
	case []interface{}:
		var n int64
		for _, e := range t {
			n += valueBytes(e)
		}
		return n
	case map[string]interface{}:
		var n int64
		for k, e := range t {
			n += int64(len(k)) + valueBytes(e)
		}
		return n
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return 0
		}
		return int64(len(raw))
	}
}

// estimateAnthropicRequestBytes estimates the UTF-8 byte footprint of the full
// client request (system + messages + tool schemas).
func estimateAnthropicRequestBytes(req *AnthropicRequest) int64 {
	if req == nil {
		return 0
	}
	n := valueBytes(req.System)
	for _, m := range req.Messages {
		n += int64(len(m.Role)) + valueBytes(m.Content)
	}
	for _, tool := range req.Tools {
		n += valueBytes(tool)
	}
	return n
}

// estimateChatMessagesBytes estimates the UTF-8 byte footprint of converted
// ChatMessages when the raw AnthropicRequest is not carried on the session.
func estimateChatMessagesBytes(msgs []ChatMessage) int64 {
	var n int64
	for _, m := range msgs {
		n += int64(len(m.Role) + len(m.Content) + len(m.Name))
		for _, tc := range m.ToolCalls {
			n += int64(len(tc.Function.Name) + len(tc.Function.Arguments))
		}
	}
	return n
}

// requestInputBytes returns the request-level input byte footprint recorded on
// the session, else falls back to the converted messages.
func requestInputBytes(session *Session, messages []ChatMessage) int64 {
	if session != nil && session.InputBytesEstimate > 0 {
		return session.InputBytesEstimate
	}
	return estimateChatMessagesBytes(messages)
}

// requestInputUsageEstimate splits the total request input estimate into
// uncached input_tokens (new trailing turn delta) and cache_read_input_tokens
// (already-in-thread prefix) when followUp is true. The sum in + cacheRead
// always equals the full requestInputEstimate so client context-window
// calculations (input_tokens + cache_read_input_tokens) remain unchanged (#233).
func requestInputUsageEstimate(session *Session, messages []ChatMessage, followUp bool) (in, cacheRead int) {
	total := requestInputEstimate(session, messages)
	if !followUp || total <= 1 {
		return total, 0
	}
	delta := 0
	if session != nil && session.FollowUpInputTokenEstimate > 0 {
		delta = session.FollowUpInputTokenEstimate
	} else {
		delta = estimateChatMessagesFollowUpDeltaTokens(messages)
	}
	if delta <= 0 || delta >= total {
		return total, 0
	}
	return delta, total - delta
}

// resolveAnthropicUsage returns the numbers for the Anthropic usage block: the
// upstream's own counts when it reported any, else the estimate. A non-empty
// requestID logs which one was used.
func resolveAnthropicUsage(real *UsageInfo, session *Session, messages []ChatMessage, outputText string, requestID string) (in, out int) {
	in, _, out = resolveAnthropicUsageWithCache(real, session, messages, outputText, requestID)
	return in, out
}

// resolveAnthropicUsageWithCache returns (input_tokens, cache_read_input_tokens, output_tokens).
// When the turn resumed an existing thread (session.LastTurnFollowUp), the
// prior history prefix is reported in cacheRead and only the new trailing turn
// delta is reported in in, preserving in + cacheRead == totalInputEstimate.
func resolveAnthropicUsageWithCache(real *UsageInfo, session *Session, messages []ChatMessage, outputText string, requestID string) (in, cacheRead, out int) {
	if real != nil && (real.PromptTokens > 0 || real.CompletionTokens > 0) {
		return real.PromptTokens, 0, real.CompletionTokens
	}
	followUp := session != nil && session.LastTurnFollowUp
	in, cacheRead = requestInputUsageEstimate(session, messages, followUp)
	out = estimateTextTokens(outputText)
	if requestID != "" {
		if cacheRead > 0 {
			log.Printf("[usage] %s estimated (upstream reported no token counts): input_tokens=%d cache_read_input_tokens=%d output_tokens=%d", requestID, in, cacheRead, out)
		} else {
			log.Printf("[usage] %s estimated (upstream reported no token counts): input_tokens=%d output_tokens=%d", requestID, in, out)
		}
	}
	return in, cacheRead, out
}

func thinkingBlocksText(blocks []ThinkingBlock) string {
	n := 0
	for _, b := range blocks {
		n += len(b.Content)
	}
	if n == 0 {
		return ""
	}
	buf := make([]byte, 0, n)
	for _, b := range blocks {
		buf = append(buf, b.Content...)
	}
	return string(buf)
}
