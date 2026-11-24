package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/nesolva/localci/internal/config"
	"github.com/nesolva/localci/internal/doctor"
)

func init() {
	register("doctor", "check this machine and report what it can run", false, cmdDoctor)
}

func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	eff, _, code := load("doctor", args, stderr, nil)
	if eff == nil {
		return code
	}
	c := eff.Capacity
	fmt.Fprintf(stdout, "capacity  %d check slots, %d job slots (%d CPUs, %s memory, Docker %s)\n\n",
		c.CheckSlots, c.JobSlots, c.CPUs, config.FormatSize(c.Memory), dockerLabel(c.DockerMemory))

	results := doctor.Run(context.Background(), doctor.Input{
		Port:    eff.Server.Port,
		DataDir: config.DataDir(),
		Repos:   len(eff.Repos),
	}, doctor.SystemDeps())
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, r := range results {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", strings.ToUpper(string(r.Status)), r.Name, r.Detail)
		if r.Fix != "" {
			fmt.Fprintf(tw, "\t\t-> %s\n", r.Fix)
		}
	}
	tw.Flush()
	if doctor.Failed(results) {
		return 1
	}
	return 0
}

func dockerLabel(mem uint64) string {
	if mem == 0 {
		return "not running"
	}
	return config.FormatSize(mem)
}
