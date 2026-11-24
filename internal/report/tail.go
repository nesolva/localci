package report

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"time"
)

// Tailer follows a report file while the test process appends to it.
type Tailer struct {
	Path string
	// Interval between looks at the file; defaults to 50ms.
	Interval time.Duration
	// OnEvent gets every well-formed line, in order, known or not.
	OnEvent func(Event)
	// OnBad, when set, gets lines that are not events (never fatal, the line is skipped).
	OnBad func(line []byte, err error)
}

// Run follows the file until ctx ends, then reads what was written meanwhile once more, so the
// caller can cancel right after the process exits without losing the last events. The file may
// not exist yet. A last line without its newline is held until the rest arrives, and delivered
// at the end if it never does and is complete JSON.
func (t *Tailer) Run(ctx context.Context) error {
	interval := t.Interval
	if interval <= 0 {
		interval = 50 * time.Millisecond
	}
	var (
		f       *os.File
		offset  int64
		partial []byte
	)
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	poll := func(final bool) error {
		if f == nil {
			var err error
			if f, err = os.Open(t.Path); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
		}
		if st, err := f.Stat(); err == nil && st.Size() < offset {
			offset, partial = 0, nil // truncated or replaced: start again
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		buf, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		offset += int64(len(buf))
		partial = append(partial, buf...)
		for {
			i := bytes.IndexByte(partial, '\n')
			if i < 0 {
				break
			}
			t.line(partial[:i])
			partial = partial[i+1:]
		}
		if final && len(bytes.TrimSpace(partial)) > 0 {
			t.line(partial)
			partial = nil
		}
		return nil
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return poll(true)
		case <-tick.C:
			if err := poll(false); err != nil {
				return err
			}
		}
	}
}

func (t *Tailer) line(b []byte) {
	if len(bytes.TrimSpace(b)) == 0 {
		return
	}
	ev, err := Parse(b)
	if err != nil {
		if t.OnBad != nil {
			t.OnBad(append([]byte(nil), b...), err)
		}
		return
	}
	if t.OnEvent != nil {
		t.OnEvent(ev)
	}
}

// ReadFile parses a whole report file. Lines that are not events are skipped.
func ReadFile(path string) ([]Event, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Event
	t := Tailer{OnEvent: func(e Event) { out = append(out, e) }}
	for _, l := range bytes.Split(b, []byte("\n")) {
		t.line(l)
	}
	return out, nil
}
