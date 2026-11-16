package health

import (
	"log/slog"
	"testing"
	"time"

	"github.com/claude/freereps/internal/models"
)

const (
	hkSleepAnalysis = "HKCategoryTypeIdentifierSleepAnalysis"
)

// sleepPriority is the list the deployed config inherits when a user configured
// no rule of their own: Withings and Oura first, then Apple Health as the empty
// name. Nobody in that deployment runs Withings or Oura.
func sleepPriority() []string { return []string{"Withings", "Oura", ""} }

func mustTime(t *testing.T, s string) models.HealthTime {
	t.Helper()
	parsed, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return models.HealthTime{Time: parsed}
}

// sample builds a sleep category sample the way the iOS app sends it: source
// "" for Apple Health, the app's display name for everything else.
func sample(t *testing.T, typ, label, source, start, end string) models.CategorySample {
	t.Helper()
	l := label
	return models.CategorySample{
		ID:         source + start,
		Type:       typ,
		Value:      2,
		ValueLabel: &l,
		StartDate:  mustTime(t, start),
		EndDate:    mustTime(t, end),
		Source:     source,
	}
}

// appleNight is a night of Apple Health stage rows: the shape a Watch writes,
// source empty, every stage its own sample.
func appleNight(t *testing.T) []models.CategorySample {
	t.Helper()
	return []models.CategorySample{
		sample(t, hkSleepAnalysis, "Asleep Core", "", "2026-11-11 23:10", "2026-11-12 00:30"),
		sample(t, hkSleepAnalysis, "Asleep Deep", "", "2026-11-12 00:30", "2026-11-12 01:45"),
		sample(t, hkSleepAnalysis, "Asleep REM", "", "2026-11-12 01:45", "2026-11-12 02:40"),
		sample(t, hkSleepAnalysis, "Asleep Core", "", "2026-11-12 02:40", "2026-11-12 06:50"),
		sample(t, hkSleepAnalysis, "Awake", "", "2026-11-12 06:50", "2026-11-12 07:05"),
		sample(t, hkSleepAnalysis, "Asleep Core", "", "2026-11-12 07:05", "2026-11-12 07:22"),
	}
}

// autoSleepNight is what AutoSleep writes for the same night: one long In Bed
// segment that starts where the Apple stages end and runs past a nap into the
// afternoon. Summed with the Apple stages it reports a night that never
// happened; on its own it reports a 13-hour night.
func autoSleepNight(t *testing.T) []models.CategorySample {
	t.Helper()
	return []models.CategorySample{
		sample(t, hkSleepAnalysis, "In Bed", "AutoSleep", "2026-11-12 03:01", "2026-11-12 16:19"),
		sample(t, hkSleepAnalysis, "Awake", "AutoSleep", "2026-11-12 03:01", "2026-11-12 03:20"),
	}
}

// sleepTotal is the asleep duration the sleep_sessions backfill derives from
// these stages: Deep + Core + REM. In Bed is deliberately not part of it.
func sleepTotal(rows []models.SleepStageRow) float64 {
	var total float64
	for _, r := range rows {
		switch r.Stage {
		case models.SleepStageDeep, models.SleepStageCore, models.SleepStageREM:
			total += r.DurationHr
		}
	}
	return total
}

func TestSleepSourceRank(t *testing.T) {
	for _, tt := range []struct {
		name       string
		source     string
		priorities []string
		want       int
	}{
		{"empty source matches the empty entry", "", sleepPriority(), 2},
		{"bare name matches itself", "Oura", sleepPriority(), 1},
		{"unlisted source ranks last", "AutoSleep", sleepPriority(), 3},
		{"prefix entry covers longer device names", "Apple Watch Series 9", []string{"Apple Watch"}, 0},
		{"empty entry does not cover a named source", "AutoSleep", []string{"", "AutoSleep"}, 1},
		{"no priorities leaves every source equal", "Zepp Life", nil, 0},
		{"matching is case-sensitive, as the SQL LIKE is", "oura", []string{"Oura"}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := sleepSourceRank(tt.source, tt.priorities); got != tt.want {
				t.Errorf("sleepSourceRank(%q, %v) = %d, want %d", tt.source, tt.priorities, got, tt.want)
			}
		})
	}
}

func TestSelectSleepStageSourceSingleNight(t *testing.T) {
	samples := appleNight(t)

	kept, source, dropped := selectSleepStageSource(samples, sleepPriority())

	if source != "" {
		t.Errorf("source = %q, want the empty Apple Health source", source)
	}
	if len(kept) != len(samples) {
		t.Errorf("kept %d of %d samples of the only source", len(kept), len(samples))
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0: nothing competes with the only source", dropped)
	}
}

