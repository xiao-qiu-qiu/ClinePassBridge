package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDeepSeek1017HasFreshDefaults(t *testing.T) {
	cfg := defaultConfig()
	want := []string{"vision", "implicit-caching"}
	if !reflect.DeepEqual(cfg.Models[0].Has, want) {
		t.Fatalf("fresh-install has = %#v, want %#v", cfg.Models[0].Has, want)
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Models[0].Has, want) {
		t.Fatalf("validation changed fresh-install defaults: %#v", cfg.Models[0].Has)
	}
}

func TestDeepSeek1017HasLegacyConfigDefaults(t *testing.T) {
	const upstream = "cline-pass/deepseek-v4.1-flash-1017"
	defaults := []string{"vision", "implicit-caching"}
	for _, format := range []string{"json", "yaml"} {
		for _, tc := range []struct {
			name       string
			alias      string
			upstream   string
			jsonFields string
			yamlFields string
			want       []string
		}{
			{name: "missing", alias: "custom-alias", upstream: upstream, want: defaults},
			{name: "null", alias: "custom-alias", upstream: upstream, jsonFields: `,"has":null`, yamlFields: "    has: null\n", want: defaults},
			{name: "empty", alias: "custom-alias", upstream: upstream, jsonFields: `,"has":[]`, yamlFields: "    has: []\n", want: []string{}},
			{name: "custom", alias: "custom-alias", upstream: upstream, jsonFields: `,"has":["prompt-caching"]`, yamlFields: "    has: [prompt-caching]\n", want: []string{"prompt-caching"}},
			// This occupies the first model slot previously used by defaultConfig's
			// 1017 model. Neither its slot nor its alias may leak capability defaults.
			{name: "other-upstream-first-slot", alias: "deepseek-v4.1-flash-1017", upstream: "cline-pass/deepseek-v4.1-flash"},
		} {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				configYAML := fmt.Sprintf("data_dir: %q\n", filepath.ToSlash(dir))
				if format == "json" {
					settings := fmt.Sprintf(`{"models":[{"id":%q,"upstream_id":%q%s}]}`, tc.alias, tc.upstream, tc.jsonFields)
					if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					configYAML += fmt.Sprintf("models:\n  - id: %s\n    upstream_id: %s\n%s", tc.alias, tc.upstream, tc.yamlFields)
				}
				s, err := registerService(t, configYAML)
				if err != nil {
					t.Fatalf("register legacy %s config: %v", format, err)
				}
				t.Cleanup(func() { observabilityShutdown(t, s) })
				model := s.config().Models[0]
				if model.ID != tc.alias || model.UpstreamID != tc.upstream || !reflect.DeepEqual(model.Has, tc.want) {
					t.Fatalf("loaded model = %s, want alias=%s upstream=%s has=%#v", jsonBytes(model), tc.alias, tc.upstream, tc.want)
				}
			})
		}
	}
}

func TestDeepSeek1017HasDefaultsMatchExactUpstream(t *testing.T) {
	for _, upstream := range []string{
		"cline-pass/deepseek-v4.1-flash",
		"cline-pass/deepseek-v4.1-flash-1017-preview",
		"deepseek-v4.1-flash-1017",
		"cline-pass/DeepSeek-v4.1-flash-1017",
		"cline-pass/glm-5.3",
	} {
		t.Run(upstream, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Models = []Model{{ID: "deepseek-v4.1-flash-1017", UpstreamID: upstream}}
			if err := cfg.validate(); err != nil {
				t.Fatal(err)
			}
			if cfg.Models[0].Has != nil {
				t.Fatalf("non-matching upstream acquired defaults: %#v", cfg.Models[0].Has)
			}
		})
	}
}

func TestDeepSeek1017HasDefaultsAreIndependentPerAlias(t *testing.T) {
	cfg := defaultConfig()
	cfg.Models = []Model{
		{ID: "first-alias", UpstreamID: "cline-pass/deepseek-v4.1-flash-1017"},
		{ID: "second-alias", UpstreamID: "cline-pass/deepseek-v4.1-flash-1017"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	want := []string{"vision", "implicit-caching"}
	for _, model := range cfg.Models {
		if !reflect.DeepEqual(model.Has, want) {
			t.Fatalf("alias %s has = %#v, want %#v", model.ID, model.Has, want)
		}
	}
	cfg.Models[0].Has[0] = "prompt-caching"
	if !reflect.DeepEqual(cfg.Models[1].Has, want) || !reflect.DeepEqual(defaultConfig().Models[0].Has, want) {
		t.Fatal("editing one alias mutated another alias or future fresh-install defaults")
	}
}

func TestDeepSeek1017HasClearSurvivesSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	configYAML := fmt.Sprintf("data_dir: %q\nmodels:\n  - id: custom-alias\n    upstream_id: cline-pass/deepseek-v4.1-flash-1017\n", filepath.ToSlash(dir))
	s, err := registerService(t, configYAML)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { observabilityShutdown(t, s) })
	models := s.config().Models
	models[0].Has = []string{}
	observabilityManagement(t, s, "PUT", "/models", nil, jsonBytes(map[string]any{"models": models}), 200)
	observabilityManagement(t, s, "PUT", "/config", nil, []byte(`{"timeout_seconds":120}`), 200)
	stored, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted Config
	if err := json.Unmarshal(stored, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Models[0].Has == nil || len(persisted.Models[0].Has) != 0 {
		t.Fatalf("cleared filter was not stored as []: %s", stored)
	}
	reloaded, err := registerService(t, configYAML)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { observabilityShutdown(t, reloaded) })
	model := reloaded.config().Models[0]
	if model.Has == nil || len(model.Has) != 0 {
		t.Fatalf("reloaded explicit [] was replaced: %s", jsonBytes(model))
	}
}

func TestDeepSeek1017HasClientEmptyOverridesDefaults(t *testing.T) {
	for _, policy := range []string{ProviderPolicyClient, ProviderPolicyConfig} {
		for _, raw := range []string{"[]", "null"} {
			t.Run(policy+"/"+raw, func(t *testing.T) {
				config := strings.ReplaceAll(modelConfig(t, "provider_policy: "+policy+"\n"), "cline-pass/deepseek-v4.1-flash", "cline-pass/deepseek-v4.1-flash-1017")
				s, err := registerMigratedService(t, config)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { observabilityShutdown(t, s) })
				gateway := gatewayOf(t, s, `{"messages":[{"role":"user","content":"hello"}],"providerOptions":{"gateway":{"has":`+raw+`}}}`)
				got, present := gateway["has"]
				want := raw
				if policy == ProviderPolicyConfig {
					want = `["vision","implicit-caching"]`
				}
				if !present || string(jsonBytes(got)) != want {
					t.Fatalf("effective has = %s (present=%t), want %s", jsonBytes(got), present, want)
				}
			})
		}
	}
}
