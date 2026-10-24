package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	agentKeepaliveDelay    = 1500 * time.Millisecond
	agentKeepaliveInterval = 10 * time.Second
)

type agentFailure struct {
	Kind        string
	Class       string
	Pending     bool
	Status      int
	ErrType     string
	Message     string
	ShouldRetry bool
}

func quotaExhaustedFailure() agentFailure {
	return agentFailure{
		Kind:        "quota_exhausted",
		Status:      http.StatusPaymentRequired,
		ErrType:     "billing_error",
		Message:     "[bridge:quota_exhausted] 所有可用上游账号额度已耗尽，请补充可用账号或等待额度刷新。",
		ShouldRetry: false,
	}
}

func classifyAgentFailure(err error) agentFailure {
	var malformed *toolBridgeMalformedError
	var merr *agentModelError
	switch {
	case errors.Is(err, ErrQuotaExhausted):
		return quotaExhaustedFailure()
	case errors.As(err, &malformed):
		return agentFailure{
			Kind:    "malformed_tool_call",
			Status:  http.StatusBadGateway,
			ErrType: "api_error",
			Message: fmt.Sprintf("[bridge:malformed_tool_call] 上游回复疑似本地工具调用请求，但未通过格式校验 (reason=%s)。本次未执行任何本地工具、未重发请求，也未将该文本作为普通回答返回。", malformed.Reason),
		}
	case errors.As(err, &merr):
		return agentFailure{
			Kind:    "model_unavailable",
			Status:  http.StatusBadRequest,
			ErrType: "invalid_request_error",
			Message: fmt.Sprintf("[bridge:model_unavailable] 模型 %q 不在受支持的上游代理模型列表中（请使用 auto、opus-5.5、sonnet-5.5 或 gpt-6.1-sol），本次请求未发送至上游。", merr.model),
		}
	case errors.Is(err, ErrPremiumFeatureUnavailable):
		return agentFailure{
			Kind:    "model_unavailable",
			Status:  http.StatusBadRequest,
			ErrType: "invalid_request_error",
			Message: "[bridge:model_unavailable] 当前可用账号套餐不支持所请求的模型（需 Max 套餐支持 Opus 5.5，或上游触发了非预期降级），已拒绝静默降级返回。",
		}
	case errors.Is(err, ErrEmptyResponse):
		return agentFailure{
			Kind:    "failed",
			Status:  http.StatusBadGateway,
			ErrType: "api_error",
			Message: "[bridge:failed] 上游代理本轮结束但未返回任何回复内容。远端轮次已结束，直接重试相同请求不会产生回复。",
		}
	default:
		return agentFailure{
			Kind:    "internal",
			Status:  http.StatusBadGateway,
			ErrType: "api_error",
			Message: "上游接口错误 (upstream API error): " + sanitizeClientErrorText(err),
		}
	}
}

var (
	clientErrURLPrefixPattern = regexp.MustCompile(`(?i)\b(?:get|post|put|delete|head|patch|options)\s+"https?://[^"]+"\s*:\s*`)
	clientErrRawURLPattern    = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)
	clientErrBrandPattern     = regexp.MustCompile(`(?i)\b(?:notion(?:\.(?:so|com))?|perplexity(?:\.ai)?|pplx)\b`)
)

func sanitizeClientErrorText(err error) string {
	if err == nil {
		return "unknown error"
	}
	msg := clientErrURLPrefixPattern.ReplaceAllString(err.Error(), "")
	msg = clientErrRawURLPattern.ReplaceAllString(msg, "[upstream-url]")
	msg = clientErrBrandPattern.ReplaceAllString(msg, "upstream")
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "unknown error"
	}
	return msg
}

func writeAnthropicError(w http.ResponseWriter, requestID string, status int, message string, errType string) {
	if errType == "" {
		errType = "api_error"
	}
	setAnthropicRequestIDHeaders(w, requestID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp := map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	}
	LogAPIOutputJSON(requestID, fmt.Sprintf("anthropic error %d", status), resp)
	_ = json.NewEncoder(w).Encode(resp)
}

