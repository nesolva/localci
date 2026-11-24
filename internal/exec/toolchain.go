package exec

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Toolchain modes, as in `fidelity.toolchains`.
const (
	ToolchainMise       = "mise"
	ToolchainContainers = "containers"
)

// toolImages maps a tool a setup-* step asks for to the image that provides it.
var toolImages = map[string]string{
	"node":   "node",
	"java":   "eclipse-temurin",
	"python": "python",
	"go":     "golang",
}

// Toolchain is how the tool versions a job declared are provided: through
// `mise exec` on the host, or by running the job in the matching image.
type Toolchain struct {
	// Tools holds the resolved version of each tool.
	Tools map[string]string
	// Image is the image the job runs in when mise is not used; empty when
	// the job needs no tools or mise provides them.
	Image    string
	mise     bool
	declared map[string]string // normalized versions, before mise resolved them
}

// HasMise reports whether mise is installed.
func HasMise() bool {
	_, err := osexec.LookPath("mise")
	return err == nil
}

// FileReader reads files below dir, for ResolveToolchain.
func FileReader(dir string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, p)) }
}

// ResolveToolchain turns the declared tools (a version, or "file:<path>" for a
// version file that read returns) into a Toolchain. mode is "mise", "containers" or ""
// (mise when installed, else containers). With mise the versions are resolved
// to what mise would install now, so a moving "20" does not reuse an old result.
func ResolveToolchain(ctx context.Context, read func(string) ([]byte, error), tools map[string]string, mode string) (Toolchain, error) {
	t := Toolchain{Tools: map[string]string{}, declared: map[string]string{}}
	if len(tools) == 0 {
		return t, nil
	}
	if mode == "" {
		mode = ToolchainContainers
		if HasMise() {
			mode = ToolchainMise
		}
	}
	for tool, v := range tools {
		if _, ok := toolImages[tool]; !ok {
			return t, fmt.Errorf("toolchain %q is not supported", tool)
		}
		if f, ok := strings.CutPrefix(v, "file:"); ok {
			var err error
			if v, err = versionFromFile(read, f, tool); err != nil {
				return t, err
			}
		}
		v = normalizeVersion(tool, v)
		if v == "" {
			return t, fmt.Errorf("toolchain %s: no version", tool)
		}
		t.Tools[tool] = v
		t.declared[tool] = v
	}
	if mode == ToolchainMise {
		if !HasMise() {
			return t, fmt.Errorf("toolchains: mise is not installed (set fidelity.toolchains to containers)")
		}
		t.mise = true
		for tool, v := range t.Tools {
			if latest := miseLatest(ctx, miseSpec(tool, v)); latest != "" {
				t.Tools[tool] = strings.TrimPrefix(latest, miseVersionPrefix(tool))
			}
		}
		return t, nil
	}
	if len(t.Tools) > 1 {
		return t, fmt.Errorf("toolchains: %s need mise (one image cannot provide several tools)", strings.Join(sortedMapKeys(t.Tools), " and "))
	}
	for tool, v := range t.Tools {
		t.Image = toolImages[tool] + ":" + v
	}
	return t, nil
}

// Key identifies the resolved toolchain, for deciding whether a result can be reused.
func (t Toolchain) Key() string {
	if len(t.Tools) == 0 {
		return ""
	}
	if t.Image != "" {
		return "image:" + t.Image
	}
	var parts []string
	for _, tool := range sortedMapKeys(t.Tools) {
		parts = append(parts, tool+"@"+t.Tools[tool])
	}
	return "mise:" + strings.Join(parts, ",")
}

// ContainerImage is the image that provides the tools when the job has to run
// in a container, or "" when no single image does.
func (t Toolchain) ContainerImage() string {
	if t.Image != "" {
		return t.Image
	}
	if len(t.declared) == 1 {
		for tool, v := range t.declared {
			return toolImages[tool] + ":" + v
		}
	}
	return ""
}

// UsesMise reports whether commands must be wrapped with Wrap.
func (t Toolchain) UsesMise() bool { return t.mise }

// Wrap runs command (a shell command line) with the declared tools on PATH.
func (t Toolchain) Wrap(command string) string {
	if !t.mise {
		return command
	}
	var specs []string
	for _, tool := range sortedMapKeys(t.Tools) {
		specs = append(specs, ShellQuote(miseSpec(tool, t.Tools[tool])))
	}
	return "mise exec " + strings.Join(specs, " ") + " -- bash -eo pipefail -c " + ShellQuote(command)
}

func miseSpec(tool, v string) string {
	return tool + "@" + miseVersionPrefix(tool) + v
}

// miseVersionPrefix is how mise names the Java distribution setup-java defaults to.
func miseVersionPrefix(tool string) string {
	if tool == "java" {
		return "temurin-"
	}
	return ""
}

// miseLatest asks mise which version a spec resolves to; "" when it cannot say.
func miseLatest(ctx context.Context, spec string) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := osexec.CommandContext(ctx, "mise", "latest", spec).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var versionPrefix = regexp.MustCompile(`^(?:[v^~=]|>=|lts/)+`)

// normalizeVersion reduces what setup-* steps accept ("v20", "20.x", "^1.22",
// "lts/*") to a plain version both mise and image tags understand.
func normalizeVersion(tool, v string) string {
	v = strings.TrimSpace(v)
	if v == "lts/*" || v == "lts" {
		return "lts"
	}
	v = versionPrefix.ReplaceAllString(v, "")
	for _, suf := range []string{".x", ".*", "-x"} {
		for strings.HasSuffix(v, suf) {
			v = strings.TrimSuffix(v, suf)
		}
	}
	if v == "*" || v == "x" {
		return "latest"
	}
	return v
}

var goDirective = regexp.MustCompile(`^go\s+(\S+)`)

// versionFromFile reads a version file the way the setup-* steps do.
func versionFromFile(read func(string) ([]byte, error), path, tool string) (string, error) {
	data, err := read(path)
	if err != nil {
		return "", fmt.Errorf("toolchain %s: version file %s: %w", tool, path, err)
	}
	base := filepath.Base(path)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		switch base {
		case "go.mod":
			if m := goDirective.FindStringSubmatch(line); m != nil {
				return m[1], nil
			}
		case ".tool-versions":
			fields := strings.Fields(line)
			name := fields[0]
			if name == tool || (tool == "node" && name == "nodejs") || (tool == "go" && name == "golang") {
				if len(fields) > 1 {
					return fields[1], nil
				}
			}
		default:
			return line, nil
		}
	}
	return "", fmt.Errorf("toolchain %s: no version in %s", tool, path)
}
