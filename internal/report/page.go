package report

import (
	"fmt"
	"html/template"
	"math"
	"time"
)

func pct(f float64) string { return fmt.Sprintf("%.1f%%", f*100) }

func human(n int) string {
	s := fmt.Sprint(n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// jpy formats yen with a sign, because every number it renders is a gain or a
// loss and an unsigned one reads as neither.
func jpy(v float64) string {
	if math.Abs(v) < 0.005 {
		return "0.00 円"
	}
	return fmt.Sprintf("%+.2f 円", v)
}

func round(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%.0f時間%.0f分", d.Hours(), d.Minutes()-60*math.Floor(d.Hours()))
	}
	if d >= time.Minute {
		return fmt.Sprintf("%.0f分%.0f秒", d.Minutes(), d.Seconds()-60*math.Floor(d.Minutes()))
	}
	return fmt.Sprintf("%.0f秒", d.Seconds())
}

// The palette is the validated reference instance, expressed as roles so the
// light and dark values swap in one place. Dark is declared under both the OS
// media query and the explicit theme attribute, so a viewer's toggle wins
// either way.
//
// Layout note. This page is meant to be read at a glance on a maximised
// browser, so the shell is a viewport-height grid and nothing important sits
// below the fold: the detail lives in tabs rather than further down. Below
// about 760px of viewport height that stops being legible, so the grid is
// released and the page scrolls normally — cramming it would be worse than
// scrolling it.
var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="ja">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}{{with .Subtitle}} — {{.}}{{end}}</title>
<style>
  :root {
    color-scheme: light;
    --plane: #f9f9f7;
    --surface-1: #fcfcfb;
    --surface-2: #f2f1ed;
    --text-primary: #0b0b0b;
    --text-secondary: #52514e;
    --muted: #898781;
    --grid: #e1e0d9;
    --baseline: #c3c2b7;
    --border: rgba(11,11,11,0.10);
    --series-1: #2a78d6;
    --series-2: #eb6834;
    --good: #0ca30c;
    --warning: #fab219;
    --critical: #d03b3b;
  }
  @media (prefers-color-scheme: dark) {
    :root:not([data-theme="light"]) {
      color-scheme: dark;
      --plane: #0d0d0d;
      --surface-1: #1a1a19;
      --surface-2: #232322;
      --text-primary: #ffffff;
      --text-secondary: #c3c2b7;
      --muted: #898781;
      --grid: #2c2c2a;
      --baseline: #383835;
      --border: rgba(255,255,255,0.10);
      --series-1: #3987e5;
      --series-2: #d95926;
    }
  }
  :root[data-theme="dark"] {
    color-scheme: dark;
    --plane: #0d0d0d;
    --surface-1: #1a1a19;
    --surface-2: #232322;
    --text-primary: #ffffff;
    --text-secondary: #c3c2b7;
    --muted: #898781;
    --grid: #2c2c2a;
    --baseline: #383835;
    --border: rgba(255,255,255,0.10);
    --series-1: #3987e5;
    --series-2: #d95926;
  }

  * { box-sizing: border-box; }
  html, body { margin: 0; }
  body {
    background: var(--plane);
    color: var(--text-primary);
    font: 14px/1.6 ui-sans-serif, system-ui, -apple-system, "Hiragino Sans",
          "Hiragino Kaku Gothic ProN", Meiryo, sans-serif;
    -webkit-text-size-adjust: 100%;
    padding: 12px;
  }
  .shell { display: grid; gap: 10px; max-width: 1900px; margin: 0 auto;
           grid-template-rows: auto auto auto minmax(0,1fr); }
  .warns:empty { display: none; }
  .warns { display: grid; gap: 6px; }

  /* At a comfortable height the whole dashboard is one screen: the detail is
     in tabs, not further down. Below that, let it scroll rather than crush. */
  @media (min-height: 760px) {
    body { height: 100dvh; overflow: hidden; }
    .shell { height: calc(100dvh - 24px); }
  }

  /* --- header ---------------------------------------------------------- */
  .top { display: flex; align-items: baseline; flex-wrap: wrap; gap: 8px 16px; }
  h1 { font-size: 17px; margin: 0; letter-spacing: -0.01em; font-weight: 700; }
  h1 span { color: var(--text-secondary); font-weight: 400; }
  .mode { display: inline-flex; align-items: center; gap: 6px; font-weight: 700;
          font-size: 12px; padding: 3px 10px; border-radius: 999px;
          border: 1px solid var(--border); background: var(--surface-1); }
  .mode i { width: 7px; height: 7px; border-radius: 50%; background: var(--muted); }
  .mode.live i { background: var(--critical); animation: pulse 1.6s ease-in-out infinite; }
  .mode.paper i { background: var(--series-1); }
  .mode.shadow i, .mode.observe i { background: var(--good); }
  @keyframes pulse { 0%,100% { opacity: 1 } 50% { opacity: .3 } }
  @media (prefers-reduced-motion: reduce) { .mode.live i { animation: none } }
  .meta { display: flex; flex-wrap: wrap; gap: 4px 14px; color: var(--text-secondary); font-size: 12px; }
  .meta b { color: var(--text-primary); font-weight: 600; font-variant-numeric: tabular-nums; }
  .warn { border: 1px solid var(--border);
          border-left: 3px solid var(--critical); background: var(--surface-1);
          border-radius: 8px; padding: 8px 12px; font-size: 12.5px; color: var(--text-secondary); }
  .warn b { color: var(--text-primary); }

  /* --- KPI strip -------------------------------------------------------- */
  .kpis { display: grid; gap: 8px; grid-template-columns: repeat(auto-fit, minmax(138px, 1fr)); }
  .kpi { background: var(--surface-1); border: 1px solid var(--border);
         border-radius: 10px; padding: 9px 12px; min-width: 0; }
  .kpi .k { font-size: 11px; color: var(--muted); margin-bottom: 1px;
            white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .kpi .v { display: flex; align-items: center; gap: 6px; font-size: 21px;
            font-weight: 700; letter-spacing: -0.02em; font-variant-numeric: tabular-nums;
            line-height: 1.25; }
  .kpi .n { font-size: 11.5px; color: var(--text-secondary); line-height: 1.4;
            margin-top: 1px; }
  /* The hero: the one number the page leads with. */
  .kpi.hero { grid-column: span 2; }
  .kpi.hero .v { font-size: 34px; }
  .dot { flex: 0 0 auto; width: 9px; height: 9px; border-radius: 50%; }
  .dot.good { background: var(--good); }
  .dot.warning { background: var(--warning); }
  .dot.critical { background: var(--critical); }

  /* --- main grid -------------------------------------------------------- */
  .main { display: grid; gap: 10px; grid-template-columns: minmax(0,1.65fr) minmax(0,1fr);
          min-height: 0; }
  @media (max-width: 1080px) { .main { grid-template-columns: minmax(0,1fr); } }
  .panel { background: var(--surface-1); border: 1px solid var(--border);
           border-radius: 10px; padding: 11px 13px; display: flex; flex-direction: column;
           min-height: 0; min-width: 0; }
  .panel > h2 { font-size: 13px; margin: 0 0 2px; font-weight: 700; }
  .panel > p.note { margin: 0 0 8px; color: var(--text-secondary); font-size: 11.5px; line-height: 1.5; }
  .panel > h3 { font-size: 11.5px; font-weight: 600; color: var(--text-secondary);
                margin: 12px 0 0; padding-top: 9px; border-top: 1px solid var(--grid); }
  .body { flex: 1; min-height: 0; overflow: auto; }

  /* --- tabs ------------------------------------------------------------- */
  .tabs { display: flex; gap: 4px; flex-wrap: wrap; margin-bottom: 8px; }
  .tabs button { font: inherit; font-size: 12px; padding: 4px 11px; border-radius: 999px;
                 border: 1px solid var(--border); background: transparent;
                 color: var(--text-secondary); cursor: pointer; }
  .tabs button[aria-selected="true"] { background: var(--text-primary); color: var(--surface-1);
                                       border-color: var(--text-primary); font-weight: 700; }
  .tabpanel[hidden] { display: none; }

  /* --- charts ----------------------------------------------------------- */
  .chart { width: 100%; height: auto; display: block; overflow: visible; }
  .grid { stroke: var(--grid); stroke-width: 1; }
  .baseline { stroke: var(--baseline); stroke-width: 1; }
  .ref { stroke: var(--text-secondary); stroke-width: 1.5; stroke-dasharray: 5 4; }
  .refcasing { stroke: var(--surface-1); stroke-width: 4; }
  .reflabel { fill: var(--muted); font-size: 11px; }
  .axis { fill: var(--muted); font-size: 11px; font-variant-numeric: tabular-nums; }
  .label { fill: var(--text-secondary); font-size: 12px; }
  .value { fill: var(--text-secondary); font-size: 11.5px; font-variant-numeric: tabular-nums; }
  .mark { fill: var(--series-1); }
  .mark.alt { fill: var(--series-2); }
  .line { fill: none; stroke: var(--series-1); stroke-width: 2;
          stroke-linejoin: round; stroke-linecap: round; }
  .dot2 { fill: var(--series-1); stroke: var(--surface-1); stroke-width: 2; }
  .tickdot { fill: var(--series-1); }
  /* A tick the model wanted to act on, and one where something executed.
     Shape, never hue: the two must stay apart with the colour removed. */
  .callrule { stroke: var(--baseline); stroke-width: 1; stroke-dasharray: 3 3; }
  .callring { fill: none; stroke: var(--series-1); stroke-width: 1.5; }
  .fillmark { fill: var(--text-primary); }
  .heldband { fill: var(--series-1); opacity: .22; }
  .mark:hover, .mark:focus, .dot2:hover, .dot2:focus,
  .tickdot:hover, .tickdot:focus { fill: var(--text-primary); outline: none; }

  /* --- rows (the “why” lists) ------------------------------------------- */
  .rows { display: flex; flex-direction: column; gap: 5px; }
  .row { display: grid; grid-template-columns: minmax(96px, 30%) minmax(0,1fr) auto;
         gap: 8px; align-items: center; }
  .nm { font-size: 12.5px; font-weight: 600; }
  .nm small { display: block; font-weight: 400; font-size: 10.5px;
              color: var(--muted); line-height: 1.35; }
  .track { height: 9px; border-radius: 5px; background: var(--surface-2); overflow: hidden; }
  .track > i { display: block; height: 100%; border-radius: 5px; background: var(--series-1); }
  .row .qt { font-size: 12px; color: var(--text-secondary);
             font-variant-numeric: tabular-nums; white-space: nowrap; }

  /* --- tables ----------------------------------------------------------- */
  table { border-collapse: collapse; width: 100%; font-size: 12px; min-width: max-content; }
  th, td { text-align: right; padding: 3px 8px; border-bottom: 1px solid var(--grid);
           font-variant-numeric: tabular-nums; white-space: nowrap; }
  th:first-child, td:first-child { text-align: left; }
  th { color: var(--muted); font-weight: 600; font-size: 11px; position: sticky; top: 0;
       background: var(--surface-1); }
  tr.act td { font-weight: 700; }
  .legend { display: flex; flex-wrap: wrap; gap: 4px 14px; font-size: 11px;
            color: var(--muted); margin-top: 6px; }
  .legend span { display: inline-flex; align-items: center; gap: 5px; }
  .legend svg { overflow: visible; }

  #tip { position: fixed; pointer-events: none; opacity: 0; transition: opacity .1s;
         background: var(--text-primary); color: var(--surface-1);
         font-size: 12px; padding: 5px 9px; border-radius: 6px; z-index: 10;
         font-variant-numeric: tabular-nums; max-width: 280px; line-height: 1.5; }
  .empty { color: var(--text-secondary); font-size: 13px; padding: 20px 0; }
