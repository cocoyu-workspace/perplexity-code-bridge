package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var quotaFetcher = CheckQuota

func CheckQuota(acc *Account) (*QuotaInfo, error) {
	if acc == nil {
		return nil, fmt.Errorf("nil account")
	}
	if acc.IsPerplexity() {
		return checkPerplexityQuota(acc)
	}
	if q := acc.quotaInfoSnapshot(); q != nil {
		return q, nil
	}
	return &QuotaInfo{IsEligible: true, SpaceLimit: 100, UserLimit: 100}, nil
}

type AccountPool struct {
	mu                 sync.RWMutex
	accounts           []*Account
	index              atomic.Uint64
	dir                string
	preferredAccountID string
	liveQuotaMu        sync.Mutex
	liveQuotaInflight  map[*Account]bool
	liveQuotaWg        sync.WaitGroup
	stopCh             chan struct{}
	stopOnce           sync.Once
}

func NewAccountPool() *AccountPool {
	return &AccountPool{
		liveQuotaInflight: make(map[*Account]bool),
		stopCh:            make(chan struct{}),
	}
}

func cloneModelEntries(src []ModelEntry) []ModelEntry {
	if len(src) == 0 {
		return nil
	}
	dst := make([]ModelEntry, len(src))
	copy(dst, src)
	return dst
}

func cloneQuotaInfo(src *QuotaInfo) *QuotaInfo {
	if src == nil {
		return nil
	}
	dst := *src
	return &dst
}

func cloneTimePtr(src *time.Time) *time.Time {
	if src == nil {
		return nil
	}
	dst := *src
	return &dst
}