func writeAgentFailureJSON(w http.ResponseWriter, requestID string, f agentFailure) {
	if f.Kind != "internal" {
		w.Header().Set("x-should-retry", fmt.Sprintf("%t", f.ShouldRetry))
		if f.ShouldRetry {
			w.Header().Set("retry-after", "5")
		}
	}
	writeAnthropicError(w, requestID, f.Status, f.Message, f.ErrType)
}

func agentFailureFor(err error) agentFailure {
	if errors.Is(err, ErrQuotaExhausted) {
		return quotaExhaustedFailure()
	}
	if errors.Is(err, ErrPremiumFeatureUnavailable) || (AppConfig != nil && AppConfig.Proxy.AgentThread) {
		return classifyAgentFailure(err)
	}
	return agentFailure{
		Kind:    "internal",
		Status:  http.StatusBadGateway,
		ErrType: "api_error",
		Message: "upstream API error: " + sanitizeClientErrorText(err),
	}
}

func writeAnthropicCallError(w http.ResponseWriter, requestID string, err error) {
	writeAgentFailureJSON(w, requestID, agentFailureFor(err))
}

type keepaliveResponseWriter struct {
	http.ResponseWriter
	ka **anthropicKeepalive
}

func (kw *keepaliveResponseWriter) Flush() {
	if f, ok := kw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (kw *keepaliveResponseWriter) Unwrap() http.ResponseWriter {
	return kw.ResponseWriter
}

func canRetryKeepalive(w http.ResponseWriter) bool {
	_, ok := w.(*keepaliveResponseWriter)
	return ok
}

type anthropicKeepalive struct {
	w                    http.ResponseWriter
	flusher              http.Flusher
	requestID            string
	model                string
	mu                   sync.Mutex
	inputTokens          int
	cacheReadInputTokens int
	bridgeTrace          *BridgeTrace
	started              bool
	stopped              bool
	gone                 chan struct{}
	stopCh               chan struct{}
	done                 chan struct{}
	goneOnce             sync.Once
}

func newAnthropicKeepalive(w http.ResponseWriter, requestID, model string) *anthropicKeepalive {
	var slot **anthropicKeepalive
	rawW := w
	if kw, ok := w.(*keepaliveResponseWriter); ok {
		slot = kw.ka
		rawW = kw.ResponseWriter
	}
	if slot != nil && *slot != nil {
		k := *slot
		k.restart()
		return k
	}
	k := &anthropicKeepalive{
		w:         rawW,
		requestID: requestID,
		model:     model,
		gone:      make(chan struct{}),
		stopCh:    make(chan struct{}),
		done:      make(chan struct{}),
	}
	k.flusher, _ = rawW.(http.Flusher)
	if slot != nil {
		*slot = k
	}
	if k.flusher != nil && AppConfig != nil && AppConfig.Proxy.AgentThread && agentKeepaliveDelay > 0 {
		go k.run(k.stopCh, k.done, false)
	} else {
		close(k.done)
	}
	return k
}

func (k *anthropicKeepalive) restart() {
	k.Stop()
	k.mu.Lock()
	k.stopped = false
	k.stopCh = make(chan struct{})
	k.done = make(chan struct{})
	stopCh := k.stopCh
	done := k.done
	alreadyStarted := k.started
	k.mu.Unlock()
	select {
	case <-k.gone:
		close(done)
		return
	default:
	}
	if k.flusher != nil && AppConfig != nil && AppConfig.Proxy.AgentThread && agentKeepaliveDelay > 0 {
		go k.run(stopCh, done, alreadyStarted)
	} else {
		close(done)
	}
}

func (k *anthropicKeepalive) run(stopCh <-chan struct{}, done chan<- struct{}, alreadyStarted bool) {
	defer close(done)
	if !alreadyStarted {
		timer := time.NewTimer(agentKeepaliveDelay)
		defer timer.Stop()
		select {
		case <-stopCh:
			return
		case <-timer.C:
		}
		if !k.begin(0) {
			return
		}
	}
	interval := agentKeepaliveInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			k.mu.Lock()
			err := k.writeLocked("ping", `{"type":"ping"}`)
			k.mu.Unlock()
			if err != nil {
				k.markGone()
				return
			}
		}
	}
}

