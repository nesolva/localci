package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/nesolva/localci/internal/capacity"
	"github.com/nesolva/localci/internal/config"
	"github.com/nesolva/localci/internal/server"
	"github.com/nesolva/localci/internal/worker"
)

func init() {
	register("worker", "join another machine's localci with a token and run shards for it (`worker join <url> …`, then `worker run`)", false, cmdWorker)
}

var workerName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

func cmdWorker(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "join" && args[0] != "run") {
		fmt.Fprintln(stderr, "usage: localci worker join <url> --token <token> --fingerprint <sha256> [--name n] [--capacity n]\n       localci worker run")
		return exitUsage
	}
	sub := args[0]
	fs := flag.NewFlagSet("localci worker "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", "", "where this worker keeps its credential, mirrors and workspaces (default: a worker folder in the standard data directory)")
	capFlag := fs.Int("capacity", 0, "shards to run at once (default: what this machine can take)")
	var token, fingerprint, name *string
	if sub == "join" {
		token = fs.String("token", "", "the single-use token `localci workers token` printed on the primary")
		fingerprint = fs.String("fingerprint", "", "the primary's certificate fingerprint, printed with the token")
		name = fs.String("name", "", "this worker's name on the primary (default: the host name)")
	}
	pos, err := parseMixed(fs, args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *dataDir == "" {
		*dataDir = filepath.Join(config.DataDir(), "worker")
	}
	if *dataDir, err = filepath.Abs(*dataDir); err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if sub == "join" {
		if len(pos) != 1 || *token == "" || *fingerprint == "" {
			fmt.Fprintln(stderr, "usage: localci worker join <url> --token <token> --fingerprint <sha256> [--name n] [--capacity n]")
			return exitUsage
		}
		u, err := url.Parse(pos[0])
		if err != nil || u.Scheme != "https" || u.Host == "" {
			fmt.Fprintf(stderr, "localci worker: %q is not an https://host:port address; use the one `localci workers token` printed\n", pos[0])
			return exitUsage
		}
		if *name == "" {
			*name, _ = os.Hostname()
			*name = strings.Map(func(r rune) rune {
				if r == '.' || r == ' ' {
					return '-'
				}
				return r
			}, *name)
		}
		if !workerName.MatchString(*name) {
			fmt.Fprintf(stderr, "localci worker: name %q must be 1-40 letters, digits, - or _\n", *name)
			return exitUsage
		}
		c, err := worker.Join(ctx, pos[0], *token, *fingerprint, *name, workerCapacity(*capFlag))
		if err != nil {
			fmt.Fprintf(stderr, "localci worker: %v\n", err)
			return exitInfra
		}
		if err := worker.SaveConfig(*dataDir, c); err != nil {
			fmt.Fprintf(stderr, "localci worker: %v\n", err)
			return exitInfra
		}
		fmt.Fprintf(stdout, "joined %s as %s\n", c.URL, c.Name)
	} else if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: localci worker run [--data-dir d] [--capacity n]")
		return exitUsage
	}

	c, err := worker.LoadConfig(*dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "localci worker: %v\n", err)
		return exitUsage
	}
	logger := log.New(stderr, "", log.LstdFlags)
	logger.Printf("worker %s taking shards from %s", c.Name, c.URL)
	err = worker.Run(ctx, worker.Options{DataDir: *dataDir, Config: c, Capacity: workerCapacity(*capFlag), Log: logger})
	switch {
	case errors.Is(err, worker.ErrRevoked):
		fmt.Fprintf(stderr, "localci worker: %v\n", err)
		return exitInfra
	case err != nil:
		fmt.Fprintf(stderr, "localci worker: %v\n", err)
		return exitInfra
	}
	return exitOK
}

func workerCapacity(flagged int) int {
	if flagged > 0 {
		return flagged
	}
	m := capacity.Detect(context.Background())
	_, jobs := capacity.Slots(m.CPUs, m.FreeMemory, m.DockerMemory)
	return jobs
}

// cmdWorkers is `localci workers [list | token | revoke <name>]`: the machines that run shards, and who may join.
func cmdWorkers(e *env, args []string) int {
	if len(args) == 0 || args[0] == "list" {
		return listWorkers(e)
	}
	switch args[0] {
	case "token":
		var res server.TokenResponse
		if err := e.c.post("/api/workers/token", nil, &res); err != nil {
			return e.fail(err)
		}
		if e.json {
			e.emit("token", res)
			return exitOK
		}
		host := e.fs.Lookup("host").Value.String()
		if host == "" {
			host = "<this-machine>"
		}
		if res.Listen != "lan" && e.fs.Lookup("host").Value.String() == "" {
			fmt.Fprintln(e.err, "note: server.listen is local, so only this machine can reach the worker port; set server.listen: lan to join from another one")
		}
		fmt.Fprintf(e.out, "on the worker machine, once (the token works one time):\n  localci worker join https://%s:%d --token %s --fingerprint %s --name <name>\n",
			host, res.Port, res.Token, res.Fingerprint)
		return exitOK
	case "revoke":
		if len(args) != 2 {
			fmt.Fprintln(e.err, "usage: localci workers revoke <name>")
			return exitUsage
		}
		if err := e.c.post("/api/workers/"+url.PathEscape(args[1])+"/revoke", nil, nil); err != nil {
			return e.fail(err)
		}
		if e.json {
			e.emit("revoked", map[string]string{"name": args[1]})
		} else {
			fmt.Fprintf(e.out, "revoked %s: its next call is refused and its shards are given to another machine\n", args[1])
		}
		return exitOK
	}
	fmt.Fprintln(e.err, "usage: localci workers [list | token [--host h] | revoke <name>]")
	return exitUsage
}

func listWorkers(e *env) int {
	var w server.Workers
	if err := e.c.get("/api/workers", &w); err != nil {
		return e.fail(err)
	}
	for _, x := range w.Workers {
		if e.json {
			e.emit("worker", x)
			continue
		}
		if x.Remote {
			line := fmt.Sprintf("%s · %s · takes %d shards at once", x.Name, x.Status, x.Capacity)
			if len(x.Shards) > 0 {
				line += " · running " + strings.Join(x.Shards, ", ")
			}
			fmt.Fprintln(e.out, line)
			continue
		}
		mem := config.FormatSize(x.Memory)
		if x.Memory == 0 {
			mem = "unlimited memory"
		} else {
			mem = fmt.Sprintf("%s/%s memory", config.FormatSize(x.UsedMemory), mem)
		}
		fmt.Fprintf(e.out, "%s · %d/%d cpus busy · %s · %s running\n", x.Name, x.UsedCPUs, x.CPUs, mem, plural(x.Running, "job", "jobs"))
	}
	return exitOK
}
