package bridge

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const usageTTL = time.Minute

type usageLimit struct {
	Type        string     `json:"type"`
	PercentUsed *float64   `json:"percent_used"`
	ResetsAt    *time.Time `json:"resets_at"`
}

type usagePlan struct {
	Name             string     `json:"name"`
	CurrentPeriodEnd *time.Time `json:"current_period_end"`
}

// Only display fields cross the management API; upstream account/stripe IDs do not.
type credentialUsage struct {
	ID            string       `json:"id"`
	Limits        []usageLimit `json:"limits"`
	Plan          *usagePlan   `json:"plan"`
	UpdatedAt     *time.Time   `json:"updated_at"`
	PlanUpdatedAt *time.Time   `json:"plan_updated_at"`
	CheckedAt     *time.Time   `json:"checked_at"`
	Status        string       `json:"status"`
	Error         string       `json:"error,omitempty"`
	PlanError     string       `json:"plan_error,omitempty"`
}

type usageCacheEntry struct {
	fingerprint [32]byte
	value       credentialUsage
	done        chan struct{}
}

func usageFingerprint(c Credential) [32]byte {
	return sha256.Sum256(jsonBytes([]any{c.APIKey, c.ProxyURL, c.Disabled}))
}

func (s *Service) credentialUsage(r ManagementRequest) (credentialUsage, error) {
	if err := s.begin(); err != nil {
		return credentialUsage{}, err
	}
	defer s.active.Done()
	credentialID := r.Query.Get("id")
	deadline := time.Now().Add(15 * time.Second)
	for {
		s.mu.Lock()
		c, ok := s.creds[credentialID]
		if !ok || s.revoked[credentialID] {
			s.mu.Unlock()
			return credentialUsage{}, fail(404, "凭据已删除，请刷新列表")
		}
		fingerprint := usageFingerprint(c)
		entry := s.usageCache[credentialID]
		if entry == nil || entry.fingerprint != fingerprint {
			entry = &usageCacheEntry{fingerprint: fingerprint, value: credentialUsage{ID: credentialID, Limits: []usageLimit{}}}
			s.usageCache[credentialID] = entry
		}
		if c.Disabled || strings.TrimSpace(c.ProxyURL) != "" {
			value := entry.value
			now := time.Now().UTC()
			value.CheckedAt = &now
			value.Status, value.Error = "disabled", "凭据已停用，暂停查询"
			if !c.Disabled {
				// CPA v7.3.12's management HTTP callback uses the global proxy and
				// exposes no per-auth override. Never silently bypass an auth proxy.
				value.Status, value.Error = "proxy_unsupported", "此 CPA 接口暂不支持凭据独立代理的用量查询"
			}
			s.mu.Unlock()
			return value, nil
		}
		if done := entry.done; done != nil {
			s.mu.Unlock()
			timer := time.NewTimer(time.Until(deadline))
			select {
			case <-done:
				timer.Stop()
				continue
			case <-s.stopCh:
				timer.Stop()
				return credentialUsage{}, fail(503, "插件正在关闭")
			case <-timer.C:
				return credentialUsage{}, fail(504, "用量查询等待超时")
			}
		}
		ttl := usageTTL
		if entry.value.Status != "ok" {
			ttl = 30 * time.Second
		}
		if r.Query.Get("refresh") == "1" {
			ttl = 10 * time.Second
		}
		if entry.value.CheckedAt != nil && time.Since(*entry.value.CheckedAt) < ttl {
			value := entry.value
			s.mu.Unlock()
			return value, nil
		}
		entry.done = make(chan struct{})
		value := entry.value
		s.mu.Unlock()

		// Bound work across tabs/accounts. Quota reads never enter CPA's model scheduler.
		timer := time.NewTimer(time.Until(deadline))
		select {
		case s.usageSlots <- struct{}{}:
			timer.Stop()
			value = s.fetchCredentialUsage(r.HostCallbackID, c, value, deadline)
			<-s.usageSlots
		case <-s.stopCh:
			timer.Stop()
			value.Status, value.Error = "unavailable", "插件正在关闭"
		case <-timer.C:
			value.Status, value.Error = "timeout", "查询繁忙，请稍后刷新"
		}
		now := time.Now().UTC()
		value.CheckedAt = &now
		s.mu.Lock()
		entry.value = value
		close(entry.done)
		entry.done = nil
		current, exists := s.creds[credentialID]
		valid := exists && !s.revoked[credentialID] && usageFingerprint(current) == fingerprint && s.usageCache[credentialID] == entry
		s.mu.Unlock()
		if !valid {
			return credentialUsage{}, fail(409, "凭据已变更，请重新查询用量")
		}
		return value, nil
	}
}

