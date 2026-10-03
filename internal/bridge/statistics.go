package bridge

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Sparse minute buckets are partitioned by UTC day. Retention of request logs
// never truncates these counters. Only changed day files are rewritten.
type statisticsTotals struct {
	Requests                int64 `json:"requests"`
	LocalBackoffRequests    int64 `json:"local_backoff_requests"`
	UpstreamRequests        int64 `json:"upstream_requests"`
	UpstreamRequestsUnknown int64 `json:"upstream_requests_unknown"`
	PromptTokens            int64 `json:"prompt_tokens"`
	CompletionTokens        int64 `json:"completion_tokens"`
	CachedTokens            int64 `json:"cached_tokens"`
	RateTokens              int64 `json:"rate_tokens"`
	RateDurationMS          int64 `json:"rate_duration_ms"`
}

func (t *statisticsTotals) add(v statisticsTotals) {
	t.Requests += v.Requests
	t.LocalBackoffRequests += v.LocalBackoffRequests
	t.UpstreamRequests += v.UpstreamRequests
	t.UpstreamRequestsUnknown += v.UpstreamRequestsUnknown
	t.PromptTokens += v.PromptTokens
	t.CompletionTokens += v.CompletionTokens
	t.CachedTokens += v.CachedTokens
	t.RateTokens += v.RateTokens
	t.RateDurationMS += v.RateDurationMS
}

type statisticsMeta struct {
	RateMinDurationMS  int64     `json:"rate_min_duration_ms"`
	RateHistoryPartial bool      `json:"rate_history_partial,omitempty"`
	Version            int       `json:"version"`
	Generation         string    `json:"generation"`
	Since              time.Time `json:"since"`
	ImportedHistory    bool      `json:"imported_history"`
}

type statisticsState struct {
	meta  statisticsMeta
	days  map[string]map[string]statisticsTotals
	dirty map[string]bool
}

func (s *Service) statisticsMetaPath() string {
	return filepath.Join(s.cfg.DataDir, "statistics.json")
}

func (s *Service) statisticsDayPath(day string) string {
	return filepath.Join(s.cfg.DataDir, "statistics", s.statistics.meta.Generation, day+".json")
}

func (s *Service) loadStatisticsLocked() error {
	if s.statisticsLoaded {
		return nil
	}
	state := statisticsState{days: map[string]map[string]statisticsTotals{}, dirty: map[string]bool{}}
	b, err := os.ReadFile(s.statisticsMetaPath())
	if err == nil {
		if err = json.Unmarshal(b, &state.meta); err != nil {
			return fmt.Errorf("read statistics metadata: %w", err)
		}
		_, keyErr := hex.DecodeString(state.meta.Generation)
		if state.meta.Version != 1 || len(state.meta.Generation) != 24 || keyErr != nil || state.meta.Since.IsZero() {
			return fmt.Errorf("invalid statistics metadata")
		}
		dir := filepath.Join(s.cfg.DataDir, "statistics", state.meta.Generation)
		files, readErr := os.ReadDir(dir)
		if readErr != nil && !os.IsNotExist(readErr) {
			return readErr
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
				continue
			}
			day := strings.TrimSuffix(file.Name(), ".json")
			if _, err := time.Parse("2006-01-02", day); err != nil {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, file.Name()))
			if err != nil {
				return err
			}
			var buckets map[string]statisticsTotals
			if err := json.Unmarshal(data, &buckets); err != nil {
				return fmt.Errorf("read statistics day %s: %w", day, err)
			}
			for minute := range buckets {
				ts, err := time.Parse(time.RFC3339, minute)
				if err != nil || ts.UTC().Format("2006-01-02") != day {
					return fmt.Errorf("invalid statistics minute in %s", day)
				}
			}
			state.days[day] = buckets
		}
		s.statistics, s.statisticsLoaded = state, true
		if state.meta.RateMinDurationMS != minAverageRateDurationMS {
			return s.migrateAverageRatesLocked()
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	state.meta = statisticsMeta{Version: 1, Generation: id(), Since: time.Now().UTC(), ImportedHistory: len(s.logs) > 0, RateMinDurationMS: minAverageRateDurationMS}
	for _, e := range s.logs {
		if completed := historicalCompletionTime(e); !completed.IsZero() && completed.Before(state.meta.Since) {
			state.meta.Since = completed
		}
	}
	s.statistics, s.statisticsLoaded = state, true
	// Bootstrap only once. A durable generation pointer prevents a reset/restart
	// from reimporting retained request logs.
	for _, e := range s.logs {
		s.addStatisticsEntryLocked(e, historicalCompletionTime(e))
	}
	s.flushStatisticsLocked()
	if s.statisticsWriteError != "" {
		s.statisticsLoaded = false
		return fmt.Errorf("initialize statistics: %s", s.statisticsWriteError)
	}
	if err := atomicJSON(s.statisticsMetaPath(), state.meta); err != nil {
		s.statisticsLoaded = false
		return err
	}
	return nil
}

