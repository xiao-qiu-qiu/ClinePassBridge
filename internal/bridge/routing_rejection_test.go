package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The upstream fixture rejects the presence of providerOptions, including {}.
// All transport here is in memory; no network listener or upstream is used.
type routingRejectionHost struct {
	host        *fakeHost
	mu          sync.Mutex
	bodies      []map[string]any
	auth        []string
	status      int
	text        string
	always      bool
	rejectDelay time.Duration
}

func newRoutingRejectionHost() *routingRejectionHost {
	return &routingRejectionHost{host: newFakeHost(), status: 403, text: providerRoutingRejection}
}

func routingSuccessPlan(stream bool) hostPlan {
	if stream {
		return ssePlan(sseFrame(map[string]any{"routing": map[string]any{
			"finalProvider": "deepseek", "fallbacksAvailable": []string{"deepseek", "wafer"},
		}}), simpleSSE())
	}
	return jsonPlan(map[string]any{"choices": []any{map[string]any{
		"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop",
	}}})
}

func (h *routingRejectionHost) call(method string, payload, out any) error {
	if method == "host.http.do_stream" {
		values := payload.(map[string]any)
		body, err := decodeObject(values["body"].([]byte))
		if err != nil {
			return err
		}
		h.mu.Lock()
		h.bodies = append(h.bodies, body)
		h.auth = append(h.auth, values["headers"].(http.Header).Get("Authorization"))
		h.mu.Unlock()
		plan := routingSuccessPlan(body["stream"] == true)
		if _, present := body["providerOptions"]; present || h.always {
			plan = jsonErrorPlan(h.status, map[string]any{"error": map[string]any{"message": h.text}})
			if h.rejectDelay > 0 {
				time.Sleep(h.rejectDelay)
			}
		}
		h.host.mu.Lock()
		h.host.plans = append(h.host.plans, plan)
		h.host.mu.Unlock()
	}
	return h.host.call(method, payload, out)
}

func routingFixtureService(t *testing.T, mode string, options map[string]any) *Service {
	t.Helper()
	s := probeService(t)
	t.Cleanup(func() { observabilityShutdown(t, s) })
	s.cfg.NonstreamMode = mode
	s.cfg.ProviderPolicy = ProviderPolicyConfig
	s.cfg.Models[0].RoutingMode = RoutingModeCustom
	s.cfg.Models[0].ProviderOptions = options
	return s
}

func routingRequestForMode(t *testing.T, mode string) ExecutorRequest {
	t.Helper()
	var r ExecutorRequest
	if err := json.Unmarshal(executorRequest("routing-client"), &r); err != nil {
		t.Fatal(err)
	}
	if mode == "model-test" || mode == "provider-probe" {
		r.HostCallbackID = "probe-callback"
	}
	return r
}

func runRoutingMode(t *testing.T, s *Service, mode string) (map[string]any, error) {
	t.Helper()
	if mode == "model-test" || mode == "provider-probe" {
		path := "test"
		if mode == "provider-probe" {
			path = "providers"
		}
		result, err := s.Handle("management.handle", jsonBytes(ManagementRequest{
			Method: "POST", Path: apiBase + "/models/" + path, HostCallbackID: "probe-callback",
			Body: jsonBytes(map[string]any{"model": "deepseek-flash", "upstream_id": "cline-pass/deepseek-v4.1-flash", "credential_id": "probe-account"}),
		}))
		if err != nil {
			return nil, err
		}
		response := result.(ManagementResponse)
		body, err := decodeObject(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return body, nil
	}
	method := "executor.execute"
	if mode == "stream" {
		method = "executor.execute_stream"
	}
	_, err := s.Handle(method, jsonBytes(routingRequestForMode(t, mode)))
	return nil, err
}

func latestRoutingLog(t *testing.T, s *Service) LogEntry {
	t.Helper()
	s.active.Wait()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.logs) != 1 {
		t.Fatalf("request logs = %d, want one logical request", len(s.logs))
	}
	return s.logs[0]
}

