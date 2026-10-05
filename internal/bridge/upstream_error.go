package bridge

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const rateLimitMarker = "[clinepassbridge:team_tpm_limit]"

var wrappedRateLimit = regexp.MustCompile(`(?is)request failed with status 429:\s*.*rate limit exceeded`)
var retrySecondsPattern = regexp.MustCompile(`(?i)retry after\s+(\d+(?:\.\d+)?)\s*s\b`)

// Vercel reports the provider it routed to inside the response body. Failures
// carry the same routing block as successes, but nested in the error text, so
// the structured lookup is only a fallback-safe first attempt.
var routingProviderPattern = regexp.MustCompile(`"(finalProvider|resolvedProvider)"\s*:\s*"([^"]+)"`)

// Upstream errors retain the transport status separately from the error inside
// a successful SSE response. The stable marker is matched by CPA's auth rules.
type upstreamError struct {
	*APIError
	HTTPStatus     int
	UpstreamStatus int
	RetryAt        time.Time
	Scope          string
	Local          bool
	WindowID       string
	Provider       string
	ProviderSource string
}

// responseRoutingProvider recovers the provider from explicit routing metadata.
// Only explicit routing statements count; candidate lists never do.
func responseRoutingProvider(body map[string]any) (string, string) {
	for _, key := range []string{"finalProvider", "resolvedProvider"} {
		for _, namespace := range []string{"provider_metadata", "providerMetadata"} {
			routing := object(object(object(body[namespace])["gateway"])["routing"])
			if p := str(routing[key]); p != "" {
				return p, namespace + ".gateway.routing." + key
			}
		}
	}
	return "", ""
}

func routingFacts(body map[string]any) (string, string) {
	if p, path := responseRoutingProvider(body); p != "" {
		return p, path
	}
	matches := routingProviderPattern.FindAllStringSubmatch(errorMessage(body), -1)
	for _, key := range []string{"finalProvider", "resolvedProvider"} {
		for _, m := range matches {
			if m[1] == key {
				return m[2], "error.message." + key
			}
		}
	}
	return "", ""
}

func classifyUpstreamError(httpStatus int, headers http.Header, body map[string]any, now time.Time, fallback ...int) error {
	message := errorMessage(body)
	detail := object(body["error"])
	code := strings.ToLower(str(detail["code"]) + " " + str(detail["type"]) + " " + str(body["code"]))
	lower := strings.ToLower(message)
	status := httpStatus
	if status < 400 || status > 599 {
		status = 502
		if len(fallback) > 0 {
			status = fallback[0]
		}
		for _, value := range []any{detail["status"], detail["status_code"], body["status"], body["status_code"]} {
			n := number(value)
			if text, ok := value.(string); ok {
				n, _ = strconv.ParseInt(text, 10, 64)
			}
			if n >= 400 && n <= 599 {
				status = int(n)
				break
			}
		}
	}
	team := strings.Contains(lower, "this team's limit") && strings.Contains(lower, "tokens per minute")
	quota := !team && (strings.Contains(code, "insufficient_quota") || strings.Contains(code, "quota_exceeded") ||
		strings.Contains(lower, "quota exhausted") || strings.Contains(lower, "quota exceeded") ||
		strings.Contains(lower, "insufficient credits") || strings.Contains(lower, "insufficient balance"))
	rate := !quota && (status == 429 || strings.Contains(code, "rate_limit_exceeded") || wrappedRateLimit.MatchString(message))
	// An actual authentication response must never become an exempt rate limit.
	if status == 401 || status == 403 {
		rate = false
	}
	result := &upstreamError{HTTPStatus: httpStatus, UpstreamStatus: status}
	kind := "upstream_error"
	switch {
	case rate:
		status, result.UpstreamStatus, kind = 429, 429, "upstream_rate_limited"
		result.Scope = "credential_model"
		if team {
			result.Scope, kind = "upstream_team", "upstream_team_rate_limited"
		}
		result.RetryAt = now.Add(upstreamRetryDelay(headers, message, now))
		if team {
			message = fmt.Sprintf("%s Upstream shared team/region token rate limit. Retry after %ds. %s", rateLimitMarker, retrySeconds(result.RetryAt, now), message)
		}
	case quota:
		kind = "upstream_quota_exhausted"
	case status == 401 || status == 403:
		kind = "upstream_auth_error"
	}
	result.APIError = &APIError{Status: status, Kind: kind, Message: message}
	result.Provider, result.ProviderSource = routingFacts(body)
	return result
}