func (acc *Account) modelsSnapshot() []ModelEntry {
	if acc == nil {
		return nil
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	return cloneModelEntries(acc.Models)
}

func (acc *Account) quotaInfoSnapshot() *QuotaInfo {
	if acc == nil {
		return nil
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	return cloneQuotaInfo(acc.QuotaInfo)
}

func (acc *Account) setModels(models []ModelEntry) {
	if acc == nil {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	acc.Models = cloneModelEntries(models)
}

func (acc *Account) setQuotaInfo(info *QuotaInfo, checkedAt *time.Time) {
	if acc == nil {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	acc.QuotaInfo = cloneQuotaInfo(info)
	acc.QuotaCheckedAt = cloneTimePtr(checkedAt)
}

func (acc *Account) markQuotaExhausted(now time.Time, permanent bool) bool {
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if acc.QuotaExhaustedAt != nil {
		if permanent {
			acc.PermanentlyExhausted = true
		}
		return false
	}
	ts := now
	acc.QuotaExhaustedAt = &ts
	acc.PermanentlyExhausted = permanent
	return true
}

func (acc *Account) clearQuotaExhausted() {
	if acc == nil {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	acc.QuotaExhaustedAt = nil
	acc.PermanentlyExhausted = false
}

func (acc *Account) isRemoved() bool {
	if acc == nil {
		return true
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	return acc.removed
}

func (acc *Account) setRemoved(v bool) {
	if acc == nil {
		return
	}
	acc.mu.Lock()
	acc.removed = v
	acc.mu.Unlock()
}

func (p *AccountPool) LoadFromDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		dir = "accounts"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	type diskFile struct {
		path            string
		defaultProvider string
	}
	var files []diskFile

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			if strings.EqualFold(name, "perplexity") {
				subDir := filepath.Join(dir, name)
				subEntries, subErr := os.ReadDir(subDir)
				if subErr != nil {
					return subErr
				}
				for _, sub := range subEntries {
					subName := sub.Name()
					if sub.IsDir() || strings.HasPrefix(subName, ".") || !strings.HasSuffix(strings.ToLower(subName), ".json") {
						continue
					}
					if strings.EqualFold(subName, "account.example.json") || strings.HasSuffix(strings.ToLower(subName), ".example.json") {
						continue
					}
					files = append(files, diskFile{
						path:            filepath.Join(subDir, subName),
						defaultProvider: "perplexity",
					})
				}
			}
			continue
		}
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		if strings.EqualFold(name, "account.example.json") || strings.HasSuffix(strings.ToLower(name), ".example.json") {
			continue
		}
		files = append(files, diskFile{path: filepath.Join(dir, name)})
	}

	var loaded []*Account
	seen := make(map[string]bool)

	for _, df := range files {
		data, err := os.ReadFile(df.path)
		if err != nil {
			log.Printf("[pool] read %s failed: %v", df.path, err)
			continue
		}
		var acc Account
		if err := json.Unmarshal(data, &acc); err != nil {
			log.Printf("[pool] parse %s failed: %v", df.path, err)
			continue
		}
		var aux struct {
			QuotaInfo *QuotaInfo `json:"quota_info"`
		}
		_ = json.Unmarshal(data, &aux)
		if aux.QuotaInfo != nil {
			now := time.Now()
			acc.QuotaInfo = aux.QuotaInfo
			acc.QuotaCheckedAt = &now
		}
		if strings.TrimSpace(acc.TokenV2) == "" && strings.TrimSpace(acc.SessionToken) != "" {
			acc.TokenV2 = strings.TrimSpace(acc.SessionToken)
		}
		if strings.TrimSpace(acc.FullCookie) == "" && strings.TrimSpace(acc.Cookie) != "" {
			acc.FullCookie = strings.TrimSpace(acc.Cookie)
		}
		if strings.TrimSpace(acc.Provider) == "" {
			if df.defaultProvider != "" {
				acc.Provider = df.defaultProvider
			} else if strings.TrimSpace(acc.SpaceID) == "" || strings.EqualFold(strings.TrimSpace(acc.SpaceID), "perplexity") {
				acc.Provider = "perplexity"
			}
		}
		acc.normalizeProviderFields()
		if len(acc.Models) == 0 && acc.IsPerplexity() {
			if models, mErr := fetchPerplexityModels(&acc); mErr == nil {
				acc.Models = models
			}
		}
		id := acc.EnsureAccountID()
		if id != "" && seen[id] {
			continue
		}
		if id != "" {
			seen[id] = true
		}
		loaded = append(loaded, &acc)
	}

	p.mu.Lock()
	p.dir = dir
	p.accounts = loaded
	p.mu.Unlock()
	return nil
}

func (p *AccountPool) AddAccount(acc *Account) {
	if acc == nil {
		return
	}
	acc.normalizeProviderFields()
	if len(acc.Models) == 0 && acc.IsPerplexity() {
		if models, mErr := fetchPerplexityModels(acc); mErr == nil {
			acc.Models = models
		}
	}
	id := acc.EnsureAccountID()
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, existing := range p.accounts {
		if existing != nil && existing.EnsureAccountID() == id && id != "" {
			p.accounts[i] = acc
			return
		}
	}
	p.accounts = append(p.accounts, acc)
}

func (p *AccountPool) AccountsSnapshot() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Account, len(p.accounts))
	copy(out, p.accounts)
	return out
}

func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}

func (p *AccountPool) AvailableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	count := 0
	for _, acc := range p.accounts {
		if !p.isUnusable(acc) {
			count++
		}
	}
	return count
}

func (p *AccountPool) AllModels() []ModelEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	seen := make(map[string]bool)
	var out []ModelEntry
	for _, acc := range p.accounts {
		for _, m := range acc.modelsSnapshot() {
			key := m.ID + ":" + m.Name
			if !seen[key] {
				seen[key] = true
				out = append(out, m)
			}
		}
	}
	return out
}

func (p *AccountPool) resolveTargetProvider(model string) string {
	target := resolveTargetProvider(model)
	p.mu.RLock()
	defer p.mu.RUnlock()
	hasTarget := false
	hasPerplexity := false
	for _, acc := range p.accounts {
		if acc == nil {
			continue
		}
		if acc.ProviderName() == target {
			hasTarget = true
		}
		if acc.IsPerplexity() {
			hasPerplexity = true
		}
	}
	if !hasTarget && hasPerplexity && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "notion/") {
		return "perplexity"
	}
	return target
}