func TestRoutingRejectionNoneStripsAllHintsWithoutChangingConfig(t *testing.T) {
	for _, mode := range []string{RoutingModePreset, RoutingModeCustom} {
		for _, client := range []string{
			`{"provider":{"only":["wafer"]},"providerOptions":{}}`,
			`{"provider":{"only":["wafer"]},"providerOptions":{"gateway":{"has":["vision"]},"deepseek":{"reasoningEffort":"low"}}}`,
			`{"provider":null,"providerOptions":null}`,
			`{}`,
		} {
			t.Run(mode+"/"+client, func(t *testing.T) {
				s := routingFixtureService(t, "native", map[string]any{"gateway": map[string]any{"has": []string{"vision"}}, "custom": map[string]any{"enabled": true}})
				s.cfg.ProviderPolicy, s.cfg.Models[0].RoutingMode = ProviderPolicyNone, mode
				s.cfg.Models[0].Providers, s.cfg.Models[0].Has = []string{"deepseek"}, []string{"vision"}
				before := jsonBytes(s.cfg)
				j, err := decodeObject([]byte(client))
				if err != nil {
					t.Fatal(err)
				}
				s.applyProviderRouting(j, "deepseek-flash")
				for _, key := range []string{"provider", "providerOptions"} {
					if _, present := j[key]; present {
						t.Errorf("none retained %s", key)
					}
				}
				if !bytes.Equal(before, jsonBytes(s.cfg)) {
					t.Fatal("none modified saved model routing")
				}
			})
		}
	}
}

func TestRoutingRejectionRetriesWholeOptionsOnceAcrossExecutors(t *testing.T) {
	for _, mode := range []string{"stream-aggregate", "native", "native-fallback", "stream", "model-test"} {
		for _, options := range []string{
			`{}`, `{"gateway":{}}`, `{"deepseek":{"reasoningEffort":"low"}}`,
			`{"gateway":{"only":["deepseek"],"has":["vision"]},"deepseek":{"reasoningEffort":"low"}}`,
		} {
			t.Run(mode+"/"+options, func(t *testing.T) {
				parsed, err := decodeObject([]byte(options))
				if err != nil {
					t.Fatal(err)
				}
				s := routingFixtureService(t, mode, parsed)
				before := jsonBytes(s.cfg.Models)
				h := newRoutingRejectionHost()
				s.SetHost(h.call)
				result, err := runRoutingMode(t, s, mode)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "model-test" && (result["ok"] != true || result["routing_fallback"] != true || number(result["status"]) != 200) {
					t.Fatalf("management response = %s", jsonBytes(result))
				}
				entry := latestRoutingLog(t, s)
				if !entry.RoutingFallback || entry.Status != 200 || entry.UpstreamAttempts == nil || *entry.UpstreamAttempts != 2 || len(entry.Attempts) != 2 {
					t.Fatalf("fallback request log = %+v", entry)
				}
				for i, status := range []int{403, 200} {
					attempt := entry.Attempts[i]
					if attempt.Status != status || attempt.UpstreamHTTPStatus != status || !attempt.RoutingFallback || (i == 0 && attempt.Error == "") {
						t.Errorf("attempt %d = %+v", i, attempt)
					}
				}
				h.mu.Lock()
				defer h.mu.Unlock()
				if len(h.bodies) != 2 || len(h.auth) != 2 || h.auth[0] != h.auth[1] || h.auth[0] == "" {
					t.Fatalf("requests = %d, same credential = %v", len(h.bodies), len(h.auth) == 2 && h.auth[0] == h.auth[1])
				}
				for _, key := range []string{"provider", "providerOptions"} {
					if _, present := h.bodies[1][key]; present {
						t.Errorf("retry retained %s", key)
					}
				}
				if !bytes.Equal(before, jsonBytes(s.cfg.Models)) {
					t.Error("retry changed model routing config")
				}
			})
		}
	}
}

