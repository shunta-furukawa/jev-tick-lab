// Package obs writes one JSONL record per evaluation.
//
// This file is the actual deliverable of the experiment. The trading P&L is
// secondary; what matters is whether Jev's confidence is calibrated on a task
// it was never trained for. That requires, for every tick: the full answer set,
// the model version that produced it, and the realised price some horizon later.
//
// Forward-fill design: records are written immediately with the price_after_*
// fields empty, and cmd/fill joins each record to the price N seconds later
// using TickID. Never try to fill the outcome inline — it would block the loop.
package obs

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/decide"
	"github.com/shunta-furukawa/jev-tick-lab/internal/jev"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
)

// Record is one tick. Field names are the BigQuery schema; changing them
// breaks historical queries, so append rather than rename.
type Record struct {
	TickID string    `json:"tick_id"` // RFC3339Nano of the tick
	RunID  string    `json:"run_id"`  // ties a record to a row in runs-*.jsonl
	At     time.Time `json:"at"`
	Pair   string    `json:"pair"`
	Mode   string    `json:"mode"`

	// Model identity. ModelVersion is the response's own `model` field, which
	// is the versioned id — not the alias that was requested.
	ModelRequested string `json:"model_requested"`
	ModelVersion   string `json:"model_version"`

	// Inputs. Position is always flat in shadow mode, but it is recorded
	// anyway: every input decide.Compose reads has to be in the record, or the
	// thresholds cannot be re-applied to it later. See TestSignalIsRederivable.
	Snapshot  marketstate.Snapshot `json:"snapshot"`
	Position  marketstate.Position `json:"position"`
	StateText string               `json:"state_text"`
	StateHash string               `json:"state_hash"`

	// Outputs.
	Answers map[string]jev.Answer `json:"answers"`
	Signal  decide.Signal         `json:"signal"`

	// Costs.
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	LatencyMs    float64 `json:"latency_ms"`

	// Filled by cmd/fill, not at write time.
	PriceAfter10s  float64 `json:"price_after_10s,omitempty"`
	PriceAfter60s  float64 `json:"price_after_60s,omitempty"`
	PriceAfter300s float64 `json:"price_after_300s,omitempty"`

	Error string `json:"error,omitempty"`
}

// Run describes one process lifetime. It is written once, to its own file, so
// that the tick schema stays uniform for a BigQuery autodetect load while the
// configuration that produced those ticks is still recoverable.
type Run struct {
	RunID          string                  `json:"run_id"`
	StartedAt      time.Time               `json:"started_at"`
	Pair           string                  `json:"pair"`
	Mode           string                  `json:"mode"`
	ModelRequested string                  `json:"model_requested"`
	TickInterval   string                  `json:"tick_interval"`
	Thresholds     decide.Thresholds       `json:"thresholds"`
	QuestionIDs    []string                `json:"question_ids"`
	QuestionsHash  string                  `json:"questions_hash"`
	Questions      map[string]jev.Question `json:"questions"`
}

// NewRunID is a sortable, human-readable id: a run is identified by when it
// started, which is also how the log files are named.
func NewRunID(now time.Time) string {
	return now.UTC().Format("20060102T150405Z")
}

// HashQuestions fingerprints the question set. If this changes between runs,
// records from either side of the change are not directly comparable.
func HashQuestions(qs map[string]jev.Question) string {
	// json.Marshal sorts map keys, so the encoding is stable.
	b, err := json.Marshal(qs)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type Logger struct {
	mu   sync.Mutex
	file *os.File
	w    *bufio.Writer
	day  string
	dir  string
}

func NewLogger(dir string) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	l := &Logger{dir: dir}
	return l, l.rotate(time.Now())
}

func (l *Logger) rotate(now time.Time) error {
	day := now.UTC().Format("2006-01-02")
	if day == l.day && l.file != nil {
		return nil
	}
	if l.w != nil {
		l.w.Flush()
		l.file.Close()
	}
	f, err := os.OpenFile(filepath.Join(l.dir, "ticks-"+day+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	l.file, l.w, l.day = f, bufio.NewWriterSize(f, 64*1024), day
	return nil
}

func (l *Logger) Write(r Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.rotate(r.At); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := l.w.Write(append(b, '\n')); err != nil {
		return err
	}
	// Flush every record. At 1 rps the cost is irrelevant and it means an
	// abrupt kill loses nothing.
	return l.w.Flush()
}

// WriteRun appends the run header to runs-YYYY-MM-DD.jsonl.
func (l *Logger) WriteRun(r Run) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	name := filepath.Join(l.dir, "runs-"+r.StartedAt.UTC().Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write run header: %w", err)
	}
	return nil
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w != nil {
		l.w.Flush()
	}
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// Scan reads a JSONL tick file and calls fn for each record, in file order.
//
// A day is about 29,000 records, so callers that need random access can
// collect them; this exists so that cmd/fill and cmd/calib agree on how a log
// is read, including what to do with a line that will not parse.
func Scan(path string, fn func(Record) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// Records carry the full state text, so the default 64KiB token limit is
	// not generous enough.
	sc.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)

	for line := 1; sc.Scan(); line++ {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			// A truncated final line is expected if the process was killed
			// mid-write; anything else is a corrupt log and should be loud.
			return fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return sc.Err()
}
