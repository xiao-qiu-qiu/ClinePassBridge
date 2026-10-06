package bridge

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func reasoningMappingService(t *testing.T) *Service {
	t.Helper()
	s := NewService()
	s.cfg.Models = []Model{{ID: "deepseek-flash", UpstreamID: "cline-pass/deepseek-v4.1-flash"}}
	if err := s.cfg.validate(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReasoningMappingDefaults(t *testing.T) {
	wantRules := map[string]string{
		"unset": "unset", "none": "none", "minimal": "low", "low": "low",
		"medium": "high", "high": "high", "xhigh": "high", "max": "max", "ultra": "max",
	}
	emptyRules := map[string]string{}
	partialRules := map[string]string{"low": "medium"}
	cfg := defaultConfig()
	cfg.Models = []Model{
		{ID: "flash", UpstreamID: "cline-pass/deepseek-v4.1-flash"},
		{ID: "flash-second", UpstreamID: "cline-pass/deepseek-v4.1-flash"},
		{ID: "mixed-case", UpstreamID: "vendor/DeepSeek-reasoner"},
		{ID: "deepseek-alias-only", UpstreamID: "vendor/other-model"},
		{ID: "other", UpstreamID: "vendor/other-model"},
		{ID: "explicit-empty", UpstreamID: "deepseek/empty", ReasoningMapping: &ReasoningMappingConfig{Enabled: true, Rules: map[string]string{}}},
		{ID: "explicit-nil-rules", UpstreamID: "deepseek/nil", ReasoningMapping: &ReasoningMappingConfig{Enabled: true}},
		{ID: "explicit-partial", UpstreamID: "deepseek/partial", ReasoningMapping: &ReasoningMappingConfig{Enabled: true, Rules: partialRules}},
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	for i, model := range cfg.Models {
		want := emptyRules
		if i < 3 {
			want = wantRules
		} else if model.ID == "explicit-partial" {
			want = partialRules
		}
		if got := model.ReasoningMapping; got == nil || got.Enabled != (i >= 5) || !reflect.DeepEqual(got.Rules, want) {
			t.Errorf("%s mapping = %+v, want enabled=%v / rules=%v", model.ID, got, i >= 5, want)
		}
	}
	if cfg.ReasoningMapping != nil {
		t.Fatal("fresh config retained a legacy global mapping")
	}
	before := jsonBytes(cfg)
	if err := cfg.validate(); err != nil || !bytes.Equal(before, jsonBytes(cfg)) {
		t.Fatalf("revalidation changed explicit/default mappings: %v", err)
	}
	if cfg.Models[0].ReasoningMapping == cfg.Models[1].ReasoningMapping {
		t.Fatal("aliases share a default mapping pointer")
	}
	cfg.Models[0].ReasoningMapping.Rules["low"] = "max"
	if cfg.Models[1].ReasoningMapping.Rules["low"] != "low" {
		t.Fatal("aliases share a default rules map")
	}
	cfg.Models[0].ReasoningMapping.Rules["low"] = "low"
	s := NewService()
	s.cfg = cfg
	s.cfg.Models[0].ReasoningMapping.Enabled = true
	for from, to := range wantRules {
		t.Run(from, func(t *testing.T) {
			body := map[string]any{}
			if from != "unset" {
				body["reasoning_effort"] = from
			}
			entry := LogEntry{Model: cfg.Models[0].ID, UpstreamModel: cfg.Models[0].UpstreamID, ReasoningEffort: requestReasoningEffort(jsonBytes(body))}
			s.applyReasoningMapping(body, &entry)
			wantEffort := to
			if to == "unset" {
				wantEffort = ""
				if _, present := body["reasoning_effort"]; present {
					t.Error("unset target must remove reasoning_effort")
				}
			} else if body["reasoning_effort"] != to {
				t.Errorf("outbound reasoning_effort = %v, want %s", body["reasoning_effort"], to)
			}
			if entry.ReasoningEffort != wantEffort || !reflect.DeepEqual(entry.ReasoningMapping, &ReasoningMappingLog{From: from, To: to}) {
				t.Errorf("mapping log = %+v / %q, want %s -> %s / %q", entry.ReasoningMapping, entry.ReasoningEffort, from, to, wantEffort)
			}
		})
	}
}

func TestReasoningMappingRequiresMatchingAliasAndUpstream(t *testing.T) {
	s := NewService()
	const upstream = "cline-pass/deepseek-v4.1-flash"
	s.cfg.Models = []Model{
		{ID: "first", UpstreamID: upstream, ReasoningMapping: &ReasoningMappingConfig{Enabled: true, Rules: map[string]string{"medium": "high"}}},
		{ID: "second", UpstreamID: upstream, ReasoningMapping: &ReasoningMappingConfig{Enabled: true, Rules: map[string]string{"medium": "max"}}},
		{ID: "other", UpstreamID: "vendor/other", ReasoningMapping: &ReasoningMappingConfig{Enabled: true, Rules: map[string]string{"medium": "low"}}},
		{ID: "disabled", UpstreamID: upstream, ReasoningMapping: &ReasoningMappingConfig{Rules: map[string]string{"medium": "max"}}},
		{ID: "missing-mapping", UpstreamID: upstream},
		{ID: "empty-rules", UpstreamID: upstream, ReasoningMapping: &ReasoningMappingConfig{Enabled: true, Rules: map[string]string{}}},
		{ID: "partial-rules", UpstreamID: upstream, ReasoningMapping: &ReasoningMappingConfig{Enabled: true, Rules: map[string]string{"low": "max"}}},
	}
	for _, tc := range []struct {
		name, alias, upstream, target string
	}{
		{"first-alias", "first", upstream, "high"},
		{"second-alias-same-upstream", "second", upstream, "max"},
		{"other-model-explicit-mapping", "other", "vendor/other", "low"},
		{"unknown-alias", "unknown", upstream, ""},
		{"missing-alias", "", upstream, ""},
		{"missing-upstream", "first", "", ""},
		{"changed-upstream", "first", "deepseek/changed", ""},
		{"another-model-upstream", "first", "vendor/other", ""},
		{"disabled", "disabled", upstream, ""},
		{"missing-mapping", "missing-mapping", upstream, ""},
		{"empty-rules", "empty-rules", upstream, ""},
		{"partial-rules-unmapped-effort", "partial-rules", upstream, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"model": upstream, "reasoning_effort": "medium"}
			entry := LogEntry{Model: tc.alias, UpstreamModel: tc.upstream, ReasoningEffort: "medium"}
			s.applyReasoningMapping(body, &entry)
			want := "medium"
			var mapping *ReasoningMappingLog
			if tc.target != "" {
				want = tc.target
				mapping = &ReasoningMappingLog{From: "medium", To: tc.target}
			}
			if body["reasoning_effort"] != want || entry.ReasoningEffort != want || !reflect.DeepEqual(entry.ReasoningMapping, mapping) {
				t.Fatalf("mapping = %s / %+v, want effort=%s / mapping=%+v", jsonBytes(body), entry, want, mapping)
			}
		})
	}
}

func TestReasoningMappingLegacyMigration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		rules   map[string]string
	}{
		{"partial-enabled", true, map[string]string{"medium": "max"}},
		{"partial-disabled", false, map[string]string{"medium": "max"}},
		{"empty", true, map[string]string{}},
		{"nil-rules", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacy := &ReasoningMappingConfig{Enabled: tc.enabled, Rules: tc.rules}
			cfg := defaultConfig()
			cfg.ReasoningMapping = legacy
			cfg.Models = []Model{
				{ID: "flash", UpstreamID: "cline-pass/deepseek-v4.1-flash"},
				{ID: "flash-second", UpstreamID: "cline-pass/deepseek-v4.1-flash"},
				{ID: "mixed-case", UpstreamID: "vendor/DeepSeek-reasoner"},
				{ID: "deepseek-alias-only", UpstreamID: "vendor/other-model"},
				{ID: "explicit-partial", UpstreamID: "deepseek/explicit", ReasoningMapping: &ReasoningMappingConfig{Rules: map[string]string{"low": "high"}}},
				{ID: "explicit-empty", UpstreamID: "deepseek/empty", ReasoningMapping: &ReasoningMappingConfig{Enabled: true, Rules: map[string]string{}}},
				{ID: "explicit-nil-rules", UpstreamID: "deepseek/nil", ReasoningMapping: &ReasoningMappingConfig{Enabled: true}},
				{ID: "other-explicit", UpstreamID: "vendor/other-model", ReasoningMapping: &ReasoningMappingConfig{Enabled: true, Rules: map[string]string{"low": "max"}}},
			}
			if err := cfg.validate(); err != nil {
				t.Fatal(err)
			}
			if cfg.ReasoningMapping != nil {
				t.Fatal("validation did not clear the legacy global mapping")
			}
			for i, model := range cfg.Models {
				want := &ReasoningMappingConfig{Rules: map[string]string{}}
				switch {
				case i < 3:
					want = legacy
				case model.ID == "explicit-partial":
					want.Rules["low"] = "high"
				case model.ID == "explicit-empty" || model.ID == "explicit-nil-rules":
					want.Enabled = true
				case model.ID == "other-explicit":
					want.Enabled = true
					want.Rules["low"] = "max"
				}
				if !reflect.DeepEqual(model.ReasoningMapping, want) {
					t.Errorf("%s mapping = %+v, want %+v", model.ID, model.ReasoningMapping, want)
				}
				if i < 3 && (model.ReasoningMapping == legacy || (i > 0 && model.ReasoningMapping == cfg.Models[0].ReasoningMapping)) {
					t.Error("migration shared a mapping pointer with the legacy config or another alias")
				}
			}
			before := jsonBytes(cfg)
			if err := cfg.validate(); err != nil || !bytes.Equal(before, jsonBytes(cfg)) {
				t.Fatalf("revalidation changed migrated/explicit mappings: %v", err)
			}
			legacyBefore := jsonBytes(legacy)
			secondBefore := jsonBytes(cfg.Models[1:3])
			cfg.Models[0].ReasoningMapping.Enabled = !tc.enabled
			cfg.Models[0].ReasoningMapping.Rules["low"] = "minimal"
			if !bytes.Equal(legacyBefore, jsonBytes(legacy)) || !bytes.Equal(secondBefore, jsonBytes(cfg.Models[1:3])) {
				t.Fatal("editing one migrated mapping changed legacy rules or another alias")
			}
		})
	}
}

