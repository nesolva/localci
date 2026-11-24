package worker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/server"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

// Options say how a joined worker runs.
type Options struct {
	DataDir  string
	Config   Config
	Capacity int // shards at once
	Log      *log.Logger
	// HeartbeatEvery and ClaimWait have defaults (10s, 20s); tests shorten them.
	HeartbeatEvery time.Duration
	ClaimWait      time.Duration
}

type runner struct {
	o      Options
	client *http.Client
	wr     *suite.WorkerRunner

	mu      sync.Mutex
	running map[string]context.CancelFunc // shard key -> stop it
}

// Run takes shards from the primary until ctx ends or the primary revokes this worker's credential.
func Run(ctx context.Context, o Options) error {
	if o.Log == nil {
		o.Log = log.New(io.Discard, "", 0)
	}
	if o.HeartbeatEvery <= 0 {
		o.HeartbeatEvery = 10 * time.Second
	}
	if o.ClaimWait <= 0 {
		o.ClaimWait = 20 * time.Second
	}
	o.Capacity = max(o.Capacity, 1)
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(o.DataDir, "localci.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	rl, err := startRelay(o.Config)
	if err != nil {
		return err
	}
	defer rl.close()
	wr, err := suite.NewWorkerRunner(suite.Deps{Store: st, Log: event.NewLog(st, event.NewBus()), Sched: sched.New(o.Capacity, 0),
		DataDir: o.DataDir, API: rl.base(),
		RepoURL: func(repo string) string { return rl.base() + "/worker/v1/git/" + repo }})
	if err != nil {
		return err
	}
	defer wr.Close()
	r := &runner{o: o, client: PinnedClient(o.Config.Fingerprint), wr: wr, running: map[string]context.CancelFunc{}}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); r.heartbeats(ctx, cancel) }()
	slots := make(chan struct{}, o.Capacity)
	for ctx.Err() == nil {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			continue
		}
		job, err := r.claim(ctx)
		if err != nil || job == nil {
			<-slots
			if errors.Is(err, ErrRevoked) {
				cancel(err)
			} else if err != nil && ctx.Err() == nil {
				o.Log.Printf("claim: %v", err)
				sleep(ctx, 2*time.Second)
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			r.runJob(ctx, job)
		}()
	}
	wg.Wait()
	if c := context.Cause(ctx); errors.Is(c, ErrRevoked) {
		return c
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (r *runner) call(ctx context.Context, method, path string, body io.Reader, ctype string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, r.o.Config.URL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.o.Config.Credential)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := r.client.Do(req)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, ErrRevoked
	}
	return resp, err
}

func (r *runner) claim(ctx context.Context) (*suite.RemoteJob, error) {
	resp, err := r.call(ctx, "POST", fmt.Sprintf("/worker/v1/claim?wait_ms=%d", r.o.ClaimWait.Milliseconds()), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		var j suite.RemoteJob
		return &j, json.NewDecoder(resp.Body).Decode(&j)
	}
	return nil, errors.New(errorText(resp))
}

func shardKey(j *suite.RemoteJob) string { return fmt.Sprintf("%s/%d/%d", j.Run, j.Shard, j.Gen) }

func (r *runner) heartbeats(ctx context.Context, stop context.CancelCauseFunc) {
	t := time.NewTicker(r.o.HeartbeatEvery)
	defer t.Stop()
	for {
		r.mu.Lock()
		keys := make([]string, 0, len(r.running))
		for k := range r.running {
			keys = append(keys, k)
		}
		r.mu.Unlock()
		b, _ := json.Marshal(server.Heartbeat{Capacity: r.o.Capacity, Running: keys})
		resp, err := r.call(ctx, "POST", "/worker/v1/heartbeat", bytes.NewReader(b), "application/json")
		switch {
		case errors.Is(err, ErrRevoked):
			stop(err)
			return
		case err == nil:
			var hr server.HeartbeatResponse
			json.NewDecoder(resp.Body).Decode(&hr)
			resp.Body.Close()
			r.mu.Lock()
			for _, k := range hr.Drop {
				if c := r.running[k]; c != nil {
					c()
				}
			}
			r.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *runner) runJob(ctx context.Context, j *suite.RemoteJob) {
	key := shardKey(j)
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.mu.Lock()
	r.running[key] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.running, key)
		r.mu.Unlock()
	}()
	base := fmt.Sprintf("/worker/v1/shards/%s/%d/%d/", j.Run, j.Shard, j.Gen)
	res, err := r.wr.RunShard(sctx, *j, &httpOutput{r: r, ctx: sctx, base: base})
	if err != nil {
		// Cancelled, dropped by the primary, or the primary stopped answering: it requeues the shard itself.
		r.o.Log.Printf("shard %s stopped: %v", key, err)
		return
	}
	b, _ := json.Marshal(res)
	if resp, err := r.call(ctx, "POST", base+"done", bytes.NewReader(b), "application/json"); err != nil {
		r.o.Log.Printf("shard %s: could not report its end: %v", key, err)
	} else {
		resp.Body.Close()
	}
}

// httpOutput sends a shard's output to the primary.
type httpOutput struct {
	r    *runner
	ctx  context.Context
	base string
}

func (o *httpOutput) send(what string, body io.Reader, ctype string) error {
	resp, err := o.r.call(o.ctx, "POST", o.base+what, body, ctype)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return errors.New(errorText(resp))
	}
	return nil
}

func (o *httpOutput) Report(p []byte) error {
	return o.send("report", bytes.NewReader(p), "application/x-ndjson")
}
func (o *httpOutput) Log(p []byte) error { return o.send("log", bytes.NewReader(p), "text/plain") }

// Artifacts sends dir as a tar of its regular files (nothing when there are none).
func (o *httpOutput) Artifacts(dir string) error {
	var files []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) == 0 {
		return nil
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		for _, p := range files {
			rel, err := filepath.Rel(dir, p)
			f, oerr := os.Open(p)
			if err != nil || oerr != nil {
				continue
			}
			st, _ := f.Stat()
			tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: 0o644, Size: st.Size(), Typeflag: tar.TypeReg})
			_, cerr := io.Copy(tw, io.LimitReader(f, st.Size()))
			f.Close()
			if cerr != nil {
				pw.CloseWithError(cerr)
				return
			}
		}
		pw.CloseWithError(tw.Close())
	}()
	err := o.send("artifacts", pr, "application/x-tar")
	pr.CloseWithError(err)
	return err
}
