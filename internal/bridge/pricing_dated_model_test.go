package bridge

import "testing"

func TestDatedFlashReferenceCost(t *testing.T) {
	for _, model := range []string{"cline-pass/deepseek-v4.1-flash", "cline-pass/deepseek-v4.1-flash-1017"} {
		cost, ok := referenceCost(estimatePricedEntry(model, 1000000, 20000, 800000, 0, false))
		if !ok {
			t.Fatalf("supported model excluded: %s", model)
		}
		estimateCloseTo(t, cost.Low, .0888, "reference cost low")
		estimateCloseTo(t, cost.High, .0888, "reference cost high")
	}
	for _, model := range []string{"cline-pass/deepseek-v4.1-flash-1018", "deepseek-v4.1-flash-1017", "cline-pass/deepseek-v4.1-flash-1017-custom"} {
		if _, ok := referenceCost(estimatePricedEntry(model, 1000000, 20000, 800000, 0, false)); ok {
			t.Fatalf("unknown model priced: %s", model)
		}
	}
}

func TestDatedFlashEstimateLedger(t *testing.T) {
	s := estimateRegister(t, t.TempDir())
	t.Cleanup(func() { observabilityShutdown(t, s) })
	c := Credential{ID: "dated-flash-test", Type: Provider, APIKey: "test-only"}
	estimateAddCredential(t, s, c)
	estimateRecordUsage(t, s, c, "cline-pass/deepseek-v4.1-flash-1017", 1000000, 20000, 800000, 0)
	k := estimateKeySnapshot(t, s, estimateKeyID(c))
	if k.Totals.Requests != 1 || k.Totals.Tokens != 1020000 || k.Totals.Unknown != 0 {
		t.Fatalf("dated model not recorded as priced: %+v", k.Totals)
	}
	estimateCloseTo(t, k.Totals.Cost.Low, .0888, "ledger cost")
}
