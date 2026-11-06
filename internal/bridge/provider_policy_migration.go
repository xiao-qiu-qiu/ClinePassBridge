package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const providerPolicyMigrationRevision = 1
const providerPolicyBackupFile = "settings.before-unpinned-policy.json"

// Migration metadata belongs to the settings file, not the editable Config.
// Ordinary partial updates therefore cannot reset or forge completion.
type providerPolicySettings struct {
	Config
	ProviderPolicyRevision int `json:"provider_policy_revision"`
}

func loadProviderPolicySettings(cfg *Config) (int, error) {
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if err := decodeConfigJSON(raw, cfg); err != nil {
		return 0, err
	}
	var state struct {
		Revision int `json:"provider_policy_revision"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return 0, err
	}
	return state.Revision, nil
}

func saveProviderPolicySettings(cfg Config, revision int) error {
	return atomicJSON(filepath.Join(cfg.DataDir, "settings.json"), providerPolicySettings{
		Config: cfg, ProviderPolicyRevision: revision,
	})
}

func backupProviderPolicySettings(cfg Config) error {
	path := filepath.Join(cfg.DataDir, providerPolicyBackupFile)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		// YAML-only installs need an effective-config snapshot for rollback too.
		raw, err = json.MarshalIndent(cfg, "", "  ")
	}
	if err != nil {
		return err
	}
	tmp := path + ".tmp-" + id()
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	// Publish the complete snapshot without replacing a backup from an earlier
	// attempt, including a concurrent registration of another plugin instance.
	if err := os.Link(tmp, path); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("back up provider policy settings: %w", err)
	}
	return nil
}

// Commit the new policy and revision together before exposing either in memory.
// A failed save leaves the old settings intact and will be retried on register.
func migrateProviderPolicy(cfg *Config, revision int) (int, error) {
	if revision >= providerPolicyMigrationRevision {
		return revision, nil
	}
	if err := backupProviderPolicySettings(*cfg); err != nil {
		return revision, err
	}
	next := *cfg
	next.ProviderPolicy = ProviderPolicyNone
	path := filepath.Join(cfg.DataDir, "settings.json")
	fields := map[string]json.RawMessage{}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return revision, err
	}
	if err == nil {
		if err := json.Unmarshal(raw, &fields); err != nil {
			return revision, err
		}
		if fields == nil {
			fields = map[string]json.RawMessage{}
		}
	}
	// Patch only the migration fields. Snapshotting the merged Config here
	// would freeze YAML values that settings has never overridden, and drop
	// unknown settings fields belonging to another version.
	fields["provider_policy"] = jsonBytes(ProviderPolicyNone)
	fields["provider_policy_revision"] = jsonBytes(providerPolicyMigrationRevision)
	if err := atomicJSON(path, fields); err != nil {
		return revision, err
	}
	*cfg = next
	return providerPolicyMigrationRevision, nil
}
