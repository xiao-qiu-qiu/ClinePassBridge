package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const customRoutingUpstream = "cline-pass/deepseek-v4.1-flash-1017"

func TestCustomRoutingLegacyFieldsStayOptional(t *testing.T) {
	cfg := defaultConfig()
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	raw := string(jsonBytes(cfg.Models[0]))
	if strings.Contains(raw, `"routing_mode"`) || strings.Contains(raw, `"provider_options"`) {
		t.Fatalf("untouched legacy model gained custom routing fields: %s", raw)
	}
}

func customRoutingService(t *testing.T, policy, options string) *Service {
	t.Helper()
	config := strings.ReplaceAll(modelConfig(t, "provider_policy: "+policy+"\n"), "cline-pass/deepseek-v4.1-flash", customRoutingUpstream)
	s, err := registerMigratedService(t, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { observabilityShutdown(t, s) })
	field := ""
	if options != "" {
		field = `,"provider_options":` + options
	}
	body := fmt.Sprintf(`{"models":[{"id":"deepseek-flash","upstream_id":%q,"routing_mode":"custom","providers":["deepseek"],"has":["vision"]%s}]}`, customRoutingUpstream, field)
	observabilityManagement(t, s, "PUT", "/models", nil, []byte(body), 200)
	return s
}

func customRoutingObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	value, err := decodeObject([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func assertCustomRoutingJSON(t *testing.T, actual any, want string) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(want))
	decoder.UseNumber()
	var expected any
	if err := decoder.Decode(&expected); err != nil {
		t.Fatal(err)
	}
	if got, expected := string(jsonBytes(actual)), string(jsonBytes(expected)); got != expected {
		t.Fatalf("JSON = %s, want %s", got, expected)
	}
}

func TestCustomRoutingPolicyPriorityAndNestedMerge(t *testing.T) {
	const configured = `{"gateway":{"only":["configured"],"order":["configured"],"nested":{"keep":true,"replace":"config","clear":["config"]},"fromConfig":"yes"},"deepseek":{"temperature":0.5,"settings":{"keep":"yes","override":"config"}},"experimental":{"wide":9007199254740993,"flag":true}}`
	const client = `{"provider":{"only":["shorthand"],"order":[],"sort":"latency","nested":{"fromTop":true,"replace":"top"}},"providerOptions":{"gateway":{"only":null,"nested":{"replace":false,"clear":[],"zero":0}},"deepseek":{"settings":{"override":"client"}},"experimental":{"flag":false},"onlyClient":[true,9223372036854775807,null]}}`
	for _, policy := range []string{ProviderPolicyConfig, ProviderPolicyClient} {
		t.Run(policy, func(t *testing.T) {
			s := customRoutingService(t, policy, configured)
			request := customRoutingObject(t, client)
			originalOptions := object(request["providerOptions"])
			originalProvider := object(request["provider"])
			beforeOptions, beforeProvider := jsonBytes(originalOptions), jsonBytes(originalProvider)
			beforeConfig := jsonBytes(s.config())
			s.applyProviderRouting(request, "deepseek-flash")
			if policy == ProviderPolicyConfig {
				assertCustomRoutingJSON(t, request["providerOptions"], configured)
				assertCustomRoutingJSON(t, request["provider"], `{"only":["configured"],"order":["configured"]}`)
			} else {
				assertCustomRoutingJSON(t, request["providerOptions"], `{"gateway":{"only":null,"order":[],"sort":"latency","nested":{"keep":true,"replace":false,"clear":[],"zero":0,"fromTop":true},"fromConfig":"yes"},"deepseek":{"temperature":0.5,"settings":{"keep":"yes","override":"client"}},"experimental":{"wide":9007199254740993,"flag":false},"onlyClient":[true,9223372036854775807,null]}`)
				assertCustomRoutingJSON(t, request["provider"], `{"only":null,"order":[]}`)
			}
			if !bytes.Equal(beforeOptions, jsonBytes(originalOptions)) || !bytes.Equal(beforeProvider, jsonBytes(originalProvider)) {
				t.Fatal("routing mutated the caller's nested options or shorthand")
			}
			object(object(request["providerOptions"])["deepseek"])["temperature"] = false
			if !bytes.Equal(beforeConfig, jsonBytes(s.config())) || !bytes.Equal(beforeOptions, jsonBytes(originalOptions)) {
				t.Fatal("outgoing options share mutable state with live config or caller")
			}
		})
	}
}

