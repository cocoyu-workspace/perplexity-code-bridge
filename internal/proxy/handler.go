package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
)

var inFlightRequests atomic.Int64

func InFlightRequests() int64 {
	return inFlightRequests.Load()
}

func TrackInFlight(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inFlightRequests.Add(1)
		defer inFlightRequests.Add(-1)
		next(w, r)
	}
}

func CallInference(acc *Account, messages []ChatMessage, model string, disableBuiltinTools bool, cb StreamCallback, opt CallOptions) error {
	return callPerplexityInference(acc, messages, model, disableBuiltinTools, cb, opt)
}

func HandleHealth(pool *AccountPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		total := 0
		available := 0
		var summary AccountsSummary
		if pool != nil {
			total = pool.Count()
			available = pool.AvailableCount()
			summary = pool.GetQuotaSummary()
		}
		resp := map[string]interface{}{
			"status":    "ok",
			"provider":  "perplexity",
			"accounts":  total,
			"available": available,
			"in_flight": InFlightRequests(),
			"quota":     summary,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

const publicModelCreatedAt = int64(1735689600)

type publicModel struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Created     int64  `json:"created"`
	OwnedBy     string `json:"owned_by"`
	DisplayName string `json:"display_name,omitempty"`
}

func HandlePublicModels(pool *AccountPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, `{"error":{"message":"method not allowed","type":"invalid_request_error"}}`, http.StatusMethodNotAllowed)
			return
		}
		models := []publicModel{
			{ID: "auto", Object: "model", Created: publicModelCreatedAt, OwnedBy: "upstream", DisplayName: "Auto (Sonnet 5.5)"},
			{ID: "sonnet-5.5", Object: "model", Created: publicModelCreatedAt, OwnedBy: "upstream", DisplayName: "Claude Sonnet 5.5"},
			{ID: "claude-sonnet-5-5", Object: "model", Created: publicModelCreatedAt, OwnedBy: "upstream", DisplayName: "Claude Sonnet 5.5"},
			{ID: "opus-5.5", Object: "model", Created: publicModelCreatedAt, OwnedBy: "upstream", DisplayName: "Claude Opus 5.5 (Max)"},
			{ID: "claude-opus-5-5", Object: "model", Created: publicModelCreatedAt, OwnedBy: "upstream", DisplayName: "Claude Opus 5.5 (Max)"},
			{ID: "gpt-6.1-sol", Object: "model", Created: publicModelCreatedAt, OwnedBy: "upstream", DisplayName: "GPT-6.1 Sol"},
			{ID: "claude-sol-6-1", Object: "model", Created: publicModelCreatedAt, OwnedBy: "upstream", DisplayName: "GPT-6.1 Sol"},
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data":   models,
		})
	}
}

func HandleAdminAccounts(pool *AccountPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var details []AccountDetail
		total := 0
		available := 0
		if pool != nil {
			details = pool.GetAccountDetails()
			total = pool.Count()
			available = pool.AvailableCount()
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"total":     total,
			"available": available,
			"summary":   summarizeAccounts(details),
			"accounts":  details,
		})
	}
}

func HandleAdminAccountsReload(pool *AccountPool, accountsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, `{"error":{"message":"method not allowed","type":"invalid_request_error"}}`, http.StatusMethodNotAllowed)
			return
		}
		if pool != nil {
			if err := pool.LoadFromDir(accountsDir); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"status": "error",
					"error":  err.Error(),
				})
				return
			}
		}
		HandleAdminAccounts(pool)(w, r)
	}
}

func HandleAdminAccountsRefresh(pool *AccountPool, accountsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, `{"error":{"message":"method not allowed","type":"invalid_request_error"}}`, http.StatusMethodNotAllowed)
			return
		}
		if pool != nil {
			pool.RefreshAll(accountsDir)
		}
		HandleAdminAccounts(pool)(w, r)
	}
}

func AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		expected := ""
		if AppConfig != nil {
			expected = strings.TrimSpace(AppConfig.Server.ApiKey)
		}
		if expected == "" {
			next(w, r)
			return
		}
		token := extractBearerOrAPIKey(r)
		if token != expected {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"type": "error",
				"error": map[string]string{
					"type":    "authentication_error",
					"message": "invalid api key",
				},
			})
			return
		}
		next(w, r)
	}
}

func extractBearerOrAPIKey(r *http.Request) string {
	if r == nil {
		return ""
	}
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); auth != "" {
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			return strings.TrimSpace(auth[7:])
		}
		return auth
	}
	if key := strings.TrimSpace(r.Header.Get("x-api-key")); key != "" {
		return key
	}
	return ""
}
