package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"perplexity-bridge/internal/netutil"
)

type Config struct {
	Server   ServerConfig      `yaml:"server"`
	Proxy    ProxyConfig       `yaml:"proxy"`
	Timeouts TimeoutConfig     `yaml:"timeouts"`
	Refresh  RefreshConfig     `yaml:"refresh"`
	Browser  BrowserConfig     `yaml:"browser"`
	ModelMap map[string]string `yaml:"model_map"`
}

type ServerConfig struct {
	Host          string `yaml:"host"`
	Port          string `yaml:"port"`
	AccountsDir   string `yaml:"accounts_dir"`
	TokenFile     string `yaml:"token_file"`
	ApiKey        string `yaml:"api_key"`
	LogFile       string `yaml:"log_file"`
	DebugLogging  bool   `yaml:"debug_logging"`
	APILogInput   bool   `yaml:"api_log_input"`
	APILogOutput  bool   `yaml:"api_log_output"`
	NotionLogReq  bool   `yaml:"notion_log_request"`
	NotionLogResp bool   `yaml:"notion_log_response"`
	DumpAPIInput  bool   `yaml:"dump_api_input"`
}

type ProxyConfig struct {
	AgentThread           bool   `yaml:"agent_thread"`
	AgentLiveTranscript   bool   `yaml:"agent_live_transcript"`
	AgentThreadResume     *bool  `yaml:"agent_thread_resume"`
	AgentApprovalMode     string `yaml:"agent_approval_mode"`
	ClientVersion         string `yaml:"client_version"`
	DefaultProvider       string `yaml:"default_provider"`
	DefaultModel          string `yaml:"default_model"`
	DisableNotionPrompt   bool   `yaml:"disable_notion_prompt"`
	EnableWebSearch       *bool  `yaml:"enable_web_search"`
	EnableWorkspaceSearch *bool  `yaml:"enable_workspace_search"`
	AskModeDefault        *bool  `yaml:"ask_mode_default"`
	UpstreamProxy         string `yaml:"upstream_proxy"`
	NotionProxy           string `yaml:"notion_proxy"`
}

type TimeoutConfig struct {
	InferenceTimeout int `yaml:"inference_timeout"`
	ResearchTimeout  int `yaml:"research_timeout"`
	APITimeout       int `yaml:"api_timeout"`
	TLSDialTimeout   int `yaml:"tls_dial_timeout"`
}

type RefreshConfig struct {
	IntervalMinutes     int `yaml:"interval_minutes"`
	QuotaRecheckMinutes int `yaml:"quota_recheck_minutes"`
	Concurrency         int `yaml:"concurrency"`
	LiveCheckSeconds    int `yaml:"live_check_seconds"`
}

type BrowserConfig struct {
	UserAgent       string `yaml:"user_agent"`
	SecChUA         string `yaml:"sec_ch_ua"`
	SecChUAPlatform string `yaml:"sec_ch_ua_platform"`
}

var AppConfig = DefaultConfig()

func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host:          "127.0.0.1",
			Port:          "18082",
			AccountsDir:   "accounts",
			TokenFile:     "token.txt",
			ApiKey:        "",
			LogFile:       "",
			DebugLogging:  true,
			APILogInput:   false,
			APILogOutput:  false,
			NotionLogReq:  false,
			NotionLogResp: false,
		},
		Proxy: ProxyConfig{
			AgentThread:           true,
			AgentApprovalMode:     "yolo",
			ClientVersion:         DefaultClientVersion,
			DefaultProvider:       "perplexity",
			DefaultModel:          "sonnet-5.5",
			EnableWebSearch:       boolPtr(false),
			EnableWorkspaceSearch: boolPtr(false),
			AskModeDefault:        boolPtr(false),
		},
		Timeouts: TimeoutConfig{
			InferenceTimeout: 600,
			ResearchTimeout:  360,
			APITimeout:       30,
			TLSDialTimeout:   30,
		},
		Refresh: RefreshConfig{
			IntervalMinutes:     30,
			QuotaRecheckMinutes: 5,
			Concurrency:         5,
			LiveCheckSeconds:    5,
		},
		Browser: BrowserConfig{
			UserAgent:       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36",
			SecChUA:         `"Chromium";v="134", "Not:A-Brand";v="24", "Google Chrome";v="134"`,
			SecChUAPlatform: `"macOS"`,
		},
		ModelMap: map[string]string{
			"sonnet-5.5":        "claude55sonnet",
			"claude-sonnet-5-5": "claude55sonnet",
			"opus-5.5":          "claude55opus",
			"claude-opus-5-5":   "claude55opus",
			"gpt-6.1-sol":       "gpt6_1_sol",
			"claude-sol-6-1":    "gpt6_1_sol",
		},
	}
}

