package bridge

import (
	"bytes"
	"testing"
	"time"
)

func TestStreamAggregatePreservesResponseFields(t *testing.T) {
	for _, tt := range []struct {
		name     string
		metadata any
		want     map[string]any
	}{
		{
			name: "recursive merge ignores nested nulls",
			metadata: map[string]any{
				"gateway": map[string]any{
					"routing":  map[string]any{"finalProvider": nil, "route_id": nil, "region": "us-east"},
					"trace_id": nil,
				},
				"billing": map[string]any{"currency": nil, "charged": true},
			},
			want: map[string]any{
				"gateway": map[string]any{
					"routing":  map[string]any{"finalProvider": "fireworks", "route_id": "route-1", "region": "us-east"},
					"trace_id": "trace-1",
				},
				"billing": map[string]any{"currency": "USD", "charged": true},
			},
		},
		{
			name:     "top-level null preserves metadata",
			metadata: nil,
			want: map[string]any{
				"gateway": map[string]any{
					"routing":  map[string]any{"finalProvider": "fireworks", "route_id": "route-1"},
					"trace_id": "trace-1",
				},
				"billing": map[string]any{"currency": "USD"},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := registeredService(t, "stream-aggregate")
			first := sseFrame(map[string]any{
				"id": "chat-fields-1", "object": "chat.completion.chunk", "obfuscation": "first-padding",
				"service_tier": "priority",
				"provider_metadata": map[string]any{
					"gateway": map[string]any{
						"routing":  map[string]any{"finalProvider": "fireworks", "route_id": "route-1"},
						"trace_id": "trace-1",
					},
					"billing": map[string]any{"currency": "USD"},
				},
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "hel"}}},
			})
			second := sseFrame(map[string]any{
				"id": nil, "object": "chat.completion.chunk", "obfuscation": "last-padding",
				"service_tier": nil, "provider_metadata": tt.metadata,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "lo"}, "finish_reason": "stop"}},
			})
			s.SetHost(newFakeHost(ssePlan(first, second, []byte("data: [DONE]\r\n\r\n"))).call)
			result, err := s.Handle("executor.execute", executorRequest(""))
			if err != nil {
				t.Fatalf("aggregate response: %v", err)
			}
			body, err := decodeObject(result.(Response).Payload)
			if err != nil {
				t.Fatal(err)
			}
			if body["id"] != "chat-fields-1" || body["service_tier"] != "priority" {
				t.Fatalf("later null erased response fields: %s", result.(Response).Payload)
			}
			if got := body["provider_metadata"]; !bytes.Equal(jsonBytes(got), jsonBytes(tt.want)) {
				t.Fatalf("provider_metadata = %s, want %s", jsonBytes(got), jsonBytes(tt.want))
			}
			if body["object"] != "chat.completion" {
				t.Errorf("aggregate object = %v, want chat.completion", body["object"])
			}
			if _, ok := body["obfuscation"]; ok {
				t.Error("aggregate leaked stream-only obfuscation")
			}
			choices := list(body["choices"])
			if len(choices) != 1 || object(object(choices[0])["message"])["content"] != "hello" || object(choices[0])["finish_reason"] != "stop" {
				t.Fatalf("response field merge damaged choices: %#v", choices)
			}
		})
	}
}