func (p *AccountPool) isUnusable(acc *Account) bool {
	if acc == nil || acc.isRemoved() {
		return true
	}
	acc.mu.RLock()
	exhausted := acc.PermanentlyExhausted || acc.QuotaExhaustedAt != nil
	qi := acc.QuotaInfo
	acc.mu.RUnlock()
	if exhausted {
		return true
	}
	if qi != nil && !qi.IsEligible && basicRemaining(qi) <= 0 {
		return true
	}
	return false
}

func (p *AccountPool) isUnusableForModel(acc *Account, model string) bool {
	if p.isUnusable(acc) {
		return true
	}
	if acc.IsPerplexity() && !acc.supportsPerplexityModel(model) {
		return true
	}
	return false
}

func (p *AccountPool) rankAccountForModel(acc *Account, model string) accountRank {
	if acc == nil {
		return accountRank{layer: rankUnknown}
	}
	if acc.IsPerplexity() {
		return perplexityAccountRank(acc, model)
	}
	q := acc.quotaInfoSnapshot()
	if q == nil {
		return accountRank{layer: 1}
	}
	return accountRank{layer: 2, plan: basicRemaining(q), rem: q.PremiumBalance}
}

func (p *AccountPool) Next() *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := len(p.accounts)
	if n == 0 {
		return nil
	}
	for i := 0; i < n; i++ {
		idx := int(p.index.Add(1)-1) % n
		acc := p.accounts[idx]
		if !p.isUnusable(acc) {
			return acc
		}
	}
	return nil
}

func (p *AccountPool) NextBestForModel(model string) *Account {
	provider := p.resolveTargetProvider(model)
	return p.NextBestForProviderModel(provider, model)
}

func (p *AccountPool) NextBestForProviderModel(provider, model string) *Account {
	return p.NextBestExcludingForProviderModel(provider, nil, model)
}

func (p *AccountPool) NextBestExcludingForProviderModel(provider string, exclude map[*Account]bool, model string) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var best *Account
	bestRank := accountRank{layer: rankUnknown}
	bestTier := 99

	for _, acc := range p.accounts {
		if acc == nil || (exclude != nil && exclude[acc]) {
			continue
		}
		if provider != "" && acc.ProviderName() != provider {
			continue
		}
		if p.isUnusableForModel(acc, model) {
			continue
		}
		tier := acc.roleTier()
		rank := p.rankAccountForModel(acc, model)
		if rank.layer == rankUnknown {
			continue
		}
		if best == nil || tier < bestTier || (tier == bestTier && rank.betterThan(bestRank)) {
			best = acc
			bestRank = rank
			bestTier = tier
		}
	}
	return best
}

