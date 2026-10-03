package bridge

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAverageRateCutoff(t *testing.T) {
	s := observabilityService(t, "")
	for _, e := range []LogEntry{
		{ID: "short", DurationMS: 1499, TTFTMS: 1000, PromptTokens: 10, CompletionTokens: 1000},
		{ID: "boundary", DurationMS: 1500, TTFTMS: 1000, PromptTokens: 10, CompletionTokens: 50},
	} {
		s.appendLog(e)
	}
	v := observabilityStatistics(t, s, nil)
	observabilityCounts(t, v, 2, 0, 0, 2, 20, 1050, 0)
	observabilityRate(t, v.OutputRate, 100)
	for _, e := range observabilityLogs(t, s, nil).Items {
		if e.CompletedAt == nil || e.OutputTokensPerSecond == nil {
			t.Fatal("per-request completion time and raw rate must remain available")
		}
	}
}

func TestAverageRateMigrationPreservesCounters(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete history", true: "pruned history"}[missing], func(t *testing.T) {
			dir := t.TempDir()
			at := time.Date(2026, 10, 3, 10, 0, 10, 0, time.UTC)
			minute := at.Truncate(time.Minute).Format(time.RFC3339)
			meta := statisticsMeta{Version: 1, Generation: "0123456789abcdef01234567", Since: at.Add(-time.Second)}
			logs := []LogEntry{
				{ID: "short", Time: at.Add(-1499 * time.Millisecond), DurationMS: 1499, TTFTMS: 1000, PromptTokens: 10, CompletionTokens: 1000},
				{ID: "boundary", Time: at.Add(-1500 * time.Millisecond), DurationMS: 1500, TTFTMS: 1000, PromptTokens: 10, CompletionTokens: 50},
			}
			if missing {
				logs = logs[1:]
			}
			// Retained pre-reset detail must not be imported into the new generation.
			logs = append(logs, LogEntry{ID: "before-reset", Time: at.Add(-time.Hour), DurationMS: 1000, CompletionTokens: 999})
			observabilitySeedLogs(t, dir, logs)
			old := statisticsTotals{Requests: 2, UpstreamRequestsUnknown: 2, PromptTokens: 20, CompletionTokens: 1050, RateTokens: 1050, RateDurationMS: 999}
			if err := atomicJSON(filepath.Join(dir, "statistics.json"), meta); err != nil {
				t.Fatal(err)
			}
			if err := atomicJSON(filepath.Join(dir, "statistics", meta.Generation, "2026-10-03.json"), map[string]statisticsTotals{minute: old}); err != nil {
				t.Fatal(err)
			}
			s := observabilityService(t, dir)
			v := observabilityStatistics(t, s, nil)
			observabilityCounts(t, v, 2, 0, 0, 2, 20, 1050, 0)
			if missing {
				if v.OutputRate != nil {
					t.Fatalf("pruned history guessed a rate: %v", v.OutputRate)
				}
			} else {
				observabilityRate(t, v.OutputRate, 100)
			}
			if s.statistics.meta.RateHistoryPartial != missing || s.statistics.meta.RateMinDurationMS != 500 || s.statistics.meta.Generation != meta.Generation || !s.statistics.meta.Since.Equal(meta.Since) {
				t.Fatal("migration metadata or reset boundary changed")
			}
			observabilityShutdown(t, s)
			reloaded := observabilityService(t, dir)
			observabilityCounts(t, observabilityStatistics(t, reloaded, nil), 2, 0, 0, 2, 20, 1050, 0)
		})
	}
}
