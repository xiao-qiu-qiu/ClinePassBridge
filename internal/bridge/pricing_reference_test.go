package bridge

import "testing"

func TestPricingCurrentReferenceModels(t *testing.T) {
	// Mixed input, cache read/write and output exercise every published rate.
	// No request time means DeepSeek must retain both peak and off-peak costs.
	for _, tc := range []struct {
		model     string
		write     int64
		low, high float64
	}{
		{"deepseek-v4.1-flash-1017", 0, .01266, .02532},
		{"deepseek-v4.1-flash", 0, .01266, .02532},
		{"deepseek-v4-pro", 0, .05522, .11044},
		{"glm-5.3-flash", 0, .0131, .0131},
		{"glm-5.3", 0, .1216, .1216},
		{"kimi-k3.1-pro", 10000, .174, .194},
		{"kimi-k3", 10000, .261, .291},
		{"muse-spark-1.3-contributor", 0, .00824, .00824},
		{"minimax-m3.1-flash", 0, .0129, .0129},
		{"minimax-m3", 0, .0264, .0264},
		{"mimo-v2.6-pro", 0, .035742, .035742},
		{"mimo-v2.6-flash", 0, .011536, .011536},
		{"qwen3.7-max", 10000, .22375, .22375},
		{"qwen3.7-plus", 10000, .0354, .0354},
		{"qwen3.8-max", 10000, .1744, .1744},
		// Legacy MiMo IDs retain their last known rates independently of V2.6.
		{"mimo-v2.5", 0, .011536, .011536},
		{"mimo-v2.5-pro", 0, .14297, .14297},
	} {
		t.Run(tc.model, func(t *testing.T) {
			entry := estimatePricedEntry("cline-pass/"+tc.model, 100000, 1000, 20000, tc.write, tc.write > 0)
			got, ok := referenceCost(entry)
			if !ok {
				t.Fatal("listed model was excluded from pricing")
			}
			estimateCloseTo(t, got.Low, tc.low, "reference low")
			estimateCloseTo(t, got.High, tc.high, "reference high")
		})
	}
}

func TestPricingKimiWriteRequiresExplicitUsage(t *testing.T) {
	for _, tc := range []struct {
		model string
		cost  float64
	}{
		{"kimi-k3.1-pro", .174},
		{"kimi-k3", .261},
	} {
		t.Run(tc.model, func(t *testing.T) {
			for _, write := range []int64{0, 10000} {
				entry := estimatePricedEntry("cline-pass/"+tc.model, 100000, 1000, 20000, write, false)
				if _, ok := referenceCost(entry); ok {
					t.Fatalf("unreported write count was accepted: %d", write)
				}
			}
			entry := estimatePricedEntry("cline-pass/"+tc.model, 100000, 1000, 20000, 0, true)
			got, ok := referenceCost(entry)
			if !ok {
				t.Fatal("explicitly reported zero writes were excluded")
			}
			// With no writes, unknown TTL must not widen input/read/output costs.
			estimateSingleValue(t, got, tc.cost, "zero-write cost")
		})
	}
}

func TestPricingMiniMaxHighContextThreshold(t *testing.T) {
	// The reference page says 512K without a numeric definition. Use K=1024,
	// consistently with this project's existing Qwen 256K tier conversion.
	for _, tc := range []struct {
		name           string
		prompt, cached int64
		cost           float64
	}{
		{"below", 524287, 500000, .0384861},
		{"at", 524288, 500000, .0384864},
		{"above", 524289, 500000, .0769734},
		{"fully-cached-above", 524289, 524289, .06531468},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := estimatePricedEntry("cline-pass/minimax-m3", tc.prompt, 1000, tc.cached, 0, false)
			got, ok := referenceCost(entry)
			if !ok {
				t.Fatal("valid MiniMax usage was excluded")
			}
			estimateSingleValue(t, got, tc.cost, "context tier cost")
		})
	}
}

func TestPricingUnknownModelDoesNotBorrowLegacyRates(t *testing.T) {
	for _, model := range []string{
		"cline-pass/mimo-v2.6", "cline-pass/mimo-v2.6-pro-latest",
		"cline-pass/mimo-v2.5-flash", "cline-pass/kimi-k3.1",
		"minimax-m3", "cline-pass/cline-pass/minimax-m3",
	} {
		t.Run(model, func(t *testing.T) {
			entry := estimatePricedEntry(model, 100000, 1000, 20000, 0, true)
			entry.Model = "cline-pass/mimo-v2.5"
			entry.ServedModel = "cline-pass/mimo-v2.5"
			if _, ok := referenceCost(entry); ok {
				t.Fatal("unknown upstream borrowed a legacy, client-alias or served-model price")
			}
		})
	}
}
