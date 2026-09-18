// Package health decides whether a running collection is still producing a
// usable dataset.
//
// systemd's Restart=always covers the process dying. It does not cover the
// failure that actually costs this experiment its deliverable: a bot that is
// alive and writing records, but writing records that cannot be analysed —
// because the book never re-synced after a reconnect, because every call is
// failing, or because the model version moved underneath a run and invalidated
// the thresholds it was tuned with.
//
// Everything here is pure. cmd/logcheck reads the files and decides the exit
// code; this decides what is wrong.
package health

import (
	"fmt"
	"sort"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/calib"
	"github.com/shunta-furukawa/jev-tick-lab/internal/obs"
)

// Thresholds is what "healthy" means. All of it is configuration, because the
// right answer depends on the cadence and on how quiet the pair is.
type Thresholds struct {
	TickInterval    time.Duration // the cadence the bot was started with
	MaxRecordAge    time.Duration // the newest record must be younger than this
	MinRecordRate   float64       // fraction of the expected record count
	MaxErrorRate    float64       // share of ticks whose model call failed
	MaxUnsyncedRate float64       // share of ticks taken against an unseeded book
	MaxStaleRate    float64       // share of ticks taken against a dead feed
	MaxGap          time.Duration // longest tolerable hole in the series
}

func DefaultThresholds() Thresholds {
	return Thresholds{
		TickInterval: time.Second,
		// Two minutes of silence is already a hundred lost ticks; anything
		// longer is not a hiccup.
		MaxRecordAge: 2 * time.Minute,
		// A few skipped ticks an hour is normal — a slow call skips rather than
		// queues, by design. Losing one record in twenty is not.
		MinRecordRate:   0.95,
		MaxErrorRate:    0.02,
		MaxUnsyncedRate: 0.01,
		MaxStaleRate:    0.01,
		MaxGap:          time.Minute,
	}
}

// Gap is a hole in the tick series.
type Gap struct {
	From, To time.Time
	Duration time.Duration
}

// Report is the verdict plus the numbers behind it.
type Report struct {
	At       time.Time     `json:"at"`
	Window   time.Duration `json:"-"`
	WindowS  string        `json:"window"`
	From     time.Time     `json:"from"`
	To       time.Time     `json:"to"`
	Scanned  int           `json:"scanned"`  // records read, whatever their age
	Records  int           `json:"records"`  // records inside the window
	Expected int           `json:"expected"` // records the cadence implies

	// The newest record in the files at all, which is what tells an empty log
	// apart from a stale one when the window turns up nothing.
	NewestOverall time.Time `json:"newest_overall"`

	RecordRate   float64 `json:"record_rate"`  // against the whole window
	CoveredSec   float64 `json:"covered_sec"`  // first to last record in the window
	DensityRate  float64 `json:"density_rate"` // against the span actually covered
	NewestAgeSec float64 `json:"newest_age_sec"`

	Errors       int     `json:"errors"`
	ErrorRate    float64 `json:"error_rate"`
	Unsynced     int     `json:"unsynced"`
	UnsyncedRate float64 `json:"unsynced_rate"`
	Stale        int     `json:"stale"`
	StaleRate    float64 `json:"stale_rate"`
	Halted       int     `json:"halted"`

	Gaps        int           `json:"gaps"`
	LongestGap  time.Duration `json:"-"`
	LongestGapS string        `json:"longest_gap"`
	MissingSec  float64       `json:"missing_sec"`

	// Distinct runs seen in the window. More than one means the collector
	// restarted, which a short gap alone does not reveal — and a restart can
	// land on different code, changing the state text mid-dataset.
	RunIDs []string `json:"run_ids"`

	ModelVersions []string `json:"model_versions"`
	LatencyP50    float64  `json:"latency_p50_ms"`
	LatencyP99    float64  `json:"latency_p99_ms"`

	Problems []string `json:"problems"`
}

