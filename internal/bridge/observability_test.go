package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type observabilityStatisticsView struct {
	Requests         int64     `json:"requests"`
	LocalRequests    int64     `json:"local_backoff_requests"`
	UpstreamRequests int64     `json:"upstream_requests"`
	UpstreamUnknown  int64     `json:"upstream_requests_unknown"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	CachedTokens     int64     `json:"cached_tokens"`
	TotalTokens      int64     `json:"total_tokens"`
	CacheRate        *float64  `json:"cache_rate"`
	OutputRate       *float64  `json:"output_tokens_per_second"`
	ImportedHistory  bool      `json:"imported_history"`
	PersistenceError string    `json:"persistence_error"`
	Resolution       string    `json:"resolution"`
	Start            time.Time `json:"start"`
	End              time.Time `json:"end"`
}

type observabilityLogsView struct {
	Items   []LogEntry `json:"items"`
	Total   int        `json:"total"`
	Summary struct {
		Requests int64 `json:"requests"`
	} `json:"summary"`
}

func observabilityJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func observabilityService(t *testing.T, dir string) *Service {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	s := NewService()
	// Register after TempDir's cleanup, so shutdown drains the observation timer
	// and persists counters before the directory is removed, even after Fatal.
	t.Cleanup(func() {
		if _, err := s.Handle("plugin.shutdown", nil); err != nil {
			t.Errorf("cleanup shutdown: %v", err)
		}
	})
	s.SetHost(func(method string, _, _ any) error {
		return fmt.Errorf("unexpected host call in offline test: %s", method)
	})
	yaml := fmt.Sprintf("data_dir: %q\nlog_retention: 50\nmodels:\n  - id: deepseek-flash\n    upstream_id: cline-pass/deepseek-v4.1-flash\n", filepath.ToSlash(dir))
	if _, err := s.Handle("plugin.register", observabilityJSON(t, map[string]any{"config_yaml": []byte(yaml)})); err != nil {
		t.Fatalf("register: %v", err)
	}
	return s
}

func observabilityShutdown(t *testing.T, s *Service) {
	t.Helper()
	if _, err := s.Handle("plugin.shutdown", nil); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.logWriteError != "" || s.statisticsWriteError != "" {
		t.Fatalf("shutdown persistence: logs=%q statistics=%q", s.logWriteError, s.statisticsWriteError)
	}
}

func observabilityManagement(t *testing.T, s *Service, method, path string, query url.Values, body []byte, status int) ManagementResponse {
	t.Helper()
	result, err := s.Handle("management.handle", observabilityJSON(t, ManagementRequest{
		Method: method, Path: apiBase + path, Query: query, Body: body,
	}))
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	response, ok := result.(ManagementResponse)
	if !ok {
		t.Fatalf("%s %s response type = %T", method, path, result)
	}
	if response.StatusCode != status {
		t.Fatalf("%s %s status = %d, want %d; body=%s", method, path, response.StatusCode, status, response.Body)
	}
	return response
}

func observabilityStatistics(t *testing.T, s *Service, query url.Values) observabilityStatisticsView {
	t.Helper()
	r := observabilityManagement(t, s, "GET", "/statistics", query, nil, 200)
	var view observabilityStatisticsView
	if err := json.Unmarshal(r.Body, &view); err != nil {
		t.Fatal(err)
	}
	if view.PersistenceError != "" || view.Resolution != "minute" {
		t.Fatalf("statistics metadata: persistence=%q resolution=%q", view.PersistenceError, view.Resolution)
	}
	return view
}

func observabilityLogs(t *testing.T, s *Service, query url.Values) observabilityLogsView {
	t.Helper()
	r := observabilityManagement(t, s, "GET", "/logs", query, nil, 200)
	var view observabilityLogsView
	if err := json.Unmarshal(r.Body, &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func observabilityCounts(t *testing.T, got observabilityStatisticsView, requests, local, upstream, unknown, prompt, completion, cached int64) {
	t.Helper()
	g := [7]int64{got.Requests, got.LocalRequests, got.UpstreamRequests, got.UpstreamUnknown, got.PromptTokens, got.CompletionTokens, got.CachedTokens}
	w := [7]int64{requests, local, upstream, unknown, prompt, completion, cached}
	if g != w || got.TotalTokens != prompt+completion {
		t.Errorf("statistics [requests local upstream unknown prompt completion cached] = %v, want %v; total_tokens=%d, want %d", g, w, got.TotalTokens, prompt+completion)
	}
}

func observabilityAttempts(count int) *int { return &count }

func observabilityRate(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("rate = null, want %g", want)
	}
	if math.IsNaN(*got) || math.IsInf(*got, 0) || math.Abs(*got-want) > 1e-9 {
		t.Errorf("rate = %g, want %g", *got, want)
	}
}

func observabilitySeedLogs(t *testing.T, dir string, logs []LogEntry) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "requests.json"), observabilityJSON(t, logs), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestObservabilityConcurrentLocalBackoff(t *testing.T) {
	s := observabilityService(t, "")
	c := Credential{Type: Provider, ID: "credential-1", Label: "fixture", APIKey: "offline-key"}
	now := time.Now().UTC()
	s.rateLimits.clock = func() time.Time { return now }
	lease, err := s.rateLimits.acquire(c, "cline-pass/deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	// Seed one real limiter window without making or logging an upstream call.
	lease.finish(classifyUpstreamError(200, nil, map[string]any{"error": teamLimitMessage}, now))
	var hostCalls atomic.Int64
	s.SetHost(func(method string, _, _ any) error {
		hostCalls.Add(1)
		return fmt.Errorf("locally blocked request reached host: %s", method)
	})
	const count = 100
	start := make(chan struct{})
	results := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		raw := observabilityJSON(t, ExecutorRequest{
			Model: "deepseek-flash", StreamID: fmt.Sprintf("client-%d", i),
			Payload:     []byte(`{"messages":[{"role":"user","content":"offline fixture"}],"reasoning_effort":"high"}`),
			StorageJSON: observabilityJSON(t, c),
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Handle("executor.execute_stream", raw)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		limited := asTeamRateLimit(err)
		if limited == nil || !limited.Local || statusOf(err) != 429 {
			t.Errorf("blocked request = %v, want local team-limit 429", err)
		}
	}
	if got := hostCalls.Load(); got != 0 {
		t.Errorf("host calls = %d, want 0", got)
	}
	logs := observabilityLogs(t, s, nil)
	if logs.Total != 1 || len(logs.Items) != 1 || logs.Summary.Requests != count {
		t.Fatalf("100 blocked requests: rows=%d items=%d summary_requests=%d", logs.Total, len(logs.Items), logs.Summary.Requests)
	}
	entry := logs.Items[0]
	if entry.RequestCount != count || !entry.UpstreamSkipped || entry.BackoffWindowID == "" || entry.ReasoningEffort != "high" || len(entry.Attempts) != 0 || entry.OutputTokensPerSecond != nil {
		t.Errorf("aggregated local log = %+v", entry)
	}
	if entry.LastTime == nil || entry.LastTime.Before(entry.Time) || entry.LastRequestID == "" {
		t.Errorf("aggregation lost request time/identity: first=%s last=%v last_id=%q", entry.Time, entry.LastTime, entry.LastRequestID)
	}
	observabilityCounts(t, observabilityStatistics(t, s, nil), 100, 100, 0, 0, 0, 0, 0)
	activity, ok := s.estimateActivitySnapshot()[estimateKeyID(c)]
	if !ok || activity.Active != 0 {
		t.Errorf("estimate activity after all requests finish = %+v, present=%t", activity, ok)
	}
	s.mu.RLock()
	if key := s.estimates.Keys[estimateKeyID(c)]; key != nil && (key.Totals.Requests != 0 || key.Totals.Unknown != 0 || key.Totals.Tokens != 0 || key.Totals.Cost.Low != 0 || key.Totals.Cost.High != 0) {
		t.Errorf("local suppression inflated quota ledger: %+v", key.Totals)
	}
	s.mu.RUnlock()
	// Shutdown is the observable durability boundary; do not sleep for the timer.
	observabilityShutdown(t, s)
	b, err := os.ReadFile(filepath.Join(s.config().DataDir, "requests.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted []LogEntry
	if err := json.Unmarshal(b, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted[0].RequestCount != count {
		t.Errorf("shutdown persisted %d rows, want one with count 100: %+v", len(persisted), persisted)
	}
	restarted := observabilityService(t, s.config().DataDir)
	observabilityCounts(t, observabilityStatistics(t, restarted, nil), 100, 100, 0, 0, 0, 0, 0)
}

func TestObservabilityBackoffAggregationIsolation(t *testing.T) {
	s := observabilityService(t, "")
	first := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	last, retry := first.Add(time.Second), first.Add(time.Minute)
	base := LogEntry{
		Time: first, Model: "alias-a", UpstreamModel: "upstream-a", CredentialID: "credential-a", Credential: "same label",
		Stream: true, Status: 429, UpstreamSkipped: true, Provider: "unknown", ReasoningEffort: "high",
		BackoffWindowID: "window-a", RetryAt: &retry, ErrorKind: "upstream_team_rate_limited", RateLimitScope: "team_model",
		Attempts: []Attempt{{Status: 429, UpstreamSkipped: true}},
	}
	groups := []struct {
		name   string
		change func(*LogEntry)
	}{
		{"base", func(*LogEntry) {}},
		{"window", func(e *LogEntry) { e.BackoffWindowID = "window-b" }},
		{"credential", func(e *LogEntry) { e.CredentialID = "credential-b" }},
		{"alias", func(e *LogEntry) { e.Model = "alias-b" }},
		{"effort", func(e *LogEntry) { e.ReasoningEffort = "low" }},
	}
	// Interleave both rounds, so matching rows are separated by unrelated rows.
	for round := 0; round < 2; round++ {
		for _, group := range groups {
			e := base
			group.change(&e)
			e.ID = fmt.Sprintf("%s-%d", group.name, round)
			if round == 1 {
				e.Time = last
			}
			s.appendLog(e)
		}
	}
	logs := observabilityLogs(t, s, url.Values{"limit": {"1"}})
	if logs.Total != 5 || len(logs.Items) != 1 || logs.Summary.Requests != 10 {
		t.Errorf("isolated paginated logs: rows=%d items=%d requests=%d, want 5/1/10", logs.Total, len(logs.Items), logs.Summary.Requests)
	}
	all := observabilityLogs(t, s, url.Values{"limit": {"200"}})
	byID := map[string]LogEntry{}
	for _, e := range all.Items {
		byID[e.ID] = e
	}
	for _, group := range groups {
		e, ok := byID[group.name+"-0"]
		if !ok || e.RequestCount != 2 || !e.Time.Equal(first) || e.LastTime == nil || !e.LastTime.Equal(last) || e.LastRequestID != group.name+"-1" || len(e.Attempts) != 0 {
			t.Errorf("group %s lost isolation/count/times: %+v, present=%t", group.name, e, ok)
		}
	}
	observabilityCounts(t, observabilityStatistics(t, s, nil), 10, 10, 0, 0, 0, 0, 0)
}

func TestObservabilityStatisticsOutliveLogRetention(t *testing.T) {
	s := observabilityService(t, "")
	for i := 0; i < 75; i++ {
		calls := 1
		attempts := []Attempt{{Status: 200}}
		if i%5 == 0 {
			calls = 2
			attempts = []Attempt{{Status: 500}, {Status: 200}}
		}
		s.appendLog(LogEntry{
			ID: fmt.Sprintf("request-%02d", i), Model: "fixture", Status: 200,
			PromptTokens: 10, CompletionTokens: 2, CachedTokens: 4, Attempts: attempts, UpstreamAttempts: &calls,
		})
	}
	logs := observabilityLogs(t, s, url.Values{"limit": {"200"}})
	if logs.Total != 50 || len(logs.Items) != 50 || logs.Summary.Requests != 50 {
		t.Errorf("retained logs: rows=%d items=%d requests=%d, want 50/50/50", logs.Total, len(logs.Items), logs.Summary.Requests)
	}
	for _, e := range logs.Items {
		if e.ID == "request-00" {
			t.Error("oldest log survived the configured retention limit")
		}
	}
	stats := observabilityStatistics(t, s, nil)
	observabilityCounts(t, stats, 75, 0, 90, 0, 750, 150, 300)
	observabilityRate(t, stats.CacheRate, 0.4)
	if stats.OutputRate != nil || stats.ImportedHistory {
		t.Errorf("live statistics invented a rate/history import: %+v", stats)
	}
	observabilityShutdown(t, s)
	restarted := observabilityService(t, s.config().DataDir)
	observabilityCounts(t, observabilityStatistics(t, restarted, nil), 75, 0, 90, 0, 750, 150, 300)
}

func TestObservabilityMinuteStatisticsRanges(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	lastLocal := day.Add(5 * time.Second)
	observabilitySeedLogs(t, dir, []LogEntry{
		{ID: "previous-day", Time: day.Add(-75 * time.Second), DurationMS: 60000, Status: 200, PromptTokens: 100, CompletionTokens: 10, UpstreamAttempts: observabilityAttempts(1)},
		{ID: "local-minute", Time: day.Add(-40 * time.Second), LastTime: &lastLocal, Status: 429, RequestCount: 3, UpstreamSkipped: true},
		{ID: "next-minute", Time: day.Add(50 * time.Second), DurationMS: 60000, Status: 200, PromptTokens: 200, CompletionTokens: 20, UpstreamAttempts: observabilityAttempts(1)},
		{ID: "exclusive-end", Time: day.Add(90 * time.Second), DurationMS: 30000, Status: 200, PromptTokens: 400, CompletionTokens: 40, UpstreamAttempts: observabilityAttempts(1)},
	})
	s := observabilityService(t, dir)
	observabilityCounts(t, observabilityStatistics(t, s, nil), 6, 3, 3, 0, 700, 70, 0)
	// UTC+8 inputs cross the day-file boundary. Partial minutes expand to whole
	// minute buckets, and an aligned end is exclusive.
	stats := observabilityStatistics(t, s, url.Values{
		"start": {"2026-10-03T08:00:30+08:00"}, "end": {"2026-10-03T08:01:30+08:00"},
	})
	observabilityCounts(t, stats, 4, 3, 1, 0, 200, 20, 0)
	if !stats.Start.Equal(day) || !stats.End.Equal(day.Add(2*time.Minute)) {
		t.Errorf("minute range = [%s, %s), want [%s, %s)", stats.Start, stats.End, day, day.Add(2*time.Minute))
	}
	observabilityCounts(t, observabilityStatistics(t, s, url.Values{
		"start": {"2026-10-03T00:00:00Z"}, "end": {"2026-10-03T00:01:00Z"},
	}), 3, 3, 0, 0, 0, 0, 0)
	observabilityCounts(t, observabilityStatistics(t, s, url.Values{"end": {"2026-10-03T00:00:00Z"}}), 1, 0, 1, 0, 100, 10, 0)
	observabilityCounts(t, observabilityStatistics(t, s, url.Values{"start": {"2026-10-03T00:02:00Z"}}), 1, 0, 1, 0, 400, 40, 0)
	observabilityCounts(t, observabilityStatistics(t, s, url.Values{"start": {"2026-10-04T00:00:00Z"}}), 0, 0, 0, 0, 0, 0, 0)
	for _, query := range []url.Values{
		{"start": {"yesterday"}},
		{"end": {"invalid"}},
		{"start": {"2026-10-03T00:02:00Z"}, "end": {"2026-10-03T00:01:00Z"}},
		{"start": {"2026-10-03T00:00:00Z"}, "end": {"2026-10-03T00:00:00Z"}},
	} {
		observabilityManagement(t, s, "GET", "/statistics", query, nil, 400)
	}
}

func TestObservabilityHistoryImportAndConfirmedReset(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	local := LogEntry{ID: "local-a", Time: at, RequestCount: 3, CredentialID: "one", Model: "alias", UpstreamModel: "upstream", BackoffWindowID: "legacy-window", UpstreamSkipped: true, Status: 429}
	other := local
	other.ID, other.Time, other.RequestCount = "local-b", at.Add(time.Second), 2
	observabilitySeedLogs(t, dir, []LogEntry{local, other, {
		ID: "upstream", Time: at.Add(time.Minute), Status: 200, PromptTokens: 80, CompletionTokens: 20, CachedTokens: 20,
		Attempts: []Attempt{{Status: 500}, {Status: 200}},
	}})
	s := observabilityService(t, dir)
	stats := observabilityStatistics(t, s, nil)
	// An old log's two Attempt descriptions do not prove two transports opened.
	observabilityCounts(t, stats, 6, 5, 0, 1, 80, 20, 20)
	if !stats.ImportedHistory {
		t.Error("initial statistics did not report imported retained history")
	}
	logs := observabilityLogs(t, s, nil)
	if logs.Total != 2 || logs.Summary.Requests != 6 {
		t.Errorf("bootstrap compacted logs: rows=%d requests=%d, want 2/6", logs.Total, logs.Summary.Requests)
	}
	observabilityShutdown(t, s)
	s = observabilityService(t, dir)
	observabilityCounts(t, observabilityStatistics(t, s, nil), 6, 5, 0, 1, 80, 20, 20)
	changedYAML := fmt.Sprintf("data_dir: %q\n", filepath.ToSlash(filepath.Join(dir, "reconfigure-other")))
	_, err := s.Handle("plugin.reconfigure", observabilityJSON(t, map[string]any{"config_yaml": []byte(changedYAML)}))
	if err == nil || statusOf(err) != 409 || filepath.Clean(s.config().DataDir) != filepath.Clean(dir) {
		t.Fatalf("loaded instance changed data_dir: error=%v data_dir=%q", err, s.config().DataDir)
	}
	observabilityCounts(t, observabilityStatistics(t, s, nil), 6, 5, 0, 1, 80, 20, 20)
	before, err := os.ReadFile(filepath.Join(dir, "requests.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{}`, `{"confirm":false}`, `{"confirm":"true"}`, `invalid JSON`} {
		observabilityManagement(t, s, "POST", "/statistics/reset", nil, []byte(body), 400)
		observabilityCounts(t, observabilityStatistics(t, s, nil), 6, 5, 0, 1, 80, 20, 20)
	}
	observabilityManagement(t, s, "POST", "/statistics/reset", nil, []byte(`{"confirm":true}`), 200)
	stats = observabilityStatistics(t, s, nil)
	observabilityCounts(t, stats, 0, 0, 0, 0, 0, 0, 0)
	if stats.ImportedHistory || stats.CacheRate != nil || stats.OutputRate != nil {
		t.Errorf("reset retained statistics metadata/rates: %+v", stats)
	}
	logs = observabilityLogs(t, s, nil)
	if logs.Total != 2 || logs.Summary.Requests != 6 {
		t.Errorf("reset changed retained logs: rows=%d requests=%d", logs.Total, logs.Summary.Requests)
	}
	observabilityShutdown(t, s)
	after, err := os.ReadFile(filepath.Join(dir, "requests.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("statistics reset changed the persisted request log file")
	}
	s = observabilityService(t, dir)
	observabilityCounts(t, observabilityStatistics(t, s, nil), 0, 0, 0, 0, 0, 0, 0)
	s.appendLog(LogEntry{ID: "after-reset", Status: 200, PromptTokens: 10, CompletionTokens: 2, CachedTokens: 5, UpstreamAttempts: observabilityAttempts(1), Attempts: []Attempt{{Status: 200}}})
	if _, err := s.Handle("plugin.quiesce", nil); err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	if _, err := s.Handle("executor.execute", observabilityJSON(t, ExecutorRequest{})); statusOf(err) != 503 {
		t.Fatalf("quiesced instance accepted new execution: %v", err)
	}
	// Simulate a replacement updating the same data directory while the old
	// instance is quiesced. Re-registering the old instance must read this newer
	// snapshot, rather than revive its previous in-memory request count of one.
	replacement := observabilityService(t, dir)
	observabilityCounts(t, observabilityStatistics(t, replacement, nil), 1, 0, 1, 0, 10, 2, 5)
	replacement.appendLog(LogEntry{ID: "replacement-request", Status: 200, PromptTokens: 20, CompletionTokens: 4, CachedTokens: 10, UpstreamAttempts: observabilityAttempts(1)})
	observabilityShutdown(t, replacement)
	registerYAML := fmt.Sprintf("data_dir: %q\nlog_retention: 50\nmodels:\n  - id: deepseek-flash\n    upstream_id: cline-pass/deepseek-v4.1-flash\n", filepath.ToSlash(dir))
	if _, err := s.Handle("plugin.register", observabilityJSON(t, map[string]any{"config_yaml": []byte(registerYAML)})); err != nil {
		t.Fatalf("re-register quiesced instance: %v", err)
	}
	if err := s.begin(); err != nil {
		t.Fatalf("re-registered instance remains stopped: %v", err)
	}
	s.active.Done()
	observabilityCounts(t, observabilityStatistics(t, s, nil), 2, 0, 2, 0, 30, 6, 15)
	logs = observabilityLogs(t, s, nil)
	if logs.Total != 4 || logs.Summary.Requests != 8 {
		t.Errorf("post-reset re-registration lost old/new logs: rows=%d requests=%d, want 4/8", logs.Total, logs.Summary.Requests)
	}
}