func TestSelectSleepStageSourceAppleBeatsAutoSleep(t *testing.T) {
	apple := appleNight(t)
	auto := autoSleepNight(t)
	samples := append(append([]models.CategorySample{}, apple...), auto...)

	kept, source, dropped := selectSleepStageSource(samples, sleepPriority())

	if source != "" {
		t.Fatalf("source = %q, want Apple Health (empty) — the empty entry ranks ahead of AutoSleep", source)
	}
	if want := len(apple); len(kept) != want {
		t.Errorf("kept %d samples, want %d (the Apple ones)", len(kept), want)
	}
	if want := len(auto); dropped != want {
		t.Errorf("dropped = %d, want %d (the AutoSleep ones)", dropped, want)
	}

	rows := buildSleepStageRows(kept, 1, slog.New(slog.DiscardHandler))
	if len(rows) != len(apple) {
		t.Fatalf("built %d stage rows, want %d", len(rows), len(apple))
	}
	for _, r := range rows {
		if r.Source != "" {
			t.Errorf("stage row from source %q survived the filter", r.Source)
		}
	}

	// The night counts once: what the Apple rows add up to, not the sum with
	// the AutoSleep segment on top.
	appleOnly := buildSleepStageRows(apple, 1, slog.New(slog.DiscardHandler))
	if got, want := sleepTotal(rows), sleepTotal(appleOnly); got != want {
		t.Errorf("asleep total = %.2f h, want %.2f h — sources were summed", got, want)
	}
	// The AutoSleep In Bed segment alone covers 13.3 h; a night that long means
	// it was counted.
	if got := sleepTotal(rows); got > 9 {
		t.Errorf("asleep total = %.2f h for one night — the second source was summed in", got)
	}
}

// The AutoSleep In Bed segment is dropped together with its source: it is a
// bed span, and one that runs to 16:19 would stretch the night over the gap
// between it and the afternoon nap. The Apple rows alone end at 07:22.
func TestAutoSleepInBedSpanIsDroppedWithItsSource(t *testing.T) {
	apple := appleNight(t)
	auto := autoSleepNight(t)
	samples := append(append([]models.CategorySample{}, auto...), apple...)

	kept, source, _ := selectSleepStageSource(samples, sleepPriority())
	if source != "" {
		t.Fatalf("source = %q, want Apple Health", source)
	}

	rows := buildSleepStageRows(kept, 1, slog.New(slog.DiscardHandler))
	var lastEnd time.Time
	for _, r := range rows {
		if r.Stage == models.SleepStageInBed {
			t.Errorf("In Bed reached sleep_stages (%v → %v): it is a bed span, not a stage, and the backfill would stretch the night over it", r.StartTime, r.EndTime)
		}
		if r.EndTime.After(lastEnd) {
			lastEnd = r.EndTime
		}
	}

	// The AutoSleep In Bed segment ends at 16:19; the night must end where the
	// Apple stages end.
	wantEnd := mustTime(t, "2026-11-12 07:22")
	if !lastEnd.Equal(wantEnd.Time) {
		t.Errorf("night ends at %v, want %v — the In Bed span extended it", lastEnd, wantEnd.Time)
	}
	if span := lastEnd.Sub(rows[0].StartTime).Hours(); span > 9 {
		t.Errorf("night spans %.2f h, want under 9: the daytime gap leaked in", span)
	}
}

func TestSelectSleepStageSourceExplicitSleepRule(t *testing.T) {
	apple := appleNight(t)
	auto := autoSleepNight(t)
	samples := append(append([]models.CategorySample{}, apple...), auto...)

	// An explicit sleep rule: AutoSleep first, Apple Health second.
	kept, source, dropped := selectSleepStageSource(samples, []string{"AutoSleep", ""})

	if source != "AutoSleep" {
		t.Fatalf("source = %q, want AutoSleep — the explicit rule outranks the default", source)
	}
	if want := len(auto); len(kept) != want {
		t.Errorf("kept %d samples, want %d", len(kept), want)
	}
	if want := len(apple); dropped != want {
		t.Errorf("dropped = %d, want %d (the Apple ones)", dropped, want)
	}
	for _, cs := range kept {
		if cs.Source != "AutoSleep" {
			t.Errorf("kept a sample from %q", cs.Source)
		}
	}
}