// OK reports whether the collection is producing usable data.
func (r Report) OK() bool { return len(r.Problems) == 0 }

func (r Report) Severity() string {
	if r.OK() {
		return "INFO"
	}
	return "ERROR"
}

// Check evaluates the records that fall inside the window ending at now.
//
// An empty window is itself a failure: the point of running this hourly is to
// find out that nothing has been written, and a report that says "0 records,
// all fine" would defeat it.
func Check(records []obs.Record, now time.Time, window time.Duration, t Thresholds) Report {
	if t.TickInterval <= 0 {
		t.TickInterval = time.Second
	}
	now = now.UTC()
	from := now.Add(-window)

	rep := Report{
		At:      now,
		Window:  window,
		WindowS: window.String(),
		From:    from,
		To:      now,
		Scanned: len(records),
	}

	var (
		inWindow []obs.Record
		models   = map[string]bool{}
		runs     = map[string]bool{}
		latency  []float64
	)
	for _, rec := range records {
		at := rec.At.UTC()
		if at.After(rep.NewestOverall) {
			rep.NewestOverall = at
		}
		if at.Before(from) || at.After(now) {
			continue
		}
		inWindow = append(inWindow, rec)
	}
	sort.Slice(inWindow, func(i, j int) bool { return inWindow[i].At.Before(inWindow[j].At) })

	rep.Records = len(inWindow)
	rep.Expected = int(window / t.TickInterval)
	if rep.Expected > 0 {
		rep.RecordRate = float64(rep.Records) / float64(rep.Expected)
	}

	if rep.Records == 0 {
		// Three very different situations produce zero records in the window,
		// and saying only "no records" leaves the reader to guess which.
		// obs.NewLogger creates the file at start-up, before the first record,
		// so an empty log is also what a healthy run looks like during warmup.
		switch {
		case rep.Scanned == 0:
			rep.Problems = append(rep.Problems, fmt.Sprintf(
				"the tick log is empty. The collector creates the file when it starts, so this is also "+
					"what a run looks like before its first record: it waits for a seeded book and for "+
					"-min-history of bar series. If it has been longer than that, check the journal"))
		case !rep.NewestOverall.IsZero():
			rep.Problems = append(rep.Problems, fmt.Sprintf(
				"%d records in the log but none in the last %s; the newest is %s old. The collector stopped",
				rep.Scanned, window, now.Sub(rep.NewestOverall).Round(time.Second)))
		default:
			rep.Problems = append(rep.Problems, fmt.Sprintf("no records in the last %s", window))
		}
		return rep
	}

	rep.From, rep.To = inWindow[0].At.UTC(), inWindow[len(inWindow)-1].At.UTC()
	rep.NewestAgeSec = now.Sub(rep.To).Seconds()

	// Two different questions, and conflating them produces an alert that fires
	// after every restart and then gets ignored:
	//
	//   coverage — how much of the window the log spans at all. Low simply
	//              means the collection is younger than the window.
	//   density  — how many of the ticks inside that span actually landed. Low
	//              means ticks are being lost, which is the real fault.
	//
	// Only density is a problem. Coverage is reported and explained.
	rep.CoveredSec = rep.To.Sub(rep.From).Seconds() + t.TickInterval.Seconds()
	if expected := rep.CoveredSec / t.TickInterval.Seconds(); expected > 0 {
		rep.DensityRate = float64(rep.Records) / expected
	}

	for _, rec := range inWindow {
		if rec.RunID != "" {
			runs[rec.RunID] = true
		}
		if rec.Error != "" {
			rep.Errors++
		} else {
			// A failed call has no model version and no latency worth quoting.
			if rec.ModelVersion != "" {
				models[rec.ModelVersion] = true
			}
			latency = append(latency, rec.LatencyMs)
		}
		if !rec.Snapshot.BookSynced {
			rep.Unsynced++
		}
		if rec.Snapshot.Stale {
			rep.Stale++
		}
		if rec.Snapshot.Halted() {
			rep.Halted++
		}
	}

	n := float64(rep.Records)
	rep.ErrorRate = float64(rep.Errors) / n
	rep.UnsyncedRate = float64(rep.Unsynced) / n
	rep.StaleRate = float64(rep.Stale) / n

	for m := range models {
		rep.ModelVersions = append(rep.ModelVersions, m)
	}
	sort.Strings(rep.ModelVersions)

	for r := range runs {
		rep.RunIDs = append(rep.RunIDs, r)
	}
	sort.Strings(rep.RunIDs)

	if len(latency) > 0 {
		rep.LatencyP50 = calib.Percentile(append([]float64(nil), latency...), 50)
		rep.LatencyP99 = calib.Percentile(append([]float64(nil), latency...), 99)
	}

	// A gap is any interval longer than two ticks: one skipped tick is the
	// in-flight guard doing its job, not a hole.
	tolerance := 2 * t.TickInterval
	for i := 1; i < len(inWindow); i++ {
		d := inWindow[i].At.Sub(inWindow[i-1].At)
		if d <= tolerance {
			continue
		}
		rep.Gaps++
		rep.MissingSec += (d - t.TickInterval).Seconds()
		if d > rep.LongestGap {
			rep.LongestGap = d
		}
	}
	rep.LongestGapS = rep.LongestGap.String()

	rep.Problems = problems(rep, t)
	return rep
}

