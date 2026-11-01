package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type estimateTotals struct {
	Cost     priceRange `json:"cost"`
	Tokens   int64      `json:"tokens"`
	Requests int64      `json:"requests"`
	Unknown  int64      `json:"unknown"`
}
type estimateKey struct {
	Account string         `json:"account"`
	Totals  estimateTotals `json:"totals"`
}
type estimateBaseline struct {
	Percent    float64        `json:"percent"`
	At         time.Time      `json:"at"`
	Totals     estimateTotals `json:"totals"`
	Concurrent bool           `json:"concurrent,omitempty"`
}

// Saved estimates survive quota resets. Preliminary snapshots stay separate
// from accepted samples so their pending consumption is never counted twice.
type estimateSample struct {
	Cost               priceRange `json:"cost"`
	Delta              float64    `json:"delta"`
	Tokens             int64      `json:"tokens"`
	Requests           int64      `json:"requests"`
	Samples            int64      `json:"samples"`
	IncompleteRequests int64      `json:"incomplete_requests,omitempty"`
	ConcurrentSamples  int64      `json:"concurrent_samples,omitempty"`
	Updated            time.Time  `json:"updated"`
	Preliminary        bool       `json:"preliminary,omitempty"`
}
type estimateWindow struct {
	Reset              time.Time         `json:"reset"`
	Base               *estimateBaseline `json:"base,omitempty"`
	Cost               priceRange        `json:"cost"`
	Delta              float64           `json:"delta"`
	Tokens             int64             `json:"tokens"`
	Requests           int64             `json:"requests"`
	Samples            int64             `json:"samples"`
	LastPercent        float64           `json:"last_percent"`
	Updated            time.Time         `json:"updated"`
	Note               string            `json:"note,omitempty"`
	IncompleteRequests int64             `json:"incomplete_requests,omitempty"`
	ConcurrentSamples  int64             `json:"concurrent_samples,omitempty"`
	CalibratedAt       time.Time         `json:"calibrated_at,omitempty"`
	Previous           *estimateSample   `json:"previous,omitempty"`
	Preliminary        *estimateSample   `json:"preliminary,omitempty"`
}
type estimateAccount struct {
	Plan    string                     `json:"plan"`
	Windows map[string]*estimateWindow `json:"windows"`
}
type estimateState struct {
	Revision string                      `json:"revision"`
	Keys     map[string]*estimateKey     `json:"keys"`
	Accounts map[string]*estimateAccount `json:"accounts"`
}
type estimateActivity struct {
	Revision uint64
	Active   int
}
type estimateResult struct {
	PendingRequests    int64       `json:"pending_requests"`
	PendingPercent     float64     `json:"pending_percent"`
	PendingIncomplete  int64       `json:"pending_incomplete"`
	IncompleteRequests int64       `json:"incomplete_requests"`
	ConcurrentSamples  int64       `json:"concurrent_samples"`
	Approximate        bool        `json:"approximate"`
	Historical         bool        `json:"historical"`
	Preliminary        bool        `json:"preliminary"`
	Type               string      `json:"type"`
	DerivedFrom        string      `json:"derived_from,omitempty"`
	Total              *priceRange `json:"total,omitempty"`
	Remaining          *priceRange `json:"remaining,omitempty"`
	Cost               priceRange  `json:"sample_cost"`
	Delta              float64     `json:"percent_change"`
	Tokens             int64       `json:"tokens"`
	Requests           int64       `json:"requests"`
	Samples            int64       `json:"samples"`
	Updated            time.Time   `json:"updated_at"`
	Note               string      `json:"note"`
}
type accountEstimate struct {
	Status            string           `json:"status"`
	Note              string           `json:"note"`
	SharedCredentials int              `json:"shared_credentials"`
	Windows           []estimateResult `json:"windows"`
	PricingRevision   string           `json:"pricing_revision"`
	PersistenceError  string           `json:"persistence_error,omitempty"`
}

func estimateHash(kind, value string) string {
	if value == "" {
		return ""
	}
	h := sha256.Sum256([]byte(kind + "\x00" + value))
	return hex.EncodeToString(h[:])
}
func estimateKeyID(c Credential) string { return estimateHash("cline-key", c.APIKey) }

