package bridge

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 估算台账测试辅助
// ---------------------------------------------------------------------------

// estimateRegister 用指定 data_dir 注册一个服务，便于验证估算存档的跨进程行为。
func estimateRegister(t *testing.T, dir string) *Service {
	t.Helper()
	s := NewService()
	configYAML := fmt.Sprintf("data_dir: %q\n", filepath.ToSlash(dir))
	configYAML += "models:\n  - id: deepseek-flash\n    upstream_id: cline-pass/deepseek-v4.1-flash\n"
	if _, err := s.Handle("plugin.register", jsonBytes(map[string]any{"config_yaml": []byte(configYAML)})); err != nil {
		t.Fatalf("register plugin: %v", err)
	}
	return s
}

func estimateAddCredential(t *testing.T, s *Service, c Credential) {
	t.Helper()
	s.mu.Lock()
	s.creds[c.ID] = c
	s.mu.Unlock()
}

func estimateUsageValue(account, plan, kind string, percent float64, reset, at time.Time) credentialUsage {
	p, r, ts := percent, reset, at
	return credentialUsage{
		accountHash: account,
		planHash:    plan,
		Status:      "ok",
		UpdatedAt:   &ts,
		Limits:      []usageLimit{{Type: kind, PercentUsed: &p, ResetsAt: &r}},
	}
}

// estimateCalibrate 模拟一次成功额度查询后的标定调用。
func estimateCalibrate(s *Service, c Credential, value credentialUsage) {
	before := s.estimateActivitySnapshot()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calibrateEstimateLocked(c, value, before)
}

// 固定使用非节假日工作日峰时，让采样行为测试不依赖运行时钟。
func estimatePeakTime() time.Time {
	return time.Date(2026, time.November, 2, 2, 0, 0, 0, time.UTC)
}

// estimateRecordUsage 走真实 appendLog 路径记一笔可计价请求。
func estimateRecordUsage(t *testing.T, s *Service, c Credential, upstreamModel string, prompt, completion, cached, write int64) {
	t.Helper()
	key := s.startEstimateRequest(c)
	if key == "" {
		t.Fatalf("credential %q has no estimate key", c.ID)
	}
	s.appendLog(LogEntry{
		estimateKey:        key,
		CredentialID:       c.ID,
		UsageReported:      true,
		UpstreamModel:      upstreamModel,
		PromptTokens:       prompt,
		CompletionTokens:   completion,
		CachedTokens:       cached,
		CacheWriteTokens:   write,
		CacheWriteReported: write > 0,
		Status:             200,
		Time:               estimatePeakTime(),
		DurationMS:         1000,
		Attempts:           []Attempt{{Status: 200, Mode: "stream"}},
	})
}

func estimateRecordFailure(t *testing.T, s *Service, c Credential) {
	t.Helper()
	key := s.startEstimateRequest(c)
	s.appendLog(LogEntry{
		estimateKey:  key,
		CredentialID: c.ID,
		Status:       500,
		Attempts:     []Attempt{{Status: 500, Mode: "stream", Error: "upstream failed"}},
	})
}

func estimateWindowSnapshot(t *testing.T, s *Service, account, kind string) estimateWindow {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	a := s.estimates.Accounts[account]
	if a == nil {
		t.Fatalf("estimate account %q 不存在", account)
	}
	w := a.Windows[kind]
	if w == nil {
		t.Fatalf("estimate window %q 不存在", kind)
	}
	return *w
}

func estimateKeySnapshot(t *testing.T, s *Service, key string) estimateKey {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	k := s.estimates.Keys[key]
	if k == nil {
		t.Fatalf("estimate key %q 不存在", key)
	}
	return *k
}

func estimateTotalsOf(t *testing.T, s *Service, account string) estimateTotals {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.estimateTotalsLocked(account)
}

func estimateViewOf(t *testing.T, s *Service, c Credential, value credentialUsage) accountEstimate {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.estimateViewLocked(c, value)
	if out == nil {
		t.Fatal("estimate view is nil")
	}
	return *out
}

// estimateWindowResultByType 从视图结果里按窗口类型取出一条估算条目。
func estimateWindowResultByType(view accountEstimate, kind string) *estimateResult {
	for i := range view.Windows {
		if view.Windows[i].Type == kind {
			return &view.Windows[i]
		}
	}
	return nil
}

func estimateCloseTo(t *testing.T, got, want float64, label string) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > 1e-9*math.Max(1, math.Abs(want)) {
		t.Fatalf("%s = %v, 期望 %v", label, got, want)
	}
}

// estimateSingleValue 断言金额上下限已统一成同一个单值，防止估值退回区间语义。
func estimateSingleValue(t *testing.T, got priceRange, want float64, label string) {
	t.Helper()
	if got.Low != got.High {
		t.Fatalf("%s = %+v, 期望单值 %v", label, got, want)
	}
	estimateCloseTo(t, got.Low, want, label)
}

func estimatePricedEntry(upstreamModel string, prompt, completion, cached, write int64, writeReported bool) LogEntry {
	return LogEntry{
		UpstreamModel:      upstreamModel,
		UsageReported:      true,
		PromptTokens:       prompt,
		CompletionTokens:   completion,
		CachedTokens:       cached,
		CacheWriteTokens:   write,
		CacheWriteReported: writeReported,
		Status:             200,
		Attempts:           []Attempt{{Status: 200}},
	}
}

// ---------------------------------------------------------------------------
// 账本身份与归并
// ---------------------------------------------------------------------------

