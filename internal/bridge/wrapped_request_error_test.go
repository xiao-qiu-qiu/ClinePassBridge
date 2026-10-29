package bridge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const wrappedBadRequestFixture = `Failed to create stream: inference request failed: failed to generate stream from Vercel: failed to invoke model 'fixture-model': request failed with status 400: {"error":{"message":"Invalid input","type":"invalid_request_error","param":"messages.19.content","code":"invalid_request_error"}}`
const wrappedAIAPICallFixture = `Failed to create stream: inference request failed: failed to generate stream from Vercel: failed to invoke model 'fixture-model' with streaming: request failed with status 400: {"error":{"message":".messages[11]: Invalid base64 data (request_id: fixture-request)","type":"AI_APICallError","param":{"error":".messages[11]: Invalid base64 data (request_id: fixture-request)","statusCode":400,"name":"AI_APICallError","message":".messages[11]: Invalid base64 data (request_id: fixture-request)","isRetryable":false,"type":"AI_APICallError"}},"providerMetadata":{"gateway":{"routing":{"finalProvider":"fixture-provider"}}}}`

func TestWrappedUpstreamRequestErrorClassification(t *testing.T) {
	multiGateway := "request failed with status 400: " + string(jsonBytes(map[string]any{"error": map[string]any{
		"type": "gateway_error", "message": wrappedBadRequestFixture,
	}}))
	for _, tc := range []struct {
		name, message, code, kind  string
		httpStatus, explicit, want int
	}{
		{"observed SSE error", wrappedBadRequestFixture, "", upstreamRequestKind, 200, 0, 400},
		{"another gateway envelope", multiGateway, "", upstreamRequestKind, 200, 0, 400},
		{"malformed nested JSON", `request failed with status 400: {"error":{"type":"invalid_request_error"}`, "", upstreamServiceKind, 200, 0, 502},
		{"plain similar text", "request failed with status 400: Invalid input messages.19.content", "", upstreamServiceKind, 200, 0, 502},
		{"missing status wrapper", `invalid input: {"error":{"type":"invalid_request_error"}}`, "", upstreamServiceKind, 200, 0, 502},
		{"non-400 wrapper", strings.Replace(wrappedBadRequestFixture, "status 400:", "status 422:", 1), "", upstreamServiceKind, 200, 0, 502},
		{"conflicting nested status", `request failed with status 400: {"error":{"type":"invalid_request_error","status":503}}`, "", upstreamServiceKind, 200, 0, 502},
		{"unknown nested type", `request failed with status 400: {"error":{"message":"Invalid input","type":"gateway_error"}}`, "", upstreamServiceKind, 200, 0, 502},
		{"trailing text", wrappedBadRequestFixture + " another error", "", upstreamServiceKind, 200, 0, 502},
		{"nested authentication", `request failed with status 400: {"error":{"type":"invalid_request_error","code":"invalid_api_key"}}`, "", upstreamServiceKind, 200, 0, 502},
		{"HTTP authentication", wrappedBadRequestFixture, "", "upstream_auth_error", 401, 0, 401},
		{"HTTP payment", wrappedBadRequestFixture, "", "upstream_error", 402, 0, 402},
		{"HTTP permission", wrappedBadRequestFixture, "", "upstream_auth_error", 403, 0, 403},
		{"HTTP rate limit", wrappedBadRequestFixture, "", "upstream_rate_limited", 429, 0, 429},
		{"structured authentication", wrappedBadRequestFixture, "invalid_api_key", "upstream_auth_error", 200, 0, 401},
		{"structured quota", wrappedBadRequestFixture, "insufficient_quota", "upstream_quota_exhausted", 200, 0, 502},
		{"structured protected status", wrappedBadRequestFixture, "", "upstream_auth_error", 200, 403, 403},
		{"real service HTTP status", wrappedBadRequestFixture, "", upstreamServiceKind, 503, 0, 503},
		{"explicit service status", wrappedBadRequestFixture, "", upstreamServiceKind, 200, 503, 503},
		{"provider transport", transportIncompleteFixture + " " + wrappedBadRequestFixture, "", transportIncompleteKind, 200, 0, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail := map[string]any{"message": tc.message}
			if tc.code != "" {
				detail["code"] = tc.code
			}
			if tc.explicit != 0 {
				detail["status"] = tc.explicit
			}
			body := map[string]any{"error": detail}
			upstream := classifyUpstreamResponseError(tc.httpStatus, nil, body, jsonBytes(body), time.Now()).(*upstreamError)
			if upstream.Status != tc.want || upstream.UpstreamStatus != tc.want || upstream.HTTPStatus != tc.httpStatus || upstream.Kind != tc.kind {
				t.Fatalf("classification = %#v", upstream)
			}
			if upstream.Message != tc.message {
				t.Fatalf("original error changed: %q", upstream.Message)
			}
			if tc.kind == upstreamRequestKind {
				policy := faultPolicyFor(upstream)
				if upstream.Scope != "request" || !policy.Stop || policy.Cooldown {
					t.Fatalf("request fault acquired retry/cooldown: %#v %#v", upstream, policy)
				}
				if upstream.RequestDetails == nil || upstream.RequestDetails.Param != "messages.19.content" || upstream.RequestDetails.Message != "Invalid input" {
					t.Fatalf("nested parameter diagnostics lost: %#v", upstream.RequestDetails)
				}
			}
		})
	}
}