func TestSelectSleepStageSourceTieBreaksByName(t *testing.T) {
	samples := []models.CategorySample{
		sample(t, hkSleepAnalysis, "Asleep Core", "Zepp Life", "2026-11-11 23:00", "2026-11-12 02:00"),
		sample(t, hkSleepAnalysis, "Asleep Core", "Pillow", "2026-11-11 23:30", "2026-11-12 02:30"),
		sample(t, hkSleepAnalysis, "Asleep Core", "AutoSleep", "2026-11-11 23:15", "2026-11-12 02:15"),
	}

	// Every source is unlisted, so all of them share one rank. The
	// alphabetically first name wins, and the choice does not depend on the
	// order the payload happened to arrive in.
	for _, order := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 0, 2}} {
		picked := make([]models.CategorySample, 0, 3)
		for _, i := range order {
			picked = append(picked, samples[i])
		}
		kept, source, dropped := selectSleepStageSource(picked, []string{"Withings"})
		if source != "AutoSleep" {
			t.Fatalf("order %v: source = %q, want AutoSleep (first name among equals)", order, source)
		}
		if len(kept) != 1 || dropped != 2 {
			t.Fatalf("order %v: kept %d, dropped %d; want 1 and 2", order, len(kept), dropped)
		}
	}
}

func TestSelectSleepStageSourceWithoutPrioritiesPicksOne(t *testing.T) {
	samples := []models.CategorySample{
		sample(t, hkSleepAnalysis, "Asleep Core", "", "2026-11-11 23:00", "2026-11-12 02:00"),
		sample(t, hkSleepAnalysis, "In Bed", "AutoSleep", "2026-11-12 02:00", "2026-11-12 16:00"),
	}

	kept, source, dropped := selectSleepStageSource(samples, nil)

	if source != "" {
		t.Errorf("source = %q, want the empty name (sorts first)", source)
	}
	if len(kept) != 1 || dropped != 1 {
		t.Errorf("kept %d, dropped %d; want 1 and 1: single source even without a configured priority", len(kept), dropped)
	}
}

func TestSleepSamplesOfIgnoresWhatCarriesNoStage(t *testing.T) {
	noLabel := sample(t, hkSleepAnalysis, "Asleep Core", "", "2026-11-11 23:00", "2026-11-12 00:00")
	noLabel.ValueLabel = nil

	samples := []models.CategorySample{
		sample(t, "HKCategoryTypeIdentifierMindfulSession", "Core", "", "2026-11-11 12:00", "2026-11-11 12:10"),
		noLabel,
		sample(t, "sleep_analysis", "Asleep Core", "", "2026-11-11 23:00", "2026-11-12 00:00"),
		sample(t, hkSleepAnalysis, "Asleep Deep", "Oura", "2026-11-12 00:00", "2026-11-12 01:00"),
	}

	got := sleepSamplesOf(samples)

	if len(got) != 2 {
		t.Fatalf("kept %d samples, want 2: only the sleep samples with a stage label", len(got))
	}
	for _, cs := range got {
		if cs.Type == "HKCategoryTypeIdentifierMindfulSession" {
			t.Error("a non-sleep category sample reached the sleep stages")
		}
		if cs.ValueLabel == nil {
			t.Error("a sample without a stage label reached the sleep stages")
		}
	}
}

func TestBuildSleepStageRowsNormalizesLabels(t *testing.T) {
	samples := []models.CategorySample{
		sample(t, hkSleepAnalysis, "Asleep Core", "", "2026-11-11 23:00", "2026-11-12 00:00"),
		sample(t, hkSleepAnalysis, "Asleep Unspecified", "", "2026-11-12 00:00", "2026-11-12 01:00"),
		sample(t, hkSleepAnalysis, "Kern", "AutoSleep", "2026-11-12 01:00", "2026-11-12 02:00"),
		sample(t, hkSleepAnalysis, "Snoozing", "AutoSleep", "2026-11-12 02:00", "2026-11-12 03:00"),
	}

	rows := buildSleepStageRows(samples, 7, slog.New(slog.DiscardHandler))

	want := []string{"Core", "Asleep", "Core", "Snoozing"}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for i, r := range rows {
		if r.Stage != want[i] {
			t.Errorf("row %d stage = %q, want %q", i, r.Stage, want[i])
		}
		if r.UserID != 7 {
			t.Errorf("row %d user = %d, want 7", i, r.UserID)
		}
	}
	if rows[0].DurationHr != 1 {
		t.Errorf("duration = %v h, want 1", rows[0].DurationHr)
	}
}
