package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type hostPlan struct {
	status int
	header http.Header
	chunks []readChunk
}

type fakeHost struct {
	mu             sync.Mutex
	plans          []hostPlan
	opened         []bool
	callbackIDs    []string
	streams        map[string][]readChunk
	reads          map[string]int
	upstreamClosed []string
	emitted        [][]byte
	emitFailAt     int
	clientError    string
	clientClosed   chan struct{}
	closeOnce      sync.Once
}

func newFakeHost(plans ...hostPlan) *fakeHost {
	return &fakeHost{
		plans:        plans,
		streams:      make(map[string][]readChunk),
		reads:        make(map[string]int),
		clientClosed: make(chan struct{}),
	}
}

func (h *fakeHost) call(method string, payload, out any) error {
	request, _ := payload.(map[string]any)
	h.mu.Lock()
	defer h.mu.Unlock()
	switch method {
	case "host.http.do_stream":
		if len(h.opened) >= len(h.plans) {
			return fmt.Errorf("unexpected upstream request %d", len(h.opened)+1)
		}
		body, ok := request["body"].([]byte)
		if !ok {
			return errors.New("upstream body was not bytes")
		}
		var parsed struct {
			Stream bool `json:"stream"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return err
		}
		h.opened = append(h.opened, parsed.Stream)
		h.callbackIDs = append(h.callbackIDs, str(request["host_callback_id"]))
		plan := h.plans[len(h.opened)-1]
		streamID := fmt.Sprintf("upstream-%d", len(h.opened))
		h.streams[streamID] = plan.chunks
		*out.(*upstreamStream) = upstreamStream{StatusCode: plan.status, Headers: plan.header, StreamID: streamID}
		return nil
	case "host.http.stream_read":
		streamID := str(request["stream_id"])
		chunks, ok := h.streams[streamID]
		if !ok {
			return fmt.Errorf("unknown upstream stream %q", streamID)
		}
		index := h.reads[streamID]
		h.reads[streamID]++
		if index >= len(chunks) {
			*out.(*readChunk) = readChunk{Done: true}
		} else {
			*out.(*readChunk) = chunks[index]
		}
		return nil
	case "host.http.stream_close":
		h.upstreamClosed = append(h.upstreamClosed, str(request["stream_id"]))
		return nil
	case "host.stream.emit":
		chunk, ok := request["payload"].([]byte)
		if !ok {
			return errors.New("emitted payload was not bytes")
		}
		if h.emitFailAt > 0 && len(h.emitted)+1 >= h.emitFailAt {
			return errors.New("client connection closed")
		}
		h.emitted = append(h.emitted, bytes.Clone(chunk))
		return nil
	case "host.stream.close":
		h.clientError = str(request["error"])
		h.closeOnce.Do(func() { close(h.clientClosed) })
		return nil
	default:
		return fmt.Errorf("unexpected host method %q", method)
	}
}

func registeredService(t *testing.T, mode string) *Service {
	t.Helper()
	s := NewService()
	configYAML := fmt.Sprintf("data_dir: %q\n", filepath.ToSlash(t.TempDir()))
	configYAML += "models:\n  - id: deepseek-flash\n    upstream_id: cline-pass/deepseek-v4.1-flash\n"
	if mode != "" {
		configYAML += fmt.Sprintf("nonstream_mode: %s\n", mode)
	}
	_, err := s.Handle("plugin.register", jsonBytes(map[string]any{"config_yaml": []byte(configYAML)}))
	if err != nil {
		t.Fatalf("register plugin: %v", err)
	}
	return s
}

func executorRequest(streamID string) json.RawMessage {
	return jsonBytes(ExecutorRequest{
		Model:          "deepseek-flash",
		Payload:        []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
		StorageJSON:    jsonBytes(Credential{Type: Provider, ID: "credential-1", Label: "test credential", APIKey: "test-key"}),
		HostCallbackID: "callback-1",
		StreamID:       streamID,
	})
}

func jsonPlan(body any) hostPlan {
	return hostPlan{status: 200, header: http.Header{"Content-Type": []string{"application/json"}}, chunks: []readChunk{{Payload: jsonBytes(body), Done: true}}}
}

func sseFrame(v any) []byte {
	return append(append([]byte("data: "), jsonBytes(v)...), []byte("\r\n\r\n")...)
}

func ssePlan(parts ...[]byte) hostPlan {
	chunks := make([]readChunk, 0, len(parts))
	for _, part := range parts {
		chunks = append(chunks, readChunk{Payload: part})
	}
	return hostPlan{status: 200, header: http.Header{"Content-Type": []string{"text/event-stream"}}, chunks: chunks}
}

func simpleSSE() []byte {
	var stream []byte
	stream = append(stream, sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hello"}}}})...)
	stream = append(stream, sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 8, "completion_tokens": 2}})...)
	stream = append(stream, []byte("data: [DONE]\r\n\r\n")...)
	return stream
}

func TestModelNamesArePreservedAndRegistered(t *testing.T) {
	for _, upstream := range []string{"deepseek-v4.1-flash", "cline-pass/deepseek-v4.1-flash", "cline-pass/cline-pass/deepseek-v4.1-flash", " custom-name "} {
		cfg := defaultConfig()
		cfg.Models = []Model{{ID: " my-alias ", UpstreamID: upstream}}
		if err := cfg.validate(); err != nil {
			t.Fatal(err)
		}
		if cfg.Models[0].ID != " my-alias " || cfg.Models[0].UpstreamID != upstream {
			t.Fatalf("validation rewrote user model: %#v", cfg.Models[0])
		}
	}
	s := registeredService(t, "native-fallback")
	got, err := s.resolveModel("deepseek-flash")
	if err != nil || got != "cline-pass/deepseek-v4.1-flash" {
		t.Fatalf("configured alias resolved to %q, %v", got, err)
	}
	models, err := s.Handle("model.static", nil)
	if err != nil || len(models.(map[string]any)["Models"].([]map[string]any)) == 0 {
		t.Fatalf("registered models missing: %v, %v", models, err)
	}
}

func TestLogSummaryUsesFilteredRecordsBeforePagination(t *testing.T) {
	s := registeredService(t, "native-fallback")
	s.logs = []LogEntry{
		{Model: "keep", Status: 200, PromptTokens: 100, CompletionTokens: 20, CachedTokens: 60},
		{Model: "keep", Status: 200, PromptTokens: 200, CompletionTokens: 30, CachedTokens: 90},
		{Model: "other", Status: 200, PromptTokens: 1000},
	}
	result, err := s.logsResponse(ManagementRequest{Query: url.Values{"search": {"keep"}, "limit": {"1"}}})
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Items   []LogEntry `json:"items"`
		Summary struct {
			Requests   int      `json:"requests"`
			Prompt     int64    `json:"prompt_tokens"`
			Completion int64    `json:"completion_tokens"`
			Cached     int64    `json:"cached_tokens"`
			Rate       *float64 `json:"cache_rate"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(result.(ManagementResponse).Body, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 || response.Summary.Requests != 2 || response.Summary.Prompt != 300 || response.Summary.Completion != 50 || response.Summary.Cached != 150 || response.Summary.Rate == nil || *response.Summary.Rate != .5 {
		t.Fatalf("incorrect filtered summary: %s", result.(ManagementResponse).Body)
	}
}

func TestWrappedNonstreamResponsePreservesToolsReasoningAndUsage(t *testing.T) {
	s := registeredService(t, "native")
	data := map[string]any{
		"choices":  []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "done", "reasoning_content": "thinking", "tool_calls": []any{map[string]any{"id": "call-1", "function": map[string]any{"name": "lookup", "arguments": "{}"}}}}}},
		"usage":    map[string]any{"prompt_tokens": 20, "completion_tokens": 4, "prompt_tokens_details": map[string]any{"cached_tokens": 12}, "completion_tokens_details": map[string]any{"reasoning_tokens": 3}},
		"provider": "deepseek",
	}
	h := newFakeHost(jsonPlan(map[string]any{"success": true, "data": data}))
	s.SetHost(h.call)
	result, err := s.Handle("executor.execute", executorRequest(""))
	if err != nil {
		t.Fatalf("execute wrapped response: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(result.(Response).Payload, &body); err != nil {
		t.Fatal(err)
	}
	message := object(object(list(body["choices"])[0])["message"])
	usage := object(body["usage"])
	if message["content"] != "done" || message["reasoning_content"] != "thinking" || len(list(message["tool_calls"])) != 1 {
		t.Fatalf("response body lost output fields: %#v", message)
	}
	if number(object(usage["prompt_tokens_details"])["cached_tokens"]) != 12 || number(object(usage["completion_tokens_details"])["reasoning_tokens"]) != 3 {
		t.Fatalf("response body lost usage details: %#v", usage)
	}
	if len(s.logs) != 1 || s.logs[0].CachedTokens != 12 || s.logs[0].ReasoningTokens != 3 || s.logs[0].Provider != "deepseek" {
		t.Fatalf("request log lost actual usage/provider: %#v", s.logs)
	}
}

func TestSSEDecoderByteSplitsCRLFAndIncompleteFrame(t *testing.T) {
	input := []byte("event: message\r\ndata: first\r\ndata: second\r\n\r\ndata: [DONE]\r\n\r\n")
	decoder := SSEDecoder{max: 1024}
	var events []string
	for _, b := range input {
		if err := decoder.Feed([]byte{b}, func(data []byte, event string) error {
			events = append(events, event+":"+string(data))
			return nil
		}); err != nil {
			t.Fatalf("feed split SSE: %v", err)
		}
	}
	if err := decoder.End(); err != nil {
		t.Fatalf("complete stream rejected: %v", err)
	}
	if len(events) != 2 || events[0] != "message:first\nsecond" || events[1] != ":[DONE]" {
		t.Fatalf("decoded SSE events = %#v", events)
	}
	broken := SSEDecoder{max: 1024}
	_ = broken.Feed([]byte("data: unfinished"), func([]byte, string) error { return nil })
	if err := broken.End(); err == nil {
		t.Error("incomplete SSE frame was accepted")
	}
}

func TestStreamAggregationMergesToolFragmentsAndUsage(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	first := sseFrame(map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": "call-1", "type": "function",
				"function": map[string]any{"name": "lookup", "arguments": "{\"x\":"},
			}}},
		}},
	})
	second := sseFrame(map[string]any{
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "tool_calls",
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "function": map[string]any{"arguments": "1}"},
			}}},
		}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 3, "prompt_tokens_details": map[string]any{"cached_tokens": 7}, "completion_tokens_details": map[string]any{"reasoning_tokens": 2}},
	})
	h := newFakeHost(ssePlan(first[:len(first)/2], append(append(bytes.Clone(first[len(first)/2:]), second...), []byte("data: [DONE]\r\n\r\n")...)))
	s.SetHost(h.call)
	result, err := s.Handle("executor.execute", executorRequest(""))
	if err != nil {
		t.Fatalf("aggregate stream: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(result.(Response).Payload, &body); err != nil {
		t.Fatal(err)
	}
	tools := list(object(object(list(body["choices"])[0])["message"])["tool_calls"])
	function := object(object(tools[0])["function"])
	if function["name"] != "lookup" || function["arguments"] != `{"x":1}` {
		t.Fatalf("tool fragments were not merged: %#v", tools)
	}
	usage := object(body["usage"])
	if number(object(usage["prompt_tokens_details"])["cached_tokens"]) != 7 || number(object(usage["completion_tokens_details"])["reasoning_tokens"]) != 2 {
		t.Fatalf("stream usage details lost: %#v", usage)
	}
	if len(h.opened) != 1 || !h.opened[0] {
		t.Fatalf("aggregate mode sent upstream stream flags %#v", h.opened)
	}
}

