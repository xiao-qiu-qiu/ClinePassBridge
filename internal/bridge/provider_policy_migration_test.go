package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func providerPolicyRegistration(dir, extra string) []byte {
	return jsonBytes(map[string]any{"config_yaml": []byte(fmt.Sprintf("data_dir: %q\n%s", filepath.ToSlash(dir), extra))})
}

func providerPolicyService(t *testing.T, raw []byte) *Service {
	t.Helper()
	s := NewService()
	t.Cleanup(func() { observabilityShutdown(t, s) })
	if _, err := s.Handle("plugin.register", raw); err != nil {
		t.Fatal(err)
	}
	return s
}

func providerPolicyStored(t *testing.T, dir string) (providerPolicySettings, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored providerPolicySettings
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	return stored, raw
}

func assertProviderPolicyMigrationPatch(t *testing.T, before, after []byte) {
	t.Helper()
	want := map[string]json.RawMessage{}
	if len(before) > 0 {
		if err := json.Unmarshal(before, &want); err != nil {
			t.Fatal(err)
		}
	}
	want["provider_policy"] = json.RawMessage(`"none"`)
	want["provider_policy_revision"] = json.RawMessage(`1`)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(after, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("migration added or removed unrelated settings: got=%s want=%s", after, jsonBytes(want))
	}
	for key, value := range want {
		var gotValue, wantValue bytes.Buffer
		if err := json.Compact(&gotValue, got[key]); err != nil {
			t.Fatalf("missing/invalid migrated field %q: %v", key, err)
		}
		if err := json.Compact(&wantValue, value); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotValue.Bytes(), wantValue.Bytes()) {
			t.Fatalf("migration changed field %q: got=%s want=%s", key, got[key], value)
		}
	}
}

func TestProviderPolicyMigrationDefaultsAndValidation(t *testing.T) {
	if got := defaultConfig().ProviderPolicy; got != ProviderPolicyNone {
		t.Fatalf("new-install policy = %q", got)
	}
	for _, policy := range []string{"", ProviderPolicyNone, ProviderPolicyClient, ProviderPolicyConfig} {
		cfg := defaultConfig()
		cfg.ProviderPolicy = policy
		if err := cfg.validate(); err != nil {
			t.Fatalf("policy %q rejected: %v", policy, err)
		}
		if policy == "" && cfg.ProviderPolicy != ProviderPolicyNone {
			t.Fatalf("absent policy = %q", cfg.ProviderPolicy)
		}
	}
	cfg := defaultConfig()
	cfg.ProviderPolicy = "invalid"
	if err := cfg.validate(); err == nil {
		t.Fatal("invalid policy accepted")
	}
}

