package exec

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// Ready says when a service can take requests. Set exactly one of HTTP, TCP, Log.
type Ready struct {
	HTTP    string        // URL; ready on any response below 500
	TCP     int           // loopback port that accepts connections
	Log     string        // regex matched against the output lines
	Timeout time.Duration // default 60s
}

// ServiceError is a service that did not become ready, with the end of its log.
type ServiceError struct {
	Reason string
	Lines  []string // last 20 lines of output
}

func (e *ServiceError) Error() string {
	if len(e.Lines) == 0 {
		return e.Reason
	}
	return e.Reason + ":\n" + strings.Join(e.Lines, "\n")
}

// Service is a long-running process that was ready when StartService returned.
type Service struct {
	*Proc
	stopped atomic.Bool
}

// Stop ends the service and everything it started.
func (s *Service) Stop() Result {
	s.stopped.Store(true)
	s.Cancel()
	return s.Wait()
}

// OnExit calls fn, once and from its own goroutine, if the service ends without Stop having been called: the
// process died, or the context it was started with ended. A service nobody watches that dies goes unnoticed
// until the tests that need it fail.
func (s *Service) OnExit(fn func(Result)) {
	go func() {
		<-s.Done()
		if !s.stopped.Load() {
			fn(s.res)
		}
	}()
}

// StartService runs spec and waits until ready holds. A service that exits
// first, or is not ready in time, is stopped and reported as a *ServiceError.
// ctx bounds the service's whole life, not only the wait.
func StartService(ctx context.Context, spec Spec, ready Ready) (*Service, error) {
	set := 0
	if ready.HTTP != "" {
		set++
	}
	if ready.TCP != 0 {
		set++
	}
	if ready.Log != "" {
		set++
	}
	if set != 1 {
		return nil, fmt.Errorf("exec: set exactly one of Ready.HTTP, Ready.TCP, Ready.Log")
	}
	var re *regexp.Regexp
	if ready.Log != "" {
		var err error
		if re, err = regexp.Compile(ready.Log); err != nil {
			return nil, fmt.Errorf("exec: ready log pattern: %w", err)
		}
	}
	if ready.Timeout <= 0 {
		ready.Timeout = 60 * time.Second
	}

	p, err := Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	fail := func(reason string) (*Service, error) {
		p.Cancel()
		<-p.Done()
		return nil, &ServiceError{Reason: reason, Lines: p.LastLines(20)}
	}

	wctx, cancel := context.WithTimeout(ctx, ready.Timeout)
	defer cancel()

	var matched <-chan struct{}
	if re != nil {
		m := make(chan struct{})
		matched = m
		lines := p.Subscribe(wctx)
		go func() {
			for l := range lines {
				if re.MatchString(l) {
					close(m)
					return
				}
			}
		}()
	}
	client := &http.Client{Timeout: time.Second}
	probe := func() bool {
		switch {
		case ready.HTTP != "":
			resp, err := client.Get(ready.HTTP)
			if err != nil {
				return false
			}
			resp.Body.Close()
			return resp.StatusCode < 500
		case ready.TCP != 0:
			c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", ready.TCP), 200*time.Millisecond)
			if err != nil {
				return false
			}
			c.Close()
			return true
		}
		return false
	}

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-p.Done():
			return fail(fmt.Sprintf("service exited early (code %d)", p.res.ExitCode))
		case <-matched:
			return &Service{Proc: p}, nil
		case <-wctx.Done():
			if ctx.Err() != nil {
				return fail("cancelled while waiting for the service")
			}
			return fail(fmt.Sprintf("service not ready after %s", ready.Timeout))
		case <-tick.C:
			if matched == nil && probe() {
				return &Service{Proc: p}, nil
			}
		}
	}
}
