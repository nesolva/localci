package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nesolva/localci/internal/capacity"
	"github.com/nesolva/localci/internal/config"
	"github.com/nesolva/localci/internal/service"
)

func init() {
	register("service", "install, remove or check the background service: service install|uninstall|status", false, cmdService)
}

func cmdService(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stderr, "usage: localci service install [--config file] | uninstall | status")
		if len(args) == 0 {
			return exitUsage
		}
		return exitOK
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("localci service "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "write the unit file here instead of the user's service directory (nothing is loaded)")
	cfgPath := fs.String("config", "", "config file the service uses (default: the standard location)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitUsage
	}
	m, err := service.System(runtime.GOOS)
	if err != nil {
		fmt.Fprintln(stderr, "localci:", err)
		return exitInfra
	}
	m.Dir = *dir
	loaded := *dir == "" // a directory of our own means a trial: write the file, load nothing

	switch sub {
	case "install":
		bin, err := os.Executable()
		if err == nil {
			bin, err = filepath.EvalSymlinks(bin)
		}
		if err != nil {
			fmt.Fprintln(stderr, "localci:", err)
			return exitInfra
		}
		// The service reads and writes what this CLI does: the same data directory (it holds the token the CLI
		// sends) and the same config file, as absolute paths, plus the environment that decided them.
		spec := service.Spec{Binary: bin, Args: []string{"serve", "--data-dir", config.DataDir()},
			Path: os.Getenv("PATH"), Env: service.CarriedEnv(os.Environ())}
		cfgFile := *cfgPath
		if cfgFile == "" {
			cfgFile = os.Getenv("LOCALCI_CONFIG")
		}
		explicit := cfgFile != ""
		if !explicit {
			cfgFile = config.ConfigPath()
		}
		abs, err := filepath.Abs(cfgFile)
		if err != nil {
			fmt.Fprintln(stderr, "localci:", err)
			return exitUsage
		}
		if _, err := os.Stat(abs); explicit || err == nil { // a missing default file is fine; a missing named one is not
			spec.Args = append(spec.Args, "--config", abs)
		}
		res, err := m.Install(spec, loaded)
		if err != nil {
			fmt.Fprintln(stderr, "localci:", err)
			return exitInfra
		}
		fmt.Fprintf(stdout, "wrote %s\n", res.File)
		if w := tokenWarning(os.Getenv, ghHasToken); w != "" {
			fmt.Fprintln(stderr, "localci: warning:", w)
		}
		if res.Started {
			fmt.Fprintf(stdout, "started; log: %s\n", res.Log)
			if eff, err := config.Load(config.Options{Path: *cfgPath, Machine: capacity.Machine{CPUs: 1}}); err == nil {
				fmt.Fprintf(stdout, "dashboard: http://127.0.0.1:%d\n", eff.Server.Port)
			}
		} else {
			fmt.Fprintf(stdout, "not started (--dir); log would be: %s\n", res.Log)
		}
		return exitOK
	case "uninstall":
		file, err := m.Uninstall(loaded)
		if err != nil {
			fmt.Fprintln(stderr, "localci:", err)
			return exitUsage
		}
		fmt.Fprintf(stdout, "removed %s\n", file)
		return exitOK
	case "status":
		s, err := m.Status()
		if err != nil {
			fmt.Fprintln(stderr, "localci:", err)
			return exitInfra
		}
		fmt.Fprintln(stdout, s)
		return exitOK
	}
	fmt.Fprintf(stderr, "localci service: unknown subcommand %q (install, uninstall or status)\n", sub)
	return exitUsage
}

// ghHasToken says whether `gh` can hand out a token without this shell's environment: the service finds gh on its
// PATH and gh keeps its login on disk or in the keychain.
// gh prints a token from GH_TOKEN/GITHUB_TOKEN (and the enterprise ones) when they are set, so those are removed
// first: the service will not have them.
func ghHasToken() bool {
	cmd := exec.Command("gh", "auth", "token")
	for _, kv := range os.Environ() {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN":
		default:
			cmd.Env = append(cmd.Env, kv)
		}
	}
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// tokenWarning is what to tell the user installing the service when the service will not find a GitHub token. The
// token itself is never written to the unit file, so a GITHUB_TOKEN set only in this shell does not reach it.
func tokenWarning(getenv func(string) string, ghLoggedIn func() bool) string {
	if ghLoggedIn() {
		return ""
	}
	if strings.TrimSpace(getenv("GITHUB_TOKEN")) != "" {
		return "GITHUB_TOKEN is set in this shell but is not copied into the service (the unit file is plain text), " +
			"so pull requests will not be watched; run `gh auth login`, then `localci service install` again"
	}
	return ""
}
