package bridge

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestUnconfiguredModelsStayDisabled(t *testing.T) {
	for _, modelsYAML := range []string{"", "models: []\n", "models: null\n"} {
		t.Run(fmt.Sprintf("config=%q", modelsYAML), func(t *testing.T) {
			s := NewService()
			raw := jsonBytes(map[string]any{"config_yaml": []byte(fmt.Sprintf("data_dir: %q\n%s", filepath.ToSlash(t.TempDir()), modelsYAML))})
			for _, method := range []string{"plugin.register", "plugin.reconfigure"} {
				if _, err := s.Handle(method, raw); err != nil {
					t.Fatal(err)
				}
				for _, registration := range []string{"model.static", "model.for_auth"} {
					result, err := s.Handle(registration, nil)
					if err != nil || len(result.(map[string]any)["Models"].([]map[string]any)) != 0 {
						t.Fatalf("unconfigured models registered: %v, %v", result, err)
					}
				}
				for _, model := range []string{"deepseek-flash", "deepseek-v4.1-flash", "cline-pass/deepseek-v4.1-flash"} {
					if _, err := s.resolveModel(model); statusOf(err) != 400 {
						t.Fatalf("unconfigured model %q accepted: %v", model, err)
					}
				}
			}
		})
	}
}

func TestOnlyConfiguredClientNamesRouteAndPersist(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	h := newFakeHost()
	s.SetHost(h.call)
	// An upstream ID is not another alias, even when it belongs to an enabled mapping.
	for _, method := range []string{"executor.execute", "executor.execute_stream"} {
		_, err := s.Handle(method, jsonBytes(ExecutorRequest{
			Model: "cline-pass/deepseek-v4.1-flash", StreamID: "test-stream",
			Payload:     []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
			StorageJSON: jsonBytes(Credential{Type: Provider, ID: "test", APIKey: "test-key"}),
		}))
		if statusOf(err) != 400 || len(h.opened) != 0 {
			t.Fatalf("unconfigured client name reached upstream: method=%s err=%v calls=%d", method, err, len(h.opened))
		}
	}
	// A client name may equal another mapping's upstream ID; its own mapping must win.
	models := []Model{
		{ID: "my-alias", UpstreamID: "cline-pass/deepseek-v4.1-flash"},
		{ID: "cline-pass/deepseek-v4.1-flash", UpstreamID: "cline-pass/another-model"},
	}
	for _, selected := range [][]Model{models, {}} {
		result, err := s.Handle("management.handle", jsonBytes(ManagementRequest{
			Method: "PUT", Path: apiBase + "/models", Body: jsonBytes(map[string]any{"models": selected}),
		}))
		if err != nil || result.(ManagementResponse).StatusCode != 200 {
			t.Fatalf("save models: %v, %v", result, err)
		}
		// Persisted settings must replace YAML mappings, including an intentionally empty list.
		reloaded := NewService()
		_, err = reloaded.Handle("plugin.register", jsonBytes(map[string]any{"config_yaml": []byte(fmt.Sprintf(
			"data_dir: %q\nmodels:\n  - id: yaml-alias\n    upstream_id: yaml-upstream\n", filepath.ToSlash(s.config().DataDir)))}))
		if err != nil {
			t.Fatal(err)
		}
		for _, service := range []*Service{s, reloaded} {
			registered := service.modelRegistration().(map[string]any)["Models"].([]map[string]any)
			if len(registered) != len(selected) {
				t.Fatalf("registration differs from saved list: %v", registered)
			}
			for _, model := range selected {
				if got, err := service.resolveModel(model.ID); err != nil || got != model.UpstreamID {
					t.Fatalf("explicit mapping %q resolved to %q: %v", model.ID, got, err)
				}
			}
			denied := []string{"deepseek-flash", "deepseek-v4.1-flash", "yaml-alias", "cline-pass/another-model"}
			if len(selected) == 0 {
				denied = append(denied, "my-alias", "cline-pass/deepseek-v4.1-flash")
			}
			for _, name := range denied {
				if _, err := service.resolveModel(name); statusOf(err) != 400 {
					t.Fatalf("deleted or unconfigured name %q accepted: %v", name, err)
				}
			}
		}
	}
}
