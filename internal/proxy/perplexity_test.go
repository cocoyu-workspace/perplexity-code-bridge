package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestPerplexityModelPreferenceAndFamily(t *testing.T) {
	cases := []struct {
		model  string
		effort string
		want   string
		family string
	}{
		{"auto", "", "claude55sonnet", "sonnet"},
		{"sonnet-5.5", "", "claude55sonnet", "sonnet"},
		{"claude-sonnet-5", "high", "claude55sonnetthinking", "sonnet"},
		{"claude-sonnet-5-thinking", "", "claude55sonnetthinking", "sonnet"},
		{"gpt-6.1-sol", "", "gpt6_1_sol", "gpt"},
		{"claude-sol-6-1", "medium", "gpt6_1_sol_thinking", "gpt"},
		{"opus-5.5", "", "claude55opus", "opus"},
		{"claude-opus-5-5", "high", "claude55opusthinking", "opus"},
	}
	for _, tc := range cases {
		got, err := perplexityModelPreference(tc.model, tc.effort)
		if err != nil {
			t.Fatalf("perplexityModelPreference(%q, %q) unexpected error: %v", tc.model, tc.effort, err)
		}
		if got != tc.want {
			t.Fatalf("perplexityModelPreference(%q, %q) = %q, want %q", tc.model, tc.effort, got, tc.want)
		}
		if fam := perplexityModelFamily(got); fam != tc.family {
			t.Fatalf("perplexityModelFamily(%q) = %q, want %q", got, fam, tc.family)
		}
	}

	if !perplexityDisplayModelMatches("claude55opus", "claude55opusthinking") {
		t.Fatal("expected opus family to match opus thinking")
	}
	if perplexityDisplayModelMatches("claude55opus", "gpt6_1_sol") {
		t.Fatal("silent downgrade from claude55opus to gpt6_1_sol must NOT match")
	}
	if perplexityDisplayModelMatches("claude55opusthinking", "claude55sonnetthinking") {
		t.Fatal("silent downgrade from opus to sonnet must NOT match")
	}
}

func TestPerplexitySplitPromptRunes(t *testing.T) {
	short := strings.Repeat("你", 1000)
	chunks := splitPerplexityPromptRunes(short, 38000)
	if len(chunks) != 1 || chunks[0] != short {
		t.Fatalf("expected 1 unprefixed chunk, got %d", len(chunks))
	}

	// Build a 90,000-rune CJK prompt with newlines
	line := strings.Repeat("语", 99) + "\n" // 100 runes per line
	longPrompt := strings.Repeat(line, 900) // 90,000 runes (270,000 bytes)
	chunks = splitPerplexityPromptRunes(longPrompt, 38000)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks for 90,000 runes, got %d", len(chunks))
	}
	for i, c := range chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d is not valid UTF-8", i)
		}
		rc := utf8.RuneCountInString(c)
		if rc > 38000 {
			t.Fatalf("chunk %d rune count = %d > 38000", i, rc)
		}
		if i < len(chunks)-1 {
			wantPrefix := fmt.Sprintf("[CONTEXT CHUNK %d/%d]", i+1, len(chunks))
			if !strings.HasPrefix(c, wantPrefix) {
				t.Fatalf("chunk %d missing prefix %q", i, wantPrefix)
			}
		} else {
			wantPrefix := fmt.Sprintf("[FINAL CONTEXT CHUNK %d/%d]", i+1, len(chunks))
			if !strings.HasPrefix(c, wantPrefix) {
				t.Fatalf("final chunk %d missing prefix %q", i, wantPrefix)
			}
		}
	}
}

