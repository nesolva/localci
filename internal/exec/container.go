package exec

import (
	"bytes"
	"context"
	"fmt"
	osexec "os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ShellQuote quotes s as one word for bash.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// CacheDir is where scanner and tool caches live inside a container that has
// a cache volume.
const CacheDir = "/localci-cache"

// CacheVolume is the named volume that keeps the cache of runs of image.
func CacheVolume(image string) string {
	var b strings.Builder
	for _, r := range image {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return "localci-cache-" + b.String()
}

// setsCacheHome matches a run that already sets XDG_CACHE_HOME itself (a mere
// mention inside the container's own command does not count).
var setsCacheHome = regexp.MustCompile(`(?:^|\s)(?:-e|--env)[\s=]+["']?XDG_CACHE_HOME\b`)

var dockerRunRe = regexp.MustCompile("(^|[\\s;&|(`])docker\\s+(?:container\\s+)?run\\b")

// runValueFlags are `docker run` flags followed by a separate argument, so the
// argument is not mistaken for the image.
var runValueFlags = map[string]bool{
	"-e": true, "--env": true, "--env-file": true, "-v": true, "--volume": true, "-p": true, "--publish": true,
	"-w": true, "--workdir": true, "--name": true, "--network": true, "--net": true, "-u": true, "--user": true,
	"--entrypoint": true, "--platform": true, "-l": true, "--label": true, "--add-host": true, "--cpus": true,
	"-m": true, "--memory": true, "--restart": true, "-h": true, "--hostname": true, "--mount": true,
	"--device": true, "--cap-add": true, "--cap-drop": true, "--security-opt": true, "--shm-size": true,
	"--ulimit": true, "--tmpfs": true, "--link": true, "--pull": true, "--log-driver": true, "--dns": true,
	"--group-add": true, "--health-cmd": true, "--pid": true, "--ipc": true,
}

// RewriteDockerRun gives every `docker run` in script a cache volume: the
// container sees it at CacheDir and XDG_CACHE_HOME points there, so scanners
// that keep a database or index in their cache directory download it once.
// Runs that already set XDG_CACHE_HOME, or whose image is a shell variable,
// are left alone.
func RewriteDockerRun(script string) string {
	var out strings.Builder
	rest := script
	for {
		loc := dockerRunRe.FindStringIndex(rest)
		if loc == nil {
			out.WriteString(rest)
			return out.String()
		}
		end := loc[1]
		out.WriteString(rest[:end])
		rest = rest[end:]
		seg := commandSegment(rest)
		if img := runImage(seg); img != "" && !strings.ContainsAny(img, "$`") && !setsCacheHome.MatchString(seg) {
			out.WriteString(" -e XDG_CACHE_HOME=" + CacheDir + " -v " + CacheVolume(img) + ":" + CacheDir)
		}
	}
}

// commandSegment is the text up to the end of the shell command: a newline
// that is not escaped, or ; & |.
func commandSegment(s string) string {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '\n', ';', '&', '|':
			return s[:i]
		}
	}
	return s
}

// runImage is the image of the `docker run` arguments in seg.
func runImage(seg string) string {
	words := strings.Fields(strings.ReplaceAll(seg, "\\\n", " "))
	for i := 0; i < len(words); i++ {
		w := strings.Trim(words[i], `"'`)
		if !strings.HasPrefix(w, "-") {
			return w
		}
		if runValueFlags[w] {
			i++
		}
	}
	return ""
}

// DockerAvailable reports whether a Docker daemon answers.
func DockerAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return osexec.CommandContext(ctx, "docker", "info", "--format", "{{.ID}}").Run() == nil
}

