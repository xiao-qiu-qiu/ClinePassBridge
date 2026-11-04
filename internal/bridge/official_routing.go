package bridge

// applyOfficialRegion strengthens the existing config policy when the operator
// pins exactly DeepSeek for 1017. Client policy and broader allow-lists retain
// their original semantics; no extra setting or persisted switch is needed.
func applyOfficialRegion(gateway map[string]any, cfg Config, alias string) {
	if cfg.ProviderPolicy != ProviderPolicyConfig {
		return
	}
	pinned := cfg.pinnedProviders(alias)
	if len(pinned) != 1 || pinned[0] != "deepseek" {
		return
	}
	for _, model := range cfg.Models {
		if model.ID == alias && model.UpstreamID == "cline-pass/deepseek-v4.1-flash-1017" {
			gateway["inferenceRegion"] = map[string]any{
				"scope":     "global",
				"providers": map[string]any{"openai-compatible-private": map[string]any{"scope": "zone", "geoRegion": "us"}},
			}
			return
		}
	}
}