func TestCustomRoutingEmptyObjectAndMissingObject(t *testing.T) {
	for _, options := range []string{"{}", "null", ""} {
		for _, policy := range []string{ProviderPolicyConfig, ProviderPolicyClient} {
			t.Run(policy+"/"+options, func(t *testing.T) {
				s := customRoutingService(t, policy, options)
				assertCustomRoutingJSON(t, s.config().Models[0].ProviderOptions, `{}`)
				request := map[string]any{}
				if policy == ProviderPolicyConfig {
					request = customRoutingObject(t, `{"provider":{"only":["caller"],"sort":"latency"},"providerOptions":{"gateway":{"only":["caller"]},"deepseek":{"keep":true}}}`)
				}
				s.applyProviderRouting(request, "deepseek-flash")
				assertCustomRoutingJSON(t, request["providerOptions"], `{}`)
				if _, ok := request["provider"]; ok {
					t.Fatal("empty custom options gained provider routing")
				}
			})
		}
	}
	// Missing preset fields must also remain unseeded in custom mode.
	s, err := registerMigratedService(t, fmt.Sprintf("data_dir: %q\nprovider_policy: config\nmodels:\n- id: deepseek-flash\n  upstream_id: %s\n  routing_mode: custom\n", filepath.ToSlash(t.TempDir()), customRoutingUpstream))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { observabilityShutdown(t, s) })
	model := s.config().Models[0]
	if len(model.Providers) != 0 || len(model.Has) != 0 {
		t.Fatalf("custom model gained preset defaults: %s", jsonBytes(model))
	}
	body := upstreamBodyOf(t, s, `{"messages":[{"role":"user","content":"hello"}]}`)
	assertCustomRoutingJSON(t, body["providerOptions"], `{}`)
}

func TestCustomRoutingClientPreservesExplicitGatewayValues(t *testing.T) {
	for _, gateway := range []string{`null`, `[]`, `false`, `9007199254740993`, `"experimental"`} {
		t.Run(gateway, func(t *testing.T) {
			s := customRoutingService(t, ProviderPolicyClient, `{"gateway":{"only":["default"]},"keep":true}`)
			request := customRoutingObject(t, `{"provider":{"only":["shorthand"]},"providerOptions":{"gateway":`+gateway+`}}`)
			s.applyProviderRouting(request, "deepseek-flash")
			assertCustomRoutingJSON(t, request["providerOptions"], `{"gateway":`+gateway+`,"keep":true}`)
			if _, ok := request["provider"]; ok {
				t.Fatal("non-object gateway was overridden by shorthand or mirrored")
			}
		})
	}
}

func TestCustomRoutingYAMLAndLegacyMode(t *testing.T) {
	for _, mode := range []string{"", RoutingModePreset, RoutingModeCustom} {
		t.Run(mode, func(t *testing.T) {
			config := fmt.Sprintf("data_dir: %q\nmodels:\n- id: deepseek-flash\n  upstream_id: %s\n  routing_mode: %q\n  provider_options:\n    gateway:\n      only: [custom]\n    experimental:\n      enabled: false\n      values: [null, 7, true]\n", filepath.ToSlash(t.TempDir()), customRoutingUpstream, mode)
			s, err := registerMigratedService(t, config+"provider_policy: config\n")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { observabilityShutdown(t, s) })
			model := s.config().Models[0]
			const draft = `{"gateway":{"only":["custom"]},"experimental":{"enabled":false,"values":[null,7,true]}}`
			assertCustomRoutingJSON(t, model.ProviderOptions, draft)
			request := map[string]any{}
			s.applyProviderRouting(request, "deepseek-flash")
			if mode == RoutingModeCustom {
				assertCustomRoutingJSON(t, request["providerOptions"], draft)
			} else {
				if model.RoutingMode != mode {
					t.Fatalf("legacy mode changed: got %q, want %q", model.RoutingMode, mode)
				}
				gateway := object(object(request["providerOptions"])["gateway"])
				assertCustomRoutingJSON(t, gateway["only"], `["deepseek"]`)
				if object(request["providerOptions"])["experimental"] != nil || gateway["inferenceRegion"] == nil {
					t.Fatalf("legacy/preset routing used draft or lost region: %s", jsonBytes(request))
				}
			}
		})
	}
}

