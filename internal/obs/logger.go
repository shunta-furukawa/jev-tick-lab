// Package obs writes one JSONL record per evaluation.
//
// This file is the actual deliverable of the experiment. The trading P&L is
// secondary; what matters is whether Jev's confidence is calibrated on a task
// it was never trained for. That requires, for every tick: the full answer set,
// the model version that produced it, and the realised price some horizon later.
//
// Forward-fill design: records are written immediately with Outcome empty, and
// a separate pass joins each record to the price N seconds later using TickID.
// Never try to fill the outcome inline — it would block the loop.
package obs

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/decide"
	"github.com/shunta-furukawa/jev-tick-lab/internal/jev"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
)

// Record is one tick. Field names are the BigQuery schema; changing them
// breaks historical queries, so append rather than rename.
type Record struct {
	TickID    string    `json:"tick_id"` // RFC3339Nano of the tick
	At        time.Time `json:"at"`
	Pair      string    `json:"pair"`

	// Model identity. ModelVersion is the response's own `model` field, which
	// is the versioned id — not the alias that was requested.
	ModelRequested string `json:"model_requested"`
	ModelVersion   string `json:"model_version"`

	// Inputs.
	Snapshot  marketstate.Snapshot `json:"snapshot"`
	StateText string               `json:"state_text"`
	StateHash string               `json:"state_hash"`

	// Outputs.
	Answers map[string]jev.Answer `json:"answers"`
	Signal  decide.Signal         `json:"signal"`

	// Costs.
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	LatencyMs    float64 `json:"latency_ms"`

	// Filled by the forward-fill pass, not at write time.
	PriceAfter10s  float64 `json:"price_after_10s,omitempty"`
	PriceAfter60s  float64 `json:"price_after_60s,omitempty"`
	PriceAfter300s float64 `json:"price_after_300s,omitempty"`

	Error string `json:"error,omitempty"`
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
	f, err := os.OpenFile(l.dir+"/ticks-"+day+".jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
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
