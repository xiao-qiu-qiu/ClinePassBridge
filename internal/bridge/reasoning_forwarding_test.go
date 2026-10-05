package bridge

import (
	"encoding/json"
	"testing"
	"time"
)

func TestExecutorReasoningMaxReachesUpstreamAndLog(t *testing.T) {
	for _, method := range []string{"executor.execute", "executor.execute_stream"} {
		t.Run(method, func(t *testing.T) {
			s := registeredService(t, "stream-aggregate")
			h := newFakeHost(ssePlan(simpleSSE()))
			bodies := make(chan map[string]any, 2)
			s.SetHost(func(method string, payload, out any) error {
				if method == "host.http.do_stream" {
					body, err := decodeObject(payload.(map[string]any)["body"].([]byte))
					if err != nil {
						return err
					}
					bodies <- body
				}
				return h.call(method, payload, out)
			})
			var req ExecutorRequest
			if err := json.Unmarshal(executorRequest("reasoning-regression"), &req); err != nil {
				t.Fatal(err)
			}
			req.Payload = []byte(`{"messages":[{"role":"user","content":"hello"}],"reasoning_effort":"max"}`)
			if _, err := s.Handle(method, jsonBytes(req)); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if method == "executor.execute_stream" {
				// Stream logs are appended before the fake host closes the client.
				select {
				case <-h.clientClosed:
				case <-time.After(3 * time.Second):
					t.Fatal("plugin did not close client stream")
				}
			}
			if got := len(bodies); got != 1 {
				t.Fatalf("upstream requests = %d, want 1", got)
			}
			if got := (<-bodies)["reasoning_effort"]; got != "max" {
				t.Errorf("upstream reasoning_effort = %v, want max", got)
			}
			s.mu.RLock()
			defer s.mu.RUnlock()
			if len(s.logs) != 1 {
				t.Fatalf("logs = %d, want 1", len(s.logs))
			}
			entry := s.logs[0]
			if entry.ReasoningEffort != "max" || entry.Status != 200 || entry.Stream != (method == "executor.execute_stream") {
				t.Errorf("log: reasoning_effort=%q status=%d stream=%t", entry.ReasoningEffort, entry.Status, entry.Stream)
			}
		})
	}
}

func TestProviderRoutingPreservesModelOptions(t *testing.T) {
	const clientGateway = `{"only":["alibaba"],"order":["alibaba"],"allowFallbacks":false}`
	for _, tt := range []struct {
		name        string
		policy      string
		providers   string
		gateway     string
		wantGateway string
	}{
		{"config_empty_no_gateway", "config", "[]", "", ""},
		{"config_empty_client_gateway", "config", "[]", clientGateway, ""},
		{"config_pinned_overrides_gateway", "config", "[deepseek, wafer]", clientGateway, `{"only":["deepseek","wafer"],"order":["deepseek","wafer"]}`},
		{"client_empty_no_gateway", "client", "[]", "", ""},
		{"client_empty_keeps_gateway", "client", "[]", clientGateway, clientGateway},
		{"client_pinned_fills_missing_order", "client", "[deepseek, wafer]", `{"only":["alibaba"]}`, `{"only":["alibaba"],"order":["deepseek","wafer"]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, err := registerService(t, modelConfig(t, "    providers: "+tt.providers+"\nprovider_policy: "+tt.policy+"\nnonstream_mode: stream-aggregate\n"))
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			h := &capturingHost{plan: ssePlan(simpleSSE())}
			s.SetHost(h.call)
			request, err := decodeObject([]byte(`{"messages":[{"role":"user","content":"hello"}],"providerOptions":{"anthropic":{"thinking":{"type":"enabled","budgetTokens":2048}},"deepseek":{"reasoningEffort":"max"}}}`))
			if err != nil {
				t.Fatal(err)
			}
			opts := object(request["providerOptions"])
			want := map[string]any{"anthropic": opts["anthropic"], "deepseek": opts["deepseek"]}
			if tt.gateway != "" {
				gateway, err := decodeObject([]byte(tt.gateway))
				if err != nil {
					t.Fatal(err)
				}
				opts["gateway"] = gateway
			}
			if tt.wantGateway != "" {
				gateway, err := decodeObject([]byte(tt.wantGateway))
				if err != nil {
					t.Fatal(err)
				}
				want["gateway"] = gateway
			}
			if _, err := s.Handle("executor.execute", requestWithPayload(string(jsonBytes(request)))); err != nil {
				t.Fatalf("execute: %v", err)
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if len(h.bodies) != 1 {
				t.Fatalf("upstream requests = %d, want 1", len(h.bodies))
			}
			if got := object(h.bodies[0]["providerOptions"]); string(jsonBytes(got)) != string(jsonBytes(want)) {
				t.Errorf("upstream providerOptions = %s, want %s", jsonBytes(got), jsonBytes(want))
			}
		})
	}
}
