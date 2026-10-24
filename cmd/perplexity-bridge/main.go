package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"perplexity-bridge/internal/proxy"
)

func requiresAPIKey(path string) bool {
	return path == "/models" || strings.HasPrefix(path, "/v1/")
}

func apiKeyAuthMiddleware(apiKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiKey == "" || !requiresAPIKey(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		var key string
		if bearer := r.Header.Get("Authorization"); bearer != "" {
			key = strings.TrimPrefix(bearer, "Bearer ")
			if key == bearer {
				key = ""
			}
		}
		if key == "" {
			key = r.Header.Get("x-api-key")
		}
		if key == "" {
			http.Error(w, `{"error":{"message":"missing api key, use 'Authorization: Bearer <key>' or 'x-api-key: <key>'","type":"auth_error"}}`, http.StatusUnauthorized)
			return
		}
		if key != apiKey {
			http.Error(w, `{"error":{"message":"invalid api key","type":"auth_error"}}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func newMux(pool *proxy.AccountPool, accountsDir string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", proxy.TrackInFlight(proxy.HandleAnthropicMessages(pool)))
	mux.HandleFunc("/v1/messages/count_tokens", proxy.HandleAnthropicCountTokens())
	mux.HandleFunc("/v1/models", proxy.HandlePublicModels(pool))
	mux.HandleFunc("/models", proxy.HandlePublicModels(pool))
	mux.HandleFunc("/health", proxy.HandleHealth(pool))
	mux.HandleFunc("/admin/accounts", proxy.HandleAdminAccounts(pool))
	mux.HandleFunc("/admin/accounts/reload", proxy.HandleAdminAccountsReload(pool, accountsDir))
	mux.HandleFunc("/admin/accounts/refresh", proxy.HandleAdminAccountsRefresh(pool, accountsDir))
	return mux
}

const defaultShutdownTimeout = 90 * time.Second

func shutdownTimeoutFromEnv() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("SHUTDOWN_TIMEOUT_SECONDS")); raw != "" {
		if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return defaultShutdownTimeout
}

func serveHTTPWithGracefulShutdown(ln net.Listener, handler http.Handler, stopCh <-chan os.Signal, shutdownTimeout time.Duration) error {
	srv := &http.Server{Handler: handler}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case sig := <-stopCh:
		log.Printf("[shutdown] signal %v received; draining %d in-flight request(s) (timeout %s)",
			sig, proxy.InFlightRequests(), shutdownTimeout)
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		shutdownErr := srv.Shutdown(ctx)
		serveErr := <-errCh
		if shutdownErr != nil {
			return shutdownErr
		}
		return serveErr
	}
}

func main() {
	cfg, err := proxy.LoadConfig("config.yaml")
	if err != nil {
		log.Fatalf("[config] %v", err)
	}

	proxy.EnsureApiKey(cfg, "config.yaml")
	proxy.ApplyConfig(cfg)

	port := cfg.Server.Port
	if port == "" {
		port = "18082"
	}
	accountsDir := cfg.Server.AccountsDir
	if accountsDir == "" {
		accountsDir = "accounts"
	}

	pool := proxy.NewAccountPool()
	if _, err := os.Stat(accountsDir); err == nil {
		if err := pool.LoadFromDir(accountsDir); err != nil {
			log.Printf("[warn] %v", err)
		}
	}

	if pool.Count() == 0 {
		log.Printf("[warn] No accounts found. Place Perplexity account JSON files in %s/ to enable API.", accountsDir)
	} else {
		log.Printf("[startup] kicking off background quota refresh for %d account(s)", pool.Count())
		go pool.RefreshAll(accountsDir)
	}

	pool.StartRefreshLoop(cfg.RefreshInterval(), accountsDir)

	cors := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, X-Web-Search, X-Workspace-Search")
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	mux := newMux(pool, accountsDir)
	log.Printf("=== perplexity-bridge ===")
	log.Printf("Listening on %s:%s", cfg.Server.Host, port)
	log.Printf("Accounts: %d", pool.Count())
	log.Printf("Endpoints:")
	log.Printf("  POST /v1/messages                 (Anthropic Messages API)")
	log.Printf("  POST /v1/messages/count_tokens    (local token estimate)")
	log.Printf("  GET  /v1/models                   (models catalog)")
	log.Printf("  GET  /health                      (health & quota summary)")
	log.Printf("  GET  /admin/accounts              (account pool status)")

	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.Server.Host, port))
	if err != nil {
		log.Fatalf("Listen error: %v", err)
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	if err := serveHTTPWithGracefulShutdown(ln, cors(apiKeyAuthMiddleware(cfg.Server.ApiKey, mux)), sigCh, shutdownTimeoutFromEnv()); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
