package affected

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Formats are the dump formats a backend coverage entry may name.
var Formats = []string{"files", "lcov", "jacoco-xml", "v8"}

// ParseDump turns what a service's dump command wrote into the repo-relative files that ran. root is the
// service's checkout (absolute paths are made relative to it); repoFiles lists the tracked files, for formats
// that name a file only by package and name. incomplete says a file that ran could not be placed in the repo
// (dependencies are left out on purpose and do not count): a set with a hole in it must not be trusted to skip tests.
func ParseDump(format string, data []byte, root string, repoFiles func() []string) (files []string, incomplete bool, err error) {
	var raw []string
	switch format {
	case "files":
		raw = lines(data)
	case "lcov":
		raw = lcov(data)
	case "jacoco-xml":
		raw, incomplete, err = jacoco(data, repoFiles)
	case "v8":
		raw, err = v8(data)
	default:
		return nil, false, fmt.Errorf("coverage format %q is not known (use %s)", format, strings.Join(Formats, ", "))
	}
	if err != nil {
		return nil, false, err
	}
	var out []string
	for _, p := range raw {
		rel, where := place(root, p)
		switch {
		case where == placed && !slices.Contains(out, rel):
			out = append(out, rel)
		case where == unplaced:
			incomplete = true
		}
	}
	slices.Sort(out)
	return out, incomplete, nil
}

type placement int

const (
	placed     placement = iota // a file inside the root
	dependency                  // not the repo's code: left out on purpose
	unplaced                    // it ran, but cannot be said to be inside the root
)

// relative makes p a clean slash path inside root; files outside it, and dependencies, are not the repo's.
func relative(root, p string) (string, bool) {
	rel, where := place(root, p)
	return rel, where == placed
}

// place makes p a clean slash path inside root, or says why it is not one.
func place(root, p string) (string, placement) {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "file://") {
		if u, err := url.Parse(p); err == nil {
			p = u.Path
		}
	}
	if p == "" {
		return "", dependency
	}
	if strings.Contains(p, "node_modules/") {
		return "", dependency
	}
	if filepath.IsAbs(p) {
		r, err := filepath.Rel(root, p)
		if err != nil {
			return "", unplaced
		}
		p = r
	}
	p = path.Clean(filepath.ToSlash(p))
	if p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
		return "", unplaced
	}
	return p, placed
}

func lines(data []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// lcov keeps the files with at least one line that ran: a file the report lists with every count at zero was
// only loaded.
func lcov(data []byte) []string {
	var out []string
	cur, hit := "", false
	for _, l := range lines(data) {
		switch {
		case strings.HasPrefix(l, "SF:"):
			cur, hit = strings.TrimPrefix(l, "SF:"), false
		case strings.HasPrefix(l, "DA:"):
			if _, n, ok := strings.Cut(strings.TrimPrefix(l, "DA:"), ","); ok {
				n, _, _ = strings.Cut(n, ",")
				if v, err := strconv.ParseInt(n, 10, 64); err == nil && v > 0 {
					hit = true
				}
			}
		case l == "end_of_record":
			if cur != "" && hit {
				out = append(out, cur)
			}
			cur = ""
		}
	}
	return out
}

func jacoco(data []byte, repoFiles func() []string) ([]string, bool, error) {
	var rep struct {
		Packages []struct {
			Name  string `xml:"name,attr"`
			Files []struct {
				Name     string `xml:"name,attr"`
				Counters []struct {
					Type    string `xml:"type,attr"`
					Covered int    `xml:"covered,attr"`
				} `xml:"counter"`
			} `xml:"sourcefile"`
		} `xml:"package"`
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	// A report names a DOCTYPE JaCoCo's DTD; the decoder ignores it and never fetches it.
	if err := dec.Decode(&rep); err != nil {
		return nil, false, fmt.Errorf("jacoco report: %w", err)
	}
	var tracked []string
	if repoFiles != nil {
		tracked = repoFiles()
	}
	var out []string
	unmatched := false
	for _, p := range rep.Packages {
		for _, f := range p.Files {
			ran := false
			for _, c := range f.Counters {
				if (c.Type == "INSTRUCTION" || c.Type == "LINE") && c.Covered > 0 {
					ran = true
				}
			}
			if !ran {
				continue
			}
			suffix := path.Join(p.Name, f.Name)
			found := false
			for _, t := range tracked {
				if t == suffix || strings.HasSuffix(t, "/"+suffix) {
					out = append(out, t)
					found = true
				}
			}
			unmatched = unmatched || !found
		}
	}
	return out, unmatched, nil
}

// v8 reads Node's NODE_V8_COVERAGE files: a script counts when a named function in it ran (the wrapper that
// runs on load does not).
func v8(data []byte) ([]string, error) {
	var rep struct {
		Result []struct {
			URL       string `json:"url"`
			Functions []struct {
				Name   string `json:"functionName"`
				Ranges []struct {
					Count int `json:"count"`
				} `json:"ranges"`
			} `json:"functions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, fmt.Errorf("v8 coverage: %w", err)
	}
	var out []string
	for _, s := range rep.Result {
		if !strings.HasPrefix(s.URL, "file://") {
			continue
		}
		for _, f := range s.Functions {
			if f.Name != "" && len(f.Ranges) > 0 && f.Ranges[0].Count > 0 {
				out = append(out, s.URL)
				break
			}
		}
	}
	return out, nil
}