// docker runs the docker CLI and returns trimmed stdout; a failure carries its stderr.
func docker(ctx context.Context, args ...string) (string, error) {
	cmd := osexec.CommandContext(ctx, "docker", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("docker %s: %s", args[0], strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// CreateNetwork creates a Docker network.
func CreateNetwork(ctx context.Context, name string) error {
	_, err := docker(ctx, "network", "create", name)
	return err
}

// RemoveNetwork removes a Docker network; one that is already gone is not an error.
func RemoveNetwork(ctx context.Context, name string) error {
	_, err := docker(ctx, "network", "rm", name)
	if err != nil && strings.Contains(err.Error(), "not found") {
		return nil
	}
	return err
}

// ContainerSpec describes a detached container.
type ContainerSpec struct {
	Name    string
	Image   string
	Network string
	Aliases []string          // network aliases
	Env     map[string]string // container environment
	Volumes []string          // host:container[:mode]
	Workdir string
	Options string // extra `docker run` flags, as written in a workflow
	// Run is the id of the run the container belongs to; it becomes the label localci.run=<id>, which the sweep
	// at start uses to remove what a killed run left behind.
	Run string
	// Entrypoint and Args replace the image's own command when Entrypoint is set.
	Entrypoint string
	Args       []string
}

// StartContainer starts spec detached and returns once Docker has started it.
func StartContainer(ctx context.Context, spec ContainerSpec) error {
	args := []string{"run", "-d", "--name", spec.Name, "--label", "localci=1"}
	if spec.Run != "" {
		args = append(args, "--label", RunLabelKey+"="+spec.Run)
	}
	if spec.Network != "" {
		args = append(args, "--network", spec.Network)
	}
	for _, a := range spec.Aliases {
		args = append(args, "--network-alias", a)
	}
	for _, k := range sortedMapKeys(spec.Env) {
		args = append(args, "-e", k+"="+spec.Env[k])
	}
	for _, v := range spec.Volumes {
		args = append(args, "-v", v)
	}
	if spec.Workdir != "" {
		args = append(args, "-w", spec.Workdir)
	}
	args = append(args, strings.Fields(spec.Options)...)
	if spec.Entrypoint != "" {
		args = append(args, "--entrypoint", spec.Entrypoint)
	}
	args = append(args, spec.Image)
	args = append(args, spec.Args...)
	_, err := docker(ctx, args...)
	return err
}

// StartJobContainer starts the long-lived container a job's steps run in.
func StartJobContainer(ctx context.Context, spec ContainerSpec) error {
	spec.Entrypoint, spec.Args = "tail", []string{"-f", "/dev/null"}
	return StartContainer(ctx, spec)
}

// RemoveContainer force-removes a container and its anonymous volumes; one that
// is already gone is not an error.
func RemoveContainer(ctx context.Context, name string) error {
	_, err := docker(ctx, "rm", "-f", "-v", name)
	if err != nil && strings.Contains(err.Error(), "No such container") {
		return nil
	}
	return err
}

// WaitHealthy waits for the container's health check to pass. A container
// without a health check counts as healthy once it runs.
func WaitHealthy(ctx context.Context, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		out, err := docker(ctx, "inspect", "-f", "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}", name)
		if err != nil {
			return err
		}
		f := strings.Fields(out)
		switch {
		case len(f) == 0 || f[0] != "running":
			return fmt.Errorf("service %s is not running (%s)", name, out)
		case len(f) == 1 || f[1] == "healthy":
			return nil
		case f[1] == "unhealthy":
			return fmt.Errorf("service %s is unhealthy", name)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("service %s not healthy after %s", name, timeout)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// ExecLine is the shell command line that runs script inside a job container.
// shell is "bash", "sh", or "" for bash when the image has it and sh otherwise
// (GitHub's default); env is the step's environment; workdir may be empty.
func ExecLine(container, workdir, shell string, env map[string]string, script string) string {
	args := []string{"docker", "exec"}
	if workdir != "" {
		args = append(args, "-w", workdir)
	}
	for _, k := range sortedMapKeys(env) {
		args = append(args, "-e", k+"="+env[k])
	}
	args = append(args, container)
	switch shell {
	case "sh":
		args = append(args, "sh", "-e", "-c", script)
	case "bash":
		args = append(args, "bash", "-eo", "pipefail", "-c", script)
	default:
		args = append(args, "sh", "-c",
			`if command -v bash >/dev/null 2>&1; then exec bash -eo pipefail -c "$1"; else exec sh -e -c "$1"; fi`, "sh", script)
	}
	for i, a := range args {
		if i > 0 {
			args[i] = ShellQuote(a)
		}
	}
	return strings.Join(args, " ")
}

func sortedMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Chown gives path inside a running container to uid:gid, as root.
func Chown(ctx context.Context, container string, uid, gid int, path string) error {
	_, err := docker(ctx, "exec", "-u", "0", container, "chown", "-R", fmt.Sprintf("%d:%d", uid, gid), path)
	return err
}

// RemoveLabeled removes the containers carrying label (key=value) and what act made around each. act labels
// only the job container (--container-options reaches no other), but it puts the job and its service containers
// on one network named after the job container (<job container>-<job id>-network), so everything on such a
// network goes with the job container, and the network too. Containers are listed by label and removed by ID,
// never by name: act names its containers deterministically, so a name can already belong to a newer live job.
func RemoveLabeled(ctx context.Context, label string) {
	jobs, err := listContainers(ctx, "label="+label)
	if err != nil {
		return
	}
	ids := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i] = j.id
	}
	removeJobContainers(ctx, ids)
}

type listed struct{ id, run string }

// listContainers lists the containers (running or not) that match filter, by full ID, with the value of the run
// label (empty when there is none).
func listContainers(ctx context.Context, filter string) ([]listed, error) {
	out, err := docker(ctx, "ps", "-a", "--no-trunc", "--filter", filter,
		"--format", `{{.ID}} run={{.Label "`+RunLabelKey+`"}}`)
	if err != nil {
		return nil, err
	}
	var res []listed
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			res = append(res, listed{id: f[0], run: strings.TrimPrefix(f[1], "run=")})
		}
	}
	return res, nil
}

// builtinNetworks are the networks every Docker host has; they are never a job's own.
var builtinNetworks = map[string]bool{"host": true, "bridge": true, "none": true}

// removeJobContainers removes the job containers with these IDs, the containers on the networks act made for
// each, and those networks. The networks come from inspecting the container, and only a network named the way
// act names its own (<container name>-…-network) counts: a workflow can attach its job to any network it likes,
// and what else runs there is not ours to remove.
func removeJobContainers(ctx context.Context, jobs []string) {
	for _, job := range jobs {
		ids := []string{job}
		var netIDs []string
		info, err := docker(ctx, "inspect", "-f",
			`{{.Name}}{{range $k, $v := .NetworkSettings.Networks}} {{$k}}={{$v.NetworkID}}{{end}}`, job)
		if err == nil {
			f := strings.Fields(info)
			name := strings.TrimPrefix(f[0], "/")
			for _, nv := range f[1:] {
				n, id, _ := strings.Cut(nv, "=")
				if id == "" || builtinNetworks[n] || !strings.HasPrefix(n, name+"-") || !strings.HasSuffix(n, "-network") {
					continue
				}
				netIDs = append(netIDs, id)
				if on, err := docker(ctx, "ps", "-a", "-q", "--no-trunc", "--filter", "network="+id); err == nil {
					ids = append(ids, strings.Fields(on)...)
				}
			}
		}
		docker(ctx, append([]string{"rm", "-f", "-v"}, ids...)...)
		for _, id := range netIDs {
			docker(ctx, "network", "rm", id)
		}
	}
}

// SweepRunContainers removes the containers of runs that have finished (ended, or cut off by a kill and then
// cancelled at start): every container labelled localci.run=<id> for which finished(id) is true. A run it does
// not report as finished is left alone, whether it is running or unknown to this instance. It returns the
// number of job containers removed and does nothing when Docker does not answer.
func SweepRunContainers(ctx context.Context, finished func(run string) bool) int {
	all, err := listContainers(ctx, "label="+RunLabelKey)
	if err != nil {
		return 0
	}
	var gone []string
	runs := map[string]bool{}
	for _, c := range all {
		if finished(c.run) {
			gone = append(gone, c.id)
			runs[c.run] = true
		}
	}
	removeJobContainers(ctx, gone)
	for run := range runs {
		_ = RemoveNetwork(ctx, "localci-"+run) // where a check's service containers were attached
	}
	return len(gone)
}
