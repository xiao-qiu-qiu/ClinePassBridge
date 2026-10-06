package bridge

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Only retain recognized metadata, never arbitrary request strings/content.
func requestReasoningEffort(payload []byte) string {
	j, err := decodeObject(payload)
	if err != nil {
		return ""
	}
	return objectReasoningEffort(j)
}

func objectReasoningEffort(j map[string]any) string {
	for _, value := range []any{j["reasoning_effort"], object(j["reasoning"])["effort"], object(j["thinking"])["effort"]} {
		v := strings.ToLower(strings.TrimSpace(str(value)))
		switch v {
		case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", "auto":
			return v
		}
	}
	thinking := object(j["thinking"])
	switch str(thinking["type"]) {
	case "disabled":
		return "none"
	case "adaptive":
		return "auto"
	case "enabled":
		if budget := number(thinking["budget_tokens"]); budget > 0 && budget <= 1000000000 {
			return "budget:" + strconv.FormatInt(budget, 10)
		}
		return "enabled"
	}
	return ""
}

// Tokens / effective generation seconds; native non-streaming responses without
// TTFT use end-to-end seconds. Missing/invalid durations have no measurable rate.
func outputRateSample(e LogEntry) (int64, int64) {
	if e.UpstreamSkipped || e.CompletionTokens <= 0 || e.DurationMS <= 0 || e.TTFTMS < 0 || e.TTFTMS >= e.DurationMS {
		return 0, 0
	}
	return e.CompletionTokens, e.DurationMS - e.TTFTMS
}

func outputTokenRate(e LogEntry) *float64 {
	tokens, ms := outputRateSample(e)
	if ms == 0 {
		return nil
	}
	rate := float64(tokens) * 1000 / float64(ms)
	return &rate
}

const minAverageRateDurationMS int64 = 500

func averageOutputRateSample(e LogEntry) (int64, int64) {
	tokens, ms := outputRateSample(e)
	if ms < minAverageRateDurationMS {
		return 0, 0
	}
	return tokens, ms
}

func logRequestCount(e LogEntry) int64 {
	if e.RequestCount > 0 {
		return e.RequestCount
	}
	return 1
}

func logLastTime(e LogEntry) time.Time {
	if e.LastTime != nil && e.LastTime.After(e.Time) {
		return *e.LastTime
	}
	return e.Time
}

func backoffLogKey(e LogEntry) string {
	if !e.UpstreamSkipped || e.CredentialID == "" || e.UpstreamModel == "" {
		return ""
	}
	window := e.BackoffWindowID
	if window == "" && e.RetryAt != nil {
		// Legacy records can only be merged when their exact retry boundary agrees.
		window = "legacy:" + e.RetryAt.UTC().Format(time.RFC3339Nano)
	}
	if window == "" {
		return ""
	}
	return string(jsonBytes([]any{window, e.CredentialID, e.UpstreamModel, e.Model, e.Stream, e.ReasoningEffort, e.ReasoningMapping, e.ErrorKind, e.RateLimitScope, e.Status}))
}

func historicalCompletionTime(e LogEntry) time.Time {
	if e.CompletedAt != nil && !e.CompletedAt.IsZero() {
		return *e.CompletedAt
	}
	t := logLastTime(e)
	if t.IsZero() {
		return t
	}
	if e.DurationMS > 0 && e.DurationMS <= int64((24*time.Hour)/time.Millisecond) {
		t = t.Add(time.Duration(e.DurationMS) * time.Millisecond)
	}
	return t
}

func mergeBackoffLog(logs []LogEntry, e LogEntry) ([]LogEntry, bool) {
	key := backoffLogKey(e)
	if key != "" {
		for i := len(logs) - 1; i >= 0; i-- {
			if backoffLogKey(logs[i]) != key {
				continue
			}
			old := logs[i]
			latest := logLastTime(e)
			lastID := e.ID
			if e.LastRequestID != "" {
				lastID = e.LastRequestID
			}
			if previous := logLastTime(old); previous.After(latest) {
				latest, lastID = previous, old.LastRequestID
				e.Error, e.RetryAt = old.Error, old.RetryAt
			}
			e.RequestCount = logRequestCount(old) + logRequestCount(e)
			e.ID, e.LastRequestID, e.LastTime = old.ID, lastID, &latest
			if old.Time.Before(e.Time) {
				e.Time = old.Time
			}
			e.Attempts, e.OutputTokensPerSecond = nil, nil
			copy(logs[i:], logs[i+1:])
			logs[len(logs)-1] = e
			return logs, true
		}
		e.RequestCount = logRequestCount(e)
		e.Attempts = nil
	}
	return append(logs, e), false
}

func compactBackoffLogs(logs []LogEntry) []LogEntry {
	out := make([]LogEntry, 0, len(logs))
	for _, e := range logs {
		e.OutputTokensPerSecond = outputTokenRate(e)
		out, _ = mergeBackoffLog(out, e)
	}
	return out
}

func (s *Service) scheduleObservationsLocked() {
	if s.observationTimer != nil || s.stopped {
		return
	}
	s.observationTimer = time.AfterFunc(2*time.Second, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.observationTimer = nil
		if !s.stopped {
			s.flushObservationsLocked()
		}
	})
}

// All writers share s.mu: an older snapshot can never overwrite a newer one.
func (s *Service) flushObservationsLocked() {
	if s.logsDirty {
		err := atomicJSON(filepath.Join(s.cfg.DataDir, "requests.json"), s.logs)
		s.logWriteError = safeError(err)
		if err == nil {
			s.logsDirty = false
		}
	}
	s.flushStatisticsLocked()
	if s.logsDirty || len(s.statistics.dirty) > 0 {
		s.scheduleObservationsLocked()
	}
}
