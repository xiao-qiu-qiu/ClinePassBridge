package bridge

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

// Start from the fresh-install defaults, rather than rebuilding a model that
// happens to satisfy applyOfficialRegion. Exercise the actual prepare path.
func TestDeepSeekDefaultRoutingFreshInstall(t *testing.T) {
	cfg := defaultConfig()
	if len(cfg.Models) != 1 || cfg.Models[0].ID != "deepseek-v4.1-flash-1017" || cfg.Models[0].UpstreamID != "cline-pass/deepseek-v4.1-flash-1017" {
		t.Fatalf("fresh-install models = %s, want only the 1017 mapping", jsonBytes(cfg.Models))
	}
	if cfg.ProviderPolicy != ProviderPolicyNone || !reflect.DeepEqual(cfg.Models[0].Providers, []string{"deepseek"}) {
		t.Fatalf("fresh-install routing defaults: policy=%q models=%s", cfg.ProviderPolicy, jsonBytes(cfg.Models))
	}
	if !reflect.DeepEqual(cfg.Models[0].Has, []string{"vision", "implicit-caching"}) {
		t.Fatalf("fresh-install has = %#v", cfg.Models[0].Has)
	}

	// No models or provider policy in the supplied config: registration must
	// retain the real defaults as well as prepare them correctly.
	s, err := registerService(t, fmt.Sprintf("data_dir: %q\n", filepath.ToSlash(t.TempDir())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { observabilityShutdown(t, s) })
	loaded := s.config()
	if len(loaded.Models) != 1 || loaded.Models[0].ID != cfg.Models[0].ID || loaded.Models[0].UpstreamID != cfg.Models[0].UpstreamID || loaded.ProviderPolicy != cfg.ProviderPolicy {
		t.Fatalf("registration changed fresh-install defaults: %s", jsonBytes(loaded.Models))
	}

	beforeModels := string(jsonBytes(loaded.Models))
	for _, tc := range []struct {
		name, payload string
	}{
		{"plain", `{"messages":[{"role":"user","content":"hello"}]}`},
		{"client-routing-and-model-options", `{"messages":[{"role":"user","content":"hello"}],"reasoning_effort":"max","temperature":0.5,"provider":{"only":["fireworks"]},"providerOptions":{"gateway":{"only":[],"has":null,"inferenceRegion":{"scope":"zone","geoRegion":"eu"}},"deepseek":{"reasoningEffort":"max"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := prepareDeepSeekDefaultRequest(t, s, loaded.Models[0].ID, tc.payload)
			if _, present := body["provider"]; present {
				t.Errorf("none policy forwarded provider routing: %s", jsonBytes(body))
			}
			if _, present := body["providerOptions"]; present {
				t.Errorf("none policy forwarded providerOptions: %s", jsonBytes(body))
			}
			if tc.name == "client-routing-and-model-options" && (body["reasoning_effort"] != "max" || string(jsonBytes(body["temperature"])) != "0.5") {
				t.Errorf("none policy changed generation parameters: %s", jsonBytes(body))
			}
			if got := string(jsonBytes(s.config().Models)); got != beforeModels {
				t.Errorf("none policy changed saved model parameters: %s", got)
			}
		})
	}
}

// Explicit config/client policies retain their provider/has behavior after
// users opt back into routing, independently of the fresh-install none policy.
func TestDeepSeekDefaultRoutingOverrides(t *testing.T) {
	for _, tc := range []struct {
		name       string
		edit       func(*Config)
		payload    string
		wantOnly   string
		wantHas    string
		wantRegion bool
	}{
		{name: "cleared-providers", edit: func(c *Config) { c.Models[0].Providers = []string{} }, wantOnly: "null", wantHas: `["vision","implicit-caching"]`},
		{name: "custom-provider", edit: func(c *Config) { c.Models[0].Providers = []string{"fireworks"} }, wantOnly: `["fireworks"]`, wantHas: `["vision","implicit-caching"]`},
		{name: "multiple-providers", edit: func(c *Config) { c.Models[0].Providers = []string{"deepseek", "fireworks"} }, wantOnly: `["deepseek","fireworks"]`, wantHas: `["vision","implicit-caching"]`},
		{name: "cleared-has-keeps-region", edit: func(c *Config) { c.Models[0].Has = []string{} }, wantOnly: `["deepseek"]`, wantHas: "null", wantRegion: true},
		{name: "custom-has-keeps-region", edit: func(c *Config) { c.Models[0].Has = []string{"prompt-caching"} }, wantOnly: `["deepseek"]`, wantHas: `["prompt-caching"]`, wantRegion: true},
		{name: "custom-alias-keeps-region", edit: func(c *Config) { c.Models[0].ID = "my-deepseek" }, wantOnly: `["deepseek"]`, wantHas: `["vision","implicit-caching"]`, wantRegion: true},
		{name: "other-upstream", edit: func(c *Config) { c.Models[0].UpstreamID = "cline-pass/deepseek-v4.1-flash" }, wantOnly: `["deepseek"]`, wantHas: `["vision","implicit-caching"]`},
		{name: "similar-upstream", edit: func(c *Config) { c.Models[0].UpstreamID += "-preview" }, wantOnly: `["deepseek"]`, wantHas: `["vision","implicit-caching"]`},
		{name: "client-policy-keeps-default-lists", edit: func(c *Config) { c.ProviderPolicy = ProviderPolicyClient }, wantOnly: `["deepseek"]`, wantHas: `["vision","implicit-caching"]`},
		{name: "client-empty-overrides", edit: func(c *Config) { c.ProviderPolicy = ProviderPolicyClient }, payload: `{"messages":[{"role":"user","content":"hello"}],"providerOptions":{"gateway":{"only":[],"has":[]}}}`, wantOnly: "[]", wantHas: "[]"},
		{name: "client-null-overrides", edit: func(c *Config) { c.ProviderPolicy = ProviderPolicyClient }, payload: `{"messages":[{"role":"user","content":"hello"}],"providerOptions":{"gateway":{"only":null,"has":null}}}`, wantOnly: "null", wantHas: "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.DataDir = t.TempDir()
			cfg.ProviderPolicy = ProviderPolicyConfig
			tc.edit(&cfg)
			if err := cfg.validate(); err != nil {
				t.Fatal(err)
			}
			s := NewService()
			s.cfg = cfg
			payload := tc.payload
			if payload == "" {
				payload = `{"messages":[{"role":"user","content":"hello"}]}`
			}
			body := prepareDeepSeekDefaultRequest(t, s, cfg.Models[0].ID, payload)
			gateway := object(object(body["providerOptions"])["gateway"])
			assertDeepSeekDefaultRegion(t, gateway, tc.wantRegion)
			if got := string(jsonBytes(gateway["only"])); got != tc.wantOnly {
				t.Errorf("prepared only = %s, want %s", got, tc.wantOnly)
			}
			if got := string(jsonBytes(gateway["has"])); got != tc.wantHas {
				t.Errorf("prepared has = %s, want %s", got, tc.wantHas)
			}
			// Explicit client nulls must survive as fields, rather than vanishing.
			if tc.name == "client-null-overrides" {
				for _, key := range []string{"only", "has"} {
					if _, present := gateway[key]; !present {
						t.Errorf("explicit client %s:null disappeared", key)
					}
				}
			}
		})
	}
}

func TestDeepSeekDefaultRoutingClientRegion(t *testing.T) {
	s := NewService()
	s.cfg.ProviderPolicy = ProviderPolicyClient
	body := prepareDeepSeekDefaultRequest(t, s, s.config().Models[0].ID, `{"messages":[{"role":"user","content":"hello"}],"providerOptions":{"gateway":{"only":["fireworks"],"inferenceRegion":{"scope":"zone","geoRegion":"eu"}},"deepseek":{"reasoningEffort":"max"}}}`)
	opts := object(body["providerOptions"])
	gateway := object(opts["gateway"])
	if got := string(jsonBytes(gateway["inferenceRegion"])); got != `{"geoRegion":"eu","scope":"zone"}` {
		t.Errorf("client region = %s, want unchanged zone/eu", got)
	}
	if got := string(jsonBytes(gateway["only"])); got != `["fireworks"]` {
		t.Errorf("client only = %s, want [fireworks]", got)
	}
	if str(object(opts["deepseek"])["reasoningEffort"]) != "max" {
		t.Error("client non-gateway namespace was lost")
	}
}

func prepareDeepSeekDefaultRequest(t *testing.T, s *Service, alias, payload string) map[string]any {
	t.Helper()
	body, _, upstream, err := s.prepare(ExecutorRequest{
		Model:       alias,
		Payload:     []byte(payload),
		StorageJSON: jsonBytes(Credential{Type: Provider, ID: "default-routing-test", APIKey: "test-key"}),
	})
	if err != nil {
		t.Fatalf("prepare default model: %v", err)
	}
	if str(body["model"]) != upstream || upstream != s.config().Models[0].UpstreamID {
		t.Fatalf("prepared upstream = %q, body model = %v", upstream, body["model"])
	}
	return body
}

func assertDeepSeekDefaultRegion(t *testing.T, gateway map[string]any, want bool) {
	t.Helper()
	region, present := gateway["inferenceRegion"]
	if !want {
		if present {
			t.Errorf("automatic region must be absent, got %s", jsonBytes(region))
		}
		return
	}
	const expected = `{"providers":{"openai-compatible-private":{"geoRegion":"us","scope":"zone"}},"scope":"global"}`
	if !present || string(jsonBytes(region)) != expected {
		t.Errorf("prepared official region = %s (present=%t), want %s", jsonBytes(region), present, expected)
	}
}