// Cline reconstructs resetsAt with sub-second jitter on each quota read.
// Compare to the original window anchor, not the previous read, so tolerated
// drift cannot accumulate indefinitely and merge genuinely different cycles.
func sameEstimateReset(a, b time.Time) bool {
	d := a.Sub(b)
	return d >= -2*time.Second && d <= 2*time.Second
}

func (w *estimateWindow) currentSample() *estimateSample {
	if w == nil || w.Samples <= 0 || w.Delta <= float64(w.Samples) {
		return nil
	}
	at := w.CalibratedAt
	if at.IsZero() {
		at = w.Updated // Existing ledgers predate a separate calibration timestamp.
	}
	return &estimateSample{
		Cost: w.Cost, Delta: w.Delta, Tokens: w.Tokens, Requests: w.Requests, Samples: w.Samples,
		IncompleteRequests: w.IncompleteRequests, ConcurrentSamples: w.ConcurrentSamples, Updated: at,
	}
}

func nextEstimateWindow(previous *estimateWindow, reset time.Time) *estimateWindow {
	w := &estimateWindow{Reset: reset}
	if previous != nil {
		w.Previous, _ = previous.displaySample()
	}
	return w
}

func (w *estimateWindow) displaySample() (*estimateSample, bool) {
	if current := w.currentSample(); current != nil {
		return current, false
	}
	// Prefer an established historical estimate to a new one-point preview.
	if w.Previous != nil && !w.Previous.Preliminary {
		return w.Previous, true
	}
	if w.Preliminary != nil {
		return w.Preliminary, false
	}
	return w.Previous, w.Previous != nil
}

// Identifying another key changes the account counters, not its quota capacity.
// Keep accepted estimates but never include the newly linked historical counters
// in the next calibration interval.
func rebaseEstimateAccount(account *estimateAccount) {
	if account != nil {
		for _, w := range account.Windows {
			if w != nil {
				w.Base = nil
			}
		}
	}
}

// Caller holds s.mu. This independent ledger is never trimmed with request logs.
func (s *Service) loadEstimatesLocked() {
	s.estimates = estimateState{Revision: pricingRevision, Keys: map[string]*estimateKey{}, Accounts: map[string]*estimateAccount{}}
	b, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "estimates.json"))
	if err == nil {
		var saved estimateState
		if json.Unmarshal(b, &saved) == nil && saved.Revision == pricingRevision && saved.Keys != nil && saved.Accounts != nil {
			s.estimates = saved
		} else {
			s.estimateWriteError = "估算存档格式或价格版本变化，已重新开始采样"
		}
	} else if !os.IsNotExist(err) {
		s.estimateWriteError = "估算存档读取失败，已重新开始采样"
	}
	// A restart can interrupt requests or miss activity. Never bridge that gap.
	for _, a := range s.estimates.Accounts {
		if a != nil {
			for _, w := range a.Windows {
				if w != nil {
					w.Base = nil
				}
			}
		}
	}
	if s.estimateActivity == nil {
		s.estimateActivity = map[string]estimateActivity{}
	}
}
func (s *Service) saveEstimatesLocked() {
	if err := atomicJSON(filepath.Join(s.cfg.DataDir, "estimates.json"), s.estimates); err != nil {
		s.estimateWriteError = "估算数据保存失败；重启后可能丢失本次采样"
	} else {
		s.estimateWriteError = ""
	}
}
func (s *Service) startEstimateRequest(c Credential) string {
	key := estimateKeyID(c)
	if key == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.estimateActivity == nil {
		s.estimateActivity = map[string]estimateActivity{}
	}
	a := s.estimateActivity[key]
	a.Active++
	a.Revision++
	s.estimateActivity[key] = a
	return key
}
func (s *Service) recordEstimateLocked(e LogEntry) {
	key := e.estimateKey
	if key == "" {
		return
	}
	a := s.estimateActivity[key]
	if a.Active > 0 {
		a.Active--
	}
	a.Revision++
	s.estimateActivity[key] = a
	if e.UpstreamSkipped {
		return
	}
	if s.estimates.Keys == nil {
		s.estimates.Keys = map[string]*estimateKey{}
		s.estimates.Accounts = map[string]*estimateAccount{}
		s.estimates.Revision = pricingRevision
	}
	k := s.estimates.Keys[key]
	if k == nil {
		k = &estimateKey{}
		s.estimates.Keys[key] = k
	}
	cost, ok := referenceCost(e)
	if !ok || e.Status < 200 || e.Status >= 300 || len(e.Attempts) != 1 {
		// Keep the gap explicit instead of silently treating missing usage as zero.
		k.Totals.Unknown++
	}
	if ok && len(e.Attempts) == 1 {
		// An interrupted response may still contain reported usage. Preserve that
		// known cost, with the Unknown marker above for any unobserved remainder.
		k.Totals.Cost.Low += cost.Low
		k.Totals.Cost.High += cost.High
		k.Totals.Tokens += e.PromptTokens + e.CompletionTokens
		k.Totals.Requests++
	}
	s.saveEstimatesLocked()
}
func (s *Service) estimateActivitySnapshot() map[string]estimateActivity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]estimateActivity{}
	for k, a := range s.estimateActivity {
		out[k] = a
	}
	return out
}
func (s *Service) estimateTotalsLocked(account string) estimateTotals {
	var out estimateTotals
	for _, k := range s.estimates.Keys {
		if k != nil && k.Account == account {
			out.Cost.Low += k.Totals.Cost.Low
			out.Cost.High += k.Totals.Cost.High
			out.Tokens += k.Totals.Tokens
			out.Requests += k.Totals.Requests
			out.Unknown += k.Totals.Unknown
		}
	}
	return out
}