func TestRoutingRejectionOnlyExact403CanRetry(t *testing.T) {
	for _, mode := range []string{"stream-aggregate", "native", "stream", "model-test", "provider-probe"} {
		for _, tc := range []struct {
			name   string
			status int
			text   string
		}{
			{"ordinary", 403, "permission denied"},
			{"similar", 403, "providerOptions not supported"},
			{"wrong-status", 401, providerRoutingRejection},
			{"embedded-message", 403, "prefix: " + providerRoutingRejection},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				s := routingFixtureService(t, mode, map[string]any{"gateway": map[string]any{"has": []string{"vision"}}})
				h := newRoutingRejectionHost()
				h.status, h.text = tc.status, tc.text
				s.SetHost(h.call)
				_, _ = runRoutingMode(t, s, mode)
				entry := latestRoutingLog(t, s)
				if entry.RoutingFallback || len(entry.Attempts) != 1 || entry.UpstreamAttempts == nil || *entry.UpstreamAttempts != 1 || entry.Attempts[0].UpstreamHTTPStatus != tc.status {
					t.Fatalf("ordinary refusal retried or lost HTTP evidence: %+v", entry)
				}
			})
		}
	}
}

func TestRoutingRejectionSecond403DoesNotRetryAgain(t *testing.T) {
	s := routingFixtureService(t, "native", map[string]any{})
	h := newRoutingRejectionHost()
	h.always = true
	s.SetHost(h.call)
	_, err := runRoutingMode(t, s, "native")
	entry := latestRoutingLog(t, s)
	if statusOf(err) != 403 || !entry.RoutingFallback || len(entry.Attempts) != 2 || *entry.UpstreamAttempts != 2 || entry.Attempts[1].Status != 403 {
		t.Fatalf("repeated rejection result = %v, %+v", err, entry)
	}
}

func TestRoutingRejectionProbeReportsFilterLoss(t *testing.T) {
	for _, policy := range []string{ProviderPolicyNone, ProviderPolicyConfig} {
		t.Run(policy, func(t *testing.T) {
			has := []string{"vision", "implicit-caching"}
			s := routingFixtureService(t, "native", map[string]any{"gateway": map[string]any{"has": has}})
			s.cfg.ProviderPolicy = policy
			h := newRoutingRejectionHost()
			s.SetHost(h.call)
			response, err := runRoutingMode(t, s, "provider-probe")
			if err != nil {
				t.Fatal(err)
			}
			if response["ok"] != true || response["has_applied"] != false || response["routing_fallback"] != (policy == ProviderPolicyConfig) {
				t.Fatalf("probe hid filter loss: %s", jsonBytes(response))
			}
			if _, present := response["has"]; present {
				t.Errorf("probe returned removed has as effective: %s", jsonBytes(response))
			}
			if policy == ProviderPolicyConfig && string(jsonBytes(response["requested_has"])) != string(jsonBytes(has)) {
				t.Errorf("probe lost requested filter: %s", jsonBytes(response))
			}
			_ = latestRoutingLog(t, s)
		})
	}
}

type routingClosingBody struct {
	io.Reader
	closing chan struct{}
	release chan struct{}
	once    sync.Once
	closes  atomic.Int32
}

func (b *routingClosingBody) Close() error {
	b.closes.Add(1)
	b.once.Do(func() { close(b.closing) })
	<-b.release
	return nil
}

