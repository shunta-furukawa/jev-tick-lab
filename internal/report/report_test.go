package report

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/exec"
	"github.com/shunta-furukawa/jev-tick-lab/internal/jev"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
	"github.com/shunta-furukawa/jev-tick-lab/internal/obs"
)

var t0 = time.Date(2026, 9, 19, 6, 0, 0, 0, time.UTC)

func records(n int, opts ...func(int, *obs.Record)) []obs.Record {
	out := make([]obs.Record, 0, n)
	for i := 0; i < n; i++ {
		at := t0.Add(time.Duration(i) * 3 * time.Second)
		rec := obs.Record{
			TickID: at.Format(time.RFC3339Nano), RunID: "run-a", At: at, Pair: "xrp_jpy",
			ModelVersion: "jev-1.13.0", InputTokens: 1900, LatencyMs: 230,
			Snapshot: marketstate.Snapshot{At: at, Last: 207.5, BookSynced: true},
			Answers: map[string]jev.Answer{
				jev.QAnomaly: {Type: "noul", Noul: 0.35},
				jev.QAction: {Type: "choice", Choice: jev.ActionWait, Confidence: 0.8,
					Probabilities: map[string]float64{jev.ActionWait: 0.8}},
			},
		}
		for _, o := range opts {
			o(i, &rec)
		}
		out = append(out, rec)
	}
	return out
}

func TestBuildSummarises(t *testing.T) {
	t.Parallel()
	rep := Build(records(1200), DefaultOptions())

	if rep.Records != 1200 {
		t.Errorf("records = %d, want 1200", rep.Records)
	}
	if rep.Density < 0.99 {
		t.Errorf("density = %.3f, want ~1 on a complete series", rep.Density)
	}
	if rep.InputTokens != 1200*1900 {
		t.Errorf("tokens = %d", rep.InputTokens)
	}
	if rep.CostPerDay <= 0 {
		t.Error("cost per day should be projected from the span")
	}
	if len(rep.Timeline) == 0 || len(rep.Gates) == 0 || len(rep.Dists) == 0 {
		t.Fatal("timeline, gates and distributions should all be populated")
	}
	// Without forward prices there is nothing to calibrate, and the page must
	// not pretend otherwise.
	if rep.HasOutcomes {
		t.Error("claimed a calibration section on a log that has not been filled")
	}
}

func TestDistributionCountsAgainstTheGateThatReadsIt(t *testing.T) {
	t.Parallel()
	// Half the answers above the 0.30 anomaly gate, half below.
	recs := records(400, func(i int, r *obs.Record) {
		v := 0.10
		if i%2 == 0 {
			v = 0.80
		}
		r.Answers[jev.QAnomaly] = jev.Answer{Type: "noul", Noul: v}
	})

	rep := Build(recs, DefaultOptions())
	var d *Dist
	for i := range rep.Dists {
		if rep.Dists[i].Question == jev.QAnomaly {
			d = &rep.Dists[i]
		}
	}
	if d == nil {
		t.Fatal("no distribution for the anomaly question")
	}
	if d.Gate != 0.30 || !d.GateAbove {
		t.Errorf("gate = %v above=%v, want 0.30 above", d.Gate, d.GateAbove)
	}
	if d.OverGate != 200 {
		t.Errorf("over the gate = %d, want 200", d.OverGate)
	}
}

func TestTimelineShowsAHole(t *testing.T) {
	t.Parallel()
	all := records(1200)
	var kept []obs.Record
	for i, rec := range all {
		if i >= 400 && i < 600 { // ten minutes missing
			continue
		}
		kept = append(kept, rec)
	}

	rep := Build(kept, DefaultOptions())
	empty := 0
	for _, b := range rep.Timeline {
		if b.Records == 0 {
			empty++
		}
	}
	if empty == 0 {
		t.Error("a ten-minute hole should leave at least one empty bucket")
	}
	if rep.Density > 0.9 {
		t.Errorf("density = %.3f, want it to reflect the missing sixth", rep.Density)
	}
}

