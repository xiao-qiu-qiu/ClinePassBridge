package bridge

import (
	"bytes"
	"encoding/json"
)

const (
	RoutingModePreset = "preset"
	RoutingModeCustom = "custom"

	maxModelProviderOptionsBytes = 64 << 10
)

// Model is also decoded directly by PUT /models, outside decodeConfigJSON.
// Preserve experimental numeric options on both paths and when loading settings.
func (m *Model) UnmarshalJSON(raw []byte) error {
	type plainModel Model
	var next plainModel
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&next); err != nil {
		return err
	}
	*m = Model(next)
	return nil
}

func (m *Model) validateRouting() error {
	if m.RoutingMode != "" && m.RoutingMode != RoutingModePreset && m.RoutingMode != RoutingModeCustom {
		return fail(400, "routing_mode must be preset or custom")
	}
	// Keep a custom draft even while preset routing is active. A missing or
	// null object is the same empty draft as an explicit {}.
	if m.ProviderOptions == nil && m.RoutingMode == RoutingModeCustom {
		m.ProviderOptions = map[string]any{}
	}
	raw, err := json.Marshal(m.ProviderOptions)
	if err != nil {
		return fail(400, "provider_options must be a JSON object")
	}
	if len(raw) > maxModelProviderOptionsBytes {
		return fail(400, "provider_options must not exceed 64 KiB")
	}
	return nil
}

// Copy decoded JSON/YAML trees without a marshal/unmarshal round trip, which
// would round json.Number values and can collapse empty collections into null.
func cloneProviderValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneProviderOptions(value)
	case []any:
		if value == nil {
			return []any(nil)
		}
		copied := make([]any, len(value))
		for i := range value {
			copied[i] = cloneProviderValue(value[i])
		}
		return copied
	case []string:
		if value == nil {
			return []string(nil)
		}
		return append([]string{}, value...)
	default:
		return value
	}
}

func cloneProviderOptions(options map[string]any) map[string]any {
	if options == nil {
		return nil
	}
	copied := make(map[string]any, len(options))
	for key, value := range options {
		copied[key] = cloneProviderValue(value)
	}
	return copied
}

// Objects merge recursively. Every other client value, including null, empty
// arrays, numbers and booleans, replaces its default without interpretation.
func mergeProviderOptions(defaults, overrides map[string]any) map[string]any {
	merged := cloneProviderOptions(defaults)
	if merged == nil {
		merged = map[string]any{}
	}
	for key, value := range overrides {
		if override := object(value); override != nil && object(merged[key]) != nil {
			merged[key] = mergeProviderOptions(object(merged[key]), override)
		} else {
			merged[key] = cloneProviderValue(value)
		}
	}
	return merged
}

func applyCustomProviderRouting(j map[string]any, configured map[string]any, policy string) {
	options := mergeProviderOptions(configured, nil)
	if policy == ProviderPolicyClient {
		client := mergeProviderOptions(nil, object(j["providerOptions"]))
		if shorthand := object(j["provider"]); len(shorthand) > 0 {
			gateway, present := client["gateway"]
			if !present || object(gateway) != nil {
				client["gateway"] = mergeProviderOptions(shorthand, object(gateway))
			}
		}
		options = mergeProviderOptions(options, client)
	}
	delete(j, "provider")
	j["providerOptions"] = options
	provider := map[string]any{}
	for _, key := range []string{"only", "order"} {
		if value, present := object(options["gateway"])[key]; present {
			provider[key] = cloneProviderValue(value)
		}
	}
	if len(provider) > 0 {
		j["provider"] = provider
	}
}