</style>
</head>
<body>
<div class="shell">

  <header class="top">
    <h1>{{.Title}} {{with .Subtitle}}<span>· {{.}}</span>{{end}}</h1>
    <span class="mode {{.ModeClass}}"><i></i>{{.ModeLabel}}</span>
    <span class="meta">{{range .Meta}}<span>{{index . 0}} <b>{{index . 1}}</b></span>{{end}}</span>
  </header>

  <div class="warns">{{range .Warnings}}<div class="warn">{{.}}</div>{{end}}</div>

  {{if .KPIs}}
  <div class="kpis">
    {{range .KPIs}}
    <div class="kpi{{if .Hero}} hero{{end}}"{{with .Tip}} data-tip="{{.}}"{{end}}>
      <div class="k">{{.Label}}</div>
      <div class="v">{{with .Status}}<span class="dot {{.}}"></span>{{end}}{{.Value}}</div>
      <div class="n">{{.Note}}</div>
    </div>
    {{end}}
  </div>
  {{end}}

  <div class="main">
    <section class="panel">
      <h2>{{.Chart.Title}}</h2>
      <p class="note">{{.Chart.Note}}</p>
      <div>{{.Chart.SVG}}{{.Chart.Extra}}</div>
      {{with .Chart.Below}}
      <h3>{{$.Chart.BelowTitle}}</h3>
      <div class="body">{{.}}</div>
      {{end}}
    </section>

    <section class="panel">
      <div class="tabs" role="tablist">
        {{range $i, $t := .Tabs}}
        <button role="tab" id="t{{$i}}" aria-controls="p{{$i}}"
                aria-selected="{{if eq $i 0}}true{{else}}false{{end}}">{{$t.Name}}</button>
        {{end}}
      </div>
      {{range $i, $t := .Tabs}}
      <div class="tabpanel body" role="tabpanel" id="p{{$i}}" aria-labelledby="t{{$i}}"
           {{if ne $i 0}}hidden{{end}}>
        {{with $t.Note}}<p class="note">{{.}}</p>{{end}}
        {{$t.HTML}}
      </div>
      {{end}}
    </section>
  </div>
