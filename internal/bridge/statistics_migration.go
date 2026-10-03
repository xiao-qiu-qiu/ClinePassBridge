package bridge

import "time"

// v0.2.8 only saved total rate tokens/time, so a new per-request cutoff cannot
// be applied to those sums directly. Rebuild rate samples only for minute
// buckets whose complete original traffic is still represented in the logs.
// Request and token counters are never rebuilt or reset by this migration.
func (s *Service) migrateAverageRatesLocked() error {
	candidates := map[string]statisticsTotals{}
	for _, e := range s.logs {
		at := historicalCompletionTime(e)
		if at.IsZero() || at.Before(s.statistics.meta.Since) {
			continue
		}
		minute := at.UTC().Truncate(time.Minute).Format(time.RFC3339)
		v := candidates[minute]
		v.Requests += logRequestCount(e)
		if e.UpstreamSkipped {
			v.LocalBackoffRequests += logRequestCount(e)
		} else {
			v.PromptTokens += e.PromptTokens
			v.CompletionTokens += e.CompletionTokens
			v.CachedTokens += e.CachedTokens
			tokens, ms := averageOutputRateSample(e)
			v.RateTokens += tokens
			v.RateDurationMS += ms
		}
		candidates[minute] = v
	}
	for day, buckets := range s.statistics.days {
		for minute, old := range buckets {
			v := candidates[minute]
			complete := v.Requests == old.Requests && v.LocalBackoffRequests == old.LocalBackoffRequests && v.PromptTokens == old.PromptTokens && v.CompletionTokens == old.CompletionTokens && v.CachedTokens == old.CachedTokens
			if complete {
				old.RateTokens, old.RateDurationMS = v.RateTokens, v.RateDurationMS
			} else {
				// Missing detail is not evidence that every old sample met the cutoff.
				if old.RateTokens > 0 {
					s.statistics.meta.RateHistoryPartial = true
				}
				old.RateTokens, old.RateDurationMS = 0, 0
			}
			buckets[minute] = old
		}
		s.statistics.dirty[day] = true
	}
	s.flushStatisticsLocked()
	if s.statisticsWriteError != "" {
		s.statisticsLoaded = false
		return fail(500, "Average-rate migration could not be persisted: "+s.statisticsWriteError)
	}
	s.statistics.meta.RateMinDurationMS = minAverageRateDurationMS
	if err := atomicJSON(s.statisticsMetaPath(), s.statistics.meta); err != nil {
		s.statisticsLoaded = false
		return err
	}
	return nil
}
