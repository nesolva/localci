package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nesolva/localci/internal/config"
	"github.com/nesolva/localci/internal/server"
)

// env is what an agent command runs with: its output, whether to speak JSON and the server to ask.
type env struct {
	out, err io.Writer
	json     bool
	c        *client
	fs       *flag.FlagSet
}

// agent builds a command that talks to the local API. setup adds the command's own flags; fn gets the positional
// arguments (flags may come before or after them).
func agent(name, synopsis string, setup func(*flag.FlagSet), fn func(e *env, args []string) int) func([]string, io.Writer, io.Writer) int {
	return func(args []string, stdout, stderr io.Writer) int {
		fs := flag.NewFlagSet("localci "+name, flag.ContinueOnError)
		fs.SetOutput(stderr)
		fs.Usage = func() {
			fmt.Fprintf(stderr, "usage: localci %s %s\n", name, synopsis)
			fs.PrintDefaults()
		}
		sockFlag := fs.String("socket", "", "API socket of the server (default: api.sock in the data directory, or $LOCALCI_SOCKET)")
		asJSON := fs.Bool("json", false, "print NDJSON, one object per line")
		if setup != nil {
			setup(fs)
		}
		pos, err := parseMixed(fs, args)
		if err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return exitOK
			}
			return exitUsage
		}
		e := &env{out: stdout, err: stderr, json: *asJSON, fs: fs, c: newClient(*sockFlag)}
		return fn(e, pos)
	}
}

// parseMixed parses flags that may appear between and after the positional arguments.
func parseMixed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// client talks to the local API, only ever over the server's Unix socket: the write token is private to this
// user, so it is never sent to a TCP port whoever listens there.
type client struct {
	sock string // empty: the server has not said where it is
	hc   *http.Client
}

// base is the placeholder host of every request; the transport dials the socket whatever it says.
const base = "http://localci"

// newClient finds the socket: --socket, $LOCALCI_SOCKET, else the one in the data directory.
func newClient(flagSock string) *client {
	sock := flagSock
	if sock == "" {
		sock = os.Getenv("LOCALCI_SOCKET")
	}
	if sock == "" {
		sock, _ = server.FindSocket(config.DataDir())
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		if sock == "" {
			return nil, errNoSocket
		}
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}
	return &client{sock: sock, hc: &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{DialContext: dial}}}
}

var errNoSocket = errors.New("no API socket recorded")

// apiError is an answer other than success; exit says what the command should exit with.
type apiError struct {
	status int
	msg    string
	exit   int
}

func (e *apiError) Error() string { return e.msg }

func (c *client) do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, base+path, rd)
	if err != nil {
		return &apiError{msg: err.Error(), exit: exitUsage}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		tok, err := server.ReadToken(config.DataDir())
		if err != nil {
			return &apiError{msg: err.Error(), exit: exitInfra}
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.unreachable(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 300 {
		msg := errorText(data)
		code := exitInfra
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusForbidden {
			code = exitUsage
		}
		return &apiError{status: resp.StatusCode, msg: msg, exit: code}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &apiError{msg: "unexpected answer from the server: " + err.Error(), exit: exitInfra}
	}
	return nil
}

func (c *client) unreachable(err error) error {
	var ne net.Error
	if errors.Is(err, errNoSocket) || errors.As(err, &ne) || strings.Contains(err.Error(), "refused") {
		where := c.sock
		if where == "" {
			where = "the data directory"
		}
		return &apiError{exit: exitInfra,
			msg: "localci is not running (nothing answers on " + where + "): start it with `localci serve` (or `localci service install`)"}
	}
	return &apiError{msg: err.Error(), exit: exitInfra}
}

func (c *client) get(path string, out any) error { return c.do(http.MethodGet, path, nil, out) }

func (c *client) post(path string, body, out any) error {
	return c.do(http.MethodPost, path, body, out)
}

// fail prints an API error and returns its exit code.
func (e *env) fail(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		fmt.Fprintln(e.err, "localci:", ae.msg)
		return ae.exit
	}
	fmt.Fprintln(e.err, "localci:", err)
	return exitInfra
}

// emit writes one NDJSON object of the given type.
func (e *env) emit(typ string, v any) {
	b, _ := json.Marshal(v)
	if len(b) > 1 && b[0] == '{' {
		fmt.Fprintf(e.out, "{\"type\":%q,%s\n", typ, b[1:])
		return
	}
	fmt.Fprintf(e.out, "{\"type\":%q,\"value\":%s}\n", typ, b)
}

// cursorLine ends an output made with --since.
func (e *env) cursorLine(seq int64) {
	if e.json {
		e.emit("cursor", struct {
			Seq int64 `json:"seq"`
		}{seq})
		return
	}
	fmt.Fprintf(e.out, "cursor %d\n", seq)
}

// errorText is the message of an error body, which is JSON from this server and anything else otherwise.
func errorText(data []byte) string {
	var e server.Error
	if json.Unmarshal(data, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(data))
}
