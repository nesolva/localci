package exec

import (
	"bufio"
	"context"
	"os"
	"sync"
)

// logHub appends a process's output to its log file and lets any number of
// subscribers follow it. Subscribers read the file themselves, so a slow one
// never blocks the process and a late one still sees everything from the start.
type logHub struct {
	path string

	mu   sync.Mutex
	f    *os.File
	wake chan struct{} // closed and replaced on every change
	done bool
}

func newLogHub(path string) (*logHub, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	return &logHub{path: path, f: f, wake: make(chan struct{})}, nil
}

func (h *logHub) write(b []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.f.Write(b)
	close(h.wake)
	h.wake = make(chan struct{})
}

func (h *logHub) finish() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.f.Close()
	h.done = true
	close(h.wake)
}

func (h *logHub) state() (chan struct{}, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.wake, h.done
}

// subscribe streams lines (without the newline) from the start of the log
// until the process is done or ctx ends; the channel is then closed.
func (h *logHub) subscribe(ctx context.Context) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		f, err := os.Open(h.path)
		if err != nil {
			return
		}
		defer f.Close()
		br := bufio.NewReader(f)
		partial := ""
		send := func(s string) bool {
			select {
			case ch <- s:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for {
			// Read the state first: a write after this point wakes us again.
			wake, done := h.state()
			for {
				s, err := br.ReadString('\n')
				partial += s
				if err != nil {
					break
				}
				if !send(partial[:len(partial)-1]) {
					return
				}
				partial = ""
			}
			if done {
				if partial != "" {
					send(partial)
				}
				return
			}
			select {
			case <-wake:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// lastLines returns up to n trailing lines of the log file.
func lastLines(path string, n int) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var lines []string
	start := 0
	for i, c := range data {
		if c == '\n' {
			lines = append(lines, string(data[start:i]))
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, string(data[start:]))
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
