package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	AccountErrQuotaExhausted = "quota_exhausted"
	AccountErrEmptyResponse  = "empty_response"
	AccountErrPremiumGone    = "premium_unavailable"
	AccountErrRequestFailed  = "request_failed"
	statusFileName           = ".account_status.json"
)

type AccountDetail struct {
	AccountID                 string       `json:"account_id"`
	Provider                  string       `json:"provider"`
	UserID                    string       `json:"user_id"`
	UserName                  string       `json:"user_name"`
	UserEmail                 string       `json:"user_email"`
	SpaceID                   string       `json:"space_id"`
	SpaceName                 string       `json:"space_name"`
	PlanType                  string       `json:"plan_type"`
	Role                      string       `json:"role,omitempty"`
	Available                 bool         `json:"available"`
	Models                    []ModelEntry `json:"models"`
	QuotaInfo                 *QuotaInfo   `json:"quota_info,omitempty"`
	QuotaCheckedAt            *time.Time   `json:"quota_checked_at,omitempty"`
	QuotaExhaustedAt          *time.Time   `json:"quota_exhausted_at,omitempty"`
	PermanentlyExhausted      bool         `json:"permanently_exhausted,omitempty"`
	PerplexityOpusUnavailable bool         `json:"perplexity_opus_unavailable,omitempty"`
	LastUsedAt                *time.Time   `json:"last_used_at,omitempty"`
	LastError                 string       `json:"last_error,omitempty"`
	LastErrorAt               *time.Time   `json:"last_error_at,omitempty"`
}

type ProviderAccountsSummary struct {
	TotalAccounts       int `json:"total_accounts"`
	AvailableAccounts   int `json:"available_accounts"`
	ExhaustedAccounts   int `json:"exhausted_accounts"`
	ProAccounts         int `json:"pro_accounts,omitempty"`
	MaxAccounts         int `json:"max_accounts,omitempty"`
	TotalRemainingPro   int `json:"total_remaining_pro,omitempty"`
	TotalRemaining      int `json:"total_remaining,omitempty"`
	TotalPremiumBalance int `json:"total_premium_balance,omitempty"`
}

type AccountsSummary struct {
	TotalAccounts       int                                `json:"total_accounts"`
	AvailableAccounts   int                                `json:"available_accounts"`
	ExhaustedAccounts   int                                `json:"exhausted_accounts"`
	TotalRemaining      int                                `json:"total_remaining"`
	TotalPremiumBalance int                                `json:"total_premium_balance"`
	ByProvider          map[string]ProviderAccountsSummary `json:"by_provider,omitempty"`
}

func (p *AccountPool) GetAccountDetails() []AccountDetail {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]AccountDetail, 0, len(p.accounts))
	for _, acc := range p.accounts {
		if acc == nil {
			continue
		}
		id := acc.EnsureAccountID()
		available := !p.isUnusable(acc)
		acc.mu.RLock()
		detail := AccountDetail{
			AccountID:                 id,
			Provider:                  acc.ProviderName(),
			UserID:                    acc.UserID,
			UserName:                  acc.UserName,
			UserEmail:                 acc.UserEmail,
			SpaceID:                   acc.SpaceID,
			SpaceName:                 acc.SpaceName,
			PlanType:                  acc.PlanType,
			Role:                      acc.Role,
			Available:                 available,
			Models:                    cloneModelEntries(acc.Models),
			QuotaInfo:                 cloneQuotaInfo(acc.QuotaInfo),
			QuotaCheckedAt:            cloneTimePtr(acc.QuotaCheckedAt),
			QuotaExhaustedAt:          cloneTimePtr(acc.QuotaExhaustedAt),
			PermanentlyExhausted:      acc.PermanentlyExhausted,
			PerplexityOpusUnavailable: acc.perplexityOpusUnavailable,
			LastUsedAt:                cloneTimePtr(acc.LastUsedAt),
			LastError:                 acc.LastError,
			LastErrorAt:               cloneTimePtr(acc.LastErrorAt),
		}
		acc.mu.RUnlock()
		out = append(out, detail)
	}
	return out
}

func summarizeAccounts(details []AccountDetail) AccountsSummary {
	summary := AccountsSummary{
		ByProvider: make(map[string]ProviderAccountsSummary),
	}
	for _, d := range details {
		summary.TotalAccounts++
		if d.Available {
			summary.AvailableAccounts++
		} else {
			summary.ExhaustedAccounts++
		}
		prov := strings.ToLower(strings.TrimSpace(d.Provider))
		if prov == "" {
			prov = "notion"
		}
		ps := summary.ByProvider[prov]
		ps.TotalAccounts++
		if d.Available {
			ps.AvailableAccounts++
		} else {
			ps.ExhaustedAccounts++
		}
		rem := basicRemaining(d.QuotaInfo)
		if prov == "perplexity" {
			plan := normalizePerplexityPlanType(d.PlanType)
			if plan == "max" {
				ps.MaxAccounts++
			} else {
				ps.ProAccounts++
			}
			ps.TotalRemainingPro += rem
		} else {
			summary.TotalRemaining += rem
			ps.TotalRemaining += rem
			if d.QuotaInfo != nil {
				summary.TotalPremiumBalance += d.QuotaInfo.PremiumBalance
				ps.TotalPremiumBalance += d.QuotaInfo.PremiumBalance
			}
		}
		summary.ByProvider[prov] = ps
	}
	return summary
}

func (p *AccountPool) GetQuotaSummary() AccountsSummary {
	return summarizeAccounts(p.GetAccountDetails())
}

func (p *AccountPool) RecordAccountUse(acc *Account) {
	if acc == nil {
		return
	}
	now := time.Now()
	acc.mu.Lock()
	acc.LastUsedAt = &now
	acc.LastError = ""
	acc.LastErrorAt = nil
	acc.mu.Unlock()
	p.saveStatusSnapshot()
}

func (p *AccountPool) RecordAccountError(acc *Account, code string) {
	if acc == nil {
		return
	}
	now := time.Now()
	acc.mu.Lock()
	acc.LastError = code
	acc.LastErrorAt = &now
	acc.mu.Unlock()
	p.saveStatusSnapshot()
}

func (p *AccountPool) saveStatusSnapshot() {
	p.mu.RLock()
	dir := p.dir
	p.mu.RUnlock()
	if strings.TrimSpace(dir) == "" {
		return
	}
	details := p.GetAccountDetails()
	summary := summarizeAccounts(details)
	byID := make(map[string]AccountDetail, len(details))
	for _, d := range details {
		if d.AccountID != "" {
			byID[d.AccountID] = d
		}
	}
	payload := map[string]interface{}{
		"updated_at": time.Now(),
		"summary":    summary,
		"accounts":   byID,
	}
	if raw, err := json.MarshalIndent(payload, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, statusFileName), raw, 0o600)
	}
}

func (p *AccountPool) RefreshAllQuotas(concurrency int) {
	accounts := p.AccountsSnapshot()
	if len(accounts) == 0 {
		return
	}
	if concurrency <= 0 {
		concurrency = 4
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(a *Account) {
			defer wg.Done()
			defer func() { <-sem }()
			p.RefreshAccountQuota(a, 0)
		}(acc)
	}
	wg.Wait()
	p.saveStatusSnapshot()
}

func (p *AccountPool) StartPeriodicRefresh(interval time.Duration, concurrency int) {
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	go func() {
		p.RefreshAllQuotas(concurrency)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.RefreshAllQuotas(concurrency)
			case <-p.stopCh:
				return
			}
		}
	}()
}

func (p *AccountPool) StopPeriodicRefresh() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
	})
}
