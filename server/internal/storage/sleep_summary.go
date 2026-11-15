package storage

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"
)

// SleepSummaryPeriod holds aggregated sleep stats for one time period.
type SleepSummaryPeriod struct {
	Period                   string  `json:"period"`
	Nights                   int     `json:"nights"`
	AvgTotalSleepHr          float64 `json:"avg_total_sleep_hr"`
	AvgDeepHr                float64 `json:"avg_deep_hr"`
	AvgREMHr                 float64 `json:"avg_rem_hr"`
	AvgCoreHr                float64 `json:"avg_core_hr"`
	AvgInBedHr               float64 `json:"avg_in_bed_hr"`
	AvgEfficiencyPct         float64 `json:"avg_efficiency_pct"`
	AvgDeepPct               float64 `json:"avg_deep_pct"`
	AvgREMPct                float64 `json:"avg_rem_pct"`
	AvgBedtime               string  `json:"avg_bedtime"`
	AvgWaketime              string  `json:"avg_waketime"`
	BedtimeConsistencyStdHr  float64 `json:"bedtime_consistency_stddev_hr"`
	WaketimeConsistencyStdHr float64 `json:"waketime_consistency_stddev_hr"`
}

// GetSleepSummary returns aggregated sleep stats per period with circular
// bedtime/waketime averages.
//
// The sessions come from QuerySleepData, so a period also holds the nights
// derived from sleep category samples where no source wrote a session row. The
// aggregation runs here rather than in SQL because those derived nights are not
// rows in sleep_sessions; it is the same set of averages the two SQL queries
// computed over the table alone.
func (db *DB) GetSleepSummary(ctx context.Context, start, end time.Time, bucket string, userID int) ([]SleepSummaryPeriod, error) {
	sessions, _, err := db.QuerySleepData(ctx, start, end, userID)
	if err != nil {
		return nil, fmt.Errorf("querying sleep summary: %w", err)
	}
	return SummarizeSleepSessions(sessions, bucket), nil
}

// SummarizeSleepSessions aggregates sessions into one SleepSummaryPeriod per
// bucket, newest bucket first.
func SummarizeSleepSessions(sessions []SleepSessionResult, bucket string) []SleepSummaryPeriod {
	type accumulator struct {
		period SleepSummaryPeriod
		total  float64
		deep   float64
		rem    float64
		core   float64
		inBed  float64
		// Percentages are averaged per night, and a night without time in bed
		// contributes 0 rather than being left out, which is how the SQL
		// AVG(CASE WHEN in_bed > 0 ...) read it.
		efficiency float64
		deepPct    float64
		remPct     float64
		bedtimes   []float64
		waketimes  []float64
	}

	byPeriod := make(map[string]*accumulator)
	var order []string

	for _, s := range sessions {
		key := bucketStart(s.Date, bucket).Format("2006-01-02")
		acc, ok := byPeriod[key]
		if !ok {
			acc = &accumulator{period: SleepSummaryPeriod{Period: key}}
			byPeriod[key] = acc
			order = append(order, key)
		}

		acc.period.Nights++
		acc.total += s.TotalSleep
		acc.deep += s.Deep
		acc.rem += s.REM
		acc.core += s.Core
		acc.inBed += s.InBed
		if s.InBed > 0 {
			acc.efficiency += s.TotalSleep / s.InBed * 100
		}
		if s.TotalSleep > 0 {
			acc.deepPct += s.Deep / s.TotalSleep * 100
			acc.remPct += s.REM / s.TotalSleep * 100
		}
		if !s.SleepStart.IsZero() {
			acc.bedtimes = append(acc.bedtimes, timeToHourOfDay(s.SleepStart))
		}
		if !s.SleepEnd.IsZero() {
			acc.waketimes = append(acc.waketimes, timeToHourOfDay(s.SleepEnd))
		}
	}

	sort.Sort(sort.Reverse(sort.StringSlice(order)))

	result := make([]SleepSummaryPeriod, 0, len(order))
	for _, key := range order {
		acc := byPeriod[key]
		nights := float64(acc.period.Nights)

		acc.period.AvgTotalSleepHr = acc.total / nights
		acc.period.AvgDeepHr = acc.deep / nights
		acc.period.AvgREMHr = acc.rem / nights
		acc.period.AvgCoreHr = acc.core / nights
		acc.period.AvgInBedHr = acc.inBed / nights
		acc.period.AvgEfficiencyPct = acc.efficiency / nights
		acc.period.AvgDeepPct = acc.deepPct / nights
		acc.period.AvgREMPct = acc.remPct / nights

		avgBed, stdBed := circularMeanStd(acc.bedtimes)
		avgWake, stdWake := circularMeanStd(acc.waketimes)
		acc.period.AvgBedtime = hoursToHHMM(avgBed)
		acc.period.AvgWaketime = hoursToHHMM(avgWake)
		acc.period.BedtimeConsistencyStdHr = math.Round(stdBed*100) / 100
		acc.period.WaketimeConsistencyStdHr = math.Round(stdWake*100) / 100

		result = append(result, acc.period)
	}
	return result
}

// bucketStart truncates a session date to the bucket that holds it, matching
// what date_trunc returned for the same rows: weeks start on Monday, months on
// the first, and an unknown bucket reads as a month.
func bucketStart(date time.Time, bucket string) time.Time {
	day := date.UTC().Truncate(24 * time.Hour)
	if truncInterval(bucket) == "week" {
		// Monday-based, as date_trunc('week') is: Sunday is 7 days into its own
		// week rather than 6 before it.
		offset := (int(day.Weekday()) + 6) % 7
		return day.AddDate(0, 0, -offset)
	}
	return time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// timeToHourOfDay extracts fractional hour of day from a time.Time.
func timeToHourOfDay(t time.Time) float64 {
	return float64(t.Hour()) + float64(t.Minute())/60.0 + float64(t.Second())/3600.0
}

// circularMeanStd computes the circular mean and standard deviation for times
// expressed as hours (0–24). This handles the midnight wrap correctly
// (e.g., 23:00 and 01:00 average to 00:00, not 12:00).
func circularMeanStd(hours []float64) (mean, std float64) {
	if len(hours) == 0 {
		return 0, 0
	}

	var sinSum, cosSum float64
	for _, h := range hours {
		rad := h / 24.0 * 2 * math.Pi
		sinSum += math.Sin(rad)
		cosSum += math.Cos(rad)
	}

	n := float64(len(hours))
	sinAvg := sinSum / n
	cosAvg := cosSum / n

	// Circular mean
	meanRad := math.Atan2(sinAvg, cosAvg)
	if meanRad < 0 {
		meanRad += 2 * math.Pi
	}
	mean = meanRad / (2 * math.Pi) * 24.0

	// Circular standard deviation
	r := math.Sqrt(sinAvg*sinAvg + cosAvg*cosAvg)
	if r > 1 {
		r = 1
	}
	// Circular variance = 1 - R, std = sqrt(-2 * ln(R)) converted to hours
	if r > 0 {
		std = math.Sqrt(-2*math.Log(r)) / (2 * math.Pi) * 24.0
	}

	return mean, std
}

// hoursToHHMM formats fractional hours (0–24) as "HH:MM".
func hoursToHHMM(h float64) string {
	h = math.Mod(h, 24)
	if h < 0 {
		h += 24
	}
	hours := int(h)
	minutes := int(math.Round((h - float64(hours)) * 60))
	if minutes == 60 {
		hours++
		minutes = 0
	}
	if hours >= 24 {
		hours -= 24
	}
	return fmt.Sprintf("%02d:%02d", hours, minutes)
}
