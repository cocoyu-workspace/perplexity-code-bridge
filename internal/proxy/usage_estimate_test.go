package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEstimateTokensAndUsageResolution(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "sonnet-5.5",
		MaxTokens: 1024,
		System:    "You are a coding assistant.",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "Read README.md and summarize it in 3 bullet points."},
		},
	}
	inEst := estimateAnthropicRequestTokens(req)
	if inEst <= 0 {
		t.Fatalf("expected positive input token estimate, got %d", inEst)
	}
	outEst := estimateTextTokens("Here is the summary:\n- Point 1\n- Point 2\n- Point 3")
	if outEst <= 0 {
		t.Fatalf("expected positive output token estimate, got %d", outEst)
	}

	sess := &Session{
		InputTokenEstimate:         120,
		FollowUpInputTokenEstimate: 30,
		LastTurnFollowUp:           true,
	}
	inTok, cacheReadTok, outTok := resolveAnthropicUsageWithCache(nil, sess, []ChatMessage{{Role: "user", Content: "next"}}, "reply text", "msg_test")
	if inTok != 30 || cacheReadTok != 90 || outTok <= 0 {
		t.Fatalf("unexpected resolved usage: in=%d cacheRead=%d out=%d", inTok, cacheReadTok, outTok)
	}
}

func TestHandleAnthropicCountTokensEndpoint(t *testing.T) {
	body := `{"model":"sonnet-5.5","messages":[{"role":"user","content":"Hello world"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	HandleAnthropicCountTokens().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.InputTokens <= 0 {
		t.Fatalf("input_tokens = %d, want > 0", resp.InputTokens)
	}
}
