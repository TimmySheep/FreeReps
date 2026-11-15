package storage

import (
	"math"
	"testing"
	"time"

	"github.com/claude/freereps/internal/models"
)

// sample builds a sleep-analysis category sample the way the iOS app sends it:
// the stage in value_label, "Asleep Core" rather than "Core".
func sample(source, label string, start, end time.Time) models.CategorySampleRow {
	l := label
	return models.CategorySampleRow{
		UserID:     1,
		Type:       SleepAnalysisCategoryType,
		ValueLabel: &l,
		StartDate:  start,
		EndDate:    end,
		Source:     source,
	}
}

func at(day, hour, minute int) time.Time {
	return time.Date(2026, 11, day, hour, minute, 0, 0, time.UTC)
}

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.01 {
		t.Errorf("%s = %.3f, want %.3f", name, got, want)
	}
}

// TestDeriveSleepNightsSingleSource covers the ordinary night: one source, an
// In Bed sample over the whole night and the stages nested inside it.
func TestDeriveSleepNightsSingleSource(t *testing.T) {
	nights := DeriveSleepNights(1, []models.CategorySampleRow{
		sample("AutoSleep", "In Bed", at(10, 22, 30), at(11, 7, 30)),
		sample("AutoSleep", "Asleep Core", at(10, 22, 40), at(10, 23, 40)),
		sample("AutoSleep", "Asleep Deep", at(10, 23, 40), at(11, 1, 10)),
		sample("AutoSleep", "Asleep REM", at(11, 1, 10), at(11, 2, 10)),
		sample("AutoSleep", "Awake", at(11, 2, 10), at(11, 2, 25)),
		sample("AutoSleep", "Asleep Core", at(11, 2, 25), at(11, 7, 0)),
	}, []string{"AutoSleep"})

	if len(nights) != 1 {
		t.Fatalf("got %d nights, want 1", len(nights))
	}
	n := nights[0]
	near(t, "total_sleep", n.Session.TotalSleep, (1.0+4+35.0/60.0)+1.5+1.0) // core 22:40-23:40 + 02:25-07:00, deep, rem
	near(t, "core", n.Session.Core, 1.0+4+35.0/60.0)
	near(t, "deep", n.Session.Deep, 1.5)
	near(t, "rem", n.Session.REM, 1.0)
	near(t, "in_bed", n.Session.InBed, 9.0)
	near(t, "asleep", n.Session.Asleep, n.Session.TotalSleep)

	if !n.Session.SleepStart.Equal(at(10, 22, 40)) {
		t.Errorf("sleep_start = %s, want %s (the first asleep stage, not In Bed)", n.Session.SleepStart, at(10, 22, 40))
	}
	if !n.Session.SleepEnd.Equal(at(11, 7, 0)) {
		t.Errorf("sleep_end = %s, want %s", n.Session.SleepEnd, at(11, 7, 0))
	}
	if !n.Session.Date.Equal(at(11, 0, 0)) {
		t.Errorf("date = %s, want %s", n.Session.Date, at(11, 0, 0))
	}
	if len(n.Stages) != 6 {
		t.Errorf("got %d stages, want 6", len(n.Stages))
	}
	if n.Stages[0].Stage != models.SleepStageInBed {
		t.Errorf("first stage = %q, want %q", n.Stages[0].Stage, models.SleepStageInBed)
	}
	for _, s := range n.Stages {
		if s.Source != "AutoSleep" {
			t.Errorf("stage source = %q, want AutoSleep", s.Source)
		}
	}
}

