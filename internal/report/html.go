package report

import (
	"bytes"
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/calib"
)

// HTML renders the report as one self-contained page.
//
// No CDN, no fonts, no scripts from anywhere: the file has to open from a
// laptop, from a GCS bucket, and from an archive in a year's time. Everything
// it needs is inline.
//
// Colours come from the validated palette. The chart marks use one hue; the
// status colours appear only in stat tiles, always beside a word, because
// status-good and status-critical are four Delta E apart under deuteranopia and
// must never carry meaning alone.
func (r Report) HTML() (string, error) {
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, r.view()); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// kpi is one stat tile. Hero marks the single number the page leads with.
type kpi struct {
	Label, Value, Note, Status, Tip string
	Hero                            bool
}

// panel is the left-hand region: the chart, and under it the tape. The tape
// lives here rather than in a tab because on a maximised browser the chart
// leaves a screenful of space below it, and the raw sequence is the thing a
// person watching a run actually wants next to the picture of it.
type panel struct {
	Title, Note string
	SVG         template.HTML
	Extra       template.HTML
	BelowTitle  string
	Below       template.HTML
}

// tab is one of the right-hand panels. Everything the old page put below the
// fold lives in one of these, so a maximised browser needs no scrolling.
type tab struct {
	Name string
	Note string
	HTML template.HTML
}

type pageView struct {
	Title, Subtitle      string
	ModeLabel, ModeClass string
	Meta                 [][2]string
	Warnings             []string
	KPIs                 []kpi
	Chart                panel
	Tabs                 []tab
	Live                 bool
}

func (r Report) view() pageView {
	mode := r.Mode()
	m := lookup(modeJA, mode)
	v := pageView{
		Title:     "jev-tick-lab",
		Subtitle:  r.Pair,
		ModeLabel: m.Label,
		ModeClass: mode,
		Live:      r.Live,
	}
	if r.Records == 0 {
		v.Warnings = []string{"まだ記録がありません。収集プロセスは動いていますか？"}
		v.Chart = panel{Title: "まだ何もありません", Note: "最初の記録が書かれると、ここに値動きが出ます。"}
		return v
	}

	v.Meta = [][2]string{
		{"期間", fmt.Sprintf("%s → %s UTC", r.From.Format("01/02 15:04"), r.To.Format("15:04"))},
		{"モデル", strings.Join(r.Models, ", ")},
		{"評価間隔", jaDuration(r.Options.TickInterval)},
	}
	if r.Live {
		age := time.Since(r.To)
		label := round(age) + "前"
		if age > 2*time.Minute {
			label += "（収集が止まっているかもしれません）"
		}
		v.Meta = append(v.Meta, [2]string{"最終記録", label})
	}

	v.Warnings = r.warnings()
	v.KPIs = r.kpis()
	v.Chart = r.chartPanel()
	v.Tabs = r.tabs()
	return v
}

// Mode reports what kind of run this log came from. It is the first thing the
// page has to say, because it is the difference between money and a rehearsal.
func (r Report) Mode() string {
	if r.IsLive() {
		return "live"
	}
	if len(r.Paper) > 0 {
		return "paper"
	}
	if r.RunMode != "" {
		return r.RunMode
	}
	return "shadow"
}

func (r Report) warnings() []string {
	var out []string
	if r.IsLive() {
		out = append(out, "本物の注文が出ています。下の金額は実際のお金です。")
	}
	// The cadence is an input, not a measurement, and every "expected" count on
	// this page hangs off it. Say so when the data disagrees, rather than
	// reporting a healthy run as a broken one.
	if o, want := r.ObservedTick, r.Options.TickInterval; o > 0 && want > 0 {
		if ratio := o.Seconds() / want.Seconds(); ratio < 0.75 || ratio > 1.33 {
			out = append(out, fmt.Sprintf(
				"評価間隔は%sと指定されていますが、記録は実際には%sおきです。"+
					"-tick を%sにするまで、取得率や想定件数の数字はすべて誤りです。",
				jaDuration(want), jaDuration(o), jaDuration(o)))
		}
	}
	if len(r.Runs) > 1 {
		out = append(out, fmt.Sprintf(
			"この期間に収集プロセスが%d回起動しています（途中で再起動した）。"+
				"再起動をまたいで比較する前に runs-*.jsonl の build_revision を確認してください。", len(r.Runs)))
	}
	if len(r.Models) > 1 {
		out = append(out, fmt.Sprintf(
			"この期間に%d種類のモデルバージョンが混ざっています（%s）。前後の記録は比較できません。",
			len(r.Models), strings.Join(r.Models, ", ")))
	}
	return out
}