func TestRoutingRejectionCancelSurvivesResponseCleanup(t *testing.T) {
	for _, mode := range []string{"native", "stream-aggregate", "stream", "model-test", "provider-probe"} {
		for _, bound := range []bool{false, true} {
			t.Run(mode+"/bound="+string(jsonBytes(bound)), func(t *testing.T) {
				s := egressTestService(t, directTestEndpoint("fixture", "fixture", 60, 1))
				s.cfg.NonstreamMode, s.cfg.ProviderPolicy = mode, ProviderPolicyConfig
				s.cfg.Models[0].RoutingMode = RoutingModeCustom
				s.cfg.Models[0].ProviderOptions = map[string]any{"gateway": map[string]any{"has": []string{"vision"}}}
				s.creds["probe-account"] = Credential{ID: "probe-account", APIKey: "fixture-key"}
				t.Cleanup(func() { observabilityShutdown(t, s) })
				callback := routingRequestForMode(t, mode).HostCallbackID
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if bound {
					defer s.SetEgressRequestContext(callback, ctx)()
				}
				body := &routingClosingBody{
					Reader:  strings.NewReader(string(jsonBytes(map[string]any{"error": map[string]any{"message": providerRoutingRejection}}))),
					closing: make(chan struct{}), release: make(chan struct{}),
				}
				var sends atomic.Int32
				s.getEgressManager().client("direct").Transport = egressPolicyRoundTripper(func(r *http.Request) (*http.Response, error) {
					sends.Add(1)
					return &http.Response{StatusCode: 403, Header: http.Header{"Content-Type": {"application/json"}}, Body: body, Request: r}, nil
				})
				s.SetHost(newFakeHost().call)
				done := make(chan struct{})
				var response map[string]any
				var err error
				go func() { response, err = runRoutingMode(t, s, mode); close(done) }()
				select {
				case <-body.closing:
				case <-time.After(2 * time.Second):
					t.Fatal("403 cleanup did not start")
				}
				if bound {
					cancel()
				} else {
					s.CancelEgressRequest(callback)
				}
				close(body.release)
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("canceled request did not finish")
				}
				status := statusOf(err)
				if response != nil {
					status = int(number(response["status"]))
				}
				if sends.Load() != 1 || status != 499 || body.closes.Load() != 1 {
					t.Errorf("cancel result: sends=%d status=%d closes=%d", sends.Load(), status, body.closes.Load())
				}
				s.active.Wait()
				p := s.getEgressManager()
				p.mu.Lock()
				defer p.mu.Unlock()
				if len(p.operations) != 0 || len(p.streams) != 0 || p.groups["fixture"].inflight != 0 {
					t.Error("canceled logical request retained operations, streams or leases")
				}
			})
		}
	}
}

func TestRoutingRejectionKeepsDeadlineAndCredential(t *testing.T) {
	s := egressTestService(t, directTestEndpoint("fixture", "fixture", 60, 1))
	t.Cleanup(func() { observabilityShutdown(t, s) })
	s.cfg.NonstreamMode, s.cfg.ProviderPolicy = "native", ProviderPolicyConfig
	s.cfg.Models[0].RoutingMode, s.cfg.Models[0].ProviderOptions = RoutingModeCustom, map[string]any{}
	var deadlines []time.Time
	var credentials []string
	s.getEgressManager().client("direct").Transport = egressPolicyRoundTripper(func(r *http.Request) (*http.Response, error) {
		deadline, _ := r.Context().Deadline()
		deadlines = append(deadlines, deadline)
		credentials = append(credentials, r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		j, err := decodeObject(body)
		if err != nil {
			return nil, err
		}
		status, raw := 200, jsonBytes(map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"content": "ok"}, "finish_reason": "stop"}}})
		if _, present := j["providerOptions"]; present {
			status, raw = 403, jsonBytes(map[string]any{"error": map[string]any{"message": providerRoutingRejection}})
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw)), Request: r}, nil
	})
	_, err := runRoutingMode(t, s, "native")
	if err != nil {
		t.Fatal(err)
	}
	if len(deadlines) != 2 || deadlines[0].IsZero() || !deadlines[0].Equal(deadlines[1]) || !reflect.DeepEqual(credentials, []string{"Bearer test-key", "Bearer test-key"}) {
		t.Fatalf("retry refreshed deadline or credential: deadlines=%v", deadlines)
	}
	entry := latestRoutingLog(t, s)
	if len(entry.Attempts) != 2 || entry.Attempts[0].Status != 403 || entry.Attempts[1].Status != 200 {
		t.Fatalf("local HTTP attempt evidence missing: %+v", entry)
	}
}