func (k *anthropicKeepalive) SetInputTokens(n int) {
	k.SetInputUsage(n, 0)
}

func (k *anthropicKeepalive) SetInputUsage(inputTokens, cacheReadInputTokens int) {
	k.mu.Lock()
	k.inputTokens = inputTokens
	k.cacheReadInputTokens = cacheReadInputTokens
	k.mu.Unlock()
}

func (k *anthropicKeepalive) SetBridgeTrace(bt *BridgeTrace) {
	k.mu.Lock()
	k.bridgeTrace = bt
	k.mu.Unlock()
}

func (k *anthropicKeepalive) markGone() {
	k.goneOnce.Do(func() { close(k.gone) })
}

func (k *anthropicKeepalive) writeLocked(event, data string) error {
	if _, err := fmt.Fprintf(k.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	k.flusher.Flush()
	return nil
}

func (k *anthropicKeepalive) Gone() <-chan struct{} { return k.gone }

func (k *anthropicKeepalive) Started() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.started
}

func (k *anthropicKeepalive) Stop() {
	k.mu.Lock()
	if !k.stopped {
		k.stopped = true
		close(k.stopCh)
	}
	done := k.done
	k.mu.Unlock()
	<-done
}

func (k *anthropicKeepalive) begin(inputTokens int) bool {
	return k.beginUsage(inputTokens, 0)
}

func (k *anthropicKeepalive) beginUsage(inputTokens, cacheReadInputTokens int) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.started {
		return true
	}
	if k.flusher == nil {
		return false
	}
	k.started = true
	if inputTokens <= 0 {
		inputTokens = k.inputTokens
	}
	if cacheReadInputTokens <= 0 {
		cacheReadInputTokens = k.cacheReadInputTokens
	}
	setAnthropicRequestIDHeaders(k.w, k.requestID)
	h := k.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	k.w.WriteHeader(http.StatusOK)
	usageMap := map[string]interface{}{"input_tokens": inputTokens, "output_tokens": 0}
	if cacheReadInputTokens > 0 {
		usageMap["cache_read_input_tokens"] = cacheReadInputTokens
	}
	sendAnthropicSSE(k.w, k.flusher, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": k.requestID, "type": "message", "role": "assistant", "content": []interface{}{},
			"model": k.model, "stop_reason": nil, "stop_sequence": nil,
			"usage": usageMap,
		},
	})
	if err := k.writeLocked("ping", `{"type":"ping"}`); err != nil {
		k.markGone()
	}
	return true
}

func (k *anthropicKeepalive) fail(err error) {
	f := agentFailureFor(err)
	k.mu.Lock()
	bt := k.bridgeTrace
	k.mu.Unlock()
	if k.Started() {
		payload := map[string]interface{}{"type": "error", "error": map[string]interface{}{"type": f.ErrType, "message": f.Message}}
		if bt != nil && (bt.ThreadID != "" || bt.TotalMs > 0) {
			payload["bridge_trace"] = bt
		}
		LogAPIOutputJSON(k.requestID, "anthropic stream error event", payload)
		k.mu.Lock()
		raw, _ := json.Marshal(payload)
		_ = k.writeLocked("error", string(raw))
		k.mu.Unlock()
		return
	}
	setAnthropicBridgeTraceHeader(k.w, bt)
	writeAgentFailureJSON(k.w, k.requestID, f)
}

func (k *anthropicKeepalive) WatchClient(gone <-chan struct{}) func() {
	if gone == nil {
		return func() {}
	}
	stopWatch, finished := make(chan struct{}), make(chan struct{})
	var stopOnce sync.Once
	go func() {
		defer close(finished)
		select {
		case <-gone:
			k.markGone()
		case <-stopWatch:
		}
	}()
	return func() {
		stopOnce.Do(func() { close(stopWatch) })
		<-finished
	}
}

func agentRequestGone(channels []<-chan struct{}) <-chan struct{} {
	if len(channels) > 0 {
		return channels[0]
	}
	return nil
}