func TestReasoningMappingPayloadSemantics(t *testing.T) {
	for _, tc := range []struct {
		name     string
		payload  string
		want     string
		disabled bool
		rules    map[string]string
		effort   string
		mapping  *ReasoningMappingLog
	}{
		{name: "disabled-value", payload: `{"reasoning_effort":" ULTRA "}`, disabled: true, effort: "ultra"},
		{name: "disabled-unset", payload: `{}`, disabled: true, rules: map[string]string{"unset": "high"}},
		{name: "normalize", payload: `{"reasoning_effort":" XHIGH "}`, want: `{"reasoning_effort":"high"}`, effort: "high", mapping: &ReasoningMappingLog{"xhigh", "high"}},
		{name: "missing-inject", payload: `{}`, want: `{"reasoning_effort":"low"}`, rules: map[string]string{"unset": "low"}, effort: "low", mapping: &ReasoningMappingLog{"unset", "low"}},
		{name: "null-inject", payload: `{"reasoning_effort":null}`, want: `{"reasoning_effort":"low"}`, rules: map[string]string{"unset": "low"}, effort: "low", mapping: &ReasoningMappingLog{"unset", "low"}},
		{name: "empty-inject", payload: `{"reasoning_effort":"  "}`, want: `{"reasoning_effort":"low"}`, rules: map[string]string{"unset": "low"}, effort: "low", mapping: &ReasoningMappingLog{"unset", "low"}},
		{name: "null-delete", payload: `{"reasoning_effort":null}`, want: `{}`, mapping: &ReasoningMappingLog{"unset", "unset"}},
		{name: "empty-delete", payload: `{"reasoning_effort":""}`, want: `{}`, mapping: &ReasoningMappingLog{"unset", "unset"}},
		{name: "custom-delete", payload: `{"reasoning_effort":"low"}`, want: `{}`, rules: map[string]string{"low": "unset"}, mapping: &ReasoningMappingLog{"low", "unset"}},
		{name: "delete-reveals-nested-effort", payload: `{"reasoning_effort":"low","reasoning":{"effort":"max"}}`, want: `{"reasoning":{"effort":"max"}}`, rules: map[string]string{"low": "unset"}, effort: "max", mapping: &ReasoningMappingLog{"low", "unset"}},
		{name: "missing-with-reasoning", payload: `{"reasoning":{"effort":"minimal"}}`, rules: map[string]string{"unset": "high"}, effort: "minimal"},
		{name: "null-with-reasoning", payload: `{"reasoning_effort":null,"reasoning":{"effort":"max"}}`, rules: map[string]string{"unset": "high"}, effort: "max"},
		{name: "empty-with-thinking", payload: `{"reasoning_effort":"","thinking":{"type":"disabled"}}`, rules: map[string]string{"unset": "high"}, effort: "none"},
		{name: "empty-reasoning-object", payload: `{"reasoning":{}}`, rules: map[string]string{"unset": "high"}},
		{name: "empty-thinking-object", payload: `{"thinking":{}}`, rules: map[string]string{"unset": "high"}},
		{name: "null-strategies-allow-injection", payload: `{"reasoning":null,"thinking":null}`, want: `{"reasoning":null,"thinking":null,"reasoning_effort":"high"}`, rules: map[string]string{"unset": "high"}, effort: "high", mapping: &ReasoningMappingLog{"unset", "high"}},
		{name: "explicit-top-level-with-strategies", payload: `{"reasoning_effort":"medium","reasoning":{"effort":"minimal"},"thinking":{"type":"adaptive"}}`, want: `{"reasoning_effort":"high","reasoning":{"effort":"minimal"},"thinking":{"type":"adaptive"}}`, effort: "high", mapping: &ReasoningMappingLog{"medium", "high"}},
		{name: "provider-options-only", payload: `{"providerOptions":{"deepseek":{"reasoningEffort":"ultra"},"anthropic":{"thinking":{"type":"enabled","budgetTokens":2048}}}}`, want: `{"reasoning_effort":"high","providerOptions":{"deepseek":{"reasoningEffort":"ultra"},"anthropic":{"thinking":{"type":"enabled","budgetTokens":2048}}}}`, rules: map[string]string{"unset": "high"}, effort: "high", mapping: &ReasoningMappingLog{"unset", "high"}},
		{name: "unknown-source", payload: `{"reasoning_effort":"future-value"}`},
		{name: "literal-unset-passthrough", payload: `{"reasoning_effort":"unset"}`, rules: map[string]string{"unset": "high"}},
		{name: "auto-passthrough", payload: `{"reasoning_effort":"auto"}`, effort: "auto"},
		{name: "number-passthrough", payload: `{"reasoning_effort":123}`},
		{name: "boolean-passthrough", payload: `{"reasoning_effort":false}`},
		{name: "object-passthrough", payload: `{"reasoning_effort":{"effort":"high"}}`},
		{name: "array-passthrough", payload: `{"reasoning_effort":["high"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := reasoningMappingService(t)
			s.cfg.Models[0].ReasoningMapping.Enabled = !tc.disabled
			for from, to := range tc.rules {
				s.cfg.Models[0].ReasoningMapping.Rules[from] = to
			}
			body, err := decodeObject([]byte(tc.payload))
			if err != nil {
				t.Fatal(err)
			}
			entry := LogEntry{Model: s.cfg.Models[0].ID, UpstreamModel: s.cfg.Models[0].UpstreamID, ReasoningEffort: requestReasoningEffort([]byte(tc.payload))}
			s.applyReasoningMapping(body, &entry)
			want := tc.want
			if want == "" {
				want = tc.payload
			}
			wantBody, err := decodeObject([]byte(want))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(body, wantBody) {
				t.Errorf("mapped body = %s, want %s", jsonBytes(body), want)
			}
			if entry.ReasoningEffort != tc.effort || !reflect.DeepEqual(entry.ReasoningMapping, tc.mapping) {
				t.Errorf("mapping log = %+v / %q, want %+v / %q", entry.ReasoningMapping, entry.ReasoningEffort, tc.mapping, tc.effort)
			}
		})
	}
}

// Capture the bytes sent through the actual host callback; fakeHost supplies
// completion frames, per-attempt responses, and stream-close synchronization.
func reasoningMappingHost(t *testing.T, s *Service, plans ...hostPlan) (*fakeHost, <-chan map[string]any) {
	t.Helper()
	h := newFakeHost(plans...)
	bodies := make(chan map[string]any, len(plans)+1)
	s.SetHost(func(method string, payload, out any) error {
		if method == "host.http.do_stream" {
			body, err := decodeObject(payload.(map[string]any)["body"].([]byte))
			if err != nil {
				return err
			}
			bodies <- body
		}
		return h.call(method, payload, out)
	})
	t.Cleanup(func() { observabilityShutdown(t, s) })
	return h, bodies
}

func TestReasoningMappingExecutorOutbound(t *testing.T) {
	for _, transport := range []struct {
		name   string
		mode   string
		method string
		stream bool
	}{
		{"native", "native", "executor.execute", false},
		{"aggregate", "stream-aggregate", "executor.execute", true},
		{"stream", "native", "executor.execute_stream", true},
	} {
		for _, tc := range []struct {
			name    string
			effort  string
			want    string
			enabled bool
			rules   map[string]string
			mapping *ReasoningMappingLog
		}{
			{name: "disabled", effort: "ultra", want: "ultra"},
			{name: "enabled", effort: "ultra", want: "max", enabled: true, mapping: &ReasoningMappingLog{"ultra", "max"}},
			{name: "unset-injection", want: "high", enabled: true, rules: map[string]string{"unset": "high"}, mapping: &ReasoningMappingLog{"unset", "high"}},
			{name: "unset-deletion", effort: "low", enabled: true, rules: map[string]string{"low": "unset"}, mapping: &ReasoningMappingLog{"low", "unset"}},
		} {
			t.Run(transport.name+"/"+tc.name, func(t *testing.T) {
				s := registeredService(t, transport.mode)
				s.cfg.Models[0].ReasoningMapping.Enabled = tc.enabled
				for from, to := range tc.rules {
					s.cfg.Models[0].ReasoningMapping.Rules[from] = to
				}
				plan := jsonPlan(map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "hello"}, "finish_reason": "stop"}}})
				if transport.stream {
					plan = ssePlan(simpleSSE())
				}
				h, bodies := reasoningMappingHost(t, s, plan)
				var req ExecutorRequest
				if err := json.Unmarshal(executorRequest("mapping-client"), &req); err != nil {
					t.Fatal(err)
				}
				payload, err := decodeObject(req.Payload)
				if err != nil {
					t.Fatal(err)
				}
				payload["providerOptions"] = map[string]any{"deepseek": map[string]any{"reasoningEffort": "xhigh"}}
				if tc.effort != "" {
					payload["reasoning_effort"] = tc.effort
				}
				req.Payload = jsonBytes(payload)
				if _, err := s.Handle(transport.method, jsonBytes(req)); err != nil {
					t.Fatal(err)
				}
				if transport.method == "executor.execute_stream" {
					select {
					case <-h.clientClosed:
					case <-time.After(3 * time.Second):
						t.Fatal("client stream did not close")
					}
				}
				if len(bodies) != 1 {
					t.Fatalf("outbound requests = %d, want 1", len(bodies))
				}
				body := <-bodies
				value, present := body["reasoning_effort"]
				if present != (tc.want != "") || str(value) != tc.want || body["stream"] != transport.stream || body["model"] != "cline-pass/deepseek-v4.1-flash" {
					t.Errorf("outbound body = %s", jsonBytes(body))
				}
				if got := object(object(body["providerOptions"])["deepseek"])["reasoningEffort"]; got != "xhigh" {
					t.Errorf("nested provider reasoning changed to %v", got)
				}
				s.mu.RLock()
				defer s.mu.RUnlock()
				if len(s.logs) != 1 {
					t.Fatalf("logs = %d, want 1", len(s.logs))
				}
				entry := s.logs[0]
				if entry.Status != 200 || entry.Stream != (transport.method == "executor.execute_stream") || entry.ReasoningEffort != tc.want || !reflect.DeepEqual(entry.ReasoningMapping, tc.mapping) {
					t.Errorf("outbound mapping log = %+v", entry)
				}
			})
		}
	}
}

func TestReasoningMappingRetryOnlyOnce(t *testing.T) {
	s := registeredService(t, "native-fallback")
	s.cfg.Models[0].ReasoningMapping.Enabled = true
	s.cfg.Models[0].ReasoningMapping.Rules["low"] = "high"
	s.cfg.Models[0].ReasoningMapping.Rules["high"] = "max"
	h, bodies := reasoningMappingHost(t, s,
		jsonPlan(map[string]any{"success": false, "error": "empty response content"}), ssePlan(simpleSSE()))
	if _, err := s.Handle("executor.execute", requestWithPayload(`{"messages":[{"role":"user","content":"hello"}],"reasoning_effort":"low"}`)); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || !reflect.DeepEqual(h.opened, []bool{false, true}) {
		t.Fatalf("retry requests = %d / %v, want native then stream", len(bodies), h.opened)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if got := (<-bodies)["reasoning_effort"]; got != "high" {
			t.Errorf("attempt %d reasoning_effort = %v, want high (not remapped to max)", attempt+1, got)
		}
	}
	if len(s.logs) != 1 || len(s.logs[0].Attempts) != 2 {
		t.Fatalf("retry logs = %+v", s.logs)
	}
	entry := s.logs[0]
	if entry.Status != 200 || entry.ReasoningEffort != "high" || !reflect.DeepEqual(entry.ReasoningMapping, &ReasoningMappingLog{"low", "high"}) {
		t.Errorf("retry changed original mapping: %+v", entry)
	}
}

func TestReasoningMappingProviderProbeOutbound(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			s := probeService(t)
			s.cfg.Models[0].ReasoningMapping.Enabled = enabled
			s.cfg.Models[0].ReasoningMapping.Rules["unset"] = "xhigh"
			_, bodies := reasoningMappingHost(t, s, ssePlan(simpleSSE()))
			result, err := s.Handle("management.handle", jsonBytes(ManagementRequest{
				Method: "POST", Path: apiBase + "/models/providers", HostCallbackID: "probe-callback",
				Body: jsonBytes(map[string]any{"model": "deepseek-flash", "credential_id": "probe-account"}),
			}))
			if err != nil {
				t.Fatal(err)
			}
			if response := result.(ManagementResponse); response.StatusCode != 200 {
				t.Fatalf("provider probe: %d / %s", response.StatusCode, response.Body)
			}
			if len(bodies) != 1 || len(s.logs) != 1 {
				t.Fatalf("provider requests/logs = %d/%d, want 1/1", len(bodies), len(s.logs))
			}
			body := <-bodies
			value, present := body["reasoning_effort"]
			want, mapping := "", (*ReasoningMappingLog)(nil)
			if enabled {
				want, mapping = "xhigh", &ReasoningMappingLog{"unset", "xhigh"}
			}
			if present != enabled || str(value) != want || body["stream"] != true || number(body["max_tokens"]) != 16 || body["providerOptions"] != nil {
				t.Errorf("provider outbound body = %s", jsonBytes(body))
			}
			entry := s.logs[0]
			if entry.Status != 200 || len(entry.Attempts) != 1 || entry.Attempts[0].Mode != "provider-probe" || entry.ReasoningEffort != want || !reflect.DeepEqual(entry.ReasoningMapping, mapping) {
				t.Errorf("provider mapping log = %+v", entry)
			}
		})
	}
}

func TestReasoningMappingConfigCopyIsolation(t *testing.T) {
	s := reasoningMappingService(t)
	s.cfg.Models = append(s.cfg.Models,
		Model{ID: "second", UpstreamID: s.cfg.Models[0].UpstreamID},
		Model{ID: "partial", UpstreamID: "vendor/other", ReasoningMapping: &ReasoningMappingConfig{Enabled: true, Rules: map[string]string{"low": "medium"}}},
		Model{ID: "empty", UpstreamID: "deepseek/empty", ReasoningMapping: &ReasoningMappingConfig{Enabled: true, Rules: map[string]string{}}},
	)
	if err := s.cfg.validate(); err != nil {
		t.Fatal(err)
	}
	// Even a shared input pointer must become independent in each copied model.
	s.cfg.Models = append(s.cfg.Models,
		Model{ID: "shared-input", UpstreamID: s.cfg.Models[0].UpstreamID, ReasoningMapping: s.cfg.Models[0].ReasoningMapping},
		Model{ID: "nil-mapping", UpstreamID: "vendor/nil"},
	)
	s.cfg.ReasoningMapping = &ReasoningMappingConfig{Enabled: true, Rules: map[string]string{"low": "high"}}
	before := jsonBytes(s.config())
	other := reasoningMappingService(t)
	otherBefore := jsonBytes(other.config())
	for i, model := range s.cfg.Models {
		t.Run(model.ID, func(t *testing.T) {
			copied := s.config()
			mapping := copied.Models[i].ReasoningMapping
			if model.ReasoningMapping == nil {
				if mapping != nil {
					t.Fatal("copy materialized a missing model mapping")
				}
				return
			}
			if mapping == model.ReasoningMapping {
				t.Fatal("config copy retained the live model mapping pointer")
			}
			untouched := make([][]byte, len(copied.Models))
			for j := range copied.Models {
				untouched[j] = jsonBytes(copied.Models[j])
			}
			mapping.Enabled = !mapping.Enabled
			mapping.Rules["low"] = "max"
			delete(mapping.Rules, "ultra")
			mapping.Rules["future"] = "low"
			for j := range copied.Models {
				if j != i && !bytes.Equal(untouched[j], jsonBytes(copied.Models[j])) {
					t.Errorf("editing %s changed copied model %s", model.ID, copied.Models[j].ID)
				}
			}
			if !bytes.Equal(before, jsonBytes(s.config())) || !bytes.Equal(otherBefore, jsonBytes(other.config())) {
				t.Fatal("editing a config copy changed live mappings or another service's defaults")
			}
		})
	}
	copied := s.config()
	if copied.ReasoningMapping == s.cfg.ReasoningMapping {
		t.Fatal("config copy retained the legacy mapping pointer")
	}
	copied.ReasoningMapping.Enabled = false
	copied.ReasoningMapping.Rules["low"] = "max"
	if !bytes.Equal(before, jsonBytes(s.config())) {
		t.Fatal("editing a copied legacy mapping changed live state")
	}
}

func TestReasoningMappingModelPUTPersistenceAndIsolation(t *testing.T) {
	s := observabilityService(t, "")
	cfg := s.config()
	cfg.TimeoutSeconds = 321
	cfg.LogRetention = 75
	cfg.MaxResponseBytes = 2 << 20
	cfg.NonstreamMode = "native-fallback"
	cfg.ProviderPolicy = ProviderPolicyClient
	cfg.Models = append(cfg.Models,
		Model{ID: "flash-second", UpstreamID: cfg.Models[0].UpstreamID, Providers: []string{"other-provider"}, ReasoningMapping: &ReasoningMappingConfig{Enabled: true, Rules: map[string]string{"medium": "max"}}},
		Model{ID: "deepseek-alias-only", UpstreamID: "vendor/other-model", Providers: []string{}},
	)
	if err := s.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		model   int
		enabled bool
		rules   map[string]string
	}{
		// Every accepted target, with ultra deliberately omitted.
		{"partial-table", 0, true, map[string]string{
			"unset": "none", "none": "unset", "minimal": "minimal", "low": "low",
			"medium": "medium", "high": "high", "xhigh": "xhigh", "max": "max",
		}},
		{"replace-second-alias-with-single-rule", 1, true, map[string]string{"low": "max"}},
		{"clear-enabled", 0, true, map[string]string{}},
		{"clear-disabled", 0, false, map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := &ReasoningMappingConfig{Enabled: tc.enabled, Rules: tc.rules}
			wantConfig := s.config()
			wantConfig.Models[tc.model].ReasoningMapping = want
			response := observabilityManagement(t, s, "PUT", "/models/reasoning", nil, jsonBytes(map[string]any{
				"model": wantConfig.Models[tc.model].ID, "upstream_id": wantConfig.Models[tc.model].UpstreamID, "reasoning_mapping": want,
				// Fields belonging to other editors must not be applied by this endpoint.
				"models": []Model{}, "timeout_seconds": 1, "log_retention": 1, "max_response_bytes": 1,
				"nonstream_mode": "invalid", "provider_policy": "invalid", "base_url": "https://example.invalid", "data_dir": "ignored",
			}), 200)
			var returned struct {
				Models []Model `json:"models"`
			}
			if err := json.Unmarshal(response.Body, &returned); err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(response.Body, &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 1 || fields["models"] == nil || !reflect.DeepEqual(returned.Models, wantConfig.Models) {
				t.Fatalf("PUT response = %s, want only the complete updated models list", response.Body)
			}
			if got := s.config(); !reflect.DeepEqual(got, wantConfig) {
				t.Fatalf("PUT changed other settings/models or merged old/default rules: got=%s want=%s", jsonBytes(got), jsonBytes(wantConfig))
			}
			raw, err := os.ReadFile(filepath.Join(wantConfig.DataDir, "settings.json"))
			if err != nil {
				t.Fatal(err)
			}
			var persisted Config
			if err := json.Unmarshal(raw, &persisted); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(persisted, wantConfig) {
				t.Errorf("persisted config = %s, want %s", raw, jsonBytes(wantConfig))
			}
			restarted := observabilityService(t, wantConfig.DataDir)
			if got := restarted.config(); !reflect.DeepEqual(got, wantConfig) {
				t.Errorf("reload changed mapping or unrelated config: got=%s want=%s", jsonBytes(got), jsonBytes(wantConfig))
			}
		})
	}
}

func TestReasoningMappingModelRejectedPUTPreservesStateAndDisk(t *testing.T) {
	s := observabilityService(t, "")
	observabilityManagement(t, s, "PUT", "/models/reasoning", nil, []byte(`{"model":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","reasoning_mapping":{"enabled":true,"rules":{"low":"medium"}}}`), 200)
	path := filepath.Join(s.config().DataDir, "settings.json")
	beforeDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeConfig := jsonBytes(s.config())
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"unknown-alias", `{"model":"missing","upstream_id":"cline-pass/deepseek-v4.1-flash","reasoning_mapping":{"enabled":false,"rules":{}}}`, 404},
		{"changed-upstream", `{"model":"deepseek-flash","upstream_id":"deepseek/changed","reasoning_mapping":{"enabled":false,"rules":{}}}`, 409},
		{"missing-mapping", `{"model":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash"}`, 400},
		{"null-mapping", `{"model":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","reasoning_mapping":null}`, 400},
		{"malformed-json", `{"model":`, 400},
		{"non-object-json", `[]`, 400},
		{"unknown-source", `{"model":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","reasoning_mapping":{"enabled":false,"rules":{"future":"high","low":"max"}}}`, 400},
		{"auto-source", `{"model":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","reasoning_mapping":{"rules":{"auto":"high"}}}`, 400},
		{"ultra-target", `{"model":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","reasoning_mapping":{"rules":{"low":"ultra"}}}`, 400},
		{"auto-target", `{"model":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","reasoning_mapping":{"rules":{"low":"auto"}}}`, 400},
		{"empty-target", `{"model":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","reasoning_mapping":{"rules":{"low":""}}}`, 400},
		{"target-type", `{"model":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","reasoning_mapping":{"rules":{"low":123}}}`, 400},
		{"rules-type", `{"model":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","reasoning_mapping":{"rules":[]}}`, 400},
		{"enabled-type", `{"model":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","reasoning_mapping":{"enabled":"true","rules":{"low":"max"}}}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observabilityManagement(t, s, "PUT", "/models/reasoning", nil, []byte(tc.body), tc.status)
			if !bytes.Equal(beforeConfig, jsonBytes(s.config())) {
				t.Error("rejected model mapping PUT polluted the live configuration")
			}
			afterDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(beforeDisk, afterDisk) {
				t.Error("rejected model mapping PUT changed persisted settings")
			}
		})
	}
}

func TestReasoningMappingConfigPUTModelReorder(t *testing.T) {
	s := observabilityService(t, "")
	defaultMapping := s.config().Models[0].ReasoningMapping
	cfg := s.config()
	cfg.Models[0].Providers = []string{"first-provider"}
	cfg.Models[0].ReasoningMapping = &ReasoningMappingConfig{Enabled: true, Rules: map[string]string{"low": "high", "unset": "high"}}
	cfg.Models = append(cfg.Models, Model{
		ID: "flash-second", UpstreamID: cfg.Models[0].UpstreamID, Providers: []string{"second-provider"},
		ReasoningMapping: &ReasoningMappingConfig{Rules: map[string]string{"high": "max"}},
	})
	if err := s.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	models := []Model{
		{ID: "flash-second", UpstreamID: cfg.Models[0].UpstreamID, Providers: []string{"second-provider"}, ReasoningMapping: &ReasoningMappingConfig{Enabled: true, Rules: map[string]string{"medium": "max"}}},
		// Missing mapping/providers are initialized for this model, not inherited
		// from the model that previously occupied its position in the slice.
		{ID: "deepseek-flash", UpstreamID: cfg.Models[0].UpstreamID},
	}
	response := observabilityManagement(t, s, "PUT", "/config", nil, jsonBytes(map[string]any{"models": models}), 200)
	want := cfg
	want.Models = []Model{models[0], {ID: "deepseek-flash", UpstreamID: cfg.Models[0].UpstreamID, Providers: []string{"deepseek"}, ReasoningMapping: defaultMapping}}
	var returned Config
	if err := json.Unmarshal(response.Body, &returned); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(returned, want) || !reflect.DeepEqual(s.config(), want) {
		t.Fatalf("reordered models inherited another position's mapping/providers: response=%s state=%s want=%s", response.Body, jsonBytes(s.config()), jsonBytes(want))
	}
}

func TestReasoningMappingConfigPUTEmptyRulesClearsAndReloads(t *testing.T) {
	s := observabilityService(t, "")
	cfg := s.config()
	cfg.Models[0].ReasoningMapping = &ReasoningMappingConfig{Enabled: true, Rules: map[string]string{"low": "high", "medium": "max", "unset": "minimal"}}
	cfg.Models = append(cfg.Models, Model{
		ID: "flash-second", UpstreamID: cfg.Models[0].UpstreamID, Providers: []string{},
		ReasoningMapping: &ReasoningMappingConfig{Enabled: true, Rules: map[string]string{"medium": "high"}},
	})
	if err := s.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	want := s.config()
	want.Models[0].ReasoningMapping = &ReasoningMappingConfig{Enabled: true, Rules: map[string]string{}}
	response := observabilityManagement(t, s, "PUT", "/config", nil, jsonBytes(map[string]any{"models": want.Models}), 200)
	var returned Config
	if err := json.Unmarshal(response.Body, &returned); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(returned, want) || !reflect.DeepEqual(s.config(), want) {
		t.Fatalf("explicit empty rules retained old/default rules or changed another alias: response=%s state=%s want=%s", response.Body, jsonBytes(s.config()), jsonBytes(want))
	}
	raw, err := os.ReadFile(filepath.Join(want.DataDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted Config
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted, want) {
		t.Errorf("persisted empty rules/config = %s, want %s", raw, jsonBytes(want))
	}
	restarted := observabilityService(t, want.DataDir)
	if got := restarted.config(); !reflect.DeepEqual(got, want) {
		t.Errorf("reload restored cleared rules or changed another alias: got=%s want=%s", jsonBytes(got), jsonBytes(want))
	}
}

func TestReasoningMappingConfigRejectedPUTPreservesStateAndDisk(t *testing.T) {
	s := observabilityService(t, "")
	observabilityManagement(t, s, "PUT", "/models/reasoning", nil, []byte(`{"model":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","reasoning_mapping":{"enabled":true,"rules":{"low":"medium"}}}`), 200)
	path := filepath.Join(s.config().DataDir, "settings.json")
	beforeDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeConfig := jsonBytes(s.config())
	for _, tc := range []struct{ name, mapping, extra string }{
		{"unknown-source", `{"enabled":false,"rules":{"future":"high","low":"max"}}`, ""},
		{"auto-source", `{"rules":{"auto":"high"}}`, ""},
		{"ultra-target", `{"rules":{"low":"ultra"}}`, ""},
		{"auto-target", `{"rules":{"low":"auto"}}`, ""},
		{"empty-target", `{"rules":{"low":""}}`, ""},
		{"target-type", `{"rules":{"low":123}}`, ""},
		{"rules-type", `{"rules":[]}`, ""},
		{"enabled-type", `{"enabled":"true","rules":{"low":"max"}}`, ""},
		{"later-decode-error", `{"enabled":false,"rules":{"low":"max"}}`, `,"timeout_seconds":"invalid"`},
		{"later-validation-error", `{"enabled":false,"rules":{"low":"max"}}`, `,"timeout_seconds":1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"models":[{"id":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","reasoning_mapping":` + tc.mapping + `}]` + tc.extra + `}`
			observabilityManagement(t, s, "PUT", "/config", nil, []byte(body), 400)
			if !bytes.Equal(beforeConfig, jsonBytes(s.config())) {
				t.Error("rejected PUT polluted the live configuration")
			}
			afterDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(beforeDisk, afterDisk) {
				t.Error("rejected PUT changed persisted settings")
			}
		})
	}
}

func TestReasoningMappingBackoffSeparatesSources(t *testing.T) {
	s := observabilityService(t, "")
	s.cfg.Models[0].ReasoningMapping.Enabled = true
	now := time.Now().UTC()
	s.rateLimits.clock = func() time.Time { return now }
	c := Credential{Type: Provider, ID: "credential-1", Label: "test credential", APIKey: "test-key"}
	lease, err := s.rateLimits.acquire(c, "cline-pass/deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	lease.finish(classifyUpstreamError(200, nil, map[string]any{"error": teamLimitMessage}, now))
	for round := 0; round < 2; round++ {
		for _, from := range []string{"medium", "high", "xhigh"} {
			_, err := s.Handle("executor.execute", requestWithPayload(string(jsonBytes(map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "hello"}}, "reasoning_effort": from,
			}))))
			if limited := asTeamRateLimit(err); limited == nil || !limited.Local || statusOf(err) != 429 {
				t.Fatalf("%s: expected local 429 without an upstream call, got %v", from, err)
			}
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.logs) != 3 {
		t.Fatalf("backoff rows = %d, want three source-specific rows: %+v", len(s.logs), s.logs)
	}
	seen := map[string]bool{}
	window := s.logs[0].BackoffWindowID
	for _, entry := range s.logs {
		mapping := entry.ReasoningMapping
		if mapping == nil {
			t.Fatal("local backoff lost reasoning mapping")
		}
		if seen[mapping.From] || (mapping.From != "medium" && mapping.From != "high" && mapping.From != "xhigh") {
			t.Errorf("unexpected/duplicate source %q", mapping.From)
		}
		seen[mapping.From] = true
		if mapping.To != "high" || entry.ReasoningEffort != "high" || entry.RequestCount != 2 || !entry.UpstreamSkipped || entry.UpstreamAttempts == nil || *entry.UpstreamAttempts != 0 || window == "" || entry.BackoffWindowID != window {
			t.Errorf("source-specific backoff aggregation = %+v", entry)
		}
	}
}
