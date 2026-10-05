package bridge

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func probeService(t *testing.T) *Service {
	t.Helper()
	s := registeredService(t, "native")
	s.creds["probe-account"] = Credential{ID: "probe-account", Label: "测试账号", APIKey: "test-secret-123"}
	return s
}

func callModelTest(t *testing.T, s *Service) ManagementResponse {
	t.Helper()
	return callModelTestWithReasoning(t, s, "")
}

func callModelTestWithReasoning(t *testing.T, s *Service, effort string) ManagementResponse {
	t.Helper()
	result, err := s.Handle("management.handle", jsonBytes(ManagementRequest{
		Method: "POST", Path: apiBase + "/models/test", HostCallbackID: "probe-callback",
		Body: jsonBytes(map[string]any{"model": "deepseek-flash", "upstream_id": "cline-pass/deepseek-v4.1-flash", "credential_id": "probe-account", "reasoning_effort": effort}),
	}))
	if err != nil {
		t.Fatal(err)
	}
	return result.(ManagementResponse)
}

func TestModelProbeReasoningReachesUpstreamAndLog(t *testing.T) {
	for _, effort := range []string{"", "max", " HIGH ", "none"} {
		t.Run("effort="+effort, func(t *testing.T) {
			s := probeService(t)
			h := newFakeHost(ssePlan(simpleSSE()))
			want := strings.ToLower(strings.TrimSpace(effort))
			s.SetHost(func(method string, payload, out any) error {
				if method == "host.http.do_stream" {
					body, err := decodeObject(payload.(map[string]any)["body"].([]byte))
					if err != nil {
						return err
					}
					value, present := body["reasoning_effort"]
					if str(value) != want || present != (want != "") {
						t.Errorf("upstream reasoning = %v (present %v), want %q", value, present, want)
					}
				}
				return h.call(method, payload, out)
			})
			r := callModelTestWithReasoning(t, s, effort)
			result, err := decodeObject(r.Body)
			if err != nil || r.StatusCode != 200 || result["ok"] != true || result["reasoning_effort"] != want {
				t.Fatalf("probe response: %s; error: %v", r.Body, err)
			}
			if len(h.opened) != 1 || len(s.logs) != 1 || s.logs[0].ReasoningEffort != want {
				t.Fatalf("reasoning log or upstream request missing: %+v", s.logs)
			}
		})
	}
}

func TestModelProbeRejectsInvalidReasoningBeforeUpstream(t *testing.T) {
	s := probeService(t)
	h := newFakeHost()
	s.SetHost(h.call)
	r := callModelTestWithReasoning(t, s, "unsupported")
	if r.StatusCode != 400 || len(h.opened) != 0 || len(s.logs) != 0 {
		t.Fatalf("invalid effort must be rejected before a probe: status=%d, requests=%d, logs=%d", r.StatusCode, len(h.opened), len(s.logs))
	}
}

func TestModelProbeStreamsConfiguredModelAndLogsUsage(t *testing.T) {
	s := probeService(t)
	h := newFakeHost(ssePlan(simpleSSE()))
	s.SetHost(func(method string, payload, out any) error {
		if method == "host.http.do_stream" {
			request := payload.(map[string]any)
			var body map[string]any
			if err := json.Unmarshal(request["body"].([]byte), &body); err != nil {
				t.Error(err)
				return err
			}
			if body["model"] != "cline-pass/deepseek-v4.1-flash" || body["stream"] != true || body["max_tokens"] != float64(64) || len(list(body["messages"])) != 1 || request["host_callback_id"] != "probe-callback" || request["headers"].(http.Header).Get("Authorization") != "Bearer test-secret-123" {
				t.Error("probe did not use configured model, credential, callback and bounded streaming message")
			}
		}
		return h.call(method, payload, out)
	})
	r := callModelTest(t, s)
	var result map[string]any
	if err := json.Unmarshal(r.Body, &result); err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != 200 || result["ok"] != true || result["credential_label"] != "测试账号" || result["error"] != "" || len(h.opened) != 1 {
		t.Fatalf("probe result: %s", r.Body)
	}
	if len(s.logs) != 1 || s.logs[0].Attempts[0].Mode != "model-test" || s.logs[0].PromptTokens != 8 || s.logs[0].CompletionTokens != 2 {
		t.Fatalf("probe usage missing: %+v", s.logs)
	}
}

