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
const transportIncompleteKind = "upstream_transport_incomplete"
const upstreamStreamIdleTimeoutKind = "upstream_stream_idle_timeout"

func streamIdleTimeout(timeout time.Duration) error {
	return &upstreamError{
		APIError: &APIError{Status: 502, Kind: upstreamStreamIdleTimeoutKind, Message: fmt.Sprintf("HTTP 502: upstream stream i/o timeout after %gs without effective output (%s)", timeout.Seconds(), upstreamStreamIdleTimeoutKind)},
		Scope:    "request",
	}
}

var wrappedRateLimit = regexp.MustCompile(`(?is)request failed with status 429:\s*.*rate limit exceeded`)
var wrappedBadRequest = regexp.MustCompile(`(?i)\brequest failed with status\s+400\s*:\s*`)
var wrappedRequestParam = regexp.MustCompile(`^\.?messages(?:\[[0-9]+\]|\.[a-zA-Z0-9_]+)+$`)
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
	TransportKind  string // Classified at the transport source, never inferred from Message.
	RetryAt        time.Time
	Scope          string
	Local          bool
	WindowID       string
	Provider       string
	ProviderSource string
	QuotaType      string
	QuotaResetAt   *time.Time
	RequestDetails *upstreamRequestDetails
}

type upstreamRequestDetails struct {
	Param   string
	Message string
}

func (e *upstreamError) IsRequestScoped() bool {
	return e != nil && !faultPolicyFor(e).Cooldown
}