// TestDeriveSleepNightsMultiSourceTakesOneSource pins the rule the 2026-03-26
// incident asks for: two sources describing one night are never added together.
func TestDeriveSleepNightsMultiSourceTakesOneSource(t *testing.T) {
	samples := []models.CategorySampleRow{
		// Oura's copy of the night, 8 hours asleep.
		sample("Oura", "In Bed", at(10, 23, 0), at(11, 7, 0)),
		sample("Oura", "Asleep Deep", at(10, 23, 0), at(11, 3, 0)),
		sample("Oura", "Asleep Core", at(11, 3, 0), at(11, 7, 0)),
		// Apple Health's copy of the same night, measured in fragments, 7.5 hours.
		sample("", "Asleep Deep", at(11, 0, 0), at(11, 3, 30)),
		sample("", "Asleep Core", at(11, 3, 30), at(11, 7, 30)),
	}

	ouraFirst := DeriveSleepNights(1, samples, []string{"Oura", ""})
	if len(ouraFirst) != 1 {
		t.Fatalf("got %d nights, want 1", len(ouraFirst))
	}
	near(t, "total_sleep with Oura first", ouraFirst[0].Session.TotalSleep, 8.0)
	if got := ouraFirst[0].Session.SleepEnd; !got.Equal(at(11, 7, 0)) {
		t.Errorf("sleep_end = %s, want Oura's %s", got, at(11, 7, 0))
	}

	appleFirst := DeriveSleepNights(1, samples, []string{"", "Oura"})
	if len(appleFirst) != 1 {
		t.Fatalf("got %d nights, want 1", len(appleFirst))
	}
	near(t, "total_sleep with Apple Health first", appleFirst[0].Session.TotalSleep, 7.5)

	// The 15.5 hours the two sources hold together must never surface.
	for _, night := range append(ouraFirst, appleFirst...) {
		if night.Session.TotalSleep > 9 {
			t.Errorf("total_sleep = %.2f h, which is two sources summed", night.Session.TotalSleep)
		}
	}
}

// TestDeriveSleepNightsInBedDoesNotCountAsSleep covers the nesting the data
// shows: the In Bed sample spans the whole night, so adding it to the stages
// would report more sleep than the person spent in bed.
func TestDeriveSleepNightsInBedDoesNotCountAsSleep(t *testing.T) {
	nights := DeriveSleepNights(1, []models.CategorySampleRow{
		sample("Pillow", "In Bed", at(10, 21, 0), at(11, 8, 0)), // 11 h in bed
		sample("Pillow", "Asleep Core", at(10, 21, 30), at(11, 1, 0)),
		sample("Pillow", "Asleep Deep", at(11, 1, 0), at(11, 2, 0)),
		sample("Pillow", "Asleep REM", at(11, 2, 0), at(11, 3, 0)),
		sample("Pillow", "Awake", at(11, 3, 0), at(11, 3, 30)),
	}, []string{"Pillow"})

	if len(nights) != 1 {
		t.Fatalf("got %d nights, want 1", len(nights))
	}
	near(t, "total_sleep", nights[0].Session.TotalSleep, 3.5+1.0+1.0) // In Bed is not sleep
	near(t, "in_bed", nights[0].Session.InBed, 11.0)
	if nights[0].Session.TotalSleep >= nights[0].Session.InBed {
		t.Errorf("total_sleep %.2f >= in_bed %.2f, so In Bed was summed into sleep",
			nights[0].Session.TotalSleep, nights[0].Session.InBed)
	}
	if !nights[0].Session.InBedStart.Equal(at(10, 21, 0)) {
		t.Errorf("in_bed_start = %s, want %s", nights[0].Session.InBedStart, at(10, 21, 0))
	}
}

// TestDeriveSleepNightsCrossMidnight covers a night whose stages straddle UTC
// midnight: one session, dated the UTC day it ended on, as the backfill dates
// it and as get_sleep_data queries it.
func TestDeriveSleepNightsCrossMidnight(t *testing.T) {
	nights := DeriveSleepNights(1, []models.CategorySampleRow{
		sample("AutoSleep", "Asleep Core", at(10, 23, 30), at(11, 1, 0)),
		sample("AutoSleep", "Asleep Rem", at(11, 1, 0), at(11, 3, 0)), // label as some writers spell it
		sample("AutoSleep", "Asleep Deep", at(11, 3, 0), at(11, 6, 30)),
	}, []string{"AutoSleep"})

	if len(nights) != 1 {
		t.Fatalf("got %d nights, want 1 session for a night spanning midnight", len(nights))
	}
	near(t, "total_sleep", nights[0].Session.TotalSleep, 6.5+0.5) // 23:30-01:00, 01:00-03:00, 03:00-06:30
	if !nights[0].Session.Date.Equal(at(11, 0, 0)) {
		t.Errorf("date = %s, want %s (UTC day of sleep_end)", nights[0].Session.Date, at(11, 0, 0))
	}
	if !nights[0].Session.SleepStart.Equal(at(10, 23, 30)) {
		t.Errorf("sleep_start = %s, want %s", nights[0].Session.SleepStart, at(10, 23, 30))
	}
}