func TestStreamErrorsAreReported(t *testing.T) {
	for _, test := range []struct {
		name   string
		plan   hostPlan
		needle string
	}{
		{"missing DONE", ssePlan(sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}, "finish_reason": "stop"}}})), "before [DONE]"},
		{"event error", ssePlan([]byte("event: error\r\ndata: {\"error\":{\"message\":\"quota exhausted\"}}\r\n\r\n")), "quota exhausted"},
		{"transport interruption", hostPlan{status: 200, header: http.Header{"Content-Type": []string{"text/event-stream"}}, chunks: []readChunk{{Payload: sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}}}})}, {Error: "connection reset", Done: true}}}, "connection reset"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := registeredService(t, "stream-aggregate")
			h := newFakeHost(test.plan)
			s.SetHost(h.call)
			_, err := s.Handle("executor.execute", executorRequest(""))
			if err == nil || !strings.Contains(err.Error(), test.needle) || statusOf(err) != 502 {
				t.Fatalf("stream error = %v (status %d), want %q/502", err, statusOf(err), test.needle)
			}
			if len(s.logs) != 1 || s.logs[0].Status != 502 {
				t.Fatalf("failed stream log = %#v", s.logs)
			}
		})
	}
}

func TestActualProviderRequiresResponseEvidence(t *testing.T) {
	for _, test := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"candidate only", map[string]any{"provider_metadata": map[string]any{"gateway": map[string]any{"routing": map[string]any{"fallbackProviders": []any{"openai", "deepseek"}}}}}, "unknown"},
		{"top level provider", map[string]any{"provider": "deepseek", "fallbackProviders": []any{"openai"}}, "deepseek"},
		{"final provider", map[string]any{"provider_metadata": map[string]any{"gateway": map[string]any{"routing": map[string]any{"finalProvider": "openai", "fallbackProviders": []any{"deepseek"}}}}}, "openai"},
	} {
		t.Run(test.name, func(t *testing.T) {
			entry := LogEntry{Provider: "unknown", ProviderSource: "not_reported"}
			attempt := Attempt{Provider: "unknown", ProviderSource: "not_reported"}
			observeMetadata(test.body, &entry, &attempt)
			if entry.Provider != test.want || attempt.Provider != test.want {
				t.Fatalf("actual provider = %q / %q, want %q", entry.Provider, attempt.Provider, test.want)
			}
		})
	}
}