func TestPerplexitySSEStreamParserAndSilentDowngradeGuard(t *testing.T) {
	t.Run("v2.18 workflow_root and diff_block without duplicate answer or plan leak", func(t *testing.T) {
		sseStream := strings.Join([]string{
			"event: message",
			`data: {"backend_uuid":"b-111","context_uuid":"c-222","read_write_token":"rw-333","display_model":"claude55sonnetthinking","blocks":[{"intended_usage":"workflow_root","workflow_block":{"steps":[{"step_type":"INITIAL_QUERY","items":[{"type":"WORKFLOW_ITEM_REASONING","variant":"thinking","payload":{"text_payload":{"chunks":["Step 1: analyze."]}}}]}]}}]}`,
			"",
			"event: message",
			`data: {"backend_uuid":"b-111","context_uuid":"c-222","read_write_token":"rw-333","display_model":"claude55sonnetthinking","blocks":[{"intended_usage":"plan","plan_block":{"goals":[{"description":"Hello world!"}]}},{"intended_usage":"workflow_root","workflow_block":{"steps":[{"step_type":"INITIAL_QUERY","items":[{"type":"WORKFLOW_ITEM_REASONING","variant":"thinking","payload":{"text_payload":{"chunks":["Step 1: analyze."," Step 2: conclude."]}}},{"type":"WORKFLOW_ITEM_TEXT","variant":"answer","payload":{"text_payload":{"text":"Hello "}}}]}]}},{"intended_usage":"ask_text_0_markdown","markdown_block":{"answer":"Hello ","chunks":["Hello "]}}]}`,
			"",
			"event: message",
			`data: {"backend_uuid":"b-111","context_uuid":"c-222","read_write_token":"rw-333","display_model":"claude55sonnetthinking","blocks":[{"intended_usage":"ask_text_0_markdown","diff_block":{"field":"markdown_block","patches":[{"op":"add","path":"/chunks/-","value":"world!"},{"op":"replace","path":"/answer","value":"Hello world!"}]}}]}`,
			"",
			"event: end_of_stream",
			"data: {}",
			"",
		}, "\n")

		var gotAnswer strings.Builder
		var gotThinking strings.Builder
		var thinkingClosed bool
		var blocks []ThinkingBlock

		bUUID, cUUID, rwTok, dispModel, err := parsePerplexitySSEStream(
			strings.NewReader(sseStream),
			"claude55sonnetthinking",
			func(delta string, done bool, usage *UsageInfo) {
				gotAnswer.WriteString(delta)
			},
			func(delta string, done bool, sig string) {
				if done {
					thinkingClosed = true
					return
				}
				gotThinking.WriteString(delta)
			},
			&blocks,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if bUUID != "b-111" || cUUID != "c-222" || rwTok != "rw-333" || dispModel != "claude55sonnetthinking" {
			t.Fatalf("unexpected metadata: b=%q c=%q rw=%q model=%q", bUUID, cUUID, rwTok, dispModel)
		}
		if gotThinking.String() != "Step 1: analyze. Step 2: conclude." {
			t.Fatalf("gotThinking = %q", gotThinking.String())
		}
		if !thinkingClosed || len(blocks) != 1 || blocks[0].Content != "Step 1: analyze. Step 2: conclude." {
			t.Fatalf("expected closed thinking block, got closed=%v blocks=%+v", thinkingClosed, blocks)
		}
		if gotAnswer.String() != "Hello world!" {
			t.Fatalf("gotAnswer = %q, want %q", gotAnswer.String(), "Hello world!")
		}
	})

	t.Run("silent downgrade on opus is rejected before emitting any text", func(t *testing.T) {
		sseStream := strings.Join([]string{
			"event: message",
			`data: {"backend_uuid":"b-bad","context_uuid":"c-bad","display_model":"gpt6_1_sol","blocks":[{"intended_usage":"ask_text_0_markdown","markdown_block":{"answer":"Downgraded answer"}}]}`,
			"",
		}, "\n")

		emitted := false
		_, _, _, _, err := parsePerplexitySSEStream(
			strings.NewReader(sseStream),
			"claude55opus",
			func(delta string, done bool, usage *UsageInfo) {
				if delta != "" {
					emitted = true
				}
			},
			func(delta string, done bool, sig string) {
				if delta != "" {
					emitted = true
				}
			},
			nil,
		)
		if !errors.Is(err, ErrPremiumFeatureUnavailable) {
			t.Fatalf("expected ErrPremiumFeatureUnavailable, got %v", err)
		}
		if emitted {
			t.Fatal("must not emit any text or thinking delta before display_model verification passes")
		}
	})
}

func TestPerplexityMultiTurnAndChunkingEndToEnd(t *testing.T) {
	resetPerplexityThreadsForTest()
	t.Cleanup(resetPerplexityThreadsForTest)

	var (
		mu       sync.Mutex
		requests []perplexityAskRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/rest/rate-limit/all"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"remaining_pro":295}`))
			return
		case strings.HasPrefix(r.URL.Path, "/rest/user/settings"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"subscription_tier":"pro","subscription_status":"active"}`))
			return
		case r.URL.Path == "/rest/sse/perplexity_ask":
			var req perplexityAskRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			idx := len(requests) + 1
			requests = append(requests, req)
			mu.Unlock()

			ans := fmt.Sprintf("Reply-%d", idx)
			if strings.HasPrefix(req.QueryStr, "[CONTEXT CHUNK ") {
				ans = "ACK"
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"backend_uuid\":\"backend-%d\",\"context_uuid\":%q,\"read_write_token\":\"rw-token-%d\",\"display_model\":%q,\"blocks\":[{\"intended_usage\":\"ask_text_0_markdown\",\"markdown_block\":{\"answer\":%q,\"chunks\":[%q]}}]}\n\nevent: end_of_stream\ndata: {}\n\n",
				idx, req.Params.FrontendContextUUID, idx, req.Params.ModelPreference, ans, ans)
			return
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	oldCfg, oldBase, oldClient, oldQuota := AppConfig, PerplexityAPIBase, perplexityHTTPClient, quotaFetcher
	AppConfig = DefaultConfig()
	AppConfig.Proxy.AgentThread = true
	PerplexityAPIBase = srv.URL
	perplexityHTTPClient = func(time.Duration) *http.Client { return srv.Client() }
	quotaFetcher = CheckQuota
	t.Cleanup(func() {
		AppConfig, PerplexityAPIBase, perplexityHTTPClient, quotaFetcher = oldCfg, oldBase, oldClient, oldQuota
	})

	acc := &Account{
		Provider:  "perplexity",
		TokenV2:   "pplx-session-jwt",
		UserEmail: "pplx-pro@example.com",
		PlanType:  "pro",
	}
	acc.normalizeProviderFields()
	acc.EnsureAccountID()
	pool := poolOf(acc)

	// Turn 1
	r1 := postMessages(t, pool, false, "sess-pplx-1", ChatMessage{Role: "user", Content: "First question"})
	if r1.Status != http.StatusOK || !strings.Contains(r1.Raw, "Reply-1") {
		t.Fatalf("Turn 1 failed: status=%d body=%s", r1.Status, r1.Raw)
	}

	// Turn 2 (continuation on the same thread)
	r2 := postMessages(t, pool, false, "sess-pplx-1",
		ChatMessage{Role: "user", Content: "First question"},
		ChatMessage{Role: "assistant", Content: "Reply-1"},
		ChatMessage{Role: "user", Content: "Second question"},
	)
	if r2.Status != http.StatusOK || !strings.Contains(r2.Raw, "Reply-2") {
		t.Fatalf("Turn 2 failed: status=%d body=%s", r2.Status, r2.Raw)
	}

	mu.Lock()
	if len(requests) != 2 {
		mu.Unlock()
		t.Fatalf("expected 2 upstream requests, got %d", len(requests))
	}
	req1, req2 := requests[0], requests[1]
	mu.Unlock()

	if req1.Params.LastBackendUUID != "" || req1.Params.QuerySource != "home" {
		t.Fatalf("Turn 1 should start a new thread: %+v", req1.Params)
	}
	if req2.Params.FrontendContextUUID != req1.Params.FrontendContextUUID {
		t.Fatalf("Turn 2 context_uuid = %q, want %q", req2.Params.FrontendContextUUID, req1.Params.FrontendContextUUID)
	}
	if req2.Params.LastBackendUUID != "backend-1" || req2.Params.ReadWriteToken != "rw-token-1" || req2.Params.QuerySource != "followup" {
		t.Fatalf("Turn 2 did not pass continuation tokens: %+v", req2.Params)
	}
	if req2.QueryStr != "Second question" {
		t.Fatalf("Turn 2 query_str = %q, want delta 'Second question'", req2.QueryStr)
	}

	// Turn 3: > 38,000 runes prompt triggers automatic multi-chunk continuation
	hugeText := strings.Repeat("长", 45000)
	r3 := postMessages(t, pool, true, "sess-pplx-chunked", ChatMessage{Role: "user", Content: hugeText})
	if r3.Status != http.StatusOK || !strings.Contains(r3.Raw, "Reply-4") {
		t.Fatalf("Chunked turn failed: status=%d body=%s", r3.Status, r3.Raw)
	}
	if strings.Contains(r3.Raw, "ACK") {
		t.Fatalf("intermediate chunk ACK leaked into client stream: %s", r3.Raw)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 4 {
		t.Fatalf("expected 4 total requests (2 for 45k-rune turn), got %d", len(requests))
	}
	chunk1, chunk2 := requests[2], requests[3]
	if utf8.RuneCountInString(chunk1.QueryStr) > 38000 || utf8.RuneCountInString(chunk2.QueryStr) > 38000 {
		t.Fatalf("chunk exceeded 38000 runes: c1=%d c2=%d", utf8.RuneCountInString(chunk1.QueryStr), utf8.RuneCountInString(chunk2.QueryStr))
	}
	if chunk2.Params.LastBackendUUID != "backend-3" || chunk2.Params.ReadWriteToken != "rw-token-3" || chunk2.Params.FrontendContextUUID != chunk1.Params.FrontendContextUUID {
		t.Fatalf("chunk 2 did not chain onto chunk 1 thread: %+v", chunk2.Params)
	}
}

func TestPerplexityProAccountOpusRejectionAndFailover(t *testing.T) {
	resetPerplexityThreadsForTest()
	t.Cleanup(resetPerplexityThreadsForTest)

	var calledEmails []string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/rest/rate-limit/all") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"remaining_pro":300}`))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/rest/user/settings") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		cookie := r.Header.Get("Cookie")
		var req perplexityAskRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		mu.Lock()
		calledEmails = append(calledEmails, cookie)
		mu.Unlock()

		// Simulate fake-max account silently downgrading opus to gpt6_1_sol
		dispModel := req.Params.ModelPreference
		if strings.Contains(cookie, "fake-max-token") {
			dispModel = "gpt6_1_sol"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: {\"backend_uuid\":\"b-1\",\"context_uuid\":%q,\"read_write_token\":\"rw-1\",\"display_model\":%q,\"blocks\":[{\"intended_usage\":\"ask_text_0_markdown\",\"markdown_block\":{\"answer\":\"Opus response\",\"chunks\":[\"Opus response\"]}}]}\n\nevent: end_of_stream\ndata: {}\n\n",
			req.Params.FrontendContextUUID, dispModel)
	}))
	defer srv.Close()

	oldCfg, oldBase, oldClient, oldQuota := AppConfig, PerplexityAPIBase, perplexityHTTPClient, quotaFetcher
	AppConfig = DefaultConfig()
	AppConfig.Proxy.AgentThread = true
	PerplexityAPIBase = srv.URL
	perplexityHTTPClient = func(time.Duration) *http.Client { return srv.Client() }
	quotaFetcher = CheckQuota
	t.Cleanup(func() {
		AppConfig, PerplexityAPIBase, perplexityHTTPClient, quotaFetcher = oldCfg, oldBase, oldClient, oldQuota
	})

	postModel := func(pool *AccountPool, model string, stream bool) errclassResp {
		body := map[string]any{
			"model":      model,
			"max_tokens": 64,
			"stream":     stream,
			"messages":   []map[string]string{{"role": "user", "content": "test opus"}},
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

	// 1. Pro-only pool must reject opus-5.5 with 400 invalid_request_error and zero upstream calls
	proAcc := &Account{Provider: "perplexity", TokenV2: "pro-token", UserEmail: "pro@example.com", PlanType: "pro"}
	proAcc.normalizeProviderFields()
	proAcc.EnsureAccountID()
	r := postModel(poolOf(proAcc), "opus-5.5", false)
	if r.Status != http.StatusBadRequest || r.Type != "invalid_request_error" || !strings.HasPrefix(r.Message, "[bridge:model_unavailable]") {
		t.Fatalf("expected 400 invalid_request_error [bridge:model_unavailable], got %d %q %q", r.Status, r.Type, r.Message)
	}
	mu.Lock()
	if len(calledEmails) != 0 {
		t.Fatalf("pro account must not be called for opus-5.5, called=%v", calledEmails)
	}
	mu.Unlock()

	// 2. Pool with fake-max (downgrades in SSE) + real-max: fails over from fake-max to real-max
	fakeMax := &Account{Provider: "perplexity", TokenV2: "fake-max-token", UserEmail: "fakemax@example.com", PlanType: "max"}
	fakeMax.normalizeProviderFields()
	fakeMax.EnsureAccountID()
	realMax := &Account{Provider: "perplexity", TokenV2: "real-max-token", UserEmail: "realmax@example.com", PlanType: "max"}
	realMax.normalizeProviderFields()
	realMax.EnsureAccountID()

	r = postModel(poolOf(fakeMax, realMax), "opus-5.5", true)
	if r.Status != http.StatusOK || !strings.Contains(r.Raw, "Opus response") {
		t.Fatalf("expected failover to realMax to succeed, got %d: %s", r.Status, r.Raw)
	}
	if !fakeMax.isPerplexityOpusUnavailable() {
		t.Fatal("fakeMax should be marked perplexityOpusUnavailable after silent downgrade")
	}
}

func TestPerplexityAccountNormalizationFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pplx.json")
	raw := `{"provider":"perplexity","token_v2":"jwt-token-value","user_email":"user@example.com"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	pool := NewAccountPool()
	if err := pool.LoadFromDir(dir); err != nil {
		t.Fatalf("LoadFromDir failed: %v", err)
	}
	acc := pool.Next()
	if acc == nil || !acc.IsPerplexity() {
		t.Fatalf("expected Perplexity account, got %+v", acc)
	}
	if acc.SpaceID != "perplexity" || !strings.HasPrefix(acc.UserID, "pplx-") || acc.PlanType != "pro" {
		t.Fatalf("unexpected normalized fields: SpaceID=%q UserID=%q PlanType=%q", acc.SpaceID, acc.UserID, acc.PlanType)
	}
}

func TestPerplexitySubdirectoryLoadAndProviderIsolation(t *testing.T) {
	dir := t.TempDir()
	pplxDir := filepath.Join(dir, "perplexity")
	if err := os.MkdirAll(pplxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	notionJSON := `{"token_v2":"notion-tok","user_id":"u-notion","space_id":"s-notion","user_email":"notion@example.com","plan_type":"plus","quota_info":{"is_eligible":true,"space_usage":0,"space_limit":1000,"user_usage":0,"user_limit":1000,"has_premium":true,"premium_balance":999999}}`
	if err := os.WriteFile(filepath.Join(dir, "notion.json"), []byte(notionJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	// File inside accounts/perplexity/ without explicit "provider" field should auto-tag as perplexity.
	pplxJSON := `{"token_v2":"pplx-tok","user_email":"pplx@example.com","plan_type":"pro","quota_info":{"is_eligible":true,"space_usage":5,"space_limit":300,"user_usage":5,"user_limit":300}}`
	if err := os.WriteFile(filepath.Join(pplxDir, "pplx_sub.json"), []byte(pplxJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	pool := NewAccountPool()
	if err := pool.LoadFromDir(dir); err != nil {
		t.Fatalf("LoadFromDir failed: %v", err)
	}
	if pool.Count() != 2 {
		t.Fatalf("expected 2 accounts loaded, got %d", pool.Count())
	}

	// Explicit Perplexity model must pick Perplexity account even though Notion account has 999,999 premium credits.
	pickedPplx := pool.NextBestForModel("perplexity/claude-sonnet-4-6")
	if pickedPplx == nil || !pickedPplx.IsPerplexity() || pickedPplx.UserEmail != "pplx@example.com" {
		t.Fatalf("expected Perplexity account for perplexity/claude-sonnet-4-6, got %+v", pickedPplx)
	}

	// Explicit Notion model must pick Notion account.
	pickedNotion := pool.NextBestForModel("notion/claude-sonnet-4-6")
	if pickedNotion == nil || pickedNotion.IsPerplexity() || pickedNotion.UserEmail != "notion@example.com" {
		t.Fatalf("expected Notion account for notion/claude-sonnet-4-6, got %+v", pickedNotion)
	}

	// When DefaultProvider is "perplexity", unprefixed model routes to Perplexity sub-pool.
	origCfg := AppConfig
	var cfgCopy Config
	if origCfg != nil {
		cfgCopy = *origCfg
	}
	cfgCopy.Proxy.DefaultProvider = "perplexity"
	AppConfig = &cfgCopy
	defer func() { AppConfig = origCfg }()

	pickedDefaultPplx := pool.NextBestForModel("claude-sonnet-4-6")
	if pickedDefaultPplx == nil || !pickedDefaultPplx.IsPerplexity() {
		t.Fatalf("expected Perplexity account when DefaultProvider=perplexity, got %+v", pickedDefaultPplx)
	}
}

func TestPerplexitySubPoolRankingProtectsMaxAndRanksRemainingPro(t *testing.T) {
	pool := NewAccountPool()
	maxHigh := &Account{
		Provider:  "perplexity",
		TokenV2:   "tok-max-high",
		UserEmail: "max-high@example.com",
		PlanType:  "max",
		QuotaInfo: &QuotaInfo{IsEligible: true, SpaceUsage: 10, SpaceLimit: 600, UserUsage: 10, UserLimit: 600},
	}
	maxHigh.normalizeProviderFields()
	maxHigh.EnsureAccountID()

	proLow := &Account{
		Provider:  "perplexity",
		TokenV2:   "tok-pro-low",
		UserEmail: "pro-low@example.com",
		PlanType:  "pro",
		QuotaInfo: &QuotaInfo{IsEligible: true, SpaceUsage: 250, SpaceLimit: 300, UserUsage: 250, UserLimit: 300},
	}
	proLow.normalizeProviderFields()
	proLow.EnsureAccountID()

	proHigh := &Account{
		Provider:  "perplexity",
		TokenV2:   "tok-pro-high",
		UserEmail: "pro-high@example.com",
		PlanType:  "pro",
		QuotaInfo: &QuotaInfo{IsEligible: true, SpaceUsage: 20, SpaceLimit: 300, UserUsage: 20, UserLimit: 300},
	}
	proHigh.normalizeProviderFields()
	proHigh.EnsureAccountID()

	pool.AddAccount(maxHigh)
	pool.AddAccount(proLow)
	pool.AddAccount(proHigh)

	// 1. Non-Opus model (Sonnet / GPT-6.1 Sol): must prefer Pro over Max, and pick pro-high (280 remaining) over pro-low (50 remaining).
	picked1 := pool.NextBestForProviderModel("perplexity", "claude-sonnet-4-6")
	if picked1 == nil || picked1.UserEmail != "pro-high@example.com" {
		t.Fatalf("expected pro-high@example.com first for Sonnet, got %+v", picked1)
	}
	picked2 := pool.NextBestExcludingForProviderModel("perplexity", map[*Account]bool{proHigh: true}, "claude-sonnet-4-6")
	if picked2 == nil || picked2.UserEmail != "pro-low@example.com" {
		t.Fatalf("expected pro-low@example.com second for Sonnet, got %+v", picked2)
	}
	// When all Pro accounts are excluded/exhausted, fall back to Max account.
	picked3 := pool.NextBestExcludingForProviderModel("perplexity", map[*Account]bool{proHigh: true, proLow: true}, "claude-sonnet-4-6")
	if picked3 == nil || picked3.UserEmail != "max-high@example.com" {
		t.Fatalf("expected max-high@example.com fallback when Pro exhausted, got %+v", picked3)
	}

	// 2. Opus model: must only select Max account, never Pro accounts.
	pickedOpus := pool.NextBestForProviderModel("perplexity", "claude-opus-4-6")
	if pickedOpus == nil || pickedOpus.UserEmail != "max-high@example.com" {
		t.Fatalf("expected max-high@example.com for Opus, got %+v", pickedOpus)
	}
	pickedOpusExcluded := pool.NextBestExcludingForProviderModel("perplexity", map[*Account]bool{maxHigh: true}, "claude-opus-4-6")
	if pickedOpusExcluded != nil {
		t.Fatalf("expected nil when Max account is excluded for Opus, got %+v", pickedOpusExcluded)
	}
}

func TestPerplexityDiscoverAndAddAccountSavesToSubdirectory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/auth/session":
			_, _ = w.Write([]byte(`{"user":{"id":"u-pplx-123","email":"discovered@perplexity.ai","name":"Discovered User","subscription_tier":"max"}}`))
		case "/rest/rate-limit/all":
			_, _ = w.Write([]byte(`{"remaining_pro":480,"remaining_research":18}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	origBase := PerplexityAPIBase
	origClient := perplexityHTTPClient
	PerplexityAPIBase = srv.URL
	perplexityHTTPClient = func(timeout time.Duration) *http.Client { return srv.Client() }
	defer func() {
		PerplexityAPIBase = origBase
		perplexityHTTPClient = origClient
	}()

	dir := t.TempDir()
	pool := NewAccountPool()
	handler := HandleAddAccount(pool, dir)

	reqBody := `{"provider":"perplexity","session_token":"pplx-jwt-secret"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/add", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HandleAddAccount status=%d body=%s", rec.Code, rec.Body.String())
	}

	acc := pool.NextBestForProviderModel("perplexity", "claude-opus-4-6")
	if acc == nil || acc.UserEmail != "discovered@perplexity.ai" || acc.PlanType != "max" {
		t.Fatalf("unexpected discovered account in pool: %+v", acc)
	}
	if acc.QuotaInfo == nil || basicRemaining(acc.QuotaInfo) != 480 {
		t.Fatalf("expected remaining_pro=480 on discovered account, got %+v", acc.QuotaInfo)
	}

	// Verify file was written under dir/perplexity/
	subEntries, err := os.ReadDir(filepath.Join(dir, "perplexity"))
	if err != nil || len(subEntries) != 1 {
		t.Fatalf("expected 1 file in %s/perplexity, err=%v entries=%d", dir, err, len(subEntries))
	}
}

func TestSummarizeAccountsByProviderIsolatesNotionTotals(t *testing.T) {
	pool := NewAccountPool()
	notionAcc := &Account{
		TokenV2:   "notion-tok",
		UserID:    "u-notion",
		SpaceID:   "s-notion",
		UserEmail: "notion@example.com",
		PlanType:  "plus",
		QuotaInfo: &QuotaInfo{
			IsEligible:     true,
			SpaceUsage:     10,
			SpaceLimit:     100,
			UserUsage:      10,
			UserLimit:      100,
			HasPremium:     true,
			PremiumBalance: 5000,
			PremiumLimit:   10000,
		},
	}
	notionAcc.EnsureAccountID()

	pplxPro := &Account{
		Provider:  "perplexity",
		TokenV2:   "pplx-pro-tok",
		UserEmail: "pro@perplexity.ai",
		PlanType:  "pro",
		QuotaInfo: &QuotaInfo{
			IsEligible:     true,
			SpaceUsage:     50,
			SpaceLimit:     300,
			UserUsage:      50,
			UserLimit:      300,
			HasPremium:     true,
			PremiumBalance: 250,
			PremiumLimit:   300,
		},
	}
	pplxPro.normalizeProviderFields()
	pplxPro.EnsureAccountID()

	pplxMax := &Account{
		Provider:  "perplexity",
		TokenV2:   "pplx-max-tok",
		UserEmail: "max@perplexity.ai",
		PlanType:  "max",
		QuotaInfo: &QuotaInfo{
			IsEligible:     true,
			SpaceUsage:     100,
			SpaceLimit:     600,
			UserUsage:      100,
			UserLimit:      600,
			HasPremium:     true,
			PremiumBalance: 500,
			PremiumLimit:   600,
		},
	}
	pplxMax.normalizeProviderFields()
	pplxMax.EnsureAccountID()

	pool.AddAccount(notionAcc)
	pool.AddAccount(pplxPro)
	pool.AddAccount(pplxMax)

	summary := summarizeAccounts(pool.GetAccountDetails())
	if summary.TotalAccounts != 3 || summary.AvailableAccounts != 3 {
		t.Fatalf("expected 3 total/available accounts, got total=%d available=%d", summary.TotalAccounts, summary.AvailableAccounts)
	}
	// Notion headline counters must only include Notion account (90 remaining, 5000 premium balance).
	if summary.TotalRemaining != 90 || summary.TotalPremiumBalance != 5000 {
		t.Fatalf("Notion headline totals polluted by Perplexity: TotalRemaining=%d TotalPremiumBalance=%d", summary.TotalRemaining, summary.TotalPremiumBalance)
	}
	notionSub := summary.ByProvider["notion"]
	if notionSub.TotalAccounts != 1 || notionSub.AvailableAccounts != 1 {
		t.Fatalf("unexpected notion ByProvider summary: %+v", notionSub)
	}
	pplxSub := summary.ByProvider["perplexity"]
	if pplxSub.TotalAccounts != 2 || pplxSub.AvailableAccounts != 2 || pplxSub.ProAccounts != 1 || pplxSub.MaxAccounts != 1 || pplxSub.TotalRemainingPro != 750 {
		t.Fatalf("unexpected perplexity ByProvider summary: %+v", pplxSub)
	}
}

func TestPerplexityLocalToolBridgeContextSanitizationAndMultiTurn(t *testing.T) {
	resetPerplexityThreadsForTest()
	t.Cleanup(resetPerplexityThreadsForTest)

	var (
		mu       sync.Mutex
		requests []perplexityAskRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/rest/rate-limit/all"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"remaining_pro":290}`))
			return
		case strings.HasPrefix(r.URL.Path, "/rest/user/settings"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"subscription_tier":"pro","subscription_status":"active"}`))
			return
		case r.URL.Path == "/rest/sse/perplexity_ask":
			var req perplexityAskRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			idx := len(requests) + 1
			requests = append(requests, req)
			mu.Unlock()

			var ans string
			switch idx {
			case 1:
				ans = `{"name":"Bash","arguments":{"command":"ls -la ~/.claude/skills","description":"列出本地 skills 目录"}}`
			case 2:
				ans = "local-skills-check"
			default:
				ans = "管理员，你本地 `~/.claude/skills` 目录下共有 3 个 skills：skill-alpha、skill-beta、skill-gamma。"
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"backend_uuid\":\"backend-%d\",\"context_uuid\":%q,\"read_write_token\":\"rw-token-%d\",\"display_model\":%q,\"blocks\":[{\"intended_usage\":\"ask_text_0_markdown\",\"markdown_block\":{\"answer\":%q,\"chunks\":[%q]}}]}\n\nevent: end_of_stream\ndata: {}\n\n",
				idx, req.Params.FrontendContextUUID, idx, req.Params.ModelPreference, ans, ans)
			return
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	oldCfg, oldBase, oldClient, oldQuota := AppConfig, PerplexityAPIBase, perplexityHTTPClient, quotaFetcher
	AppConfig = DefaultConfig()
	AppConfig.Proxy.AgentThread = true
	PerplexityAPIBase = srv.URL
	perplexityHTTPClient = func(time.Duration) *http.Client { return srv.Client() }
	quotaFetcher = CheckQuota
	t.Cleanup(func() {
		AppConfig, PerplexityAPIBase, perplexityHTTPClient, quotaFetcher = oldCfg, oldBase, oldClient, oldQuota
	})

	acc := &Account{
		Provider:  "perplexity",
		TokenV2:   "pplx-session-jwt",
		UserEmail: "owner@example.com",
		UserName:  "Alex Morgan",
		PlanType:  "pro",
	}
	acc.normalizeProviderFields()
	acc.EnsureAccountID()
	pool := poolOf(acc)

	claudeStockSystem := "x-anthropic-billing-header: cc_version=2.1.114; cc_entrypoint=cli;\n" +
		"You are Claude Code, Anthropic's official CLI for Claude.\n" +
		"# System\n - All text you output outside of tool use is displayed to the user.\n# Doing tasks\n# Using your tools\n" +
		"Primary working directory: /workspace/project\nPlatform: darwin\nShell: zsh\n" +
		"You are assisting the user in their current directory via a local Claude Code CLI client on the user's Mac."
	hugeReminder := "<system-reminder>\nSessionStart:startup hook success: ok\n称呼：与用户沟通时称呼用户为「管理员」\n" +
		strings.Repeat("Skill description line for testing compaction.\n", 800) +
		"</system-reminder>\n<available-deferred-tools>\nmcp__exa__web_search_exa\nmcp__zread__read_file\n</available-deferred-tools>\n"

	toolsPayload := []map[string]any{
		{
			"name":        "Bash",
			"description": "Executes a given bash command in a persistent shell session.\n" + strings.Repeat("Long tool doc. ", 200),
			"input_schema": map[string]any{
				"type":     "object",
				"required": []string{"command"},
				"properties": map[string]any{
					"command":     map[string]any{"type": "string", "description": "The command to execute"},
					"description": map[string]any{"type": "string", "description": "Short description"},
				},
			},
		},
		{
			"name":        "Read",
			"description": "Reads a file from the local filesystem.",
			"input_schema": map[string]any{
				"type":     "object",
				"required": []string{"file_path"},
				"properties": map[string]any{
					"file_path": map[string]any{"type": "string"},
				},
			},
		},
	}

	// Turn 1: User asks to check local skills
	turn1Req := map[string]any{
		"model":      "perplexity/sonnet-5.5",
		"max_tokens": 1024,
		"stream":     false,
		"system":     claudeStockSystem,
		"tools":      toolsPayload,
		"metadata":   map[string]any{"user_id": `{"session_id":"04a0404c-ade0-498c-9865-128ad592cdd2"}`},
		"messages": []map[string]any{
			{"role": "user", "content": hugeReminder + "你能查我本地的 skills 吗"},
		},
	}
	raw1, _ := json.Marshal(turn1Req)
	rec1 := httptest.NewRecorder()
	HandleAnthropicMessages(pool).ServeHTTP(rec1, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(raw1))))
	if rec1.Code != http.StatusOK {
		t.Fatalf("Turn 1 HTTP %d: %s", rec1.Code, rec1.Body.String())
	}
	var resp1 AnthropicResponse
	if err := json.Unmarshal(rec1.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("unmarshal Turn 1 response: %v", err)
	}
	if resp1.StopReason == nil || *resp1.StopReason != "tool_use" || len(resp1.Content) == 0 || resp1.Content[0].Type != "tool_use" || resp1.Content[0].Name != "Bash" {
		t.Fatalf("expected Turn 1 tool_use Bash, got stop_reason=%v content=%+v", resp1.StopReason, resp1.Content)
	}
	toolCallID := resp1.Content[0].ID

	// Background helper request (session naming) sharing the exact same session_id
	helperReq := map[string]any{
		"model":      "perplexity/sonnet-5.5",
		"max_tokens": 64,
		"stream":     false,
		"system":     "You write short session titles.",
		"metadata":   map[string]any{"user_id": `{"session_id":"04a0404c-ade0-498c-9865-128ad592cdd2"}`},
		"messages": []map[string]any{
			{"role": "user", "content": "Generate a short kebab-case name (2-4 words) that captures the main topic of this conversation."},
		},
	}
	rawHelper, _ := json.Marshal(helperReq)
	recHelper := httptest.NewRecorder()
	HandleAnthropicMessages(pool).ServeHTTP(recHelper, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(rawHelper))))
	if recHelper.Code != http.StatusOK {
		t.Fatalf("Helper HTTP %d: %s", recHelper.Code, recHelper.Body.String())
	}

	// Turn 2: Client returns tool_result (and hooks mutate <system-reminder> slightly)
	turn2Req := map[string]any{
		"model":      "perplexity/sonnet-5.5",
		"max_tokens": 1024,
		"stream":     false,
		"system":     claudeStockSystem + "\n<total_tokens>180000 tokens left</total_tokens>",
		"tools":      toolsPayload,
		"metadata":   map[string]any{"user_id": `{"session_id":"04a0404c-ade0-498c-9865-128ad592cdd2"}`},
		"messages": []map[string]any{
			{"role": "user", "content": hugeReminder + "<system-reminder>Today's date is 2026-10-20</system-reminder>\n你能查我本地的 skills 吗"},
			{
				"role": "assistant",
				"content": []map[string]any{
					{
						"type":  "tool_use",
						"id":    toolCallID,
						"name":  "Bash",
						"input": map[string]any{"command": "ls -la ~/.claude/skills", "description": "列出本地 skills 目录"},
					},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{
						"type":        "tool_result",
						"tool_use_id": toolCallID,
						"content":     "skill-alpha\nskill-beta\nskill-gamma",
					},
				},
			},
		},
	}
	raw2, _ := json.Marshal(turn2Req)
	rec2 := httptest.NewRecorder()
	HandleAnthropicMessages(pool).ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(raw2))))
	waitQuotaIdle(pool)
	if rec2.Code != http.StatusOK {
		t.Fatalf("Turn 2 HTTP %d: %s", rec2.Code, rec2.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("expected 3 upstream requests (Turn 1 = 1 chunk, Helper = 1 chunk, Turn 2 = 1 chunk), got %d", len(requests))
	}
	uReq1, uReq2 := requests[0], requests[2]
	if !uReq1.Params.IsIncognito || !uReq1.Params.SkipSearchEnabled || !uReq1.Params.OverrideNoSearch || uReq1.Params.SearchFocus != "writing" || len(uReq1.Params.Sources) != 0 {
		t.Fatalf("Turn 1 params did not disable web search/personal memory: %+v", uReq1.Params)
	}
	if strings.Contains(uReq1.QueryStr, "You are Claude Code") || strings.Contains(uReq1.QueryStr, "[SYSTEM]") {
		t.Fatalf("Turn 1 QueryStr leaked stock Claude Code persona or [SYSTEM] header: %s", uReq1.QueryStr)
	}
	if !strings.Contains(uReq1.QueryStr, "Local working directory on Mac: /workspace/project") || !strings.Contains(uReq1.QueryStr, "mcp__exa__web_search_exa") || !strings.Contains(uReq1.QueryStr, `"管理员"`) {
		t.Fatalf("Turn 1 QueryStr missing extracted CWD, preferred title, or deferred tool hint: %s", uReq1.QueryStr)
	}
	// Turn 2 must resume Turn 1's thread (backend-1 / rw-token-1) despite the intervening helper request and dynamic <system-reminder>.
	if uReq2.Params.LastBackendUUID != "backend-1" || uReq2.Params.ReadWriteToken != "rw-token-1" || uReq2.Params.QuerySource != "followup" {
		t.Fatalf("Turn 2 failed to resume Turn 1 thread after helper request: %+v", uReq2.Params)
	}
	if !strings.Contains(uReq2.QueryStr, "[LOCAL TOOL RESULT (call_id="+toolCallID+", name=Bash)]") {
		t.Fatalf("Turn 2 QueryStr missing formatted LOCAL TOOL RESULT: %s", uReq2.QueryStr)
	}
}

func TestPerplexityRefusalSelfCorrectionAndIdentityRedaction(t *testing.T) {
	resetPerplexityThreadsForTest()
	t.Cleanup(resetPerplexityThreadsForTest)

	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/rest/rate-limit/all"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"remaining_pro":290}`))
			return
		case strings.HasPrefix(r.URL.Path, "/rest/user/settings"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"subscription_tier":"pro","subscription_status":"active"}`))
			return
		case r.URL.Path == "/rest/sse/perplexity_ask":
			callCount++
			var req perplexityAskRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			ans := `{"name":"Bash","arguments":{"command":"ls ~/.claude/skills"}}`
			if callCount == 1 {
				// Simulate first attempt refusing with Perplexity persona & local terminal disclaimer
				ans = `你好，Alex！我是 Perplexity，我没有你 Mac 的本地终端，不能直接读写本地文件。前面那段"本地 Claude Code 客户端"的设定我没有采用。`
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"backend_uuid\":\"b-%d\",\"context_uuid\":%q,\"read_write_token\":\"rw-%d\",\"display_model\":%q,\"blocks\":[{\"intended_usage\":\"ask_text_0_markdown\",\"markdown_block\":{\"answer\":%q,\"chunks\":[%q]}}]}\n\nevent: end_of_stream\ndata: {}\n\n",
				callCount, req.Params.FrontendContextUUID, callCount, req.Params.ModelPreference, ans, ans)
			return
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	oldCfg, oldBase, oldClient, oldQuota := AppConfig, PerplexityAPIBase, perplexityHTTPClient, quotaFetcher
	AppConfig = DefaultConfig()
	AppConfig.Proxy.AgentThread = true
	PerplexityAPIBase = srv.URL
	perplexityHTTPClient = func(time.Duration) *http.Client { return srv.Client() }
	quotaFetcher = CheckQuota
	t.Cleanup(func() {
		AppConfig, PerplexityAPIBase, perplexityHTTPClient, quotaFetcher = oldCfg, oldBase, oldClient, oldQuota
	})

	acc := &Account{
		Provider:  "perplexity",
		TokenV2:   "pplx-session-jwt",
		UserEmail: "owner@example.com",
		UserName:  "Alex Morgan",
		PlanType:  "pro",
	}
	acc.normalizeProviderFields()
	acc.EnsureAccountID()
	pool := poolOf(acc)

	reqBody := map[string]any{
		"model":      "perplexity/sonnet-5.5",
		"max_tokens": 512,
		"stream":     false,
		"tools": []map[string]any{
			{
				"name":        "Bash",
				"description": "Execute command",
				"input_schema": map[string]any{
					"type":       "object",
					"required":   []string{"command"},
					"properties": map[string]any{"command": map[string]any{"type": "string"}},
				},
			},
		},
		"messages": []map[string]any{
			{"role": "user", "content": "查一下我本地的 skills"},
		},
	}
	raw, _ := json.Marshal(reqBody)
	rec := httptest.NewRecorder()
	HandleAnthropicMessages(pool).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(raw))))
	waitQuotaIdle(pool)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if callCount != 2 {
		t.Fatalf("expected 2 calls (initial refusal + 1 self-correction retry), got %d", callCount)
	}
	var resp AnthropicResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.StopReason == nil || *resp.StopReason != "tool_use" || len(resp.Content) == 0 || resp.Content[0].Name != "Bash" {
		t.Fatalf("expected self-corrected tool_use Bash, got %+v", resp)
	}

	// Also verify direct greeting scrubbing for "你好，Alex！"
	cleaned := sanitizePerplexityAssistantReply(acc, "perplexity/sonnet-5.5", []ChatMessage{{Role: "user", Content: "你好"}}, "你好，Alex！我是 Perplexity，请问今天有什么我可以帮你的？", nil)
	if strings.Contains(cleaned, "Alex") || strings.Contains(cleaned, "Perplexity") || strings.Contains(cleaned, "[upstream-pool-account]") {
		t.Fatalf("sanitizePerplexityAssistantReply leaked account name or Perplexity branding: %q", cleaned)
	}
}