func TestHTMLIsSelfContained(t *testing.T) {
	t.Parallel()
	html, err := Build(records(600), DefaultOptions()).HTML()
	if err != nil {
		t.Fatal(err)
	}

	// The file has to open from a laptop, a GCS bucket, and an archive in a
	// year. Anything fetched at view time is a way for it to stop working.
	for _, forbidden := range []string{"http://", "https://", "<link", "cdn", "@import"} {
		if strings.Contains(html, forbidden) {
			t.Errorf("page reaches outside itself: found %q", forbidden)
		}
	}
	for _, want := range []string{
		"<svg", "prefers-color-scheme", `data-theme="dark"`, // dark mode is selected, not flipped
		"<table",     // every chart has a non-visual reading beside it
		"data-tip",   // and a hover layer
		`role="tab"`, // the detail is in tabs, so nothing is below the fold
		"ティック取得率",    // the numbers that matter are tiles, not buried
	} {
		if !strings.Contains(html, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

func TestEmptyLogRendersAnExplanationRatherThanACrash(t *testing.T) {
	t.Parallel()
	rep := Build(nil, DefaultOptions())
	html, err := rep.HTML()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "まだ記録がありません") {
		t.Error("an empty log should say so on the page")
	}
}

func TestARestartIsCalledOut(t *testing.T) {
	t.Parallel()
	recs := records(600, func(i int, r *obs.Record) {
		if i > 300 {
			r.RunID = "run-b"
		}
	})
	html, _ := Build(recs, DefaultOptions()).HTML()
	if !strings.Contains(html, "build_revision") {
		t.Error("a window spanning two runs should point at the build revision")
	}
}

func TestHandlerServesAndRebuildsWhenTheLogGrows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ticks-2026-09-19.jsonl")

	write := func(recs []obs.Record) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		enc := json.NewEncoder(f)
		for _, r := range recs {
			if err := enc.Encode(r); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(records(100))

	srv := httptest.NewServer(Handler(dir, DefaultOptions()))
	defer srv.Close()

	body := get(t, srv.URL+"/")
	if !strings.Contains(body, "最終記録") {
		t.Error("a served page should say how fresh it is")
	}
	if !strings.Contains(body, "jtl-tab") {
		t.Error("a served page should keep the selected tab across reloads")
	}

	var health struct {
		OK      bool `json:"ok"`
		Records int  `json:"records"`
	}
	if err := json.Unmarshal([]byte(get(t, srv.URL+"/healthz")), &health); err != nil {
		t.Fatal(err)
	}
	if !health.OK || health.Records != 100 {
		t.Fatalf("healthz = %+v, want ok with 100 records", health)
	}

	// The cache must notice the file growing, or a live dashboard is a
	// screenshot.
	time.Sleep(10 * time.Millisecond)
	more := records(50)
	for i := range more {
		more[i].At = more[i].At.Add(10 * time.Minute)
	}
	write(more)

	if err := json.Unmarshal([]byte(get(t, srv.URL+"/healthz")), &health); err != nil {
		t.Fatal(err)
	}
	if health.Records != 150 {
		t.Errorf("records = %d after appending, want 150", health.Records)
	}
}

// Before the first record there is nothing to draw, and that is warmup rather
// than a fault.
func TestHandlerExplainsItselfBeforeTheFirstRecord(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(Handler(t.TempDir(), DefaultOptions()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 while there is nothing to show", resp.StatusCode)
	}
	if !strings.Contains(string(b), "min-history") {
		t.Error("the waiting page should say what it is waiting for")
	}
}

func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTapePositionsEveryTickByTheTimeItHappened(t *testing.T) {
	t.Parallel()
	// A hole at the sixth tick: the chart must leave a gap where it is rather
	// than close the ranks, or the cadence it claims to show is a fiction.
	recs := records(40)
	recs = append(recs[:6], recs[7:]...)

	opt := DefaultOptions()
	opt.RecentTicks = 40
	rep := Build(recs, opt)

	if len(rep.Recent) != len(recs) {
		t.Fatalf("recent = %d ticks, want %d", len(rep.Recent), len(recs))
	}
	if rep.PriceMin <= 0 || rep.PriceMax < rep.PriceMin {
		t.Fatalf("price range = %v..%v", rep.PriceMin, rep.PriceMax)
	}

	svg := string(svgPrice(rep.Recent, rep.PriceMin, rep.PriceMax, 24))
	if strings.Count(svg, "<circle") < len(rep.Recent) {
		t.Errorf("want one dot per tick, got %d circles for %d ticks",
			strings.Count(svg, "<circle"), len(rep.Recent))
	}

	// The x of the tick after the hole must sit two cadences past the one
	// before it, not one.
	xs := dotXs(t, svg)
	before, after := xs[5], xs[6]
	step := xs[1] - xs[0]
	if gap := after - before; gap < 1.9*step || gap > 2.1*step {
		t.Errorf("gap across the missing tick = %.1f, want ~2 steps of %.1f", gap, step)
	}
}

func TestTapeMarksWhatTheModelWantedRatherThanTradesItNeverMade(t *testing.T) {
	t.Parallel()
	// Shadow mode fills nothing. Claiming a trade here would be the one thing
	// on this page that is not in the record.
	recs := records(20, func(i int, r *obs.Record) {
		if i == 3 || i == 11 {
			r.Answers[jev.QAction] = jev.Answer{Type: "choice", Choice: jev.ActionBuy,
				Confidence: 0.7, Probabilities: map[string]float64{jev.ActionBuy: 0.7}}
		}
	})
	opt := DefaultOptions()
	opt.RecentTicks = 20
	rep := Build(recs, opt)

	wanted := 0
	for _, tk := range rep.Recent {
		if tk.Wanted {
			wanted++
		}
	}
	if wanted != 2 {
		t.Fatalf("wanted ticks = %d, want 2", wanted)
	}
	if rings := strings.Count(string(svgPrice(rep.Recent, rep.PriceMin, rep.PriceMax, 24)), "callring"); rings != 2 {
		t.Errorf("rings = %d, want 2", rings)
	}

	// And the panel has to say so, in words, rather than leaving a reader to
	// assume the rings were fills.
	note := rep.chartPanel().Note
	if !strings.Contains(note, "注文は一切出していません") {
		t.Errorf("the note does not disclaim execution: %q", note)
	}
}

func TestAFlatWindowDoesNotDivideByZero(t *testing.T) {
	t.Parallel()
	// Every tick at the same price: the scale has no span to work with.
	rep := Build(records(10), DefaultOptions())
	svg := string(svgPrice(rep.Recent, rep.PriceMin, rep.PriceMax, 24))
	if svg == "" {
		t.Fatal("no chart for a flat window")
	}
	if strings.Contains(svg, "NaN") || strings.Contains(svg, "Inf") {
		t.Errorf("chart has non-finite coordinates:\n%s", svg)
	}
}

func TestBpsIsNeverSignedBelowItsOwnPrecision(t *testing.T) {
	t.Parallel()
	// "-0.0" reads as a fall. At one decimal place, this move has no direction.
	if got := bpsStr(-0.004); got != "0.0" {
		t.Errorf("bpsStr(-0.004) = %q, want %q", got, "0.0")
	}
	if got := bpsStr(1.24); got != "+1.2" {
		t.Errorf("bpsStr(1.24) = %q, want %q", got, "+1.2")
	}
}

// dotXs pulls the cx of every tick dot out of the chart, in order.
func dotXs(t *testing.T, svg string) []float64 {
	t.Helper()
	var out []float64
	for _, m := range regexp.MustCompile(`<circle cx="([0-9.]+)"[^>]*class="tickdot"`).FindAllStringSubmatch(svg, -1) {
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if len(out) < 8 {
		t.Fatalf("found %d dots, want at least 8", len(out))
	}
	return out
}

func TestACadenceMismatchIsCalledOutRatherThanReportedAsBrokenCollection(t *testing.T) {
	t.Parallel()
	// records() writes a 3s series. Reading it at 1s — the local default —
	// makes a perfect collection look a third full.
	opt := DefaultOptions()
	opt.TickInterval = time.Second
	rep := Build(records(300), opt)

	if rep.ObservedTick != 3*time.Second {
		t.Fatalf("observed cadence = %s, want 3s", rep.ObservedTick)
	}
	if !strings.Contains(warnings(rep), "実際には3秒おき") {
		t.Errorf("no cadence warning; the page reports %s density instead", pct(rep.Density))
	}

	// Told the truth, it says nothing.
	opt.TickInterval = 3 * time.Second
	if w := warnings(Build(records(300), opt)); w != "" {
		t.Errorf("warned on a matching cadence: %q", w)
	}
}

func warnings(r Report) string { return strings.Join(r.view().Warnings, " | ") }

// paperRecords is a run where the taker path traded and the maker path did not.
func paperRecords(n int, heldFrom, heldTo int) []obs.Record {
	return records(n, func(i int, r *obs.Record) {
		r.Mode = "paper"
		pos := marketstate.Position{}
		if i >= heldFrom && i < heldTo {
			pos = marketstate.Position{Side: "long", Size: 48, EntryPrice: 207.0, OpenedAt: t0}
		}
		r.Position = pos
		r.Paper = []exec.StyleState{
			{Style: "taker", Position: pos, RoundTrip: 2, Wins: 0, NetJPY: -24, FeesJPY: 50},
			{Style: "maker", RoundTrip: 0, Wins: 0, NetJPY: 0, FeesJPY: 0, Working: 1},
		}
	})
}

func TestPaperStandingIsReadFromTheNewestRecordNotSummed(t *testing.T) {
	t.Parallel()
	// The ledger in the bot already carries the running totals, so every
	// record repeats them. Adding the column up would multiply the P&L by the
	// number of ticks.
	rep := Build(paperRecords(50, 10, 20), DefaultOptions())
	if len(rep.Paper) != 2 {
		t.Fatalf("paths = %d, want 2", len(rep.Paper))
	}
	if rep.Paper[0].NetJPY != -24 {
		t.Errorf("taker net = %v, want -24 — the standing, not a sum", rep.Paper[0].NetJPY)
	}
	// Gross is derived so the fee bite is a number on the page rather than
	// something the reader has to do in their head.
	if rep.Paper[0].GrossJPY != 26 {
		t.Errorf("taker gross = %v, want 26 (net -24 plus 50 of fees)", rep.Paper[0].GrossJPY)
	}
}

func TestShadowLogsGetNoPaperSection(t *testing.T) {
	t.Parallel()
	rep := Build(records(50), DefaultOptions())
	if len(rep.Paper) != 0 {
		t.Fatal("a shadow log produced an execution section")
	}
	page, err := rep.HTML()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, "Paper execution") {
		t.Error("the page claims paper execution on a run that executed nothing")
	}
}

func TestTheTapeMarksWhereThePositionChangedHands(t *testing.T) {
	t.Parallel()
	opt := DefaultOptions()
	opt.RecentTicks = 50
	rep := Build(paperRecords(50, 10, 20), opt)

	var held, filled int
	for _, tk := range rep.Recent {
		if tk.Held {
			held++
		}
		if tk.Filled {
			filled++
		}
	}
	if held != 10 {
		t.Errorf("held ticks = %d, want 10", held)
	}
	// Two changes of hands: into the position and out of it.
	if filled != 2 {
		t.Errorf("executions marked = %d, want 2", filled)
	}

	// The first tick of the window has nothing to differ from, so it must not
	// be marked — otherwise every reload invents an execution.
	if rep.Recent[0].Filled {
		t.Error("the first tick in the window was marked as an execution")
	}
}

func TestALiveRunSaysSoInWordsRatherThanLeavingItToBeInferred(t *testing.T) {
	t.Parallel()
	// The difference between this page and the paper one is money. A reader
	// must not have to notice a path called "live" to work that out.
	recs := records(20, func(i int, r *obs.Record) {
		r.Mode = "live"
		r.Paper = []exec.StyleState{{
			Style: "live", Intent: "trade", Gate: "no brake on",
			RoundTrip: 1, Wins: 1, NetJPY: -7.2, FeesJPY: 7.2,
		}}
	})
	rep := Build(recs, DefaultOptions())
	if !rep.IsLive() {
		t.Fatal("a live log was not recognised as live")
	}

	page, err := rep.HTML()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page, "本物の注文が出ています") {
		t.Error("the page does not warn that this is real money")
	}
	if strings.Contains(page, "模擬売買") {
		t.Error("a live run is labelled as a simulation")
	}
	// And the brake state has to be visible, since it is the thing that
	// decides whether the bot is still allowed to trade.
	if !strings.Contains(page, "ブレーキ") {
		t.Error("the risk verdict is not shown")
	}
}

func TestAPaperRunIsNeverLabelledLive(t *testing.T) {
	t.Parallel()
	rep := Build(paperRecords(20, 5, 10), DefaultOptions())
	if rep.IsLive() {
		t.Fatal("a paper log was reported as live")
	}
	page, err := rep.HTML()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, "本物の注文が出ています") {
		t.Error("a paper run claims to place real orders")
	}
}

func TestThePageIsInJapaneseAndNamesItsMode(t *testing.T) {
	t.Parallel()
	// The page has one reader and they read Japanese. An English dashboard
	// over a live run is not a cosmetic problem: it is somebody unable to tell
	// what their money is doing.
	for mode, want := range map[string]string{
		"shadow": "影運転",
		"paper":  "模擬売買",
	} {
		recs := records(40, func(i int, r *obs.Record) {
			r.Mode = mode
			if mode == "paper" {
				r.Paper = []exec.StyleState{{Style: "taker"}, {Style: "maker"}}
			}
		})
		page, err := Build(recs, DefaultOptions()).HTML()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(page, want) {
			t.Errorf("%s run does not name its mode as %q", mode, want)
		}
		if !strings.Contains(page, `lang="ja"`) {
			t.Errorf("%s: the document is not declared Japanese", mode)
		}
	}
}

func TestEveryGateIsExplainedRatherThanNamed(t *testing.T) {
	t.Parallel()
	// "gate=entry_quality" tells a reader nothing. Every value decide can
	// emit needs a Japanese label and a line saying why it stopped the trade.
	for _, g := range []string{
		"", "decision_age", "feed", "halt", "anomaly", "hold_risk", "confidence",
		"pyramid", "entry_quality", "fakeout", "spread", "wait", "flat",
	} {
		key := gateLabel(g) // the label the report stores, "" folded to a phrase
		term, ok := gateJA[key]
		if !ok {
			t.Errorf("gate %q has no Japanese entry (looked up %q)", g, key)
			continue
		}
		if term.Label == "" || term.Why == "" {
			t.Errorf("gate %q is named but not explained: %+v", g, term)
		}
	}
}

func TestEveryQuestionIsExplained(t *testing.T) {
	t.Parallel()
	// Same rule for the answer distributions: the id is the dataset's column
	// name, not something a person should have to interpret.
	for _, id := range []string{
		jev.QRegime, jev.QMomentum, jev.QVolatility, jev.QFakeout, jev.QBookPressure,
		jev.QAction, jev.QEntryScore, jev.QAnomaly, jev.QHoldRisk,
	} {
		term, ok := questionJA[id]
		if !ok || term.Label == "" || term.Why == "" {
			t.Errorf("question %q is not explained: %+v", id, term)
		}
	}
}

func TestAnUnknownIdFallsBackToItselfRatherThanVanishing(t *testing.T) {
	t.Parallel()
	// A gate appended to decide but not yet to this file must still show up.
	// An empty cell would hide it exactly when someone is trying to find out
	// why the bot stopped trading.
	if got := gateLabelJA("some_new_gate"); got != "some_new_gate" {
		t.Errorf("unknown gate rendered as %q, want the raw id", got)
	}
}

func TestAQuietMarketLooksQuietRatherThanDramatic(t *testing.T) {
	t.Parallel()
	// xrp_jpy moved 0.45bps over two minutes when this was measured. Fitting
	// the axis to that turns single ticks into cliffs, which is the opposite
	// of what the chart is for: the question is always "did it move enough to
	// pay the round trip", and a frame that magnifies 0.45bps to full height
	// answers it wrongly.
	const cost = 24.0
	base := 222.600
	quiet := make([]Tick, 60)
	for i := range quiet {
		quiet[i] = Tick{At: t0.Add(time.Duration(i) * time.Second), Price: base + float64(i%2)*0.001}
	}
	lo, hi := base, base+0.001

	svg := string(svgPrice(quiet, lo, hi, cost))
	ys := dotYs(t, svg)
	spread := maxOf(ys) - minOf(ys)

	// One tick of 0.001 on 222.6 is 0.045bps. Against a 24bps frame that is a
	// fortieth of the plot height, not a mountain.
	if spread > 12 {
		t.Errorf("a 0.045bps wiggle spans %.1fpx of the plot; the axis is not floored at the round-trip cost", spread)
	}

	// And a move that COULD pay for itself has to be clearly visible, or the
	// floor has been set so wide that nothing ever reads as significant.
	big := make([]Tick, 60)
	for i := range big {
		big[i] = Tick{At: t0.Add(time.Duration(i) * time.Second), Price: base * (1 + float64(i)*cost/10000/59)}
	}
	bigSpread := maxOf(dotYs(t, string(svgPrice(big, big[0].Price, big[len(big)-1].Price, cost)))) -
		minOf(dotYs(t, string(svgPrice(big, big[0].Price, big[len(big)-1].Price, cost))))
	if bigSpread < 100 {
		t.Errorf("a full round-trip move spans only %.1fpx; the floor is too wide to read", bigSpread)
	}
}

func dotYs(t *testing.T, svg string) []float64 {
	t.Helper()
	var out []float64
	for _, m := range regexp.MustCompile(`<circle cx="[0-9.]+" cy="([0-9.]+)"[^>]*class="tickdot"`).FindAllStringSubmatch(svg, -1) {
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		t.Fatal("no dots in the chart")
	}
	return out
}

func minOf(v []float64) float64 {
	m := v[0]
	for _, x := range v {
		if x < m {
			m = x
		}
	}
	return m
}

func maxOf(v []float64) float64 {
	m := v[0]
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}