func (s *Service) usageGET(callbackID string, c Credential, path string, deadline time.Time) (json.RawMessage, error) {
	up, err := s.openUpstream(map[string]any{"host_callback_id": callbackID, "method": "GET", "url": s.config().BaseURL + path, "headers": headers(c)}, deadline)
	if err != nil {
		return nil, err
	}
	b, err := s.readJSON(up)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(b, &envelope) != nil || !envelope.Success || len(envelope.Data) == 0 {
		return nil, fail(502, "invalid plan response")
	}
	return envelope.Data, nil
}

func usageFailure(err error) (string, string) {
	switch statusOf(err) {
	case 401, 403:
		return "unauthorized", "Cline 拒绝查询，请检查 API Key 或账号权限"
	case 429:
		return "rate_limited", "Cline 查询限流，请稍后刷新"
	case 504:
		return "timeout", "Cline 查询超时，请稍后刷新"
	default:
		return "unavailable", fmt.Sprintf("暂时未能获取 Cline 用量（HTTP %d）", statusOf(err))
	}
}

func usageTime(raw string) *time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil
	}
	return &parsed
}

func (s *Service) fetchCredentialUsage(callbackID string, c Credential, value credentialUsage, deadline time.Time) credentialUsage {
	data, err := s.usageGET(callbackID, c, "/users/me/plan/usage-limits", deadline)
	var body struct {
		Limits *[]struct {
			Type        string   `json:"type"`
			PercentUsed *float64 `json:"percentUsed"`
			ResetsAt    string   `json:"resetsAt"`
		} `json:"limits"`
	}
	if err == nil && (json.Unmarshal(data, &body) != nil || body.Limits == nil) {
		err = fail(502, "invalid limits response")
	}
	if err != nil {
		value.Status, value.Error = usageFailure(err)
		return value // Keep last successful values with their original timestamp.
	}
	limits := []usageLimit{}
	for _, limit := range *body.Limits {
		if limit.Type == "" {
			continue
		}
		if limit.PercentUsed != nil && *limit.PercentUsed < 0 {
			limit.PercentUsed = nil
		}
		limits = append(limits, usageLimit{Type: limit.Type, PercentUsed: limit.PercentUsed, ResetsAt: usageTime(limit.ResetsAt)})
	}
	now := time.Now().UTC()
	value.Limits, value.UpdatedAt, value.Status, value.Error = limits, &now, "ok", ""
	if value.PlanUpdatedAt != nil && time.Since(*value.PlanUpdatedAt) < 5*time.Minute && value.PlanError == "" {
		return value
	}
	data, err = s.usageGET(callbackID, c, "/users/me/plan", deadline)
	var plan *struct {
		CurrentPeriodEnd string `json:"currentPeriodEnd"`
		Plan             *struct {
			DisplayName string `json:"displayName"`
		} `json:"plan"`
	}
	if err == nil && json.Unmarshal(data, &plan) != nil {
		err = fail(502, "invalid plan response")
	}
	if err != nil {
		_, value.PlanError = usageFailure(err)
		return value
	}
	value.Plan = nil
	if plan != nil && plan.Plan != nil {
		value.Plan = &usagePlan{Name: plan.Plan.DisplayName, CurrentPeriodEnd: usageTime(plan.CurrentPeriodEnd)}
	}
	value.PlanUpdatedAt, value.PlanError = &now, ""
	return value
}
