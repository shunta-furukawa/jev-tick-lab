package obs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/decide"
	"github.com/shunta-furukawa/jev-tick-lab/internal/jev"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
)

func sampleRecord(at time.Time) Record {
	return Record{
		TickID:         at.Format(time.RFC3339Nano),
		RunID:          "20260917T120000Z",
		At:             at,
		Pair:           "xrp_jpy",
		Mode:           "shadow",
		ModelRequested: "jev-1.13.0",
		ModelVersion:   "jev-1.13.0",
		Snapshot:       marketstate.Snapshot{At: at, Pair: "xrp_jpy", Last: 202.25, Bars: make([]marketstate.Bar, 300)},
		StateText:      "# Market: xrp_jpy",
		StateHash:      "deadbeef",
		Answers:        map[string]jev.Answer{jev.QAnomaly: {Type: "noul", Noul: 0.1}},
		Signal:         decide.Signal{At: at, Intent: decide.IntentNone, Gate: decide.GateWait, Reason: "model says wait"},
		InputTokens:    1500,
		LatencyMs:      412.5,
	}
}

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line is not valid JSON: %v\n%s", err, line)
		}
		out = append(out, m)
	}
	return out
}

func TestWriteProducesOneJSONObjectPerTick(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	at := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := l.Write(sampleRecord(at.Add(time.Duration(i) * time.Second))); err != nil {
			t.Fatal(err)
		}
	}

	lines := readLines(t, filepath.Join(dir, "ticks-2026-09-17.jsonl"))
	if len(lines) != 3 {
		t.Fatalf("got %d records, want 3", len(lines))
	}
	for _, want := range []string{"tick_id", "run_id", "pair", "model_version", "state_text", "state_hash", "answers", "signal", "input_tokens", "latency_ms"} {
		if _, ok := lines[0][want]; !ok {
			t.Errorf("record is missing the %q field", want)
		}
	}
}

// Each write is flushed, so a kill -9 loses nothing. This is what makes the log
// the deliverable rather than a best-effort trace.
func TestWriteIsDurableWithoutClosing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Write(sampleRecord(time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC))); err != nil {
		t.Fatal(err)
	}
	// Deliberately no Close().
	if lines := readLines(t, filepath.Join(dir, "ticks-2026-09-17.jsonl")); len(lines) != 1 {
		t.Fatalf("got %d records before Close, want 1", len(lines))
	}
}

// 300 bars per record at one record per second is about a gigabyte a day of
// data the state text already carries.
func TestBarsAreNotSerialisedIntoEveryRecord(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, _ := NewLogger(dir)
	defer l.Close()

	rec := sampleRecord(time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC))
	if err := l.Write(rec); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "ticks-2026-09-17.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"Bars"`) {
		t.Error("the bar series was serialised into the tick record")
	}
	if len(raw) > 4096 {
		t.Errorf("a record is %d bytes; something large leaked into the schema", len(raw))
	}
}

func TestRotationFollowsTheUTCDay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if err := l.Write(sampleRecord(time.Date(2026, 9, 17, 23, 59, 59, 0, time.UTC))); err != nil {
		t.Fatal(err)
	}
	if err := l.Write(sampleRecord(time.Date(2026, 9, 18, 0, 0, 1, 0, time.UTC))); err != nil {
		t.Fatal(err)
	}

	for _, day := range []string{"2026-09-17", "2026-09-18"} {
		if lines := readLines(t, filepath.Join(dir, "ticks-"+day+".jsonl")); len(lines) != 1 {
			t.Errorf("%s: got %d records, want 1", day, len(lines))
		}
	}
}

func TestFailedCallsAreStillRecorded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, _ := NewLogger(dir)
	defer l.Close()

	rec := sampleRecord(time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC))
	rec.Answers, rec.ModelVersion = nil, ""
	rec.Error = "typesafe 529: overloaded"
	if err := l.Write(rec); err != nil {
		t.Fatal(err)
	}

	lines := readLines(t, filepath.Join(dir, "ticks-2026-09-17.jsonl"))
	if got := lines[0]["error"]; got != "typesafe 529: overloaded" {
		t.Errorf("error field = %v; a gap in the log is only data if it is labelled", got)
	}
}