func TestDefaultNonstreamUsesOneStreamingAttempt(t *testing.T) {
	s := registeredService(t, "") // No explicit setting: exercise the installation default.
	h := newFakeHost(ssePlan(simpleSSE()))
	s.SetHost(h.call)
	result, err := s.Handle("executor.execute", executorRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	response := result.(Response)
	var payload map[string]any
	if response.Headers.Get("Content-Type") != "application/json" || json.Unmarshal(response.Payload, &payload) != nil {
		t.Fatalf("nonstream response is not JSON: %s", response.Payload)
	}
	if len(h.opened) != 1 || !h.opened[0] {
		t.Fatalf("default made native/repeated attempt: %#v", h.opened)
	}
	if !bytes.Contains(response.Payload, []byte("hello")) || number(object(payload["usage"])["prompt_tokens"]) != 8 {
		t.Fatalf("aggregation lost text or usage: %s", response.Payload)
	}
	if len(s.logs) != 1 || len(s.logs[0].Attempts) != 1 || s.logs[0].Attempts[0].Mode != "stream-aggregate" || s.logs[0].Status != 200 {
		t.Fatalf("unexpected attempts: %#v", s.logs)
	}
}

func TestEmptyModelSelectionClearsRegistration(t *testing.T) {
	s := registeredService(t, "")
	h := newFakeHost()
	s.SetHost(h.call)
	result, err := s.Handle("management.handle", jsonBytes(ManagementRequest{Method: "PUT", Path: apiBase + "/models", Body: jsonBytes(map[string]any{"models": []Model{}})}))
	if err != nil || result.(ManagementResponse).StatusCode != 200 {
		t.Fatalf("empty selection failed: %#v, %v", result, err)
	}
	models := s.modelRegistration().(map[string]any)["Models"].([]map[string]any)
	if len(models) != 0 {
		t.Fatalf("empty selection retained models: %#v", models)
	}
}

func TestNativeEmptyResponseFallsBackOnceAndLogsBothAttempts(t *testing.T) {
	s := registeredService(t, "native-fallback")
	h := newFakeHost(jsonPlan(map[string]any{"success": false, "error": "empty response content"}), ssePlan(simpleSSE()))
	s.SetHost(h.call)
	result, err := s.Handle("executor.execute", executorRequest(""))
	if err != nil {
		t.Fatalf("native-to-stream fallback: %v", err)
	}
	if !bytes.Contains(result.(Response).Payload, []byte("hello")) {
		t.Fatalf("fallback lost response: %s", result.(Response).Payload)
	}
	if len(h.opened) != 2 || h.opened[0] || !h.opened[1] {
		t.Fatalf("upstream attempts = %#v, want native then stream", h.opened)
	}
	if len(s.logs) != 1 || len(s.logs[0].Attempts) != 2 || s.logs[0].Attempts[0].Status != 500 || s.logs[0].Attempts[1].Status != 200 || s.logs[0].Status != 200 {
		t.Fatalf("fallback attempt log = %#v", s.logs)
	}
	if s.logs[0].Provider != "unknown" || len(h.upstreamClosed) != 2 {
		t.Fatalf("fallback provider/stream close = %q / %#v", s.logs[0].Provider, h.upstreamClosed)
	}
}

func TestStreamingEmitsMultipleChunksAndHandlesClientCancel(t *testing.T) {
	for _, test := range []struct {
		name       string
		emitFailAt int
		wantStatus int
		path       string
	}{
		{"success", 0, 200, "/v1/chat/completions"},
		{"Claude HTTP translation", 0, 200, "/v1/messages"},
		{"client cancel", 2, 499, "/v1/chat/completions"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := registeredService(t, "native-fallback")
			sse := simpleSSE()
			h := newFakeHost(ssePlan(sse[:len(sse)/3], sse[len(sse)/3:2*len(sse)/3], sse[2*len(sse)/3:]))
			h.emitFailAt = test.emitFailAt
			s.SetHost(h.call)
			var req ExecutorRequest
			_ = json.Unmarshal(executorRequest("client-1"), &req)
			req.Metadata = map[string]any{"request_path": test.path}
			if _, err := s.Handle("executor.execute_stream", jsonBytes(req)); err != nil {
				t.Fatalf("start executor stream: %v", err)
			}
			select {
			case <-h.clientClosed:
			case <-time.After(3 * time.Second):
				t.Fatal("plugin did not close client stream")
			}
			h.mu.Lock()
			emitted := append([][]byte(nil), h.emitted...)
			closed := append([]string(nil), h.upstreamClosed...)
			callbackIDs := append([]string(nil), h.callbackIDs...)
			closeError := h.clientError
			h.mu.Unlock()
			if len(closed) != 1 || len(callbackIDs) != 1 || callbackIDs[0] != "callback-1" {
				t.Fatalf("stream callbacks: upstreamClosed=%#v callbackIDs=%#v", closed, callbackIDs)
			}
			if len(s.logs) != 1 || s.logs[0].Status != test.wantStatus || s.logs[0].Provider != "unknown" {
				t.Fatalf("stream log = %#v", s.logs)
			}
			if test.emitFailAt == 0 {
				if len(emitted) < 2 || closeError != "" {
					t.Fatalf("stream output = %#v, close error = %q", emitted, closeError)
				}
				for _, payload := range emitted {
					if test.path == "/v1/messages" {
						if !bytes.HasPrefix(payload, []byte("data: ")) {
							t.Fatalf("Claude translator requires SSE input: %q", payload)
						}
						payload = bytes.TrimSpace(bytes.TrimPrefix(payload, []byte("data: ")))
					}
					if !json.Valid(payload) {
						t.Fatalf("executor payload must be raw JSON for CPA framing: %q", payload)
					}
				}
			} else if len(emitted) != 1 || !strings.Contains(closeError, "client disconnected") {
				t.Fatalf("cancel output = %#v, close error = %q", emitted, closeError)
			}
		})
	}
}

