package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/claude/freereps/internal/models"
)

// SleepAnalysisCategoryType is the HealthKit type the iOS app files sleep
// stages under. The app sends the stages as category samples and nothing turned
// them into sleep_sessions or sleep_stages on the way in, so a night that only
// ever arrived this way had no row in either table and every sleep query
// returned nothing for it. The rows are read here instead, at query time, the
// same way health metrics resolve their source at query time rather than at
// ingest (DECISIONS.md, 2026-03-25).
const SleepAnalysisCategoryType = "HKCategoryTypeIdentifierSleepAnalysis"

// sleepNightGap is the silence that separates two nights, the threshold
// BackfillSleepSessions uses. Keeping it identical means a night derived here
// spans the same interval the backfill would have written had the same stages
// reached sleep_stages.
const sleepNightGap = 12 * time.Hour

// sleepSamplePadding widens the category-sample read on both sides of the
// requested range. A night belongs to the date it ended on, so a night inside
// the range can start a night's length before it, and a night starting inside
// the range can end after it. The derived nights are filtered on their date
// afterwards, which is what makes the padding safe.
const sleepSamplePadding = 48 * time.Hour

// maxNightSpan is the longest a single night can be. A group reaching past it
// is not one night but two whose gap fell inside sleepNightGap — the shape that
// produced the 22-hour and longer durations INCIDENTS.md records for
// 2026-03-26 — so it is split at its widest internal gap.
const maxNightSpan = 24 * time.Hour

// hkSleepStages maps HKCategoryValueSleepAnalysis, used when a sample arrived
// without a value_label.
var hkSleepStages = map[int]string{
	0: models.SleepStageInBed,
	1: models.SleepStageAsleep,
	2: models.SleepStageAwake,
	3: models.SleepStageCore,
	4: models.SleepStageDeep,
	5: models.SleepStageREM,
}

// SleepSegment is one stage interval read from a category sample.
type SleepSegment struct {
	Start  time.Time
	End    time.Time
	Stage  string
	Source string
}

// SleepNight is one derived night: the session row a direct source would have
// written, and the stages behind it.
type SleepNight struct {
	Session models.SleepSessionRow
	Stages  []models.SleepStageRow
}

// canonicalSleepStage maps a category sample to the stage name sleep_stages
// stores.
//
// HealthKit labels the stages "Asleep Core", "Asleep Deep", "Asleep REM" and
// "Asleep Unspecified", while sleep_stages holds "Core", "Deep", "REM" and
// "Asleep" — the names NormalizeSleepStage knows. The "Asleep" prefix is
// stripped first so both spellings land on one name, and the localized names
// keep going through NormalizeSleepStage.
func canonicalSleepStage(s models.CategorySampleRow) (string, bool) {
	raw := ""
	if s.ValueLabel != nil {
		raw = strings.TrimSpace(*s.ValueLabel)
	}

	if raw != "" {
		if stage, known := models.NormalizeSleepStage(raw); known {
			return stage, true
		}
		lower := strings.ToLower(raw)
		if strings.HasPrefix(lower, "asleep") {
			rest := strings.TrimSpace(strings.TrimPrefix(lower, "asleep"))
			switch rest {
			case "", "unspecified", "(unspecified)":
				return models.SleepStageAsleep, true
			}
			if stage, known := models.NormalizeSleepStage(rest); known {
				return stage, true
			}
		}
		return "", false
	}

	if stage, ok := hkSleepStages[s.Value]; ok {
		return stage, true
	}
	return "", false
}