func TestModelProbePreservesFullErrorsAndRedactsKey(t *testing.T) {
	longError := strings.Repeat("long error detail ", 100) + "test-secret-123 <script>alert(1)</script> tail-marker"
	for _, tc := range []struct {
		name      string
		plan      hostPlan
		transport bool
		want      string
	}{
		{"http-json", hostPlan{status: 403, chunks: []readChunk{{Payload: jsonBytes(map[string]any{"error": map[string]any{"message": "denied", "details": longError}}), Done: true}}}, false, "tail-marker"},
		{"http-html", hostPlan{status: 502, chunks: []readChunk{{Payload: []byte(longError), Done: true}}}, false, "tail-marker"},
		{"sse-error", ssePlan(sseFrame(map[string]any{"error": map[string]any{"message": "denied", "details": longError}})), false, "tail-marker"},
		{"transport", hostPlan{}, true, "tail-marker"},
		{"incomplete", ssePlan(sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}}}})), false, "before [DONE]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := probeService(t)
			h := newFakeHost(tc.plan)
			s.SetHost(func(method string, payload, out any) error {
				if tc.transport && method == "host.http.do_stream" {
					return errors.New(longError)
				}
				return h.call(method, payload, out)
			})
			r := callModelTest(t, s)
			var result struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(r.Body, &result); err != nil {
				t.Fatal(err)
			}
			if r.StatusCode != 200 || result.OK || !strings.Contains(result.Error, tc.want) || strings.Contains(result.Error, "test-secret-123") {
				t.Fatalf("incorrect probe diagnostic: %s", r.Body)
			}
			if strings.Contains(string(jsonBytes(s.logs)), "test-secret-123") {
				t.Fatal("probe log leaked credential")
			}
		})
	}
}

func TestModelProbeTimeoutAndPreflight(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		s := probeService(t)
		s.cfg.TimeoutSeconds = 0
		release := make(chan struct{})
		s.SetHost(func(method string, payload, out any) error { <-release; return errors.New("late callback") })
		r := callModelTest(t, s)
		close(release)
		s.active.Wait()
		var result struct {
			Status int    `json:"status"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(r.Body, &result); err != nil {
			t.Fatal(err)
		}
		if r.StatusCode != 200 || result.Status != 504 || !strings.Contains(result.Error, "timed out") {
			t.Fatalf("timeout missing: %s", r.Body)
		}
	})
	for _, tc := range []string{"disabled", "removed", "proxy", "unmapped", "changed", "duplicate"} {
		t.Run(tc, func(t *testing.T) {
			s := probeService(t)
			switch tc {
			case "disabled":
				c := s.creds["probe-account"]
				c.Disabled = true
				s.creds[c.ID] = c
			case "removed":
				delete(s.creds, "probe-account")
			case "proxy":
				c := s.creds["probe-account"]
				c.ProxyURL = "http://example.invalid"
				s.creds[c.ID] = c
			case "unmapped":
				s.cfg.Models = nil
			case "changed":
				s.cfg.Models[0].UpstreamID = "changed"
			case "duplicate":
				s.modelTests = map[string]bool{string(jsonBytes([]string{"deepseek-flash", "probe-account"})): true}
			}
			s.SetHost(func(string, any, any) error {
				t.Error("invalid probe contacted upstream")
				return errors.New("unexpected callback")
			})
			r := callModelTest(t, s)
			if strings.Contains(string(r.Body), `"ok":true`) {
				t.Fatalf("invalid probe succeeded: %s", r.Body)
			}
		})
	}
}
