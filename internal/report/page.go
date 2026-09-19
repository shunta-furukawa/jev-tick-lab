package report

import (
	"fmt"
	"html/template"
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

func round(d time.Duration) string {
	if d >= time.Hour {
		return d.Round(time.Minute).String()
	}
	return d.Round(time.Second).String()
}

// The palette is the validated reference instance, expressed as roles so the
// light and dark values swap in one place. Dark is declared under both the OS
// media query and the explicit theme attribute, so a viewer's toggle wins
// either way.
var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}{{with .Subtitle}} — {{.}}{{end}}</title>
<style>
  :root {
    color-scheme: light;
    --plane: #f9f9f7;
    --surface-1: #fcfcfb;
    --text-primary: #0b0b0b;
    --text-secondary: #52514e;
    --muted: #898781;
    --grid: #e1e0d9;
    --baseline: #c3c2b7;
    --border: rgba(11,11,11,0.10);
    --series-1: #2a78d6;
    --good: #0ca30c;
    --critical: #d03b3b;
  }
  @media (prefers-color-scheme: dark) {
    :root:not([data-theme="light"]) {
      color-scheme: dark;
      --plane: #0d0d0d;
      --surface-1: #1a1a19;
      --text-primary: #ffffff;
      --text-secondary: #c3c2b7;
      --muted: #898781;
      --grid: #2c2c2a;
      --baseline: #383835;
      --border: rgba(255,255,255,0.10);
      --series-1: #3987e5;
      --good: #0ca30c;
      --critical: #d03b3b;
    }
  }
  :root[data-theme="dark"] {
    color-scheme: dark;
    --plane: #0d0d0d;
    --surface-1: #1a1a19;
    --text-primary: #ffffff;
    --text-secondary: #c3c2b7;
    --muted: #898781;
    --grid: #2c2c2a;
    --baseline: #383835;
    --border: rgba(255,255,255,0.10);
    --series-1: #3987e5;
  }

  * { box-sizing: border-box; }
  body {
    margin: 0;
    padding: 24px 16px 64px;
    background: var(--plane);
    color: var(--text-primary);
    font: 15px/1.5 ui-sans-serif, system-ui, -apple-system, "Helvetica Neue", Arial, sans-serif;
    -webkit-text-size-adjust: 100%;
  }
  .wrap { max-width: 860px; margin: 0 auto; }
  h1 { font-size: 22px; margin: 0 0 2px; letter-spacing: -0.01em; }
  h1 span { color: var(--text-secondary); font-weight: 400; }
  h2 { font-size: 15px; margin: 0 0 4px; letter-spacing: -0.005em; }
  p.note { margin: 0 0 14px; color: var(--text-secondary); font-size: 13px; }

  .meta { display: flex; flex-wrap: wrap; gap: 6px 18px; margin: 0 0 22px;
          color: var(--text-secondary); font-size: 12.5px; }
  .meta b { color: var(--text-primary); font-weight: 600; }
  .meta .live { display: inline-flex; align-items: center; gap: 6px; color: var(--text-primary);
                font-weight: 600; }
  .meta .live i { width: 7px; height: 7px; border-radius: 50%; background: var(--good);
                  animation: pulse 2s ease-in-out infinite; }
  @keyframes pulse { 0%,100% { opacity: 1 } 50% { opacity: .35 } }
  @media (prefers-reduced-motion: reduce) { .meta .live i { animation: none } }
  .meta .fresh { color: var(--muted); }

  .warn { border: 1px solid var(--border); border-left: 3px solid var(--critical);
          background: var(--surface-1); border-radius: 8px; padding: 10px 14px;
          margin: 0 0 22px; font-size: 13px; color: var(--text-secondary); }

  .tiles { display: grid; gap: 10px; margin: 0 0 26px;
           grid-template-columns: repeat(auto-fit, minmax(132px, 1fr)); }
  .tile { background: var(--surface-1); border: 1px solid var(--border);
          border-radius: 10px; padding: 12px 14px; }
  .tile .k { font-size: 11.5px; letter-spacing: .04em; text-transform: uppercase;
             color: var(--muted); margin-bottom: 4px; }
  .tile .v { font-size: 24px; font-weight: 600; letter-spacing: -0.02em;
             font-variant-numeric: tabular-nums; }
  .tile .n { font-size: 12px; color: var(--text-secondary); margin-top: 2px; }
  .tile .v .dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%;
                  margin-right: 6px; vertical-align: middle; }
  .dot.good { background: var(--good); }
  .dot.critical { background: var(--critical); }

  section { background: var(--surface-1); border: 1px solid var(--border);
            border-radius: 10px; padding: 16px 16px 10px; margin: 0 0 16px; }
  .chart { width: 100%; height: auto; display: block; overflow: visible; margin-top: 4px; }
  .grid { stroke: var(--grid); stroke-width: 1; }
  .baseline { stroke: var(--baseline); stroke-width: 1; }
  .ref { stroke: var(--text-secondary); stroke-width: 1.5; stroke-dasharray: 5 4; }
  .refcasing { stroke: var(--surface-1); stroke-width: 4; }
  .reflabel { fill: var(--muted); font-size: 11px; }
  .axis { fill: var(--muted); font-size: 11px; font-variant-numeric: tabular-nums; }
  .label { fill: var(--text-secondary); font-size: 12px; }
  .value { fill: var(--text-secondary); font-size: 11.5px; font-variant-numeric: tabular-nums; }
  .mark { fill: var(--series-1); }
  .line { fill: none; stroke: var(--series-1); stroke-width: 2;
          stroke-linejoin: round; stroke-linecap: round; }
  .dot { fill: var(--series-1); stroke: var(--surface-1); stroke-width: 2; }
  .mark:hover, .mark:focus, .dot:hover, .dot:focus { fill: var(--text-primary); outline: none; }

  details { margin: 6px 0 0; }
  summary { cursor: pointer; font-size: 12.5px; color: var(--text-secondary); padding: 4px 0; }
  table { border-collapse: collapse; width: 100%; font-size: 12.5px; margin-top: 6px; }
  th, td { text-align: right; padding: 4px 8px; border-bottom: 1px solid var(--grid);
           font-variant-numeric: tabular-nums; }
  th:first-child, td:first-child { text-align: left; }
  th { color: var(--muted); font-weight: 600; font-size: 11.5px;
       text-transform: uppercase; letter-spacing: .04em; }

  #tip { position: fixed; pointer-events: none; opacity: 0; transition: opacity .1s;
         background: var(--text-primary); color: var(--surface-1);
         font-size: 12px; padding: 5px 9px; border-radius: 6px; z-index: 10;
         font-variant-numeric: tabular-nums; max-width: 260px; }
  @media (max-width: 560px) { .tile .v { font-size: 20px; } }
