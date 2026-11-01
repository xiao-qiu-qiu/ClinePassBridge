package bridge

import "strings"

// USD per million tokens. Snapshot of Cline's reference prices, 2026-11-02.
// Deliberately match upstream IDs, never user-defined aliases or similar names.
const pricingRevision = "cline-2026-11-02"
const pricingSource = "https://docs.cline.bot/getting-started/clinepass#reference-pricing"

type priceRange struct {
	Low  float64 `json:"low"`
	High float64 `json:"high"`
}

type tokenPrice struct{ input, output, read, write float64 }

func (p tokenPrice) cost(e LogEntry) float64 {
	return (float64(e.PromptTokens-e.CachedTokens-e.CacheWriteTokens)*p.input + float64(e.CachedTokens)*p.read + float64(e.CacheWriteTokens)*p.write + float64(e.CompletionTokens)*p.output) / 1e6
}

func referenceCost(e LogEntry) (priceRange, bool) {
	for _, n := range []int64{e.PromptTokens, e.CompletionTokens, e.CachedTokens, e.CacheWriteTokens} {
		if n < 0 || n > 1_000_000_000_000 {
			return priceRange{}, false
		}
	}
	if !e.UsageReported || e.PromptTokens < 0 || e.CompletionTokens < 0 || e.CachedTokens < 0 || e.CacheWriteTokens < 0 || e.CachedTokens+e.CacheWriteTokens > e.PromptTokens {
		return priceRange{}, false
	}
	deepseekFlash := tokenPrice{.30, 1.20, .006, 0}
	prices := map[string]tokenPrice{
		"glm-5.3": {1.40, 4.40, .26, 0}, "glm-5.3-flash": {.15, .50, .03, 0},
		"kimi-k3.1-pro": {2, 10, .20, 2}, "kimi-k3": {3, 15, .30, 3},
		"deepseek-v4.1-flash":      deepseekFlash,
		"deepseek-v4.1-flash-1017": deepseekFlash,
		"mimo-v2.6-pro":            {.435, .87, .0036, 0}, "mimo-v2.6-flash": {.14, .28, .0028, 0},
		// Legacy IDs only: last known 2026-09-28 rates, absent from the current table.
		// Never use these as fallback prices for a newer MiMo model.
		"mimo-v2.5": {.14, .28, .0028, 0}, "mimo-v2.5-pro": {1.74, 3.48, .0145, 0},
		"minimax-m3.1-flash": {.15, .60, .015, 0},
		"minimax-m3":         {.30, 1.20, .06, 0}, "muse-spark-1.3-contributor": {.10, .20, .002, 0},
		"qwen3.8-max": {2, 6, .17, 2.50}, "qwen3.7-max": {2.50, 7.50, .50, 3.125},
		"qwen3.7-plus": {.40, 1.60, .04, .50}, "deepseek-v4-pro": {1.32, 3.96, .044, 0},
	}
	if !strings.HasPrefix(e.UpstreamModel, "cline-pass/") {
		return priceRange{}, false
	}
	model := strings.TrimPrefix(e.UpstreamModel, "cline-pass/")
	p, ok := prices[model]
	if !ok {
		return priceRange{}, false
	}
	// Cache writes must be explicitly reported on models with a separate write price.
	if p.write > 0 && !e.CacheWriteReported {
		return priceRange{}, false
	}
	if e.CacheWriteTokens > 0 && p.write == 0 {
		return priceRange{}, false
	}
	if model == "qwen3.7-plus" && e.PromptTokens > 262144 {
		p = tokenPrice{1.20, 4.80, .12, 1.50}
	}
	if model == "minimax-m3" && e.PromptTokens > 524288 {
		p = tokenPrice{.60, 2.40, .12, 0}
	}
	cost := p.cost(e)
	out := priceRange{cost, cost}
	switch model {
	case "deepseek-v4.1-flash", "deepseek-v4.1-flash-1017", "deepseek-v4-pro":
		out = deepseekReferenceCost(e, cost)
	case "kimi-k3.1-pro", "kimi-k3":
		// Usage reports total writes without a TTL breakdown. The 1h write rate
		// is twice the 5min rate; only the write component widens the range.
		p.write *= 2
		out.High = p.cost(e)
	}
	return out, true
}