// kpis is the strip across the top: the few numbers worth reading first.
func (r Report) kpis() []kpi {
	var out []kpi

	// Money leads, when there is money. Otherwise the run's own health does.
	if len(r.Paper) > 0 {
		out = append(out, r.moneyKPIs()...)
	}

	density := "good"
	densityNote := "1秒ごとに記録できた割合"
	if r.Density < 0.95 || r.Density > 1.05 {
		density = "critical"
		densityNote = "取りこぼしか、評価間隔の指定違いです"
	}
	fails := "good"
	if r.FailRate > 0.02 {
		fails = "critical"
	}

	out = append(out,
		kpi{Label: "記録件数", Value: human(r.Records),
			Note: round(r.To.Sub(r.From)) + "ぶん",
			Tip:  "モデルに1回聞くごとに1件。失敗した呼び出しも記録されます"},
		kpi{Label: "ティック取得率", Value: pct(r.Density), Note: densityNote, Status: density,
			Tip: "この期間に本来あるべき件数に対して、実際に何件書けたか。100%が正常です"},
		kpi{Label: "API失敗率", Value: fmt.Sprintf("%.2f%%", r.FailRate*100),
			Note: fmt.Sprintf("%d件 / %d件", r.Failed, r.Records), Status: fails,
			Tip: "モデルへの呼び出しが失敗した割合。失敗した回は判断ができていません"},
	)
	// Latency and API cost matter, but not before the money and the health do.
	// They live with the rest of the collection numbers, in that tab.
	if len(r.Paper) == 0 {
		out = append(out,
			kpi{Label: "応答時間", Value: fmt.Sprintf("%.0f ms", r.Latency50),
				Note: fmt.Sprintf("遅い方から1%%が %.0f ms", r.Latency99),
				Tip:  "モデルが答えるまでの時間。2秒を超えた回答は古すぎるとして捨てられます"},
			kpi{Label: "APIコスト", Value: fmt.Sprintf("$%.2f", r.CostUSD),
				Note: fmt.Sprintf("この間隔なら1日 $%.2f", r.CostPerDay),
				Tip:  "TypeSafe への支払い。取引の損益とは別物です"})
	}
	return out
}

// moneyKPIs are the execution numbers, and they go first.
func (r Report) moneyKPIs() []kpi {
	var out []kpi
	live := r.IsLive()

	var net, fees float64
	var trips, wins, working int
	var pos string
	var brake, brakeWhy string
	for _, p := range r.Paper {
		net += p.NetJPY + p.UnrealJPY
		fees += p.FeesJPY
		trips += p.Trips
		wins += p.Wins
		working += p.Working
		if p.Size > 0 && pos == "" {
			pos = fmt.Sprintf("買い %.4f @ %.3f", p.Size, p.EntryPx)
		}
		if live {
			brake, brakeWhy = verdictLabelJA(p.Intent), p.Gate
		}
	}
	if pos == "" {
		pos = "なし"
	}

	status := "good"
	if net < 0 {
		status = "critical"
	}
	label := "模擬損益（円）"
	note := fmt.Sprintf("%d回の売買、うち%d回プラス", trips, wins)
	if live {
		label = "損益（円）"
		note = fmt.Sprintf("%d回の売買、うち%d回プラス。手数料込み", trips, wins)
	}
	out = append(out, kpi{
		Label: label, Value: fmt.Sprintf("%+.2f", net), Note: note,
		Status: status, Hero: true,
		Tip: "確定した損益と含み損益の合計、手数料を引いたあとの値です",
	})

	posNote := "建玉なし"
	if working > 0 {
		posNote = fmt.Sprintf("注文が%d件出ています", working)
	} else if pos != "なし" {
		posNote = "持っている状態です"
	}
	out = append(out, kpi{
		Label: "いまの建玉", Value: pos, Note: posNote,
		Tip: "今この瞬間、いくら持っているか。「なし」なら何も持っていません",
	})

	out = append(out, kpi{
		Label: "支払った手数料（円）", Value: fmt.Sprintf("%+.2f", fees),
		Note: "売買1往復ごとに必ず出ていく分",
		Tip:  "成行は往復で約24bps（3000円なら約7.2円）。指値はリベートなのでマイナスになります",
	})

	if live {
		st := "good"
		if brake != "取引可" {
			st = "warning"
		}
		if brake == "全停止" {
			st = "critical"
		}
		if brakeWhy == "" {
			brakeWhy = "上限には触れていません"
		}
		out = append(out, kpi{
			Label: "ブレーキ", Value: brake, Note: brakeWhy, Status: st,
			Tip: "安全装置の判定。「決済のみ」なら新規は止めていますが、持っているものは出せます",
		})
	}
	return out
}

func jaDuration(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return fmt.Sprintf("%.0f分", d.Minutes())
	case d >= time.Second:
		return fmt.Sprintf("%.0f秒", d.Seconds())
	}
	return d.String()
}

func perCall(r Report) float64 {
	answered := r.Records - r.Failed
	if answered == 0 {
		return 0
	}
	return float64(r.InputTokens) / float64(answered)
}

