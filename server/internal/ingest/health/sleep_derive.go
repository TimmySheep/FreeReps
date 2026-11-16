package health

import (
	"log/slog"
	"sort"
	"strings"

	"github.com/claude/freereps/internal/models"
)

// isSleepSampleType reports whether a category sample's type carries sleep
// stages. HealthKit names the sleep type HKCategoryTypeIdentifierSleepAnalysis;
// older app versions and other importers use sleep_analysis or a name that
// contains sleep.
func isSleepSampleType(t string) bool {
	return strings.EqualFold(t, "HKCategoryTypeIdentifierSleepAnalysis") ||
		strings.EqualFold(t, "sleep_analysis") ||
		strings.Contains(strings.ToLower(t), "sleep")
}

// sleepSamplesOf returns the category samples that can become a sleep stage.
// A sample without a value label carries no stage and is left out here, so the
// source selection below counts only samples that would be written.
func sleepSamplesOf(samples []models.CategorySample) []models.CategorySample {
	var out []models.CategorySample
	for _, cs := range samples {
		if !isSleepSampleType(cs.Type) || cs.ValueLabel == nil {
			continue
		}
		out = append(out, cs)
	}
	return out
}

// sleepSourceRank returns the position of a source in the priority list: lower
// is better, and len(priorities) means no entry covers the source.
//
// The matching mirrors sourcePriorityCaseSQL, which every read path ranks
// with: a named entry covers the sources that start with it ("Apple Watch"
// reaches "Apple Watch Series 9"), and the empty entry covers the empty source
// exactly — that is Apple Health, whose rows carry no name. Matching is
// case-sensitive for the same reason: the SQL compares LIKE on the stored
// value.
func sleepSourceRank(source string, priorities []string) int {
	for i, p := range priorities {
		if p == "" {
			if source == "" {
				return i
			}
			continue
		}
		if strings.HasPrefix(source, p) {
			return i
		}
	}
	return len(priorities)
}

// selectSleepStageSource keeps the samples of the one source whose stages make
// up this user's sleep, and reports that source and how many samples of other
// sources it left out.
//
// One source only, because nothing downstream separates them again. A night
// arrives from several: Apple Health as stage rows without a source name,
// AutoSleep as long In-Bed and Awake segments, Zepp Life or Pillow as their
// own stage rows. Inserting all of them and summing them by time window (the
// way storage.BackfillSleepSessions counts a night) made 2026-03-26 report
// 22 hours of sleep, and an AutoSleep In Bed segment running from 03:01 to
// 16:19 swallows the four hours of wakefulness between a night and a nap. The
// choice therefore has to be made before the rows are written, exactly as the
// read paths do with sourcePriorityCaseSQL.
//
// The winner is the source with the best rank; ties inside that rank (two
// sources no entry covers, or one entry covering both by prefix) go to the
// alphabetically first source name, so the same payload always derives the
// same stages. With no priorities configured every source ranks equally and
// the first name still wins, which keeps the single-source rule in force where
// the config says nothing.
func selectSleepStageSource(samples []models.CategorySample, priorities []string) (kept []models.CategorySample, source string, dropped int) {
	if len(samples) == 0 {
		return nil, "", 0
	}

	best := sleepSourceRank(samples[0].Source, priorities)
	names := map[string]bool{samples[0].Source: true}
	for _, cs := range samples[1:] {
		switch rank := sleepSourceRank(cs.Source, priorities); {
		case rank < best:
			best = rank
			names = map[string]bool{cs.Source: true}
		case rank == best:
			names[cs.Source] = true
		}
	}

	candidates := make([]string, 0, len(names))
	for name := range names {
		candidates = append(candidates, name)
	}
	sort.Strings(candidates)
	source = candidates[0]

	kept = make([]models.CategorySample, 0, len(samples))
	for _, cs := range samples {
		if cs.Source == source {
			kept = append(kept, cs)
		}
	}
	return kept, source, len(samples) - len(kept)
}

// buildSleepStageRows turns sleep category samples into rows of sleep_stages.
// Callers pass the samples of one source only, as selectSleepStageSource
// returned them; this function does not look at sources again.
//
// An "Asleep X" label becomes the stage X, "Asleep Unspecified" becomes
// Asleep, and an unknown label is stored as it arrived. Durations come from
// the sample's own bounds; no time zone is applied here.
func buildSleepStageRows(samples []models.CategorySample, userID int, log *slog.Logger) []models.SleepStageRow {
	var rows []models.SleepStageRow
	for _, cs := range samples {
		if cs.ValueLabel == nil {
			continue
		}
		label := *cs.ValueLabel
		if after, ok := strings.CutPrefix(label, "Asleep "); ok {
			if after == "Unspecified" {
				label = "Asleep"
			} else {
				label = after
			}
		}
		stage, known := models.NormalizeSleepStage(label)
		if !known && log != nil {
			log.Warn("unknown sleep stage from category sample, storing as-is", "raw", label)
		}
		rows = append(rows, models.SleepStageRow{
			StartTime:    cs.StartDate.Time,
			EndTime:      cs.EndDate.Time,
			UserID:       userID,
			Stage:        stage,
			DurationHr:   cs.EndDate.Time.Sub(cs.StartDate.Time).Hours(),
			Source:       cs.Source,
			SourceBundle: cs.SourceBundle,
		})
	}
	return rows
}