func TestEstimateAccountsWithSameLabelStayIsolated(t *testing.T) {
	s := registeredService(t, "")
	c1 := Credential{ID: "same-1", Type: Provider, APIKey: "key-account-a", Label: "同一标签"}
	c2 := Credential{ID: "same-2", Type: Provider, APIKey: "key-account-b", Label: "同一标签"}
	estimateAddCredential(t, s, c1)
	estimateAddCredential(t, s, c2)
	if estimateKeyID(c1) == estimateKeyID(c2) {
		t.Fatal("相同标签的不同 API Key 被合并成同一估算主体")
	}
	reset := time.Now().UTC().Add(240 * time.Hour)
	base := time.Now().UTC().Add(-2 * time.Hour)
	estimateCalibrate(s, c1, estimateUsageValue("account-a", "plan-a", "five_hour", 10, reset, base))
	estimateCalibrate(s, c2, estimateUsageValue("account-b", "plan-b", "five_hour", 10, reset, base))
	if got := estimateKeySnapshot(t, s, estimateKeyID(c1)).Account; got != "account-a" {
		t.Fatalf("c1 account = %q, 期望 account-a", got)
	}
	if got := estimateKeySnapshot(t, s, estimateKeyID(c2)).Account; got != "account-b" {
		t.Fatalf("c2 account = %q, 期望 account-b", got)
	}
	estimateRecordUsage(t, s, c1, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	if got := estimateTotalsOf(t, s, "account-a").Requests; got != 1 {
		t.Fatalf("account-a requests = %d, 期望 1", got)
	}
	if got := estimateTotalsOf(t, s, "account-b").Requests; got != 0 {
		t.Fatalf("account-b 被同标签账号的消费污染: requests = %d", got)
	}
	if view := estimateViewOf(t, s, c1, estimateUsageValue("account-a", "plan-a", "five_hour", 10, reset, base)); view.SharedCredentials != 1 {
		t.Fatalf("标签被当成账号身份: shared_credentials = %d, 期望 1", view.SharedCredentials)
	}
}

func TestEstimateSharedAccountAggregatesKeysAndRebaselinesOnNewBinding(t *testing.T) {
	s := registeredService(t, "")
	c1 := Credential{ID: "k1", Type: Provider, APIKey: "key-one", Label: "主账号"}
	c2 := Credential{ID: "k2", Type: Provider, APIKey: "key-two", Label: "主账号"}
	c3 := Credential{ID: "k3", Type: Provider, APIKey: "key-three", Label: "新绑定"}
	for _, c := range []Credential{c1, c2, c3} {
		estimateAddCredential(t, s, c)
	}
	reset := time.Now().UTC().Add(240 * time.Hour)
	t0 := time.Now().UTC().Add(-4 * time.Hour)
	perRequest := (200000*0.30 + 1000*1.20) / 1e6
	usage := func(percent float64, at time.Time) credentialUsage {
		return estimateUsageValue("account-shared", "plan-shared", "five_hour", percent, reset, at)
	}

	// 两个 key 先后认领同一账号：只重基线，不产生样本。
	estimateCalibrate(s, c1, usage(10, t0))
	estimateCalibrate(s, c2, usage(10, t0.Add(30*time.Minute)))
	for _, c := range []Credential{c1, c2} {
		if got := estimateKeySnapshot(t, s, estimateKeyID(c)).Account; got != "account-shared" {
			t.Fatalf("%s account = %q, 期望 account-shared", c.ID, got)
		}
	}
	estimateRecordUsage(t, s, c1, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	estimateRecordUsage(t, s, c2, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	if got := estimateTotalsOf(t, s, "account-shared").Requests; got != 2 {
		t.Fatalf("同账号多 key 未归并: requests = %d, 期望 2", got)
	}
	// 先完成一个可校准区间，让窗口里存在已累积样本。
	estimateCalibrate(s, c1, usage(12, t0.Add(time.Hour)))
	if got := estimateWindowSnapshot(t, s, "account-shared", "five_hour").Samples; got != 1 {
		t.Fatalf("区间样本未累积: samples = %d, 期望 1", got)
	}
	// 新绑定的同账号 key 只重基线，已接受估值必须保留。
	estimateCalibrate(s, c3, usage(12, t0.Add(2*time.Hour)))
	w := estimateWindowSnapshot(t, s, "account-shared", "five_hour")
	if w.Base == nil || w.Base.Percent != 12 || w.Base.Totals.Requests != 2 {
		t.Fatalf("新绑定基线错误: %+v", w.Base)
	}
	if w.Samples != 1 || w.Delta != 2 {
		t.Fatalf("新绑定丢失已接受样本: %+v", w)
	}
	estimateCloseTo(t, w.Cost.Low, 2*perRequest, "新绑定保留的已接受估值")

	// 视图继续展示已接受估值，并回到按新基线计算的待校准统计。
	view := estimateWindowResultByType(estimateViewOf(t, s, c3, usage(12, t0.Add(2*time.Hour))), "five_hour")
	if view == nil || view.Historical || view.Samples != 1 || view.Total == nil {
		t.Fatalf("新绑定后未展示已接受估值: %+v", view)
	}
	estimateCloseTo(t, view.Cost.Low, 2*perRequest, "新绑定后展示的估值")
	if view.PendingRequests != 0 || view.PendingPercent != 0 {
		t.Fatalf("新绑定后的基线未从当前合计重算: %+v", *view)
	}

	// 新 key 的后续消费从新基线累计，不能重复计入归并前的请求。
	estimateRecordUsage(t, s, c3, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	estimateCalibrate(s, c3, usage(14, t0.Add(3*time.Hour)))
	w = estimateWindowSnapshot(t, s, "account-shared", "five_hour")
	if w.Samples != 2 || w.Requests != 3 || w.Delta != 4 {
		t.Fatalf("新基线后的消费未正确累计: %+v", w)
	}
	estimateCloseTo(t, w.Cost.Low, 3*perRequest, "新基线后的累计估值")
}

// ---------------------------------------------------------------------------
// 采样区间
// ---------------------------------------------------------------------------

func TestEstimateAccumulatesConsumptionWhilePercentUnchanged(t *testing.T) {
	s := registeredService(t, "")
	c := Credential{ID: "acc-key", Type: Provider, APIKey: "key-acc", Label: "累计"}
	estimateAddCredential(t, s, c)
	reset := time.Now().UTC().Add(240 * time.Hour)
	t0 := time.Now().UTC().Add(-4 * time.Hour)
	usage := func(percent float64, at time.Time) credentialUsage {
		return estimateUsageValue("account-acc", "plan-acc", "five_hour", percent, reset, at)
	}
	perRequest := (200000*0.30 + 1000*1.20) / 1e6

	estimateCalibrate(s, c, usage(10, t0))
	estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	estimateCalibrate(s, c, usage(10, t0.Add(time.Hour)))
	if w := estimateWindowSnapshot(t, s, "account-acc", "five_hour"); w.Samples != 0 {
		t.Fatalf("百分比未变时不应产生样本: %+v", w)
	}
	estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	estimateCalibrate(s, c, usage(12, t0.Add(2*time.Hour)))
	w := estimateWindowSnapshot(t, s, "account-acc", "five_hour")
	estimateCloseTo(t, w.Cost.Low, 2*perRequest, "累计区间成本")
	estimateCloseTo(t, w.Cost.High, 2*perRequest, "累计区间成本上限")
	if w.Requests != 2 || w.Samples != 1 || w.Delta != 2 {
		t.Fatalf("百分比不变期间的消费被丢弃: %+v", w)
	}

	// 第二轮：百分比仍不变时继续消费，下一次上涨时也应被计入。
	estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	estimateCalibrate(s, c, usage(12, t0.Add(3*time.Hour)))
	estimateCalibrate(s, c, usage(14, t0.Add(3*time.Hour+30*time.Minute)))
	w = estimateWindowSnapshot(t, s, "account-acc", "five_hour")
	estimateCloseTo(t, w.Cost.Low, 3*perRequest, "二次累计区间成本")
	if w.Requests != 3 || w.Samples != 2 || w.Delta != 4 {
		t.Fatalf("第二轮百分比不变期间的消费被丢弃: %+v", w)
	}
}

func TestEstimateAccumulatesSuccessfulUsageWithIncompleteRequests(t *testing.T) {
	perRequest := (200000*0.30 + 1000*1.20) / 1e6
	cases := []struct {
		name string
		// suffix 只用于区分测试内的 API Key。
		suffix string
		record func(t *testing.T, s *Service, c Credential)
	}{
		{
			name:   "失败请求",
			suffix: "failed",
			record: func(t *testing.T, s *Service, c Credential) {
				estimateRecordFailure(t, s, c)
			},
		},
		{
			name:   "未计价模型",
			suffix: "unpriced",
			record: func(t *testing.T, s *Service, c Credential) {
				estimateRecordUsage(t, s, c, "cline-pass/mimo-v2.6", 200000, 1000, 0, 0)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := registeredService(t, "")
			c := Credential{ID: "unknown-key", Type: Provider, APIKey: "key-unknown-" + tc.suffix, Label: "缺失"}
			estimateAddCredential(t, s, c)
			reset := time.Now().UTC().Add(240 * time.Hour)
			t0 := time.Now().UTC().Add(-2 * time.Hour)
			account := "account-unknown-" + tc.suffix
			plan := "plan-unknown-" + tc.suffix
			usage := func(percent float64, at time.Time) credentialUsage {
				return estimateUsageValue(account, plan, "five_hour", percent, reset, at)
			}

			estimateCalibrate(s, c, usage(10, t0))
			// 同段内既有可计费消费也有缺失计价：成功消费继续累计，缺失单独计数。
			estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
			tc.record(t, s, c)
			estimateCalibrate(s, c, usage(13, t0.Add(time.Hour)))
			w := estimateWindowSnapshot(t, s, account, "five_hour")
			estimateCloseTo(t, w.Cost.Low, perRequest, "仅成功请求计入成本")
			estimateCloseTo(t, w.Cost.High, perRequest, "仅成功请求计入成本上限")
			if w.Samples != 1 || w.Requests != 1 || w.Delta != 3 {
				t.Fatalf("成功消费未与缺失请求同段累计: %+v", w)
			}
			if w.IncompleteRequests != 1 || w.ConcurrentSamples != 0 {
				t.Fatalf("缺失请求计数错误: %+v", w)
			}
			// 缺失计价不再重建当前 Base，缺口只记进新的基线合计。
			if w.Base == nil || w.Base.Percent != 13 || w.Base.Totals.Unknown != 1 {
				t.Fatalf("缺失计价时错误重建基线: %+v", w.Base)
			}

			// 后续成功消费继续累计，缺失次数不得被重复计算。
			estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
			estimateCalibrate(s, c, usage(15, t0.Add(2*time.Hour)))
			w = estimateWindowSnapshot(t, s, account, "five_hour")
			estimateCloseTo(t, w.Cost.Low, 2*perRequest, "后续成功消费累计")
			if w.Samples != 2 || w.Requests != 2 || w.Delta != 5 {
				t.Fatalf("后续成功消费未累计: %+v", w)
			}
			if w.IncompleteRequests != 1 || w.ConcurrentSamples != 0 {
				t.Fatalf("缺失请求被重复计数或误判并发: %+v", w)
			}

			result := estimateWindowResultByType(estimateViewOf(t, s, c, usage(15, t0.Add(2*time.Hour))), "five_hour")
			if result == nil {
				t.Fatalf("缺少 five_hour 估算结果: %+v", w)
			}
			if !result.Approximate || result.IncompleteRequests != 1 || result.PendingIncomplete != 0 {
				t.Fatalf("结果未按不完整请求标记近似: %+v", *result)
			}
			if result.ConcurrentSamples != 0 {
				t.Fatalf("无并发却标记了并发样本: %+v", *result)
			}
		})
	}
}

func TestEstimateKeepsIncompleteCountUntilSampleAccepted(t *testing.T) {
	s := registeredService(t, "")
	c := Credential{ID: "incomplete-key", Type: Provider, APIKey: "key-incomplete", Label: "缺失累计"}
	estimateAddCredential(t, s, c)
	reset := time.Now().UTC().Add(240 * time.Hour)
	t0 := time.Now().UTC().Add(-4 * time.Hour)
	usage := func(percent float64, at time.Time) credentialUsage {
		return estimateUsageValue("account-incomplete", "plan-incomplete", "five_hour", percent, reset, at)
	}
	perRequest := (200000*0.30 + 1000*1.20) / 1e6
	priced := func() {
		estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	}

	estimateCalibrate(s, c, usage(9, t0))
	priced()
	estimateRecordFailure(t, s, c)
	// 增量只有 1 个百分点：不更新 Base，缺失也不提前写入窗口。
	estimateCalibrate(s, c, usage(10, t0.Add(time.Hour)))
	w := estimateWindowSnapshot(t, s, "account-incomplete", "five_hour")
	if w.Base == nil || w.Base.Percent != 9 || w.Base.Totals.Unknown != 0 {
		t.Fatalf("不足 2 个百分点时更新了基线: %+v", w.Base)
	}
	if w.Samples != 0 || w.IncompleteRequests != 0 || w.Cost.Low != 0 {
		t.Fatalf("不足 2 个百分点时提前写出样本: %+v", w)
	}
	pending := estimateWindowResultByType(estimateViewOf(t, s, c, usage(10, t0.Add(time.Hour))), "five_hour")
	if pending == nil || pending.PendingIncomplete != 1 {
		t.Fatalf("待累计缺失请求计数错误: %+v", pending)
	}

	priced()
	// 累计到 2 个百分点：一次采样把同段成功消费与缺失次数一起计入。
	estimateCalibrate(s, c, usage(11, t0.Add(2*time.Hour)))
	w = estimateWindowSnapshot(t, s, "account-incomplete", "five_hour")
	estimateCloseTo(t, w.Cost.Low, 2*perRequest, "接受样本时的成功消费")
	if w.Samples != 1 || w.Requests != 2 || w.Delta != 2 || w.IncompleteRequests != 1 {
		t.Fatalf("累计采样结果错误: %+v", w)
	}

	priced()
	// 后续样本不得重复计入已经记录过的缺失次数。
	estimateCalibrate(s, c, usage(13, t0.Add(3*time.Hour)))
	w = estimateWindowSnapshot(t, s, "account-incomplete", "five_hour")
	estimateCloseTo(t, w.Cost.Low, 3*perRequest, "后续样本的成功消费")
	if w.Samples != 2 || w.Requests != 3 || w.IncompleteRequests != 1 {
		t.Fatalf("后续样本重复计数缺失请求: %+v", w)
	}
}

func TestEstimateIncompleteAndConcurrentStayPerAccount(t *testing.T) {
	s := registeredService(t, "")
	a := Credential{ID: "acct-a", Type: Provider, APIKey: "key-account-a", Label: "账号 A"}
	b := Credential{ID: "acct-b", Type: Provider, APIKey: "key-account-b", Label: "账号 B"}
	estimateAddCredential(t, s, a)
	estimateAddCredential(t, s, b)
	reset := time.Now().UTC().Add(240 * time.Hour)
	t0 := time.Now().UTC().Add(-2 * time.Hour)
	perRequest := (200000*0.30 + 1000*1.20) / 1e6
	usageA := func(percent float64, at time.Time) credentialUsage {
		return estimateUsageValue("account-a", "plan-a", "five_hour", percent, reset, at)
	}
	usageB := func(percent float64, at time.Time) credentialUsage {
		return estimateUsageValue("account-b", "plan-b", "five_hour", percent, reset, at)
	}

	// 账号 A：成功消费 + 失败请求，样本只应记录不完整。
	estimateCalibrate(s, a, usageA(10, t0))
	estimateRecordUsage(t, s, a, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	estimateRecordFailure(t, s, a)
	estimateCalibrate(s, a, usageA(13, t0.Add(time.Hour)))

	// 账号 B：只有成功消费，但采样时存在进行中的请求。
	estimateCalibrate(s, b, usageB(10, t0))
	estimateRecordUsage(t, s, b, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	inFlight := s.startEstimateRequest(b)
	estimateCalibrate(s, b, usageB(12, t0.Add(time.Hour)))

	wa := estimateWindowSnapshot(t, s, "account-a", "five_hour")
	estimateCloseTo(t, wa.Cost.Low, perRequest, "账号 A 成本")
	if wa.Samples != 1 || wa.IncompleteRequests != 1 || wa.ConcurrentSamples != 0 {
		t.Fatalf("账号 A 样本计数错误: %+v", wa)
	}
	wb := estimateWindowSnapshot(t, s, "account-b", "five_hour")
	estimateCloseTo(t, wb.Cost.Low, perRequest, "账号 B 成本")
	if wb.Samples != 1 || wb.IncompleteRequests != 0 || wb.ConcurrentSamples != 1 {
		t.Fatalf("账号 B 被账号 A 的缺失或并发污染: %+v", wb)
	}
	if wb.Base == nil || !wb.Base.Concurrent {
		t.Fatalf("账号 B 的新基线未记录并发: %+v", wb.Base)
	}

	resultA := estimateWindowResultByType(estimateViewOf(t, s, a, usageA(13, t0.Add(time.Hour))), "five_hour")
	if resultA == nil || !resultA.Approximate || resultA.IncompleteRequests != 1 || resultA.ConcurrentSamples != 0 {
		t.Fatalf("账号 A 的近似原因错误: %+v", resultA)
	}
	resultB := estimateWindowResultByType(estimateViewOf(t, s, b, usageB(12, t0.Add(time.Hour))), "five_hour")
	if resultB == nil || !resultB.Approximate || resultB.IncompleteRequests != 0 || resultB.ConcurrentSamples != 1 {
		t.Fatalf("账号 B 的近似原因错误: %+v", resultB)
	}

	// 收尾进行中的请求，避免残留活动计数影响后续断言。
	s.appendLog(LogEntry{
		estimateKey: inFlight, CredentialID: b.ID, Status: 200, UsageReported: true,
		UpstreamModel: "cline-pass/deepseek-v4.1-flash", PromptTokens: 1000,
		Attempts: []Attempt{{Status: 200}},
	})
}

func TestEstimateRestartsSamplingOnResetOrPercentDrop(t *testing.T) {
	s := registeredService(t, "")
	c := Credential{ID: "reset-key", Type: Provider, APIKey: "key-reset", Label: "重置"}
	estimateAddCredential(t, s, c)
	r1 := time.Now().UTC().Add(120 * time.Hour)
	r2 := time.Now().UTC().Add(240 * time.Hour)
	t0 := time.Now().UTC().Add(-2 * time.Hour)

	estimateCalibrate(s, c, estimateUsageValue("account-reset", "plan-reset", "five_hour", 10, r1, t0))
	estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	// reset 时间变化：窗口必须重建，旧周期消费不得算进新周期。
	estimateCalibrate(s, c, estimateUsageValue("account-reset", "plan-reset", "five_hour", 12, r2, t0.Add(time.Hour)))
	w := estimateWindowSnapshot(t, s, "account-reset", "five_hour")
	if !w.Reset.Equal(r2) || w.Base == nil || w.Base.Percent != 12 || w.Samples != 0 || w.Cost.Low != 0 || w.Cost.High != 0 {
		t.Fatalf("reset 后未重新采样: %+v", w)
	}
	// 同一 reset 窗口内百分比下降：同样视为重新采样。
	estimateCalibrate(s, c, estimateUsageValue("account-reset", "plan-reset", "five_hour", 5, r2, t0.Add(2*time.Hour)))
	w = estimateWindowSnapshot(t, s, "account-reset", "five_hour")
	if w.Base == nil || w.Base.Percent != 5 || w.Samples != 0 || w.Cost.Low != 0 || w.Delta != 0 {
		t.Fatalf("额度下降后未重新采样: %+v", w)
	}
}

func TestEstimateResetJitterKeepsPendingUntilSecondPoint(t *testing.T) {
	// 月额度不再独立采样：1/2 个百分点只适用于 five_hour 与 weekly，
	// 月视图的推导契约单独由 TestEstimateMonthlyDerivesFromWeekly 覆盖。
	for _, kind := range []string{"five_hour", "weekly"} {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			s := registeredService(t, "")
			c := Credential{ID: "jitter-" + kind, Type: Provider, APIKey: "key-jitter-" + kind, Label: "抖动"}
			estimateAddCredential(t, s, c)
			account, plan := "account-jitter", "plan-jitter"

			// 同一 reset 锚点被 Cline 重算到相邻秒，±2s 内不得重建窗口。
			resetAnchor := time.Now().UTC().Add(240 * time.Hour).Truncate(time.Second).Add(500 * time.Millisecond)
			resetAt10 := resetAnchor.Add(600 * time.Millisecond)
			resetAt11 := resetAnchor.Add(-600 * time.Millisecond)
			t0 := time.Now().UTC().Add(-4 * time.Hour)
			t1 := t0.Add(time.Hour)
			t2 := t0.Add(2 * time.Hour)
			usage := func(percent float64, reset, at time.Time) credentialUsage {
				return estimateUsageValue(account, plan, kind, percent, reset, at)
			}
			findResult := func(view accountEstimate) *estimateResult {
				for i := range view.Windows {
					if view.Windows[i].Type == kind {
						return &view.Windows[i]
					}
				}
				return nil
			}
			perRequest := (200000*0.30 + 1000*1.20) / 1e6

			estimateCalibrate(s, c, usage(9, resetAnchor, t0))
			estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
			estimateCalibrate(s, c, usage(10, resetAt10, t1))
			w := estimateWindowSnapshot(t, s, account, kind)
			if w.Samples != 0 || w.Cost.Low != 0 || w.Cost.High != 0 || w.LastPercent != 10 {
				t.Fatalf("reset 抖动被误判为新窗口或提前写出样本: %+v", w)
			}
			pending := findResult(estimateViewOf(t, s, c, usage(10, resetAt10, t1)))
			if pending == nil {
				t.Fatalf("缺少 %s 估算结果", kind)
			}
			if pending.PendingRequests != 1 || pending.PendingPercent != 1 {
				t.Fatalf("10%% pending 未保持: %+v", *pending)
			}
			// 1 个百分点只给初步总额：不写出正式样本，也不恢复 ±1 区间。
			if !pending.Preliminary || pending.Total == nil {
				t.Fatalf("10%% 未输出初步估计: %+v", *pending)
			}
			if pending.Historical {
				t.Fatalf("本周期初步估计被当成历史估值: %+v", *pending)
			}
			estimateSingleValue(t, *pending.Total, 100*perRequest, "10% 初步总额")

			estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
			estimateCalibrate(s, c, usage(11, resetAt11, t2))
			w = estimateWindowSnapshot(t, s, account, kind)
			if w.Samples != 1 || w.Delta != 2 || w.Requests != 2 {
				t.Fatalf("11%% 未形成样本: %+v", w)
			}
			estimateCloseTo(t, w.Cost.Low, 2*perRequest, "11% 样本成本")
			estimateCloseTo(t, w.Cost.High, 2*perRequest, "11% 样本成本上限")

			estimated := findResult(estimateViewOf(t, s, c, usage(11, resetAt11, t2)))
			if estimated == nil {
				t.Fatalf("缺少 %s 估算结果", kind)
			}
			if estimated.Total == nil || estimated.Remaining == nil {
				t.Fatalf("11%% 未输出 Total/Remaining: %+v", *estimated)
			}
			if estimated.Preliminary {
				t.Fatalf("11%% 正式样本仍标记为初步估计: %+v", *estimated)
			}
			estimateSingleValue(t, *estimated.Total, 100*perRequest, "总额度")
			estimateSingleValue(t, *estimated.Remaining, estimated.Total.Low*0.89, "剩余额度")
		})
	}
}

// 1 个百分点的初步估计不写出正式样本：随存档持久化，直到本周期攒够 2 个
// 百分点才被正式样本替换，并且同一段消费只计一次。
func TestEstimatePreliminaryPreviewPersistsUntilAcceptedSample(t *testing.T) {
	dir := t.TempDir()
	c := Credential{ID: "preview-key", Type: Provider, APIKey: "key-preview", Label: "初步"}
	account, plan := "account-preview", "plan-preview"
	base := time.Now().UTC()
	reset := base.Add(240 * time.Hour)
	t0 := base.Add(-3 * time.Hour)
	t1 := t0.Add(time.Hour)
	t1b := t1.Add(time.Minute)
	t2 := t1.Add(time.Hour)
	usage := func(percent float64, at time.Time) credentialUsage {
		return estimateUsageValue(account, plan, "five_hour", percent, reset, at)
	}
	perRequest := (200000*0.30 + 1000*1.20) / 1e6

	first := estimateRegister(t, dir)
	estimateAddCredential(t, first, c)
	estimateCalibrate(first, c, usage(10, t0))
	estimateRecordUsage(t, first, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	estimateCalibrate(first, c, usage(11, t1))

	preview := estimateWindowResultByType(estimateViewOf(t, first, c, usage(11, t1)), "five_hour")
	if preview == nil || !preview.Preliminary || preview.Total == nil {
		t.Fatalf("1 个百分点未输出初步估计: %+v", preview)
	}
	if preview.Historical || preview.Samples != 1 {
		t.Fatalf("初步估计状态异常: %+v", *preview)
	}
	if preview.PendingRequests != 1 || preview.PendingPercent != 1 {
		t.Fatalf("初步估计前的 pending 未按原基线保留: %+v", *preview)
	}
	estimateSingleValue(t, *preview.Total, 100*perRequest, "初步总额")

	// 初步快照随存档持久化：重启后没有正式估值时继续显示。
	second := estimateRegister(t, dir)
	estimateAddCredential(t, second, c)
	reloaded := estimateWindowResultByType(estimateViewOf(t, second, c, usage(11, t1)), "five_hour")
	if reloaded == nil || !reloaded.Preliminary || reloaded.Total == nil {
		t.Fatalf("重启后丢失初步估计: %+v", reloaded)
	}
	estimateSingleValue(t, *reloaded.Total, 100*perRequest, "重启后初步总额")

	// 重启后的第一次查询只能重建基线，本周期再攒够 2 个百分点才转正式样本。
	estimateCalibrate(second, c, usage(11, t1b))
	estimateRecordUsage(t, second, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	estimateCalibrate(second, c, usage(13, t2))
	accepted := estimateWindowResultByType(estimateViewOf(t, second, c, usage(13, t2)), "five_hour")
	if accepted == nil || accepted.Preliminary || accepted.Historical {
		t.Fatalf("2 个百分点未替换初步估计: %+v", accepted)
	}
	if accepted.Samples != 1 || accepted.Delta != 2 || accepted.Total == nil {
		t.Fatalf("正式样本未按单段累计: %+v", *accepted)
	}
	estimateCloseTo(t, accepted.Cost.Low, perRequest, "正式样本成本")
	estimateSingleValue(t, *accepted.Total, 100*perRequest/2, "正式总额")
}

// 展示优先级：本周期正式样本 > 历史正式估值 > 本周期初步值 > 历史初步值；
// 初步值按窗口与账号隔离，额度到达上限时保持旧的跳过逻辑。
func TestEstimatePreliminaryPriorityAndIsolation(t *testing.T) {
	perRequest := (200000*0.30 + 1000*1.20) / 1e6

	t.Run("正式历史估值优先于新周期初步值", func(t *testing.T) {
		s := registeredService(t, "")
		c := Credential{ID: "prefer-key", Type: Provider, APIKey: "key-prefer", Label: "优先"}
		estimateAddCredential(t, s, c)
		account, plan := "account-prefer", "plan-prefer"
		base := time.Now().UTC()
		r1 := base.Add(240 * time.Hour)
		r2 := base.Add(360 * time.Hour)
		t0 := base.Add(-4 * time.Hour)
		t1 := t0.Add(time.Hour)
		t2 := t0.Add(2 * time.Hour)
		t3 := t0.Add(3 * time.Hour)
		t4 := t0.Add(4 * time.Hour)
		usage := func(percent float64, reset, at time.Time) credentialUsage {
			return estimateUsageValue(account, plan, "five_hour", percent, reset, at)
		}

		// 第一周期攒出正式估值（>=2 个百分点）。
		estimateCalibrate(s, c, usage(10, r1, t0))
		estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
		estimateCalibrate(s, c, usage(12, r1, t1))
		if w := estimateWindowSnapshot(t, s, account, "five_hour"); w.Samples != 1 {
			t.Fatalf("前置正式样本未建立: %+v", w)
		}

		// 新周期只有 1 个百分点：继续展示正式历史估值，不被初步值降级。
		estimateCalibrate(s, c, usage(3, r2, t2))
		estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
		estimateCalibrate(s, c, usage(4, r2, t3))
		fallback := estimateWindowResultByType(estimateViewOf(t, s, c, usage(4, r2, t3)), "five_hour")
		if fallback == nil || fallback.Total == nil {
			t.Fatalf("缺少回退展示的历史估值: %+v", fallback)
		}
		if fallback.Preliminary || !fallback.Historical || fallback.Samples != 1 || fallback.Delta != 2 {
			t.Fatalf("新周期初步值压过正式历史估值: %+v", *fallback)
		}
		estimateSingleValue(t, *fallback.Total, 100*perRequest/2, "历史正式估值总额")

		// 新周期攒够 2 个百分点：替换为新的正式估值。
		estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 400000, 2000, 0, 0)
		estimateCalibrate(s, c, usage(5, r2, t4))
		replaced := estimateWindowResultByType(estimateViewOf(t, s, c, usage(5, r2, t4)), "five_hour")
		if replaced == nil || replaced.Preliminary || replaced.Historical {
			t.Fatalf("新周期正式样本未替换历史估值: %+v", replaced)
		}
		if replaced.Samples != 1 || replaced.Delta != 2 || replaced.Total == nil {
			t.Fatalf("新周期正式样本未按单段累计: %+v", *replaced)
		}
		newCost := perRequest + (400000*0.30+2000*1.20)/1e6
		estimateCloseTo(t, replaced.Cost.Low, newCost, "新周期正式样本成本")
		estimateSingleValue(t, *replaced.Total, 100*newCost/2, "新周期正式总额")
	})

	t.Run("初步历史可被新周期初步值替换", func(t *testing.T) {
		s := registeredService(t, "")
		c := Credential{ID: "preview-swap", Type: Provider, APIKey: "key-preview-swap", Label: "初步替换"}
		estimateAddCredential(t, s, c)
		account, plan := "account-preview-swap", "plan-preview-swap"
		base := time.Now().UTC()
		r1 := base.Add(240 * time.Hour)
		r2 := base.Add(360 * time.Hour)
		t0 := base.Add(-4 * time.Hour)
		t1 := t0.Add(time.Hour)
		t2 := t0.Add(2 * time.Hour)
		t3 := t0.Add(3 * time.Hour)
		usage := func(percent float64, reset, at time.Time) credentialUsage {
			return estimateUsageValue(account, plan, "five_hour", percent, reset, at)
		}

		// 第一周期只有 1 个百分点：留下初步快照。
		estimateCalibrate(s, c, usage(10, r1, t0))
		estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
		estimateCalibrate(s, c, usage(11, r1, t1))

		// 新周期没有新消费：继续显示上一周期的初步快照。
		estimateCalibrate(s, c, usage(3, r2, t2))
		kept := estimateWindowResultByType(estimateViewOf(t, s, c, usage(3, r2, t2)), "five_hour")
		if kept == nil || kept.Total == nil || !kept.Preliminary || !kept.Historical {
			t.Fatalf("新周期未继续显示初步快照: %+v", kept)
		}
		estimateSingleValue(t, *kept.Total, 100*perRequest, "跨周期初步总额")

		// 新周期出现 1 个百分点初步值：允许替换更早的初步快照。
		estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 400000, 2000, 0, 0)
		estimateCalibrate(s, c, usage(4, r2, t3))
		swapped := estimateWindowResultByType(estimateViewOf(t, s, c, usage(4, r2, t3)), "five_hour")
		if swapped == nil || !swapped.Preliminary || swapped.Historical {
			t.Fatalf("新周期初步值未替换旧初步快照: %+v", swapped)
		}
		newCost := (400000*0.30 + 2000*1.20) / 1e6
		estimateCloseTo(t, swapped.Cost.Low, newCost, "新周期初步样本成本")
		if swapped.Total == nil {
			t.Fatalf("新周期初步值缺少总额: %+v", *swapped)
		}
		estimateSingleValue(t, *swapped.Total, 100*newCost, "新周期初步总额")
	})

	t.Run("初步值按窗口与账号隔离", func(t *testing.T) {
		s := registeredService(t, "")
		c := Credential{ID: "isolate-key", Type: Provider, APIKey: "key-isolate", Label: "隔离"}
		estimateAddCredential(t, s, c)
		other := Credential{ID: "isolate-other", Type: Provider, APIKey: "key-isolate-other", Label: "隔离其他"}
		estimateAddCredential(t, s, other)
		account, plan := "account-isolate", "plan-isolate"
		reset := time.Now().UTC().Add(240 * time.Hour)
		t0 := time.Now().UTC().Add(-3 * time.Hour)
		t1 := t0.Add(30 * time.Minute)
		t2 := t0.Add(time.Hour)
		value := func(five, weekly float64, at time.Time) credentialUsage {
			pf, pw := five, weekly
			rf, rw := reset, reset
			return credentialUsage{
				accountHash: account, planHash: plan, Status: "ok", UpdatedAt: &at,
				Limits: []usageLimit{
					{Type: "five_hour", PercentUsed: &pf, ResetsAt: &rf},
					{Type: "weekly", PercentUsed: &pw, ResetsAt: &rw},
				},
			}
		}

		// 百分比上升但没有计价消费：不写初步值。
		estimateCalibrate(s, c, value(10, 20, t0))
		estimateCalibrate(s, c, value(11, 20, t1))
		if got := estimateWindowResultByType(estimateViewOf(t, s, c, value(11, 20, t1)), "five_hour"); got == nil || got.Total != nil || got.Preliminary {
			t.Fatalf("无计价消费仍写出初步值: %+v", got)
		}

		// 有计价消费后只有 five_hour 出现初步值，weekly 不受影响。
		estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
		estimateCalibrate(s, c, value(12, 20, t2))
		view := estimateViewOf(t, s, c, value(12, 20, t2))
		five := estimateWindowResultByType(view, "five_hour")
		if five == nil || !five.Preliminary || five.Total == nil {
			t.Fatalf("five_hour 未按 1 个百分点输出初步值: %+v", five)
		}
		estimateSingleValue(t, *five.Total, 100*perRequest, "five_hour 初步总额")
		if weekly := estimateWindowResultByType(view, "weekly"); weekly == nil || weekly.Total != nil || weekly.Preliminary {
			t.Fatalf("weekly 被 five_hour 的初步值污染: %+v", weekly)
		}

		// 另一个账号只建立基线，不继承任何初步值。
		estimateCalibrate(s, other, estimateUsageValue("account-isolate-other", "plan-isolate-other", "five_hour", 10, reset, t2))
		leaked := estimateWindowResultByType(estimateViewOf(t, s, other, estimateUsageValue("account-isolate-other", "plan-isolate-other", "five_hour", 10, reset, t2)), "five_hour")
		if leaked == nil || leaked.Total != nil || leaked.Preliminary {
			t.Fatalf("其他账号继承了初步值: %+v", leaked)
		}
	})

	t.Run("到达上限跳过截断增量", func(t *testing.T) {
		s := registeredService(t, "")
		c := Credential{ID: "cap-key", Type: Provider, APIKey: "key-cap", Label: "上限"}
		estimateAddCredential(t, s, c)
		account, plan := "account-cap", "plan-cap"
		reset := time.Now().UTC().Add(240 * time.Hour)
		t0 := time.Now().UTC().Add(-3 * time.Hour)
		t1 := t0.Add(time.Hour)
		t2 := t0.Add(2 * time.Hour)
		usage := func(percent float64, at time.Time) credentialUsage {
			return estimateUsageValue(account, plan, "five_hour", percent, reset, at)
		}

		estimateCalibrate(s, c, usage(10, t0))
		estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
		estimateCalibrate(s, c, usage(11, t1))
		estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
		estimateCalibrate(s, c, usage(100, t2))
		w := estimateWindowSnapshot(t, s, account, "five_hour")
		if w.Samples != 0 || w.Base == nil || w.Base.Percent != 100 {
			t.Fatalf("到达上限时写出了截断样本: %+v", w)
		}
	})
}

func TestEstimateResetDriftBeyondAnchorRestartsSampling(t *testing.T) {
	t.Run("reset 漂移超过容差", func(t *testing.T) {
		s := registeredService(t, "")
		c := Credential{ID: "drift-key", Type: Provider, APIKey: "key-drift", Label: "漂移"}
		estimateAddCredential(t, s, c)
		account, plan := "account-drift", "plan-drift"
		resetAnchor := time.Now().UTC().Add(240 * time.Hour).Truncate(time.Second).Add(500 * time.Millisecond)
		t0 := time.Now().UTC().Add(-4 * time.Hour)
		t1 := t0.Add(time.Hour)
		t2 := t0.Add(2 * time.Hour)
		usage := func(percent float64, reset, at time.Time) credentialUsage {
			return estimateUsageValue(account, plan, "five_hour", percent, reset, at)
		}

		estimateCalibrate(s, c, usage(9, resetAnchor, t0))
		estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
		estimateCalibrate(s, c, usage(11, resetAnchor.Add(300*time.Millisecond), t1))
		w := estimateWindowSnapshot(t, s, account, "five_hour")
		if w.Samples != 1 || w.Cost.Low <= 0 {
			t.Fatalf("重建前样本未保留: %+v", w)
		}

		// 容差只围绕原始 reset 锚点；累计漂移超过 2s 必须重建而不是继续并样本。
		drifted := resetAnchor.Add(3 * time.Second)
		estimateCalibrate(s, c, usage(12, drifted, t2))
		w = estimateWindowSnapshot(t, s, account, "five_hour")
		if !w.Reset.Equal(drifted) || w.Base == nil || w.Base.Percent != 12 || w.LastPercent != 12 || w.Samples != 0 || w.Cost.Low != 0 || w.Cost.High != 0 || w.Delta != 0 {
			t.Fatalf("reset 漂移超过 2s 后未重建或继承了旧样本: %+v", w)
		}
	})

	t.Run("过期窗口仍重建", func(t *testing.T) {
		s := registeredService(t, "")
		c := Credential{ID: "expired-key", Type: Provider, APIKey: "key-expired", Label: "过期"}
		estimateAddCredential(t, s, c)
		account, plan := "account-expired", "plan-expired"
		start := time.Now().UTC()
		anchor := start.Add(time.Hour).Truncate(time.Second).Add(500 * time.Millisecond)
		t0 := start.Add(-2 * time.Hour)
		t1 := start.Add(-time.Hour)
		usage := func(percent float64, reset, at time.Time) credentialUsage {
			return estimateUsageValue(account, plan, "five_hour", percent, reset, at)
		}

		estimateCalibrate(s, c, usage(9, anchor, t0))
		estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
		estimateCalibrate(s, c, usage(11, anchor, t1))
		w := estimateWindowSnapshot(t, s, account, "five_hour")
		if w.Samples != 1 {
			t.Fatalf("过期场景前置样本未建立: %+v", w)
		}

		expiredAt := anchor.Add(200 * time.Millisecond)
		nextReset := expiredAt.Add(time.Second)
		estimateCalibrate(s, c, usage(12, nextReset, expiredAt))
		w = estimateWindowSnapshot(t, s, account, "five_hour")
		if !w.Reset.Equal(nextReset) || w.Base == nil || w.Base.Percent != 12 || w.Samples != 0 || w.Cost.Low != 0 {
			t.Fatalf("过期窗口未按旧行为重建: %+v", w)
		}
	})

	t.Run("百分比下降仍重建", func(t *testing.T) {
		s := registeredService(t, "")
		c := Credential{ID: "drop-key", Type: Provider, APIKey: "key-drop", Label: "下降"}
		estimateAddCredential(t, s, c)
		account, plan := "account-drop", "plan-drop"
		reset := time.Now().UTC().Add(240 * time.Hour).Truncate(time.Second).Add(500 * time.Millisecond)
		t0 := time.Now().UTC().Add(-4 * time.Hour)
		t1 := t0.Add(time.Hour)
		t2 := t0.Add(2 * time.Hour)
		usage := func(percent float64, at time.Time) credentialUsage {
			return estimateUsageValue(account, plan, "five_hour", percent, reset, at)
		}

		estimateCalibrate(s, c, usage(9, t0))
		estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
		estimateCalibrate(s, c, usage(11, t1))
		w := estimateWindowSnapshot(t, s, account, "five_hour")
		if w.Samples != 1 {
			t.Fatalf("下降场景前置样本未建立: %+v", w)
		}

		estimateCalibrate(s, c, usage(5, t2))
		w = estimateWindowSnapshot(t, s, account, "five_hour")
		if !w.Reset.Equal(reset) || w.Base == nil || w.Base.Percent != 5 || w.LastPercent != 5 || w.Samples != 0 || w.Cost.Low != 0 || w.Delta != 0 {
			t.Fatalf("百分比下降未按旧行为重建: %+v", w)
		}
	})
}

func TestEstimateRestartDoesNotBridgeGap(t *testing.T) {
	dir := t.TempDir()
	c := Credential{ID: "persist-key", Type: Provider, APIKey: "key-persist", Label: "持久化"}
	reset := time.Now().UTC().Add(240 * time.Hour)
	t0 := time.Now().UTC().Add(-3 * time.Hour)

	first := estimateRegister(t, dir)
	estimateAddCredential(t, first, c)
	estimateCalibrate(first, c, estimateUsageValue("account-persist", "plan-persist", "five_hour", 10, reset, t0))
	estimateRecordUsage(t, first, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	estimateCalibrate(first, c, estimateUsageValue("account-persist", "plan-persist", "five_hour", 12, reset, t0.Add(time.Hour)))
	saved := estimateWindowSnapshot(t, first, "account-persist", "five_hour")
	if saved.Samples != 1 || saved.Base == nil || saved.Cost.Low <= 0 {
		t.Fatalf("重启前样本未建立: %+v", saved)
	}

	second := estimateRegister(t, dir)
	estimateAddCredential(t, second, c)
	loaded := estimateWindowSnapshot(t, second, "account-persist", "five_hour")
	if loaded.Base != nil {
		t.Fatalf("重启后仍保留跨进程基线: %+v", loaded.Base)
	}
	if loaded.Samples != saved.Samples || loaded.Cost.Low != saved.Cost.Low {
		t.Fatalf("重启丢失历史样本: %+v", loaded)
	}
	// 重启后的第一次校准只能重新建立基线，不能补记 gap 内消费。
	estimateCalibrate(second, c, estimateUsageValue("account-persist", "plan-persist", "five_hour", 13, reset, t0.Add(2*time.Hour)))
	after := estimateWindowSnapshot(t, second, "account-persist", "five_hour")
	if after.Base == nil || after.Base.Percent != 13 {
		t.Fatalf("重启后未重新基线: %+v", after.Base)
	}
	if after.Samples != saved.Samples || after.Cost.Low != saved.Cost.Low {
		t.Fatalf("重启 gap 被错误补记: %+v", after)
	}
}

func TestEstimateMarksConcurrentSamplesWhileRequestsOrQueriesInFlight(t *testing.T) {
	reset := time.Now().UTC().Add(240 * time.Hour)
	t0 := time.Now().UTC().Add(-2 * time.Hour)
	perRequest := (200000*0.30 + 1000*1.20) / 1e6
	usage := func(percent float64, at time.Time) credentialUsage {
		return estimateUsageValue("account-busy", "plan-busy", "five_hour", percent, reset, at)
	}
	newBusyService := func(t *testing.T, suffix string) (*Service, Credential) {
		t.Helper()
		s := registeredService(t, "")
		c := Credential{ID: "busy-key-" + suffix, Type: Provider, APIKey: "key-busy-" + suffix, Label: "忙"}
		estimateAddCredential(t, s, c)
		return s, c
	}
	finish := func(t *testing.T, s *Service, c Credential, key string) {
		t.Helper()
		s.appendLog(LogEntry{
			estimateKey: key, CredentialID: c.ID, Status: 200, UsageReported: true,
			UpstreamModel: "cline-pass/deepseek-v4.1-flash", PromptTokens: 200000, CompletionTokens: 1000,
			Time: estimatePeakTime(), DurationMS: 1000,
			Attempts: []Attempt{{Status: 200}},
		})
	}

	t.Run("活动请求期间等待用量", func(t *testing.T) {
		s, c := newBusyService(t, "wait")
		estimateCalibrate(s, c, usage(10, t0))
		inFlight := s.startEstimateRequest(c)
		// 查询期间有进行中的请求且尚无消费：不重建 Base，也不写出样本。
		estimateCalibrate(s, c, usage(30, t0.Add(time.Hour)))
		w := estimateWindowSnapshot(t, s, "account-busy", "five_hour")
		if w.Base == nil || w.Base.Percent != 10 || w.Base.Totals.Unknown != 0 {
			t.Fatalf("等待用量期间错误重建基线: %+v", w.Base)
		}
		if w.LastPercent != 30 || w.Samples != 0 || w.Delta != 0 {
			t.Fatalf("尚无消费时写出了样本: %+v", w)
		}
		// 请求返回用量后，下一次校准仍按 >=2 个百分点正常采样。
		finish(t, s, c, inFlight)
		estimateCalibrate(s, c, usage(31, t0.Add(2*time.Hour)))
		w = estimateWindowSnapshot(t, s, "account-busy", "five_hour")
		estimateCloseTo(t, w.Cost.Low, perRequest, "等待用量后的成功消费")
		if w.Samples != 1 || w.ConcurrentSamples != 0 {
			t.Fatalf("请求返回后仍被标记并发: %+v", w)
		}
	})

	t.Run("采样时存在进行中请求", func(t *testing.T) {
		s, c := newBusyService(t, "inflight")
		estimateCalibrate(s, c, usage(10, t0))
		estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
		inFlight := s.startEstimateRequest(c)
		estimateCalibrate(s, c, usage(12, t0.Add(time.Hour)))
		w := estimateWindowSnapshot(t, s, "account-busy", "five_hour")
		estimateCloseTo(t, w.Cost.Low, perRequest, "并发样本仍累计成功消费")
		if w.Samples != 1 || w.ConcurrentSamples != 1 || w.IncompleteRequests != 0 {
			t.Fatalf("进行中的请求未被标记并发: %+v", w)
		}
		if w.Base == nil || !w.Base.Concurrent {
			t.Fatalf("新基线未记录采样期间的并发: %+v", w.Base)
		}
		result := estimateWindowResultByType(estimateViewOf(t, s, c, usage(12, t0.Add(time.Hour))), "five_hour")
		if result == nil || !result.Approximate || result.ConcurrentSamples != 1 || result.IncompleteRequests != 0 {
			t.Fatalf("并发样本未标记近似: %+v", result)
		}
		finish(t, s, c, inFlight)
	})

	t.Run("基线期间存在进行中请求", func(t *testing.T) {
		s, c := newBusyService(t, "basebusy")
		inFlight := s.startEstimateRequest(c)
		estimateCalibrate(s, c, usage(10, t0))
		w := estimateWindowSnapshot(t, s, "account-busy", "five_hour")
		if w.Base == nil || !w.Base.Concurrent {
			t.Fatalf("基线未记录查询期间的请求: %+v", w.Base)
		}
		finish(t, s, c, inFlight)
		estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
		estimateCalibrate(s, c, usage(12, t0.Add(time.Hour)))
		w = estimateWindowSnapshot(t, s, "account-busy", "five_hour")
		if w.Samples != 1 || w.ConcurrentSamples != 1 {
			t.Fatalf("基线期间的并发未传播到后续样本: %+v", w)
		}
	})

	t.Run("查询期间请求完成", func(t *testing.T) {
		s, c := newBusyService(t, "revision")
		estimateCalibrate(s, c, usage(10, t0))
		before := s.estimateActivitySnapshot()
		key := s.startEstimateRequest(c)
		// 请求在额度查询进行中完成：活动数归零但 revision 前进。
		finish(t, s, c, key)
		s.mu.Lock()
		s.calibrateEstimateLocked(c, usage(12, t0.Add(time.Hour)), before)
		s.mu.Unlock()
		w := estimateWindowSnapshot(t, s, "account-busy", "five_hour")
		if w.Samples != 1 || w.ConcurrentSamples != 1 || w.IncompleteRequests != 0 {
			t.Fatalf("查询期间完成的请求未标记并发: %+v", w)
		}
	})
}

// ---------------------------------------------------------------------------
// 跨周期保留
// ---------------------------------------------------------------------------

// 真实 reset 或百分比下降只重启本周期采样；上一轮有效估值继续用于展示，
// 直到新周期产生有效样本，期间可以跨过多个没有用量的周期。
func TestEstimateHistoricalEstimateSurvivesEmptyCyclesUntilNewSample(t *testing.T) {
	s := registeredService(t, "")
	c := Credential{ID: "history-key", Type: Provider, APIKey: "key-history", Label: "跨周期"}
	estimateAddCredential(t, s, c)
	account, plan := "account-history", "plan-history"
	base := time.Now().UTC()
	r1 := base.Add(240 * time.Hour)
	r2 := base.Add(360 * time.Hour)
	r3 := base.Add(480 * time.Hour)
	t0 := base.Add(-2 * time.Hour)
	t1 := t0.Add(time.Hour)
	t2 := t0.Add(2 * time.Hour)
	t3 := t0.Add(3 * time.Hour)
	t4 := t0.Add(4 * time.Hour)
	usage := func(percent float64, reset, at time.Time) credentialUsage {
		return estimateUsageValue(account, plan, "five_hour", percent, reset, at)
	}
	perRequest := (200000*0.30 + 1000*1.20) / 1e6

	// 第一周期攒出一个可用估值：本周期显示，并给出剩余额度。
	estimateCalibrate(s, c, usage(10, r1, t0))
	estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	estimateCalibrate(s, c, usage(12, r1, t1))
	current := estimateWindowResultByType(estimateViewOf(t, s, c, usage(12, r1, t1)), "five_hour")
	if current == nil || current.Historical || current.Samples != 1 || current.Remaining == nil {
		t.Fatalf("第一周期未显示本周期估值与剩余额度: %+v", current)
	}
	estimateCloseTo(t, current.Cost.Low, perRequest, "第一周期估值")

	// 进入新周期：本周期样本归零，Pending 属于新周期，展示回退到旧估值。
	estimateCalibrate(s, c, usage(4, r2, t2))
	w := estimateWindowSnapshot(t, s, account, "five_hour")
	if !w.Reset.Equal(r2) || w.Samples != 0 || w.Cost.Low != 0 || w.Cost.High != 0 {
		t.Fatalf("真实 reset 后本周期样本未归零: %+v", w)
	}
	estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	empty := estimateWindowResultByType(estimateViewOf(t, s, c, usage(4, r2, t2)), "five_hour")
	if empty == nil || !empty.Historical || empty.Samples != 1 {
		t.Fatalf("空周期未回退展示上一轮估值: %+v", empty)
	}
	estimateCloseTo(t, empty.Cost.Low, perRequest, "空周期回退估值")
	if empty.Total == nil {
		t.Fatalf("空周期未保留可展示的估值总额: %+v", *empty)
	}
	if empty.PendingRequests != 1 || empty.PendingPercent != 0 {
		t.Fatalf("Pending 未按新周期基线计算: %+v", *empty)
	}

	// 连续第三个没有用量的周期：回退估值仍然保留。
	estimateCalibrate(s, c, usage(2, r3, t3))
	empty = estimateWindowResultByType(estimateViewOf(t, s, c, usage(2, r3, t3)), "five_hour")
	if empty == nil || !empty.Historical || empty.Samples != 1 {
		t.Fatalf("跨多个空周期丢失历史估值: %+v", empty)
	}
	estimateCloseTo(t, empty.Cost.Low, perRequest, "跨空周期保留的估值")

	// 新周期产生有效样本后必须换成新周期估值。
	estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 400000, 2000, 0, 0)
	estimateCalibrate(s, c, usage(4, r3, t4))
	replaced := estimateWindowResultByType(estimateViewOf(t, s, c, usage(4, r3, t4)), "five_hour")
	if replaced == nil || replaced.Historical || replaced.Samples != 1 {
		t.Fatalf("新周期有效样本未替换历史回退: %+v", replaced)
	}
	estimateCloseTo(t, replaced.Cost.Low, 2*perRequest, "新周期估值")
	if replaced.Total == nil || replaced.Remaining == nil {
		t.Fatalf("新周期有效样本缺少总额或剩余额度: %+v", *replaced)
	}
	estimateSingleValue(t, *replaced.Remaining, replaced.Total.Low*0.96, "新周期剩余额度")
}

// 旧 reset 已到期但后台还没取到新周期数据时继续展示旧估值；过期百分比不
// 能冒充新周期的剩余额度。
func TestEstimateExpiredCycleKeepsEstimateButDropsRemaining(t *testing.T) {
	s := registeredService(t, "")
	c := Credential{ID: "expired-key", Type: Provider, APIKey: "key-expired", Label: "过期窗口"}
	estimateAddCredential(t, s, c)
	account, plan := "account-expired", "plan-expired"
	base := time.Now().UTC()
	reset := base.Add(-time.Hour) // 采样时仍在窗口内，展示时已过期。
	t0 := base.Add(-3 * time.Hour)
	t1 := base.Add(-2 * time.Hour)
	t2 := base.Add(-90 * time.Minute)
	usage := func(percent float64, at time.Time) credentialUsage {
		return estimateUsageValue(account, plan, "five_hour", percent, reset, at)
	}
	perRequest := (200000*0.30 + 1000*1.20) / 1e6

	estimateCalibrate(s, c, usage(10, t0))
	estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	estimateCalibrate(s, c, usage(12, t1))
	if w := estimateWindowSnapshot(t, s, account, "five_hour"); w.Samples != 1 {
		t.Fatalf("过期场景前置样本未建立: %+v", w)
	}

	view := estimateWindowResultByType(estimateViewOf(t, s, c, usage(12, t2)), "five_hour")
	if view == nil {
		t.Fatal("缺少 five_hour 估算结果")
	}
	if !view.Historical {
		t.Fatalf("过期周期仍被当作本周期估值: %+v", *view)
	}
	if view.Total == nil || view.Samples != 1 {
		t.Fatalf("过期周期丢失可展示的旧估值: %+v", *view)
	}
	estimateCloseTo(t, view.Cost.Low, perRequest, "过期周期展示的旧估值")
	if view.Remaining != nil {
		t.Fatalf("过期百分比被当成剩余额度: %+v", *view.Remaining)
	}
}

// 重启后 Base 清空，但已接受的本周期样本与跨周期回退估值都要保留。
func TestEstimateReloadKeepsAcceptedAndHistoricalEstimates(t *testing.T) {
	dir := t.TempDir()
	c := Credential{ID: "reload-key", Type: Provider, APIKey: "key-reload", Label: "重启保留"}
	account, plan := "account-reload", "plan-reload"
	base := time.Now().UTC()
	r1 := base.Add(240 * time.Hour)
	r2 := base.Add(360 * time.Hour)
	t0 := base.Add(-3 * time.Hour)
	t1 := t0.Add(time.Hour)
	t2 := t1.Add(30 * time.Minute)
	t3 := t2.Add(30 * time.Minute)
	t4 := t3.Add(30 * time.Minute)
	usage := func(percent float64, reset, at time.Time) credentialUsage {
		return estimateUsageValue(account, plan, "five_hour", percent, reset, at)
	}
	perRequest := (200000*0.30 + 1000*1.20) / 1e6

	first := estimateRegister(t, dir)
	estimateAddCredential(t, first, c)
	estimateCalibrate(first, c, usage(10, r1, t0))
	estimateRecordUsage(t, first, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	estimateCalibrate(first, c, usage(12, r1, t1))

	// 重启：Base 清空，已接受样本保留。
	second := estimateRegister(t, dir)
	estimateAddCredential(t, second, c)
	loaded := estimateWindowSnapshot(t, second, account, "five_hour")
	if loaded.Base != nil {
		t.Fatalf("重启后未清空 Base: %+v", loaded.Base)
	}
	if loaded.Samples != 1 || loaded.Delta != 2 {
		t.Fatalf("重启丢失已接受样本: %+v", loaded)
	}
	estimateCloseTo(t, loaded.Cost.Low, perRequest, "重启保留的已接受估值")
	accepted := estimateWindowResultByType(estimateViewOf(t, second, c, usage(12, r1, t1)), "five_hour")
	if accepted == nil || accepted.Historical || accepted.Samples != 1 {
		t.Fatalf("重启后未展示已接受估值: %+v", accepted)
	}
	estimateCloseTo(t, accepted.Cost.Low, perRequest, "重启后展示的已接受估值")

	// 旧 reset 到期进入新周期：旧估值转为回退展示，再次重启仍然保留。
	estimateCalibrate(second, c, usage(3, r2, t2))
	fallback := estimateWindowResultByType(estimateViewOf(t, second, c, usage(3, r2, t2)), "five_hour")
	if fallback == nil || !fallback.Historical || fallback.Samples != 1 {
		t.Fatalf("新周期未回退展示已接受估值: %+v", fallback)
	}

	third := estimateRegister(t, dir)
	estimateAddCredential(t, third, c)
	reloaded := estimateWindowResultByType(estimateViewOf(t, third, c, usage(3, r2, t2)), "five_hour")
	if reloaded == nil || !reloaded.Historical || reloaded.Samples != 1 {
		t.Fatalf("重启丢失跨周期回退估值: %+v", reloaded)
	}
	estimateCloseTo(t, reloaded.Cost.Low, perRequest, "重启后保留的回退估值")

	// 重启后新周期仍能攒出新样本并替换回退估值。
	estimateCalibrate(third, c, usage(3, r2, t3))
	estimateRecordUsage(t, third, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	estimateCalibrate(third, c, usage(5, r2, t4))
	fresh := estimateWindowResultByType(estimateViewOf(t, third, c, usage(5, r2, t4)), "five_hour")
	if fresh == nil || fresh.Historical || fresh.Samples != 1 {
		t.Fatalf("重启后新周期样本未替换回退估值: %+v", fresh)
	}
}

// 三个额度窗口各自维护本周期与回退估值；账号或套餐切换不得串值。
func TestEstimateHistoryStaysPerWindowAndPerAccount(t *testing.T) {
	t.Run("三窗口各自隔离", func(t *testing.T) {
		s := registeredService(t, "")
		c := Credential{ID: "windows-key", Type: Provider, APIKey: "key-windows", Label: "三窗口"}
		estimateAddCredential(t, s, c)
		account, plan := "account-windows", "plan-windows"
		base := time.Now().UTC()
		resetFive := base.Add(240 * time.Hour)
		resetFiveNext := base.Add(500 * time.Hour)
		resetWeekly := base.Add(300 * time.Hour)
		resetMonthly := base.Add(400 * time.Hour)
		t0 := base.Add(-2 * time.Hour)
		t1 := t0.Add(time.Hour)
		t2 := t0.Add(2 * time.Hour)
		usage := func(five, weekly, monthly float64, fiveReset time.Time, at time.Time) credentialUsage {
			f, w, m := five, weekly, monthly
			rf, rw, rm := fiveReset, resetWeekly, resetMonthly
			return credentialUsage{
				accountHash: account,
				planHash:    plan,
				Status:      "ok",
				UpdatedAt:   &at,
				Limits: []usageLimit{
					{Type: "five_hour", PercentUsed: &f, ResetsAt: &rf},
					{Type: "weekly", PercentUsed: &w, ResetsAt: &rw},
					{Type: "monthly", PercentUsed: &m, ResetsAt: &rm},
				},
			}
		}
		perRequest := (200000*0.30 + 1000*1.20) / 1e6

		estimateCalibrate(s, c, usage(10, 20, 30, resetFive, t0))
		estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
		estimateCalibrate(s, c, usage(12, 22, 32, resetFive, t1))
		view := estimateViewOf(t, s, c, usage(12, 22, 32, resetFive, t1))
		for _, kind := range []string{"five_hour", "weekly"} {
			r := estimateWindowResultByType(view, kind)
			if r == nil || r.Historical || r.Samples != 1 || r.DerivedFrom != "" {
				t.Fatalf("%s 初始样本错误: %+v", kind, r)
			}
			estimateCloseTo(t, r.Cost.Low, perRequest, kind+" 初始估值")
		}
		// 月视图复制周结果的 metadata，并按同一周估值 × 2 输出单值总额。
		monthly := estimateWindowResultByType(view, "monthly")
		weekly := estimateWindowResultByType(view, "weekly")
		if monthly == nil || monthly.Historical || monthly.Samples != 1 || monthly.DerivedFrom != "weekly" {
			t.Fatalf("monthly 初始样本错误: %+v", monthly)
		}
		estimateCloseTo(t, monthly.Cost.Low, perRequest, "monthly 初始估值")
		if weekly == nil || weekly.Total == nil || monthly.Total == nil {
			t.Fatalf("初始周/月总额缺失: weekly=%+v monthly=%+v", weekly, monthly)
		}
		estimateSingleValue(t, *monthly.Total, 2*weekly.Total.Low, "初始月总额度")

		// 只有 five_hour 进入新周期，weekly/monthly 仍显示本周期估值。
		estimateCalibrate(s, c, usage(2, 22, 32, resetFiveNext, t2))
		view = estimateViewOf(t, s, c, usage(2, 22, 32, resetFiveNext, t2))
		five := estimateWindowResultByType(view, "five_hour")
		if five == nil || !five.Historical || five.Samples != 1 {
			t.Fatalf("five_hour 未回退展示旧估值: %+v", five)
		}
		estimateCloseTo(t, five.Cost.Low, perRequest, "five_hour 回退估值")
		for _, kind := range []string{"weekly", "monthly"} {
			r := estimateWindowResultByType(view, kind)
			if r == nil || r.Historical || r.Samples != 1 {
				t.Fatalf("%s 被 five_hour 的 reset 污染: %+v", kind, r)
			}
		}
	})

	t.Run("账号与套餐切换不串值", func(t *testing.T) {
		s := registeredService(t, "")
		c := Credential{ID: "switch-key", Type: Provider, APIKey: "key-switch", Label: "切换"}
		estimateAddCredential(t, s, c)
		base := time.Now().UTC()
		reset := base.Add(240 * time.Hour)
		t0 := base.Add(-2 * time.Hour)
		t1 := t0.Add(time.Hour)
		t2 := t1.Add(time.Hour)
		t3 := t2.Add(time.Hour)
		value := func(account, plan string, percent float64, at time.Time) credentialUsage {
			return estimateUsageValue(account, plan, "five_hour", percent, reset, at)
		}

		estimateCalibrate(s, c, value("account-one", "plan-one", 10, t0))
		estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
		estimateCalibrate(s, c, value("account-one", "plan-one", 12, t1))
		if got := estimateWindowResultByType(estimateViewOf(t, s, c, value("account-one", "plan-one", 12, t1)), "five_hour"); got == nil || got.Samples != 1 {
			t.Fatalf("切换前估值未建立: %+v", got)
		}

		// 同一 key 换到另一个账号：不得沿用 account-one 的估值。
		estimateCalibrate(s, c, value("account-two", "plan-two", 4, t2))
		switched := estimateWindowResultByType(estimateViewOf(t, s, c, value("account-two", "plan-two", 4, t2)), "five_hour")
		if switched == nil {
			t.Fatal("切换账号后缺少 five_hour 结果")
		}
		if switched.Samples != 0 || switched.Historical || switched.Total != nil {
			t.Fatalf("账号切换后串用了旧账号估值: %+v", *switched)
		}

		// 同账号换套餐：容量口径不同，旧估值不沿用。
		estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
		planTwoUsage := value("account-two", "plan-two", 6, t2.Add(30*time.Minute))
		estimateCalibrate(s, c, planTwoUsage)
		if prior := estimateWindowResultByType(estimateViewOf(t, s, c, planTwoUsage), "five_hour"); prior == nil || prior.Total == nil {
			t.Fatal("套餐切换前必须已有有效估值")
		}
		if mismatched := estimateViewOf(t, s, c, value("account-two", "plan-two-b", 6, t3)); len(mismatched.Windows) != 0 {
			t.Fatal("新套餐已确认但校准尚未执行时仍暴露旧套餐估值")
		}
		estimateCalibrate(s, c, value("account-two", "plan-two-b", 6, t3))
		replanned := estimateWindowResultByType(estimateViewOf(t, s, c, value("account-two", "plan-two-b", 6, t3)), "five_hour")
		if replanned == nil {
			t.Fatal("切换套餐后缺少 five_hour 结果")
		}
		if replanned.Samples != 0 || replanned.Total != nil {
			t.Fatalf("套餐切换后串用了旧估值: %+v", *replanned)
		}
	})
}

// 月额度不再独立采样：总额由同一账号的周估值 × 2 得到，样本 metadata 复制
// 周来源，剩余额度改用月窗口自己的百分比，因此月百分比不变也照常展示。
func TestEstimateMonthlyDerivesFromWeekly(t *testing.T) {
	s := registeredService(t, "")
	c := Credential{ID: "month-key", Type: Provider, APIKey: "key-month", Label: "月折算"}
	estimateAddCredential(t, s, c)
	account, plan := "account-month", "plan-month"
	base := time.Now().UTC()
	resetFive := base.Add(240 * time.Hour)
	resetWeekly := base.Add(300 * time.Hour)
	resetMonthly := base.Add(400 * time.Hour)
	t0 := base.Add(-2 * time.Hour)
	t1 := t0.Add(time.Hour)
	t2 := t1.Add(time.Hour)
	usage := func(five, weekly, monthly float64, at time.Time) credentialUsage {
		f, w, m := five, weekly, monthly
		rf, rw, rm := resetFive, resetWeekly, resetMonthly
		return credentialUsage{
			accountHash: account, planHash: plan, Status: "ok", UpdatedAt: &at,
			Limits: []usageLimit{
				{Type: "five_hour", PercentUsed: &f, ResetsAt: &rf},
				{Type: "weekly", PercentUsed: &w, ResetsAt: &rw},
				{Type: "monthly", PercentUsed: &m, ResetsAt: &rm},
			},
		}
	}

	// 月百分比全程保持 30：月视图只跟着周估值走，不依赖月窗口自身变化。
	estimateCalibrate(s, c, usage(10, 20, 30, t0))
	estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	estimateCalibrate(s, c, usage(12, 22, 30, t1))
	view := estimateViewOf(t, s, c, usage(12, 22, 30, t1))
	weekly := estimateWindowResultByType(view, "weekly")
	monthly := estimateWindowResultByType(view, "monthly")
	if weekly == nil || weekly.Total == nil {
		t.Fatalf("周估值缺失: %+v", weekly)
	}
	if monthly == nil || monthly.DerivedFrom != "weekly" || monthly.Total == nil {
		t.Fatalf("月估值未由周估值推导: %+v", monthly)
	}
	if monthly.Historical || monthly.Samples != weekly.Samples || monthly.Cost.Low != weekly.Cost.Low || monthly.Delta != weekly.Delta || monthly.PendingRequests != weekly.PendingRequests {
		t.Fatalf("月估值未复制周来源 metadata: monthly=%+v weekly=%+v", monthly, weekly)
	}
	estimateSingleValue(t, *monthly.Total, 2*weekly.Total.Low, "月总额度")
	// 月剩余用月窗口自己的百分比，与周剩余相互独立。
	if weekly.Remaining == nil || monthly.Remaining == nil {
		t.Fatalf("剩余额度缺失: weekly=%+v monthly=%+v", weekly, monthly)
	}
	estimateSingleValue(t, *weekly.Remaining, weekly.Total.Low*0.78, "周剩余额度")
	estimateSingleValue(t, *monthly.Remaining, monthly.Total.Low*0.7, "月剩余额度")

	// 月百分比保持不变：只有周窗口继续累计，月视图仍然展示推导总额。
	estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	estimateCalibrate(s, c, usage(14, 24, 30, t2))
	view = estimateViewOf(t, s, c, usage(14, 24, 30, t2))
	weekly = estimateWindowResultByType(view, "weekly")
	monthly = estimateWindowResultByType(view, "monthly")
	if weekly == nil || weekly.Total == nil || weekly.Samples != 2 {
		t.Fatalf("周估值第二轮样本错误: %+v", weekly)
	}
	if monthly == nil || monthly.Total == nil || monthly.Historical || monthly.Samples != 2 || monthly.DerivedFrom != "weekly" {
		t.Fatalf("月百分比不变时未继续展示: %+v", monthly)
	}
	estimateSingleValue(t, *monthly.Total, 2*weekly.Total.Low, "第二轮月总额度")
	estimateSingleValue(t, *monthly.Remaining, monthly.Total.Low*0.7, "第二轮月剩余额度")
	estimateSingleValue(t, *weekly.Remaining, weekly.Total.Low*0.76, "第二轮周剩余额度")
}

// 月剩余额度只在月窗口仍然有效时给出：reset 已过期时保留推导总额、不再猜剩余；
// 月推导同样按账号隔离，另一个账号没有周估值时只能等待。
func TestEstimateMonthlyRemainingExpiryAndAccountIsolation(t *testing.T) {
	s := registeredService(t, "")
	c := Credential{ID: "month-expire", Type: Provider, APIKey: "key-month-expire", Label: "月过期"}
	estimateAddCredential(t, s, c)
	account, plan := "account-month-expire", "plan-month-expire"
	base := time.Now().UTC()
	resetWeekly := base.Add(300 * time.Hour)
	validMonthly := base.Add(400 * time.Hour)
	expiredMonthly := base.Add(-time.Hour)
	t0 := base.Add(-2 * time.Hour)
	t1 := t0.Add(time.Hour)
	usage := func(weekly float64, monthlyReset time.Time, at time.Time) credentialUsage {
		w, m, rw, rm := weekly, 30.0, resetWeekly, monthlyReset
		return credentialUsage{
			accountHash: account, planHash: plan, Status: "ok", UpdatedAt: &at,
			Limits: []usageLimit{
				{Type: "weekly", PercentUsed: &w, ResetsAt: &rw},
				{Type: "monthly", PercentUsed: &m, ResetsAt: &rm},
			},
		}
	}

	estimateCalibrate(s, c, usage(20, validMonthly, t0))
	estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash", 200000, 1000, 0, 0)
	estimateCalibrate(s, c, usage(22, validMonthly, t1))

	view := estimateViewOf(t, s, c, usage(22, expiredMonthly, t1))
	weekly := estimateWindowResultByType(view, "weekly")
	monthly := estimateWindowResultByType(view, "monthly")
	if weekly == nil || weekly.Total == nil || monthly == nil || monthly.Total == nil {
		t.Fatalf("过期月窗口丢失可展示总额: weekly=%+v monthly=%+v", weekly, monthly)
	}
	if monthly.Remaining != nil {
		t.Fatalf("过期月窗口仍给出剩余额度: %+v", *monthly.Remaining)
	}
	estimateSingleValue(t, *monthly.Total, 2*weekly.Total.Low, "过期月窗口总额度")

	other := Credential{ID: "month-other", Type: Provider, APIKey: "key-month-other", Label: "其他账号"}
	estimateAddCredential(t, s, other)
	otherUsage := func(percent float64, at time.Time) credentialUsage {
		p, r := percent, validMonthly
		return credentialUsage{
			accountHash: "account-month-other", planHash: "plan-month-other", Status: "ok", UpdatedAt: &at,
			Limits: []usageLimit{
				{Type: "weekly", PercentUsed: &p, ResetsAt: &r},
				{Type: "monthly", PercentUsed: &p, ResetsAt: &r},
			},
		}
	}
	estimateCalibrate(s, other, otherUsage(40, t0))
	otherMonthly := estimateWindowResultByType(estimateViewOf(t, s, other, otherUsage(40, t1)), "monthly")
	if otherMonthly == nil || otherMonthly.Total != nil || otherMonthly.Remaining != nil {
		t.Fatalf("月估值跨账号串值: %+v", otherMonthly)
	}
	if otherMonthly.DerivedFrom != "weekly" || !strings.Contains(otherMonthly.Note, "等待周额度") {
		t.Fatalf("空月窗口未给出等待说明: %+v", otherMonthly)
	}

	// 原账号的月估值仍在，未被另一个账号的空窗口影响。
	kept := estimateWindowResultByType(estimateViewOf(t, s, c, usage(22, validMonthly, t1)), "monthly")
	if kept == nil || kept.Total == nil || kept.Remaining == nil {
		t.Fatalf("原账号月估值被其他账号影响: %+v", kept)
	}
}

// 峰谷两档参考价在估值里折算成两者的平均成本，输出的仍然是单值。
func TestEstimatePeakValleyCostUsesMidpoint(t *testing.T) {
	s := registeredService(t, "")
	c := Credential{ID: "peak-key", Type: Provider, APIKey: "key-peak", Label: "峰谷"}
	estimateAddCredential(t, s, c)
	account, plan := "account-peak", "plan-peak"
	reset := time.Now().UTC().Add(240 * time.Hour)
	t0 := time.Now().UTC().Add(-2 * time.Hour)
	t1 := t0.Add(time.Hour)
	usage := func(percent float64, at time.Time) credentialUsage {
		return estimateUsageValue(account, plan, "five_hour", percent, reset, at)
	}
	// deepseek-v4-pro 只公开峰/谷两档单价：谷价 = 峰价 / 2。
	peak := (200000*1.32 + 1000*3.96) / 1e6
	valley := peak / 2

	estimateCalibrate(s, c, usage(10, t0))
	// 故意缺少请求时间：无法选定峰谷档，账本与估值应保留区间及均价。
	entry := estimatePricedEntry("cline-pass/deepseek-v4-pro", 200000, 1000, 0, 0, false)
	entry.estimateKey, entry.CredentialID = s.startEstimateRequest(c), c.ID
	s.appendLog(entry)
	estimateCalibrate(s, c, usage(12, t1))

	w := estimateWindowSnapshot(t, s, account, "five_hour")
	estimateCloseTo(t, w.Cost.Low, valley, "峰谷样本谷价成本")
	estimateCloseTo(t, w.Cost.High, peak, "峰谷样本峰价成本")
	result := estimateWindowResultByType(estimateViewOf(t, s, c, usage(12, t1)), "five_hour")
	if result == nil || result.Total == nil {
		t.Fatalf("峰谷计价未给出总额: %+v", result)
	}
	estimateSingleValue(t, *result.Total, 100*((valley+peak)/2)/2, "峰谷平均总额度")
	if !strings.Contains(result.Note, "峰谷") {
		t.Fatalf("峰谷平均未被说明: %+v", *result)
	}
}

// ---------------------------------------------------------------------------
// 参考价格
// ---------------------------------------------------------------------------

func TestPricingCachedReadIsNotChargedAsInput(t *testing.T) {
	plain, ok := referenceCost(estimatePricedEntry("cline-pass/deepseek-v4.1-flash", 200000, 0, 0, 0, false))
	if !ok {
		t.Fatal("普通请求未计价")
	}
	cached, ok := referenceCost(estimatePricedEntry("cline-pass/deepseek-v4.1-flash", 200000, 0, 100000, 0, false))
	if !ok {
		t.Fatal("缓存读请求未计价")
	}
	estimateCloseTo(t, plain.Low, 200000*0.15/1e6, "谷时输入成本")
	estimateCloseTo(t, plain.High, 200000*0.30/1e6, "峰时输入成本")
	if cached.Low >= plain.Low {
		t.Fatalf("缓存读被按输入价重复收费: cached=%v plain=%v", cached.Low, plain.Low)
	}
	estimateCloseTo(t, cached.Low, (100000*0.15+100000*0.003)/1e6, "谷时缓存读成本")
	estimateCloseTo(t, cached.High, (100000*0.30+100000*0.006)/1e6, "峰时缓存读成本")
}

func TestPricingQwenWriteRequirementAndHighContextThreshold(t *testing.T) {
	withWrite, ok := referenceCost(estimatePricedEntry("cline-pass/qwen3.7-plus", 1000, 0, 0, 100, true))
	if !ok {
		t.Fatal("显式上报缓存写后仍未计价")
	}
	estimateCloseTo(t, withWrite.Low, (900*0.40+100*0.50)/1e6, "Qwen 缓存写价格")
	if _, ok := referenceCost(estimatePricedEntry("cline-pass/qwen3.7-plus", 1000, 0, 0, 100, false)); ok {
		t.Fatal("未显式上报缓存写却按写价计价")
	}
	if _, ok := referenceCost(estimatePricedEntry("cline-pass/qwen3.7-plus", 1000, 0, 0, 0, false)); ok {
		t.Fatal("存在独立缓存写单价的模型在未上报缓存写时仍计价")
	}

	threshold, ok := referenceCost(estimatePricedEntry("cline-pass/qwen3.7-plus", 262144, 0, 0, 0, true))
	if !ok {
		t.Fatal("阈值内的 Qwen 请求未计价")
	}
	high, ok := referenceCost(estimatePricedEntry("cline-pass/qwen3.7-plus", 262145, 0, 0, 0, true))
	if !ok {
		t.Fatal("超过阈值的 Qwen 请求未计价")
	}
	estimateCloseTo(t, threshold.Low, 262144*0.40/1e6, "阈值内单价")
	estimateCloseTo(t, high.Low, 262145*1.20/1e6, "超阈值单价")
	if high.Low <= threshold.Low {
		t.Fatalf("超过 262144 上下文未切换高价档: threshold=%v high=%v", threshold.Low, high.Low)
	}
}

func TestPricingDeepSeekPeakValleyRange(t *testing.T) {
	r, ok := referenceCost(estimatePricedEntry("cline-pass/deepseek-v4-pro", 1000000, 0, 0, 0, false))
	if !ok {
		t.Fatal("deepseek-v4-pro 未计价")
	}
	peak := 1000000 * 1.32 / 1e6
	estimateCloseTo(t, r.High, peak, "峰时价格")
	estimateCloseTo(t, r.Low, peak/2, "谷时价格")
	if r.Low >= r.High {
		t.Fatalf("峰谷价格未保留为区间: %+v", r)
	}
}

func TestPricingUnknownModelIsExcluded(t *testing.T) {
	if _, ok := referenceCost(estimatePricedEntry("cline-pass/mimo-v2.6", 1000, 0, 0, 0, false)); ok {
		t.Fatal("未知 mimo-v2.6 被计价")
	}
	if _, ok := referenceCost(estimatePricedEntry("cline-pass/mimo-v2.5", 1000, 0, 0, 0, false)); !ok {
		t.Fatal("已登记的 mimo-v2.5 未计价")
	}
	if _, ok := referenceCost(estimatePricedEntry("deepseek-v4.1-flash", 1000, 0, 0, 0, false)); ok {
		t.Fatal("缺少 cline-pass/ 前缀的模型被计价")
	}
}

// ---------------------------------------------------------------------------
// 默认配置
// ---------------------------------------------------------------------------

func TestDefaultTimeout600KeepsExplicitSetting(t *testing.T) {
	if got := defaultConfig().TimeoutSeconds; got != 600 {
		t.Fatalf("默认请求超时 = %d, 期望 600", got)
	}
	if got := registeredService(t, "").config().TimeoutSeconds; got != 600 {
		t.Fatalf("新注册实例未使用 600 秒默认值: %d", got)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"timeout_seconds":180}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := estimateRegister(t, dir).config().TimeoutSeconds; got != 180 {
		t.Fatalf("显式 180 秒设置被默认值覆盖: %d", got)
	}
}

// ---------------------------------------------------------------------------
// 额度查询到估算总额的集成路径
// ---------------------------------------------------------------------------

func TestUsageEstimateIntegrationReturnsTotalWithoutSecrets(t *testing.T) {
	const (
		apiKey = "secret-test-key"
		userID = "private-user"
		subID  = "private-subscription"
	)
	reset := time.Now().UTC().Add(5 * time.Hour).Truncate(time.Second)
	limitsJSON := func(percent float64) string {
		return fmt.Sprintf(`{"success":true,"data":{"limits":[{"type":"five_hour","percentUsed":%g,"resetsAt":%q}]}}`, percent, reset.Format(time.RFC3339))
	}
	planJSON := fmt.Sprintf(`{"success":true,"data":{"userId":%q,"subscriptionId":%q,"currentPeriodEnd":"2026-10-22T14:08:10Z","plan":{"displayName":"Cline Pass (Monthly)"}}}`, userID, subID)
	limitsCalls := 0
	s := usageTestService(t, func(req map[string]any) (int, string) {
		if strings.HasSuffix(str(req["url"]), "/usage-limits") {
			limitsCalls++
			if limitsCalls == 1 {
				return 200, limitsJSON(5)
			}
			return 200, limitsJSON(7)
		}
		return 200, planJSON
	})
	req := usageTestRequest()

	// 1) 首次查询用 plan 响应确认账号并建立百分比基线。
	firstRaw, err := s.management(jsonBytes(req))
	if err != nil {
		t.Fatal(err)
	}
	firstBody := firstRaw.(ManagementResponse).Body
	var first credentialUsage
	if json.Unmarshal(firstBody, &first) != nil || first.Status != "ok" || first.Estimate == nil {
		t.Fatalf("首次查询未建立估算基线: %s", firstBody)
	}
	if first.Estimate.Status != "sampling" || first.Estimate.Windows[0].Type != "five_hour" || first.Estimate.Windows[0].Total != nil {
		t.Fatalf("首次查询基线状态异常: %s", firstBody)
	}

	// 2) 走真实 newLog + observeMetadata 路径模拟一次分帧上报的流式请求。
	s.mu.RLock()
	c := s.creds["one"]
	s.mu.RUnlock()
	entry := s.newLog(ExecutorRequest{Model: "deepseek-flash", HostCallbackID: "usage-callback"}, c, "cline-pass/deepseek-v4.1-flash")
	entry.Time, entry.DurationMS = estimatePeakTime(), 1000
	attempt := Attempt{Mode: "stream"}
	observeMetadata(map[string]any{"usage": map[string]any{
		"prompt_tokens":         float64(200000),
		"prompt_tokens_details": map[string]any{"cached_tokens": float64(100000)},
	}}, &entry, &attempt)
	observeMetadata(map[string]any{"usage": map[string]any{"completion_tokens": float64(1000)}}, &entry, &attempt)
	if !entry.UsageReported || entry.PromptTokens != 200000 || entry.CompletionTokens != 1000 || entry.CachedTokens != 100000 {
		t.Fatalf("分帧 usage 未正确合并: %+v", entry)
	}
	entry.Attempts = []Attempt{attempt}
	entry.Status = 200
	s.appendLog(entry)

	// 3) 缓存过期且百分比上升，第二次查询应给出估算总额。
	past := time.Now().Add(-2 * time.Minute)
	s.mu.Lock()
	s.usageCache["one"].value.CheckedAt = &past
	s.mu.Unlock()
	secondRaw, err := s.management(jsonBytes(req))
	if err != nil {
		t.Fatal(err)
	}
	secondBody := secondRaw.(ManagementResponse).Body
	var second credentialUsage
	if json.Unmarshal(secondBody, &second) != nil || second.Status != "ok" || second.Estimate == nil {
		t.Fatalf("第二次查询失败: %s", secondBody)
	}
	var five *estimateResult
	for i := range second.Estimate.Windows {
		if second.Estimate.Windows[i].Type == "five_hour" {
			five = &second.Estimate.Windows[i]
		}
	}
	if five == nil || five.Total == nil {
		t.Fatalf("额度上升后未返回估算总额: %s", secondBody)
	}
	cost := (100000*0.30 + 100000*0.006 + 1000*1.20) / 1e6
	// 单值估算：判断与展示都不再把百分比取整误差展开成上下限。
	estimateSingleValue(t, *five.Total, 100*cost/2, "估算总额")

	// 4) 响应与持久化都不得出现原始 API key / userId。
	estimates, err := os.ReadFile(filepath.Join(s.config().DataDir, "estimates.json"))
	if err != nil {
		t.Fatalf("估算存档未写入: %v", err)
	}
	requests := observabilityJSON(t, observabilityPersistedLogs(t, s.config().DataDir))
	for name, blob := range map[string][]byte{
		"responses":      append(append([]byte{}, firstBody...), secondBody...),
		"estimates.json": estimates,
		"requests.json":  requests,
	} {
		for _, secret := range []string{apiKey, userID, subID} {
			if strings.Contains(string(blob), secret) {
				t.Fatalf("%s 泄露 %q: %s", name, secret, blob)
			}
		}
	}
}

func TestSSEPartialUsageFramesKeepPromptCompletionAndCacheWrite(t *testing.T) {
	var entry LogEntry
	var attempt Attempt
	observeMetadata(map[string]any{"usage": map[string]any{
		"prompt_tokens":         float64(200000),
		"prompt_tokens_details": map[string]any{"cached_tokens": float64(100000)},
	}}, &entry, &attempt)
	if entry.UsageReported {
		t.Fatal("仅上报 prompt 时不应标记为完整 usage")
	}
	observeMetadata(map[string]any{"usage": map[string]any{"completion_tokens": float64(1000)}}, &entry, &attempt)
	if !entry.UsageReported {
		t.Fatal("prompt 与 completion 分帧上报后未标记为完整 usage")
	}
	observeMetadata(map[string]any{"usage": map[string]any{
		"prompt_tokens_details": map[string]any{"cache_write_tokens": float64(500)},
	}}, &entry, &attempt)
	if !entry.CacheWriteReported {
		t.Fatal("缓存写分帧上报未被识别")
	}
	if entry.PromptTokens != 200000 || entry.CompletionTokens != 1000 || entry.CachedTokens != 100000 || entry.CacheWriteTokens != 500 {
		t.Fatalf("分帧 usage 互相归零: %+v", entry)
	}
	// Cline/Anthropic 形态的 cache_creation_input_tokens 只更新缓存写。
	observeMetadata(map[string]any{"usage": map[string]any{"cache_creation_input_tokens": float64(700)}}, &entry, &attempt)
	if entry.CacheWriteTokens != 700 || entry.PromptTokens != 200000 || entry.CompletionTokens != 1000 || entry.CachedTokens != 100000 {
		t.Fatalf("cache_creation_input_tokens 覆盖了其它 usage 字段: %+v", entry)
	}
}
