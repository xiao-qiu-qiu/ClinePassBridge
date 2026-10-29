package bridge

import (
	"net/url"
	"testing"
	"time"
)

func TestObserveServedModelDetectsDegradedPool(t *testing.T) {
	for _, tt := range []struct {
		name     string
		frames   []map[string]any
		served   string
		degraded bool
	}{
		{
			name:     "main pool model",
			frames:   []map[string]any{{"model": "deepseek/deepseek-v4.1-flash-1017"}},
			served:   "deepseek/deepseek-v4.1-flash-1017",
			degraded: false,
		},
		{
			name:     "contributor-fallbacks pool",
			frames:   []map[string]any{{"model": "vmc/deepseek-v4p1-flash-1017-contributor-fallbacks"}},
			served:   "vmc/deepseek-v4p1-flash-1017-contributor-fallbacks",
			degraded: true,
		},
		{
			name:     "plain /fallbacks suffix",
			frames:   []map[string]any{{"model": "vmc/qwen3p8-max-contributor-fallbacks"}},
			served:   "vmc/qwen3p8-max-contributor-fallbacks",
			degraded: true,
		},
		{
			name:     "a model whose name merely contains fallback",
			frames:   []map[string]any{{"model": "vendor/fallbackless-model"}},
			served:   "vendor/fallbackless-model",
			degraded: false,
		},
		{
			name: "last frame wins, as the pool is reported on every frame",
			frames: []map[string]any{
				{"model": "deepseek/deepseek-v4.1-flash-1017"},
				{"model": "vmc/deepseek-v4p1-flash-1017-contributor-fallbacks"},
			},
			served:   "vmc/deepseek-v4p1-flash-1017-contributor-fallbacks",
			degraded: true,
		},
		{
			name:     "frame without a model leaves the record alone",
			frames:   []map[string]any{{"model": "vmc/deepseek-v4p1-flash-1017-contributor-fallbacks"}, {}},
			served:   "vmc/deepseek-v4p1-flash-1017-contributor-fallbacks",
			degraded: true,
		},
		{
			name:     "empty model is not a served model",
			frames:   []map[string]any{{"model": ""}},
			served:   "",
			degraded: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			entry := &LogEntry{}
			attempt := &Attempt{}
			for _, frame := range tt.frames {
				observeServedModel(frame, entry, attempt)
			}
			if entry.ServedModel != tt.served || attempt.ServedModel != tt.served {
				t.Fatalf("served model: entry %q attempt %q, want %q", entry.ServedModel, attempt.ServedModel, tt.served)
			}
			if entry.Degraded != tt.degraded || attempt.Degraded != tt.degraded {
				t.Fatalf("degraded: entry %v attempt %v, want %v", entry.Degraded, attempt.Degraded, tt.degraded)
			}
		})
	}
}

// A degraded request is one the provider pin could not prevent: the gateway can
// report finalProvider "deepseek" and still serve from the degraded pool, so the
// provider must not clear the degraded mark.
func TestObserveMetadataKeepsDegradedWithPinnedProvider(t *testing.T) {
	entry := &LogEntry{}
	attempt := &Attempt{}
	observeMetadata(map[string]any{
		"model": "vmc/deepseek-v4p1-flash-1017-contributor-fallbacks",
		"provider_metadata": map[string]any{
			"gateway": map[string]any{
				"routing": map[string]any{"finalProvider": "deepseek"},
			},
		},
	}, entry, attempt)
	if !entry.Degraded {
		t.Fatal("a response served from the degraded pool must stay marked degraded")
	}
	if entry.Provider != "deepseek" {
		t.Fatalf("provider: %q, want deepseek", entry.Provider)
	}
	if entry.ServedModel != "vmc/deepseek-v4p1-flash-1017-contributor-fallbacks" {
		t.Fatalf("served model: %q", entry.ServedModel)
	}
}

func TestDegradedLogFilter(t *testing.T) {
	s := observabilityService(t, "")
	rows := []LogEntry{
		{ID: "main", Status: 200, ServedModel: "deepseek/deepseek-v4.1-flash-1017", Degraded: false},
		{ID: "degraded", Status: 200, ServedModel: "vmc/deepseek-v4p1-flash-1017-contributor-fallbacks", Degraded: true},
	}
	start := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for i, row := range rows {
		row.Time = start.Add(time.Duration(i) * time.Second)
		row.UpstreamAttempts = observabilityAttempts(1)
		s.appendLog(row)
	}
	cases := []struct {
		filter string
		want   int
	}{
		{"yes", 1},
		{"no", 1},
		{"all", 2},
		{"", 2},
	}
	for _, tc := range cases {
		view := networkLogs(t, s, url.Values{"degraded": {tc.filter}})
		if view.Total != tc.want {
			t.Fatalf("degraded filter %q: total %d, want %d", tc.filter, view.Total, tc.want)
		}
	}
	only := networkLogs(t, s, url.Values{"degraded": {"yes"}})
	if len(only.Items) != 1 || only.Items[0].ID != "degraded" {
		t.Fatalf("degraded filter returned the wrong row: %+v", only.Items)
	}
}
