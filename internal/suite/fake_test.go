package suite

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The test binary doubles as a tiny test framework: suites under test run `$FAKE_BIN __fake list|run`, which
// speaks the report protocol with behaviours chosen by test id prefix:
//
//	pass*    passes
//	setter*  holds a marker file for a while (global state)
//	reader*  fails if the marker exists when it finishes (conflicts with a setter running beside it)
//	broken*  always fails
//	hang*    starts a child process and never ends
//	ghost*   is never reported at all
//	slow*    takes 5 seconds
//	memo.a.b.x  calls the flows a then b before its work (the segments between "memo" and the last one); a flow is
//	         built once per name path (FLOW_MS, default 150) and restored (fast) when its marker exists; every call is
//	         logged as "saved <path>" or "restored <path>" in $FAKE_DIR/flows.log
//
// FAKE_FIRST and FAKE_RAW are raw report lines written before "begin" and right after it; FAKE_NO_END leaves out the
// "end" event; FAKE_BAD_END writes test.end lines whose fields have the wrong type.
//
// With FAKE_ARTIFACT=1 every test also leaves a screenshot in the output directory, the way Playwright does: the
// directory is $LOCALCI_OUTPUT_DIR, else test-results/shard-$LOCALCI_SHARD in the working directory, and it is
// emptied when the command starts.
func fakeMain(args []string) {
	if len(args) == 0 {
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		f, err := os.Open("tests.txt")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if id := strings.TrimSpace(sc.Text()); id != "" {
				b, _ := json.Marshal(map[string]any{"test": id, "file": "t/" + id + ".spec", "title": id, "line": 1})
				fmt.Println(string(b))
			}
		}
	case "run":
		fakeRun()
	}
}

