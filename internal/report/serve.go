package report

import (
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/obs"
)

// Handler serves the report over HTTP, rebuilt from whatever is on disk.
//
// This exists because watching a collection through three CLI tools and a JSONL
// file is a chore, and a chore you will not do is a collection you are not
// really watching. The page is the same one cmd/report writes; the only
// difference is that it reloads itself and says how stale it is.
//
// It re-reads the log when the file has changed and serves a cached render
// otherwise, so leaving the tab open does not re-parse a day of records every
// ten seconds.
func Handler(dir string, opt Options) http.Handler {
	c := &cache{dir: dir, opt: opt}
	mux := http.NewServeMux()
	mux.HandleFunc("/", c.serve)
	// A liveness probe that does not cost a render, for a terminal loop or a
	// health check that does not want the whole page.
	mux.HandleFunc("/healthz", c.health)
	return mux
}

type cache struct {
	dir string
	opt Options

	mu      sync.Mutex
	html    string
	built   time.Time
	sig     string
	lastRep Report
	lastErr error
}

// signature is cheap and changes whenever a log file grows, which is the only
// way these files ever change.
func (c *cache) signature() string {
	paths, _ := filepath.Glob(filepath.Join(c.dir, "ticks-*.jsonl"))
	var sig string
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			sig += fmt.Sprintf("%s:%d:%d;", fi.Name(), fi.Size(), fi.ModTime().UnixNano())
		}
	}
	return sig
}

func (c *cache) build() (string, Report, error) {
	sig := c.signature()
	if sig == c.sig && c.html != "" {
		return c.html, c.lastRep, c.lastErr
	}

	paths, err := filepath.Glob(filepath.Join(c.dir, "ticks-*.jsonl"))
	if err != nil {
		return "", Report{}, err
	}
	if len(paths) == 0 {
		return "", Report{}, fmt.Errorf("no ticks-*.jsonl in %s", c.dir)
	}

	var records []obs.Record
	for _, p := range paths {
		if err := obs.Scan(p, func(r obs.Record) error {
			records = append(records, r)
			return nil
		}); err != nil {
			return "", Report{}, err
		}
	}

	rep := Build(records, c.opt)
	rep.Live = true
	html, err := rep.HTML()
	c.sig, c.html, c.lastRep, c.lastErr, c.built = sig, html, rep, err, time.Now()
	return html, rep, err
}

func (c *cache) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	html, _, err := c.build()
	c.mu.Unlock()

	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, waitingPage, template.HTMLEscapeString(err.Error()))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, html)
}

func (c *cache) health(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	_, rep, err := c.build()
	c.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"ok":false,"error":%q}`+"\n", err.Error())
		return
	}
	age := time.Since(rep.To).Seconds()
	fmt.Fprintf(w, `{"ok":true,"records":%d,"newest_age_sec":%.0f,"density":%.3f,"failed":%d}`+"\n",
		rep.Records, age, rep.Density, rep.Failed)
}

// waitingPage is what you see before the first record lands, which on a cold
// start is a minute of warmup rather than a fault.
const waitingPage = `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="5"><title>jev-tick-lab — waiting</title>
<style>
 :root{color-scheme:light dark}
 body{margin:0;min-height:100vh;display:grid;place-items:center;background:#f9f9f7;color:#0b0b0b;
      font:15px/1.6 ui-sans-serif,system-ui,-apple-system,sans-serif}
 @media (prefers-color-scheme:dark){body{background:#0d0d0d;color:#fff}}
 div{max-width:32rem;padding:24px;text-align:center}
 code{font-size:13px;opacity:.75}
</style></head><body><div>
<h1>Waiting for the first record</h1>
<p>The collector writes its log file when it starts and its first record once
the book is seeded and <code>-min-history</code> of bar series has built up.</p>
<p><code>%s</code></p>
<p>This page reloads itself every five seconds.</p>
</div></body></html>`
