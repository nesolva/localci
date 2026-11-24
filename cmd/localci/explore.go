package main

import (
	"flag"
	"fmt"

	"github.com/nesolva/localci/internal/server"
)

func init() {
	register("explore", "start the exploratory agent now, as the nightly schedule would (it uses the agent's quota)", false,
		agent("explore", "", func(*flag.FlagSet) {}, cmdExplore))
}

func cmdExplore(e *env, _ []string) int {
	var res server.ExploreStarted
	if err := e.c.post("/api/explore", nil, &res); err != nil {
		return e.fail(err)
	}
	if e.json {
		e.emit("explore", res)
		return exitOK
	}
	fmt.Fprintf(e.out, "#%s exploring · localci wait %s · then localci findings --run %s\n", res.Run, res.Run, res.Run)
	return exitOK
}