// Only the provider APIConnectionError with an explicit truncated-response
// signature is exempt. A generic 502, EOF or connection error is insufficient.
func isProviderTransportIncomplete(body map[string]any) bool {
	detail := object(body["error"])
	text := strings.ToLower(errorMessage(body))
	identity := text + " " + strings.ToLower(str(detail["code"])+" "+str(detail["type"]))
	provider, _ := routingFacts(body)
	if !strings.Contains(identity, "apiconnectionerror") ||
		(!strings.Contains(identity, "fireworks") && !strings.Contains(identity, "litellm") && !strings.Contains(strings.ToLower(provider), "fireworks")) {
		return false
	}
	return strings.Contains(text, "transferencodingerror") ||
		strings.Contains(text, "response payload is not completed") ||
		strings.Contains(text, "response payload incomplete") ||
		strings.Contains(text, "response payload is incomplete") ||
		strings.Contains(text, "upstream payload incomplete")
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

func hasUpstreamErrorStatus(body map[string]any, except int) bool {
	detail := object(body["error"])
	for _, value := range []any{detail["status"], detail["status_code"], body["status"], body["status_code"]} {
		n := number(value)
		if text, ok := value.(string); ok {
			n, _ = strconv.ParseInt(text, 10, 64)
		}
		if n >= 400 && n <= 599 && int(n) != except {
			return true
		}
	}
	return false
}

// A successful SSE response can hide the provider's HTTP 400 inside a gateway
// message. Accept only its complete JSON error envelope and a recognized request
// identity, never a bare "400" or "Invalid input" in arbitrary error text.
func wrappedUpstreamRequestError(message string) *upstreamRequestDetails {
	if len(message) > 64<<10 {
		return nil
	}
	for depth := 0; depth < 4; depth++ {
		prefix := wrappedBadRequest.FindStringIndex(message)
		if prefix == nil {
			return nil
		}
		body, err := decodeObject([]byte(message[prefix[1]:]))
		if err != nil || object(body["error"]) == nil || hasUpstreamErrorStatus(body, 400) {
			return nil
		}
		message = errorMessage(body)
		if protectedWrappedUpstreamError(body) {
			return nil
		}
		if isUpstreamRequestError(0, body) {
			return &upstreamRequestDetails{Param: str(object(body["error"])["param"]), Message: message}
		}
		if request := wrappedAIAPICallRequestError(body); request != nil {
			return request
		}
	}
	return nil
}

func protectedWrappedUpstreamError(body map[string]any) bool {
	codes := strings.Join(upstreamErrorCodes(body), " ")
	return isUpstreamCredentialError(body) || isUpstreamModelError(body) || isProviderTransportIncomplete(body) ||
		quotaErrorType(codes, errorMessage(body)) != "" || strings.Contains(codes, "rate_limit")
}

// The AI SDK's generic exception name is not itself a request-error identity.
// Only inside the complete HTTP-400 wrapper, require its serialized API-call
// metadata to confirm the same status before stopping host route fallback.
func wrappedAIAPICallRequestError(body map[string]any) *upstreamRequestDetails {
	detail := object(body["error"])
	param := object(detail["param"])
	if param == nil {
		return nil
	}
	identified := false
	for _, value := range []any{detail["type"], detail["name"], param["name"]} {
		identified = identified || strings.EqualFold(strings.TrimSpace(str(value)), "AI_APICallError")
	}
	status, err := strconv.ParseInt(fmt.Sprint(param["statusCode"]), 10, 64)
	if !identified || err != nil || status != http.StatusBadRequest || hasUpstreamErrorStatus(map[string]any{"error": param}, 400) {
		return nil
	}
	message := errorMessage(body)
	paramError := map[string]any{"error": map[string]any{
		"code": param["code"], "type": param["type"],
		"message": strings.Join([]string{message, str(param["error"]), str(param["message"])}, " "),
	}}
	if protectedWrappedUpstreamError(paramError) {
		return nil
	}
	if strings.TrimSpace(message) == "" {
		message = str(param["error"])
		if strings.TrimSpace(message) == "" {
			message = str(param["message"])
		}
	}
	request := &upstreamRequestDetails{Message: message}
	if path, reason, ok := strings.Cut(message, ":"); ok && wrappedRequestParam.MatchString(strings.TrimSpace(path)) {
		request.Param = strings.TrimPrefix(strings.TrimSpace(path), ".")
		request.Message = strings.TrimSpace(reason)
	}
	return request
}

func classifyUpstreamError(httpStatus int, headers http.Header, body map[string]any, now time.Time, fallback ...int) error {
	message := errorMessage(body)
	detail := object(body["error"])
	code := strings.Join(upstreamErrorCodes(body), " ")
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
	// Some gateways wrap a real auth/quota/rate failure in HTTP 200 or 5xx.
	// Explicit protected statuses win over the generic service-failure policy.
	if status != 401 && status != 402 && status != 403 && status != 429 {
		for _, value := range []any{detail["status"], detail["status_code"], body["status"], body["status_code"]} {
			n := number(value)
			if text, ok := value.(string); ok {
				n, _ = strconv.ParseInt(text, 10, 64)
			}
			if n == 401 || n == 402 || n == 403 || n == 429 {
				status = int(n)
				break
			}
		}
	}
	if status >= 500 && status <= 599 {
		for _, value := range []any{detail["code"], detail["type"], body["code"], body["type"]} {
			switch strings.ToLower(str(value)) {
			case "authentication_error", "invalid_api_key", "invalid_grant", "invalid_token", "expired_token", "token_expired", "invalid_client", "unauthorized", "unauthorized_client", "account_deactivated", "account_disabled", "account_suspended":
				status = 401
			case "permission_error", "permission_denied", "insufficient_permissions", "access_denied":
				status = 403
			case "rate_limit_error", "rate_limit_exceeded":
				status = 429
			}
			if status < 500 {
				break
			}
		}
	}
	team := strings.Contains(lower, "this team's limit") && strings.Contains(lower, "tokens per minute")
	quotaType := quotaErrorType(code, message)
	quota := !team && quotaType != "" && status != 401 && status != 403
	rate := !quota && (status == 429 || strings.Contains(code, "rate_limit_exceeded") || wrappedRateLimit.MatchString(message))
	// An actual authentication response must never become an exempt rate limit.
	if status == 401 || status == 402 || status == 403 {
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
		result.Scope, result.QuotaType = "account", quotaType
		result.QuotaResetAt = quotaResetTime(headers, message, now)
	case status == 401 || status == 403:
		kind = "upstream_auth_error"
	case status != 402 && isUpstreamCredentialError(body):
		kind, result.Scope = "upstream_auth_error", "account"
	case status != 402 && isUpstreamModelError(body):
		kind, result.Scope = upstreamModelKind, "credential_model"
	case status != 402 && isProviderTransportIncomplete(body):
		status, result.UpstreamStatus, kind = 502, 502, transportIncompleteKind
		result.Scope = "request"
	case status != 402 && isUpstreamRequestError(status, body):
		kind, result.Scope = upstreamRequestKind, "request"
	case status >= 500 && status <= 599:
		kind, result.Scope = upstreamServiceKind, "request"
		if httpStatus == http.StatusOK && !hasUpstreamErrorStatus(body, 0) {
			if request := wrappedUpstreamRequestError(message); request != nil {
				status, result.UpstreamStatus, kind = http.StatusBadRequest, http.StatusBadRequest, upstreamRequestKind
				result.RequestDetails = request
			}
		}
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
	entry.ErrorPhase, entry.CooldownDisposition = "", ""
	if err != nil {
		entry.ErrorPhase = "before_output"
		if entry.DownstreamStarted != nil && *entry.DownstreamStarted {
			entry.ErrorPhase = "after_output"
		}
		entry.CooldownDisposition = "host_policy"
		if !faultPolicyFor(err).Cooldown {
			entry.CooldownDisposition = "legacy_host_unknown"
			if entry.StreamErrorDetailsSupported != nil && *entry.StreamErrorDetailsSupported {
				entry.CooldownDisposition = "exempt"
			}
		}
	}
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

func (s *Service) logErrorDetails(entry *LogEntry, attempt *Attempt, err error) {
	logErrorDetails(entry, attempt, err)
	if err == nil {
		return
	}
	entry.CooldownDisposition = "host_policy"
	if !s.hostFaultPolicy(err).Cooldown {
		entry.CooldownDisposition = "legacy_host_unknown"
		if entry.StreamErrorDetailsSupported != nil && *entry.StreamErrorDetailsSupported {
			entry.CooldownDisposition = "exempt"
		}
	}
}