// TestDeriveSleepNightsEmpty covers a range with no samples at all, which is
// what every range returned before this change.
func TestDeriveSleepNightsEmpty(t *testing.T) {
	if nights := DeriveSleepNights(1, nil, []string{"Oura"}); len(nights) != 0 {
		t.Fatalf("got %d nights from no samples, want 0", len(nights))
	}

	// Only unknown labels: nothing says how much of it was sleep.
	unknown := sample("Oura", "Something Else", at(10, 22, 0), at(11, 6, 0))
	if nights := DeriveSleepNights(1, []models.CategorySampleRow{unknown}, []string{"Oura"}); len(nights) != 0 {
		t.Fatalf("got %d nights from an unrecognized label, want 0", len(nights))
	}
}

// TestSleepStageLabelMapping pins the labels the stored table uses against the
// ones HealthKit sends.
func TestSleepStageLabelMapping(t *testing.T) {
	tests := []struct {
		label string
		want  string
	}{
		{"In Bed", models.SleepStageInBed},
		{"Awake", models.SleepStageAwake},
		{"Asleep Core", models.SleepStageCore},
		{"Asleep Deep", models.SleepStageDeep},
		{"Asleep REM", models.SleepStageREM},
		{"Asleep Unspecified", models.SleepStageAsleep},
		{"Asleep", models.SleepStageAsleep},
		{"Core", models.SleepStageCore},
		{"REM", models.SleepStageREM},
		{"Im Bett", models.SleepStageInBed}, // localized labels still resolve
	}

	for _, tt := range tests {
		s := sample("", tt.label, at(10, 22, 0), at(11, 6, 0))
		got, ok := canonicalSleepStage(s)
		if !ok {
			t.Errorf("canonicalSleepStage(%q) not recognized", tt.label)
			continue
		}
		if got != tt.want {
			t.Errorf("canonicalSleepStage(%q) = %q, want %q", tt.label, got, tt.want)
		}
	}

	// A sample without a label falls back to the HKCategoryValueSleepAnalysis
	// value.
	noLabel := models.CategorySampleRow{Value: 4, StartDate: at(10, 22, 0), EndDate: at(11, 6, 0)}
	if got, ok := canonicalSleepStage(noLabel); !ok || got != models.SleepStageDeep {
		t.Errorf("canonicalSleepStage(value 4) = %q (ok=%v), want Deep", got, ok)
	}
}

// TestGroupSleepSegmentsSplitsNights verifies the 12-hour rule separates an
// evening nap from the following night.
func TestGroupSleepSegmentsSplitsNights(t *testing.T) {
	segs := []SleepSegment{
		{Start: at(10, 3, 0), End: at(10, 5, 0), Stage: models.SleepStageCore},
		{Start: at(10, 22, 30), End: at(11, 6, 0), Stage: models.SleepStageCore},
	}
	groups := groupSleepSegments(segs)
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}

	nights := DeriveSleepNights(1, []models.CategorySampleRow{
		sample("AutoSleep", "Asleep Core", at(10, 3, 0), at(10, 5, 0)),
		sample("AutoSleep", "Asleep Core", at(10, 22, 30), at(11, 6, 0)),
	}, []string{"AutoSleep"})
	if len(nights) != 2 {
		t.Fatalf("got %d nights, want 2", len(nights))
	}
}