func LoadConfig(configPath string) (*Config, error) {
	cfg := DefaultConfig()
	loadedFromFile := false
	missingConfig := false

	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err == nil {
			loadedFromFile = true
			var raw struct {
				Proxy struct {
					AgentThread *bool `yaml:"agent_thread"`
				} `yaml:"proxy"`
			}
			_ = yaml.Unmarshal(data, &raw)
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse config.yaml: %w", err)
			}
			if raw.Proxy.AgentThread == nil {
				cfg.Proxy.AgentThread = true
			}
			if cfg.ModelMap == nil {
				cfg.ModelMap = DefaultConfig().ModelMap
			}
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read config.yaml: %w", err)
		} else {
			missingConfig = true
		}
	}

	if v := os.Getenv("HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := os.Getenv("PORT"); v != "" {
		cfg.Server.Port = v
	}
	if v := os.Getenv("ACCOUNTS_DIR"); v != "" {
		cfg.Server.AccountsDir = v
	}
	if v := os.Getenv("TOKEN_FILE"); v != "" {
		cfg.Server.TokenFile = v
	}
	if v := os.Getenv("API_KEY"); v != "" {
		cfg.Server.ApiKey = v
	}
	if v := os.Getenv("LOG_FILE"); v != "" {
		cfg.Server.LogFile = v
	}
	if v := os.Getenv("DEBUG_LOGGING"); v != "" {
		cfg.Server.DebugLogging = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("API_LOG_INPUT"); v != "" {
		cfg.Server.APILogInput = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("API_LOG_OUTPUT"); v != "" {
		cfg.Server.APILogOutput = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("NOTION_LOG_REQUEST"); v != "" {
		cfg.Server.NotionLogReq = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("NOTION_LOG_RESPONSE"); v != "" {
		cfg.Server.NotionLogResp = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("AGENT_THREAD"); v != "" {
		cfg.Proxy.AgentThread = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("DEFAULT_PROVIDER"); v != "" {
		cfg.Proxy.DefaultProvider = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv("DEFAULT_MODEL"); v != "" {
		cfg.Proxy.DefaultModel = v
	}
	if v := os.Getenv("INFERENCE_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Timeouts.InferenceTimeout = n
		}
	}
	if v := os.Getenv("API_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Timeouts.APITimeout = n
		}
	}
	if v := os.Getenv("TLS_DIAL_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Timeouts.TLSDialTimeout = n
		}
	}
	if v := os.Getenv("REFRESH_INTERVAL_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Refresh.IntervalMinutes = n
		}
	}
	if v := os.Getenv("QUOTA_RECHECK_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Refresh.QuotaRecheckMinutes = n
		}
	}
	if v := os.Getenv("REFRESH_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Refresh.Concurrency = n
		}
	}
	if v := os.Getenv("QUOTA_LIVE_CHECK_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Refresh.LiveCheckSeconds = n
		}
	}
	if v := os.Getenv("ENABLE_WEB_SEARCH"); v != "" {
		b := strings.EqualFold(v, "true") || v == "1"
		cfg.Proxy.EnableWebSearch = &b
	}
	if v := os.Getenv("UPSTREAM_PROXY"); v != "" {
		cfg.Proxy.UpstreamProxy = strings.TrimSpace(v)
	} else if v := os.Getenv("PERPLEXITY_PROXY"); v != "" {
		cfg.Proxy.UpstreamProxy = strings.TrimSpace(v)
	} else if v := os.Getenv("NOTION_PROXY"); v != "" {
		cfg.Proxy.NotionProxy = strings.TrimSpace(v)
	}

	proxyURL := cfg.UpstreamProxyURL()
	if proxyURL != "" {
		if err := netutil.ValidateProxyURL(proxyURL); err != nil {
			log.Printf("[config] invalid upstream proxy %q: %v (falling back to direct dial)", proxyURL, err)
			cfg.Proxy.UpstreamProxy = ""
			cfg.Proxy.NotionProxy = ""
		}
	}

	if err := ConfigureLogOutput(cfg.Server.LogFile); err != nil {
		return nil, fmt.Errorf("configure log output %q: %w", cfg.Server.LogFile, err)
	}

	AppConfig = cfg
	SetDebugLoggingEnabled(cfg.Server.DebugLogging)
	SetAPILogInputEnabled(cfg.Server.APILogInput)
	SetAPILogOutputEnabled(cfg.Server.APILogOutput)
	SetNotionRequestLoggingEnabled(cfg.Server.NotionLogReq)
	SetNotionResponseLoggingEnabled(cfg.Server.NotionLogResp)
	if loadedFromFile {
		log.Printf("[config] loaded from %s", configPath)
	} else if missingConfig {
		log.Printf("[config] %s not found, using defaults", configPath)
	}
	return cfg, nil
}

func GenerateApiKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "sk-" + hex.EncodeToString(b)
}

func EnsureApiKey(cfg *Config, configPath string) {
	if cfg.Server.ApiKey != "" {
		return
	}
	cfg.Server.ApiKey = GenerateApiKey()
	log.Printf("[config] no api_key configured, generated new key")
	if configPath == "" {
		return
	}
	out, err := yaml.Marshal(cfg)
	if err == nil {
		_ = os.WriteFile(configPath, out, 0o600)
	}
}

func (c *Config) InferenceTimeoutDuration() time.Duration {
	if c == nil || c.Timeouts.InferenceTimeout <= 0 {
		return 600 * time.Second
	}
	return time.Duration(c.Timeouts.InferenceTimeout) * time.Second
}

func (c *Config) ResearchTimeoutDuration() time.Duration {
	if c == nil || c.Timeouts.ResearchTimeout <= 0 {
		return 360 * time.Second
	}
	return time.Duration(c.Timeouts.ResearchTimeout) * time.Second
}

func (c *Config) APITimeoutDuration() time.Duration {
	if c == nil || c.Timeouts.APITimeout <= 0 {
		return 30 * time.Second
	}
	return time.Duration(c.Timeouts.APITimeout) * time.Second
}

func (c *Config) TLSDialTimeoutDuration() time.Duration {
	if c == nil || c.Timeouts.TLSDialTimeout <= 0 {
		return 30 * time.Second
	}
	return time.Duration(c.Timeouts.TLSDialTimeout) * time.Second
}

func (c *Config) RefreshInterval() time.Duration {
	if c == nil || c.Refresh.IntervalMinutes <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(c.Refresh.IntervalMinutes) * time.Minute
}

func (c *Config) QuotaRecheckInterval() time.Duration {
	if c == nil || c.Refresh.QuotaRecheckMinutes <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(c.Refresh.QuotaRecheckMinutes) * time.Minute
}

func (c *Config) QuotaLiveCheckInterval() time.Duration {
	if c == nil {
		return 5 * time.Second
	}
	if c.Refresh.LiveCheckSeconds < 0 {
		return 0
	}
	return time.Duration(c.Refresh.LiveCheckSeconds) * time.Second
}

func (c *Config) AgentThreadResumeEnabled() bool {
	if c == nil || c.Proxy.AgentThreadResume == nil {
		return true
	}
	return *c.Proxy.AgentThreadResume
}

func (c *Config) WebSearchEnabled() bool {
	if c == nil || c.Proxy.EnableWebSearch == nil {
		return false
	}
	return *c.Proxy.EnableWebSearch
}

func (c *Config) WorkspaceSearchEnabled() bool {
	if c == nil || c.Proxy.EnableWorkspaceSearch == nil {
		return false
	}
	return *c.Proxy.EnableWorkspaceSearch
}

func (c *Config) AskModeDefault() bool {
	if c == nil || c.Proxy.AskModeDefault == nil {
		return false
	}
	return *c.Proxy.AskModeDefault
}

func (c *Config) UpstreamProxyURL() string {
	if c == nil {
		return ""
	}
	if strings.TrimSpace(c.Proxy.UpstreamProxy) != "" {
		return strings.TrimSpace(c.Proxy.UpstreamProxy)
	}
	return strings.TrimSpace(c.Proxy.NotionProxy)
}

func (c *Config) NotionProxyURL() string {
	return c.UpstreamProxyURL()
}

func boolPtr(b bool) *bool { return &b }
