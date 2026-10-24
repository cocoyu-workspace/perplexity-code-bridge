package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func poolOf(accs ...*Account) *AccountPool {
	pool := NewAccountPool()
	for _, acc := range accs {
		pool.AddAccount(acc)
	}
	return pool
}

type errclassResp struct {
	Status  int
	Header  http.Header
	Type    string
	Message string
	Raw     string
}

func postMessages(t *testing.T, pool *AccountPool, stream bool, session string, msgs ...ChatMessage) errclassResp {
	t.Helper()
	var wire []map[string]string
	for _, m := range msgs {
		wire = append(wire, map[string]string{"role": m.Role, "content": m.Content})
	}
	body := map[string]interface{}{"model": "auto", "max_tokens": 64, "stream": stream, "messages": wire}
	if session != "" {
		body["metadata"] = map[string]interface{}{"user_id": `{"session_id":"` + session + `"}`}
	}
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	HandleAnthropicMessages(pool).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(raw))))
	waitQuotaIdle(pool)
	out := errclassResp{Status: rec.Code, Header: rec.Header(), Raw: rec.Body.String()}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	out.Type, out.Message = payload.Error.Type, payload.Error.Message
	return out
}

func waitQuotaIdle(pool *AccountPool) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pool.liveQuotaMu.Lock()
		n := len(pool.liveQuotaInflight)
		pool.liveQuotaMu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestClientFacingErrorsDoNotExposeBranding(t *testing.T) {
	candidates := []error{
		ErrQuotaExhausted,
		ErrPremiumFeatureUnavailable,
		ErrEmptyResponse,
		&toolBridgeMalformedError{Reason: "missing_required"},
		&agentModelError{model: "unknown-model"},
		fmt.Errorf(`Post "https://www.perplexity.ai/rest/sse/perplexity_ask": perplexity upstream failed`),
		fmt.Errorf(`Get "https://www.notion.so/api/v3/getUserAnalyticsSettings": notion account failed`),
	}
	forbidden := []string{"perplexity", "pplx", "notion", "notion.so", "perplexity.ai"}
	for _, err := range candidates {
		f := classifyAgentFailure(err)
		lower := strings.ToLower(f.Message)
		for _, brand := range forbidden {
			if strings.Contains(lower, brand) {
				t.Fatalf("classifyAgentFailure(%v) leaked forbidden brand %q in %q", err, brand, f.Message)
			}
		}
	}
}

func TestToolGuardValidAndMalformed(t *testing.T) {
	tools := buildDeclaredTools([]AnthropicTool{
		{
			Name:        "Bash",
			Description: "Execute bash command",
			InputSchema: map[string]interface{}{
				"type":     "object",
				"required": []interface{}{"command"},
				"properties": map[string]interface{}{
					"command": map[string]interface{}{"type": "string"},
				},
			},
		},
	})

	res, err := prepareToolBridgeResponseGuarded(`{"name":"Bash","arguments":{"command":"ls -la"}}`, nil, tools)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.HasCalls || len(res.ToolCalls) != 1 || res.ToolCalls[0].Function.Name != "Bash" {
		t.Fatalf("expected 1 Bash tool call, got %+v", res)
	}

	_, err = prepareToolBridgeResponseGuarded(`{"name":"Bash","arguments":{}}`, nil, tools)
	if err == nil {
		t.Fatalf("expected error for missing required 'command' field")
	}
}