// Identity comes only from the authenticated plan response, never labels/models.
// Only successful fresh quota reads call this, under s.mu and after key validation.
func (s *Service) calibrateEstimateLocked(c Credential, value credentialUsage, before map[string]estimateActivity) {
	if value.accountHash == "" || value.UpdatedAt == nil || value.Status != "ok" {
		return
	}
	if s.estimates.Keys == nil {
		s.estimates.Keys = map[string]*estimateKey{}
		s.estimates.Accounts = map[string]*estimateAccount{}
		s.estimates.Revision = pricingRevision
	}
	key := estimateKeyID(c)
	k := s.estimates.Keys[key]
	if k == nil {
		k = &estimateKey{}
		s.estimates.Keys[key] = k
	}
	account := s.estimates.Accounts[value.accountHash]
	if account == nil {
		account = &estimateAccount{Windows: map[string]*estimateWindow{}}
		s.estimates.Accounts[value.accountHash] = account
	}
	if account.Windows == nil {
		account.Windows = map[string]*estimateWindow{}
	}
	if account.Plan != value.planHash {
		account.Windows = map[string]*estimateWindow{}
		account.Plan = value.planHash
	}
	if k.Account != value.accountHash {
		rebaseEstimateAccount(account)
		rebaseEstimateAccount(s.estimates.Accounts[k.Account])
		k.Account = value.accountHash
	}
	concurrent := false
	for key, known := range s.estimates.Keys {
		if known == nil || known.Account != value.accountHash {
			continue
		}
		current := s.estimateActivity[key]
		if current.Active > 0 || before[key].Active > 0 || current.Revision != before[key].Revision {
			concurrent = true
		}
	}
	totals := s.estimateTotalsLocked(value.accountHash)
	now := *value.UpdatedAt
	for _, limit := range value.Limits {
		// Monthly is never sampled on its own: the view scales the weekly estimate.
		if limit.Type != "five_hour" && limit.Type != "weekly" {
			continue
		}
		if limit.PercentUsed == nil || limit.ResetsAt == nil || !limit.ResetsAt.After(now) || *limit.PercentUsed < 0 || *limit.PercentUsed > 100 {
			continue
		}
		p := *limit.PercentUsed
		w := account.Windows[limit.Type]
		if w != nil && !w.Updated.IsZero() && !now.After(w.Updated) {
			continue
		}
		if w == nil || !sameEstimateReset(w.Reset, *limit.ResetsAt) || !w.Reset.After(now) || p < w.LastPercent {
			w = nextEstimateWindow(w, *limit.ResetsAt)
			account.Windows[limit.Type] = w
		}
		w.LastPercent, w.Updated = p, now
		base := &estimateBaseline{Percent: p, At: now, Totals: totals, Concurrent: concurrent}
		if w.Base == nil {
			w.Base = base
			w.Note = "已记录基线，等待额度变化"
			if concurrent {
				w.Note = "已记录基线；查询期间有请求，后续结果将标注近似"
			}
			continue
		}
		b := w.Base
		if now.Sub(b.At) > 5*time.Hour && limit.Type == "five_hour" {
			w.Base = base
			w.Note = "本段跨度超过 5 小时滚动窗口，已重新建立基线"
			continue
		}
		delta := p - b.Percent
		incomplete := totals.Unknown - b.Totals.Unknown
		low, high := totals.Cost.Low-b.Totals.Cost.Low, totals.Cost.High-b.Totals.Cost.High
		if delta > 0 && high <= 0 {
			if concurrent {
				w.Note = "额度已变化，等待进行中的请求返回用量"
				continue
			}
			w.Base = base
			w.Note = "额度变化未匹配到本插件消费，已重新采样"
			continue
		}
		if p >= 100 {
			w.Base = base
			w.Note = "额度到达上限，跳过可能被截断的百分比"
			continue
		}
		// Preview the pending interval at one point, without advancing its base.
		// At two points, the same interval is accepted once with its full cost.
		if delta < 2 {
			w.Note = "累计增加 1 个百分点后显示初步估计，2 个百分点后校准"
			if delta >= 1 {
				preview := &estimateSample{
					Cost: priceRange{low, high}, Delta: delta, Samples: 1,
					Tokens: totals.Tokens - b.Totals.Tokens, Requests: totals.Requests - b.Totals.Requests,
					IncompleteRequests: incomplete, Updated: now, Preliminary: true,
				}
				if b.Concurrent || concurrent {
					preview.ConcurrentSamples = 1
				}
				w.Preliminary = preview
				w.Note = "已有初步样本，累计增加 2 个百分点后校准"
			}
			if incomplete > 0 {
				w.Note += fmt.Sprintf("；已保留已知消费，%d 次请求用量不完整", incomplete)
			}
			continue
		}
		w.Cost.Low += low
		w.Cost.High += high
		w.Delta += delta
		w.Tokens += totals.Tokens - b.Totals.Tokens
		w.Requests += totals.Requests - b.Totals.Requests
		w.Samples++
		w.CalibratedAt = now
		w.Previous = nil // The current cycle now provides its own estimate.
		w.Preliminary = nil
		w.IncompleteRequests += incomplete
		if b.Concurrent || concurrent {
			w.ConcurrentSamples++
		}
		w.Base = base
		w.Note = "已按本周期样本校准"
	}
	s.saveEstimatesLocked()
}

