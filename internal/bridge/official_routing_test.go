package bridge

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestOfficialRoutingOverridesCallerForBothPolicies(t *testing.T) {
	for _, policy := range []string{"config", "client"} {
		t.Run(policy, func(t *testing.T) {
			cfg := fmt.Sprintf("data_dir: %q\nprovider_policy: %s\nmodels:\n- id: deepseek-flash\n  upstream_id: cline-pass/deepseek-v4.1-flash-1017\n  official_routing: true\n  providers: [alibaba]\n", t.TempDir(), policy)
			s, err := registerService(t, cfg)
			if err != nil {
				t.Fatal(err)
			}
			body := upstreamBodyOf(t, s, `{"messages":[{"role":"user","content":"hello"}],"providerOptions":{"gateway":{"only":["novita"],"order":["novita"],"inferenceRegion":{"scope":"zone","geoRegion":"cn"}},"deepseek":{"reasoningEffort":"max"}}}`)
			gateway := object(object(body["providerOptions"])["gateway"])
			want := `{"scope":"global","providers":{"openai-compatible-private":{"scope":"zone","geoRegion":"us"}}}`
			var region any
			_ = json.Unmarshal([]byte(want), &region)
			if !reflect.DeepEqual(gateway["inferenceRegion"], region) {
				t.Fatalf("wrong region: %s", jsonBytes(gateway))
			}
			for _, route := range []map[string]any{gateway, object(body["provider"])} {
				for _, key := range []string{"only", "order"} {
					if string(jsonBytes(route[key])) != `["deepseek"]` {
						t.Fatalf("wrong %s: %s", key, jsonBytes(route))
					}
				}
			}
			if str(object(object(body["providerOptions"])["deepseek"])["reasoningEffort"]) != "max" {
				t.Fatal("lost other namespace")
			}
		})
	}
}

func TestOfficialRoutingManagementPersistsAndDisables(t *testing.T) {
	dir := t.TempDir()
	cfg := fmt.Sprintf("data_dir: %q\nmodels:\n- id: deepseek-flash\n  upstream_id: cline-pass/deepseek-v4.1-flash-1017\n", dir)
	s, err := registerService(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false} {
		body := []byte(fmt.Sprintf(`{"model":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash-1017","enabled":%t}`, enabled))
		observabilityManagement(t, s, "PUT", "/models/official-routing", nil, body, 200)
		reloaded, err := registerService(t, cfg)
		if err != nil {
			t.Fatal(err)
		}
		actual := upstreamBodyOf(t, reloaded, `{"messages":[{"role":"user","content":"hello"}]}`)
		gateway := object(object(actual["providerOptions"])["gateway"])
		if (gateway["inferenceRegion"] != nil) != enabled {
			t.Fatalf("enabled=%t region=%v", enabled, gateway["inferenceRegion"])
		}
		s = reloaded
	}
}

func TestOfficialRoutingRejectsStaleAndMalformedUpdates(t *testing.T) {
	s, err := registerService(t, fmt.Sprintf("data_dir: %q\nmodels:\n- id: deepseek-flash\n  upstream_id: cline-pass/deepseek-v4.1-flash-1017\n", t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"model":"deepseek-flash","upstream_id":"old","enabled":true}`, 409},
		{`{"model":"missing","upstream_id":"old","enabled":true}`, 404},
		{`{"model":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash-1017"}`, 400},
		{`{"model":"deepseek-flash","enabled":"true"}`, 400},
	} {
		observabilityManagement(t, s, "PUT", "/models/official-routing", nil, []byte(tc.body), tc.status)
	}
	gateway := object(object(object(upstreamBodyOf(t, s, `{"messages":[{"role":"user","content":"hello"}]}`))["providerOptions"])["gateway"])
	if gateway["inferenceRegion"] != nil {
		t.Fatal("rejected update enabled official routing")
	}
}

func TestOfficialRoutingRejectsUnsupportedModel(t *testing.T) {
	_, err := registerService(t, fmt.Sprintf("data_dir: %q\nmodels:\n- id: glm\n  upstream_id: cline-pass/glm-5.3-flash\n  official_routing: true\n", t.TempDir()))
	if err == nil {
		t.Fatal("accepted unsupported official routing model")
	}
}

func TestOfficialRoutingReachesStreamAndManagementTest(t *testing.T) {
	for _, method := range []string{"executor.execute_stream", "management.handle"} {
		t.Run(method, func(t *testing.T) {
			s, err := registerService(t, fmt.Sprintf("data_dir: %q\nmodels:\n- id: deepseek-flash\n  upstream_id: cline-pass/deepseek-v4.1-flash-1017\n  official_routing: true\n", t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			host := &capturingHost{plan: ssePlan(simpleSSE())}
			s.SetHost(host.call)
			if method == "management.handle" {
				s.creds["probe-account"] = Credential{ID: "probe-account", APIKey: "test-key"}
				response := callModelTestForModel(t, s, "deepseek-flash", "cline-pass/deepseek-v4.1-flash-1017", "")
				if response.StatusCode != 200 {
					t.Fatalf("model test: %s", response.Body)
				}
			} else {
				var req ExecutorRequest
				_ = json.Unmarshal(requestWithPayload(`{"messages":[{"role":"user","content":"hello"}]}`), &req)
				req.StreamID = "official-routing-stream"
				if _, err := s.Handle(method, jsonBytes(req)); err != nil {
					t.Fatal(err)
				}
				s.active.Wait()
			}
			host.mu.Lock()
			defer host.mu.Unlock()
			if len(host.bodies) != 1 {
				t.Fatalf("outgoing requests=%d", len(host.bodies))
			}
			gateway := object(object(host.bodies[0]["providerOptions"])["gateway"])
			if str(object(gateway["inferenceRegion"])["scope"]) != "global" || string(jsonBytes(gateway["only"])) != `["deepseek"]` {
				t.Fatalf("routing missing from %s: %s", method, jsonBytes(gateway))
			}
		})
	}
}