// TestDeriveSleepNightsSplitsJoinedNights covers two nights whose gap is
// shorter than sleepNightGap: the group spans more than a day, so it is split at
// its widest internal gap rather than reported as one night of nearly 20 hours —
// the shape INCIDENTS.md records for 2026-03-26.
func TestDeriveSleepNightsSplitsJoinedNights(t *testing.T) {
	nights := DeriveSleepNights(1, []models.CategorySampleRow{
		sample("AutoSleep", "In Bed", at(11, 19, 0), at(12, 8, 20)),
		sample("AutoSleep", "Asleep Core", at(11, 19, 20), at(11, 23, 0)),
		sample("AutoSleep", "Asleep Deep", at(11, 23, 0), at(12, 0, 10)),
		sample("AutoSleep", "Asleep Core", at(12, 4, 46), at(12, 8, 20)),
		sample("AutoSleep", "In Bed", at(12, 18, 22), at(13, 3, 31)),
		sample("AutoSleep", "Asleep Core", at(12, 18, 40), at(12, 22, 0)),
		sample("AutoSleep", "Asleep REM", at(12, 22, 0), at(13, 3, 31)),
	}, []string{"AutoSleep"})

	if len(nights) != 2 {
		t.Fatalf("got %d nights, want 2 (the two nights joined by a short gap)", len(nights))
	}
	if !nights[0].Session.Date.Equal(at(12, 0, 0)) {
		t.Errorf("first date = %s, want %s", nights[0].Session.Date, at(12, 0, 0))
	}
	if !nights[1].Session.Date.Equal(at(13, 0, 0)) {
		t.Errorf("second date = %s, want %s", nights[1].Session.Date, at(13, 0, 0))
	}
	for _, n := range nights {
		if n.Session.TotalSleep > 12 {
			t.Errorf("night of %s reports %.2f h of sleep, which is two nights",
				n.Session.Date.Format("2006-01-02"), n.Session.TotalSleep)
		}
	}
}

// TestDeriveSleepNightsKeepsLongNightWhole is the other half of that rule: a
// night that genuinely runs past twelve hours, with a long waking stretch in the
// middle, stays one session.
func TestDeriveSleepNightsKeepsLongNightWhole(t *testing.T) {
	nights := DeriveSleepNights(1, []models.CategorySampleRow{
		sample("AutoSleep", "In Bed", at(11, 19, 0), at(12, 8, 20)),
		sample("AutoSleep", "Asleep Core", at(11, 19, 20), at(11, 23, 0)),
		sample("AutoSleep", "Asleep Deep", at(11, 23, 0), at(12, 0, 10)),
		sample("AutoSleep", "Awake", at(12, 0, 10), at(12, 4, 46)),
		sample("AutoSleep", "Asleep Core", at(12, 4, 46), at(12, 8, 20)),
	}, []string{"AutoSleep"})

	if len(nights) != 1 {
		t.Fatalf("got %d nights, want 1", len(nights))
	}
	near(t, "total_sleep", nights[0].Session.TotalSleep, (3+40.0/60.0)+(1+10.0/60.0)+(3+34.0/60.0))
	near(t, "in_bed", nights[0].Session.InBed, 13+20.0/60.0)
}

// TestBuildSleepNightCapsInBedAtItsInterval covers overlapping In Bed samples of
// one source: summing them would report more time in bed than they cover.
func TestBuildSleepNightCapsInBedAtItsInterval(t *testing.T) {
	segs := []SleepSegment{
		{Start: at(10, 22, 0), End: at(11, 6, 0), Stage: models.SleepStageInBed, Source: "AutoSleep"},
		{Start: at(10, 22, 30), End: at(11, 5, 30), Stage: models.SleepStageInBed, Source: "AutoSleep"},
		{Start: at(10, 22, 30), End: at(11, 4, 0), Stage: models.SleepStageCore, Source: "AutoSleep"},
	}
	session, _, ok := BuildSleepNight(1, segs)
	if !ok {
		t.Fatal("BuildSleepNight reported no night")
	}
	near(t, "in_bed", session.InBed, 8.0)
	near(t, "total_sleep", session.TotalSleep, 5.5)
}

