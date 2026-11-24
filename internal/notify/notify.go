// Package notify raises desktop notifications when a run finishes: terminal-notifier (or osascript) on macOS,
// notify-send on Linux. It only runs the platform's own tool, so nothing here needs a daemon or a permission
// beyond what the person already granted that tool.
package notify

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"time"
)

// Message is one notification. URL is opened when the notification is clicked, on the platforms that can do it.
type Message struct {
	Title, Body, URL string
}

// ErrNoNotifier means the machine has none of the tools localci knows how to call.
var ErrNoNotifier = errors.New("no notification tool found (macOS: osascript, or terminal-notifier for click-to-open; Linux: notify-send)")

// Notifier sends messages. The zero value is not usable: use New.
type Notifier struct {
	goos     string
	lookPath func(string) (string, error)
	run      func(ctx context.Context, name string, args ...string) error
}

// New returns a notifier for this machine.
func New() *Notifier {
	return &Notifier{goos: runtime.GOOS, lookPath: exec.LookPath, run: func(ctx context.Context, name string, args ...string) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, name, args...).Run()
	}}
}

// Send shows m with the first tool this machine has.
func (n *Notifier) Send(ctx context.Context, m Message) error {
	name, args, ok := n.command(m)
	if !ok {
		return ErrNoNotifier
	}
	return n.run(ctx, name, args...)
}

// command picks the tool and its arguments. Text is always passed as an argument, never spliced into a script, so a
// title or body (a suite name comes from a config file) cannot be read as code.
func (n *Notifier) command(m Message) (string, []string, bool) {
	has := func(bin string) (string, bool) {
		p, err := n.lookPath(bin)
		return p, err == nil
	}
	switch n.goos {
	case "darwin":
		if p, ok := has("terminal-notifier"); ok {
			args := []string{"-title", m.Title, "-message", m.Body, "-group", "localci"}
			if m.URL != "" {
				args = append(args, "-open", m.URL)
			}
			return p, args, true
		}
		if p, ok := has("osascript"); ok {
			// The text rides in argv; the script only reads it.
			script := `on run argv
display notification (item 2 of argv) with title (item 1 of argv)
end run`
			return p, []string{"-e", script, m.Title, m.Body}, true
		}
	case "linux":
		if p, ok := has("notify-send"); ok {
			return p, []string{"--app-name=localci", "--", m.Title, m.Body}, true
		}
	}
	return "", nil, false
}