</div>

<div id="tip" role="status" aria-live="polite"></div>
<script>
{{if .Live}}
// Reload on a timer rather than a socket: the page is a few tens of KB and the
// collector writes every second or so, so polling is simpler and enough. The
// scroll position and the selected tab both survive, because a dashboard that
// resets itself every ten seconds is one nobody keeps open.
(function () {
  try {
    var t = sessionStorage.getItem('jtl-tab');
    if (t !== null) { window.__jtlTab = parseInt(t, 10); }
  } catch (e) {}
  setTimeout(function () { location.reload(); }, 10000);
})();
{{end}}
(function () {
  // Tabs. Plain buttons and hidden panels, so it still reads without JS —
  // every panel is in the document, only the first is shown.
  var tabs = [].slice.call(document.querySelectorAll('[role="tab"]'));
  var panels = [].slice.call(document.querySelectorAll('[role="tabpanel"]'));
  function select(i) {
    tabs.forEach(function (t, j) { t.setAttribute('aria-selected', j === i ? 'true' : 'false'); });
    panels.forEach(function (p, j) { p.hidden = j !== i; });
    try { sessionStorage.setItem('jtl-tab', String(i)); } catch (e) {}
  }
  tabs.forEach(function (t, i) { t.addEventListener('click', function () { select(i); }); });
  if (typeof window.__jtlTab === 'number' && tabs[window.__jtlTab]) { select(window.__jtlTab); }

  // A hover layer with no dependencies: every mark carries data-tip, and a
  // native <title> underneath covers the no-JS and screen-reader cases.
  var tip = document.getElementById('tip');
  function show(e, t) {
    tip.textContent = t;
    tip.style.opacity = '1';
    var r = tip.getBoundingClientRect();
    var x = (e.clientX || 0) + 14, y = (e.clientY || 0) + 14;
    if (x + r.width > window.innerWidth - 8) { x = window.innerWidth - r.width - 8; }
    if (y + r.height > window.innerHeight - 8) { y = (e.clientY || 0) - r.height - 14; }
    tip.style.left = x + 'px'; tip.style.top = y + 'px';
  }
  function hide() { tip.style.opacity = '0'; }
  function find(e) { return e.target && e.target.closest ? e.target.closest('[data-tip]') : null; }
  document.addEventListener('mouseover', function (e) { var t = find(e); if (t) { show(e, t.getAttribute('data-tip')); } else { hide(); } });
  document.addEventListener('mousemove', function (e) { var t = find(e); if (t) { show(e, t.getAttribute('data-tip')); } });
  document.addEventListener('mouseout', hide);
  document.addEventListener('focusin', function (e) {
    var t = find(e);
    if (!t) { return hide(); }
    var b = t.getBoundingClientRect();
    show({ clientX: b.left + b.width / 2, clientY: b.top }, t.getAttribute('data-tip'));
  });
  document.addEventListener('focusout', hide);
})();
</script>
</body>
</html>
`))