func (s *Service) estimateViewLocked(c Credential, value credentialUsage) *accountEstimate {
	out := &accountEstimate{Status: "waiting", Note: "首次成功查询套餐后开始采样", PricingRevision: pricingRevision, Windows: []estimateResult{}, PersistenceError: s.estimateWriteError}
	k := s.estimates.Keys[estimateKeyID(c)]
	if k == nil || k.Account == "" {
		return out
	}
	a := s.estimates.Accounts[k.Account]
	if a == nil {
		return out
	}
	if (value.accountHash != "" && value.accountHash != k.Account) || (value.planHash != "" && value.planHash != a.Plan) {
		out.Note = "账号或套餐已变化，等待新的用量采样"
		return out
	}
	for _, cred := range s.creds {
		if b := s.estimates.Keys[estimateKeyID(cred)]; b != nil && b.Account == k.Account && !s.revoked[cred.ID] {
			out.SharedCredentials++
		}
	}
	out.Status, out.Note = "sampling", "按全部额度消费均经 CPA 本插件记录估算；月总额度按周总额度的 2 倍折算"
	totals := s.estimateTotalsLocked(k.Account)
	now := time.Now()
	for _, kind := range []string{"five_hour", "weekly"} {
		result := estimateResult{Type: kind, Note: "等待有效用量窗口"}
		w := a.Windows[kind]
		if w != nil {
			if w.Base != nil {
				result.PendingRequests = totals.Requests - w.Base.Totals.Requests
				result.PendingPercent = w.LastPercent - w.Base.Percent
				result.PendingIncomplete = totals.Unknown - w.Base.Totals.Unknown
			}
			result.Note = w.Note
			matchingWindow := false
			for _, limit := range value.Limits {
				if limit.Type == kind && limit.ResetsAt != nil && sameEstimateReset(*limit.ResetsAt, w.Reset) {
					matchingWindow = true
				}
			}
			sample, historical := w.displaySample()
			result.Historical = sample != nil && (historical || !matchingWindow || !w.Reset.After(now))
			if sample != nil && sample.Samples > 0 && sample.Delta >= 1 && (sample.Preliminary || sample.Delta > float64(sample.Samples)) {
				result.Cost, result.Delta, result.Tokens, result.Requests, result.Samples, result.Updated = sample.Cost, sample.Delta, sample.Tokens, sample.Requests, sample.Samples, sample.Updated
				result.IncompleteRequests, result.ConcurrentSamples = sample.IncompleteRequests, sample.ConcurrentSamples
				result.Approximate = sample.IncompleteRequests > 0 || sample.ConcurrentSamples > 0
				result.Preliminary = sample.Preliminary
				if result.Historical {
					result.Note = "沿用上次有效估值，等待新周期样本；" + result.Note
				}
				if result.Approximate {
					result.Note += fmt.Sprintf("；近似估计：%d 次请求用量不完整，%d 段存在并发边界，结果可能受影响", sample.IncompleteRequests, sample.ConcurrentSamples)
				}
				if result.Preliminary {
					result.Note += "；初步估计：样本较少，数据可能不准"
				}
				// Use nominal consumption / observed change. Unresolved time-of-use
				// or cache-write TTL rates use the midpoint of their reference costs.
				amount := 100 * ((sample.Cost.Low + sample.Cost.High) / 2) / sample.Delta
				total := priceRange{amount, amount}
				if sample.Cost.Low != sample.Cost.High {
					result.Note += "；峰谷时段或缓存写入有效期未明确时按参考成本区间的均值估算"
				}
				result.Total = &total
				for _, limit := range value.Limits {
					if limit.Type == kind && limit.PercentUsed != nil && *limit.PercentUsed >= 0 && *limit.PercentUsed <= 100 && limit.ResetsAt != nil && limit.ResetsAt.After(now) && sameEstimateReset(*limit.ResetsAt, w.Reset) && value.Status == "ok" {
						remaining := 1 - *limit.PercentUsed/100
						if remaining < 0 {
							remaining = 0
						}
						if remaining > 1 {
							remaining = 1
						}
						result.Remaining = &priceRange{total.Low * remaining, total.High * remaining}
					}
				}
				out.Status = "estimated"
			}
		}
		out.Windows = append(out.Windows, result)
	}
	// Monthly capacity follows the same account's weekly estimate. Keep its
	// source sample metadata, but use the actual monthly percentage for remaining.
	// Old independently sampled monthly ledger entries are intentionally ignored.
	monthly := out.Windows[1]
	monthly.Type, monthly.DerivedFrom = "monthly", "weekly"
	monthly.Remaining = nil
	if monthly.Total != nil {
		amount := monthly.Total.Low * 2
		monthly.Total = &priceRange{amount, amount}
		monthly.Note = "月总额度按周总额度 × 2 估算，采样依据来自每周窗口；" + monthly.Note
		for _, limit := range value.Limits {
			if limit.Type == "monthly" && limit.PercentUsed != nil && *limit.PercentUsed >= 0 && *limit.PercentUsed <= 100 && limit.ResetsAt != nil && limit.ResetsAt.After(now) && value.Status == "ok" {
				remaining := amount * (1 - *limit.PercentUsed/100)
				monthly.Remaining = &priceRange{remaining, remaining}
			}
		}
	} else {
		monthly.Note = "等待周额度估值，月总额度按周总额度 × 2 估算"
	}
	out.Windows = append(out.Windows, monthly)
	return out
}
