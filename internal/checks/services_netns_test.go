package checks

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nesolva/localci/internal/store"
)

// ncService is a service that answers "pong" on port: a stand-in for Postgres that needs no image of its own.
// It returns the YAML of the service, named name, with its ports lines.
func ncService(t *testing.T, img, name string, port int, ports ...string) string {
	t.Helper()
	return ncAnswer(t, img, name, port, "pong", ports...)
}

// ncAnswer is ncService with a shell expression for what it answers.
func ncAnswer(t *testing.T, img, name string, port int, answer string, ports ...string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), name+".sh")
	body := fmt.Sprintf("#!/bin/sh\nwhile true; do printf 'HTTP/1.0 200 OK\\r\\n\\r\\n%%s\\n' \"%s\" | nc -l -p %d; done\n", answer, port)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	y := fmt.Sprintf("      %s:\n        image: %s\n        volumes:\n          - %s:/serve.sh:ro\n        options: --entrypoint /serve.sh\n", name, img, script)
	if len(ports) > 0 {
		y += "        ports:\n"
		for _, p := range ports {
			y += fmt.Sprintf("          - %q\n", p)
		}
	}
	return y
}

// fetch is a step that waits for url to answer.
func fetch(url string) string {
	return fmt.Sprintf("'for i in $(seq 40); do wget -qO- %s && exit 0; sleep 0.25; done; exit 1'", url)
}

func TestServicesReachableOnLocalhost(t *testing.T) {
	img := dockerImage(t)
	wf := fmt.Sprintf(`name: CI
on: [pull_request]
jobs:
  db:
    runs-on: ubuntu-latest
    services:
%s%s    steps:
      - name: by localhost
        run: %s
      - name: by name
        run: %s
      - name: a service reaches another by name
        run: 'for i in $(seq 40); do wget -qO- http://localhost:6000/ | grep -q relayed-pong && exit 0; sleep 0.25; done; exit 1'
`, ncService(t, img, "postgres", 5432, "5432:5432"),
		ncAnswer(t, img, "relay", 6000, "relayed-$(wget -qO- http://postgres:5432/)"),
		fetch("http://localhost:5432/"), fetch("http://postgres:5432/"))
	fx := newFixture(t, wf)
	fx.runner.Options.DefaultImage = img
	r := fx.submit(fx.request(9, fx.pull("9", map[string]string{"a": "1"}))).Runs[0]

	if st := fx.run(r.RunID); st.State != store.StatePassed {
		t.Fatalf("state = %s %s\n%s", st.State, st.Reason, fx.runLog(r.RunID))
	}
	if left := docker(t, "ps", "-a", "--filter", "name=localci-"+r.RunID, "-q"); left != "" {
		t.Fatalf("containers left behind: %s", left)
	}
}

func TestServicesDoNotClaimHostPorts(t *testing.T) {
	img := dockerImage(t)
	// Something already listens on 127.0.0.1:5432, as a developer's own Postgres does. If the port is taken
	// by the real thing, that is the same situation.
	var hostHits atomic.Int32
	if l, err := net.Listen("tcp", "127.0.0.1:5432"); err == nil {
		defer l.Close()
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				hostHits.Add(1)
				c.Close()
			}
		}()
	}
	job := func(name string) string {
		return fmt.Sprintf(`  %s:
    runs-on: ubuntu-latest
    services:
%s    steps:
      - run: %s
      - run: sleep 3
`, name, ncService(t, img, "postgres", 5432, "5432:5432"), fetch("http://localhost:5432/"))
	}
	wf := "name: CI\non: [pull_request]\njobs:\n" + job("one") + job("two")
	fx := newFixture(t, wf)
	fx.runner.Options.DefaultImage = img
	sub := fx.submit(fx.request(9, fx.pull("9", map[string]string{"a": "1"})))

	if len(sub.Runs) != 2 {
		t.Fatalf("runs = %d", len(sub.Runs))
	}
	for _, r := range sub.Runs {
		if st := fx.run(r.RunID); st.State != store.StatePassed {
			t.Fatalf("state = %s %s\n%s", st.State, st.Reason, fx.runLog(r.RunID))
		}
	}
	if fx.admit.max < 2 {
		t.Fatalf("the jobs did not overlap (max %d running together): the test proves nothing", fx.admit.max)
	}
	if n := hostHits.Load(); n != 0 {
		t.Fatalf("the host's own listener on 5432 got %d connections from the jobs", n)
	}
}

func TestServicePortRemapNote(t *testing.T) {
	img := dockerImage(t)
	wf := fmt.Sprintf(`name: CI
on: [pull_request]
jobs:
  db:
    runs-on: ubuntu-latest
    services:
%s%s%s    steps:
      - run: echo steps ran
`, ncService(t, img, "pg", 5432, "5433:5432"), ncService(t, img, "web", 8080, "8080:8080"), ncService(t, img, "bare", 9090, "9090"))
	fx := newFixture(t, wf)
	fx.runner.Options.DefaultImage = img
	r := fx.submit(fx.request(9, fx.pull("9", map[string]string{"a": "1"}))).Runs[0]

	if st := fx.run(r.RunID); st.State != store.StatePassed {
		t.Fatalf("state = %s %s\n%s", st.State, st.Reason, fx.runLog(r.RunID))
	}
	log := fx.runLog(r.RunID)
	if n := strings.Count(log, "note: service"); n != 1 || !strings.Contains(log, "note: service pg: ports 5433:5432") ||
		!strings.Contains(log, "localhost:5432") {
		t.Fatalf("want one note naming pg and port 5432:\n%s", log)
	}
}

func TestServicesSamePort(t *testing.T) {
	wf := `name: CI
on: [pull_request]
jobs:
  db:
    runs-on: ubuntu-latest
    services:
      postgres:
        image: acme/db
        ports: ["5432:5432"]
      replica:
        image: acme/db
        ports: ["127.0.0.1:5433:5432"]
    steps:
      - run: echo "$RUNLOG" > /dev/null
`
	fx := newFixture(t, wf)
	r := fx.submit(fx.request(9, fx.pull("9", map[string]string{"a": "1"}))).Runs[0]

	st := fx.run(r.RunID)
	if st.State != store.StateFailed || !strings.Contains(st.Reason, "postgres") || !strings.Contains(st.Reason, "replica") ||
		!strings.Contains(st.Reason, "5432") {
		t.Fatalf("state = %s, reason = %q: want a failure naming both services and the port", st.State, st.Reason)
	}
	if log := fx.runLog(r.RunID); strings.Contains(log, "==>") {
		t.Fatalf("a step started although two services want one port:\n%s", log)
	}
}