func TestShutdownClosesAndWaitsForActiveStream(t *testing.T) {
	s := registeredService(t, "native-fallback")
	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	clientClosed := make(chan struct{})
	var startOnce, releaseOnce, closeOnce sync.Once
	s.SetHost(func(method string, payload, out any) error {
		switch method {
		case "host.http.do_stream":
			*out.(*upstreamStream) = upstreamStream{StatusCode: 200, Headers: http.Header{"Content-Type": []string{"text/event-stream"}}, StreamID: "blocked-upstream"}
		case "host.http.stream_read":
			startOnce.Do(func() { close(readStarted) })
			<-releaseRead
			*out.(*readChunk) = readChunk{Error: "stream canceled", Done: true}
		case "host.http.stream_close":
			releaseOnce.Do(func() { close(releaseRead) })
		case "host.stream.close":
			closeOnce.Do(func() { close(clientClosed) })
		default:
			return fmt.Errorf("unexpected host callback %s", method)
		}
		return nil
	})
	if _, err := s.Handle("executor.execute_stream", executorRequest("client-1")); err != nil {
		t.Fatalf("start stream: %v", err)
	}
	select {
	case <-readStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream stream read did not start")
	}
	shutdownDone := make(chan error, 1)
	go func() {
		_, err := s.Handle("plugin.shutdown", nil)
		shutdownDone <- err
	}()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not cancel and wait for the active stream")
	}
	select {
	case <-clientClosed:
	default:
		t.Fatal("shutdown returned before the client stream was closed")
	}
	if _, err := s.Handle("executor.execute", executorRequest("")); err == nil || statusOf(err) != 503 {
		t.Fatalf("new execution after shutdown = %v, want HTTP 503", err)
	}
}