</style>
</head>
<body>
<div class="wrap">
  <h1>{{.Title}} {{with .Subtitle}}<span>· {{.}}</span>{{end}}</h1>

  <div class="meta">
    {{if .Live}}<span class="live"><i></i>live</span>{{end}}
    {{range .Meta}}<span>{{index . 0}} <b>{{index . 1}}</b></span>{{end}}
    {{with .LiveNote}}<span class="fresh">{{.}}</span>{{end}}
  </div>

  {{with .Warning}}<div class="warn">{{.}}</div>{{end}}

  {{if .Tiles}}
  <div class="tiles">
    {{range .Tiles}}
    <div class="tile">
      <div class="k">{{.Label}}</div>
      <div class="v">{{with .Status}}<span class="dot {{.}}"></span>{{end}}{{.Value}}</div>
      <div class="n">{{.Note}}</div>
    </div>
    {{end}}
  </div>
  {{end}}

  {{range .Sections}}
  <section>
    <h2>{{.Title}}</h2>
    <p class="note">{{.Note}}</p>
    {{.SVG}}
    <details>
      <summary>Table view</summary>
      <table>
        <thead><tr>{{range .Head}}<th>{{.}}</th>{{end}}</tr></thead>
        <tbody>{{range .Table}}<tr>{{range .}}<td>{{.}}</td>{{end}}</tr>{{end}}</tbody>
      </table>
    </details>
  </section>
  {{end}}
</div>

<div id="tip" role="status" aria-live="polite"></div>
<script>
{{if .Live}}
// Reload on a timer rather than a socket: the page is a few tens of KB and the
// collector writes every few seconds, so polling is both simpler and enough.
// The scroll position survives, because a dashboard that jumps to the top every
// ten seconds is a dashboard nobody keeps open.
(function () {
  try {
    var y = sessionStorage.getItem('jtl-scroll');
    if (y) { window.scrollTo(0, parseInt(y, 10)); }
  } catch (e) {}
  setTimeout(function () {
    try { sessionStorage.setItem('jtl-scroll', String(window.scrollY)); } catch (e) {}
    location.reload();
  }, 10000);
})();
{{end}}
// A hover layer with no dependencies: every mark carries data-tip, and a
// native <title> underneath covers the no-JS and screen-reader cases.
(function () {
  var tip = document.getElementById('tip');
  function show(e, t) {
    tip.textContent = t;
    tip.style.opacity = '1';
    var r = tip.getBoundingClientRect();
    var x = (e.clientX || 0) + 12, y = (e.clientY || 0) + 12;
    if (x + r.width > window.innerWidth - 8) { x = window.innerWidth - r.width - 8; }
    if (y + r.height > window.innerHeight - 8) { y = (e.clientY || 0) - r.height - 12; }
    tip.style.left = x + 'px'; tip.style.top = y + 'px';
  }
  function hide() { tip.style.opacity = '0'; }
  document.addEventListener('mouseover', function (e) {
    var t = e.target.closest ? e.target.closest('[data-tip]') : null;
    if (t) { show(e, t.getAttribute('data-tip')); } else { hide(); }
  });
  document.addEventListener('mousemove', function (e) {
    var t = e.target.closest ? e.target.closest('[data-tip]') : null;
    if (t) { show(e, t.getAttribute('data-tip')); }
  });
  document.addEventListener('mouseout', hide);
  document.addEventListener('focusin', function (e) {
    var t = e.target.closest ? e.target.closest('[data-tip]') : null;
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
