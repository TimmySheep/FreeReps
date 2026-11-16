package health

import (
	"log/slog"
	"testing"

	"github.com/claude/freereps/internal/models"
	"github.com/google/uuid"
)

// storedSample is a category_samples row as the re-derivation reads it back:
// the same night the ingest tests build, only through the database's shape.
func storedSample(t *testing.T, typ, label, source, start, end string) models.CategorySampleRow {
	t.Helper()
	l := label
	return models.CategorySampleRow{
		ID:         uuid.New(),
		UserID:     1,
		Type:       typ,
		Value:      2,
		ValueLabel: &l,
		StartDate:  mustTime(t, start).Time,
		EndDate:    mustTime(t, end).Time,
		Source:     source,
	}
}

func TestDeriveSleepStagesFromCategoryRows(t *testing.T) {
	rows := []models.CategorySampleRow{
		// Apple Health, source empty.
		storedSample(t, hkSleepAnalysis, "Asleep Core", "", "2026-11-11 23:10", "2026-11-12 02:00"),
		storedSample(t, hkSleepAnalysis, "Asleep Deep", "", "2026-11-12 02:00", "2026-11-12 03:01"),
		storedSample(t, hkSleepAnalysis, "Awake", "", "2026-11-12 03:01", "2026-11-12 03:20"),
		// AutoSleep for the same night, and its long In Bed span into the
		// afternoon.
		storedSample(t, hkSleepAnalysis, "In Bed", "AutoSleep", "2026-11-12 03:01", "2026-11-12 16:19"),
		storedSample(t, hkSleepAnalysis, "Asleep Unspecified", "AutoSleep", "2026-11-12 03:01", "2026-11-12 07:00"),
		// A source no priority entry lists.
		storedSample(t, hkSleepAnalysis, "Asleep Core", "Zepp Life", "2026-11-11 23:30", "2026-11-12 02:30"),
		// Not sleep at all, and must not count as filtered sleep samples.
		storedSample(t, "HKCategoryTypeIdentifierMindfulSession", "Core", "AutoSleep", "2026-11-12 12:00", "2026-11-12 12:10"),
	}

	stages, report := deriveSleepStagesFromCategoryRows(rows, sleepPriority(), 1, slog.New(slog.DiscardHandler))

	if report.Source != "" {
		t.Errorf("source = %q, want Apple Health (the empty name)", report.Source)
	}
	if report.SamplesUsed != 3 {
		t.Errorf("samples_used = %d, want 3", report.SamplesUsed)
	}
	if report.SamplesFiltered != 3 {
		t.Errorf("samples_filtered = %d, want 3 (two AutoSleep, one Zepp Life)", report.SamplesFiltered)
	}
	if report.Stages != 3 || len(stages) != 3 {
		t.Fatalf("stages = %d (%d rows), want 3", report.Stages, len(stages))
	}
	for _, s := range stages {
		if s.Source != "" {
			t.Errorf("stage row from %q survived", s.Source)
		}
		if s.Stage == models.SleepStageInBed {
			t.Error("an In Bed row reached the stages")
		}
		if s.UserID != 1 {
			t.Errorf("user_id = %d, want 1", s.UserID)
		}
	}
}

func TestDeriveSleepStagesFromCategoryRowsIsRepeatable(t *testing.T) {
	rows := []models.CategorySampleRow{
		storedSample(t, hkSleepAnalysis, "Asleep Core", "", "2026-11-11 23:10", "2026-11-12 02:00"),
		storedSample(t, hkSleepAnalysis, "In Bed", "AutoSleep", "2026-11-12 02:00", "2026-11-12 16:00"),
	}
	// QueryCategorySamples returns newest first; a second run may return the
	// rows in another order once the table has grown.
	reversed := []models.CategorySampleRow{rows[1], rows[0]}

	first, firstReport := deriveSleepStagesFromCategoryRows(rows, sleepPriority(), 1, slog.New(slog.DiscardHandler))
	second, secondReport := deriveSleepStagesFromCategoryRows(reversed, sleepPriority(), 1, slog.New(slog.DiscardHandler))
	again, _ := deriveSleepStagesFromCategoryRows(rows, sleepPriority(), 1, slog.New(slog.DiscardHandler))

	if firstReport != secondReport {
		t.Errorf("reports differ with row order: %+v vs %+v", firstReport, secondReport)
	}
	if len(first) != len(second) {
		t.Errorf("row order changed the row count: %d vs %d", len(first), len(second))
	}
	// A re-run derives the same rows, which is what makes the insert a no-op:
	// sleep_stages deduplicates on (start_time, end_time, stage, user_id).
	if len(first) != len(again) {
		t.Fatalf("second run built %d rows, first built %d", len(again), len(first))
	}
	for i := range first {
		if !first[i].StartTime.Equal(again[i].StartTime) || first[i].Stage != again[i].Stage {
			t.Errorf("row %d differs between runs: %+v vs %+v", i, first[i], again[i])
		}
	}
}

func TestDeriveSleepStagesFromCategoryRowsWithoutSamples(t *testing.T) {
	stages, report := deriveSleepStagesFromCategoryRows(nil, sleepPriority(), 1, slog.New(slog.DiscardHandler))

	if len(stages) != 0 {
		t.Fatalf("built %d rows from no samples", len(stages))
	}
	if report != (SleepRederiveReport{}) {
		t.Errorf("report = %+v, want the zero report", report)
	}
}

func TestDeriveSleepStagesFromCategoryRowsHonoursAnExplicitRule(t *testing.T) {
	rows := []models.CategorySampleRow{
		storedSample(t, hkSleepAnalysis, "Asleep Core", "", "2026-11-11 23:10", "2026-11-12 02:00"),
		storedSample(t, hkSleepAnalysis, "Asleep Unspecified", "AutoSleep", "2026-11-11 23:00", "2026-11-12 06:30"),
	}

	stages, report := deriveSleepStagesFromCategoryRows(rows, []string{"AutoSleep", ""}, 1, slog.New(slog.DiscardHandler))

	if report.Source != "AutoSleep" {
		t.Fatalf("source = %q, want AutoSleep from the explicit rule", report.Source)
	}
	if len(stages) != 1 || stages[0].Source != "AutoSleep" {
		t.Fatalf("stages = %+v, want the single AutoSleep row", stages)
	}
	if got, want := stages[0].DurationHr, 7.5; got != want {
		t.Errorf("duration = %v h, want %v", got, want)
	}
	if !stages[0].StartTime.Equal(mustTime(t, "2026-11-11 23:00").Time) {
		t.Errorf("start = %v, want 2026-11-11 23:00", stages[0].StartTime)
	}
}