// TestMergeSleepDataKeepsStoredSession verifies compatibility with the stored
// path: where a source already wrote a session for a date, that row and its
// stages are what the caller gets, and the derived night for the same date is
// dropped.
func TestMergeSleepDataKeepsStoredSession(t *testing.T) {
	stored := []SleepSessionResult{{
		SleepSessionRow: models.SleepSessionRow{
			UserID:     1,
			Date:       at(11, 0, 0),
			TotalSleep: 7.25,
			SleepStart: at(10, 23, 0),
			SleepEnd:   at(11, 6, 15),
		},
		ID: 42,
	}}
	storedStages := []models.SleepStageRow{{
		StartTime: at(10, 23, 0), EndTime: at(11, 6, 15), Stage: models.SleepStageCore,
	}}

	derived := DeriveSleepNights(1, []models.CategorySampleRow{
		// A second, derived view of the same date.
		sample("Oura", "Asleep Core", at(10, 23, 10), at(11, 6, 40)),
		// A night with no stored session.
		sample("Oura", "Asleep Deep", at(9, 23, 0), at(10, 6, 0)),
	}, []string{"Oura"})
	if len(derived) != 2 {
		t.Fatalf("got %d derived nights, want 2", len(derived))
	}

	sessions, stages := mergeSleepData(stored, storedStages, derived)

	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2 (the stored one plus one derived)", len(sessions))
	}
	// Newest first.
	if !sessions[0].Date.Equal(at(11, 0, 0)) {
		t.Fatalf("first session date = %s, want %s", sessions[0].Date, at(11, 0, 0))
	}
	if sessions[0].ID != 42 || sessions[0].Derived {
		t.Errorf("stored session was replaced: ID=%d derived=%v", sessions[0].ID, sessions[0].Derived)
	}
	near(t, "stored total_sleep", sessions[0].TotalSleep, 7.25)
	if sessions[1].Derived != true {
		t.Error("the night without a stored session is not marked derived")
	}
	if !sessions[1].Date.Equal(at(10, 0, 0)) {
		t.Errorf("second session date = %s, want %s", sessions[1].Date, at(10, 0, 0))
	}

	// The derived stages of the replaced date must not be appended either.
	for _, s := range stages {
		if s.StartTime.Equal(at(10, 23, 10)) {
			t.Error("stages of the dropped derived night were returned")
		}
	}
	if len(stages) != 2 {
		t.Errorf("got %d stages, want 2", len(stages))
	}
	if !stages[0].StartTime.Before(stages[1].StartTime) {
		t.Error("stages are not sorted by start time")
	}
}