func TestProviderPolicyMigrationLegacySourcesKeepRoutingDrafts(t *testing.T) {
	const models = `[{"id":"saved-custom","upstream_id":"cline-pass/saved","routing_mode":"custom","providers":["wafer"],"has":["vision"],"provider_options":{"gateway":{"only":["wafer"],"order":[],"experimental":true},"seed":9007199254740993}},{"id":"saved-preset","upstream_id":"cline-pass/other","routing_mode":"preset","providers":[],"has":[],"provider_options":{"draft":{"keep":[null,{},[]]}}}]`
	for _, tc := range []struct {
		name     string
		yaml     string
		settings string
	}{
		{name: "new-install"},
		{name: "yaml-config", yaml: "provider_policy: config\n"},
		{name: "yaml-client", yaml: "provider_policy: client\n"},
		{name: "yaml-cannot-forge-revision", yaml: "provider_policy: config\nprovider_policy_revision: 999\n"},
		{name: "legacy-missing-policy", yaml: "provider_policy: config\n", settings: `{"timeout_seconds":120}`},
		{name: "legacy-config", settings: `{"provider_policy":"config","timeout_seconds":120}`},
		{name: "legacy-client", settings: `{"provider_policy":"client","timeout_seconds":120}`},
		{name: "legacy-none", settings: `{"provider_policy":"none","timeout_seconds":120}`},
		{name: "legacy-null-revision", settings: `{"provider_policy":"config","provider_policy_revision":null,"timeout_seconds":120}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.settings != "" {
				tc.settings = strings.TrimSuffix(tc.settings, "}") + `,"models":` + models + "}"
				if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(tc.settings), 0600); err != nil {
					t.Fatal(err)
				}
			}
			s := providerPolicyService(t, providerPolicyRegistration(dir, tc.yaml))
			cfg := s.config()
			stored, saved := providerPolicyStored(t, dir)
			if cfg.ProviderPolicy != ProviderPolicyNone || stored.ProviderPolicy != ProviderPolicyNone || stored.ProviderPolicyRevision != providerPolicyMigrationRevision {
				t.Fatalf("migration did not commit policy and revision together: config=%q stored=%+v", cfg.ProviderPolicy, stored)
			}
			assertProviderPolicyMigrationPatch(t, []byte(tc.settings), saved)
			if tc.settings != "" {
				if cfg.TimeoutSeconds != 120 {
					t.Fatal("migration changed unrelated timeout")
				}
				var want []Model
				if err := json.Unmarshal([]byte(models), &want); err != nil {
					t.Fatal(err)
				}
				got := cfg.Models
				for i := range got {
					if got[i].ReasoningMapping == nil {
						t.Fatal("model validation was skipped")
					}
					got[i].ReasoningMapping = nil
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("migration changed routing drafts: got=%s want=%s", jsonBytes(got), jsonBytes(want))
				}
				backup, err := os.ReadFile(filepath.Join(dir, providerPolicyBackupFile))
				if err != nil || !bytes.Equal(backup, []byte(tc.settings)) {
					t.Fatalf("original settings were not backed up verbatim: %v", err)
				}
			} else {
				if !reflect.DeepEqual(cfg.Models[0].Providers, []string{"deepseek"}) || !reflect.DeepEqual(cfg.Models[0].Has, []string{"vision", "implicit-caching"}) {
					t.Fatal("migration discarded default routing drafts")
				}
				var backup Config
				raw, err := os.ReadFile(filepath.Join(dir, providerPolicyBackupFile))
				if err != nil || json.Unmarshal(raw, &backup) != nil {
					t.Fatalf("YAML configuration snapshot is unreadable: %v", err)
				}
				if strings.Contains(tc.yaml, "provider_policy: config") && backup.ProviderPolicy != ProviderPolicyConfig {
					t.Fatal("YAML-only backup lost the original config policy")
				}
				if strings.Contains(tc.yaml, "provider_policy: client") && backup.ProviderPolicy != ProviderPolicyClient {
					t.Fatal("YAML-only backup lost the original client policy")
				}
			}
		})
	}
}

func TestProviderPolicyMigrationKeepsYAMLFieldsReconfigurable(t *testing.T) {
	for _, tc := range []struct {
		name        string
		settings    string
		wantTimeout int
	}{
		{name: "yaml-only", wantTimeout: 240},
		{name: "partial-settings", settings: `{"debug":true,"unknown":{"seed":9007199254740993,"fraction":1.234567890123456789,"empty":[],"null":null}}`, wantTimeout: 240},
		{name: "explicit-settings-timeout", settings: `{"timeout_seconds":150,"unknown":{"draft":{"keep":true}}}`, wantTimeout: 150},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.settings != "" {
				if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(tc.settings), 0600); err != nil {
					t.Fatal(err)
				}
			}
			raw := providerPolicyRegistration(dir, "provider_policy: config\ntimeout_seconds: 120\nstream_idle_timeout_seconds: 23\nmodels:\n  - id: before\n    upstream_id: cline-pass/before\n    providers: [wafer]\n    has: [vision]\n")
			s := providerPolicyService(t, raw)
			_, saved := providerPolicyStored(t, dir)
			assertProviderPolicyMigrationPatch(t, []byte(tc.settings), saved)
			if tc.name == "partial-settings" && !s.config().Debug {
				t.Fatal("migration lost an explicit settings override")
			}
			next := providerPolicyRegistration(dir, "provider_policy: client\ntimeout_seconds: 240\nstream_idle_timeout_seconds: 45\nmodels:\n  - id: after\n    upstream_id: cline-pass/after\n    providers: [deepseek]\n    has: []\n")
			if _, err := s.Handle("plugin.reconfigure", next); err != nil {
				t.Fatal(err)
			}
			for _, current := range []*Service{s, providerPolicyService(t, next)} {
				cfg := current.config()
				if cfg.ProviderPolicy != ProviderPolicyNone || cfg.TimeoutSeconds != tc.wantTimeout || cfg.StreamIdleTimeoutSeconds != 45 {
					t.Fatalf("migration changed YAML/settings precedence: policy=%q timeout=%d idle=%d", cfg.ProviderPolicy, cfg.TimeoutSeconds, cfg.StreamIdleTimeoutSeconds)
				}
				if len(cfg.Models) != 1 || cfg.Models[0].ID != "after" || !reflect.DeepEqual(cfg.Models[0].Providers, []string{"deepseek"}) || cfg.Models[0].Has == nil || len(cfg.Models[0].Has) != 0 {
					t.Fatalf("migration froze the YAML model routing: %s", jsonBytes(cfg.Models))
				}
			}
			_, reloaded := providerPolicyStored(t, dir)
			if !bytes.Equal(saved, reloaded) {
				t.Fatal("reconfigure or restart rewrote the migrated partial settings")
			}
		})
	}
}

func TestProviderPolicyMigrationRunsOnceAndRespectsManualSelection(t *testing.T) {
	for _, policy := range []string{ProviderPolicyConfig, ProviderPolicyClient} {
		t.Run(policy, func(t *testing.T) {
			dir := t.TempDir()
			raw := providerPolicyRegistration(dir, "provider_policy: config\n")
			s := providerPolicyService(t, raw)
			models := jsonBytes(s.config().Models)
			backup, err := os.ReadFile(filepath.Join(dir, providerPolicyBackupFile))
			if err != nil {
				t.Fatal(err)
			}
			observabilityManagement(t, s, "PUT", "/config", nil, jsonBytes(map[string]any{"provider_policy": policy, "provider_policy_revision": 0}), 200)
			for _, body := range []string{`{"debug":true}`, `{"provider_policy_revision":null}`, `{"provider_policy_revision":999}`, `{"Provider_Policy_Revision":0}`} {
				observabilityManagement(t, s, "PUT", "/config", nil, []byte(body), 200)
			}
			stored, saved := providerPolicyStored(t, dir)
			if stored.ProviderPolicyRevision != providerPolicyMigrationRevision || stored.ProviderPolicy != policy {
				t.Fatalf("partial update changed migration state: %+v", stored)
			}
			if _, err := s.Handle("plugin.reconfigure", raw); err != nil {
				t.Fatal(err)
			}
			observabilityShutdown(t, s)
			if _, err := s.Handle("plugin.register", raw); err != nil {
				t.Fatal(err)
			}
			restarted := providerPolicyService(t, raw)
			for _, current := range []*Service{s, restarted} {
				if current.config().ProviderPolicy != policy || !bytes.Equal(models, jsonBytes(current.config().Models)) {
					t.Fatal("reconfigure or restart overwrote the operator's policy/routing drafts")
				}
			}
			_, reloaded := providerPolicyStored(t, dir)
			if !bytes.Equal(reloaded, saved) {
				t.Fatal("completed migration rewrote settings on registration")
			}
			currentBackup, err := os.ReadFile(filepath.Join(dir, providerPolicyBackupFile))
			if err != nil || !bytes.Equal(currentBackup, backup) {
				t.Fatal("re-registration overwrote the original backup")
			}
		})
	}
}

func TestProviderPolicyMigrationCompletedRevisionPreserved(t *testing.T) {
	for _, revision := range []int{providerPolicyMigrationRevision, providerPolicyMigrationRevision + 10} {
		t.Run(fmt.Sprint(revision), func(t *testing.T) {
			dir := t.TempDir()
			seed := []byte(fmt.Sprintf(`{"provider_policy_revision":%d}`, revision))
			if err := os.WriteFile(filepath.Join(dir, "settings.json"), seed, 0600); err != nil {
				t.Fatal(err)
			}
			s := providerPolicyService(t, providerPolicyRegistration(dir, "provider_policy: client\n"))
			if s.config().ProviderPolicy != ProviderPolicyClient {
				t.Fatal("completed migration changed YAML-selected policy")
			}
			_, before := providerPolicyStored(t, dir)
			if !bytes.Equal(before, seed) {
				t.Fatal("completed revision was rewritten on registration")
			}
			observabilityManagement(t, s, "PUT", "/config", nil, []byte(`{"debug":true,"provider_policy_revision":0}`), 200)
			stored, _ := providerPolicyStored(t, dir)
			if stored.ProviderPolicyRevision != revision {
				t.Fatalf("save downgraded revision %d to %d", revision, stored.ProviderPolicyRevision)
			}
			if _, err := os.Stat(filepath.Join(dir, providerPolicyBackupFile)); !os.IsNotExist(err) {
				t.Fatalf("completed migration created a legacy backup: %v", err)
			}
			response := observabilityManagement(t, s, "GET", "/config", nil, nil, 200)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(response.Body, &fields); err != nil {
				t.Fatal(err)
			}
			if _, present := fields["provider_policy_revision"]; present {
				t.Fatal("migration metadata leaked into editable configuration")
			}
		})
	}
}

func TestProviderPolicyMigrationReadErrorsAreNotIgnored(t *testing.T) {
	for _, seed := range []string{"directory", `{"provider_policy":"config",`, `{"provider_policy_revision":"bad"}`, `{"provider_policy_revision":1.5}`} {
		t.Run(seed, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "settings.json")
			if seed == "directory" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(seed), 0600); err != nil {
				t.Fatal(err)
			}
			s := NewService()
			before := jsonBytes(s.config())
			if _, err := s.Handle("plugin.register", providerPolicyRegistration(dir, "provider_policy: config\n")); err == nil {
				t.Fatal("unreadable/invalid settings were silently ignored")
			}
			if s.loaded || s.providerPolicyRevision != 0 || !bytes.Equal(before, jsonBytes(s.config())) {
				t.Fatal("failed registration changed live configuration or migration state")
			}
			if seed != "directory" {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, []byte(seed)) {
					t.Fatal("failed registration rewrote invalid settings")
				}
			}
			if _, err := os.Stat(filepath.Join(dir, providerPolicyBackupFile)); !os.IsNotExist(err) {
				t.Fatal("failed settings decode created a migration backup")
			}
		})
	}
}

func TestProviderPolicyMigrationFailedSaveRetriesWithoutOverwritingBackup(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.DataDir, cfg.ProviderPolicy = dir, ProviderPolicyConfig
	before := jsonBytes(cfg)
	backup := []byte(`{"provider_policy":"client","original":true}`)
	if err := os.WriteFile(filepath.Join(dir, providerPolicyBackupFile), backup, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if revision, err := migrateProviderPolicy(&cfg, 0); err == nil || revision != 0 || !bytes.Equal(before, jsonBytes(cfg)) {
		t.Fatalf("failed save exposed migration: revision=%d error=%v policy=%q", revision, err, cfg.ProviderPolicy)
	}
	if err := os.Rename(path, filepath.Join(dir, "settings-blocker")); err != nil {
		t.Fatal(err)
	}
	if revision, err := migrateProviderPolicy(&cfg, 0); err != nil || revision != providerPolicyMigrationRevision {
		t.Fatalf("retry failed: revision=%d error=%v", revision, err)
	}
	stored, _ := providerPolicyStored(t, dir)
	if cfg.ProviderPolicy != ProviderPolicyNone || stored.ProviderPolicy != ProviderPolicyNone || stored.ProviderPolicyRevision != providerPolicyMigrationRevision {
		t.Fatal("retry did not atomically save policy and completion")
	}
	gotBackup, err := os.ReadFile(filepath.Join(dir, providerPolicyBackupFile))
	if err != nil || !bytes.Equal(gotBackup, backup) {
		t.Fatal("migration retry replaced the original backup")
	}
	entries, err := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed save leaked temporary settings files: %v %v", entries, err)
	}
}

func TestProviderPolicyMigrationFailedReplacementRetriesRegistration(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows file sharing can block replacement while allowing settings reads")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	seed := []byte(`{"provider_policy":"config","unknown":{"seed":9007199254740993}}`)
	if err := os.WriteFile(path, seed, 0600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	s := NewService()
	before := jsonBytes(s.config())
	raw := providerPolicyRegistration(dir, "timeout_seconds: 120\n")
	if _, err := s.Handle("plugin.register", raw); err == nil {
		t.Fatal("registration succeeded while the settings replacement was blocked")
	}
	if s.loaded || s.providerPolicyRevision != 0 || !bytes.Equal(before, jsonBytes(s.config())) {
		t.Fatal("failed replacement exposed the new configuration or completion state")
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(unchanged, seed) {
		t.Fatal("failed replacement damaged the original settings")
	}
	backup, err := os.ReadFile(filepath.Join(dir, providerPolicyBackupFile))
	if err != nil || !bytes.Equal(backup, seed) {
		t.Fatal("failed replacement lost the original backup")
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { observabilityShutdown(t, s) })
	if _, err := s.Handle("plugin.register", raw); err != nil {
		t.Fatal(err)
	}
	_, saved := providerPolicyStored(t, dir)
	assertProviderPolicyMigrationPatch(t, seed, saved)
	if s.config().ProviderPolicy != ProviderPolicyNone || s.config().TimeoutSeconds != 120 {
		t.Fatal("registration retry did not activate the migrated policy and YAML settings")
	}
}

func TestProviderPolicyMigrationFailedConfigSavePreservesRevision(t *testing.T) {
	dir := t.TempDir()
	raw := providerPolicyRegistration(dir, "")
	s := providerPolicyService(t, raw)
	before := jsonBytes(s.config())
	path := filepath.Join(dir, "settings.json")
	savedPath := filepath.Join(dir, "settings.saved.json")
	if err := os.Rename(path, savedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	observabilityManagement(t, s, "PUT", "/config", nil, []byte(`{"provider_policy":"config","provider_policy_revision":0}`), 502)
	if !bytes.Equal(before, jsonBytes(s.config())) || s.providerPolicyRevision != providerPolicyMigrationRevision {
		t.Fatal("failed config save changed active policy or completion state")
	}
	if err := os.Rename(path, filepath.Join(dir, "settings-blocker")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(savedPath, path); err != nil {
		t.Fatal(err)
	}
	observabilityManagement(t, s, "PUT", "/config", nil, []byte(`{"provider_policy":"config"}`), 200)
	if got := providerPolicyService(t, raw).config().ProviderPolicy; got != ProviderPolicyConfig {
		t.Fatalf("successful retry was remigrated on restart: %q", got)
	}
}

func TestProviderPolicyMigrationRejectsDataDirChangeBeforeWriting(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			s := providerPolicyService(t, providerPolicyRegistration(t.TempDir(), ""))
			before := jsonBytes(s.config())
			target := filepath.Join(t.TempDir(), "other-data")
			seed := []byte(`{"provider_policy":"client"}`)
			if existing {
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, "settings.json"), seed, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Handle("plugin.reconfigure", providerPolicyRegistration(target, "provider_policy: config\n")); err == nil || statusOf(err) != 409 {
				t.Fatalf("data_dir change was not rejected: %v", err)
			}
			if !bytes.Equal(before, jsonBytes(s.config())) {
				t.Fatal("rejected data_dir change altered live settings")
			}
			if existing {
				got, err := os.ReadFile(filepath.Join(target, "settings.json"))
				if err != nil || !bytes.Equal(got, seed) {
					t.Fatal("rejected data_dir change migrated the new path")
				}
				if _, err := os.Stat(filepath.Join(target, providerPolicyBackupFile)); !os.IsNotExist(err) {
					t.Fatal("rejected data_dir change wrote a migration backup")
				}
			} else if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatal("rejected data_dir change created the new path")
			}
		})
	}
}
