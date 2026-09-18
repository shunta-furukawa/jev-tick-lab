package main

import (
	"bufio"
	"encoding/json"
	"io"

	"github.com/shunta-furukawa/jev-tick-lab/internal/obs"
)

type jsonlWriter struct{ w *bufio.Writer }

func newWriter(out io.Writer) *jsonlWriter {
	return &jsonlWriter{w: bufio.NewWriterSize(out, 256*1024)}
}

func (j *jsonlWriter) write(rec obs.Record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := j.w.Write(b); err != nil {
		return err
	}
	return j.w.WriteByte('\n')
}

func (j *jsonlWriter) flush() error { return j.w.Flush() }