// sourceRank places a source in the priority list exactly as the dedup CTE's
// sourcePriorityCaseSQL does: a named entry matches every source that starts
// with it ("Apple Watch" matches "Apple Watch Series 9"), "" matches only the
// empty source, and a source no entry matches ranks after all of them.
func sourceRank(source string, priorities []string) int {
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

// selectNightSource keeps the segments of exactly one source for a night.
//
// Segments of two sources are never added together. That sum is what the
// 2026-03-26 incident produced — nights of 22 hours and more once Oura and
// Health Auto Export both delivered the same night (INCIDENTS.md) — and its
// lesson was that a derivation aggregating across sources needs the source
// filter at the point it reads. The highest priority wins; between equally
// ranked sources the first by name wins, matching the ORDER BY source of
// winningSourceRN.
func selectNightSource(segs []SleepSegment, priorities []string) (string, []SleepSegment) {
	best := ""
	bestRank := -1
	for _, s := range segs {
		r := sourceRank(s.Source, priorities)
		if bestRank < 0 || r < bestRank || (r == bestRank && s.Source < best) {
			bestRank, best = r, s.Source
		}
	}

	kept := make([]SleepSegment, 0, len(segs))
	for _, s := range segs {
		if s.Source == best {
			kept = append(kept, s)
		}
	}
	return best, kept
}

// groupSleepSegments splits sorted segments wherever more than sleepNightGap
// separates the next start from the night so far.
//
// Time in bed does not extend the night. An "In Bed" sample covers the whole
// night while the stage segments nest inside it, so taking its projected end as
// the night's reach merged the following night into the same session: with a
// 19:01-08:19 sample the next night's 19:2x start sat 11 h later, inside the
// threshold, and two nights came out as one of 17.5 h. The reach is therefore
// the furthest end of a stage that says when the person was asleep or awake,
// and the In Bed bounds are used only for a night that holds nothing else.
func groupSleepSegments(segs []SleepSegment) [][]SleepSegment {
	var groups [][]SleepSegment
	var current []SleepSegment
	var reach, bedReach time.Time

	for _, s := range segs {
		ref := reach
		if ref.IsZero() {
			ref = bedReach
		}
		if len(current) > 0 && s.Start.Sub(ref) > sleepNightGap {
			groups = append(groups, current)
			current = nil
			reach, bedReach = time.Time{}, time.Time{}
		}

		current = append(current, s)
		if s.Stage == models.SleepStageInBed {
			if s.End.After(bedReach) {
				bedReach = s.End
			}
			continue
		}
		if s.End.After(reach) {
			reach = s.End
		}
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}

	split := make([][]SleepSegment, 0, len(groups))
	for _, g := range groups {
		split = append(split, splitOversizedNight(g)...)
	}
	return split
}

// splitOversizedNight divides a group whose span exceeds maxNightSpan at its
// widest internal gap, recursively, so two nights joined by sleepNightGap come
// out as the two nights they are.
func splitOversizedNight(group []SleepSegment) [][]SleepSegment {
	if len(group) < 2 {
		return [][]SleepSegment{group}
	}

	var spanEnd time.Time
	for _, s := range group {
		if s.End.After(spanEnd) {
			spanEnd = s.End
		}
	}
	if spanEnd.Sub(group[0].Start) <= maxNightSpan {
		return [][]SleepSegment{group}
	}

	widestIdx, widest := -1, time.Duration(0)
	var reach time.Time
	for i, s := range group {
		if i > 0 && s.Start.Sub(reach) > widest {
			widest, widestIdx = s.Start.Sub(reach), i
		}
		if s.End.After(reach) {
			reach = s.End
		}
	}
	if widestIdx <= 0 {
		return [][]SleepSegment{group}
	}

	var out [][]SleepSegment
	out = append(out, splitOversizedNight(group[:widestIdx])...)
	out = append(out, splitOversizedNight(group[widestIdx:])...)
	return out
}

// BuildSleepNight turns one night's segments, already reduced to a single
// source, into the session row a direct source would have written and the stage
// rows behind it. It reports false when the night holds neither sleep nor time
// in bed, which is what an "Awake"-only group amounts to.
//
// The session date is the UTC day the night ended on, which is the rule
// BackfillSleepSessions applies and the one get_sleep_data queries by. Changing
// it here alone would put derived and stored sessions on different dates for
// the same night.
func BuildSleepNight(userID int, segs []SleepSegment) (models.SleepSessionRow, []models.SleepStageRow, bool) {
	if len(segs) == 0 {
		return models.SleepSessionRow{}, nil, false
	}
	source := segs[0].Source

	var core, deep, rem, asleep, inBedDur float64
	var asleepStart, asleepEnd, inBedStart, inBedEnd time.Time

	stages := make([]models.SleepStageRow, 0, len(segs))
	for _, s := range segs {
		hrs := s.End.Sub(s.Start).Hours()
		if hrs < 0 {
			hrs = 0
		}
		stages = append(stages, models.SleepStageRow{
			StartTime:  s.Start,
			EndTime:    s.End,
			UserID:     userID,
			Stage:      s.Stage,
			DurationHr: hrs,
			Source:     source,
		})

		switch s.Stage {
		case models.SleepStageCore:
			core += hrs
		case models.SleepStageDeep:
			deep += hrs
		case models.SleepStageREM:
			rem += hrs
		case models.SleepStageAsleep:
			asleep += hrs
		case models.SleepStageInBed:
			inBedDur += hrs
			if inBedStart.IsZero() || s.Start.Before(inBedStart) {
				inBedStart = s.Start
			}
			if s.End.After(inBedEnd) {
				inBedEnd = s.End
			}
		case models.SleepStageAwake:
			// Counted as a stage, never as sleep.
		}

		// Time in bed is not sleep: it covers the night the stages nest in, so
		// it must not widen the asleep interval either.
		if s.Stage != models.SleepStageInBed && s.Stage != models.SleepStageAwake {
			if asleepStart.IsZero() || s.Start.Before(asleepStart) {
				asleepStart = s.Start
			}
			if s.End.After(asleepEnd) {
				asleepEnd = s.End
			}
		}
	}

	totalSleep := core + deep + rem + asleep
	if totalSleep == 0 && inBedDur == 0 {
		return models.SleepSessionRow{}, nil, false
	}

	sleepStart, sleepEnd := asleepStart, asleepEnd
	if sleepEnd.IsZero() {
		// No stage said "asleep"; fall back to the time in bed, and then to the
		// segments as they came, so the night keeps a position on the timeline.
		sleepStart, sleepEnd = inBedStart, inBedEnd
	}
	if sleepEnd.IsZero() {
		sleepStart, sleepEnd = segs[0].Start, segs[0].End
	}

	inBed := inBedDur
	if inBed == 0 {
		inBed = sleepEnd.Sub(sleepStart).Hours()
	} else if covered := inBedEnd.Sub(inBedStart).Hours(); inBed > covered {
		// Overlapping In Bed samples of one source would otherwise report more
		// time in bed than the interval they cover.
		inBed = covered
	}
	if inBedStart.IsZero() {
		inBedStart, inBedEnd = sleepStart, sleepEnd
	}

	return models.SleepSessionRow{
		UserID:     userID,
		Date:       sleepEnd.Truncate(24 * time.Hour),
		TotalSleep: totalSleep,
		Asleep:     totalSleep,
		Core:       core,
		Deep:       deep,
		REM:        rem,
		InBed:      inBed,
		SleepStart: sleepStart,
		SleepEnd:   sleepEnd,
		InBedStart: inBedStart,
		InBedEnd:   inBedEnd,
	}, stages, true
}

// DeriveSleepNights groups sleep category samples into nights, resolves each
// night to a single source and returns the sessions and stages that follow.
//
// Unknown stage labels are dropped rather than counted as sleep: a label this
// build does not recognize says nothing about how much of the night was spent
// asleep, and guessing "Asleep" would invent the very figure the caller asked
// for.
func DeriveSleepNights(userID int, samples []models.CategorySampleRow, priorities []string) []SleepNight {
	segs := make([]SleepSegment, 0, len(samples))
	for _, s := range samples {
		stage, known := canonicalSleepStage(s)
		if !known {
			continue
		}
		if !s.EndDate.After(s.StartDate) {
			continue
		}
		segs = append(segs, SleepSegment{
			Start:  s.StartDate,
			End:    s.EndDate,
			Stage:  stage,
			Source: s.Source,
		})
	}
	sort.SliceStable(segs, func(i, j int) bool { return segs[i].Start.Before(segs[j].Start) })

	var nights []SleepNight
	for _, group := range groupSleepSegments(segs) {
		_, kept := selectNightSource(group, priorities)
		session, stages, ok := BuildSleepNight(userID, kept)
		if !ok {
			continue
		}
		nights = append(nights, SleepNight{Session: session, Stages: stages})
	}

	sort.SliceStable(nights, func(i, j int) bool {
		return nights[i].Session.Date.Before(nights[j].Session.Date)
	})

	// One session per date, the earliest-ending night of that date first, which
	// is the row sleep_sessions' UNIQUE (user_id, date) leaves in place under
	// BackfillSleepSessions' ON CONFLICT DO NOTHING. Two nights can end on the
	// same UTC day when an early-morning night is followed by an evening one.
	unique := nights[:0]
	seen := make(map[int64]bool, len(nights))
	for _, n := range nights {
		key := n.Session.Date.Unix()
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, n)
	}
	return unique
}

