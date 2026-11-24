package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

func init() {
	register("logs", "the failing lines of a run's log and the path of the whole file", true,
		agent("logs", "<id> [--shard n] [--lines 20] [--follow]", func(fs *flag.FlagSet) {
			fs.Int("shard", 0, "shard to read (default: the first one with a failure)")
			fs.Int("lines", 20, "how many lines to print")
			fs.Bool("follow", false, "stream the log until the run ends")
		}, cmdLogs))
}

// problem matches the lines of a log worth an agent's attention.
var problem = regexp.MustCompile(`(?i)\b(error|errors|fail|failed|failure|fatal|exception|panic|timed out|timeout)\b|✘|✗|^\s*\d+\) `)

func cmdLogs(e *env, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(e.err, "usage: localci logs <id> [--shard n] [--lines 20] [--follow]")
		return exitUsage
	}
	n, err := strconv.Atoi(e.fs.Lookup("lines").Value.String())
	if err != nil || n < 1 {
		fmt.Fprintln(e.err, "localci: --lines is a positive number")
		return exitUsage
	}
	q := url.Values{}
	if s := e.fs.Lookup("shard").Value.String(); s != "0" {
		q.Set("shard", s)
	}
	follow := flagOn(e, "follow")
	if follow {
		q.Set("follow", "1")
	} else {
		q.Set("tail", "2000")
	}
	resp, err := e.c.stream("/api/runs/" + url.PathEscape(args[0]) + "/log?" + q.Encode())
	if err != nil {
		return e.fail(err)
	}
	defer resp.Body.Close()
	path := resp.Header.Get("X-Log-Path")
	if follow {
		io.Copy(e.out, resp.Body)
		return exitOK
	}

	var all []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		all = append(all, sc.Text())
	}
	picked := all[:0:0]
	for _, l := range all {
		if problem.MatchString(l) {
			picked = append(picked, trim(l, 240))
		}
	}
	if len(picked) == 0 { // nothing looks wrong: the end of the log is the best guess
		for _, l := range all[max(len(all)-n, 0):] {
			picked = append(picked, trim(l, 240))
		}
	}
	picked = picked[max(len(picked)-n, 0):]
	if e.json {
		e.emit("log", struct {
			Path  string   `json:"path"`
			Lines []string `json:"lines"`
		}{path, picked})
		return exitOK
	}
	fmt.Fprintf(e.out, "log: %s\n", path)
	for _, l := range picked {
		fmt.Fprintln(e.out, "  "+strings.TrimRight(l, " "))
	}
	return exitOK
}

// stream opens a long answer; the caller closes the body.
func (c *client) stream(path string) (*http.Response, error) {
	hc := *c.hc
	hc.Timeout = 0
	resp, err := hc.Get(base + path)
	if err != nil {
		return nil, c.unreachable(err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		code := exitUsage
		if resp.StatusCode >= 500 {
			code = exitInfra
		}
		return nil, &apiError{status: resp.StatusCode, msg: errorText(data), exit: code}
	}
	return resp, nil
}
