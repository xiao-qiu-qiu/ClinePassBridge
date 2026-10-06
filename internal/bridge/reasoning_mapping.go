package bridge

import "strings"

// The mapping concerns Chat Completions' top-level reasoning_effort only.
// It never changes a provider namespace or enables/disables thinking itself.
type ReasoningMappingConfig struct {
	Enabled bool              `json:"enabled" yaml:"enabled"`
	Rules   map[string]string `json:"rules" yaml:"rules"`
}

type ReasoningMappingLog struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func defaultReasoningRules() map[string]string {
	return map[string]string{
		"unset": "unset", "none": "none", "minimal": "low", "low": "low",
		"medium": "high", "high": "high", "xhigh": "high", "max": "max", "ultra": "max",
	}
}

func (m ReasoningMappingConfig) clone() ReasoningMappingConfig {
	if m.Rules != nil {
		rules := make(map[string]string, len(m.Rules))
		for k, v := range m.Rules {
			rules[k] = v
		}
		m.Rules = rules
	}
	return m
}

func (m *ReasoningMappingConfig) validate() error {
	defaults := defaultReasoningRules()
	if m.Rules == nil {
		m.Rules = map[string]string{}
	}
	for from, to := range m.Rules {
		if _, ok := defaults[from]; !ok {
			return fail(400, "invalid reasoning mapping source")
		}
		switch to {
		case "unset", "none", "minimal", "low", "medium", "high", "xhigh", "max":
		default:
			return fail(400, "invalid reasoning mapping target")
		}
	}
	return nil
}

func defaultModelReasoningMapping(upstream string) *ReasoningMappingConfig {
	m := &ReasoningMappingConfig{Rules: map[string]string{}}
	if strings.Contains(strings.ToLower(upstream), "deepseek") {
		m.Rules = defaultReasoningRules()
	}
	return m
}

// Apply once before any attempt, including local backoff. The same snapshot
// determines both the actual outbound value and the persisted log metadata.
func (s *Service) applyReasoningMapping(j map[string]any, entry *LogEntry) {
	if j == nil {
		return
	}
	var mapping *ReasoningMappingConfig
	for _, model := range s.config().Models {
		if model.ID == entry.Model && model.UpstreamID == entry.UpstreamModel {
			mapping = model.ReasoningMapping
			break
		}
	}
	if mapping == nil || !mapping.Enabled {
		return
	}
	from := "unset"
	if raw := j["reasoning_effort"]; raw != nil {
		value, ok := raw.(string)
		if !ok {
			return // Preserve unsupported values for upstream validation.
		}
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			if value == "unset" {
				return // "unset" is a config sentinel, not an accepted input effort.
			}
			from = value
		}
	}
	if from == "unset" && (j["reasoning"] != nil || j["thinking"] != nil) {
		return // An explicit nested thinking directive is not an absent setting.
	}
	to, ok := mapping.Rules[from]
	if !ok {
		return
	}
	entry.ReasoningMapping = &ReasoningMappingLog{From: from, To: to}
	if to == "unset" {
		delete(j, "reasoning_effort")
	} else {
		j["reasoning_effort"] = to
	}
	entry.ReasoningEffort = objectReasoningEffort(j)
}