// mergeSleepData combines the stored sessions with the derived nights. A date
// that already holds a stored session keeps it: a session written by the source
// itself is the more accurate row, and it is the same rule BackfillSleepSessions
// follows with ON CONFLICT DO NOTHING.
func mergeSleepData(stored []SleepSessionResult, storedStages []models.SleepStageRow, derived []SleepNight) ([]SleepSessionResult, []models.SleepStageRow) {
	sessions := append([]SleepSessionResult(nil), stored...)
	stages := append([]models.SleepStageRow(nil), storedStages...)

	have := make(map[int64]bool, len(stored))
	for _, s := range stored {
		have[s.Date.Unix()] = true
	}
	for _, n := range derived {
		key := n.Session.Date.Unix()
		if have[key] {
			continue
		}
		have[key] = true
		sessions = append(sessions, SleepSessionResult{SleepSessionRow: n.Session, Derived: true})
		stages = append(stages, n.Stages...)
	}

	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].Date.After(sessions[j].Date) })
	sort.SliceStable(stages, func(i, j int) bool { return stages[i].StartTime.Before(stages[j].StartTime) })
	return sessions, stages
}

// sleepCategorySamples reads the sleep category samples around a date range.
func (db *DB) sleepCategorySamples(ctx context.Context, start, end time.Time, userID int) ([]models.CategorySampleRow, error) {
	rows, err := db.Pool.Query(ctx,
		`SELECT id, user_id, type, value, value_label, start_date, end_date, source
		 FROM category_samples
		 WHERE user_id = $1 AND type = $2 AND start_date >= $3 AND start_date < $4
		 ORDER BY start_date ASC`,
		userID, SleepAnalysisCategoryType,
		start.Add(-sleepSamplePadding), end.Add(sleepSamplePadding))
	if err != nil {
		return nil, fmt.Errorf("querying sleep category samples: %w", err)
	}
	defer rows.Close()

	var result []models.CategorySampleRow
	for rows.Next() {
		var r models.CategorySampleRow
		if err := rows.Scan(&r.ID, &r.UserID, &r.Type, &r.Value, &r.ValueLabel,
			&r.StartDate, &r.EndDate, &r.Source); err != nil {
			return nil, fmt.Errorf("scanning sleep category sample: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// DerivedSleepNights returns the nights the sleep category samples imply for
// the dates in [start, end).
//
// The source priority is the user's "sleep" rule, falling back to "_default"
// and then to the configured list — the same resolution sleepClaimedBySync uses
// to decide whether a syncing provider already owns sleep.
func (db *DB) DerivedSleepNights(ctx context.Context, start, end time.Time, userID int) ([]SleepNight, error) {
	priorities := db.ResolveSourcePriority(ctx, userID, "sleep")

	samples, err := db.sleepCategorySamples(ctx, start, end, userID)
	if err != nil {
		return nil, err
	}

	nights := DeriveSleepNights(userID, samples, priorities)
	kept := make([]SleepNight, 0, len(nights))
	for _, n := range nights {
		date := n.Session.Date
		if date.Before(start) || !date.Before(end) {
			continue
		}
		kept = append(kept, n)
	}
	return kept, nil
}

// QuerySleepData returns the sleep sessions and stages for a date range.
//
// A date that holds a stored session returns that session. Every other date is
// derived from the sleep category samples, which is the only place a night that
// came in through the iOS app's category-sample payload exists.
func (db *DB) QuerySleepData(ctx context.Context, start, end time.Time, userID int) ([]SleepSessionResult, []models.SleepStageRow, error) {
	stored, err := db.QuerySleepSessions(ctx, start, end, userID)
	if err != nil {
		return nil, nil, err
	}

	storedStages, err := db.QuerySleepStages(ctx, start, end, userID)
	if err != nil {
		return nil, nil, err
	}

	derived, err := db.DerivedSleepNights(ctx, start, end, userID)
	if err != nil {
		return nil, nil, err
	}

	sessions, stages := mergeSleepData(stored, storedStages, derived)
	return sessions, stages, nil
}
