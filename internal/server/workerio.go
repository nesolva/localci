package server

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"os"
	osexec "os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/suite"
)

// claimPoll is how long a claim waits for a shard before answering 204 (the worker asks again).
const claimPoll = 20 * time.Second

// workerClaim hands the calling worker a shard it has room for. 204: none yet.
func (s *Server) workerClaim(w http.ResponseWriter, r *http.Request) {
	wait := claimPoll
	if ms, err := strconv.Atoi(r.URL.Query().Get("wait_ms")); err == nil && ms >= 0 && time.Duration(ms)*time.Millisecond < wait {
		wait = time.Duration(ms) * time.Millisecond
	}
	job := s.hub.claim(r.Context(), workerName(r), wait)
	if job == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// workerShardOut takes what a worker produces for a shard it holds: report lines, log, artifacts, or its end.
func (s *Server) workerShardOut(w http.ResponseWriter, r *http.Request) {
	shardN, err1 := strconv.Atoi(r.PathValue("shard"))
	gen, err2 := strconv.Atoi(r.PathValue("gen"))
	name := workerName(r)
	a := (*assignment)(nil)
	if err1 == nil && err2 == nil {
		a = s.hub.assignmentOf(name, r.PathValue("run"), shardN, gen)
	}
	if a == nil {
		fail(w, http.StatusConflict, "this shard is not assigned to you (any more)")
		return
	}
	s.hub.touch(name)
	var err error
	switch r.PathValue("what") {
	case "report", "log":
		var b []byte
		if b, err = io.ReadAll(r.Body); err == nil {
			if r.PathValue("what") == "report" {
				err = a.sink.Report(b)
			} else {
				err = a.sink.Log(b)
			}
		}
	case "artifacts":
		err = a.sink.Artifacts(r.Body)
	case "done":
		var res suite.RemoteResult
		if err = json.NewDecoder(r.Body).Decode(&res); err == nil {
			s.hub.finish(a, res, nil)
		}
	default:
		fail(w, http.StatusNotFound, "no such shard call")
		return
	}
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// workerGit serves git's smart HTTP protocol (fetch only) from the primary's mirror of a repository, so a worker
// clones from here. Any commit may be asked for by its id: the commits under test (a pull request's head, a stash
// snapshot) are on no ref. Only the repositories of the configured suites are served.
func (s *Server) workerGit(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	repo = strings.TrimSuffix(repo, ".git")
	allowed := false
	for _, g := range s.d.GitRepos {
		allowed = allowed || g == repo
	}
	dir := exec.MirrorDir(s.d.DataDir, repo)
	if _, err := os.Stat(dir); !allowed || err != nil {
		fail(w, http.StatusNotFound, "no such repository")
		return
	}
	args := []string{"-c", "uploadpack.allowAnySHA1InWant=true", "upload-pack", "--stateless-rpc"}
	cmd := osexec.CommandContext(r.Context(), "git")
	cmd.Env = append(os.Environ(), "GIT_PROTOCOL="+r.Header.Get("Git-Protocol"))
	if r.Method == http.MethodGet {
		if r.URL.Query().Get("service") != "git-upload-pack" {
			fail(w, http.StatusForbidden, "only fetching is served")
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.Header().Set("Cache-Control", "no-cache")
		if !strings.Contains(r.Header.Get("Git-Protocol"), "version=2") {
			io.WriteString(w, "001e# service=git-upload-pack\n0000")
		}
		args = append(args, "--advertise-refs")
	} else {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("Cache-Control", "no-cache")
		var in io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				fail(w, http.StatusBadRequest, "bad request body")
				return
			}
			defer gz.Close()
			in = gz
		}
		cmd.Stdin = in
	}
	cmd.Args = append(cmd.Args, append(args, dir)...)
	cmd.Stdout = w
	if err := cmd.Run(); err != nil && r.Context().Err() == nil {
		// Headers are out already; the client sees a broken stream and says so.
		return
	}
}