func (s *Service) addStatisticsEntryLocked(e LogEntry, at time.Time) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	day := at.UTC().Format("2006-01-02")
	minute := at.UTC().Truncate(time.Minute).Format(time.RFC3339)
	if s.statistics.days[day] == nil {
		s.statistics.days[day] = map[string]statisticsTotals{}
	}
	v := statisticsTotals{Requests: logRequestCount(e)}
	if e.UpstreamSkipped {
		v.LocalBackoffRequests = v.Requests
	} else {
		// Count calls, including fallback attempts, only when the executor
		// actually reached the upstream transport.
		if e.UpstreamAttempts != nil {
			v.UpstreamRequests = int64(*e.UpstreamAttempts)
		} else {
			// Older failEarly records contain a synthetic Attempt even before
			// opening a transport. Their upstream call count is unmeasured.
			v.UpstreamRequestsUnknown = v.Requests
		}
		v.PromptTokens, v.CompletionTokens, v.CachedTokens = e.PromptTokens, e.CompletionTokens, e.CachedTokens
		v.RateTokens, v.RateDurationMS = averageOutputRateSample(e)
	}
	total := s.statistics.days[day][minute]
	total.add(v)
	s.statistics.days[day][minute] = total
	s.statistics.dirty[day] = true
}

func (s *Service) recordStatisticsLocked(e LogEntry) {
	if err := s.loadStatisticsLocked(); err != nil {
		s.statisticsWriteError = safeError(err)
		return
	}
	// Live traffic is counted when it finishes, so resetting statistics also
	// has a well-defined boundary for already-running requests.
	s.addStatisticsEntryLocked(e, historicalCompletionTime(e))
}

func (s *Service) flushStatisticsLocked() {
	if !s.statisticsLoaded {
		return
	}
	for day := range s.statistics.dirty {
		if err := atomicJSON(s.statisticsDayPath(day), s.statistics.days[day]); err != nil {
			s.statisticsWriteError = safeError(err)
			return
		}
		delete(s.statistics.dirty, day)
	}
	s.statisticsWriteError = ""
}

func statisticsRange(r ManagementRequest) (time.Time, time.Time, error) {
	var start, end time.Time
	for _, item := range []struct {
		name string
		dst  *time.Time
	}{{"start", &start}, {"end", &end}} {
		if value := r.Query.Get(item.name); value != "" {
			ts, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return start, end, fmt.Errorf("%s must be an RFC3339 timestamp", item.name)
			}
			*item.dst = ts.UTC().Truncate(time.Minute)
			if item.name == "end" && !ts.Equal(*item.dst) {
				*item.dst = item.dst.Add(time.Minute)
			}
		}
	}
	if !start.IsZero() && !end.IsZero() && !end.After(start) {
		return start, end, fmt.Errorf("end must be after start")
	}
	return start, end, nil
}

func (s *Service) statisticsResponse(r ManagementRequest) (any, error) {
	start, end, err := statisticsRange(r)
	if err != nil {
		return managementJSON(400, map[string]any{"error": err.Error()})
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var total statisticsTotals
	for _, buckets := range s.statistics.days {
		for minute, value := range buckets {
			ts, _ := time.Parse(time.RFC3339, minute)
			if (!start.IsZero() && ts.Before(start)) || (!end.IsZero() && !ts.Before(end)) {
				continue
			}
			total.add(value)
		}
	}
	var cacheRate, rate any
	if total.PromptTokens > 0 {
		cacheRate = float64(total.CachedTokens) / float64(total.PromptTokens)
	}
	if total.RateDurationMS > 0 {
		rate = float64(total.RateTokens) * 1000 / float64(total.RateDurationMS)
	}
	out := map[string]any{
		"requests": total.Requests, "local_backoff_requests": total.LocalBackoffRequests, "upstream_requests": total.UpstreamRequests,
		"upstream_requests_unknown": total.UpstreamRequestsUnknown,
		"prompt_tokens":             total.PromptTokens, "completion_tokens": total.CompletionTokens, "cached_tokens": total.CachedTokens,
		"total_tokens": total.PromptTokens + total.CompletionTokens, "cache_rate": cacheRate, "output_tokens_per_second": rate,
		"since": s.statistics.meta.Since, "imported_history": s.statistics.meta.ImportedHistory, "persistence_error": s.statisticsWriteError,
		"resolution":           "minute",
		"rate_history_partial": s.statistics.meta.RateHistoryPartial,
	}
	if !start.IsZero() {
		out["start"] = start
	}
	if !end.IsZero() {
		out["end"] = end
	}
	return managementJSON(200, out)
}

func (s *Service) resetStatistics(r ManagementRequest) (any, error) {
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if json.Unmarshal(r.Body, &body) != nil || !body.Confirm {
		return managementJSON(400, map[string]any{"error": "请确认清空统计数据"})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushStatisticsLocked()
	if s.statisticsWriteError != "" {
		return managementJSON(500, map[string]any{"error": "统计写入失败，请稍后重试"})
	}
	meta := statisticsMeta{Version: 1, Generation: id(), Since: time.Now().UTC(), RateMinDurationMS: minAverageRateDurationMS}
	// Switch generations atomically. The previous generation is the rollback
	// copy; no request logs, quota ledger, or credentials are deleted.
	if err := atomicJSON(s.statisticsMetaPath(), meta); err != nil {
		return managementJSON(500, map[string]any{"error": safeError(err)})
	}
	s.statistics = statisticsState{meta: meta, days: map[string]map[string]statisticsTotals{}, dirty: map[string]bool{}}
	s.statisticsLoaded = true
	return managementJSON(200, map[string]any{"ok": true, "since": meta.Since})
}
