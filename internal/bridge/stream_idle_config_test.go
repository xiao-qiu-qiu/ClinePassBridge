package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStreamIdleConfigLegacyDefaults(t *testing.T) {
	for _, source := range []string{"yaml", "settings-json"} {
		t.Run(source, func(t *testing.T) {
			dir := t.TempDir()
			if source == "settings-json" {
				legacy := []byte(`{"timeout_seconds":120,"client_retry_compatibility":false}`)
				if err := os.WriteFile(filepath.Join(dir, "settings.json"), legacy, 0600); err != nil {
					t.Fatal(err)
				}
			}
			// The existing fixture registers with YAML that omits both fields.
			s := observabilityService(t, dir)
			cfg := s.config()
			if cfg.StreamIdleCutoffEnabled || cfg.StreamIdleTimeoutSeconds != 20 {
				t.Fatalf("legacy %s lost idle defaults: %s", source, jsonBytes(cfg))
			}
			if source == "settings-json" && (cfg.TimeoutSeconds != 120 || cfg.ClientRetryCompatibility) {
				t.Fatal("applying idle defaults changed existing settings")
			}
			response := observabilityManagement(t, s, "GET", "/config", nil, nil, 200)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(response.Body, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["stream_idle_cutoff_enabled"]) != "false" || string(fields["stream_idle_timeout_seconds"]) != "20" {
				t.Fatalf("management config omitted idle defaults: %s", response.Body)
			}
		})
	}
}

func TestStreamIdleConfigExplicitValuesPersistAndReload(t *testing.T) {
	for _, source := range []string{"yaml", "management-json"} {
		for _, seconds := range []int{1, 10, 37, 200, 1800} {
			t.Run(fmt.Sprintf("%s-%d", source, seconds), func(t *testing.T) {
				var s *Service
				if source == "yaml" {
					// Supply YAML before initial migration persists the installation settings.
					configYAML := fmt.Sprintf("data_dir: %q\nstream_idle_cutoff_enabled: false\nstream_idle_timeout_seconds: %d\n", filepath.ToSlash(t.TempDir()), seconds)
					var err error
					s, err = registerService(t, configYAML)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { observabilityShutdown(t, s) })
				} else {
					s = observabilityService(t, "")
					observabilityManagement(t, s, "PUT", "/config", nil, jsonBytes(map[string]any{
						"stream_idle_cutoff_enabled": false, "stream_idle_timeout_seconds": seconds,
					}), 200)
				}
				cfg := s.config()
				if cfg.StreamIdleCutoffEnabled || cfg.StreamIdleTimeoutSeconds != seconds {
					t.Fatalf("explicit %s values were lost: %s", source, jsonBytes(cfg))
				}
				// An unrelated save must persist a YAML false instead of dropping it.
				observabilityManagement(t, s, "PUT", "/config", nil, []byte(`{"log_retention":250}`), 200)
				cfg.LogRetention = 250
				assertStreamIdleConfigPersisted(t, s, cfg)
			})
		}
	}
}

func TestStreamIdleConfigPartialUpdatesRemainIndependent(t *testing.T) {
	s := observabilityService(t, "")
	observabilityManagement(t, s, "PUT", "/config", nil, []byte(`{"timeout_seconds":120,"log_retention":250,"client_retry_compatibility":false,"stream_idle_cutoff_enabled":false,"stream_idle_timeout_seconds":37}`), 200)
	want := s.config()
	for _, tc := range []struct {
		name   string
		body   string
		update func(*Config)
	}{
		{
			name: "request-settings", body: `{"timeout_seconds":900,"log_retention":500,"nonstream_mode":"native"}`,
			update: func(c *Config) { c.TimeoutSeconds, c.LogRetention, c.NonstreamMode = 900, 500, "native" },
		},
		{
			name: "retry-compatibility", body: `{"client_retry_compatibility":true}`,
			update: func(c *Config) { c.ClientRetryCompatibility = true },
		},
		{
			name: "idle-seconds-only", body: `{"stream_idle_timeout_seconds":1}`,
			update: func(c *Config) { c.StreamIdleTimeoutSeconds = 1 },
		},
		{
			name: "idle-enabled-only", body: `{"stream_idle_cutoff_enabled":true}`,
			update: func(c *Config) { c.StreamIdleCutoffEnabled = true },
		},
		{
			name: "idle-card", body: `{"stream_idle_cutoff_enabled":false,"stream_idle_timeout_seconds":1800}`,
			update: func(c *Config) { c.StreamIdleCutoffEnabled, c.StreamIdleTimeoutSeconds = false, 1800 },
		},
		{
			name: "empty-update", body: `{}`,
			update: func(*Config) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := observabilityManagement(t, s, "PUT", "/config", nil, []byte(tc.body), 200)
			tc.update(&want)
			returned := defaultConfig()
			if err := json.Unmarshal(response.Body, &returned); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(returned, want) {
				t.Fatalf("partial update changed other returned fields: got=%s want=%s", response.Body, jsonBytes(want))
			}
			assertStreamIdleConfigPersisted(t, s, want)
		})
	}
}

func TestStreamIdleConfigInvalidTimeoutPreservesStateAndDisk(t *testing.T) {
	s := observabilityService(t, "")
	observabilityManagement(t, s, "PUT", "/config", nil, []byte(`{"stream_idle_cutoff_enabled":false,"stream_idle_timeout_seconds":37,"client_retry_compatibility":false,"timeout_seconds":120}`), 200)
	path := filepath.Join(s.config().DataDir, "settings.json")
	beforeDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := s.config()
	beforeConfig := jsonBytes(want)
	for _, enabled := range []bool{false, true} {
		for _, value := range []struct{ name, raw string }{
			{"negative", "-1"},
			{"zero", "0"},
			{"above-maximum", "1801"},
			{"fraction", "1.5"},
			{"string", `"10"`},
		} {
			t.Run(fmt.Sprintf("enabled-%t-%s", enabled, value.name), func(t *testing.T) {
				body := fmt.Sprintf(`{"stream_idle_cutoff_enabled":%t,"stream_idle_timeout_seconds":%s,"client_retry_compatibility":true,"timeout_seconds":900}`, enabled, value.raw)
				observabilityManagement(t, s, "PUT", "/config", nil, []byte(body), 400)
				if !bytes.Equal(beforeConfig, jsonBytes(s.config())) {
					t.Error("rejected timeout changed in-memory configuration")
				}
				afterDisk, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(beforeDisk, afterDisk) {
					t.Error("rejected timeout changed settings.json")
				}
			})
		}
	}
	assertStreamIdleConfigPersisted(t, s, want)
}

func assertStreamIdleConfigPersisted(t *testing.T, s *Service, want Config) {
	t.Helper()
	if got := s.config(); !reflect.DeepEqual(got, want) {
		t.Fatalf("live config = %s, want %s", jsonBytes(got), jsonBytes(want))
	}
	raw, err := os.ReadFile(filepath.Join(want.DataDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Decode over defaults so accidentally omitting an explicit false is visible.
	persisted := defaultConfig()
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted, want) {
		t.Fatalf("persisted config = %s, want %s", raw, jsonBytes(want))
	}
	restarted := observabilityService(t, want.DataDir)
	if got := restarted.config(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reloaded config = %s, want %s", jsonBytes(got), jsonBytes(want))
	}
}