func (r Report) chartPanel() panel {
	cadence := r.ObservedTick
	if cadence <= 0 {
		cadence = r.Options.TickInterval
	}

	wanted, filled, held, failed := 0, 0, 0, 0
	for _, t := range r.Recent {
		if t.Wanted {
			wanted++
		}
		if t.Filled {
			filled++
		}
		if t.Held {
			held++
		}
		if t.Error != "" {
			failed++
		}
	}

	note := fmt.Sprintf("直近%d回ぶんの値動きです。点ひとつが1回の評価で、実際の時刻の位置に置いてあるので、"+
		"点の間隔がそのまま%sの評価間隔になります。間隔が空いていれば、そこは評価できなかった回です。",
		len(r.Recent), jaDuration(cadence))
	if first, last := firstLastPrice(r.Recent); first > 0 && last > 0 {
		swing := 0.0
		if r.PriceMin > 0 {
			swing = (r.PriceMax - r.PriceMin) / r.PriceMin * 10000
		}
		note += fmt.Sprintf(" 価格は%s → %s（%s bps、この間の高値と安値の幅は%.1f bps）。",
			priceStr(first, r.PriceMax-r.PriceMin), priceStr(last, r.PriceMax-r.PriceMin),
			bpsStr((last-first)/first*10000), swing)
		// The y-axis is floored at the round-trip cost, so say what that means.
		// Otherwise a flat line reads as a broken chart rather than as a market
		// that did not move far enough to be worth trading.
		if cost := r.Options.BandBps; cost > 0 {
			note += fmt.Sprintf("縦軸は最低でも往復手数料%.0f bpsぶんの幅を取ってあるので、"+
				"線が平らなら本当に動いていないという意味です", cost)
			if swing > 0 && swing < cost {
				note += fmt.Sprintf("——この間の値幅は手数料の%.0f%%しかなく、"+
					"どんなに予測が当たっても往復では足が出ます", swing/cost*100)
			}
			note += "。"
		}
	}
	switch {
	case wanted == 0:
		note += " この間、モデルは一度も売買を求めていません（ずっと「待ち」）。"
	case len(r.Paper) > 0:
		note += fmt.Sprintf(" うち%d回はモデルが売買を求めました（○印）。実際にどうなったかは右の「いまの状況」に出ています。", wanted)
	default:
		note += fmt.Sprintf(" うち%d回はモデルが売買を求めました（○印）。"+
			"ただし影運転なので注文は一切出していません。止めた理由は右の一覧にあります。", wanted)
	}
	if filled > 0 || held > 0 {
		note += fmt.Sprintf(" ◆が実際に約定した回（%d回）、下の帯が建玉を持っていた区間（%d回ぶん）です。", filled, held)
	}
	if failed > 0 {
		note += fmt.Sprintf(" %d回は呼び出しに失敗していて、判断がありません。", failed)
	}

	tape := r.tapeTab()
	return panel{
		Title:      "値動きと売買（1回ごと）",
		Note:       note,
		SVG:        svgPrice(r.Recent, r.PriceMin, r.PriceMax, r.Options.BandBps),
		Extra:      chartLegend(wanted > 0, filled > 0 || held > 0),
		BelowTitle: tape.Name + " — " + tape.Note,
		Below:      tape.HTML,
	}
}