func (p *AccountPool) FindByAccountID(id string) *Account {
	target := strings.TrimSpace(id)
	if target == "" {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, acc := range p.accounts {
		if acc != nil && acc.accountIDSnapshot() == target {
			return acc
		}
	}
	return nil
}

func (p *AccountPool) GetByEmail(email string) *Account {
	target := strings.TrimSpace(email)
	if target == "" {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, acc := range p.accounts {
		if acc != nil && strings.EqualFold(acc.UserEmail, target) {
			return acc
		}
	}
	return nil
}

func (p *AccountPool) PreferredAccountID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.preferredAccountID
}

func (p *AccountPool) RefreshAccountQuota(acc *Account, minInterval time.Duration) bool {
	if acc == nil {
		return false
	}
	acc.mu.RLock()
	checkedAt := acc.QuotaCheckedAt
	acc.mu.RUnlock()
	if minInterval > 0 && checkedAt != nil && time.Since(*checkedAt) < minInterval {
		return !p.isUnusable(acc)
	}
	fn := quotaFetcher
	if fn == nil {
		fn = CheckQuota
	}
	info, err := fn(acc)
	now := time.Now()
	if err != nil {
		if errors.Is(err, ErrQuotaExhausted) {
			acc.markQuotaExhausted(now, false)
			return false
		}
		return !p.isUnusable(acc)
	}
	if info != nil {
		acc.setQuotaInfo(info, &now)
		if info.IsEligible && basicRemaining(info) > 0 {
			acc.clearQuotaExhausted()
		} else if !info.IsEligible {
			acc.markQuotaExhausted(now, false)
		}
	}
	return !p.isUnusable(acc)
}

func (p *AccountPool) RefreshAccountQuotaForModel(acc *Account, minInterval time.Duration, model string) bool {
	if !p.RefreshAccountQuota(acc, minInterval) {
		return false
	}
	return !p.isUnusableForModel(acc, model)
}

func (p *AccountPool) RefreshAccountQuotaAsync(acc *Account) {
	if acc == nil {
		return
	}
	p.liveQuotaMu.Lock()
	if p.liveQuotaInflight[acc] {
		p.liveQuotaMu.Unlock()
		return
	}
	p.liveQuotaInflight[acc] = true
	p.liveQuotaWg.Add(1)
	p.liveQuotaMu.Unlock()

	go func() {
		defer func() {
			p.liveQuotaMu.Lock()
			delete(p.liveQuotaInflight, acc)
			p.liveQuotaWg.Done()
			p.liveQuotaMu.Unlock()
		}()
		interval := 5 * time.Second
		if AppConfig != nil {
			interval = AppConfig.QuotaLiveCheckInterval()
		}
		p.RefreshAccountQuota(acc, interval)
	}()
}

func (p *AccountPool) MarkInferenceUnavailable(acc *Account) {
	if acc == nil {
		return
	}
	acc.markQuotaExhausted(time.Now(), false)
	if globalSessionManager != nil {
		globalSessionManager.DeleteByAccount(acc.UserEmail)
		globalSessionManager.DeleteByAccountID(acc.accountIDSnapshot())
	}
}

func (p *AccountPool) MarkAccountAuthExpired(acc *Account) {
	p.MarkInferenceUnavailable(acc)
}

func (p *AccountPool) hasPerplexityModelMismatch(model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	sawPerplexity := false
	for _, acc := range p.accounts {
		if acc == nil || !acc.IsPerplexity() || p.isUnusable(acc) {
			continue
		}
		sawPerplexity = true
		if acc.supportsPerplexityModel(model) {
			return false
		}
	}
	return sawPerplexity
}

func (p *AccountPool) allQuotaExhaustedForProviderModel(provider, model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	matched := 0
	exhausted := 0
	for _, acc := range p.accounts {
		if acc == nil || (provider != "" && acc.ProviderName() != provider) {
			continue
		}
		if acc.IsPerplexity() && !acc.supportsPerplexityModel(model) {
			continue
		}
		matched++
		if p.isUnusable(acc) {
			exhausted++
		}
	}
	return matched > 0 && matched == exhausted
}

func HandleAddAccount(pool *AccountPool, dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Provider     string `json:"provider"`
			SessionToken string `json:"session_token"`
			TokenV2      string `json:"token_v2"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		tok := strings.TrimSpace(req.SessionToken)
		if tok == "" {
			tok = strings.TrimSpace(req.TokenV2)
		}
		acc, err := DiscoverPerplexityAccountFromToken(tok)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		pool.AddAccount(acc)
		if strings.TrimSpace(dir) != "" {
			subDir := filepath.Join(dir, "perplexity")
			_ = os.MkdirAll(subDir, 0o700)
			id := acc.EnsureAccountID()
			short := id
			if len(short) > 12 {
				short = short[:12]
			}
			outPath := filepath.Join(subDir, fmt.Sprintf("perplexity_%s.json", short))
			if raw, mErr := json.MarshalIndent(acc, "", "  "); mErr == nil {
				_ = os.WriteFile(outPath, raw, 0o600)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(acc)
	}
}

func (p *AccountPool) RefreshAll(accountsDir string) {
	p.mu.RLock()
	accs := append([]*Account(nil), p.accounts...)
	p.mu.RUnlock()
	for _, acc := range accs {
		p.RefreshAccountQuota(acc, 0)
	}
}

func (p *AccountPool) StartRefreshLoop(interval time.Duration, accountsDir string) {
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			p.RefreshAll(accountsDir)
		}
	}()
}