func fakeRun() {
	dir := os.Getenv("FAKE_DIR")
	rf, err := os.OpenFile(os.Getenv("LOCALCI_REPORT"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	emit := func(t string, body map[string]any) {
		body["v"], body["t"], body["ts"] = 1, t, time.Now().UnixMilli()
		b, _ := json.Marshal(body)
		rf.Write(append(b, '\n'))
	}
	hook := func(kind, test string) {
		if api := os.Getenv("LOCALCI_API"); api != "" {
			req, _ := http.NewRequest("POST", api+"/hooks/test/"+kind+"?test="+url.QueryEscape(test), nil)
			if tok := os.Getenv("LOCALCI_HOOK_TOKEN"); tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}
	ids := readLines(os.Getenv("LOCALCI_TESTS"))
	if f, err := os.OpenFile(filepath.Join(dir, "shards.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		fmt.Fprintf(f, "%s %s %s %d\n", os.Getenv("LOCALCI_SHARD"), os.Getenv("LOCALCI_DB_URL"), os.Getenv("APP_PORT"), len(ids))
		f.Close()
	}
	out := os.Getenv("LOCALCI_OUTPUT_DIR")
	if out == "" {
		out = filepath.Join("test-results", "shard-"+os.Getenv("LOCALCI_SHARD"))
	}
	if os.Getenv("FAKE_ARTIFACT") != "" {
		os.RemoveAll(out)
	}
	raw := func(line string) {
		if line != "" {
			rf.Write([]byte(line + "\n"))
		}
	}
	raw(os.Getenv("FAKE_FIRST"))
	emit("begin", map[string]any{"shard": 1, "total": len(ids)})
	raw(os.Getenv("FAKE_RAW"))
	marker := filepath.Join(dir, "marker")
	// A tenant run names its flow; FAKE_TENANT_FAIL=<shard> makes the flow fail on that shard, the way the Playwright
	// package reports it: the tenant file ($LOCALCI_TENANT_FILE, else tenant.json in the output directory), and every
	// test failing for it. Like the package, a flow already recorded in the file is not run again.
	tenantDown := false
	tenantAt := os.Getenv("LOCALCI_TENANT_FILE")
	if tenantAt == "" {
		tenantAt = filepath.Join(out, "tenant.json")
	}
	var had struct{ Flow, Error string }
	if b, err := os.ReadFile(tenantAt); err == nil && json.Unmarshal(b, &had) == nil && had.Flow == os.Getenv("LOCALCI_TENANT_FLOW") {
		tenantDown = had.Error != ""
	} else if flow := os.Getenv("LOCALCI_TENANT_FLOW"); flow != "" {
		state := map[string]any{"flow": flow, "outputs": map[string]string{"slug": "shard-" + os.Getenv("LOCALCI_SHARD")}}
		if os.Getenv("FAKE_TENANT_FAIL") == os.Getenv("LOCALCI_SHARD") {
			msg := "POST /api/tenants answered 500"
			if v := os.Getenv("FAKE_TENANT_ERROR"); v != "" {
				msg = v // what a flow's error can look like: colours, a stack, a very long line
			}
			state, tenantDown = map[string]any{"flow": flow, "error": msg}, true
		}
		os.MkdirAll(filepath.Dir(tenantAt), 0o755)
		b, _ := json.Marshal(state)
		os.WriteFile(tenantAt, b, 0o644)
		if f, err := os.OpenFile(filepath.Join(dir, "tenant.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			fmt.Fprintf(f, "%s %s\n", os.Getenv("LOCALCI_SHARD"), flow)
			f.Close()
		}
	}
	for _, id := range ids {
		if strings.HasPrefix(id, "ghost") {
			continue
		}
		hook("begin", id)
		emit("test.begin", map[string]any{"test": id, "file": "t/" + id + ".spec", "title": id, "line": 1, "retry": 0})
		if strings.HasPrefix(id, "memo.") {
			fakeFlows(emit, id, dir)
		}
		step := id + ":1"
		emit("step.begin", map[string]any{"test": id, "step": step, "parent": nil, "title": "work", "kind": "step"})
		began := time.Now()
		ok := !tenantDown
		switch {
		case strings.HasPrefix(id, "setter"):
			os.WriteFile(marker, nil, 0o644)
			time.Sleep(1200 * time.Millisecond)
			os.Remove(marker)
		case strings.HasPrefix(id, "reader"):
			time.Sleep(400 * time.Millisecond)
			_, err := os.Stat(marker)
			ok = err != nil
		case strings.HasPrefix(id, "slow"):
			time.Sleep(5 * time.Second)
		case strings.HasPrefix(id, "broken"):
			ok = false
		case strings.HasPrefix(id, "flaky"):
			// Fails the first time it ever runs and passes after, whatever runs next to it.
			seen := filepath.Join(dir, "seen-"+id)
			_, err := os.Stat(seen)
			ok = err == nil
			os.WriteFile(seen, nil, 0o644)
		case strings.HasPrefix(id, "hang"):
			c := osexec.Command("sleep", "60")
			c.Start()
			f, _ := os.OpenFile(filepath.Join(dir, "pids"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			fmt.Fprintln(f, c.Process.Pid)
			f.Close()
			time.Sleep(60 * time.Second)
		default:
			ms := 80
			if v := os.Getenv("FAKE_MS"); v != "" {
				fmt.Sscan(v, &ms)
			}
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		took := time.Since(began).Milliseconds()
		emit("step.end", map[string]any{"step": step, "ms": took, "ok": ok})
		end := map[string]any{"test": id, "status": "passed", "ms": took, "retry": 0}
		if !ok {
			end["status"] = "failed"
			end["failure"] = map[string]any{"step": "work", "message": "boom", "expected": nil, "actual": nil, "location": nil,
				"aria": "- button \"Pay\"", "console": []string{"TypeError: x"}, "network": []map[string]any{{"method": "POST", "url": "/pay", "status": 500}},
				"screenshot": nil, "trace": nil}
		}
		if os.Getenv("FAKE_ARTIFACT") != "" {
			shot := filepath.Join(out, id, "shot.png")
			os.MkdirAll(filepath.Dir(shot), 0o755)
			os.WriteFile(shot, []byte("png"), 0o644)
			abs, _ := filepath.Abs(shot)
			emit("artifact", map[string]any{"test": id, "kind": "screenshot", "path": abs})
		}
		if os.Getenv("FAKE_BAD_END") != "" {
			emit("test.end", map[string]any{"test": id, "status": 5, "ms": "slow", "retry": 0})
		} else {
			emit("test.end", end)
		}
		hook("end", id)
	}
	if os.Getenv("FAKE_NO_END") == "" {
		emit("end", map[string]any{"status": "passed"})
	}
}

// fakeFlows plays the flow calls of a "memo.a.b.x" test the way the Playwright package reports them.
func fakeFlows(emit func(string, map[string]any), id, dir string) {
	parts := strings.Split(id, ".")
	if len(parts) < 3 {
		return
	}
	build := 150
	if v := os.Getenv("FLOW_MS"); v != "" {
		fmt.Sscan(v, &build)
	}
	for i := 1; i < len(parts)-1; i++ {
		path := strings.Join(parts[1:i+1], ".")
		step := fmt.Sprintf("%s:f%d", id, i)
		emit("step.begin", map[string]any{"test": id, "step": step, "parent": nil, "title": "flow: " + parts[i], "kind": "flow"})
		emit("flow.begin", map[string]any{"test": id, "step": step, "flow": parts[i], "inputs": map[string]any{"path": path}, "key": "k-" + path})
		began := time.Now()
		marker := filepath.Join(dir, "ck-"+path)
		state := "restored"
		if _, err := os.Stat(marker); err != nil {
			time.Sleep(time.Duration(build) * time.Millisecond)
			os.WriteFile(marker, nil, 0o644)
			state = "saved"
		} else {
			time.Sleep(10 * time.Millisecond)
		}
		if f, err := os.OpenFile(filepath.Join(dir, "flows.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			fmt.Fprintf(f, "%s %s\n", state, path)
			f.Close()
		}
		emit("flow.end", map[string]any{"step": step, "outputs": map[string]any{}, "checkpoint": state})
		emit("step.end", map[string]any{"step": step, "ms": time.Since(began).Milliseconds(), "ok": true})
	}
}