func TestObservabilityOutputRates(t *testing.T) {
	for _, tt := range []struct {
		name  string
		entry LogEntry
		want  float64 // Zero denotes an unmeasurable sample, returned as null.
	}{
		{"missing tokens", LogEntry{DurationMS: 2000}, 0},
		{"negative tokens", LogEntry{CompletionTokens: -1, DurationMS: 2000}, 0},
		{"missing duration", LogEntry{CompletionTokens: 100}, 0},
		{"negative duration", LogEntry{CompletionTokens: 100, DurationMS: -1}, 0},
		{"negative TTFT", LogEntry{CompletionTokens: 100, DurationMS: 2000, TTFTMS: -1}, 0},
		{"TTFT equals duration", LogEntry{CompletionTokens: 100, DurationMS: 2000, TTFTMS: 2000}, 0},
		{"TTFT exceeds duration", LogEntry{CompletionTokens: 100, DurationMS: 2000, TTFTMS: 2001}, 0},
		{"native without TTFT", LogEntry{CompletionTokens: 100, DurationMS: 2000}, 50},
		{"generation excludes TTFT", LogEntry{CompletionTokens: 100, DurationMS: 2000, TTFTMS: 1000}, 100},
		{"local metadata is not output", LogEntry{UpstreamSkipped: true, CompletionTokens: 100, DurationMS: 2000}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := outputTokenRate(tt.entry)
			if tt.want == 0 {
				if got != nil {
					t.Errorf("unmeasurable output rate = %g, want null", *got)
				}
				return
			}
			observabilityRate(t, got, tt.want)
		})
	}
	t.Run("weighted statistics and JSON null", func(t *testing.T) {
		s := observabilityService(t, "")
		entries := []LogEntry{
			{ID: "slow", Status: 200, PromptTokens: 100, CachedTokens: 25, CompletionTokens: 100, DurationMS: 2000, UpstreamAttempts: observabilityAttempts(1)},
			{ID: "fast", Status: 200, PromptTokens: 100, CachedTokens: 25, CompletionTokens: 20, DurationMS: 2000, TTFTMS: 1000, UpstreamAttempts: observabilityAttempts(1)},
			{ID: "unmeasurable", Status: 200, PromptTokens: 200, CachedTokens: 100, CompletionTokens: 500, UpstreamAttempts: observabilityAttempts(1)},
			{ID: "local", Status: 429, UpstreamSkipped: true, PromptTokens: 9999, CachedTokens: 9999, CompletionTokens: 9999, DurationMS: 1},
		}
		for _, e := range entries {
			s.appendLog(e)
		}
		stats := observabilityStatistics(t, s, nil)
		observabilityCounts(t, stats, 4, 1, 3, 0, 400, 620, 150)
		// 100 tokens / 2s and 20 tokens / 1s: 120 / 3 = 40, not
		// the arithmetic mean (50 + 20) / 2 = 35. Invalid/local samples do not weigh in.
		observabilityRate(t, stats.OutputRate, 40)
		observabilityRate(t, stats.CacheRate, 0.375)
		r := observabilityManagement(t, s, "GET", "/logs", nil, nil, 200)
		var wire struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(r.Body, &wire); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, item := range wire.Items {
			var id string
			if err := json.Unmarshal(item["id"], &id); err != nil {
				t.Fatal(err)
			}
			seen[id] = true
			rate, present := item["output_tokens_per_second"]
			if !present {
				t.Errorf("log %s omitted the output-rate field", id)
				continue
			}
			if id == "local" || id == "unmeasurable" {
				if !bytes.Equal(rate, []byte("null")) {
					t.Errorf("log %s rate = %s, want JSON null", id, rate)
				}
				continue
			}
			var value float64
			if err := json.Unmarshal(rate, &value); err != nil {
				t.Fatal(err)
			}
			want := 50.0
			if id == "fast" {
				want = 20
			}
			observabilityRate(t, &value, want)
		}
		if len(seen) != 4 {
			t.Errorf("rate fixture returned %d logs, want 4", len(seen))
		}
	})
}

