package explore

import (
	"context"
	"fmt"
	"sync"

	"github.com/nesolva/localci/internal/flow"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

// Runner is the part of the suite runner the service needs.
type Runner interface {
	Submit(suite.Request) (string, error)
	Wait(ctx context.Context, id string) (store.Run, error)
	Suite(name string) (*suite.Suite, bool)
}

// Service starts exploratory runs, on the schedule and on request. One at a time: a night that overruns skips the
// next start instead of queueing behind itself.
type Service struct {
	Cfg    Config
	Runner Runner
	Store  *store.Store
	Clock  Clock
	Log    func(format string, args ...any)

	mu     sync.Mutex
	active string
}

// New checks that the configured suite exists and returns the service; ctx bounds the runs it waits on.
func New(cfg Config, r Runner, st *store.Store) (*Service, error) {
	if _, ok := r.Suite(cfg.Suite); !ok {
		return nil, fmt.Errorf("explore.suite: there is no suite %q in `suites`", cfg.Suite)
	}
	return &Service{Cfg: cfg, Runner: r, Store: st, Clock: SystemClock, Log: func(string, ...any) {}}, nil
}

// ErrBusy says an exploratory run is already queued or running.
type ErrBusy struct{ Run string }

func (e ErrBusy) Error() string {
	return fmt.Sprintf("exploratory run %s is still queued or running: one at a time", e.Run)
}

// Start queues an exploratory run of the configured suite and returns its id. by says who asked ("schedule",
// "cli"). ctx must outlive the call: it bounds how long the service follows the run.
func (s *Service) Start(ctx context.Context, by string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != "" {
		return "", ErrBusy{s.active}
	}
	x := &Explorer{Cfg: s.Cfg, Store: s.Store, Clock: s.Clock, Flows: s.flows(ctx)}
	id, err := s.Runner.Submit(suite.Request{Suite: s.Cfg.Suite, Trigger: suite.ExploreTrigger, RequestedBy: by,
		Explore: &suite.ExploreRequest{BaseURL: s.Cfg.BaseURL, Run: x.Run}})
	if err != nil {
		return "", err
	}
	s.active = id
	go func() {
		s.Runner.Wait(ctx, id)
		s.mu.Lock()
		s.active = ""
		s.mu.Unlock()
	}()
	return id, nil
}

func (s *Service) flows(ctx context.Context) []string {
	evs, err := s.Store.KindEvents(ctx, 50_000, flow.Kinds...)
	if err != nil {
		return nil
	}
	return FlowLines(flow.Summaries(evs))
}

// Schedule starts a run at every time the schedule names, until ctx is done.
func (s *Service) Schedule(ctx context.Context) {
	cron, err := ParseCron(s.Cfg.Schedule)
	if err != nil {
		s.Log("explore: %v", err)
		return
	}
	for {
		now := s.Clock.Now()
		next := cron.Next(now)
		if next.IsZero() {
			s.Log("explore: the schedule %q never fires", s.Cfg.Schedule)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-s.Clock.After(next.Sub(now)):
		}
		id, err := s.Start(ctx, "schedule")
		var busy ErrBusy
		switch {
		case err == nil:
			s.Log("explore: run %s started", id)
		case asBusy(err, &busy):
			s.Log("explore: not started, %v", err)
		default:
			s.Log("explore: not started: %v", err)
		}
	}
}

func asBusy(err error, out *ErrBusy) bool {
	b, ok := err.(ErrBusy)
	*out = b
	return ok
}