func TestCustomRoutingValidationPreservesConfigAndDisk(t *testing.T) {
	s := customRoutingService(t, ProviderPolicyConfig, `{"experimental":true}`)
	path := filepath.Join(s.config().DataDir, "settings.json")
	beforeConfig := jsonBytes(s.config())
	beforeDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, fields := range []string{
		`"routing_mode":"unknown","provider_options":{}`,
		`"routing_mode":"Custom","provider_options":{}`,
		`"routing_mode":"custom","provider_options":[]`,
		`"routing_mode":"custom","provider_options":"bad"`,
		`"routing_mode":"custom","provider_options":42`,
		`"routing_mode":"custom","provider_options":true`,
		`"routing_mode":"preset","provider_options":{"padding":"` + strings.Repeat("x", maxModelProviderOptionsBytes) + `"}`,
	} {
		body := `{"models":[{"id":"deepseek-flash","upstream_id":"` + customRoutingUpstream + `",` + fields + `}]}`
		for _, endpoint := range []string{"/models", "/config"} {
			observabilityManagement(t, s, "PUT", endpoint, nil, []byte(body), 400)
			if !bytes.Equal(beforeConfig, jsonBytes(s.config())) {
				t.Fatal("rejected update changed live options")
			}
			afterDisk, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(beforeDisk, afterDisk) {
				t.Fatalf("rejected update changed settings: %v", err)
			}
		}
	}
	// The limit is inclusive and applies to the encoded object, not just values.
	options := `{"padding":"` + strings.Repeat("x", maxModelProviderOptionsBytes-len(`{"padding":""}`)) + `"}`
	if len(options) != maxModelProviderOptionsBytes {
		t.Fatal("incorrect size-limit fixture")
	}
	body := `{"models":[{"id":"deepseek-flash","upstream_id":"` + customRoutingUpstream + `","routing_mode":"custom","provider_options":` + options + `}]}`
	observabilityManagement(t, s, "PUT", "/models", nil, []byte(body), 200)
}

func TestCustomRoutingDraftPersistenceReloadAndAliasIsolation(t *testing.T) {
	const draft = `{"gateway":{"only":["custom"],"experimental":{"number":9007199254740993,"decimal":0.12345678901234567890123456789,"exponent":1e999}},"deepseek":{"flag":false}}`
	for _, endpoint := range []string{"/models", "/config"} {
		t.Run(endpoint, func(t *testing.T) {
			s := customRoutingService(t, ProviderPolicyConfig, `{}`)
			body := `{"models":[{"id":"alpha","upstream_id":"` + customRoutingUpstream + `","routing_mode":"custom","provider_options":` + draft + `},{"id":"beta","upstream_id":"` + customRoutingUpstream + `","routing_mode":"custom","provider_options":{"gateway":{"only":["beta"]}}}]}`
			observabilityManagement(t, s, "PUT", endpoint, nil, []byte(body), 200)
			response := observabilityManagement(t, s, "GET", "/models", nil, nil, 200)
			var returned struct {
				Models []Model `json:"models"`
			}
			if err := json.Unmarshal(response.Body, &returned); err != nil {
				t.Fatal(err)
			}
			assertCustomRoutingJSON(t, returned.Models[0].ProviderOptions, draft)
			configYAML := []byte(fmt.Sprintf("data_dir: %q\n", filepath.ToSlash(s.config().DataDir)))
			if _, err := s.Handle("plugin.reconfigure", jsonBytes(map[string]any{"config_yaml": configYAML})); err != nil {
				t.Fatal(err)
			}
			fresh := NewService()
			if _, err := fresh.Handle("plugin.register", jsonBytes(map[string]any{"config_yaml": configYAML})); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { observabilityShutdown(t, fresh) })
			for _, service := range []*Service{s, fresh} {
				for _, alias := range []string{"beta", "alpha", "beta"} {
					request := map[string]any{}
					service.applyProviderRouting(request, alias)
					want := draft
					if alias == "beta" {
						want = `{"gateway":{"only":["beta"]}}`
					}
					assertCustomRoutingJSON(t, request["providerOptions"], want)
				}
			}
			// Toggle to preset, retain the custom draft, then restore custom.
			models := s.config().Models
			models[0].RoutingMode = RoutingModePreset
			observabilityManagement(t, s, "PUT", "/models", nil, jsonBytes(map[string]any{"models": models}), 200)
			assertCustomRoutingJSON(t, s.config().Models[0].ProviderOptions, draft)
			request := customRoutingObject(t, `{"providerOptions":{"deepseek":{"client":true}}}`)
			s.applyProviderRouting(request, "alpha")
			gateway := object(object(request["providerOptions"])["gateway"])
			assertCustomRoutingJSON(t, gateway["only"], `["deepseek"]`)
			assertCustomRoutingJSON(t, gateway["has"], `["vision","implicit-caching"]`)
			if str(object(gateway["inferenceRegion"])["scope"]) != "global" || gateway["experimental"] != nil {
				t.Fatalf("preset behavior changed or custom draft leaked: %s", jsonBytes(request))
			}
			assertCustomRoutingJSON(t, object(request["providerOptions"])["deepseek"], `{"client":true}`)
			models = s.config().Models
			models[0].RoutingMode = RoutingModeCustom
			observabilityManagement(t, s, "PUT", "/models", nil, jsonBytes(map[string]any{"models": models}), 200)
			request = map[string]any{}
			s.applyProviderRouting(request, "alpha")
			assertCustomRoutingJSON(t, request["providerOptions"], draft)
		})
	}
}

