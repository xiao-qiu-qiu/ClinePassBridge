package bridge

import "time"

// Cline's pricing page references DeepSeek's time-of-use rules:
// https://api-docs.deepseek.com/quick_start/pricing/
// Peak: Mon-Fri 01:00-04:00 and 06:00-10:00 UTC, except Chinese public holidays.
// Off-peak is half price; weekends remain off-peak on make-up working days.
func deepseekReferenceCost(e LogEntry, peakCost float64) priceRange {
	rangeCost := priceRange{peakCost / 2, peakCost}
	if e.Time.IsZero() || e.DurationMS < 0 || e.DurationMS > int64((24*time.Hour)/time.Millisecond) {
		return rangeCost
	}
	start := e.Time.UTC()
	end := start.Add(time.Duration(e.DurationMS) * time.Millisecond)
	if e.CompletedAt != nil {
		end = e.CompletedAt.UTC()
	}
	if end.Before(start) || end.Sub(start) > 24*time.Hour {
		return rangeCost
	}
	peak, known := deepseekPeakAt(start)
	if !known {
		return rangeCost
	}
	// Check every intervening hour, including an end exactly on a boundary.
	// Equal start/end rates alone would miss a whole peak window in between.
	for at := start.Truncate(time.Hour).Add(time.Hour); !at.After(end); at = at.Add(time.Hour) {
		nextPeak, nextKnown := deepseekPeakAt(at)
		if !nextKnown || nextPeak != peak {
			return rangeCost
		}
	}
	if peak {
		return priceRange{peakCost, peakCost}
	}
	return priceRange{peakCost / 2, peakCost / 2}
}

func deepseekPeakAt(at time.Time) (peak, known bool) {
	at = at.UTC()
	hour := at.Hour()
	if at.Weekday() == time.Saturday || at.Weekday() == time.Sunday ||
		!(hour >= 1 && hour < 4 || hour >= 6 && hour < 10) {
		return false, true
	}
	// Peak hours fall on the same date in UTC and Beijing (UTC+8).
	// Unknown holiday calendars only affect otherwise possible peak hours.
	// Estimate holidays using the State Council's 2026 time-off ranges:
	// https://www.gov.cn/zhengce/content/202511/content_7047090.htm
	// The supplier's internal holiday calendar has not been published.
	if at.Year() != 2026 {
		return false, false
	}
	day := int(at.Month())*100 + at.Day()
	holiday := day >= 101 && day <= 103 ||
		day >= 215 && day <= 223 ||
		day >= 404 && day <= 406 ||
		day >= 501 && day <= 505 ||
		day >= 619 && day <= 621 ||
		day >= 925 && day <= 927 ||
		day >= 1001 && day <= 1007
	return !holiday, true
}