func TestHeaderDeadlineCleansUpLateUpstreamStream(t *testing.T) {
	s := registeredService(t, "native-fallback")
	started := make(chan struct{})
	release := make(chan struct{})
	lateClosed := make(chan struct{})
	var startOnce, releaseOnce, closeOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	s.SetHost(func(method string, payload, out any) error {
		switch method {
		case "host.http.do_stream":
			startOnce.Do(func() { close(started) })
			<-release
			*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "late-upstream"}
		case "host.http.stream_close":
			if str(payload.(map[string]any)["stream_id"]) != "late-upstream" {
				return errors.New("unexpected stream closed")
			}
			closeOnce.Do(func() { close(lateClosed) })
		default:
			return fmt.Errorf("unexpected host callback %s", method)
		}
		return nil
	})
	finished := make(chan error, 1)
	go func() {
		_, err := s.openUpstream(map[string]any{"method": "GET"}, time.Now().Add(100*time.Millisecond))
		finished <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("host callback did not start")
	}
	select {
	case err := <-finished:
		if err == nil || statusOf(err) != 504 {
			t.Fatalf("header timeout = %v, want HTTP 504", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("header deadline did not return while host callback was pending")
	}
	select {
	case <-lateClosed:
		t.Fatal("stream was closed before the host returned its stream ID")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-lateClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("late stream was not closed after the host callback returned")
	}
	settled := make(chan struct{})
	go func() { s.active.Wait(); close(settled) }()
	select {
	case <-settled:
	case <-time.After(2 * time.Second):
		t.Fatal("late host callback remained active after cleanup")
	}
	s.mu.RLock()
	_, tracked := s.streams["late-upstream"]
	s.mu.RUnlock()
	if tracked {
		t.Fatal("abandoned stream remained tracked after cleanup")
	}
}

func TestDeletedCredentialRejectsStaleStorageJSON(t *testing.T) {
	s := registeredService(t, "native-fallback")
	authDir := t.TempDir()
	credential := Credential{Type: Provider, ID: "credential-1", Label: "test credential", APIKey: "test-key-123"}
	authPath := filepath.Join(authDir, credential.ID+".json")
	if err := os.WriteFile(authPath, jsonBytes(credential), 0600); err != nil {
		t.Fatal(err)
	}
	parseRequest := jsonBytes(map[string]any{
		"Provider": Provider, "Path": authPath, "FileName": filepath.Base(authPath),
		"RawJSON": jsonBytes(credential), "Host": map[string]any{"AuthDir": authDir},
	})
	if _, err := s.Handle("auth.parse", parseRequest); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	deletion := ManagementRequest{Method: "DELETE", Path: apiBase + "/credentials", Query: url.Values{"id": {credential.ID}}}
	result, err := s.Handle("management.handle", jsonBytes(deletion))
	if err != nil || result.(ManagementResponse).StatusCode != 200 {
		t.Fatalf("delete credential: %#v, %v", result, err)
	}
	if _, err := os.Stat(authPath); !os.IsNotExist(err) {
		t.Fatalf("credential file still exists or stat failed: %v", err)
	}
	called := false
	s.SetHost(func(string, any, any) error { called = true; return nil })
	request := ExecutorRequest{
		Model: "deepseek-flash", Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
		StorageJSON: jsonBytes(credential), HostCallbackID: "callback-1",
	}
	_, err = s.Handle("executor.execute", jsonBytes(request))
	if err == nil || statusOf(err) != 401 || called {
		t.Fatalf("stale StorageJSON execution = %v (status %d), host called=%v", err, statusOf(err), called)
	}
}

func TestUpdateCredentialPreservesMigratedFileAndEffectiveKey(t *testing.T) {
	s := registeredService(t, "native-fallback")
	original := Credential{Type: Provider, ID: "credential-1", Label: "original", APIKey: "old-key-1234"}
	const filename = "migrated-file.json"
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": filename, "RawJSON": jsonBytes(original),
	})); err != nil {
		t.Fatalf("parse migrated credential: %v", err)
	}
	var saved []Credential
	var savedNames []string
	failSave := false
	s.SetHost(func(method string, payload, out any) error {
		if method != "host.auth.save" {
			return fmt.Errorf("unexpected host callback %s", method)
		}
		request := payload.(map[string]any)
		var credential Credential
		if err := json.Unmarshal(request["json"].(json.RawMessage), &credential); err != nil {
			return err
		}
		savedNames = append(savedNames, str(request["name"]))
		saved = append(saved, credential)
		if failSave {
			return errors.New("host save failed")
		}
		return nil
	})
	update := func(id string, body any) ManagementResponse {
		t.Helper()
		result, err := s.Handle("management.handle", jsonBytes(ManagementRequest{
			Method: "PUT", Path: apiBase + "/credentials", Query: url.Values{"id": {id}}, Body: jsonBytes(body),
		}))
		if err != nil {
			t.Fatalf("update credential %q: %v", id, err)
		}
		return result.(ManagementResponse)
	}
	response := update(original.ID, map[string]any{"label": "renamed", "api_key": "new-key-5678"})
	if response.StatusCode != 200 || bytes.Contains(response.Body, []byte("api_key")) || bytes.Contains(response.Body, []byte(original.APIKey)) || bytes.Contains(response.Body, []byte("new-key-5678")) {
		t.Fatalf("updated credential response disclosed key or failed: status=%d body=%s", response.StatusCode, response.Body)
	}
	if len(saved) != 1 || savedNames[0] != filename || saved[0].ID != original.ID || saved[0].Label != "renamed" || saved[0].APIKey != "new-key-5678" {
		t.Fatalf("credential save used wrong file or contents: names=%#v credentials=%#v", savedNames, saved)
	}
	selected, err := s.selectedCredential(ExecutorRequest{StorageJSON: jsonBytes(original)})
	if err != nil || selected.APIKey != "new-key-5678" || selected.Label != "renamed" {
		t.Fatalf("stale StorageJSON selected old credential: label=%q key-updated=%v err=%v", selected.Label, selected.APIKey == "new-key-5678", err)
	}
	response = update(original.ID, map[string]any{"label": "label only"})
	if response.StatusCode != 200 || len(saved) != 2 || saved[1].APIKey != "new-key-5678" || saved[1].Label != "label only" {
		t.Fatalf("label-only update changed key: status=%d saved=%#v", response.StatusCode, saved)
	}
	response = update(original.ID, map[string]any{"label": "empty key", "api_key": ""})
	if response.StatusCode != 200 || len(saved) != 3 || saved[2].APIKey != "new-key-5678" || saved[2].Label != "empty key" {
		t.Fatalf("empty-key update changed key: status=%d saved=%#v", response.StatusCode, saved)
	}
	for _, name := range savedNames {
		if name != filename {
			t.Fatalf("update replaced migrated filename: %#v", savedNames)
		}
	}
	failSave = true
	response = update(original.ID, map[string]any{"label": "failed update", "api_key": "failed-key-1234"})
	if response.StatusCode != 500 {
		t.Fatalf("failed host save status = %d, want 500", response.StatusCode)
	}
	selected, err = s.selectedCredential(ExecutorRequest{StorageJSON: jsonBytes(original)})
	if err != nil || selected.Label != "empty key" || selected.APIKey != "new-key-5678" {
		t.Fatalf("failed save updated memory: label=%q key-unchanged=%v err=%v", selected.Label, selected.APIKey == "new-key-5678", err)
	}
	if response := update("missing-id", map[string]any{"label": "missing"}); response.StatusCode != 404 {
		t.Fatalf("missing credential status = %d, want 404", response.StatusCode)
	}
	if response := update(original.ID, map[string]any{"api_key": "short"}); response.StatusCode != 400 {
		t.Fatalf("short key status = %d, want 400", response.StatusCode)
	}
	if len(saved) != 4 {
		t.Fatalf("invalid updates called host save: %#v", savedNames)
	}
}

