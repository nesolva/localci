package triage

import (
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/nesolva/localci/internal/report"
)

var urlInText = regexp.MustCompile(`https?://[^\s)'"<>]+`)

// generic path segments say nothing about which page a file is for.
var generic = map[string]bool{"src": true, "app": true, "apps": true, "pages": true, "page": true, "components": true,
	"component": true, "index": true, "main": true, "lib": true, "web": true, "public": true, "static": true,
	"views": true, "view": true, "routes": true, "route": true, "packages": true, "test": true, "tests": true}

// touchedPage returns a changed file named after a page the failure visited (a path segment of a URL it saw), or
// "". It is a hint for a target that vanished, never proof.
func touchedPage(changed []string, f report.Failure) string {
	segs := map[string]bool{}
	add := func(raw string) {
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		for _, s := range strings.Split(u.Path, "/") {
			if s = strings.ToLower(s); s != "" && !generic[s] {
				segs[s] = true
			}
		}
	}
	for _, n := range f.Network {
		add(n.URL)
	}
	for _, u := range urlInText.FindAllString(f.Message, -1) {
		add(u)
	}
	if len(segs) == 0 {
		return ""
	}
	for _, c := range changed {
		parts := strings.Split(strings.ToLower(strings.TrimSuffix(c, path.Ext(c))), "/")
		for _, p := range parts {
			if p = strings.TrimSuffix(p, ".test"); segs[p] && !generic[p] {
				return c
			}
		}
	}
	return ""
}
