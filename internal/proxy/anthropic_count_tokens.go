package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// HandleAnthropicCountTokens provides the client-side context-size probe without
// creating a Notion thread or consuming an account's inference allowance. Notion
// exposes no tokenizer endpoint; use the same estimate as response usage so the
// two client compaction signals agree. This is not an upstream token reading.
func HandleAnthropicCountTokens() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := "msg_" + generateUUIDv4()
		w.Header().Set("request-id", requestID)
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAnthropicError(w, requestID, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
			return
		}

		// This bounds the HTTP body, independently of the smaller Notion prompt
		// budgets. Count requests never upload their content to Notion.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeAnthropicError(w, requestID, http.StatusRequestEntityTooLarge, "request body exceeds 32 MiB", "request_too_large")
			} else {
				writeAnthropicError(w, requestID, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
			}
			return
		}
		var req AnthropicRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeAnthropicError(w, requestID, http.StatusBadRequest, "invalid JSON request body", "invalid_request_error")
			return
		}
		if strings.TrimSpace(req.Model) == "" || len(req.Messages) == 0 {
			writeAnthropicError(w, requestID, http.StatusBadRequest, "model and messages are required", "invalid_request_error")
			return
		}
		for _, m := range req.Messages {
			if m.Role != "user" && m.Role != "assistant" {
				writeAnthropicError(w, requestID, http.StatusBadRequest, "message role must be user or assistant", "invalid_request_error")
				return
			}
			switch m.Content.(type) {
			case string, []interface{}:
			default:
				writeAnthropicError(w, requestID, http.StatusBadRequest, "message content must be text or content blocks", "invalid_request_error")
				return
			}
		}

		_, projection, err := projectAnthropicContext(&req)
		if err != nil {
			writeAnthropicError(w, requestID, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		var contextManagement *contextCountResponse
		if projection.Requested {
			contextManagement = &contextCountResponse{OriginalInputTokens: projection.OriginalTokens}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Bridge-Token-Count-Source", "estimated")
		json.NewEncoder(w).Encode(struct {
			InputTokens       int                   `json:"input_tokens"`
			ContextManagement *contextCountResponse `json:"context_management,omitempty"`
		}{projection.InputTokens, contextManagement})
	}
}