func TestNativeFallbackRefreshesProviderAcrossAttempts(t *testing.T) {
	s := registeredService(t, "native-fallback")
	native := jsonErrorPlan(500, map[string]any{
		"error": "empty response content",
		"providerMetadata": map[string]any{"gateway": map[string]any{"routing": map[string]any{
			"finalProvider": "old",
		}}},
	})
	success := sseFrame(map[string]any{
		"provider_metadata": map[string]any{"gateway": map[string]any{"routing": map[string]any{
			"resolvedProvider": "new",
		}}},
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"content": "hello"}, "finish_reason": "stop",
		}},
	})
	h := newFakeHost(native, ssePlan(success, []byte("data: [DONE]\r\n\r\n")))
	s.SetHost(h.call)
	result, err := s.Handle("executor.execute", executorRequest(""))
	if err != nil {
		t.Fatalf("native-to-stream fallback: %v", err)
	}
	if !bytes.Contains(result.(Response).Payload, []byte("hello")) {
		t.Fatalf("fallback lost successful response: %s", result.(Response).Payload)
	}
	if len(h.opened) != 2 || h.opened[0] || !h.opened[1] {
		t.Fatalf("upstream attempts = %#v, want native then stream", h.opened)
	}
	if len(s.logs) != 1 || len(s.logs[0].Attempts) != 2 {
		t.Fatalf("fallback logs = %#v, want one log with two attempts", s.logs)
	}
	entry := s.logs[0]
	wantSource := "provider_metadata.gateway.routing.resolvedProvider"
	if entry.Status != 200 || entry.Provider != "new" || entry.ProviderSource != wantSource {
		t.Fatalf("final log status/provider/source = %d/%q/%q, want 200/new/%q", entry.Status, entry.Provider, entry.ProviderSource, wantSource)
	}
	for i, want := range []struct {
		status   int
		mode     string
		provider string
		source   string
	}{
		{status: 500, mode: "nonstream", provider: "old", source: "providerMetadata.gateway.routing.finalProvider"},
		{status: 200, mode: "stream-aggregate", provider: "new", source: wantSource},
	} {
		attempt := entry.Attempts[i]
		if attempt.Status != want.status || attempt.Mode != want.mode || attempt.Provider != want.provider || attempt.ProviderSource != want.source {
			t.Fatalf("attempt %d = %#v, want status/mode/provider/source %d/%s/%s/%s", i, attempt, want.status, want.mode, want.provider, want.source)
		}
	}
}

func TestObserveMetadataProviderAliasesAndFinalPriority(t *testing.T) {
	for _, metadataKey := range []string{"provider_metadata", "providerMetadata"} {
		for _, tt := range []struct {
			name         string
			location     string
			routing      map[string]any
			deltaRouting map[string]any
			provider     string
			field        string
		}{
			{name: "resolved at root", routing: map[string]any{"resolvedProvider": "fireworks"}, provider: "fireworks", field: "resolvedProvider"},
			{name: "resolved in delta", location: "delta", routing: map[string]any{"resolvedProvider": "fireworks"}, provider: "fireworks", field: "resolvedProvider"},
			{name: "final in message", location: "message", routing: map[string]any{"finalProvider": "baseten"}, provider: "baseten", field: "finalProvider"},
			{name: "final beats resolved in routing", routing: map[string]any{"resolvedProvider": "fireworks", "finalProvider": "baseten"}, provider: "baseten", field: "finalProvider"},
			{name: "final beats later resolved in same frame", routing: map[string]any{"finalProvider": "baseten"}, deltaRouting: map[string]any{"resolvedProvider": "fireworks"}, provider: "baseten", field: "finalProvider"},
			{name: "candidates are not actual provider", location: "delta", routing: map[string]any{"fallbackProviders": []any{"fireworks"}, "fallbacksAvailable": []any{"baseten"}}, provider: "unknown"},
		} {
			t.Run(metadataKey+"/"+tt.name, func(t *testing.T) {
				body := map[string]any{}
				root := body
				prefix := ""
				if tt.location != "" {
					root = map[string]any{}
					body["choices"] = []any{map[string]any{"index": 0, tt.location: root}}
					prefix = "choices[0]." + tt.location + "."
				}
				root[metadataKey] = map[string]any{"gateway": map[string]any{"routing": tt.routing}}
				if tt.deltaRouting != nil {
					body["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{
						metadataKey: map[string]any{"gateway": map[string]any{"routing": tt.deltaRouting}},
					}}}
				}
				entry := LogEntry{Provider: "unknown", ProviderSource: "not_reported"}
				attempt := Attempt{Provider: "unknown", ProviderSource: "not_reported"}
				observeMetadata(body, &entry, &attempt)
				wantSource := "not_reported"
				if tt.field != "" {
					wantSource = prefix + metadataKey + ".gateway.routing." + tt.field
				}
				if entry.Provider != tt.provider || attempt.Provider != tt.provider || entry.ProviderSource != wantSource || attempt.ProviderSource != wantSource {
					t.Fatalf("provider/source = %q/%q, attempt %q/%q; want %q/%q", entry.Provider, entry.ProviderSource, attempt.Provider, attempt.ProviderSource, tt.provider, wantSource)
				}
			})
		}
		t.Run(metadataKey+"/final survives later weaker frames", func(t *testing.T) {
			entry := LogEntry{Provider: "unknown", ProviderSource: "not_reported"}
			attempt := Attempt{Provider: "unknown", ProviderSource: "not_reported"}
			wantSource := "choices[0].delta." + metadataKey + ".gateway.routing.finalProvider"
			for _, frame := range []struct {
				name string
				body map[string]any
			}{
				{
					name: "initial final",
					body: map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
						metadataKey: map[string]any{"gateway": map[string]any{"routing": map[string]any{"finalProvider": "baseten"}}},
					}}}},
				},
				{
					name: "later resolved",
					body: map[string]any{metadataKey: map[string]any{"gateway": map[string]any{"routing": map[string]any{"resolvedProvider": "fireworks"}}}},
				},
				{name: "later generic provider", body: map[string]any{"provider": "gateway"}},
			} {
				observeMetadata(frame.body, &entry, &attempt)
				if entry.Provider != "baseten" || attempt.Provider != "baseten" || entry.ProviderSource != wantSource || attempt.ProviderSource != wantSource {
					t.Fatalf("%s: provider/source = %q/%q, attempt %q/%q; want baseten/%q", frame.name, entry.Provider, entry.ProviderSource, attempt.Provider, attempt.ProviderSource, wantSource)
				}
			}
		})
	}
}