// chartLegend names the marks in words. Shape carries the meaning, so the
// legend shows the shape rather than a colour swatch.
func chartLegend(calls, fills bool) template.HTML {
	var b strings.Builder
	b.WriteString(`<div class="legend">`)
	b.WriteString(`<span><svg width="14" height="14" viewBox="0 0 14 14"><circle cx="7" cy="7" r="2.4" class="tickdot"/></svg>1回の評価</span>`)
	if calls {
		b.WriteString(`<span><svg width="14" height="14" viewBox="0 0 14 14"><circle cx="7" cy="7" r="5" class="callring"/></svg>モデルが売買を求めた</span>`)
	}
	if fills {
		b.WriteString(`<span><svg width="14" height="14" viewBox="0 0 14 14"><path d="M 7 2 l 4.5 5 l -4.5 5 l -4.5 -5 Z" class="fillmark"/></svg>約定した</span>`)
		b.WriteString(`<span><svg width="18" height="14" viewBox="0 0 18 14"><rect x="1" y="5" width="16" height="5" class="heldband"/></svg>建玉を持っていた区間</span>`)
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

// tabs are the right-hand panels. "Now" first: it answers the question
// somebody opening this page actually has.
func (r Report) tabs() []tab {
	var out []tab
	// The first tab is whichever one answers the question a person opening
	// this page actually has. With execution that is "what did it do"; without
	// it there is nothing to show but what stopped every tick, so that leads.
	if len(r.Paper) > 0 {
		out = append(out, r.nowTab())
	}
	out = append(out, r.whyTab(), r.answersTab(), r.healthTab())
	if r.HasOutcomes {
		out = append(out, r.calibrationTab())
	}
	return out
}

// nowTab is the execution standing. It is only built when there is execution
// to report; see tabs.
func (r Report) nowTab() tab {
	var b strings.Builder
	note := "同じ判断を2通りの出し方で約定させた結果です。手数料を引く前と後を並べてあります。"
	if r.IsLive() {
		note = "bitbank に実際に出した注文の結果です。手数料を引く前と後を並べてあります。"
	}

	b.WriteString(`<div class="rows">`)
	for _, p := range r.Paper {
		st := lookup(styleJA, p.Style)
		pos := "建玉なし"
		if p.Size > 0 {
			pos = fmt.Sprintf("買い %.4f @ %.3f", p.Size, p.EntryPx)
		}
		if p.Working > 0 {
			pos += fmt.Sprintf("（注文%d件）", p.Working)
		}
		fmt.Fprintf(&b, `<div style="margin-bottom:10px"><div class="nm">%s<small>%s</small></div>`,
			template.HTMLEscapeString(st.Label), template.HTMLEscapeString(st.Why))
		fmt.Fprintf(&b, `<table><tbody>`)
		rows := [][2]string{
			{"いまの建玉", pos},
			{"売買した回数", fmt.Sprintf("%d回（うちプラス %d回）", p.Trips, p.Wins)},
			{"手数料を引く前", jpy(p.GrossJPY)},
			{"支払った手数料", jpy(p.FeesJPY)},
			{"手数料を引いた後", jpy(p.NetJPY)},
			{"含み損益", jpy(p.UnrealJPY)},
		}
		if r.IsLive() {
			brake := verdictLabelJA(p.Intent)
			if p.Gate != "" {
				brake += "（" + p.Gate + "）"
			}
			rows = append([][2]string{{"ブレーキ", brake}}, rows...)
		}
		for _, kv := range rows {
			fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td></tr>`,
				template.HTMLEscapeString(kv[0]), template.HTMLEscapeString(kv[1]))
		}
		b.WriteString(`</tbody></table></div>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(string(svgPaths(r.Paper)))
	return tab{Name: "いまの状況", Note: note, HTML: template.HTML(b.String())}
}

// whyTab is the most useful panel on the page: what stopped each evaluation
// from becoming a trade, in words, ordered by how often it happened.
func (r Report) whyTab() tab {
	if len(r.Gates) == 0 {
		return tab{Name: "止めた理由", HTML: `<p class="empty">まだ判断がありません。</p>`}
	}
	max := r.Gates[0].N
	var b strings.Builder
	b.WriteString(`<div class="rows">`)
	for _, c := range r.Gates {
		t := lookup(gateJA, c.Label)
		w := 0.0
		if max > 0 {
			w = float64(c.N) / float64(max) * 100
		}
		fmt.Fprintf(&b, `<div class="row" tabindex="0" data-tip="%s ・ %d回（%s）">`+
			`<div class="nm">%s<small>%s</small></div>`+
			`<div class="track"><i style="width:%.1f%%"></i></div>`+
			`<div class="qt">%d回 %s</div></div>`,
			template.HTMLEscapeString(t.Why), c.N, pct(c.Share),
			template.HTMLEscapeString(t.Label), template.HTMLEscapeString(t.Why),
			w, c.N, pct(c.Share))
	}
	b.WriteString(`</div>`)
	note := "1回の評価ごとに、必ずどれか1つが記録されます。「通過」以外は、何かが取引を止めたということです。"
	if len(r.Paper) == 0 {
		note += "この運転では注文を出していないので（影運転）、これはあくまで「もし取引していたら」の記録です。" +
			"実際の約定まで見たい場合は make paper（模擬）か make live（実取引）で起動してください。"
	}
	return tab{Name: "止めた理由", Note: note, HTML: template.HTML(b.String())}
}

// tapeTab is the raw sequence, newest first.
func (r Report) tapeTab() tab {
	if len(r.Recent) == 0 {
		return tab{Name: "直近の記録", HTML: `<p class="empty">まだ記録がありません。</p>`}
	}
	span := r.PriceMax - r.PriceMin
	tape, base := r.Recent, 0
	if len(tape) > tapeRows {
		base = len(tape) - tapeRows
		tape = tape[base:]
	}

	var b strings.Builder
	b.WriteString(`<table><thead><tr><th>時刻(UTC)</th><th>価格</th><th>前回比(bps)</th>` +
		`<th>モデルの判断</th><th>確率</th><th>止めた理由</th></tr></thead><tbody>`)
	for i := len(tape) - 1; i >= 0; i-- {
		t := tape[i]
		called := "—"
		if t.Error != "" {
			called = "失敗"
		} else if t.Action != "" {
			called = lookup(actionJA, t.Action).Label
		}
		prob := "—"
		if t.Prob > 0 {
			prob = fmt.Sprintf("%.2f", t.Prob)
		}
		delta := "—"
		if t.Price > 0 && base+i > 0 {
			delta = bpsStr(t.DeltaBps)
		}
		cls := ""
		if t.Filled {
			cls = ` class="act"`
		}
		fmt.Fprintf(&b, `<tr%s><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			cls, t.At.Format("15:04:05"), priceStr(t.Price, span), delta,
			template.HTMLEscapeString(called), prob,
			template.HTMLEscapeString(gateLabelJA(t.Gate)))
	}
	b.WriteString(`</tbody></table>`)
	return tab{
		Name: "直近の記録",
		Note: fmt.Sprintf("新しい順に%d件。太字の行は実際に約定した回です。", len(tape)),
		HTML: template.HTML(b.String()),
	}
}

// answersTab shows where each answer actually sits against the threshold that
// reads it — the thing a bare histogram cannot say.
func (r Report) answersTab() tab {
	if len(r.Dists) == 0 {
		return tab{Name: "モデルの回答", HTML: `<p class="empty">まだ回答がありません。</p>`}
	}
	var b strings.Builder
	for _, d := range r.Dists {
		t := lookup(questionJA, d.Question)
		line := fmt.Sprintf("%d件、中央値%.2f。", d.N, d.Median)
		if d.Gate > 0 {
			side := "超えて"
			if !d.GateAbove {
				side = "下回って"
			}
			line += fmt.Sprintf("しきい値%.2fを%d件（%.0f%%）が%sいます。",
				d.Gate, d.OverGate, float64(d.OverGate)/float64(d.N)*100, side)
		}
		fmt.Fprintf(&b, `<div style="margin-bottom:12px"><div class="nm">%s <small>%s</small></div>`+
			`<p class="note" style="margin:2px 0 4px">%s</p>%s</div>`,
			template.HTMLEscapeString(t.Label), template.HTMLEscapeString(d.Question),
			template.HTMLEscapeString(t.Why+" "+line), svgHist(d))
	}
	return tab{
		Name: "モデルの回答",
		Note: "点線がしきい値です。回答がしきい値の向こう側に寄っているなら、そのゲートはほぼ常に効いています。",
		HTML: template.HTML(b.String()),
	}
}

// healthTab is whether the collection itself is usable.
func (r Report) healthTab() tab {
	var b strings.Builder
	fmt.Fprintf(&b, `<p class="note">応答時間 中央値 %.0f ms（遅い方から1%%が %.0f ms）。`+
		`APIコスト $%.2f、この間隔なら1日 $%.2f。`+
		`入力トークン %s（1回あたり %.0f）。モデル %s。</p>`,
		r.Latency50, r.Latency99, r.CostUSD, r.CostPerDay,
		human(r.InputTokens), perCall(r), template.HTMLEscapeString(strings.Join(r.Models, ", ")))
	b.WriteString(string(svgTimeline(r.Timeline)))
	b.WriteString(`<table><thead><tr><th>開始</th><th>記録</th><th>想定</th><th>取得率</th><th>失敗</th></tr></thead><tbody>`)
	for _, bk := range r.Timeline {
		fmt.Fprintf(&b, `<tr><td>%s</td><td>%d</td><td>%d</td><td>%s</td><td>%d</td></tr>`,
			bk.At.Format("01/02 15:04"), bk.Records, bk.Expected, pct(bk.Density), bk.Errors)
	}
	b.WriteString(`</tbody></table>`)
	return tab{
		Name: "収集の健康",
		Note: "時間帯ごとに、本来あるべき件数のうち何件書けたか。へこみは取りこぼし、空白は収集が止まっていた時間です。",
		HTML: template.HTML(b.String()),
	}
}

// calibrationTab is the deliverable of the whole experiment.
func (r Report) calibrationTab() tab {
	c := r.Calibration
	note := fmt.Sprintf("%d秒後の結果と照合。使えた記録%d件 / %d件。"+
		"Brier %.4f、較正誤差 %.4f。対角線より上は「言ったより実際に起きた」、下は「言ったほど起きなかった」です。",
		c.Options.HorizonSec, c.Usable, c.Total, c.Brier, c.ECE)

	var b strings.Builder
	b.WriteString(string(svgReliability(c)))
	b.WriteString(`<table><thead><tr><th>予測確率</th><th>件数</th><th>モデルの言い値</th>` +
		`<th>実際の頻度</th><th>ずれ</th></tr></thead><tbody>`)
	for _, bin := range c.Bins {
		if bin.N == 0 {
			fmt.Fprintf(&b, `<tr><td>%.1f–%.1f</td><td>0</td><td>—</td><td>—</td><td>—</td></tr>`, bin.Lo, bin.Hi)
			continue
		}
		fmt.Fprintf(&b, `<tr><td>%.1f–%.1f</td><td>%d</td><td>%.3f</td><td>%.3f</td><td>%+.3f</td></tr>`,
			bin.Lo, bin.Hi, bin.N, bin.MeanPredicted, bin.Realised, bin.Gap())
	}
	b.WriteString(`</tbody></table>`)
	return tab{Name: "較正", Note: note, HTML: template.HTML(b.String())}
}

// tapeRows is how much of the tail the tape prints. The chart covers the whole
// recent window; the table is for reading, and a hundred rows is not.
const tapeRows = 40

func firstLastPrice(ticks []Tick) (first, last float64) {
	for _, t := range ticks {
		if t.Price <= 0 {
			continue
		}
		if first == 0 {
			first = t.Price
		}
		last = t.Price
	}
	return first, last
}

// bpsStr signs a move, but never signs one too small to have a direction at
// the precision shown: "%+.1f" renders a two-hundredth of a basis point as
// "-0.0", which reads as a fall that did not happen.
func bpsStr(v float64) string {
	if math.Abs(v) < 0.05 {
		return "0.0"
	}
	return fmt.Sprintf("%+.1f", v)
}

// priceStr picks its decimals from the range on screen rather than the pair, so
// xrp_jpy at 223.041 and btc_jpy at 15,400,000 both read correctly without the
// renderer knowing which is which.
func priceStr(v, span float64) string {
	if v <= 0 {
		return "—"
	}
	d := 0
	switch {
	case span < 0.01:
		d = 4
	case span < 1:
		d = 3
	case span < 10:
		d = 2
	case span < 1000:
		d = 1
	}
	return fmt.Sprintf("%.*f", d, v)
}

// --- svg --------------------------------------------------------------------

const (
	chartW = 720.0
	chartH = 240.0
	// Wide enough for a seven-digit price label at 11px. It used to be 44,
	// which silently rendered "222.803" as "22.803" once the panel clipped it
	// — a wrong number, not a cosmetic one.
	padL = 58.0
	padR = 16.0
	padT = 14.0
	padB = 30.0
)

func svgOpen(w, h float64) *strings.Builder {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" preserveAspectRatio="xMidYMid meet" role="img" class="chart">`, w, h)
	return &b
}

// gridlines and the baseline, both recessive.
func axes(b *strings.Builder, w, h float64, labels []string) {
	for i := 0; i <= 4; i++ {
		y := padT + (h-padT-padB)*float64(i)/4
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid"/>`, padL, y, w-padR, y)
		if i < len(labels) {
			fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%s</text>`,
				padL-8, y+4, template.HTMLEscapeString(labels[i]))
		}
	}
	fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="baseline"/>`,
		padL, h-padB, w-padR, h-padB)
}

func svgTimeline(buckets []Bucket) template.HTML {
	if len(buckets) == 0 {
		return ""
	}
	b := svgOpen(chartW, chartH)
	axes(b, chartW, chartH, []string{"100%", "75%", "50%", "25%", "0"})

	plotW := chartW - padL - padR
	plotH := chartH - padT - padB
	step := plotW / float64(len(buckets))
	barW := math.Max(2, step-2) // a 2px surface gap between fills

	for i, bk := range buckets {
		x := padL + float64(i)*step
		bh := plotH * bk.Density
		if bh < 1 && bk.Records > 0 {
			bh = 1
		}
		y := padT + plotH - bh
		fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2" class="mark" `+
			`tabindex="0" data-tip="%s ・ %d件 / 想定%d件 ・ %s"><title>%s %d件 / %d件（%s）</title></rect>`,
			x, y, barW, math.Max(bh, 0),
			template.HTMLEscapeString(bk.At.Format("01-02 15:04")), bk.Records, bk.Expected, pct(bk.Density),
			template.HTMLEscapeString(bk.At.Format("01-02 15:04")), bk.Records, bk.Expected, pct(bk.Density))
	}
	// First and last time, directly labelled rather than a full axis.
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis">%s</text>`,
		padL, chartH-10, template.HTMLEscapeString(buckets[0].At.Format("01-02 15:04")))
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%s</text>`,
		chartW-padR, chartH-10, template.HTMLEscapeString(buckets[len(buckets)-1].At.Format("15:04")))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func svgHist(d Dist) template.HTML {
	if len(d.Bins) == 0 {
		return ""
	}
	h := 206.0
	b := svgOpen(chartW, h)

	maxShare := 0.0
	for _, bin := range d.Bins {
		if bin.Share > maxShare {
			maxShare = bin.Share
		}
	}
	axes(b, chartW, h, []string{pct(maxShare), "", "", "", "0"})

	plotW := chartW - padL - padR
	plotH := h - padT - padB
	step := plotW / float64(len(d.Bins))
	barW := math.Max(2, step-2)

	for i, bin := range d.Bins {
		x := padL + float64(i)*step
		bh := 0.0
		if maxShare > 0 {
			bh = plotH * bin.Share / maxShare
		}
		fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2" class="mark" `+
			`tabindex="0" data-tip="%.2f〜%.2f ・ %d件 ・ %s"><title>%.2f〜%.2f %d件（%s）</title></rect>`,
			x, padT+plotH-bh, barW, bh,
			bin.Lo, bin.Hi, bin.N, pct(bin.Share),
			bin.Lo, bin.Hi, bin.N, pct(bin.Share))
	}

	// The gate is a reference, not a series: a dashed neutral rule, never a hue
	// that could be mistaken for data. It is drawn over the bars, so it carries
	// a surface-coloured casing to stay legible against them, and its label
	// sits in the top padding where no mark can reach it.
	if d.Gate > 0 && d.Max > 0 {
		x := padL + plotW*d.Gate/d.Max
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="refcasing"/>`, x, padT, x, padT+plotH)
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="ref"/>`, x, padT, x, padT+plotH)

		anchor, dx := "start", 6.0
		if x > chartW*0.65 {
			anchor, dx = "end", -6.0
		}
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="reflabel" text-anchor="%s">%s %.2f</text>`,
			x+dx, padT-3, anchor, template.HTMLEscapeString(d.GateLabel), d.Gate)
	}

	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis">0</text>`, padL, h-10)
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%.0f</text>`, chartW-padR, h-10, d.Max)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// svgReliability is the headline chart: what the model said against what
// happened. The diagonal is perfect calibration — a reference, drawn in neutral
// ink, never a series.
//
// It uses the same viewBox width as every other chart on the page. That is not
// cosmetic: the charts are sized by CSS at 100% width, so a narrower viewBox
// scales up more, and a square 360-wide canvas rendered type at twice the size
// of its neighbours and overflowed its own frame.
func svgReliability(c calib.Report) template.HTML {
	h := 340.0
	plot := h - padT - padB - 16 // square, and it sets the height budget
	left := (chartW - plot) / 2  // centred, so the page rhythm holds
	top := padT + 8

	b := svgOpen(chartW, h)
	for i := 0; i <= 4; i++ {
		f := float64(i) / 4
		y := top + plot*f
		x := left + plot*f
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid"/>`, left, y, left+plot, y)
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%.2f</text>`, left-8, y+4, 1-f)
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="middle">%.1f</text>`, x, top+plot+16, f)
	}

	fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="ref"/>`,
		left, top+plot, left+plot, top)
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="reflabel" text-anchor="start">完全に較正された線</text>`,
		left+plot+8, top+4)

	maxN := 0
	for _, bin := range c.Bins {
		if bin.N > maxN {
			maxN = bin.N
		}
	}

	var path []string
	for _, bin := range c.Bins {
		if bin.N == 0 {
			continue
		}
		path = append(path, fmt.Sprintf("%.1f,%.1f",
			left+plot*bin.MeanPredicted, top+plot*(1-bin.Realised)))
	}
	if len(path) > 1 {
		fmt.Fprintf(b, `<polyline points="%s" class="line"/>`, strings.Join(path, " "))
	}
	for _, bin := range c.Bins {
		if bin.N == 0 {
			continue
		}
		rad := 5.0
		if maxN > 0 {
			rad = 4 + 6*math.Sqrt(float64(bin.N)/float64(maxN))
		}
		fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="%.1f" class="dot" tabindex="0" `+
			`data-tip="stated %.3f · realised %.3f · n=%d · gap %+.3f">`+
			`<title>stated %.3f, realised %.3f, n=%d (gap %+.3f)</title></circle>`,
			left+plot*bin.MeanPredicted, top+plot*(1-bin.Realised), rad,
			bin.MeanPredicted, bin.Realised, bin.N, bin.Gap(),
			bin.MeanPredicted, bin.Realised, bin.N, bin.Gap())
	}

	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="middle">モデルの言い値</text>`,
		left+plot/2, h-6)
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="middle" transform="rotate(-90 %.1f %.1f)">実際</text>`,
		left-46, top+plot/2, left-46, top+plot/2)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// svgPrice draws the tail of the log as a price line with one dot per
// evaluation, each placed at the time it actually happened.
//
// Placing by time rather than by index is the whole point of the chart: at an
// even cadence the dots are evenly spaced, so the cadence is legible by
// construction and a skipped tick shows up as a gap in the rhythm instead of
// being quietly closed up.
//
// Ticks where the model asked for an entry or an exit are marked with a rule
// and a ring. Both are shape, never hue: this page's two status colours are
// four Delta E apart under deuteranopia and may not carry meaning alone.
func svgPrice(ticks []Tick, lo, hi, costBps float64) template.HTML {
	if len(ticks) == 0 || hi <= 0 {
		return ""
	}
	h := 220.0
	b := svgOpen(chartW, h)

	plotW := chartW - padL - padR
	plotH := h - padT - padB

	// The y-axis never zooms tighter than the round-trip cost.
	//
	// Auto-scaling to whatever happened is what makes a dead market look like
	// a mountain range: xrp_jpy moved 0.45bps over two minutes on 2026-09-19,
	// and a frame fitted to that magnifies single ticks into cliffs. The floor
	// is the cost of a round trip because a move smaller than that could not
	// have been traded profitably however well it was predicted — so a chart
	// that makes it look large is lying about the only thing that matters.
	mid := (lo + hi) / 2
	span := hi - lo
	if floor := mid * costBps / 10000; span < floor {
		span = floor
	}
	if span <= 0 {
		span = math.Max(mid*0.0001, 0.0001)
	}
	span *= 1.15 // breathing room above and below
	lo, hi = mid-span/2, mid+span/2

	y := func(p float64) float64 { return padT + plotH*(hi-p)/span }

	t0, tn := ticks[0].At, ticks[len(ticks)-1].At
	width := tn.Sub(t0).Seconds()
	x := func(i int) float64 {
		if width <= 0 {
			if len(ticks) == 1 {
				return padL + plotW/2
			}
			return padL + plotW*float64(i)/float64(len(ticks)-1)
		}
		return padL + plotW*ticks[i].At.Sub(t0).Seconds()/width
	}

	// Gridlines with the price they stand for, top to bottom.
	for i := 0; i <= 4; i++ {
		gy := padT + plotH*float64(i)/4
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid"/>`, padL, gy, chartW-padR, gy)
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%s</text>`,
			padL-8, gy+4, priceStr(hi-span*float64(i)/4, span))
	}

	// A band along the foot of the plot for the stretches a position was
	// actually held. It sits under everything: it is context for the line, not
	// a series of its own.
	for i, t := range ticks {
		if !t.Held {
			continue
		}
		w := plotW / math.Max(float64(len(ticks)-1), 1)
		if width > 0 && i+1 < len(ticks) {
			w = x(i+1) - x(i)
		}
		fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="6" class="heldband"/>`,
			x(i), padT+plotH-6, math.Max(w, 1))
	}

	// The opening price, as a reference. With a fixed minimum span the line's
	// distance from it is directly readable as "did this move enough to pay
	// for itself".
	if first, _ := firstLastPrice(ticks); first > 0 {
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="ref"/>`,
			padL, y(first), chartW-padR, y(first))
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="reflabel" text-anchor="end">`+
			`この画面の高さ = 往復手数料 %.0fbps ぶん</text>`, chartW-padR, padT-3, costBps)
	}

	// The call rules go down first so the price line reads over them.
	for i, t := range ticks {
		if !t.Wanted {
			continue
		}
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="callrule"/>`,
			x(i), padT, x(i), padT+plotH)
	}

	var pts []string
	for i, t := range ticks {
		if t.Price <= 0 {
			continue
		}
		pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(i), y(t.Price)))
	}
	if len(pts) > 1 {
		fmt.Fprintf(b, `<polyline points="%s" class="line"/>`, strings.Join(pts, " "))
	}

	for i, t := range ticks {
		if t.Price <= 0 {
			continue
		}
		tip := fmt.Sprintf("%s ・ %s ・ 前回比 %s bps",
			t.At.Format("15:04:05"), priceStr(t.Price, span), bpsStr(t.DeltaBps))
		if t.Error != "" {
			tip += " ・ 呼び出し失敗"
		} else if t.Action != "" {
			tip += fmt.Sprintf(" ・ %s（確率 %.2f）・ %s",
				lookup(actionJA, t.Action).Label, t.Prob, gateLabelJA(t.Gate))
		}
		fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="2.4" class="tickdot" tabindex="0" data-tip="%s">`+
			`<title>%s</title></circle>`,
			x(i), y(t.Price), template.HTMLEscapeString(tip), template.HTMLEscapeString(tip))
		if t.Wanted {
			fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="5.5" class="callring"/>`, x(i), y(t.Price))
		}
		// A fill is a different event from a call, so it gets a different
		// shape: a filled diamond, legible without colour beside a ring.
		if t.Filled {
			fmt.Fprintf(b, `<path d="M %.1f %.1f l 5 5 l -5 5 l -5 -5 Z" class="fillmark"><title>%s</title></path>`,
				x(i), y(t.Price)-5, template.HTMLEscapeString(t.At.Format("15:04:05")+" に約定"))
		}
	}

	fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="baseline"/>`,
		padL, padT+plotH, chartW-padR, padT+plotH)
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis">%s</text>`,
		padL, h-10, template.HTMLEscapeString(t0.Format("15:04:05")))
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="middle">%d回ぶん</text>`,
		padL+plotW/2, h-10, len(ticks))
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%s</text>`,
		chartW-padR, h-10, template.HTMLEscapeString(tn.Format("15:04:05")))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// svgPaths draws gross and net per path as a paired bar, zero-centred.