func TestCatalogRefreshUsesPassOffersAndPreservesAliases(t *testing.T) {
	s := registeredService(t, "native-fallback")
	s.mu.Lock()
	s.cfg.Models = append(s.cfg.Models, Model{ID: "my-deepseek-alias", UpstreamID: "cline-pass/deepseek-v4.1-flash"})
	s.mu.Unlock()
	credential := Credential{Type: Provider, ID: "credential-1", Label: "test credential", APIKey: "test-key-123"}
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": credential.ID + ".json", "RawJSON": jsonBytes(credential),
	})); err != nil {
		t.Fatalf("parse credential for registry refresh: %v", err)
	}
	originalModels := s.config().Models
	catalog := map[string]any{
		"clinePass": []any{
			map[string]any{"id": "cline-pass/deepseek-v4.1-flash"},
			map[string]any{"id": "cline-pass/new-offer"},
			map[string]any{"id": "byok-only-model"},
		},
		"models": []any{map[string]any{"id": "cline-pass/decoy-from-models"}},
		"byok":   []any{map[string]any{"id": "cline-pass/decoy-from-byok"}},
	}
	var requestedURL, requestedMethod, callbackID string
	var hadAuthorization bool
	var savedAuth []string
	readCount := 0
	s.SetHost(func(method string, payload, out any) error {
		request, _ := payload.(map[string]any)
		switch method {
		case "host.http.do_stream":
			requestedURL = str(request["url"])
			requestedMethod = str(request["method"])
			callbackID = str(request["host_callback_id"])
			if headers, ok := request["headers"].(http.Header); ok {
				hadAuthorization = headers.Get("Authorization") != ""
			}
			*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "catalog", Headers: http.Header{"Content-Type": []string{"application/json"}}}
		case "host.http.stream_read":
			readCount++
			if readCount == 1 {
				*out.(*readChunk) = readChunk{Payload: jsonBytes(catalog), Done: true}
			} else {
				return errors.New("catalog was read more than once")
			}
		case "host.http.stream_close":
			return nil
		case "host.auth.save":
			savedAuth = append(savedAuth, str(request["name"]))
		default:
			return fmt.Errorf("unexpected host callback %s", method)
		}
		return nil
	})
	result, err := s.Handle("management.handle", jsonBytes(ManagementRequest{
		Method: "POST", Path: apiBase + "/models/refresh", HostCallbackID: "catalog-callback",
	}))
	if err != nil || result.(ManagementResponse).StatusCode != 200 {
		t.Fatalf("refresh Pass catalog: %#v, %v", result, err)
	}
	if requestedURL != "https://api.cline.bot/api/v1/ai/cline/recommended-models" || requestedMethod != "GET" || callbackID != "catalog-callback" || hadAuthorization {
		t.Fatalf("catalog request = %q %q callback=%q authorization=%v", requestedMethod, requestedURL, callbackID, hadAuthorization)
	}
	var response struct {
		Models []Model `json:"models"`
	}
	if err := json.Unmarshal(result.(ManagementResponse).Body, &response); err != nil {
		t.Fatalf("decode model candidates: %v", err)
	}
	if len(response.Models) != 2 || response.Models[0].ID != "deepseek-v4.1-flash" || response.Models[0].UpstreamID != "cline-pass/deepseek-v4.1-flash" || response.Models[1].ID != "new-offer" || response.Models[1].UpstreamID != "cline-pass/new-offer" {
		t.Fatalf("unexpected Pass candidates: %#v", response.Models)
	}
	models := s.config().Models
	if len(models) != 2 || !bytes.Equal(jsonBytes(models), jsonBytes(originalModels)) || len(savedAuth) != 0 {
		t.Fatalf("candidate refresh changed registered models or saved auth: models=%#v saved=%#v", models, savedAuth)
	}
	if _, err := os.Stat(filepath.Join(s.config().DataDir, "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("candidate refresh wrote settings: %v", err)
	}
	selected := append(append([]Model(nil), models...), response.Models[1])
	result, err = s.Handle("management.handle", jsonBytes(ManagementRequest{
		Method: "PUT", Path: apiBase + "/models", Body: jsonBytes(map[string]any{"models": selected}),
	}))
	if err != nil || result.(ManagementResponse).StatusCode != 200 {
		t.Fatalf("save selected model: %#v, %v", result, err)
	}
	models = s.config().Models
	byID := make(map[string]Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	if len(models) != 3 || byID["my-deepseek-alias"].UpstreamID != "cline-pass/deepseek-v4.1-flash" || byID["new-offer"].UpstreamID != "cline-pass/new-offer" {
		t.Fatalf("selected model lost alias/Pass offer: %#v", models)
	}
	if _, exists := byID["byok-only-model"]; exists {
		t.Fatal("BYOK model was imported into Pass catalog")
	}
	if _, exists := byID["cline-pass/decoy-from-models"]; exists {
		t.Fatal("ordinary models array was imported into Pass catalog")
	}
	if _, exists := byID["cline-pass/decoy-from-byok"]; exists {
		t.Fatal("BYOK array was imported into Pass catalog")
	}
	if len(savedAuth) != 1 || savedAuth[0] != credential.ID+".json" {
		t.Fatalf("model registration refresh did not persist auth: %#v", savedAuth)
	}
}

func TestSSEAllChoicesMustFinishAndRefusalLogprobsAccumulate(t *testing.T) {
	t.Run("unfinished second choice", func(t *testing.T) {
		s := registeredService(t, "stream-aggregate")
		frame := sseFrame(map[string]any{"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{"content": "first"}, "finish_reason": "stop"},
			map[string]any{"index": 1, "delta": map[string]any{"content": "second"}},
		}})
		h := newFakeHost(ssePlan(frame, []byte("data: [DONE]\n\n")))
		s.SetHost(h.call)
		_, err := s.Handle("executor.execute", executorRequest(""))
		if err == nil || statusOf(err) != 502 || !strings.Contains(err.Error(), "finish_reason") {
			t.Fatalf("unfinished second choice = %v (status %d), want finish_reason/502", err, statusOf(err))
		}
	})
	t.Run("refusal and logprobs", func(t *testing.T) {
		s := registeredService(t, "stream-aggregate")
		first := sseFrame(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"refusal": "I "},
			"logprobs": map[string]any{"content": []any{map[string]any{"token": "I"}}},
		}}})
		second := sseFrame(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"refusal": "cannot help"}, "finish_reason": "stop",
			"logprobs": map[string]any{"content": []any{map[string]any{"token": "cannot"}}},
		}}})
		h := newFakeHost(ssePlan(first, second, []byte("data: [DONE]\n\n")))
		s.SetHost(h.call)
		result, err := s.Handle("executor.execute", executorRequest(""))
		if err != nil {
			t.Fatalf("aggregate refusal: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(result.(Response).Payload, &body); err != nil {
			t.Fatal(err)
		}
		choice := object(list(body["choices"])[0])
		message := object(choice["message"])
		logprobs := list(object(choice["logprobs"])["content"])
		if message["refusal"] != "I cannot help" || len(logprobs) != 2 || object(logprobs[0])["token"] != "I" || object(logprobs[1])["token"] != "cannot" {
			t.Fatalf("refusal/logprobs across frames = %#v", choice)
		}
	})
}
