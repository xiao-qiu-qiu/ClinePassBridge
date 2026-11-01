package bridge

import (
	"testing"
	"time"
)

func TestPricingDeepSeekSchedule(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   string
		low  float64
		high float64
	}{
		{"before morning peak", "2026-11-02T08:59:59+08:00", .15, .15},
		{"morning peak starts", "2026-11-02T09:00:00+08:00", .30, .30},
		{"morning peak ends", "2026-11-02T12:00:00+08:00", .15, .15},
		{"afternoon peak starts", "2026-11-02T14:00:00+08:00", .30, .30},
		{"afternoon peak ends", "2026-11-02T18:00:00+08:00", .15, .15},
		{"UTC uses same rule", "2026-11-02T02:00:00Z", .30, .30},
		{"weekend", "2026-11-01T10:00:00+08:00", .15, .15},
		{"make-up workday remains weekend", "2026-10-10T10:00:00+08:00", .15, .15},
		{"New Year holiday", "2026-01-02T10:00:00+08:00", .15, .15},
		{"Spring Festival holiday", "2026-02-20T10:00:00+08:00", .15, .15},
		{"Qingming holiday", "2026-04-06T10:00:00+08:00", .15, .15},
		{"Labor Day holiday", "2026-05-05T10:00:00+08:00", .15, .15},
		{"Dragon Boat holiday", "2026-06-19T10:00:00+08:00", .15, .15},
		{"Mid-Autumn holiday", "2026-09-25T10:00:00+08:00", .15, .15},
		{"National Day holiday", "2026-10-07T10:00:00+08:00", .15, .15},
		{"first weekday after holiday", "2026-10-08T10:00:00+08:00", .30, .30},
		{"unknown holiday year", "2027-11-01T10:00:00+08:00", .15, .30},
		{"unknown year at night", "2027-11-01T22:00:00+08:00", .15, .15},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, tc.at)
			if err != nil {
				t.Fatal(err)
			}
			entry := estimatePricedEntry("cline-pass/deepseek-v4.1-flash-1017", 1_000_000, 0, 0, 0, false)
			entry.Time = at
			got, ok := referenceCost(entry)
			if !ok {
				t.Fatal("DeepSeek request not priced")
			}
			estimateCloseTo(t, got.Low, tc.low, "scheduled low")
			estimateCloseTo(t, got.High, tc.high, "scheduled high")
		})
	}
}

func TestPricingDeepSeekCrossingWindows(t *testing.T) {
	for _, tc := range []struct {
		name     string
		at       string
		duration time.Duration
		endAt    string
	}{
		{"cross peak start", "2026-11-02T08:59:59+08:00", 2 * time.Second, ""},
		{"cross peak end", "2026-11-02T11:59:59+08:00", 2 * time.Second, ""},
		{"end exactly on boundary", "2026-11-02T11:59:59+08:00", time.Second, ""},
		{"peak window between off-peak endpoints", "2026-11-02T08:00:00+08:00", 5 * time.Hour, ""},
		{"completed timestamp takes precedence", "2026-11-02T08:59:59+08:00", 0, "2026-11-02T09:00:01+08:00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, tc.at)
			if err != nil {
				t.Fatal(err)
			}
			entry := LogEntry{Time: at, DurationMS: tc.duration.Milliseconds()}
			if tc.endAt != "" {
				end, err := time.Parse(time.RFC3339, tc.endAt)
				if err != nil {
					t.Fatal(err)
				}
				entry.CompletedAt = &end
			}
			got := deepseekReferenceCost(entry, 1.32)
			if got.Low != .66 || got.High != 1.32 {
				t.Fatalf("crossing request lost price range: %+v", got)
			}
		})
	}
}