func upstreamRetryDelay(headers http.Header, message string, now time.Time) time.Duration {
	// Honour ordinary long waits without tying them to the request timeout. Bad
	// or implausible upstream values fall back to a bounded one-minute window.
	parse := func(value string) (time.Duration, bool) {
		seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err == nil && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds >= 0 && seconds <= 86400 {
			return max(time.Second, time.Duration(math.Ceil(seconds))*time.Second), true
		}
		return 0, false
	}
	if value := headers.Get("Retry-After"); value != "" {
		if delay, ok := parse(value); ok {
			return delay
		}
		if date, err := http.ParseTime(value); err == nil && date.After(now) && date.Sub(now) <= 24*time.Hour {
			return date.Sub(now)
		}
	}
	if match := retrySecondsPattern.FindStringSubmatch(message); len(match) == 2 {
		if delay, ok := parse(match[1]); ok {
			return delay
		}
	}
	return time.Minute
}

func retrySeconds(until, now time.Time) int64 {
	return max(1, int64(math.Ceil(until.Sub(now).Seconds())))
}

func asTeamRateLimit(err error) *upstreamError {
	var result *upstreamError
	if errors.As(err, &result) && result.Kind == "upstream_team_rate_limited" {
		return result
	}
	return nil
}

func logErrorDetails(entry *LogEntry, attempt *Attempt, err error) {
	entry.ErrorKind, entry.RateLimitScope = "", ""
	entry.UpstreamHTTPStatus, entry.UpstreamErrorStatus = 0, 0
	entry.UpstreamSkipped, entry.RetryAt = false, nil
	entry.BackoffWindowID = ""
	var detail *upstreamError
	if !errors.As(err, &detail) {
		if err != nil {
			message := err.Error()
			switch {
			case strings.Contains(message, "context canceled"), statusOf(err) == 499:
				entry.ErrorKind = "request_canceled"
			case statusOf(err) == 504:
				entry.ErrorKind = "upstream_timeout"
			case strings.Contains(message, "upstream stream ended"), strings.Contains(message, "before [DONE]"):
				entry.ErrorKind = "upstream_incomplete_stream"
			default:
				entry.ErrorKind = "upstream_error"
			}
			if attempt != nil {
				attempt.ErrorKind = entry.ErrorKind
			}
		}
		return
	}
	entry.ErrorKind = detail.Kind
	entry.UpstreamHTTPStatus = detail.HTTPStatus
	entry.UpstreamErrorStatus = detail.UpstreamStatus
	entry.RateLimitScope = detail.Scope
	entry.UpstreamSkipped = detail.Local
	entry.BackoffWindowID = detail.WindowID
	if !detail.RetryAt.IsZero() {
		entry.RetryAt = &detail.RetryAt
	}
	// A failed request still says which provider refused it. Only fill a gap:
	// a provider already read from a successful frame is more specific.
	provider, source := detail.Provider, detail.ProviderSource
	if provider != "" && (entry.Provider == "" || entry.Provider == "unknown") {
		entry.Provider, entry.ProviderSource = provider, source
	}
	if attempt != nil {
		attempt.ErrorKind = entry.ErrorKind
		attempt.UpstreamHTTPStatus = entry.UpstreamHTTPStatus
		attempt.UpstreamErrorStatus = entry.UpstreamErrorStatus
		attempt.UpstreamSkipped = entry.UpstreamSkipped
		if provider != "" && (attempt.Provider == "" || attempt.Provider == "unknown") {
			attempt.Provider, attempt.ProviderSource = provider, source
		}
	}
}
