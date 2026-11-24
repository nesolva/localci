package workflow

import "strings"

// dockerRunValueFlags are `docker run` flags that take a separate argument, so
// the argument is not mistaken for the image.
var dockerRunValueFlags = map[string]bool{
	"-e": true, "--env": true, "--env-file": true, "-v": true, "--volume": true, "-p": true, "--publish": true,
	"-w": true, "--workdir": true, "--name": true, "--network": true, "--net": true, "-u": true, "--user": true,
	"--entrypoint": true, "--platform": true, "-l": true, "--label": true, "--add-host": true, "--cpus": true,
	"-m": true, "--memory": true, "--restart": true, "-h": true, "--hostname": true, "--mount": true,
	"--device": true, "--cap-add": true, "--cap-drop": true, "--security-opt": true, "--shm-size": true,
	"--ulimit": true, "--tmpfs": true, "--link": true, "--pull": true, "--log-driver": true, "--dns": true,
	"--group-add": true, "--health-cmd": true, "--pid": true, "--ipc": true,
}

// dockerTags returns the image references a shell script builds (-t/--tag) or
// runs, in order of appearance and without duplicates. References that depend
// on shell variables are left out.
func dockerTags(script string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.Trim(s, `"'`)
		if s == "" || strings.ContainsAny(s, "$`") || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	script = strings.ReplaceAll(script, "\\\n", " ")
	for _, line := range strings.Split(script, "\n") {
		for _, cmd := range strings.FieldsFunc(line, func(r rune) bool { return r == ';' || r == '&' || r == '|' }) {
			words := strings.Fields(cmd)
			for len(words) > 0 && (words[0] == "sudo" || strings.Contains(words[0], "=")) {
				words = words[1:]
			}
			if len(words) < 2 || words[0] != "docker" {
				continue
			}
			words = words[1:]
			switch {
			case words[0] == "build" || (len(words) > 1 && (words[0] == "buildx" || words[0] == "image") && words[1] == "build"):
				for i, w := range words {
					switch {
					case (w == "-t" || w == "--tag") && i+1 < len(words):
						add(words[i+1])
					case strings.HasPrefix(w, "--tag="):
						add(strings.TrimPrefix(w, "--tag="))
					case strings.HasPrefix(w, "-t="):
						add(strings.TrimPrefix(w, "-t="))
					}
				}
			case words[0] == "run" || (len(words) > 1 && words[0] == "container" && words[1] == "run"):
				if words[0] == "container" {
					words = words[2:]
				} else {
					words = words[1:]
				}
				for i := 0; i < len(words); i++ {
					w := words[i]
					if !strings.HasPrefix(w, "-") {
						add(w)
						break
					}
					if dockerRunValueFlags[w] {
						i++
					}
				}
			}
		}
	}
	return out
}