func TestExecuteWrappedUpstreamRequestErrorStopsRPCAndPreservesLog(t *testing.T) {
	for _, tc := range []struct {
		name, fixture, param, reason string
	}{
		{"standard request error", wrappedBadRequestFixture, "messages.19.content", "Invalid input"},
		{"AI API call image error", wrappedAIAPICallFixture, "messages[11]", "Invalid base64 data (request_id: fixture-request)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := registeredService(t, "stream-aggregate")
			s.errorRules = nil
			h := newFakeHost(ssePlan(sseFrame(map[string]any{"error": map[string]any{
				"code": "stream_initialization_failed", "type": "stream_error", "message": tc.fixture, "request_id": "fixture-request",
			}})))
			s.SetHost(h.call)
			_, err := s.Handle("executor.execute", executorRequest(""))
			if err == nil {
				t.Fatal("upstream rejection was treated as a completion")
			}
			policy := s.hostFaultPolicy(err)
			if !policy.Stop || policy.Cooldown || policy.Explicit || policy.Scope != "request" {
				t.Fatalf("default RPC policy = %#v", policy)
			}
			for _, enabled := range []bool{true, false} {
				s.cfg.ClientRetryCompatibility = enabled
				message, status := s.HostErrorDetails(err)
				var wire struct {
					Error struct{ Type, Code, Message string }
				}
				if parseErr := json.Unmarshal([]byte(message), &wire); parseErr != nil || status != 400 ||
					wire.Error.Type != "invalid_request_error" || wire.Error.Code != "invalid_request_error" ||
					!strings.Contains(wire.Error.Message, "HTTP 400:") || !strings.Contains(wire.Error.Message, tc.param+": "+tc.reason) ||
					!strings.Contains(wire.Error.Message, "Check the message content") || !strings.Contains(wire.Error.Message, "supported input types") ||
					strings.Contains(message, "Vercel") || strings.Contains(message, "failed to invoke") || strings.Contains(message, "cpa_compat") {
					t.Fatalf("compatibility=%v RPC encoding status=%d message=%q parse=%v", enabled, status, message, parseErr)
				}
			}
			if len(h.opened) != 1 || !h.opened[0] || len(h.emitted) != 0 || len(s.logs) != 1 {
				t.Fatalf("aggregate rejection retried or emitted: requests=%v chunks=%d logs=%d", h.opened, len(h.emitted), len(s.logs))
			}
			entry := s.logs[0]
			if entry.Status != 400 || entry.ErrorKind != upstreamRequestKind || entry.UpstreamHTTPStatus != 200 ||
				entry.UpstreamErrorStatus != 400 || entry.Error != safeError(err) || entry.ErrorPhase != "before_output" ||
				entry.DownstreamStarted == nil || *entry.DownstreamStarted || entry.EffectiveFrameCount != 0 ||
				entry.UpstreamAttempts == nil || *entry.UpstreamAttempts != 1 {
				t.Fatalf("aggregate rejection lost diagnostics: %#v", entry)
			}
			if len(entry.Attempts) != 1 || entry.Attempts[0].Error != safeError(err) || entry.Attempts[0].Status != 400 {
				t.Fatalf("attempt diagnostics changed: %#v", entry.Attempts)
			}
			// Explicit user rules still override the default stop policy.
			s.errorRules = []RequestErrorRule{{Status: 400, Match: []string{tc.reason}, Action: "continue"}}
			if policy = s.hostFaultPolicy(err); !policy.Explicit || policy.Stop || policy.Cooldown {
				t.Fatalf("explicit continue lost priority: %#v", policy)
			}
		})
	}
}

func TestWrappedAIAPICallRequestErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name, message              string
		httpStatus, explicit, want int
	}{
		{"observed image failure", wrappedAIAPICallFixture, 200, 0, 400},
		{"identity in name", strings.Replace(wrappedAIAPICallFixture, `"type":"AI_APICallError",`, "", 1), 200, 0, 400},
		{"string status", strings.Replace(wrappedAIAPICallFixture, `"statusCode":400`, `"statusCode":"400"`, 1), 200, 0, 400},
		{"missing structured status", strings.Replace(wrappedAIAPICallFixture, `"statusCode":400,`, "", 1), 200, 0, 502},
		{"fractional status", strings.Replace(wrappedAIAPICallFixture, `"statusCode":400`, `"statusCode":400.5`, 1), 200, 0, 502},
		{"structured service status", strings.Replace(wrappedAIAPICallFixture, `"statusCode":400`, `"statusCode":503`, 1), 200, 0, 502},
		{"unknown identity", strings.ReplaceAll(wrappedAIAPICallFixture, "AI_APICallError", "gateway_error"), 200, 0, 502},
		{"malformed envelope", strings.TrimSuffix(wrappedAIAPICallFixture, "}"), 200, 0, 502},
		{"trailing text", wrappedAIAPICallFixture + " another error", 200, 0, 502},
		{"real service HTTP status", wrappedAIAPICallFixture, 503, 0, 503},
		{"explicit service status", wrappedAIAPICallFixture, 200, 503, 503},
		{"nested authentication", strings.Replace(wrappedAIAPICallFixture, `"statusCode":400`, `"statusCode":400,"code":"invalid_api_key"`, 1), 200, 0, 502},
		{"nested quota", strings.Replace(wrappedAIAPICallFixture, `"statusCode":400`, `"statusCode":400,"code":"insufficient_quota"`, 1), 200, 0, 502},
		{"nested rate limit", strings.Replace(wrappedAIAPICallFixture, `"statusCode":400`, `"statusCode":400,"code":"rate_limit_error"`, 1), 200, 0, 502},
		{"nested model error", strings.Replace(wrappedAIAPICallFixture, `"statusCode":400`, `"statusCode":400,"code":"model_not_found"`, 1), 200, 0, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail := map[string]any{"code": "stream_initialization_failed", "type": "stream_error", "message": tc.message, "request_id": "fixture-request"}
			if tc.explicit != 0 {
				detail["status"] = tc.explicit
			}
			body := map[string]any{"error": detail}
			upstream := classifyUpstreamResponseError(tc.httpStatus, nil, body, jsonBytes(body), time.Now()).(*upstreamError)
			kind := upstreamServiceKind
			if tc.want == 400 {
				kind = upstreamRequestKind
			}
			if upstream.Status != tc.want || upstream.UpstreamStatus != tc.want || upstream.HTTPStatus != tc.httpStatus || upstream.Kind != kind {
				t.Fatalf("classification status=%d upstream=%d HTTP=%d kind=%s", upstream.Status, upstream.UpstreamStatus, upstream.HTTPStatus, upstream.Kind)
			}
			if upstream.Message != tc.message {
				t.Fatal("original error diagnostics changed")
			}
			if tc.want == 400 {
				if upstream.RequestDetails == nil || upstream.RequestDetails.Param != "messages[11]" || upstream.RequestDetails.Message != "Invalid base64 data (request_id: fixture-request)" {
					t.Fatalf("image parameter diagnostics = %#v", upstream.RequestDetails)
				}
				policy := faultPolicyFor(upstream)
				if upstream.Scope != "request" || !policy.Stop || policy.Cooldown || !upstream.RetryAt.IsZero() {
					t.Fatalf("image request acquired fallback/cooldown: %#v", policy)
				}
			}
		})
	}
	// An AI SDK exception alone is not a request-error identity globally.
	upstream := classifyUpstreamError(200, nil, map[string]any{"error": map[string]any{
		"type": "AI_APICallError", "message": "provider request failed", "param": map[string]any{"statusCode": 400},
	}}, time.Now()).(*upstreamError)
	if upstream.Kind != upstreamServiceKind || upstream.Status != 502 {
		t.Fatalf("unwrapped AI SDK error became a request fault: %#v", upstream)
	}
}

func TestWrappedUpstreamRequestClientMessageBoundsAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		name, reason, param, want string
	}{
		{"missing reason and param", "", "", "Invalid request parameters"},
		{"long reason and secret", "Invalid input Bearer fixture-secret " + strings.Repeat("detail ", 150), "messages.19.content", "Invalid input [REDACTED]"},
		{"long parameter and secret", "Invalid input", "messages.19.content sk-fixture-secret " + strings.Repeat("field", 100), "messages.19.content [REDACTED]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := "request failed with status 400: " + string(jsonBytes(map[string]any{"error": map[string]any{
				"type": "invalid_request_error", "message": tc.reason, "param": tc.param,
			}}))
			err := classifyUpstreamError(200, nil, map[string]any{"error": original}, time.Now())
			s := NewService()
			for _, enabled := range []bool{true, false} {
				s.cfg.ClientRetryCompatibility = enabled
				message, status := s.HostErrorDetails(err)
				var wire struct{ Error struct{ Message string } }
				if parseErr := json.Unmarshal([]byte(message), &wire); parseErr != nil || status != 400 || len(wire.Error.Message) > 500 ||
					!strings.Contains(wire.Error.Message, tc.want) || !strings.Contains(wire.Error.Message, "supported input types") ||
					strings.Contains(wire.Error.Message, "fixture-secret") || strings.Contains(wire.Error.Message, "request failed with status") {
					t.Fatalf("compatibility=%v client details status=%d message=%q parse=%v", enabled, status, message, parseErr)
				}
				if err.Error() != original {
					t.Fatal("client formatting changed original diagnostics")
				}
			}
		})
	}
}