func TestObservabilityReasoningMetadataWhitelist(t *testing.T) {
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", "auto"} {
		for _, field := range []string{"reasoning_effort", "reasoning", "thinking"} {
			t.Run(field+"/"+effort, func(t *testing.T) {
				var value any = effort
				if field != "reasoning_effort" {
					value = map[string]any{"effort": effort}
				}
				if got := requestReasoningEffort(observabilityJSON(t, map[string]any{field: value})); got != effort {
					t.Errorf("reasoning metadata = %q, want %q", got, effort)
				}
			})
		}
	}
	for _, tt := range []struct{ name, payload, want string }{
		{"normalization", `{"reasoning_effort":" HIGH "}`, "high"},
		{"top-level preference", `{"reasoning_effort":"low","reasoning":{"effort":"high"}}`, "low"},
		{"nested preference", `{"reasoning":{"effort":"medium"},"thinking":{"effort":"max"}}`, "medium"},
		{"unrecognized top-level fallback", `{"reasoning_effort":"private request text","reasoning":{"effort":"high"}}`, "high"},
		{"thinking disabled", `{"thinking":{"type":"disabled","budget_tokens":1024}}`, "none"},
		{"thinking adaptive", `{"thinking":{"type":"adaptive"}}`, "auto"},
		{"thinking enabled", `{"thinking":{"type":"enabled"}}`, "enabled"},
		{"budget", `{"thinking":{"type":"enabled","budget_tokens":1024}}`, "budget:1024"},
		{"minimum budget", `{"thinking":{"type":"enabled","budget_tokens":1}}`, "budget:1"},
		{"maximum budget", `{"thinking":{"type":"enabled","budget_tokens":1000000000}}`, "budget:1000000000"},
		{"zero budget", `{"thinking":{"type":"enabled","budget_tokens":0}}`, "enabled"},
		{"negative budget", `{"thinking":{"type":"enabled","budget_tokens":-1}}`, "enabled"},
		{"oversized budget", `{"thinking":{"type":"enabled","budget_tokens":1000000001}}`, "enabled"},
		{"string budget", `{"thinking":{"type":"enabled","budget_tokens":"private text"}}`, "enabled"},
		{"budget alone", `{"thinking":{"budget_tokens":1024}}`, ""},
		{"unknown metadata", `{"reasoning_effort":"private prompt","reasoning":{"effort":"sk-fixture-private"},"thinking":{"type":"private text"}}`, ""},
		{"wrong field types", `{"reasoning_effort":true,"reasoning":"high","thinking":["max"]}`, ""},
		{"messages ignored", `{"messages":[{"role":"user","content":"reasoning_effort: high"}]}`, ""},
		{"missing metadata", `{}`, ""},
		{"malformed JSON", `{"reasoning_effort":`, ""},
		{"array root", `["high"]`, ""},
		{"null root", `null`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := requestReasoningEffort([]byte(tt.payload)); got != tt.want {
				t.Errorf("reasoning metadata = %q, want %q", got, tt.want)
			}
		})
	}
}