// TestSummarizeSleepSessions checks the per-bucket averages against the figures
// the SQL aggregation produced for the same rows.
func TestSummarizeSleepSessions(t *testing.T) {
	sessions := []SleepSessionResult{
		{ // 2026-11-02, a Monday
			SleepSessionRow: models.SleepSessionRow{
				Date: at(2, 0, 0), TotalSleep: 8, Core: 4, Deep: 1.6, REM: 1.6, InBed: 8.5,
				SleepStart: at(1, 23, 0), SleepEnd: at(2, 7, 30),
			},
		},
		{ // 2026-11-03
			SleepSessionRow: models.SleepSessionRow{
				Date: at(3, 0, 0), TotalSleep: 6, Core: 3, Deep: 1.2, REM: 0.6, InBed: 7.5,
				SleepStart: at(2, 22, 0), SleepEnd: at(3, 5, 30),
			},
		},
		{ // 2026-11-09, the following Monday
			SleepSessionRow: models.SleepSessionRow{
				Date: at(9, 0, 0), TotalSleep: 7, Core: 3.5, Deep: 1.4, REM: 1.4, InBed: 7,
				SleepStart: at(8, 23, 0), SleepEnd: at(9, 6, 0),
			},
		},
	}

	weekly := SummarizeSleepSessions(sessions, "1 week")
	if len(weekly) != 2 {
		t.Fatalf("got %d weekly periods, want 2", len(weekly))
	}
	// Newest first, as the SQL ORDER BY period DESC returned them.
	if weekly[0].Period != "2026-11-09" || weekly[1].Period != "2026-11-02" {
		t.Fatalf("periods = %q, %q, want 2026-11-09 then 2026-11-02", weekly[0].Period, weekly[1].Period)
	}
	if weekly[1].Nights != 2 {
		t.Errorf("nights = %d, want 2", weekly[1].Nights)
	}
	near(t, "avg_total_sleep_hr", weekly[1].AvgTotalSleepHr, 7.0)
	near(t, "avg_in_bed_hr", weekly[1].AvgInBedHr, 8.0)
	// (8/8.5*100 + 6/7.5*100) / 2 = (94.1176 + 80) / 2
	near(t, "avg_efficiency_pct", weekly[1].AvgEfficiencyPct, (8.0/8.5*100+6.0/7.5*100)/2)
	near(t, "avg_deep_pct", weekly[1].AvgDeepPct, (1.6/8*100+1.2/6*100)/2)
	near(t, "avg_rem_pct", weekly[1].AvgREMPct, (1.6/8*100+0.6/6*100)/2)
	if weekly[1].AvgBedtime != "22:30" {
		t.Errorf("avg_bedtime = %q, want 22:30", weekly[1].AvgBedtime)
	}

	monthly := SummarizeSleepSessions(sessions, "1 month")
	if len(monthly) != 1 {
		t.Fatalf("got %d monthly periods, want 1", len(monthly))
	}
	if monthly[0].Period != "2026-11-01" || monthly[0].Nights != 3 {
		t.Errorf("period = %q with %d nights, want 2026-11-01 with 3", monthly[0].Period, monthly[0].Nights)
	}
	near(t, "monthly avg_total_sleep_hr", monthly[0].AvgTotalSleepHr, 7.0)

	// An empty range produces no periods rather than a zero row.
	if got := SummarizeSleepSessions(nil, "1 week"); len(got) != 0 {
		t.Errorf("got %d periods from no sessions, want 0", len(got))
	}
}

// TestBucketStart pins the truncation to what date_trunc produced: weeks on
// Monday, months on the first, in UTC.
func TestBucketStart(t *testing.T) {
	tests := []struct {
		date   time.Time
		bucket string
		want   time.Time
	}{
		{at(2, 0, 0), "1 week", time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}, // Monday
		{at(8, 0, 0), "1 week", time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}, // Sunday ends that week
		{at(9, 0, 0), "1 week", time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)}, // next Monday
		{at(15, 0, 0), "1 month", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		{at(15, 0, 0), "anything else", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		if got := bucketStart(tt.date, tt.bucket); !got.Equal(tt.want) {
			t.Errorf("bucketStart(%s, %q) = %s, want %s", tt.date, tt.bucket, got, tt.want)
		}
	}
}

// TestSourceRank pins the priority matching against sourcePriorityCaseSQL:
// prefix for named sources, exact for the empty source, unlisted last.
func TestSourceRank(t *testing.T) {
	priorities := []string{"Withings", "Oura", ""}
	tests := []struct {
		source string
		want   int
	}{
		{"Withings", 0},
		{"Oura", 1},
		{"", 2},
		{"AutoSleep", 3},
	}
	for _, tt := range tests {
		if got := sourceRank(tt.source, priorities); got != tt.want {
			t.Errorf("sourceRank(%q) = %d, want %d", tt.source, got, tt.want)
		}
	}

	// Prefix matching, as the SQL LIKE '%' branch does.
	if got := sourceRank("Apple Watch Series 9", []string{"Apple Watch"}); got != 0 {
		t.Errorf("sourceRank(Apple Watch Series 9) = %d, want 0", got)
	}
	// No configured list: every source ties, and the name decides.
	if got := sourceRank("Oura", nil); got != 0 {
		t.Errorf("sourceRank with no priorities = %d, want 0", got)
	}
}