func TestUpstreamErrorTextProviderAliasesAndSource(t *testing.T) {
	for _, tt := range []struct {
		field    string
		provider string
	}{
		{field: "finalProvider", provider: "baseten"},
		{field: "resolvedProvider", provider: "fireworks"},
	} {
		t.Run(tt.field, func(t *testing.T) {
			inner := map[string]any{
				"error": map[string]any{"message": "Rate limit exceeded"},
				"providerMetadata": map[string]any{"gateway": map[string]any{"routing": map[string]any{
					tt.field: tt.provider,
				}}},
			}
			body := map[string]any{"error": map[string]any{
				"message": "request failed with status 429: " + string(jsonBytes(inner)),
			}}
			err := classifyUpstreamError(200, nil, body, time.Unix(0, 0))
			detail := err.(*upstreamError)
			wantSource := "error.message." + tt.field
			if detail.Provider != tt.provider || detail.ProviderSource != wantSource {
				t.Fatalf("error provider/source = %q/%q, want %q/%q", detail.Provider, detail.ProviderSource, tt.provider, wantSource)
			}
			entry := LogEntry{Provider: "unknown"}
			attempt := Attempt{Provider: "unknown"}
			logErrorDetails(&entry, &attempt, err)
			if entry.Provider != tt.provider || attempt.Provider != tt.provider || entry.ProviderSource != wantSource || attempt.ProviderSource != wantSource {
				t.Fatalf("logged provider/source = %q/%q, attempt %q/%q; want %q/%q", entry.Provider, entry.ProviderSource, attempt.Provider, attempt.ProviderSource, tt.provider, wantSource)
			}
			if detail.HTTPStatus != 200 || detail.UpstreamStatus != 429 || statusOf(err) != 429 {
				t.Fatalf("error routing extraction changed statuses: %#v", detail)
			}
		})
	}
}