func TestRoutingRejectionAttemptDurationsAreIndependent(t *testing.T) {
	for _, mode := range []string{"native", "stream-aggregate", "stream", "model-test", "provider-probe"} {
		t.Run(mode, func(t *testing.T) {
			s := routingFixtureService(t, mode, map[string]any{"gateway": map[string]any{"has": []string{"vision"}}})
			h := newRoutingRejectionHost()
			h.rejectDelay = 40 * time.Millisecond
			s.SetHost(h.call)
			if _, err := runRoutingMode(t, s, mode); err != nil {
				t.Fatal(err)
			}
			entry := latestRoutingLog(t, s)
			if len(entry.Attempts) != 2 {
				t.Fatalf("attempt count = %d, want 2", len(entry.Attempts))
			}
			first, final := entry.Attempts[0].DurationMS, entry.Attempts[1].DurationMS
			if first < 40 || first+final > entry.DurationMS+5 {
				t.Fatalf("attempt durations overlap: first=%d final=%d logical=%d", first, final, entry.DurationMS)
			}
		})
	}
}

type routingCountedBody struct {
	io.ReadCloser
	closes *atomic.Int32
}

func (b *routingCountedBody) Close() error {
	b.closes.Add(1)
	return b.ReadCloser.Close()
}

func TestRoutingRejectionLogicalCleanupDoesNotCountOrLeak(t *testing.T) {
	s := egressTestService(t, directTestEndpoint("fixture", "fixture", 60, 1))
	t.Cleanup(func() { observabilityShutdown(t, s) })
	s.cfg.NonstreamMode, s.cfg.ProviderPolicy = "native", ProviderPolicyConfig
	s.cfg.Models[0].RoutingMode, s.cfg.Models[0].ProviderOptions = RoutingModeCustom, map[string]any{}
	var sends, closes atomic.Int32
	s.getEgressManager().client("direct").Transport = egressPolicyRoundTripper(func(r *http.Request) (*http.Response, error) {
		sends.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		j, err := decodeObject(raw)
		if err != nil {
			return nil, err
		}
		status, body := 200, jsonBytes(map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"content": "ok"}, "finish_reason": "stop"}}})
		if _, present := j["providerOptions"]; present {
			status, body = 403, jsonBytes(map[string]any{"error": map[string]any{"message": providerRoutingRejection}})
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: &routingCountedBody{io.NopCloser(bytes.NewReader(body)), &closes}, Request: r}, nil
	})
	for range 4 {
		if _, err := runRoutingMode(t, s, "native"); err != nil {
			t.Fatal(err)
		}
	}
	s.active.Wait()
	p := s.getEgressManager()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		p.mu.Lock()
		clean := len(p.operations) == 0 && len(p.streams) == 0 && p.groups["fixture"].inflight == 0
		charged := len(p.groups["fixture"].sent)
		p.mu.Unlock()
		if clean {
			if sends.Load() != 8 || closes.Load() != 8 || charged != 8 {
				t.Errorf("logical cleanup counted HTTP or closed twice: sends=%d closes=%d charged=%d", sends.Load(), closes.Load(), charged)
			}
			break
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatal("completed logical requests retained operations, streams or leases")
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.logs) != 4 {
		t.Fatalf("logical logs = %d, want 4", len(s.logs))
	}
	for _, entry := range s.logs {
		if entry.UpstreamAttempts == nil || *entry.UpstreamAttempts != 2 || len(entry.Attempts) != 2 {
			t.Errorf("logical cleanup changed attempt counts: %+v", entry)
		}
	}
}
