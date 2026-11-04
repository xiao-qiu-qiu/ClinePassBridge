package bridge

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestOfficialRoutingFollowsProviderPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, policy, upstream, providers string
		want                              bool
	}{
		{"default-official", "config", "cline-pass/deepseek-v4.1-flash-1017", "[deepseek]", true},
		{"default-install", "", "cline-pass/deepseek-v4.1-flash-1017", "null", true},
		{"client", "client", "cline-pass/deepseek-v4.1-flash-1017", "[deepseek]", false},
		{"automatic", "config", "cline-pass/deepseek-v4.1-flash-1017", "[]", false},
		{"other-provider", "config", "cline-pass/deepseek-v4.1-flash-1017", "[alibaba]", false},
		{"multiple", "config", "cline-pass/deepseek-v4.1-flash-1017", "[deepseek, alibaba]", false},
		{"other-model", "config", "cline-pass/glm-5.3-flash", "[deepseek]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fmt.Sprintf("data_dir: %q\nprovider_policy: %q\nmodels:\n- id: deepseek-flash\n  upstream_id: %s\n  providers: %s\n", t.TempDir(), tc.policy, tc.upstream, tc.providers)
			s, err := registerService(t, cfg)
			if err != nil {
				t.Fatal(err)
			}
			body := upstreamBodyOf(t, s, `{"messages":[{"role":"user","content":"hello"}],"providerOptions":{"gateway":{"only":["novita"]},"deepseek":{"reasoningEffort":"max"}}}`)
			gateway := object(object(body["providerOptions"])["gateway"])
			if (gateway["inferenceRegion"] != nil) != tc.want {
				t.Fatalf("region mismatch: %s", jsonBytes(gateway))
			}
			if tc.want {
				region := object(gateway["inferenceRegion"])
				private := object(object(region["providers"])["openai-compatible-private"])
				if str(region["scope"]) != "global" || str(private["scope"]) != "zone" || str(private["geoRegion"]) != "us" {
					t.Fatalf("wrong region: %s", jsonBytes(region))
				}
				for _, route := range []map[string]any{gateway, object(body["provider"])} {
					for _, key := range []string{"only", "order"} {
						if string(jsonBytes(route[key])) != `["deepseek"]` {
							t.Fatalf("wrong %s: %s", key, jsonBytes(route))
						}
					}
				}
			}
			if tc.policy == "client" && string(jsonBytes(gateway["only"])) != `["novita"]` {
				t.Fatal("client preference lost")
			}
			if str(object(object(body["providerOptions"])["deepseek"])["reasoningEffort"]) != "max" {
				t.Fatal("other namespace lost")
			}
		})
	}
}
func TestOfficialRoutingClientRegionPreserved(t *testing.T) {
	s, err := registerService(t, fmt.Sprintf("data_dir: %q\nprovider_policy: client\nmodels:\n- id: deepseek-flash\n  upstream_id: cline-pass/deepseek-v4.1-flash-1017\n", t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	body := upstreamBodyOf(t, s, `{"messages":[{"role":"user","content":"hello"}],"providerOptions":{"gateway":{"inferenceRegion":{"scope":"zone","geoRegion":"eu"}}}}`)
	region := object(object(object(body["providerOptions"])["gateway"])["inferenceRegion"])
	if str(region["geoRegion"]) != "eu" {
		t.Fatalf("client region changed: %s", jsonBytes(region))
	}
}
func TestOfficialRoutingReachesStreamAndManagementTest(t *testing.T) {
	for _, method := range []string{"executor.execute_stream", "management.handle"} {
		t.Run(method, func(t *testing.T) {
			s, err := registerService(t, fmt.Sprintf("data_dir: %q\nmodels:\n- id: deepseek-flash\n  upstream_id: cline-pass/deepseek-v4.1-flash-1017\n", t.TempDir()))
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