func TestWriteRunRecordsTheConfiguration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, _ := NewLogger(dir)
	defer l.Close()

	started := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	run := Run{
		RunID:          NewRunID(started),
		StartedAt:      started,
		Pair:           "xrp_jpy",
		Mode:           "shadow",
		ModelRequested: "jev-1.13.0",
		Thresholds:     decide.DefaultThresholds(),
		QuestionIDs:    jev.IDs(jev.QuestionSet()),
		QuestionsHash:  HashQuestions(jev.QuestionSet()),
		Questions:      jev.QuestionSet(),
	}
	if err := l.WriteRun(run); err != nil {
		t.Fatal(err)
	}

	lines := readLines(t, filepath.Join(dir, "runs-2026-09-17.jsonl"))
	if len(lines) != 1 {
		t.Fatalf("got %d run rows, want 1", len(lines))
	}
	if lines[0]["run_id"] != "20260917T120000Z" {
		t.Errorf("run_id = %v", lines[0]["run_id"])
	}
	th, ok := lines[0]["thresholds"].(map[string]any)
	if !ok || th["MinActionProb"] != 0.55 {
		t.Errorf("thresholds were not recorded: %v", lines[0]["thresholds"])
	}
}

// If the question set changes, records from either side of the change are not
// directly comparable — so the fingerprint has to move when the set does.
func TestHashQuestionsIsStableAndSensitive(t *testing.T) {
	t.Parallel()
	base := HashQuestions(jev.QuestionSet())
	if base == "" {
		t.Fatal("empty hash")
	}
	if again := HashQuestions(jev.QuestionSet()); again != base {
		t.Error("the same question set hashed differently twice")
	}

	changed := jev.QuestionSet()
	q := changed[jev.QMomentum]
	q.Instructions += " (reworded)"
	changed[jev.QMomentum] = q
	if HashQuestions(changed) == base {
		t.Error("a reworded question did not change the hash")
	}
}

func TestRecordRoundTrips(t *testing.T) {
	t.Parallel()
	rec := sampleRecord(time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC))
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var back Record
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.TickID != rec.TickID || back.Signal.Gate != rec.Signal.Gate || back.Answers[jev.QAnomaly].Noul != 0.1 {
		t.Errorf("round trip lost data: %+v", back)
	}
}

// The gates are a derived column, not a measurement.
//
// Shadow mode consumes nothing: decide.Compose runs, its Signal is logged, and
// no executor reads it. Since Compose is pure, any threshold set can be applied
// to the logged answers afterwards — which is what makes phase 3 possible, and
// what makes it safe to start a multi-day collection without having tuned
// MaxAnomalyNoul first.
//
// That only holds if every input Compose reads survives the round trip. This
// test is what keeps it true.
func TestSignalIsRederivableFromALoggedRecord(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC)
	snap := marketstate.Snapshot{
		At: at, Pair: "xrp_jpy", Last: 207.52, BestBid: 207.51, BestAsk: 207.53,
		SpreadBps: 0.96, BookSynced: true, CircuitBreak: "NONE",
	}
	// The answers preflight actually returned on 2026-09-18.
	answers := map[string]jev.Answer{
		jev.QAnomaly:    {Type: "noul", Noul: 0.41},
		jev.QFakeout:    {Type: "noul", Noul: 0.58},
		jev.QHoldRisk:   {Type: "score", Score: 0.24, Confidence: 0.80},
		jev.QEntryScore: {Type: "score", Score: 1.45, Confidence: 0.43},
		jev.QAction: {
			Type: "choice", Choice: jev.ActionWait, Confidence: 0.78,
			Probabilities: map[string]float64{jev.ActionWait: 0.83, jev.ActionBuy: 0.17},
		},
	}
	pos := marketstate.Position{Side: "long", Size: 100, EntryPrice: 200, OpenedAt: at.Add(-time.Minute)}

	th := decide.DefaultThresholds()
	decidedAt := at.Add(561 * time.Millisecond)

	rec := Record{TickID: at.Format(time.RFC3339Nano), At: at, Snapshot: snap, Position: pos, Answers: answers}
	rec.Signal = decide.Compose(at, decidedAt, snap, answers, pos, th)

	// Round trip through the wire format, as cmd/fill and cmd/calib see it.
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var back Record
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}

	// decidedAt is recoverable from the recorded age.
	rederivedAt := back.At.Add(time.Duration(back.Signal.AgeMs) * time.Millisecond)
	got := decide.Compose(back.At, rederivedAt, back.Snapshot, back.Answers, back.Position, th)

	if got != rec.Signal {
		t.Fatalf("re-derived signal differs:\n got %+v\nwant %+v", got, rec.Signal)
	}

	// And the point of all this: a different threshold set produces a different
	// answer from the same record, months later.
	th.MaxAnomalyNoul = 0.60 // above the 0.41 this tick actually returned
	retuned := decide.Compose(back.At, rederivedAt, back.Snapshot, back.Answers, back.Position, th)
	if retuned.Gate == decide.GateAnomaly {
		t.Error("raising MaxAnomalyNoul did not release the anomaly gate")
	}
	if rec.Signal.Gate != decide.GateAnomaly {
		t.Errorf("fixture no longer exercises the anomaly gate: %q", rec.Signal.Gate)
	}
}
