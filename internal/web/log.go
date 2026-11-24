package web

import (
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/nesolva/localci/internal/store"
)

const (
	logWindow = 256 << 10 // bytes read from the end of a log
	tailLines = 200
	failLines = 40
)

var failingLine = regexp.MustCompile(`(?i)(\berror\b|\bfail(ed|ure|ing)?\b|\bpanic\b|timed ?out|✘|\bunhandled\b|exit status [1-9])`)

type logView struct {
	Page
	R       store.Run
	Shards  []logLink
	Shard   int
	Failing []string
	Tail    []string
	Missing bool
	Cut     bool
}

func (s *server) log(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	run, err := s.st.Run(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	shards, err := s.st.Shards(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v := logView{Page: Page{Title: "Log " + shortID(run.ID), Run: run.ID, Live: !store.Terminal(run.State)}, R: run}
	v.Seq = s.seq(ctx)
	want, _ := strconv.Atoi(r.URL.Query().Get("shard"))
	var path string
	for _, sh := range shards {
		if sh.LogPath == "" {
			continue
		}
		v.Shards = append(v.Shards, logLink{N: sh.N, State: sh.State})
		// Only the path the store holds for this shard is ever opened; the query picks a shard, never a file.
		if path == "" && (want == 0 || sh.N == want) {
			path, v.Shard = sh.LogPath, sh.N
		}
	}
	if path == "" {
		v.Missing = true
	} else if text, cut, err := readTail(path); err != nil {
		v.Missing = true
	} else {
		v.Cut = cut
		lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
		for _, l := range lines {
			if len(v.Failing) < failLines && failingLine.MatchString(l) {
				v.Failing = append(v.Failing, l)
			}
		}
		if len(lines) > tailLines {
			lines = lines[len(lines)-tailLines:]
			v.Cut = true
		}
		v.Tail = lines
	}
	s.render(w, http.StatusOK, "log", v)
}

func readTail(path string) (text string, cut bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	off := max(fi.Size()-logWindow, 0)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", false, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", false, err
	}
	text = strings.ToValidUTF8(string(b), "�")
	if off > 0 {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:] // the first line is cut in the middle
		}
		cut = true
	}
	return text, cut, nil
}
