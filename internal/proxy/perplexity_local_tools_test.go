package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	pplxDenialNoChannel = "我是 Claude Code 本地编程助手。你在消息里说要通过本地 Mac CLI 中间件来执行文件和命令操作，但我这边没有这个通道。我能用的是已连接的云端服务和代码沙箱，所以没法在你的 Mac 上读写文件或跑命令。"
	pplxDenialSnapshot  = "我现在没法实时扫描你的 Mac，下面来自前面上下文，是快照不是现查。"
	pplxDenialCloudRepo = "我能用的是 GitHub 云端连接，不是你 Mac 本地目录。登录账号 example-user，新仓库返回 404，建议改另一个仓库或 fork。"
	pplxDenialEnglish   = "I can only use cloud connectors and a code sandbox; I don't have a local channel to run commands on your Mac."
	pplxToolJSON        = `{"name":"Bash","arguments":{"command":"echo ok"}}`
)

func TestPerplexityLocalToolsDenialDetection(t *testing.T) {
	greeting := []ChatMessage{{Role: "user", Content: "你好"}}
	skills := []ChatMessage{{Role: "user", Content: "查一下我本地的 skills"}}

	positives := []struct {
		name string
		text string
		msgs []ChatMessage
	}{
		{"no_channel_cloud_substitute", pplxDenialNoChannel, greeting},
		{"snapshot_not_live", pplxDenialSnapshot, skills},
		{"cloud_github_instead_of_local", pplxDenialCloudRepo, []ChatMessage{{Role: "user", Content: "看看本地仓库和 PR"}}},
		{"english", pplxDenialEnglish, skills},
	}
	for _, tc := range positives {
		if !isPerplexityLocalRefusal(tc.text, true, tc.msgs) {
			t.Errorf("%s: expected denial to be detected: %q", tc.name, tc.text)
		}
	}

	negatives := []struct {
		name     string
		text     string
		hasTools bool
		msgs     []ChatMessage
	}{
		{"plain_greeting", "你好！我是 Claude Code 本地编程助手，可以帮你读写本地文件、运行命令。有什么需要？", true, greeting},
		{"unrelated_github_help", "GitHub 云端连接可以在设置页里配置。", true, []ChatMessage{{Role: "user", Content: "怎么配置 github"}}},
		{"json_tool_args_mention_phrase", `{"name":"Bash","arguments":{"command":"echo 没有这个通道"}}`, true, skills},
		{"fenced_json", "```json\n{\"name\":\"Bash\",\"arguments\":{\"command\":\"echo 没有这个通道\"}}\n```", true, skills},
		{"user_quotes_phrase", "那句“没有这个通道”是模型的错误自述，可以忽略。", true, []ChatMessage{{Role: "user", Content: "为什么模型会说『没有这个通道』"}}},
		{"identity_question", pplxDenialNoChannel, true, []ChatMessage{{Role: "user", Content: "你是谁"}}},
		{"no_client_tools", pplxDenialNoChannel, false, greeting},
		{"specific_permission_requirement", "无法在你的 Mac 上直接安装这个组件，需要你先授权 sudo。现有文件读取通道仍可使用。", true, skills},
	}
	for _, tc := range negatives {
		if isPerplexityLocalRefusal(tc.text, tc.hasTools, tc.msgs) {
			t.Errorf("%s: unexpected denial detection: %q", tc.name, tc.text)
		}
	}
}

type pplxLocalToolsRig struct {
	mu   sync.Mutex
	reqs []perplexityAskRequest
}

func (r *pplxLocalToolsRig) all() []perplexityAskRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]perplexityAskRequest(nil), r.reqs...)
}