//
// The pair is the message: a gross bar well above zero beside a net bar below
// it is a strategy that was right and still lost, which is the specific
// failure CLAUDE.md predicts for anything taking liquidity at 24bps a round
// trip. Both bars use the one series hue; they are told apart by position and
// by their labels, never by colour.
func svgPaths(paths []Path) template.HTML {
	if len(paths) == 0 {
		return ""
	}
	rowH, barH := 54.0, 16.0
	h := padT + float64(len(paths))*rowH + 26
	labelW := 92.0
	b := svgOpen(chartW, h)

	span := 0.0
	for _, p := range paths {
		span = math.Max(span, math.Max(math.Abs(p.GrossJPY), math.Abs(p.NetJPY)))
	}
	if span <= 0 {
		span = 1 // nothing has happened yet; draw the axis, not a divide by zero
	}
	// A wide right gutter: the value labels sit outside the plot and
	// "gross -10.22 JPY" is ~95px of type at this size.
	plotW := chartW - labelW - padR - 130
	zero := labelW + plotW/2

	for i, p := range paths {
		y := padT + float64(i)*rowH
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="label" text-anchor="end">%s</text>`,
			labelW-10, y+22, template.HTMLEscapeString(styleLabelJA(p.Style)))

		for j, v := range []struct {
			name string
			val  float64
		}{{"手数料前", p.GrossJPY}, {"手数料後", p.NetJPY}} {
			by := y + float64(j)*(barH+4)
			w := plotW / 2 * math.Abs(v.val) / span
			x := zero
			if v.val < 0 {
				x = zero - w
			}
			tip := fmt.Sprintf("%s ・ %s %s", styleLabelJA(p.Style), v.name, jpy(v.val))
			fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2" class="mark" `+
				`tabindex="0" data-tip="%s"><title>%s</title></rect>`,
				x, by, math.Max(w, 1.5), barH, template.HTMLEscapeString(tip), template.HTMLEscapeString(tip))
			fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="value">%s %s</text>`,
				zero+plotW/2+8, by+12, template.HTMLEscapeString(v.name), jpy(v.val))
		}
	}

	// The zero line is a reference, not a series.
	fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="ref"/>`,
		zero, padT-4, zero, h-22)
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="reflabel" text-anchor="middle">損益ゼロ</text>`,
		zero, h-8)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
