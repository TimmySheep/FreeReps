package health

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/claude/freereps/internal/models"
)

// SleepRederiveReport is what one re-derivation of stored category samples did.
type SleepRederiveReport struct {
	// Source is the source whose samples became the stages.
	Source string `json:"source"`
	// SamplesUsed counts the sleep samples of that source.
	SamplesUsed int `json:"samples_used"`
	// SamplesFiltered counts the sleep samples of every other source.
	SamplesFiltered int `json:"samples_filtered"`
	// Stages counts the stage rows built, StagesInserted the ones sleep_stages
	// did not hold yet.
	Stages         int   `json:"stages"`
	StagesInserted int64 `json:"stages_inserted"`
	// Skipped names why nothing was derived, empty when stages were built.
	Skipped string `json:"skipped,omitempty"`
}

// ReDeriveSleepStages rebuilds the sleep stages of one date range from the
// category samples the server already holds, with the same source rule Ingest
// applies to fresh ones.
//
// It exists because the ingest path only ever sees what a client sends again.
// The iOS app syncs incrementally: an anchor query delivers the samples added
// since the last run, plus a re-send of the last 24 hours, and the full sync
// seeds its cursors from the server's own checkpoints minus a week. A client
// whose category samples are already stored therefore never re-sends them, and
// the sleep stages a changed derivation would produce for those nights are not
// written unless this runs.
//
// Idempotent: sleep_stages has a unique index on
// (start_time, end_time, stage, user_id) and InsertSleepStages uses ON CONFLICT
// DO NOTHING, so a second run inserts nothing. It only ever inserts — rows
// another source wrote earlier are not removed, which keeps a mistaken range a
// no-op rather than a data loss. Run it before relying on the result, with the
// range of the samples to fix.
func (p *Provider) ReDeriveSleepStages(ctx context.Context, userID int, from, to time.Time) (SleepRederiveReport, error) {
	priorities := p.db.ResolveSourcePriority(ctx, userID, "sleep")
	if owner, claimed := sleepOwner(priorities); claimed {
		return SleepRederiveReport{
			Skipped: fmt.Sprintf("sleep is resolved to %s, which syncs it directly", owner),
		}, nil
	}

	rows, err := p.db.QueryCategorySamples(ctx, from, to, userID, "")
	if err != nil {
		return SleepRederiveReport{}, fmt.Errorf("reading category samples: %w", err)
	}

	stages, report := deriveSleepStagesFromCategoryRows(rows, priorities, userID, p.log)
	if len(stages) == 0 {
		p.log.Info("re-derived sleep stages: nothing to derive", "user_id", userID,
			"from", from.Format(time.RFC3339), "to", to.Format(time.RFC3339),
			"category_samples", len(rows))
		return report, nil
	}

	inserted, err := p.db.InsertSleepStages(ctx, stages)
	if err != nil {
		return report, fmt.Errorf("inserting re-derived sleep stages: %w", err)
	}
	report.StagesInserted = inserted

	p.log.Info("re-derived sleep stages", "user_id", userID,
		"from", from.Format(time.RFC3339), "to", to.Format(time.RFC3339),
		"source", report.Source, "samples_used", report.SamplesUsed,
		"samples_filtered", report.SamplesFiltered, "stages", report.Stages,
		"inserted", report.StagesInserted)
	return report, nil
}

// deriveSleepStagesFromCategoryRows is the half of ReDeriveSleepStages that
// does not touch the database: stored category sample rows in, the stage rows
// of one source out.
//
// The rows come from the database rather than from a payload, so they are
// converted back into the shape the ingest path selects from — same fields,
// same empty source for Apple Health — and then go through exactly the same
// filter and builder. Nothing here ranks sources differently from a live
// ingest.
func deriveSleepStagesFromCategoryRows(rows []models.CategorySampleRow, priorities []string, userID int, log *slog.Logger) ([]models.SleepStageRow, SleepRederiveReport) {
	samples := make([]models.CategorySample, 0, len(rows))
	for _, r := range rows {
		samples = append(samples, models.CategorySample{
			ID:           r.ID.String(),
			Type:         r.Type,
			Value:        r.Value,
			ValueLabel:   r.ValueLabel,
			StartDate:    models.HealthTime{Time: r.StartDate},
			EndDate:      models.HealthTime{Time: r.EndDate},
			Source:       r.Source,
			SourceBundle: r.SourceBundle,
		})
	}

	kept, source, filtered := selectSleepStageSource(sleepSamplesOf(samples), priorities)
	stages := buildSleepStageRows(kept, userID, log)

	return stages, SleepRederiveReport{
		Source:          source,
		SamplesUsed:     len(kept),
		SamplesFiltered: filtered,
		Stages:          len(stages),
	}
}