// newPplxLocalToolsRig starts a mock upstream; answer receives the 1-based
// request number and the decoded request.
func newPplxLocalToolsRig(t *testing.T, answer func(n int, req perplexityAskRequest) string) (*pplxLocalToolsRig, *Account) {
	t.Helper()
	resetPerplexityThreadsForTest()
	t.Cleanup(resetPerplexityThreadsForTest)

	rig := &pplxLocalToolsRig{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/rest/rate-limit/all"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"remaining_pro":290}`))
		case strings.HasPrefix(r.URL.Path, "/rest/user/settings"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"subscription_tier":"pro","subscription_status":"active"}`))
		case r.URL.Path == "/rest/sse/perplexity_ask":
			var req perplexityAskRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			rig.mu.Lock()
			rig.reqs = append(rig.reqs, req)
			n := len(rig.reqs)
			rig.mu.Unlock()
			ans := answer(n, req)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"backend_uuid\":\"backend-%d\",\"context_uuid\":%q,\"read_write_token\":\"rw-token-%d\",\"display_model\":%q,\"blocks\":[{\"intended_usage\":\"ask_text_0_markdown\",\"markdown_block\":{\"answer\":%q,\"chunks\":[%q]}}]}\n\nevent: end_of_stream\ndata: {}\n\n",
				n, req.Params.FrontendContextUUID, n, req.Params.ModelPreference, ans, ans)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

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
		UserEmail: "user@example.test",
		UserName:  "Example User",
		PlanType:  "pro",
	}
	acc.normalizeProviderFields()
	acc.EnsureAccountID()
	return rig, acc
}

func (r *pplxLocalToolsRig) post(t *testing.T, acc *Account, tools []map[string]any, user string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{
		"model":      "perplexity/sonnet-5.5",
		"max_tokens": 512,
		"stream":     false,
		"messages":   []map[string]any{{"role": "user", "content": user}},
	}
	if tools != nil {
		body["tools"] = tools
	}
	raw, _ := json.Marshal(body)
	pool := poolOf(acc)
	rec := httptest.NewRecorder()
	HandleAnthropicMessages(pool).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(raw))))
	waitQuotaIdle(pool)
	return rec
}

func pplxLocalTestTools(descPad int) []map[string]any {
	return []map[string]any{
		{
			"name":        "Bash",
			"description": "Executes a bash command.\n" + strings.Repeat("Long tool doc. ", descPad/15),
			"input_schema": map[string]any{
				"type":       "object",
				"required":   []string{"command"},
				"properties": map[string]any{"command": map[string]any{"type": "string"}},
			},
		},
		{
			"name":        "Read",
			"description": "Reads a file.",
			"input_schema": map[string]any{
				"type":       "object",
				"required":   []string{"file_path"},
				"properties": map[string]any{"file_path": map[string]any{"type": "string"}},
			},
		},
	}
}

func pplxFillerLines(runes int, marker string) string {
	var b strings.Builder
	for i := 0; utf8.RuneCountInString(b.String()) < runes; i++ {
		fmt.Fprintf(&b, "filler line %06d for chunking\n", i)
	}
	b.WriteString(marker)
	return b.String()
}

func TestPerplexityLocalToolsGreetingDenialIsCorrectedOnce(t *testing.T) {
	rig, acc := newPplxLocalToolsRig(t, func(n int, _ perplexityAskRequest) string {
		if n == 1 {
			return pplxDenialNoChannel
		}
		return "你好！有什么可以帮你？"
	})
	rec := rig.post(t, acc, pplxLocalTestTools(0), "你好")
	reqs := rig.all()
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if len(reqs) != 2 {
		t.Fatalf("expected 1 upstream call + exactly 1 correction, got %d", len(reqs))
	}
	if !strings.Contains(reqs[1].QueryStr, "LOCAL ACTION PROTOCOL") {
		t.Fatalf("correction prompt lost the local tool protocol")
	}
	body := rec.Body.String()
	if strings.Contains(body, "没有这个通道") || strings.Contains(body, "代码沙箱") || !strings.Contains(body, "你好！") {
		t.Fatalf("unexpected body after correction: %s", body)
	}
}

func TestPerplexityLocalToolsPersistentDenialReturnsNeutralError(t *testing.T) {
	rig, acc := newPplxLocalToolsRig(t, func(int, perplexityAskRequest) string {
		return pplxDenialCloudRepo
	})
	rec := rig.post(t, acc, pplxLocalTestTools(0), "看看本地仓库和 PR")
	body := rec.Body.String()
	if rec.Code == http.StatusOK {
		t.Fatalf("persistent denial must not be returned as a successful reply: %s", body)
	}
	for _, leaked := range []string{"GitHub 云端", "404", "example-user", "fork"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("error body leaked denial text %q: %s", leaked, body)
		}
	}
	if strings.Contains(strings.ToLower(body), "notion") {
		t.Fatalf("client-facing error exposes upstream branding: %s", body)
	}
	t.Logf("persistent denial: status=%d upstream_calls=%d body=%s", rec.Code, len(rig.all()), body)
	if n := len(rig.all()); n > 4 {
		t.Fatalf("correction must stay bounded, got %d upstream calls", n)
	}
}

func TestPerplexityLocalToolsPlainGreetingNotForcedToTool(t *testing.T) {
	rig, acc := newPplxLocalToolsRig(t, func(int, perplexityAskRequest) string {
		return "你好！我是 Claude Code 本地编程助手，需要我做什么？"
	})
	rec := rig.post(t, acc, pplxLocalTestTools(0), "你好")
	if rec.Code != http.StatusOK || len(rig.all()) != 1 {
		t.Fatalf("plain greeting should pass through once: code=%d calls=%d body=%s", rec.Code, len(rig.all()), rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "tool_use") {
		t.Fatalf("greeting was forced into a tool call: %s", rec.Body.String())
	}
}

// The tool protocol is stranded in the first (ACK-only) chunk when the final
// chunk is too full to prepend it again.
func TestPerplexityLocalToolsProtocolSurvivesChunkSplit(t *testing.T) {
	rig, acc := newPplxLocalToolsRig(t, func(int, perplexityAskRequest) string { return pplxToolJSON })
	tools := pplxLocalTestTools(6000)

	probe := rig.post(t, acc, tools, "ping")
	probeReqs := rig.all()
	if probe.Code != http.StatusOK || len(probeReqs) != 1 {
		t.Fatalf("probe setup failed: code=%d calls=%d body=%s", probe.Code, len(probeReqs), probe.Body.String())
	}
	baseRunes := utf8.RuneCountInString(probeReqs[0].QueryStr)
	if !strings.Contains(probeReqs[0].QueryStr, "file_path") {
		t.Fatalf("setup: protocol does not carry the Read schema")
	}
	resetPerplexityThreadsForTest()

	// Make the final chunk ~36k runes so protocol + final chunk cannot fit in 38k.
	pad := perplexityDefaultMaxPromptRunes - 200 + 36000 - (baseRunes - 4)
	const marker = "FINAL-QUESTION-MARKER"
	rec := rig.post(t, acc, tools, pplxFillerLines(pad, marker))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	reqs := rig.all()[1:]
	if len(reqs) < 2 {
		t.Fatalf("expected a chunked turn, got %d requests", len(reqs))
	}
	for i, r := range reqs {
		if n := utf8.RuneCountInString(r.QueryStr); n > perplexityDefaultMaxPromptRunes {
			t.Fatalf("chunk %d has %d runes (> %d)", i+1, n, perplexityDefaultMaxPromptRunes)
		}
	}
	last := reqs[len(reqs)-1].QueryStr
	for _, want := range []string{"LOCAL ACTION PROTOCOL", "file_path", "[LOCAL CLI MIDDLEWARE REMINDER]", marker} {
		if !strings.Contains(last, want) {
			t.Fatalf("final chunk missing %q (protocol stranded in an earlier chunk)", want)
		}
	}
}

// A protocol that cannot fit with any conversation must fail loudly instead of
// silently dropping the tool schema.
func TestPerplexityLocalToolsOversizedProtocolFailsExplicitly(t *testing.T) {
	rig, acc := newPplxLocalToolsRig(t, func(int, perplexityAskRequest) string { return pplxToolJSON })
	// Tool descriptions are compacted by the injector, so size the protocol by tool count.
	tools := pplxLocalTestTools(0)
	for i := 0; i < 400; i++ {
		tools = append(tools, map[string]any{
			"name":        fmt.Sprintf("mcp__example__tool_%03d", i),
			"description": strings.Repeat("Example MCP tool. ", 6),
			"input_schema": map[string]any{
				"type":       "object",
				"required":   []string{"query"},
				"properties": map[string]any{"query": map[string]any{"type": "string"}, "limit": map[string]any{"type": "number"}},
			},
		})
	}
	rec := rig.post(t, acc, tools, "hello")
	if rec.Code == http.StatusOK {
		t.Fatalf("oversized tool protocol was silently downgraded to a successful reply: %s", rec.Body.String())
	}
	for i, r := range rig.all() {
		if n := utf8.RuneCountInString(r.QueryStr); n > perplexityDefaultMaxPromptRunes {
			t.Fatalf("upstream request %d has %d runes (> %d)", i+1, n, perplexityDefaultMaxPromptRunes)
		}
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "notion") {
		t.Fatalf("client-facing error exposes upstream branding: %s", rec.Body.String())
	}
}

// The correction retry must be split like any other prompt and keep the protocol.
func TestPerplexityLocalToolsRetryIsChunkedAndKeepsProtocol(t *testing.T) {
	finals := 0
	rig, acc := newPplxLocalToolsRig(t, func(_ int, req perplexityAskRequest) string {
		if strings.Contains(req.QueryStr, "[FINAL CONTEXT CHUNK") || !strings.Contains(req.QueryStr, "[CONTEXT CHUNK") {
			finals++
			if finals == 1 {
				return pplxDenialNoChannel
			}
			return pplxToolJSON
		}
		return "ACK"
	})
	const marker = "RETRY-QUESTION-MARKER"
	rec := rig.post(t, acc, pplxLocalTestTools(0), pplxFillerLines(60000, marker))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	reqs := rig.all()
	for i, r := range reqs {
		if n := utf8.RuneCountInString(r.QueryStr); n > perplexityDefaultMaxPromptRunes {
			t.Fatalf("upstream request %d has %d runes (> %d): retry bypassed chunking", i+1, n, perplexityDefaultMaxPromptRunes)
		}
	}
	last := reqs[len(reqs)-1].QueryStr
	for _, want := range []string{"LOCAL ACTION PROTOCOL", marker} {
		if !strings.Contains(last, want) {
			t.Fatalf("final retry chunk missing %q", want)
		}
	}
	if !strings.Contains(rec.Body.String(), "tool_use") {
		t.Fatalf("expected tool_use after retry: %s", rec.Body.String())
	}
}

func (r *pplxLocalToolsRig) postMessages(t *testing.T, acc *Account, tools []map[string]any, messages []map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{
		"model":      "perplexity/sonnet-5.5",
		"max_tokens": 512,
		"stream":     false,
		"messages":   messages,
	}
	if tools != nil {
		body["tools"] = tools
	}
	raw, _ := json.Marshal(body)
	pool := poolOf(acc)
	rec := httptest.NewRecorder()
	HandleAnthropicMessages(pool).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(raw))))
	waitQuotaIdle(pool)
	return rec
}

// After a denial the single correction must replay the whole conversation
// (early constraint, tool result, latest turn), not just the last user message,
// and must not hardcode tools beyond the declared ## Functions.
func TestPerplexityLocalToolsRetryReplaysHistory(t *testing.T) {
	rig, acc := newPplxLocalToolsRig(t, func(n int, _ perplexityAskRequest) string {
		if n == 1 {
			return pplxDenialNoChannel
		}
		return pplxToolJSON
	})
	messages := []map[string]any{
		{"role": "user", "content": "CONSTRAINT-ALPHA 只读，不要修改任何文件，先看看目录结构"},
		{"role": "assistant", "content": []map[string]any{
			{"type": "text", "text": "好的，我先列目录。"},
			{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls"}},
		}},
		{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "toolu_1", "content": "TOOLRESULT-BETA"},
		}},
		{"role": "assistant", "content": "目录已列出。"},
		{"role": "user", "content": "继续"},
	}
	rec := rig.postMessages(t, acc, pplxLocalTestTools(0), messages)
	reqs := rig.all()
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if len(reqs) != 2 {
		t.Fatalf("expected 1 upstream call + exactly 1 correction, got %d", len(reqs))
	}
	retry := reqs[1].QueryStr
	for _, want := range []string{"CONSTRAINT-ALPHA", "TOOLRESULT-BETA", "LOCAL TOOL RESULT", "继续", "LOCAL ACTION PROTOCOL", "[CORRECTION]", perplexityLocalMiddlewareReminder} {
		if !strings.Contains(retry, want) {
			t.Fatalf("retry prompt lost %q (history not replayed)", want)
		}
	}
	ci := strings.Index(retry, "[CORRECTION]")
	ri := strings.LastIndex(retry, perplexityLocalMiddlewareReminder)
	if ci > ri {
		t.Fatalf("correction must sit before the final reminder (corr=%d reminder=%d)", ci, ri)
	}
	if strings.Index(retry, "CONSTRAINT-ALPHA") > ci || strings.LastIndex(retry, "继续") > ci {
		t.Fatalf("history must precede the correction")
	}
	for _, hard := range []string{"Glob", "Grep", "Bash", "Read", "Write", "Edit"} {
		if strings.Contains(retry[ci:ri], hard) {
			t.Fatalf("correction hardcodes tool name %q", hard)
		}
	}
	if !strings.Contains(rec.Body.String(), "tool_use") {
		t.Fatalf("expected tool_use after retry: %s", rec.Body.String())
	}
}

func pplxStripChunk(t *testing.T, chunk, protocol string, last bool) string {
	t.Helper()
	if last {
		if !strings.HasPrefix(chunk, protocol+"\n\n") {
			t.Fatalf("final chunk does not start with the protocol")
		}
		chunk = strings.TrimPrefix(chunk, protocol+"\n\n")
	} else if strings.Contains(chunk, protocol) {
		t.Fatalf("protocol leaked into a non-final chunk")
	}
	i := strings.Index(chunk, "\n")
	if i < 0 {
		t.Fatalf("chunk has no header line")
	}
	return chunk[i+1:]
}

// Only the final chunk gives up room for the protocol: a ~34k protocol with a
// 120k body must not shrink every chunk, and no body text may be lost.
func TestPerplexityBuildChunksOnlyFinalChunkReservesProtocol(t *testing.T) {
	protocol := strings.Repeat("P", 34000)
	body := pplxFillerLines(120000, "TAIL-MARK") + "\n\n" + perplexityLocalMiddlewareReminder
	chunks, err := buildPerplexityChunks(protocol, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) > 5 {
		t.Fatalf("earlier chunks must be filled to the normal budget: got %d chunks", len(chunks))
	}
	if n := utf8.RuneCountInString(chunks[0]); n < perplexityDefaultMaxPromptRunes-300 {
		t.Fatalf("first chunk only %d runes: protocol room was deducted from a non-final chunk", n)
	}
	var rebuilt strings.Builder
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c); n > perplexityDefaultMaxPromptRunes {
			t.Fatalf("chunk %d has %d runes (> %d)", i+1, n, perplexityDefaultMaxPromptRunes)
		}
		rebuilt.WriteString(pplxStripChunk(t, c, protocol, i == len(chunks)-1))
	}
	if rebuilt.String() != body {
		t.Fatalf("body text was dropped or reordered: got %d runes want %d", utf8.RuneCountInString(rebuilt.String()), utf8.RuneCountInString(body))
	}
	if !strings.HasSuffix(chunks[len(chunks)-1], perplexityLocalMiddlewareReminder) {
		t.Fatalf("final chunk lost the middleware reminder")
	}
}

// Exact boundary: the protocol fits iff the final chunk can still hold the
// middleware reminder; one more protocol rune must fail explicitly.
func TestPerplexityBuildChunksProtocolBoundary(t *testing.T) {
	reminder := utf8.RuneCountInString(perplexityLocalMiddlewareReminder)
	fit := perplexityDefaultMaxPromptRunes - 2 - perplexityChunkHeaderReserve - reminder
	body := pplxFillerLines(60000, "X") + "\n\n" + perplexityLocalMiddlewareReminder

	protocol := strings.Repeat("P", fit)
	chunks, err := buildPerplexityChunks(protocol, body)
	if err != nil {
		t.Fatalf("protocol of %d runes should fit: %v", fit, err)
	}
	var rebuilt strings.Builder
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c); n > perplexityDefaultMaxPromptRunes {
			t.Fatalf("chunk %d has %d runes (> %d)", i+1, n, perplexityDefaultMaxPromptRunes)
		}
		rebuilt.WriteString(pplxStripChunk(t, c, protocol, i == len(chunks)-1))
	}
	if rebuilt.String() != body {
		t.Fatalf("body text was dropped or reordered at the boundary")
	}

	if _, err := buildPerplexityChunks(strings.Repeat("P", fit+1), body); err == nil || !errors.Is(err, ErrToolBridgeNoTool) {
		t.Fatalf("protocol of %d runes must fail explicitly, got err=%v", fit+1, err)
	}
}