func problems(r Report, t Thresholds) []string {
	var out []string

	if t.MaxRecordAge > 0 && r.NewestAgeSec > t.MaxRecordAge.Seconds() {
		out = append(out, fmt.Sprintf("newest record is %.0fs old (limit %s) — is the bot still writing?",
			r.NewestAgeSec, t.MaxRecordAge))
	}
	if t.MinRecordRate > 0 && r.DensityRate < t.MinRecordRate {
		out = append(out, fmt.Sprintf(
			"ticks are being lost: %d records across the %s the log covers, %.1f%% of the %.0f%% floor expects",
			r.Records, time.Duration(r.CoveredSec)*time.Second, r.DensityRate*100, t.MinRecordRate*100))
	}
	if t.MaxErrorRate > 0 && r.ErrorRate > t.MaxErrorRate {
		out = append(out, fmt.Sprintf("%.1f%% of calls failed (ceiling %.1f%%)", r.ErrorRate*100, t.MaxErrorRate*100))
	}
	if t.MaxUnsyncedRate > 0 && r.UnsyncedRate > t.MaxUnsyncedRate {
		out = append(out, fmt.Sprintf("%.1f%% of ticks ran against an unseeded book (ceiling %.1f%%) — those answers describe a book we could not see",
			r.UnsyncedRate*100, t.MaxUnsyncedRate*100))
	}
	if t.MaxStaleRate > 0 && r.StaleRate > t.MaxStaleRate {
		out = append(out, fmt.Sprintf("%.1f%% of ticks ran against a stale feed (ceiling %.1f%%)", r.StaleRate*100, t.MaxStaleRate*100))
	}
	if t.MaxGap > 0 && r.LongestGap > t.MaxGap {
		out = append(out, fmt.Sprintf("longest hole in the series is %s (limit %s)", r.LongestGap, t.MaxGap))
	}
	// A restart or two in an hour is somebody deploying. Four is a process that
	// cannot stay up, and the gap check will not say so because each restart
	// only costs a few seconds.
	if len(r.RunIDs) >= 4 {
		out = append(out, fmt.Sprintf("the collector restarted %d times in this window; it is not staying up", len(r.RunIDs)-1))
	}

	// Rule 5: a moved model invalidates every tuned threshold, so a run that
	// spans two versions is not one run.
	if len(r.ModelVersions) > 1 {
		out = append(out, fmt.Sprintf("the model version changed mid-window: %v — records either side are not comparable", r.ModelVersions))
	}
	return out
}