func TestCustomRoutingSnapshotDeepCopy(t *testing.T) {
	s := customRoutingService(t, ProviderPolicyConfig, `{"gateway":{"only":["deepseek"]},"nested":[{"items":["keep"]}],"empty":[]}`)
	s.cfg.Models = append(s.cfg.Models, Model{ID: "other", UpstreamID: customRoutingUpstream, RoutingMode: RoutingModeCustom, ProviderOptions: s.cfg.Models[0].ProviderOptions})
	before := jsonBytes(s.config())
	snapshot := s.config()
	object(snapshot.Models[0].ProviderOptions["gateway"])["only"].([]any)[0] = "changed"
	object(snapshot.Models[0].ProviderOptions["nested"].([]any)[0])["items"].([]any)[0] = "changed"
	snapshot.Models[0].ProviderOptions["empty"] = append(snapshot.Models[0].ProviderOptions["empty"].([]any), true)
	assertCustomRoutingJSON(t, snapshot.Models[1].ProviderOptions, `{"gateway":{"only":["deepseek"]},"nested":[{"items":["keep"]}],"empty":[]}`)
	if !bytes.Equal(before, jsonBytes(s.config())) {
		t.Fatal("editing a snapshot changed live config")
	}
	// Preset must not delete or rewrite caller-owned provider namespaces either.
	s.cfg.Models[0].RoutingMode = RoutingModePreset
	request := customRoutingObject(t, `{"providerOptions":{"gateway":{"only":["client"]},"deepseek":{"keep":true}}}`)
	original := object(request["providerOptions"])
	beforeOptions := jsonBytes(original)
	s.applyProviderRouting(request, "deepseek-flash")
	if !bytes.Equal(beforeOptions, jsonBytes(original)) {
		t.Fatal("preset routing mutated caller providerOptions")
	}
}

func TestCustomRoutingReachesStreamAndManagementTest(t *testing.T) {
	const configured = `{"gateway":{"only":["custom"],"sort":"latency"},"experiment":{"seed":9007199254740993,"enabled":false}}`
	for _, method := range []string{"executor.execute", "executor.execute_stream", "management.handle"} {
		t.Run(method, func(t *testing.T) {
			s := customRoutingService(t, ProviderPolicyConfig, configured)
			host := newFakeHost(ssePlan(simpleSSE()))
			bodies := make(chan []byte, 1)
			s.SetHost(func(method string, payload, out any) error {
				if method == "host.http.do_stream" {
					bodies <- payload.(map[string]any)["body"].([]byte)
				}
				return host.call(method, payload, out)
			})
			if method == "management.handle" {
				s.creds["probe-account"] = Credential{ID: "probe-account", APIKey: "test-key"}
				response := callModelTestForModel(t, s, "deepseek-flash", customRoutingUpstream, "")
				result := customRoutingObject(t, string(response.Body))
				if response.StatusCode != 200 || result["ok"] != true {
					t.Fatalf("model test failed: %s", response.Body)
				}
			} else {
				var req ExecutorRequest
				if err := json.Unmarshal(requestWithPayload(`{"messages":[{"role":"user","content":"hello"}],"provider":{"only":["caller"]},"providerOptions":{"gateway":{"only":["caller"]},"discard":true}}`), &req); err != nil {
					t.Fatal(err)
				}
				req.StreamID = "custom-routing-stream"
				if _, err := s.Handle(method, jsonBytes(req)); err != nil {
					t.Fatal(err)
				}
				if method == "executor.execute_stream" {
					select {
					case <-host.clientClosed:
					case <-time.After(3 * time.Second):
						t.Fatal("stream did not finish")
					}
					s.active.Wait()
				}
			}
			body := customRoutingObject(t, string(<-bodies))
			assertCustomRoutingJSON(t, body["providerOptions"], configured)
			assertCustomRoutingJSON(t, body["provider"], `{"only":["custom"]}`)
		})
	}
}
